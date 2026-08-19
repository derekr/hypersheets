package main

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// mutate.go — inserting and deleting rows and columns.
//
// A cell is addressed by a storage key handed out by a band index (bandkey.go)
// rather than by its display position, because position-as-primary-key would
// make every row mutation O(sheet): with `cells` clustered on (row, col), moving
// a row rewrites every row below it. See applyRowOp for what the band model
// costs instead.
//
// A formula is a template plus its references in integer columns on its own row
// (FormulaTemplate in formula.go), and those references are keys. So a formula
// naming a row that did not move needs no write however far its rank shifted,
// and a range straddling an insertion point expands with no write at all —
// repair is an indexed probe over what the mutation destroyed, not a scan of
// every formula. It is also why there is no dependency edge table: "who reads
// this cell" is a query against those columns.
//
// The column axis is O(sheet): a column is not a contiguous range of the primary
// key, so inserting one rewrites every cell to its right. The same trick would
// work there; the axis is 26 wide, so it has not been worth it.
//
// A primary key cannot be shifted in place — `UPDATE cells SET k = k + 1` hits a
// UNIQUE violation as soon as the first row collides with the one still sitting
// where it is going, and SQLite has no deferred uniqueness and (in this build)
// no UPDATE ... ORDER BY to force a safe direction. So a shift is
// copy-to-temp / delete / re-insert, over one band's worth of keys.

var (
	// ErrBadMutation rejects nonsense arguments: an out-of-grid position, a
	// zero or negative count, a count that runs off the end of the axis.
	ErrBadMutation = errors.New("bad structural mutation")

	// ErrWouldTruncate is the bounds rule for the column axis, fixed at
	// MaxCols: an insert that would push non-empty content off the right-hand
	// edge fails instead of silently dropping it. Pushing empty cells off the
	// end is fine and is the normal case. Rows grow instead of refusing, so
	// this never applies to them.
	ErrWouldTruncate = errors.New("insert would push non-empty content off the grid")

	// ErrRowCeiling is the only row-count refusal: a mutation that would take
	// the sheet past RowCeiling. It is a sanity bound against a typo'd count,
	// not a design limit, so its message quotes the numbers.
	ErrRowCeiling = errors.New("sheet cannot grow past the row ceiling")

	// ErrBadWidth rejects a column width outside MinColWidth..MaxColWidth.
	ErrBadWidth = errors.New("bad column width")
)

// Dirty is what a mutation returns so the caller can publish invalidations.
//
// Cells is nil for a structural mutation, because when rows move "cell B7
// changed" is not expressible as a patch: the B7 the viewer is looking at is a
// different cell than the B7 the server now has. A caller seeing Structural must
// re-render its window rather than patch cells.
//
// Bands is computed, not assumed: an empty sheet below the mutation point
// dirties nothing, and a formula above it whose text changed dirties its band.
type Dirty struct {
	Cells      []CellRef // per-cell patch list; nil when Structural
	Bands      []int     // deduped, ascending; the subjects to publish
	Structural bool      // row/column geometry moved: re-render, do not patch
	Stats      MutateStats

	// Config says the change was to shared sheet configuration — a column or row
	// style — rather than to any cell. It reports dirty bands and no dirty
	// cells, which is the whole saving: styling a column is one record, and a
	// per-cell patch list would re-materialize the cost the cascade exists to
	// delete. To a renderer it means "re-send the stylesheet, then re-render the
	// affected bands", the same shape a column width change has.
	Config bool
}

// IsEmpty reports whether the mutation changed nothing anyone can see.
func (d Dirty) IsEmpty() bool { return len(d.Bands) == 0 && len(d.Cells) == 0 }

// MutateStats is the cost breakdown of one structural mutation, for the same
// reason RecalcStats exists: a single wall-clock number cannot say which part
// of the worst case to fix.
type MutateStats struct {
	CellsMoved     int // rows of `cells` rewritten by the shift
	CellsDropped   int // cells destroyed (deleted range, or pushed off the end)
	FormulasSeen   int // formula cells handled one at a time (repairs + held refs)
	FormulasRewrit int // formula cells whose references did not merely translate
	Recomputed     int // formula cells re-evaluated (repaired + downstream)
	CellsWritten   int // cells re-persisted after recompute
	Queries        int // SQL round trips issued by the recompute reads
	CellsRead      int // cells returned by those reads

	// DepsMoved, DepsWritten, ShiftDeps and WriteDeps are always zero, and are
	// kept only because structure.go and the measurement harness read them.
	// There is no dependency table to move or rewrite: a formula's edges are
	// its own reference columns and they shift with its row.
	DepsMoved   int
	DepsWritten int

	// Storage-band bookkeeping: the part of the model that costs something
	// occasionally rather than always.
	BandsSplit int  // bands divided to keep the shift bounded
	Rebalanced bool // the sheet ran out of slack and was re-keyed: O(sheet)
	KeyBands   int  // storage bands the sheet has

	// Grew is the net change in the sheet's allocated row extent, measured from
	// the band index the mutation produced rather than predicted from the
	// arguments so it cannot disagree with the sheet. The scroll container has
	// to be re-asked its height whenever it is non-zero, which is rare.
	Grew int

	Elapsed    time.Duration // the whole transaction
	ShiftCells time.Duration // move keys inside a band (or columns), reshape, widths
	Scan       time.Duration // find the references this mutation destroys
	ShiftDeps  time.Duration // always zero; see above
	Recalc     time.Duration // topological re-evaluation
	WriteCells time.Duration // persist recomputed values
	WriteDeps  time.Duration // always zero; see above
}

// Shift is the whole geometry-moving phase and Persist the whole write-back
// phase, summed from the fields above so the breakdown stays the source of
// truth.
func (s MutateStats) Shift() time.Duration   { return s.ShiftCells + s.ShiftDeps }
func (s MutateStats) Persist() time.Duration { return s.WriteCells + s.WriteDeps }

// BandsUpTo is every band index of a sheet `rows` display rows tall.
func BandsUpTo(rows int) []int {
	last := BandOf(max(rows, 1) - 1)
	out := make([]int, 0, last+1)
	for b := 0; b <= last; b++ {
		out = append(out, b)
	}
	return out
}

// AllBands is every band of this sheet, at its current extent.
func (s *Sheet) AllBands() []int { return BandsUpTo(s.Rows()) }

// ─── The four entry points ────────────────────────────────────────────────────

// InsertRows inserts n blank rows before row `at` (0-indexed). Every cell at or
// below `at` moves down n rows; every formula ref pointing at or past `at`
// shifts with it; a range spanning the insertion point expands. The sheet grows
// rather than dropping content off the bottom, so the only refusal is
// ErrRowCeiling.
//
// Must be called from the sheet's actor goroutine (actor.go), and is one
// transaction.
func (s *Sheet) InsertRows(at, n int) (Dirty, error) {
	return s.structural(shiftOp{axis: axisRow, at: at, n: n})
}

// DeleteRows deletes n rows starting at row `at`. Rows below close up. A ref to
// a deleted row becomes #REF!; a range overlapping the deletion shrinks; a
// range wholly inside it becomes #REF!.
func (s *Sheet) DeleteRows(at, n int) (Dirty, error) {
	return s.structural(shiftOp{axis: axisRow, at: at, n: n, del: true})
}

// InsertCols inserts n blank columns before column `at`. Same rules as
// InsertRows, transposed.
func (s *Sheet) InsertCols(at, n int) (Dirty, error) {
	return s.structural(shiftOp{axis: axisCol, at: at, n: n})
}

// DeleteCols deletes n columns starting at column `at`. Same rules as
// DeleteRows, transposed. Stored column widths move with their columns.
func (s *Sheet) DeleteCols(at, n int) (Dirty, error) {
	return s.structural(shiftOp{axis: axisCol, at: at, n: n, del: true})
}

// ─── The transform ────────────────────────────────────────────────────────────
//
// One value type describes all four operations: the row and column cases are the
// same arithmetic on a different coordinate.

type axisKind int

const (
	axisRow axisKind = iota
	axisCol
)

func (a axisKind) String() string {
	if a == axisRow {
		return "row"
	}
	return "col"
}

// shiftOp is one structural mutation: insert or delete n slots at index `at`
// along one axis.
type shiftOp struct {
	axis axisKind
	del  bool
	at   int
	n    int

	// lim is the exclusive upper bound of the axis after the mutation — what
	// point() and span() mean by "off the end". It is a field because the row
	// axis has no constant bound: an insert that grows the sheet sets it to
	// extent+n so nothing falls off. Zero means "unset", which the column axis
	// always is and which gives a row op built as a literal the default grid.
	lim int

	// grow says this row insert extends the sheet rather than reusing n blank
	// rows at the bottom. Decided by rowInsertGrows before anything is touched.
	grow bool
}

// grows reports whether this op makes the sheet taller.
func (op shiftOp) grows() bool { return op.grow }

// limit is the exclusive upper bound of the axis after the mutation.
func (op shiftOp) limit() int {
	if op.axis != axisRow {
		return MaxCols
	}
	if op.lim > 0 {
		return op.lim
	}
	return DefaultRows
}

func (op shiftOp) verb() string {
	if op.del {
		return "delete"
	}
	return "insert"
}

func (op shiftOp) String() string {
	return fmt.Sprintf("%s %d %s at %d", op.verb(), op.n, op.axis, op.at)
}

// validate checks the arguments against the axis as it is right now: extent for
// rows, MaxCols for columns. The one asymmetry is deliberate: a row insert may
// name `at == extent`, meaning "append at the bottom". Everything else, delete
// included, has to name a slot that exists.
func (op shiftOp) validate(extent int) error {
	if op.axis != axisRow {
		extent = MaxCols
	}
	hi := extent - 1
	if op.axis == axisRow && !op.del {
		hi = extent
	}
	if op.at < 0 || op.at > hi {
		return fmt.Errorf("%w: %s at %d out of 0..%d", ErrBadMutation, op.verb(), op.at, hi)
	}
	if op.n < 1 {
		return fmt.Errorf("%w: %s %d %ss at %d", ErrBadMutation, op.verb(), op.n, op.axis, op.at)
	}
	// How many slots the count may claim. A delete cannot remove more than
	// exists; a column insert cannot exceed the fixed axis; a row insert is
	// bounded only by the ceiling, because rows it cannot find at the bottom it
	// creates.
	maxN := extent - op.at
	if op.axis == axisRow && !op.del {
		maxN = RowCeiling - extent
	}
	if op.n > maxN {
		if op.axis == axisRow && !op.del {
			return fmt.Errorf("%w: %s would take the sheet to %d rows, past the %d-row ceiling (it has %d)",
				ErrRowCeiling, op, extent+op.n, RowCeiling, extent)
		}
		return fmt.Errorf("%w: %s %d %ss at %d (max %d here)",
			ErrBadMutation, op.verb(), op.n, op.axis, op.at, maxN)
	}
	return nil
}

// point maps a single index through the mutation. ok == false means the index
// ceased to exist — deleted, or pushed off the end of the axis by an insert —
// which a caller turns into #REF!.
func (op shiftOp) point(i int) (int, bool) {
	if op.del {
		switch {
		case i >= op.at+op.n:
			return i - op.n, true
		case i >= op.at:
			return 0, false // this slot was deleted
		}
		return i, true
	}
	if i < op.at {
		return i, true
	}
	if i+op.n >= op.limit() {
		return 0, false // pushed off the end of the fixed grid
	}
	return i + op.n, true
}

// span maps an inclusive range through the mutation: the two endpoints move
// independently by the point rule, clamped back into the range. ok == false
// means the range no longer exists at all.
//
//	insert inside a range  -> only hi moves        -> the range expands
//	insert below a range   -> neither moves        -> unchanged
//	insert above a range   -> both move            -> the range shifts
//	delete overlapping     -> hi pulled to at-1 or shifted -> the range shrinks
//	delete containing      -> hi < lo              -> #REF!
func (op shiftOp) span(lo, hi int) (int, int, bool) {
	if op.del {
		nlo, nhi := lo, hi
		switch {
		case lo >= op.at+op.n:
			nlo = lo - op.n
		case lo >= op.at:
			nlo = op.at // start of the range was deleted; it now starts here
		}
		switch {
		case hi >= op.at+op.n:
			nhi = hi - op.n
		case hi >= op.at:
			nhi = op.at - 1 // end of the range was deleted; it now ends here
		}
		if nhi < nlo {
			return 0, 0, false // the range was wholly inside the deletion
		}
		return nlo, nhi, true
	}

	nlo, nhi := lo, hi
	if lo >= op.at {
		nlo = lo + op.n
	}
	if hi >= op.at {
		nhi = hi + op.n
	}
	lim := op.limit()
	if nlo >= lim {
		return 0, 0, false
	}
	if nhi >= lim {
		// The tail of the range fell off the end. Clamp rather than #REF!: the
		// insert only got this far because everything past the edge was empty,
		// so a clamped range covers the same non-empty cells and the SUM keeps
		// its value.
		nhi = lim - 1
	}
	return nlo, nhi, true
}

// ref maps a whole cell reference along this op's axis.
func (op shiftOp) ref(c CellRef) (CellRef, bool) {
	if op.axis == axisRow {
		r, ok := op.point(c.Row)
		return CellRef{Row: r, Col: c.Col}, ok
	}
	col, ok := op.point(c.Col)
	return CellRef{Row: c.Row, Col: col}, ok
}

// ─── Formula rewriting ────────────────────────────────────────────────────────

// rewriteFormula returns raw with every cell reference mapped through op, and
// whether anything moved. It is the specification of what a structural mutation
// does to a formula, text in and text out; the storage path moves integer
// columns instead and does not call it, so this is the oracle that path is
// tested against.
//
// An unchanged formula keeps its exact original text, so a mutation that misses
// a cell leaves no trace on it. A dead reference becomes the literal "#REF!",
// which is both what a spreadsheet shows and text the parser already rejects
// with TokenRef, so `=#REF!*2` evaluates to the right error with no special
// case.
func rewriteFormula(raw string, op shiftOp) (string, bool) {
	f, err := ParseFormula(raw)
	if err != nil {
		// Already broken (or already #REF!'d by an earlier mutation): nothing
		// to move, and re-rendering would destroy the text the user still needs
		// to see to fix it.
		return raw, false
	}
	switch f.Kind {
	case FormulaSum:
		nlo, nhi := f.Lo, f.Hi
		var ok bool
		if op.axis == axisRow {
			var a, b int
			a, b, ok = op.span(f.Lo.Row, f.Hi.Row)
			nlo.Row, nhi.Row = a, b
		} else {
			var a, b int
			a, b, ok = op.span(f.Lo.Col, f.Hi.Col)
			nlo.Col, nhi.Col = a, b
		}
		if !ok {
			return "=SUM(" + TokenRef + ")", true
		}
		if nlo == f.Lo && nhi == f.Hi {
			return raw, false
		}
		return "=SUM(" + nlo.String() + ":" + nhi.String() + ")", true

	case FormulaBinary:
		l, lc := op.operandText(f.Left)
		r, rc := op.operandText(f.Right)
		if !lc && !rc {
			return raw, false
		}
		return "=" + l + string(f.Op) + r, true

	default: // FormulaValue
		l, lc := op.operandText(f.Left)
		if !lc {
			return raw, false
		}
		return "=" + l, true
	}
}

// operandText renders one operand after the mutation, and reports whether it
// moved. A literal never moves and keeps its source text verbatim.
func (op shiftOp) operandText(o Operand) (string, bool) {
	if !o.IsRef {
		return o.Text, false
	}
	nr, ok := op.ref(o.Ref)
	if !ok {
		return TokenRef, true
	}
	if nr == o.Ref {
		return o.Text, false
	}
	return nr.String(), true
}

// ─── The transaction ──────────────────────────────────────────────────────────

// structural runs one mutation end to end in one transaction: bounds check, cell
// shift, width shift, reference repair, topological recompute, and the event
// describing it. Splitting any of it out would let a reader see a sheet whose
// cells have moved but whose formulas still point where they were.
//
// The rank<->key mapping is part of that atomicity, not a cache beside it, and
// is installed by the same call that commits: a reader holding the old mapping
// against the new table would render the right cells at the wrong row numbers
// with nothing reporting an error.
func (s *Sheet) structural(op shiftOp) (Dirty, error) {
	// The sheet cannot change height underneath this: every path that moves the
	// extent runs on the actor goroutine, which is this one. runStructural
	// re-reads it from the index anyway, so nothing downstream depends on this
	// sample.
	if err := op.validate(s.Rows()); err != nil {
		return Dirty{}, err
	}
	start := time.Now()
	var d Dirty
	err := s.use(func(db *sql.DB) error {
		bi := s.index()
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin %s: %w", op, err)
		}
		defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

		out, next, err := runStructural(tx, bi, op)
		if err != nil {
			return err
		}
		// The resident cascade levels are rebuilt from the transaction rather
		// than reasoned about: a column op moved the column level with the
		// widths (shiftColMeta) and a row op may have destroyed row styles with
		// their rows, so the in-memory snapshot the read path resolves against
		// is stale by the time this commits, and a stale column array would
		// silently paint the wrong column on every push.
		nextSty, err := relevelState(tx, s.styleStateNow())
		if err != nil {
			return err
		}
		if err := s.commitState(tx, next, nextSty); err != nil {
			return fmt.Errorf("commit %s: %w", op, err)
		}
		d = out
		return nil
	})
	if err != nil {
		return Dirty{}, err
	}
	d.Stats.Elapsed = time.Since(start)
	return d, nil
}

// runStructural is the whole mutation against one transaction, returning the
// dirty set and the rank<->key mapping the sheet has afterwards. The phases are
// a claim about what a structural mutation is allowed to touch:
//
//	steps 1-4 are O(what the mutation destroys) — indexed probes, not scans
//	step 5    is O(rows in one band), never O(sheet)
//	steps 6-8 are O(what actually changed value)
func runStructural(tx *sql.Tx, bi *bandIndex, op shiftOp) (Dirty, *bandIndex, error) {
	var st MutateStats
	bands := map[int]struct{}{}

	// The row axis is as long as the sheet is; step 1 raises it to bi.rows+n if
	// the insert turns out to need rows the sheet does not have.
	if op.axis == axisRow {
		op.lim = bi.rows
	}

	// ── 1. Where the rows come from, and the event ───────────────────────────
	//
	// An insert takes its n rows either from the blank tail at the bottom (the
	// extent is unchanged) or from nowhere (the sheet grows by n). It has to be
	// decided first, because the answer sets the axis length that steps 3 and 6
	// map every reference through.
	//
	// The event is appended second, as in WriteCell: everything after this point
	// could fail, and having the log row already inside the transaction means
	// the rollback removes it, so "event written, mutation lost" is unreachable.
	switch {
	case op.del:
		// A delete destroys rows on purpose; there is nothing to decide.
	case op.axis == axisCol:
		// The column axis is fixed, so it refuses rather than growing.
		if err := checkOverflow(tx, op); err != nil {
			return Dirty{}, nil, err
		}
	default:
		grows, err := rowInsertGrows(tx, bi, op)
		if err != nil {
			return Dirty{}, nil, err
		}
		if grows {
			if bi.rows+op.n > RowCeiling {
				return Dirty{}, nil, fmt.Errorf(
					"%w: %s would take the sheet to %d rows, past the %d-row ceiling (it has %d)",
					ErrRowCeiling, op, bi.rows+op.n, RowCeiling, bi.rows)
			}
			op.lim = bi.rows + op.n
			op.grow = true
		}
	}
	if err := appendEvent(tx,
		"#"+op.axis.String()+"s",
		fmt.Sprintf("%s %d %d", op.verb(), op.at, op.n),
		"",
	); err != nil {
		return Dirty{}, nil, fmt.Errorf("append event for %s: %w", op, err)
	}

	// ── 2. Which bands can possibly change ───────────────────────────────────
	//
	// Everything at or past the first surviving content on the mutated axis
	// moves; everything before it was empty and stays empty, so an empty sheet
	// reports an empty dirty set.
	if err := seedBands(tx, bi, op, bands); err != nil {
		return Dirty{}, nil, err
	}

	// ── 3. The references this mutation destroys ─────────────────────────────
	//
	// Asked of the pre-mutation coordinates. The only references a mutation can
	// get wrong are the ones naming a row that ceases to exist: every surviving
	// row keeps its key and therefore its meaning, however far its rank moves.
	scanStart := time.Now()
	dead := deadRanges(bi, op)
	broken, err := scanBrokenRefs(tx, bi, op, dead)
	if err != nil {
		return Dirty{}, nil, err
	}
	st.FormulasSeen += len(broken)
	st.FormulasRewrit = len(broken)
	st.Scan = time.Since(scanStart)

	// ── 4. Formulas above the mutation whose displayed text changes ──────────
	//
	// A formula in row 1 reading `=A9000*2` does not move and needs no write:
	// its reference names a key that did not move. But the A1 text it renders as
	// changes, so the viewer looking at it has to be told.
	//
	// It must run after the scan above: on the column axis heldColRefBands
	// rewrites reference columns in place, so a broken-reference scan reading
	// them afterwards would map an already-mapped column a second time.
	held, err := heldRefBands(tx, bi, op, bands)
	if err != nil {
		return Dirty{}, nil, err
	}
	st.FormulasSeen += held

	// ── 5. Move the geometry ─────────────────────────────────────────────────
	shiftStart := time.Now()
	next, err := applyStructural(tx, bi, op, dead, &st)
	if err != nil {
		return Dirty{}, nil, err
	}
	st.Grew = next.rows - bi.rows
	if op.axis == axisCol {
		if err := shiftColMeta(tx, op); err != nil {
			return Dirty{}, nil, err
		}
	}
	st.ShiftCells = time.Since(shiftStart)

	// ── 6. Repair the references the mutation destroyed ──────────────────────
	nodes, err := repairBrokenRefs(tx, next, op, broken, bands)
	if err != nil {
		return Dirty{}, nil, err
	}

	// ── 7. Recompute, in dependency order ────────────────────────────────────
	recalcStart := time.Now()
	src := newTxCells(tx, next)
	changed, err := recomputeFormulas(tx, next, src, nodes)
	if err != nil {
		return Dirty{}, nil, err
	}
	st.Recomputed = len(changed.evaluated)
	st.Queries, st.CellsRead = src.queries, src.cellsRead
	st.Recalc = time.Since(recalcStart)

	// ── 8. Persist ───────────────────────────────────────────────────────────
	persistStart := time.Now()
	if err := writeBack(tx, next, changed.cells); err != nil {
		return Dirty{}, nil, err
	}
	st.CellsWritten = len(changed.cells)
	st.WriteCells = time.Since(persistStart)

	for _, c := range changed.cells {
		bands[c.Ref.Band()] = struct{}{}
	}

	out := make([]int, 0, len(bands))
	for b := range bands {
		out = append(out, b)
	}
	sort.Ints(out)
	st.KeyBands = len(next.bands)
	return Dirty{Bands: out, Structural: true, Stats: st}, next, nil
}

// rowInsertGrows decides where a row insert's n new rows come from: the blank
// rows already at the bottom of the sheet, or n rows the sheet does not have. It
// grows when the last n rows hold content, so reusing them would destroy it, or
// when the sheet has no n-row tail at or below `at` to reuse.
//
// "Holds content" is asked of stored cells, so a row that was typed in and
// cleared is blank; a style counts, since an empty yellow cell is something the
// user put there. A row-level style needs its own probe because it is not a cell
// — a styled row with nothing typed in it has no row in `cells` at all —
// and counting it costs one more allocated row rather than a refusal, unlike the
// column level in checkOverflow.
//
// Both probes are primary-key seeks: keys ascend with display rank, so "the last
// n rows" is a key range on either table.
func rowInsertGrows(tx *sql.Tx, bi *bandIndex, op shiftOp) (bool, error) {
	if op.n > bi.rows-op.at {
		return true, nil
	}
	tail := bi.keyOf(bi.rows - op.n)
	var k int64
	var col int
	err := tx.QueryRow(fmt.Sprintf(
		`SELECT k, col FROM cells
		  WHERE k >= %d AND (raw <> '' OR computed <> '' OR kind <> 0 OR style <> 0)
		  LIMIT 1`, tail)).Scan(&k, &col)
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("tail probe for %s: %w", op, err)
	}
	err = tx.QueryRow(fmt.Sprintf(
		`SELECT k FROM rows WHERE k >= %d AND style <> 0 LIMIT 1`, tail)).Scan(&k)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("row-state tail probe for %s: %w", op, err)
	}
	return true, nil
}

// checkOverflow enforces the bounds rule for a column insert: content in the
// last n columns would be pushed past the fixed right-hand edge, so refuse.
// Empty cells are allowed to fall off. The row axis has no equivalent — it grows
// instead, see rowInsertGrows.
//
// A column-level style deliberately does not count as content: the axis is fixed
// at MaxCols, so counting it would turn "I coloured column Z" into "this sheet
// can never accept a column insert again", and it destroys no data — a yellow
// column Z is one integer in `cols`, where a yellow cell Z9 is a row of `cells`
// the user made. A column width in the same row does not block an insert either;
// shiftColMeta maps it and drops it if the column is gone.
func checkOverflow(tx *sql.Tx, op shiftOp) error {
	var k int64
	var col int
	err := tx.QueryRow(fmt.Sprintf(
		`SELECT k, col FROM cells
		  WHERE col >= %d AND (raw <> '' OR computed <> '' OR kind <> 0 OR style <> 0)
		  LIMIT 1`, op.limit()-op.n)).Scan(&k, &col)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("overflow check for %s: %w", op, err)
	}
	return fmt.Errorf("%w: %s would destroy content in column %d (%ss %d..%d hold content)",
		ErrWouldTruncate, op, col, op.axis, op.limit()-op.n, op.limit()-1)
}

// seedBands adds the bands that the geometry change alone makes dirty. For a
// row op that is BandOf(first surviving content at or past `at`) .. BandOf(DefaultRows-1);
// for a column op every row that holds content at or past `at` is redrawn, so
// it is the band range those rows span.
func seedBands(tx *sql.Tx, bi *bandIndex, op shiftOp, bands map[int]struct{}) error {
	var lo, hi int
	if op.axis == axisRow {
		// The rows an insert creates are dirty whether or not anything moved.
		// For an insert at the top the content probe below says the same thing;
		// for an append at the bottom of an otherwise untouched sheet this is
		// the only dirty band, where the probe alone would say nothing changed.
		if op.grows() {
			for b := BandOf(op.at); b <= BandOf(op.limit()-1); b++ {
				bands[b] = struct{}{}
			}
		}
		// One primary-key seek: the first key at or past the mutation point.
		// keyAt, not keyOf, because `at` may be the append position, and deadKey
		// is negative — `k >= -1` matches the whole table.
		var k int64
		err := tx.QueryRow(
			`SELECT k FROM cells WHERE k >= ? ORDER BY k LIMIT 1`, bi.keyAt(op.at)).Scan(&k)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // nothing at or past the mutation point: nothing moved
		}
		if err != nil {
			return fmt.Errorf("dirty band probe for %s: %w", op, err)
		}
		// Everything from the first surviving content down to the bottom of the
		// sheet is renumbered — the bottom being the extent the sheet has after
		// the mutation, which for a growing insert is further down than it was.
		lo, hi = bi.rankOf(k), max(bi.rows, op.limit())-1
	} else {
		var loK, hiK sql.NullInt64
		if err := tx.QueryRow(
			`SELECT MIN(k), MAX(k) FROM cells WHERE col >= ?`, op.at).Scan(&loK, &hiK); err != nil {
			return fmt.Errorf("dirty band probe for %s: %w", op, err)
		}
		if !loK.Valid {
			return nil
		}
		// A column op moves cells sideways only. Rows outside [lo, hi] are
		// untouched, so claiming BandOf(DefaultRows-1) would be a lie that wakes every viewer.
		lo, hi = bi.rankOf(loK.Int64), bi.rankOf(hiK.Int64)
	}
	if lo < 0 {
		return nil
	}
	for b := BandOf(lo); b <= BandOf(hi); b++ {
		bands[b] = struct{}{}
	}
	return nil
}

// heldRefBands finds the formulas that do not move but whose displayed text
// changes, and adds their bands to the dirty set. It returns how many there were.
//
// Which driver is cheaper depends on where the mutation is — the same asymmetry
// that gave dependentsSQL two directions. Near the top the region above the
// mutation is tiny, so drive on the primary key; anywhere else it is most of the
// sheet, so drive on the reference indexes, which visit only formulas whose
// target is at or below the mutation point. The wrong choice scans the table.
func heldRefBands(tx *sql.Tx, bi *bandIndex, op shiftOp, bands map[int]struct{}) (int, error) {
	if op.axis != axisRow {
		return heldColRefBands(tx, bi, op, bands)
	}
	k := bi.keyAt(op.at)
	var queries []string
	// heldPKRows is where the primary-key driver stops being the cheap one.
	// The crossover is not sharp and neither is this constant.
	const heldPKRows = 500
	if op.at <= heldPKRows {
		queries = []string{fmt.Sprintf(
			`SELECT DISTINCT k FROM cells
			   WHERE k < %d AND (ref0_k >= %d OR ref1_k >= %d)`, k, k, k)}
	} else {
		queries = []string{
			fmt.Sprintf(`SELECT k FROM cells
			   WHERE ref_span = 0 AND ref0_k IS NOT NULL AND ref0_k >= %d AND k < %d`, k, k),
			fmt.Sprintf(`SELECT k FROM cells
			   WHERE ref_span = 0 AND ref1_k IS NOT NULL AND ref1_k >= %d AND k < %d`, k, k),
			// One span arm, not two: a range's endpoints are ordered, so
			// `ref1_k >= K` is true of every range with either endpoint at or
			// below the mutation point.
			fmt.Sprintf(`SELECT k FROM cells INDEXED BY cells_span_hi
			   WHERE ref_span = 1 AND ref1_k >= %d AND k < %d`, k, k),
		}
	}
	seen := map[int64]struct{}{}
	for _, q := range queries {
		rows, err := tx.Query(q)
		if err != nil {
			return 0, fmt.Errorf("held-ref probe for %s: %w", op, err)
		}
		for rows.Next() {
			var owner int64
			if err := rows.Scan(&owner); err != nil {
				rows.Close()
				return 0, fmt.Errorf("held-ref scan: %w", err)
			}
			seen[owner] = struct{}{}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("held-ref probe: %w", err)
		}
	}
	for owner := range seen {
		if r := bi.rankOf(owner); r >= 0 {
			bands[BandOf(r)] = struct{}{}
		}
	}
	return len(seen), nil
}

// heldColRefBands is the column-axis half, which unlike the row axis has real
// work to do: a column op moves no keys, so a reference's column component is
// still a display coordinate that has to be rewritten. Cells at or right of the
// mutation get theirs mapped by the statement that shifts them; the ones to the
// left need this UPDATE. There is no index on the column components alone, so it
// is a scan — the axis is 26 wide.
func heldColRefBands(tx *sql.Tx, bi *bandIndex, op shiftOp, bands map[int]struct{}) (int, error) {
	a, b := op.slotKeys()
	where := fmt.Sprintf(`col < %d AND (%s >= %d OR %s >= %d)`, op.at, a, op.at, b, op.at)
	rows, err := tx.Query(`SELECT DISTINCT k FROM cells WHERE ` + where)
	if err != nil {
		return 0, fmt.Errorf("held-ref probe for %s: %w", op, err)
	}
	for rows.Next() {
		var owner int64
		if err := rows.Scan(&owner); err != nil {
			rows.Close()
			return 0, fmt.Errorf("held-ref scan: %w", err)
		}
		if r := bi.rankOf(owner); r >= 0 {
			bands[BandOf(r)] = struct{}{}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("held-ref probe: %w", err)
	}
	res, err := tx.Exec(fmt.Sprintf(`UPDATE cells SET %s = %s, %s = %s WHERE %s`,
		a, op.mapped(a), b, op.mapped(b), where))
	if err != nil {
		return 0, fmt.Errorf("shift held refs for %s: %w", op, err)
	}
	return affected(res), nil
}

// ─── Moving the geometry ──────────────────────────────────────────────────────

// deadRanges is the key ranges whose display rows cease to exist: the run a
// delete removes, or the blank tail an insert reuses. A column op destroys cells
// but not rows, so it has none — its destruction is in the column predicate
// instead. An insert that grows the sheet destroys nothing, so it too has none.
func deadRanges(bi *bandIndex, op shiftOp) []keyRange {
	if op.axis != axisRow {
		return nil
	}
	if !op.del {
		if op.grows() {
			return nil
		}
		return bi.clone().dropTail(op.n)
	}
	var out []keyRange
	i, slot := bi.locate(op.at)
	rem := op.n
	for rem > 0 && i < len(bi.bands) {
		band := bi.bands[i]
		if avail := band.nrows - slot; avail > 0 {
			take := min(avail, rem)
			out = append(out, keyRange{
				lo: band.base + int64(slot),
				hi: band.base + int64(slot+take) - 1,
			})
			rem -= take
		}
		i++
		slot = 0
	}
	return out
}

// applyStructural performs the mutation on storage and returns the mapping the
// sheet has afterwards.
func applyStructural(tx *sql.Tx, bi *bandIndex, op shiftOp, dead []keyRange, st *MutateStats) (*bandIndex, error) {
	if op.axis == axisCol {
		moved, dropped, err := shiftCols(tx, op)
		st.CellsMoved, st.CellsDropped = moved, dropped
		return bi, err
	}
	return applyRowOp(tx, bi, op, dead, st)
}

// applyRowOp is the storage model in one function: a row insert or delete
// touches the cells of the rows that cease to exist, the cells below the
// mutation point inside its own band, and one integer per band. Everything else
// is renumbered by that integer and is not read, written, or locked.
func applyRowOp(tx *sql.Tx, bi *bandIndex, op shiftOp, dead []keyRange, st *MutateStats) (*bandIndex, error) {
	// 1. The rows that cease to exist — their cells and their per-row state,
	//    because "this row is gone" is one fact and leaving half of it behind
	//    would file a row style at a key the next insert hands to a different
	//    row.
	dropped := 0
	for _, r := range dead {
		res, err := tx.Exec(fmt.Sprintf(`DELETE FROM cells WHERE k BETWEEN %d AND %d`, r.lo, r.hi))
		if err != nil {
			return nil, fmt.Errorf("drop rows for %s: %w", op, err)
		}
		dropped += affected(res)
		if err := dropRowMeta(tx, r); err != nil {
			return nil, err
		}
	}
	st.CellsDropped = dropped

	work := bi.clone()
	if op.del {
		// 2a. Delete: close the gap inside each affected band. The sheet is now
		//     n rows shorter; how much of that is given back is decided below.
		i, slot := work.locate(op.at)
		rem := op.n
		for rem > 0 && i < len(work.bands) {
			band := work.bands[i]
			if avail := band.nrows - slot; avail > 0 {
				take := min(avail, rem)
				if rest := band.nrows - (slot + take); rest > 0 {
					moved, err := shiftKeyRange(tx,
						keyRange{lo: band.base + int64(slot+take), hi: band.last()}, -int64(take))
					if err != nil {
						return nil, err
					}
					st.CellsMoved += moved
				}
				work.bands[i].nrows -= take
				rem -= take
			}
			i++
			slot = 0
		}
		work.reindex()
		//     The extent then shrinks, down to the DefaultRows floor. Giving the
		//     rows back unconditionally would make the extent monotonic — delete
		//     a million rows and the scrollbar still describes a sheet a million
		//     rows long. The floor also keeps this symmetric with insert: below
		//     it neither operation changes the extent, so insert/delete cycles
		//     do not drift, and above it both move it by n.
		if back := DefaultRows - work.rows; back > 0 {
			work.appendTail(back)
		}
		if err := reshapeIfCrowded(tx, work, st); err != nil {
			return nil, err
		}
		return work, syncBandIndex(tx, bi, work)
	}

	// 2b. Insert: give the index the shape the database is about to have, so
	//     that reshaping below reasons about the real sheet — where a band past
	//     bandSplitRows gets split, and a band with no slack forces a rebalance.
	//
	//     Reusing a blank tail and growing differ by one line. The step below
	//     adds op.n rows to the band at the insertion point and is the only
	//     place rows are added, so dropping n off the bottom first leaves the
	//     extent where it was and not dropping them leaves it n taller. Growth
	//     needs no cell writes and no key allocation.
	if !op.grows() {
		work.dropTail(op.n)
	}
	if err := reshapeForInsert(tx, work, op, st); err != nil {
		return nil, err
	}
	b, slot := work.locate(op.at)
	band := work.bands[b]
	if band.free() < op.n {
		return nil, fmt.Errorf("%w: band %d has %d free slots, need %d",
			ErrBadMutation, b, band.free(), op.n)
	}
	if r := (keyRange{lo: band.base + int64(slot), hi: band.last()}); !r.empty() {
		moved, err := shiftKeyRange(tx, r, int64(op.n))
		if err != nil {
			return nil, err
		}
		st.CellsMoved += moved
	}
	work.bands[b].nrows += op.n
	work.reindex()
	return work, syncBandIndex(tx, bi, work)
}

// shiftKeyRange moves a contiguous run of keys by delta, taking the references
// that name those keys with it. It is the one primitive every row mutation is
// made of: insert, delete and band split are the same statement with a different
// range and sign, over one band rather than the sheet.
//
// Copy / delete / re-insert, not UPDATE, because the thing being changed is the
// primary key of a WITHOUT ROWID table and an in-place `SET k = k + 1` collides
// with the row it is about to move.
func shiftKeyRange(tx *sql.Tx, r keyRange, delta int64) (int, error) {
	if r.empty() || delta == 0 {
		return 0, nil
	}
	// `style` rides along in the copy, which is why it is a column on `cells`
	// and not a side table: it follows its cell through a structural mutation
	// with no second table to shift and no join to keep in step.
	if err := scratch(tx, "mut_cells", fmt.Sprintf(
		`SELECT k + %d AS nk, col, raw, computed, kind,
		        ref0_k, ref0_col, ref1_k, ref1_col, ref_span, style
		   FROM cells WHERE k BETWEEN %d AND %d`, delta, r.lo, r.hi)); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(fmt.Sprintf(
		`DELETE FROM cells WHERE k BETWEEN %d AND %d`, r.lo, r.hi)); err != nil {
		return 0, fmt.Errorf("clear shifted keys %d..%d: %w", r.lo, r.hi, err)
	}
	ins, err := tx.Exec(
		`INSERT INTO cells (k, col, raw, computed, kind, ` + slotCols + `, ref_span, style)
		   SELECT nk, col, raw, computed, kind, ref0_k, ref0_col, ref1_k, ref1_col,
		          ref_span, style
		     FROM temp.mut_cells`)
	if err != nil {
		return 0, fmt.Errorf("re-insert shifted keys %d..%d: %w", r.lo, r.hi, err)
	}
	moved := affected(ins)
	if _, err := tx.Exec(`DROP TABLE temp.mut_cells`); err != nil {
		return 0, fmt.Errorf("drop scratch table: %w", err)
	}
	// The per-row state of the same keys, by the same delta, from the same
	// place — so insert, delete and band split all carry row styles without
	// knowing that table exists. It is why the table is keyed by `k`: a row that
	// does not move needs no write however far its display rank shifts, which on
	// a top-of-sheet insert is every row but the one band being shifted.
	if err := shiftRowMeta(tx, r, delta); err != nil {
		return 0, err
	}

	// The references that name the rows that moved. Four indexed range updates,
	// one per partial index, each bounded by the number of references into this
	// band.
	for _, q := range []string{
		`UPDATE cells SET ref0_k = ref0_k + %d
		   WHERE ref_span = 0 AND ref0_k IS NOT NULL AND ref0_k BETWEEN %d AND %d`,
		`UPDATE cells SET ref1_k = ref1_k + %d
		   WHERE ref_span = 0 AND ref1_k IS NOT NULL AND ref1_k BETWEEN %d AND %d`,
		`UPDATE cells SET ref0_k = ref0_k + %d
		   WHERE ref_span = 1 AND ref0_k BETWEEN %d AND %d`,
		`UPDATE cells SET ref1_k = ref1_k + %d
		   WHERE ref_span = 1 AND ref1_k BETWEEN %d AND %d`,
	} {
		if _, err := tx.Exec(fmt.Sprintf(q, delta, r.lo, r.hi)); err != nil {
			return 0, fmt.Errorf("remap references over %d..%d: %w", r.lo, r.hi, err)
		}
	}
	return moved, nil
}

// reshapeForInsert makes room and keeps the next insert cheap. Those are two
// different needs and conflating them is how this goes wrong: a split bounds the
// shift, since a band that has absorbed inserts holds more rows and every future
// insert into it moves all of them; a rebalance makes room, because a split
// halves the interval it divides and so gives no new slack. When a band has none
// left — a few hundred inserts at one position, or one insert of a few thousand
// rows — re-keying the sheet with fresh spacing is the only answer, and it is
// the one O(sheet) operation here.
func reshapeForInsert(tx *sql.Tx, bi *bandIndex, op shiftOp, st *MutateStats) error {
	for op.n <= bandSplitRows {
		b, _ := bi.locate(op.at)
		if bi.bands[b].nrows+op.n <= bandSplitRows || !bi.canSplit(b) {
			break
		}
		lo, hi, delta := bi.splitAt(b)
		moved, err := shiftKeyRange(tx, keyRange{lo: lo, hi: hi}, delta)
		if err != nil {
			return fmt.Errorf("split band %d: %w", b, err)
		}
		st.CellsMoved += moved
		st.BandsSplit++
	}
	b, _ := bi.locate(op.at)
	if bi.bands[b].free() >= op.n && len(bi.bands) <= maxKeyBandsFor(bi.rows) {
		return nil
	}
	return rebalance(tx, bi, op.n, st)
}

// reshapeIfCrowded rebalances a sheet that has accumulated more storage bands
// than it should. Splits add bands and deletes never remove them, so without a
// ceiling a long-lived sheet grows an index that every read has to search.
func reshapeIfCrowded(tx *sql.Tx, bi *bandIndex, st *MutateStats) error {
	if len(bi.bands) <= maxKeyBandsFor(bi.rows) {
		return nil
	}
	return rebalance(tx, bi, 0, st)
}

// rebalance re-keys the whole sheet into the canonical layout: one band per
// BandHeight display rows, evenly spaced, with `need` slots of headroom for the
// caller's insert. It is the O(sheet) operation the band model exists to avoid,
// kept because the alternative is failing a legal mutation.
//
// Every key in the sheet changes, so every stored reference key changes with it
// — through a rank-to-key map rather than arithmetic, because the mapping
// between two arbitrary layouts is piecewise and getting one piece wrong would
// silently repoint a formula.
func rebalance(tx *sql.Tx, bi *bandIndex, need int, st *MutateStats) error {
	stride := int64(keyStride)
	for stride < int64(BandHeight+need+1) {
		stride *= 2
	}
	// The layout is built for the rows the sheet has right now — during an
	// insert, the extent minus any tail already dropped. A constant here would
	// hand back a grid of the wrong height and silently move every row.
	next := layoutFor(bi.rows, stride)

	// The map is declared rather than derived from a SELECT, and the difference
	// is orders of magnitude. `CREATE TEMP TABLE AS SELECT` gives its columns no
	// type affinity, and the planner then refuses to drive the two reference
	// joins below off an index on them: it re-scans the whole map once per cell.
	// Declared WITHOUT ROWID with an INTEGER primary key, all three joins are
	// primary-key searches.
	if _, err := tx.Exec(`DROP TABLE IF EXISTS temp.kmap`); err != nil {
		return fmt.Errorf("reset rebalance map: %w", err)
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE kmap (
		old_k INTEGER PRIMARY KEY, new_k INTEGER NOT NULL) WITHOUT ROWID`); err != nil {
		return fmt.Errorf("create rebalance map: %w", err)
	}
	if err := bulkInsert(tx, bi.rows, 2, `INSERT INTO temp.kmap (old_k, new_k) VALUES `, ``,
		func(i int, args []any) []any {
			return append(args, bi.keyOf(i), next.keyOf(i))
		}); err != nil {
		return fmt.Errorf("build rebalance map: %w", err)
	}

	var before int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM cells`).Scan(&before); err != nil {
		return fmt.Errorf("count cells before rebalance: %w", err)
	}
	// A reference key that is not a live rank cannot be repointed, so it becomes
	// #REF! rather than quietly becoming "no operand at all".
	if err := scratch(tx, "rebal", `
		SELECT m.new_k AS k, c.col, c.raw, c.computed, c.kind,
		       CASE WHEN c.ref0_k IS NULL THEN NULL
		            WHEN c.ref0_k < 0 THEN -1
		            ELSE COALESCE(m0.new_k, -1) END AS ref0_k,
		       c.ref0_col,
		       CASE WHEN c.ref1_k IS NULL THEN NULL
		            WHEN c.ref1_k < 0 THEN -1
		            ELSE COALESCE(m1.new_k, -1) END AS ref1_k,
		       c.ref1_col, c.ref_span, c.style
		  FROM cells c
		  JOIN temp.kmap m ON m.old_k = c.k
		  LEFT JOIN temp.kmap m0 ON m0.old_k = c.ref0_k
		  LEFT JOIN temp.kmap m1 ON m1.old_k = c.ref1_k`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM cells`); err != nil {
		return fmt.Errorf("clear cells for rebalance: %w", err)
	}
	res, err := tx.Exec(
		`INSERT INTO cells (k, col, raw, computed, kind, ` + slotCols + `, ref_span, style)
		   SELECT k, col, raw, computed, kind, ref0_k, ref0_col, ref1_k, ref1_col,
		          ref_span, style
		     FROM temp.rebal`)
	if err != nil {
		return fmt.Errorf("re-key cells: %w", err)
	}
	if got := affected(res); got != before {
		return fmt.Errorf("rebalance re-keyed %d of %d cells: some key named no display row",
			got, before)
	}
	// The per-row state goes through the same map, before it is dropped. A
	// rebalance changes every key in the sheet, so this is the one path where
	// "keyed by k costs nothing" is false — and it is bounded by the number of
	// styled rows, not by the sheet.
	if err := rekeyRowMeta(tx); err != nil {
		return err
	}
	for _, t := range []string{"temp.rebal", "temp.kmap"} {
		if _, err := tx.Exec(`DROP TABLE ` + t); err != nil {
			return fmt.Errorf("drop %s: %w", t, err)
		}
	}
	st.CellsMoved += before
	st.Rebalanced = true
	*bi = *next
	return nil
}

// ─── The column axis, which is O(sheet) ───────────────────────────────────────

// cellKey is the `cells` column this op shifts. Only the column axis has one:
// a row op changes no column of any row, it changes which rows exist.
func (op shiftOp) cellKey() string { return `col` }

// mapped is the SQL expression for a column's post-mutation value.
func (op shiftOp) mapped(col string) string {
	if op.del {
		return fmt.Sprintf(`CASE WHEN %s >= %d THEN %s - %d ELSE %s END`,
			col, op.at+op.n, col, op.n, col)
	}
	return fmt.Sprintf(`CASE WHEN %s >= %d THEN %s + %d ELSE %s END`,
		col, op.at, col, op.n, col)
}

// touched is the SQL predicate for "this row is affected by the mutation".
func (op shiftOp) touched(col string) string {
	return fmt.Sprintf(`%s >= %d`, col, op.at)
}

// survives is the SQL predicate for "this row still exists afterwards".
func (op shiftOp) survives(col string) string {
	if op.del {
		return fmt.Sprintf(`(%s < %d OR %s >= %d)`, col, op.at, col, op.at+op.n)
	}
	return fmt.Sprintf(`(%s < %d OR %s < %d)`, col, op.at, col, op.limit()-op.n)
}

// shiftCols moves the affected slice of `cells` sideways. It is O(sheet): a
// column is not a contiguous range of the primary key, so inserting one rewrites
// every cell to its right in every row. Bounding it would need a column key with
// its own bands; the axis is 26 wide, so the worst case is a fixed multiple of
// the sheet rather than an unbounded one, and it is left as it is.
func shiftCols(tx *sql.Tx, op shiftOp) (moved, dropped int, err error) {
	key := op.cellKey()
	// An existence probe, not a count: COUNT(*) here is a full scan of the table
	// to compute a statistic. The row counts below come from the DELETE and
	// INSERT that have to run anyway.
	any, err := existsWhere(tx, "cells", op.touched(key))
	if err != nil {
		return 0, 0, err
	}
	if !any {
		return 0, 0, nil
	}
	if err := scratch(tx, "mut_cells", fmt.Sprintf(
		`SELECT k, %s AS k_col, raw, computed, kind,
		        ref0_k, %s AS s0c, ref1_k, %s AS s1c, ref_span, style
		   FROM cells WHERE %s AND %s`,
		op.mapped(`col`), op.mapped("ref0_col"), op.mapped("ref1_col"),
		op.touched(key), op.survives(key))); err != nil {
		return 0, 0, err
	}
	del, err := tx.Exec(fmt.Sprintf(`DELETE FROM cells WHERE %s`, op.touched(key)))
	if err != nil {
		return 0, 0, fmt.Errorf("clear shifted cells for %s: %w", op, err)
	}
	ins, err := tx.Exec(
		`INSERT INTO cells (k, col, raw, computed, kind, ` + slotCols + `, ref_span, style)
		   SELECT k, k_col, raw, computed, kind, ref0_k, s0c, ref1_k, s1c, ref_span, style
		     FROM temp.mut_cells`)
	if err != nil {
		return 0, 0, fmt.Errorf("re-insert shifted cells for %s: %w", op, err)
	}
	if _, err := tx.Exec(`DROP TABLE temp.mut_cells`); err != nil {
		return 0, 0, fmt.Errorf("drop scratch table: %w", err)
	}
	touchedN, moved := affected(del), affected(ins)
	return moved, touchedN - moved, nil
}

// affected is RowsAffected with the error folded away. These counts are
// statistics for the cost breakdown, never control flow, so a driver that
// declines to report one must not fail the mutation.
func affected(r sql.Result) int {
	n, err := r.RowsAffected()
	if err != nil {
		return 0
	}
	return int(n)
}

// shiftColMeta moves stored per-column state — the width and the column level of
// the style cascade — with its column. At most MaxCols rows, so it is read /
// clear / re-insert in Go rather than another scratch table, and the two travel
// in one statement because they are one row.
//
// A column pushed off the right-hand edge takes its width and its style with it;
// see checkOverflow for why that is not a refusal.
func shiftColMeta(tx *sql.Tx, op shiftOp) error {
	meta, err := readColMeta(tx)
	if err != nil {
		return err
	}
	if len(meta) == 0 {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM cols`); err != nil {
		return fmt.Errorf("clear column state for %s: %w", op, err)
	}
	for col, m := range meta {
		nc, ok := op.point(col)
		if !ok {
			continue // that column is gone
		}
		if _, err := tx.Exec(
			`INSERT INTO cols (col, width, style) VALUES (?, ?, ?)
			   ON CONFLICT(col) DO UPDATE SET width = excluded.width, style = excluded.style`,
			nc, m.width, m.style); err != nil {
			return fmt.Errorf("move column state %d->%d: %w", col, nc, err)
		}
	}
	return nil
}

// scratch creates temp.<name> from a SELECT, dropping any leftover first. Temp
// tables are per-connection and a *sql.Tx pins one connection, so this is safe
// inside the transaction; the drop-first guards against an earlier transaction
// that failed between create and drop on the same pooled connection.
func scratch(tx *sql.Tx, name, query string) error {
	if _, err := tx.Exec(`DROP TABLE IF EXISTS temp.` + name); err != nil {
		return fmt.Errorf("reset scratch %s: %w", name, err)
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE ` + name + ` AS ` + query); err != nil {
		return fmt.Errorf("fill scratch %s: %w", name, err)
	}
	return nil
}

// existsWhere reports whether any row matches, stopping at the first one:
// COUNT(*) would visit every matching row to answer a question the first row
// already settled.
func existsWhere(tx *sql.Tx, table, where string) (bool, error) {
	var one int
	q := fmt.Sprintf(`SELECT 1 FROM %s WHERE %s LIMIT 1`, table, where)
	err := tx.QueryRow(q).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe %s: %w", table, err)
	}
	return true, nil
}

// ─── Structural references: find, translate, repair ───────────────────────────

// formulaNode is one formula cell at its post-mutation position, with its A1
// text already materialized from the template and the current slots.
type formulaNode struct {
	ref       CellRef
	raw       string // materialized A1 text, for evaluation and for the parse
	oldValue  string
	oldKind   Kind
	rewritten bool // its references did not survive as a plain translation
	expr      Formula
	perr      error
}

// brokenRef is one formula whose references the mutation destroys, captured at
// its pre-mutation address and slots, translated out of key space into display
// space so that everything downstream is the same arithmetic rewriteFormula
// performs on text and the two can be compared case for case.
type brokenRef struct {
	ref   CellRef
	tmpl  string
	slots []CellRef
	span  bool
	value string
	kind  Kind
}

// slotKeys are the two `cells` reference columns a column op moves. A row op
// moves no reference column at all — it moves keys, and the references already
// name keys.
func (op shiftOp) slotKeys() (a, b string) { return "ref0_col", "ref1_col" }

// nonUniformRefs is the column-axis predicate for "the plain shift arithmetic is
// the wrong answer for this cell's references":
//
//	insert  a point reference pushed off the right edge  -> #REF!
//	insert  a range straddling the insertion column      -> expands
//	insert  a range whose end would clamp at the edge    -> clamps
//	delete  a point reference inside the deleted run     -> #REF!
//	delete  a range overlapping the deleted run          -> shrinks
//
// The row axis needs no equivalent: a surviving row keeps its key, so the only
// references a row mutation can get wrong are the ones naming a row that is
// gone — a range query on the reference indexes rather than a predicate every
// formula in the sheet has to be asked.
func (op shiftOp) nonUniformRefs() string {
	a, b := op.slotKeys()
	if op.del {
		inRun := func(c string) string {
			return fmt.Sprintf(`(%s >= %d AND %s < %d)`, c, op.at, c, op.at+op.n)
		}
		return fmt.Sprintf(
			`((ref_span = 0 AND (%s OR %s))
			  OR (ref_span = 1 AND %s < %d AND %s >= %d))`,
			inRun(a), inRun(b), a, op.at+op.n, b, op.at)
	}
	edge := op.limit() - op.n
	return fmt.Sprintf(
		`((ref_span = 0 AND (%s >= %d OR %s >= %d))
		  OR (ref_span = 1 AND ((%s < %d AND %s >= %d) OR %s >= %d)))`,
		a, edge, b, edge, a, op.at, b, op.at, b, edge)
}

// scanBrokenRefs collects the formulas whose references the mutation destroys.
// Cells that do not survive are excluded: their formulas are about to cease to
// exist, so there is nothing to repair.
func scanBrokenRefs(tx *sql.Tx, bi *bandIndex, op shiftOp, dead []keyRange) ([]brokenRef, error) {
	if op.axis == axisCol {
		return scanBrokenColRefs(tx, bi, op)
	}
	if len(dead) == 0 {
		return nil, nil
	}
	inDead := func(k int64) bool {
		for _, r := range dead {
			if k >= r.lo && k <= r.hi {
				return true
			}
		}
		return false
	}
	type owner struct {
		k   int64
		col int
	}
	seen := map[owner]struct{}{}
	var out []brokenRef
	for _, r := range dead {
		// One arm per partial index, so each is an indexed range probe over the
		// destroyed keys rather than a scan of the table.
		arms := []string{
			`ref_span = 0 AND ref0_k IS NOT NULL AND ref0_k BETWEEN %d AND %d`,
			`ref_span = 0 AND ref1_k IS NOT NULL AND ref1_k BETWEEN %d AND %d`,
			`ref_span = 1 AND ref0_k BETWEEN %d AND %d`,
			`ref_span = 1 AND ref1_k BETWEEN %d AND %d`,
		}
		if op.del {
			// A range that merely contains a deleted row is not broken — both
			// endpoints survive and neither key moves — but its value changes,
			// because a cell it sums ceased to exist: `=SUM(A1:A4)` over a
			// delete of A2 keeps its endpoints and loses a term. The overlap has
			// to be asked as an interval query, which is what the span indexes
			// are for, bounded by the number of range formulas.
			arms = append(arms,
				`ref_span = 1 AND ref0_k <= %[2]d AND ref1_k >= %[1]d`)
		}
		for _, where := range arms {
			rows, err := tx.Query(fmt.Sprintf(
				`SELECT k, col, raw, computed, kind, `+slotCols+`, ref_span FROM cells
				  WHERE `+where, r.lo, r.hi))
			if err != nil {
				return nil, fmt.Errorf("scan broken refs for %s: %w", op, err)
			}
			for rows.Next() {
				b, k, err := scanBrokenRow(bi, rows)
				if err != nil {
					rows.Close()
					return nil, err
				}
				o := owner{k, b.ref.Col}
				if _, dup := seen[o]; dup || inDead(k) {
					continue
				}
				seen[o] = struct{}{}
				out = append(out, b)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("scan broken refs: %w", err)
			}
		}
	}
	sortBrokenRefs(out)
	return out, nil
}

// scanBrokenColRefs is the column-axis half: one predicate scan, because a
// column op moves no keys and so has no destroyed key range to probe.
func scanBrokenColRefs(tx *sql.Tx, bi *bandIndex, op shiftOp) ([]brokenRef, error) {
	q := fmt.Sprintf(
		`SELECT k, col, raw, computed, kind, %s, ref_span FROM cells
		  WHERE ref0_k IS NOT NULL AND %s AND %s
		  ORDER BY k, col`,
		slotCols, op.nonUniformRefs(), op.survives(op.cellKey()))
	rows, err := tx.Query(q)
	if err != nil {
		return nil, fmt.Errorf("scan broken refs for %s: %w", op, err)
	}
	defer rows.Close()
	var out []brokenRef
	for rows.Next() {
		b, _, err := scanBrokenRow(bi, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan broken refs: %w", err)
	}
	return out, nil
}

// scanBrokenRow reads one row of a broken-reference query, translating keys
// into display coordinates.
func scanBrokenRow(bi *bandIndex, rows *sql.Rows) (brokenRef, int64, error) {
	var b brokenRef
	var k int64
	var kind, span int
	var slots nullSlots
	args := append([]any{&k, &b.ref.Col, &b.tmpl, &b.value, &kind}, slots.scanArgs()...)
	args = append(args, &span)
	if err := rows.Scan(args...); err != nil {
		return brokenRef{}, 0, fmt.Errorf("scan broken ref: %w", err)
	}
	b.ref.Row = bi.rankOf(k)
	b.kind, b.slots, b.span = Kind(kind), slots.refs(bi), span != 0
	return b, k, nil
}

func sortBrokenRefs(out []brokenRef) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].ref.Row != out[j].ref.Row {
			return out[i].ref.Row < out[j].ref.Row
		}
		return out[i].ref.Col < out[j].ref.Col
	})
}

// mapSlots maps one formula's stored references through the mutation. The two
// shapes are why ref_span is stored: a range's endpoints move by the span rule
// and it dies all at once or not at all, where independent operands move by the
// point rule and each dies on its own.
func (op shiftOp) mapSlots(slots []CellRef, span bool) (out []CellRef, dead bool) {
	if span {
		if len(slots) != 2 {
			return slots, false
		}
		lo, hi := slots[0], slots[1]
		var ok bool
		if op.axis == axisRow {
			lo.Row, hi.Row, ok = op.span(slots[0].Row, slots[1].Row)
		} else {
			lo.Col, hi.Col, ok = op.span(slots[0].Col, slots[1].Col)
		}
		if !ok {
			return nil, true
		}
		return []CellRef{lo, hi}, false
	}
	out = make([]CellRef, len(slots))
	for i, s := range slots {
		if n, ok := op.ref(s); ok {
			out[i] = n
		} else {
			out[i] = deadRef
		}
	}
	return out, false
}

// deadRef is the display-space coordinate of a destroyed reference. It is off
// the grid, so CellRef.String already renders it as #REF!, and it maps to
// deadKey on the way into storage.
var deadRef = CellRef{Row: -1, Col: -1}

// repairBrokenRefs writes the correct references for the formulas the mutation
// destroyed, and returns them as the seed of the recompute.
//
// A range that died collapses to `=SUM(#REF!)`, a text change, because two dead
// endpoints would otherwise render as `=SUM(#REF!:#REF!)`. Everything else keeps
// its template exactly as the user wrote it and only its coordinates change.
func repairBrokenRefs(tx *sql.Tx, bi *bandIndex, op shiftOp, broken []brokenRef, bands map[int]struct{}) ([]formulaNode, error) {
	if len(broken) == 0 {
		return nil, nil
	}
	out := make([]formulaNode, 0, len(broken))
	for _, b := range broken {
		ref, ok := op.ref(b.ref)
		if !ok {
			continue // the owner did not survive; nothing to repair
		}
		slots, dead := op.mapSlots(b.slots, b.span)
		tmpl, span := b.tmpl, b.span
		if dead {
			tmpl, slots, span = "=SUM("+TokenRef+")", nil, false
		}
		sa := slotArgs(bi, slots)
		if _, err := tx.Exec(
			`UPDATE cells SET raw = ?, ref0_k = ?, ref0_col = ?,
			                  ref1_k = ?, ref1_col = ?, ref_span = ?
			  WHERE k = ? AND col = ?`,
			tmpl, sa[0], sa[1], sa[2], sa[3], boolInt(span), bi.keyOf(ref.Row), ref.Col,
		); err != nil {
			return nil, fmt.Errorf("repair refs of %s: %w", ref, err)
		}
		bands[ref.Band()] = struct{}{}
		n := formulaNode{
			ref:       ref,
			raw:       RenderTemplate(tmpl, slots),
			oldValue:  b.value,
			oldKind:   b.kind,
			rewritten: true,
		}
		n.expr, n.perr = ParseFormula(n.raw)
		out = append(out, n)
	}
	return out, nil
}

// reachFrom grows the seed set into everything downstream of it, walking the
// reference columns backwards inside the transaction so it sees the edges as the
// shift and the repairs left them. It covers only the cells that can actually
// change, so a structural mutation does not scale with the sheet's formula
// count.
func reachFrom(tx *sql.Tx, bi *bandIndex, seeds []formulaNode) ([]formulaNode, error) {
	nodes := make([]formulaNode, len(seeds))
	copy(nodes, seeds)
	index := make(map[CellRef]int, len(seeds)*2)
	for i, n := range nodes {
		index[n.ref] = i
	}
	for i := 0; i < len(nodes); i++ {
		deps, err := dependentsInTx(tx, bi, nodes[i].ref)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if _, seen := index[d]; seen {
				continue
			}
			n, ok, err := loadFormulaNode(tx, bi, d)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue // the reference outlived its formula: the cell is
				// gone, or is no longer a formula at all
			}
			index[d] = len(nodes)
			nodes = append(nodes, n)
			if len(nodes) > maxRecalcNodes {
				return nil, fmt.Errorf("%w: structural mutation reaches more than %d cells",
					ErrRecalcTooLarge, maxRecalcNodes)
			}
		}
	}
	return nodes, nil
}

// dependentsInTx is Sheet.DependentsOf asked of the transaction, so an expanded
// range's new dependents are visible the moment repairBrokenRefs writes its
// endpoints.
func dependentsInTx(tx *sql.Tx, bi *bandIndex, ref CellRef) ([]CellRef, error) {
	rows, err := tx.Query(dependentsSQLFor(ref.Row, bi.rows), bi.keyOf(ref.Row), ref.Col)
	if err != nil {
		return nil, fmt.Errorf("dependents of %s in tx: %w", ref, err)
	}
	defer rows.Close()
	out, err := scanDependents(bi, rows)
	if err != nil {
		return nil, fmt.Errorf("dependents of %s in tx: %w", ref, err)
	}
	return out, nil
}

// loadFormulaNode reads one downstream cell as a node. ok is false when the cell
// is gone or is not a formula, a normal outcome of a delete: the edge is stale
// and there is nothing to recompute.
func loadFormulaNode(tx *sql.Tx, bi *bandIndex, ref CellRef) (formulaNode, bool, error) {
	n := formulaNode{ref: ref}
	var kind int
	var slots nullSlots
	args := append([]any{&n.raw, &n.oldValue, &kind}, slots.scanArgs()...)
	err := tx.QueryRow(
		`SELECT raw, computed, kind, `+slotCols+` FROM cells WHERE k = ? AND col = ?`,
		bi.keyOf(ref.Row), ref.Col).Scan(args...)
	if errors.Is(err, sql.ErrNoRows) {
		return formulaNode{}, false, nil
	}
	if err != nil {
		return formulaNode{}, false, fmt.Errorf("load dependent %s: %w", ref, err)
	}
	n.oldKind = Kind(kind)
	if refs := slots.refs(bi); refs != nil {
		n.raw = RenderTemplate(n.raw, refs)
	}
	if InferKind(n.raw) != KindFormula {
		return formulaNode{}, false, nil
	}
	n.expr, n.perr = ParseFormula(n.raw)
	return n, true, nil
}

type recomputed struct {
	evaluated []CellRef // every formula re-evaluated
	cells     []Cell    // the ones that actually changed, ready to persist
}

// recomputeFormulas re-evaluates the formulas a structural mutation disturbed,
// in dependency order.
//
// The seed is "formulas whose references did not survive as a plain
// translation", and that is complete: a translation moves a reference and the
// cell it names by the same n, so the formula reads the same content and cannot
// change value. A formula reads something new only if a reference died or a
// range changed extent, which is what scanBrokenRefs selects. Everything
// downstream is reached through the reverse reference lookup, so a chain like
// W -> X -> Y settles in one pass.
func recomputeFormulas(tx *sql.Tx, bi *bandIndex, src *txCells, seeds []formulaNode) (recomputed, error) {
	var out recomputed
	if len(seeds) == 0 {
		return out, nil
	}
	nodes, err := reachFrom(tx, bi, seeds)
	if err != nil {
		return out, err
	}

	index := make(map[CellRef]int, len(nodes))
	for i, n := range nodes {
		index[n.ref] = i
	}

	// Edges point precedent -> dependent, the direction change flows. Only edges
	// between nodes matter: a precedent outside the set cannot change here, or
	// this formula would itself be in the seed.
	outEdges := make([][]int, len(nodes))
	for i, n := range nodes {
		if n.perr != nil {
			continue
		}
		for _, r := range n.expr.Refs() {
			if j, ok := index[r]; ok && j != i {
				outEdges[j] = append(outEdges[j], i)
			}
		}
	}

	queue := make([]int, len(nodes))
	for i := range nodes {
		queue[i] = i
	}
	if len(queue) == 0 {
		return out, nil
	}

	// Kahn over the induced subgraph. Nodes left over are in (or downstream of)
	// a cycle and get #CYCLE! without ever being evaluated, which is what makes
	// this terminate.
	indeg := make(map[int]int, len(queue))
	for _, i := range queue {
		if _, ok := indeg[i]; !ok {
			indeg[i] = 0
		}
	}
	for _, i := range queue {
		for _, j := range outEdges[i] {
			if _, ok := indeg[j]; ok {
				indeg[j]++
			}
		}
	}
	order := make([]int, 0, len(queue))
	for _, i := range queue {
		if indeg[i] == 0 {
			order = append(order, i)
		}
	}
	for h := 0; h < len(order); h++ {
		for _, j := range outEdges[order[h]] {
			if d, ok := indeg[j]; ok {
				indeg[j] = d - 1
				if d-1 == 0 {
					order = append(order, j)
				}
			}
		}
	}

	// One batched read for everything the pass touches: the nodes themselves, so
	// "did it change" has a baseline, and every cell they read.
	rows := make([]int, 0, len(queue)*2)
	for _, i := range queue {
		rows = append(rows, nodes[i].ref.Row)
		if nodes[i].perr == nil {
			for _, r := range nodes[i].expr.Refs() {
				rows = append(rows, r.Row)
			}
		}
	}
	if err := src.loadRows(rows); err != nil {
		return out, err
	}

	ordered := make([]bool, len(nodes))
	for _, i := range order {
		ordered[i] = true
	}
	for _, i := range queue {
		if !ordered[i] {
			src.put(nodes[i].ref, ComputedCell{Ref: nodes[i].ref, Computed: TokenCycle, Kind: KindError})
		}
	}
	for _, i := range order {
		n := nodes[i]
		var (
			computed string
			kind     Kind
			err      error
		)
		if n.perr != nil {
			computed, kind = ErrorToken(n.perr), KindError
		} else {
			computed, kind, err = EvalFormula(n.expr, src.value)
		}
		if err != nil {
			return out, fmt.Errorf("recompute %s: %w", n.ref, err)
		}
		src.put(n.ref, ComputedCell{Ref: n.ref, Computed: computed, Kind: kind})
	}

	for _, i := range queue {
		n := nodes[i]
		cc, ok := src.result(n.ref)
		if !ok {
			continue
		}
		out.evaluated = append(out.evaluated, n.ref)
		if !n.rewritten && cc.Computed == n.oldValue && cc.Kind == n.oldKind {
			continue // nothing to write: same text, same value, same kind
		}
		out.cells = append(out.cells, Cell{
			Ref: n.ref, Raw: n.raw, Computed: cc.Computed, Kind: cc.Kind,
		})
	}
	return out, nil
}

// writeBack persists recomputed values only. It deliberately does not touch raw
// or the reference columns: the formula text is a template the mutation did not
// change, and the references were already written by the shift and by
// repairBrokenRefs. Writing raw here would put a materialized A1 string where a
// template belongs and freeze the formula for every future mutation.
func writeBack(tx *sql.Tx, bi *bandIndex, cells []Cell) error {
	return bulkInsert(tx, len(cells), 5,
		`INSERT INTO cells (k, col, raw, computed, kind) VALUES `,
		` ON CONFLICT(k, col) DO UPDATE SET
		    computed = excluded.computed, kind = excluded.kind`,
		func(i int, args []any) []any {
			c := cells[i]
			return append(args, bi.keyOf(c.Ref.Row), c.Ref.Col, "", c.Computed, int(c.Kind))
		})
}

// ─── Reading cells inside the transaction ─────────────────────────────────────
//
// recalc.go's cellSource reads through *sql.DB, which inside this transaction
// would see the sheet as it was before the shift. Same batching idea — coalesce
// rows into contiguous range scans — pointed at the tx instead.

type txCells struct {
	tx     *sql.Tx
	bi     *bandIndex
	cells  map[CellRef]Cell
	loaded map[int]bool
	res    map[CellRef]ComputedCell

	queries   int
	cellsRead int
}

func newTxCells(tx *sql.Tx, bi *bandIndex) *txCells {
	return &txCells{
		tx:     tx,
		bi:     bi,
		cells:  make(map[CellRef]Cell, 1024),
		loaded: make(map[int]bool, 256),
		res:    make(map[CellRef]ComputedCell, 256),
	}
}

func (t *txCells) put(ref CellRef, cc ComputedCell) { t.res[ref] = cc }

func (t *txCells) result(ref CellRef) (ComputedCell, bool) {
	cc, ok := t.res[ref]
	return cc, ok
}

func (t *txCells) loadRows(rows []int) error {
	if len(rows) == 0 {
		return nil
	}
	want := make([]int, 0, len(rows))
	seen := make(map[int]bool, len(rows))
	for _, r := range rows {
		// RowCeiling, not the sheet's extent: a row past the bottom reads back
		// empty, which is the right answer for a formula naming one.
		if r < 0 || r >= RowCeiling || t.loaded[r] || seen[r] {
			continue
		}
		seen[r] = true
		want = append(want, r)
	}
	if len(want) == 0 {
		return nil
	}
	sort.Ints(want)

	lo, hi := want[0], want[0]
	flush := func() error {
		if err := t.readRange(lo, hi); err != nil {
			return err
		}
		for r := lo; r <= hi; r++ {
			t.loaded[r] = true
		}
		return nil
	}
	for _, r := range want[1:] {
		if r-hi <= rowRunGap && r-lo < rowRunMax {
			hi = r
			continue
		}
		if err := flush(); err != nil {
			return err
		}
		lo, hi = r, r
	}
	return flush()
}

func (t *txCells) readRange(lo, hi int) error {
	rows, err := t.tx.Query(
		`SELECT k, col, raw, computed, kind FROM cells
		  WHERE k BETWEEN ? AND ?`, t.bi.keyOf(lo), t.bi.keyOf(hi))
	t.queries++
	if err != nil {
		return fmt.Errorf("tx window %d..%d: %w", lo, hi, err)
	}
	defer rows.Close()
	for rows.Next() {
		var c Cell
		var kind int
		var k int64
		if err := rows.Scan(&k, &c.Ref.Col, &c.Raw, &c.Computed, &kind); err != nil {
			return fmt.Errorf("tx window scan: %w", err)
		}
		if c.Ref.Row = t.bi.rankOf(k); c.Ref.Row < 0 {
			continue
		}
		c.Kind = Kind(kind)
		t.cells[c.Ref] = c
		t.cellsRead++
	}
	return rows.Err()
}

// stored is the cell as the transaction has it: post-shift, pre-recompute.
func (t *txCells) stored(ref CellRef) (Cell, error) {
	if c, ok := t.cells[ref]; ok {
		return c, nil
	}
	if t.loaded[ref.Row] {
		return Cell{Ref: ref}, nil
	}
	if err := t.loadRows([]int{ref.Row}); err != nil {
		return Cell{}, err
	}
	if c, ok := t.cells[ref]; ok {
		return c, nil
	}
	return Cell{Ref: ref}, nil
}

// value is the CellLookup handed to the evaluator: a cell this pass already
// recomputed reads as its new value, everything else reads through.
func (t *txCells) value(ref CellRef) (Cell, error) {
	if cc, ok := t.res[ref]; ok {
		return Cell{Ref: ref, Computed: cc.Computed, Kind: cc.Kind}, nil
	}
	return t.stored(ref)
}

// ─── Column widths ────────────────────────────────────────────────────────────
//
// Widths are shared sheet state: everyone sees the same columns, so resizing one
// is a command against the sheet rather than a client-side preference, which is
// why it lives in a table beside the cells and dirties the whole grid.
//
// The table is sparse — a column with no row is the default width — which keeps
// the common sheet at zero rows and makes "reset to default" a DELETE.

const (
	// MinColWidth / MaxColWidth clamp a width to something a grid can still be
	// read at: narrower cannot show two digits, wider pushes every other column
	// off screen. Widths are shared, so without the clamp one viewer could ruin
	// the sheet for everybody.
	MinColWidth = 24
	MaxColWidth = 600
	// DefaultColWidth is what an absent row means.
	DefaultColWidth = 96
)

// ClampColWidth brings a width into range. Exported so a caller can echo the
// value actually stored back to the user.
func ClampColWidth(px int) int {
	if px < MinColWidth {
		return MinColWidth
	}
	if px > MaxColWidth {
		return MaxColWidth
	}
	return px
}

// SetColWidth stores a width for one column, clamped to
// MinColWidth..MaxColWidth. Setting the default width removes the row rather
// than storing it, so the table stays sparse. The dirty set is the entire grid,
// because every rendered row contains every column. Must be called from the
// sheet's actor goroutine.
func (s *Sheet) SetColWidth(col, px int) error {
	if col < 0 || col >= MaxCols {
		return fmt.Errorf("%w: column %d out of 0..%d", ErrBadRef, col, MaxCols-1)
	}
	if px == 0 {
		return fmt.Errorf("%w: 0 (use %d..%d, or DefaultColWidth to reset)",
			ErrBadWidth, MinColWidth, MaxColWidth)
	}
	w := ClampColWidth(px)
	return s.use(func(db *sql.DB) error {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin width %d: %w", col, err)
		}
		defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

		var prev string
		var stored int
		switch err := tx.QueryRow(`SELECT width FROM cols WHERE col = ?`, col).Scan(&stored); {
		case err == nil:
			prev = fmt.Sprint(stored)
		case errors.Is(err, sql.ErrNoRows):
		default:
			return fmt.Errorf("read width %d: %w", col, err)
		}

		if err := appendEvent(tx, "#width",
			fmt.Sprintf("%s %d", string(rune('A'+col)), w), prev,
		); err != nil {
			return fmt.Errorf("append width event %d: %w", col, err)
		}

		// Resetting to the default width deletes the row, but only if the row
		// has nothing else to say. A column carrying a style keeps its row and
		// stores the default width explicitly, which means the same thing an
		// absent row does; deleting it would silently drop the column's colour.
		if w == DefaultColWidth {
			if _, err := tx.Exec(
				`DELETE FROM cols WHERE col = ? AND style = 0`, col); err != nil {
				return fmt.Errorf("clear width %d: %w", col, err)
			}
			// A styled column survived that DELETE, so it has to be told the
			// new width explicitly. The UPDATE matches nothing when the row was
			// deleted, which is the common case and costs one seek.
			if _, err := tx.Exec(
				`UPDATE cols SET width = ? WHERE col = ?`, w, col); err != nil {
				return fmt.Errorf("reset width %d: %w", col, err)
			}
		} else if _, err := tx.Exec(
			`INSERT INTO cols (col, width, style) VALUES (?, ?, 0)
			   ON CONFLICT(col) DO UPDATE SET width = excluded.width`, col, w,
		); err != nil {
			return fmt.Errorf("set width %d: %w", col, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit width %d: %w", col, err)
		}
		return nil
	})
}

// ColWidths returns the stored widths, keyed by 0-indexed column. Columns
// absent from the map are DefaultColWidth; a sheet that has never been resized
// returns an empty, non-nil map.
func (s *Sheet) ColWidths() (map[int]int, error) {
	out := make(map[int]int, 4)
	err := s.use(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT col, width FROM cols ORDER BY col`)
		if err != nil {
			return fmt.Errorf("read widths: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var col, w int
			if err := rows.Scan(&col, &w); err != nil {
				return fmt.Errorf("width scan: %w", err)
			}
			if col < 0 || col >= MaxCols {
				continue
			}
			out[col] = ClampColWidth(w)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// colMeta is one row of `cols`. The two fields are unrelated facts kept together
// because they are one row and move as one — see shiftColMeta.
type colMeta struct {
	width int
	style int
}

func readColMeta(tx *sql.Tx) (map[int]colMeta, error) {
	rows, err := tx.Query(`SELECT col, width, style FROM cols`)
	if err != nil {
		return nil, fmt.Errorf("read column state in tx: %w", err)
	}
	defer rows.Close()
	out := map[int]colMeta{}
	for rows.Next() {
		var col int
		var m colMeta
		if err := rows.Scan(&col, &m.width, &m.style); err != nil {
			return nil, fmt.Errorf("column state scan in tx: %w", err)
		}
		out[col] = m
	}
	return out, rows.Err()
}
