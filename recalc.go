package main

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// recalc.go — the dirty set is discovered during the write.
//
// The cells one edit changes cannot be declared up front: `B1=A1*2` and
// `C1=SUM(A1:A10)` are edges in a graph only the database holds, so the write
// path finds its dirty set by walking that graph rather than being handed one.
//
// Ordering contract (see store.go's WriteCell too): recalc runs before
// WriteCell, because the mutation and its event must be one transaction and
// WriteCell takes the finished results as arguments. While recalc runs the
// database therefore still holds the edited cell's old value, and the pass
// evaluates against the store plus an overlay of {editedRef: newRaw}. Every
// read of an edited cell sees the new text; every other cell reads through to
// SQLite. Write, recalc, write again would split the transaction.
//
// Three properties the pass has to hold:
//
//  1. Topological order. A cell is evaluated only after everything it reads;
//     otherwise a two-hop chain needs two passes to settle and viewers see a
//     half-updated sheet in between.
//  2. Changed-value-only dirtying. A cell that recomputes to the value it
//     already had stays out of the dirty set: dirty cells become bands become
//     NATS subjects become re-renders for every subscribed viewer.
//  3. Termination. Cycles are found and marked, never followed. A queue with a
//     visited set, so no recursion to overflow and no walk to hang.
//
// Those three are also what let one pass cover N cells: Recalc is RecalcBatch
// with a single write in it, and a range clear/fill/paste hands over the whole
// rectangle so the union of everything it touches is walked, ordered and
// evaluated once instead of once per cell. See RecalcBatch, and
// (*Sheet).ApplyBatch for the seam the range commands use.

// maxRecalcNodes caps how many cells one edit may recompute. The real unit is
// time, not cells: fan-out costs ~13 µs per dependent and is linear, so 5,000
// is ~65 ms holding the sheet's single writer — every other viewer's writes
// included — which is the same order as the structural mutations and the
// full-window style command this actor already serializes behind.
//
// The refusal is clean by construction, which is what lets the cap be this
// tight: recalc runs before WriteCell (see the ordering contract above), so an
// edit that trips it has written nothing. The structural mutations use the same
// constant from inside their transaction (reachFrom, mutate.go), where the
// error rolls that transaction back. No path reaches a cap halfway through a
// commit.
//
// For a batch the budget is per written cell: len(writes) * maxRecalcNodes.
// See discover() for why a flat cap over the union is the wrong bound.
const maxRecalcNodes = 5000

// prefetch tuning. Cells are stored clustered on their storage key, and a run
// of consecutive display rows is a contiguous run of keys (see bandkey.go), so
// reading one is a single b-tree scan: it is much cheaper to over-read a few
// rows than to issue a second query. rowRunGap is how large a hole we will read
// across to keep a run together; rowRunMax caps a single query's width.
const (
	rowRunGap = 8
	rowRunMax = 256
)

// ErrRecalcTooLarge is returned when an edit's affected set exceeds
// maxRecalcNodes.
var ErrRecalcTooLarge = errors.New("recalc: affected set too large")

// RecalcStats is the cost of one pass. Nothing depends on it for correctness;
// it exists to answer how expensive dependency-driven invalidation actually is.
type RecalcStats struct {
	Nodes     int           // cells evaluated, including the edited cell
	Depth     int           // longest chain of hops from the edited cell
	Cycled    int           // cells marked #CYCLE!
	Queries   int           // SQL round trips
	CellsRead int           // rows returned by those queries
	Elapsed   time.Duration // wall time of the whole pass
}

// RecalcResult is everything WriteCell needs, and nothing it doesn't. The three
// slices are meant to be passed straight through:
//
//	res, err := Recalc(sh, ref, raw)
//	err = sh.WriteCell(ref, raw, res.Computed)
//	publish(res.Bands())
type RecalcResult struct {
	// Computed is the values to persist: every written cell, always (WriteCell
	// would otherwise have to guess a formula's value from its text), plus
	// every other cell whose value actually changed. Cells that recomputed to
	// what they already held are omitted — the database is already right about
	// them, and writing them back is pure write amplification.
	Computed []ComputedCell

	// Dirty is every cell whose computed value or kind changed, the edited cell
	// included — the affected set discovered during the write.
	Dirty []CellRef

	Stats RecalcStats
}

// Bands is the dirty set expressed as subscription topics.
func (r RecalcResult) Bands() []int { return BandsFor(r.Dirty) }

// Precedents returns the cells a raw cell value reads. A literal has none. A
// malformed formula returns the parse error along with no refs: a formula that
// does not parse depends on nothing, so its old edges are correctly cleared.
func Precedents(raw string) ([]CellRef, error) {
	if InferKind(raw) != KindFormula {
		return nil, nil
	}
	f, err := ParseFormula(raw)
	if err != nil {
		return nil, err
	}
	return f.Refs(), nil
}

// BatchWrite is one cell of a batch: the address, and the A1 text to put there.
// It is deliberately the same two fields rangeops.go's rangeWrite already
// carries, so a caller that has built that list has built this one.
type BatchWrite struct {
	Ref CellRef
	Raw string
}

// BatchRecalcFunc is RecalcBatch's contract, named for the same reason
// RecalcFunc is (http.go): a server can hold the engine as a value, and a test
// can drive a handler with a known dirty set.
type BatchRecalcFunc func(sh *Sheet, writes []BatchWrite) (RecalcResult, error)

// literalRecalcBatch is the degenerate batch engine and the counterpart of
// http.go's literalRecalc: no formulas, no cascade, the written cells are the
// only dirty cells and their values come from their own text. It exists so a
// Server built without an engine behaves the same way on a range command as on
// a single edit — wiring the range handlers straight to (*Sheet).ApplyBatch
// would give such a server the real dependency graph for a paste and the
// degenerate one for a keystroke.
func literalRecalcBatch(_ *Sheet, writes []BatchWrite) (RecalcResult, error) {
	dirty := make([]CellRef, 0, len(writes))
	for _, w := range writes {
		if !w.Ref.Valid() {
			return RecalcResult{}, fmt.Errorf("%w: %v out of grid", ErrBadRef, w.Ref)
		}
		dirty = append(dirty, w.Ref)
	}
	return RecalcResult{Dirty: dirty, Stats: RecalcStats{Nodes: len(dirty)}}, nil
}

// Recalc computes the effect of setting ref to raw, without writing anything.
//
// It reads the sheet as it is now and overlays the pending edit, so the caller
// can hand the result to WriteCell and have the mutation, the recalculated
// values, the dependency edges and the event all land in one transaction.
//
// Safe to call off the actor goroutine (it only reads), but the caller must not
// interleave another write to the same sheet between Recalc and WriteCell —
// running both inside one actor command is the supported pattern.
func Recalc(sh *Sheet, ref CellRef, raw string) (RecalcResult, error) {
	return RecalcBatch(sh, []BatchWrite{{Ref: ref, Raw: raw}})
}

// RecalcBatch is Recalc for N cells at once: one dependency walk over the union
// of everything the batch can reach, one topological order, one evaluation. The
// cost of a bulk write tracks the cascade, not the number of cells written, so a
// loop over the single-cell path pays for the cascade N times.
//
// It is the same pass as the single-cell one in every respect that matters:
//
//   - Intra-batch dependencies are ordinary edges. Filling `B1:B10` where each
//     cell reads the one above puts all ten in the graph as roots, and the
//     edges between them come from their new formulas, so Kahn's algorithm
//     orders them exactly as it orders an external cascade.
//   - Stale edges into any written cell are dropped, not just into one. Storage
//     still holds every written cell's old references, and following them would
//     report cycles the batch is in the act of breaking.
//   - A cycle inside the batch is still a cycle. `A1 = B1*2` and `B1 = A1+1`
//     written together leave both nodes with an incoming edge, so neither is
//     ever ordered and both settle as `#CYCLE!`.
//
// Two places where the batch's answer differs from a per-cell loop's: a ref
// named twice takes its last value and the intermediate state is never
// computed, and Dirty holds the cells whose value moved across the whole batch,
// so a cell that changed and changed back is not dirty.
//
// Callers that need the written cells in the dirty set whatever their value did
// — which every range command does, because a viewer must see the new raw —
// should use (*Sheet).ApplyBatch, which unions them in.
func RecalcBatch(sh *Sheet, writes []BatchWrite) (RecalcResult, error) {
	start := time.Now()
	if sh == nil {
		return RecalcResult{}, errors.New("recalc: nil sheet")
	}
	writes, err := dedupeWrites(writes)
	if err != nil {
		return RecalcResult{}, err
	}
	if len(writes) == 0 {
		return RecalcResult{Stats: RecalcStats{Elapsed: time.Since(start)}}, nil
	}

	src := newCellSource(sh)
	if len(writes) > 1 {
		// newCellSource sizes the overlay for one edit, which is the shape of
		// every keystroke; a batch replaces it rather than growing it N times.
		src.over = make(map[CellRef]string, len(writes))
	}
	// The database still says these cells hold their old values; from here on,
	// this pass says otherwise.
	for _, w := range writes {
		src.overlay(w.Ref, w.Raw)
	}

	// Each written cell's outgoing edges come from its NEW formula, not from its
	// stored reference columns. A parse failure is not fatal and is deliberately
	// swallowed here: the cell ends up holding an error token (the evaluation
	// pass below re-derives it) and depending on nothing, which is the correct
	// edge set for a formula that does not parse.
	newPrec := make([][]CellRef, len(writes))
	nprec := 0
	for i, w := range writes {
		newPrec[i], _ = Precedents(w.Raw)
		nprec += len(newPrec[i])
	}

	g, err := discover(sh, src, writes, newPrec)
	if err != nil {
		return RecalcResult{}, err
	}
	roots := len(writes)

	// Round 1: the raws (and old values) of every affected cell.
	rows := make([]int, 0, len(g.nodes))
	for _, n := range g.nodes {
		rows = append(rows, n.Row)
	}
	if err := src.loadRows(rows); err != nil {
		return RecalcResult{}, err
	}

	// Parse every affected cell once, and use the parses to work out round 2:
	// the cells those formulas read, which are mostly not in the affected set
	// (V1 reads U1, which is affected, and B1, which is not).
	plans := make([]nodePlan, len(g.nodes))
	rows = rows[:0]
	for i, n := range g.nodes {
		var p nodePlan
		if i < roots {
			p.raw = writes[i].Raw // a written cell: the overlay's text
		} else {
			c, err := src.cell(n)
			if err != nil {
				return RecalcResult{}, err
			}
			p.raw = c.Raw
		}
		p.kind = InferKind(p.raw)
		if p.kind == KindFormula {
			p.expr, p.err = ParseFormula(p.raw)
		}
		if p.kind == KindFormula && p.err == nil {
			for _, r := range p.expr.Refs() {
				rows = append(rows, r.Row)
			}
		}
		plans[i] = p
	}
	if err := src.loadRows(rows); err != nil {
		return RecalcResult{}, err
	}

	order, stuck, depth := g.topoSort()

	var res RecalcResult

	// Cycle members (and everything downstream of them) are settled before the
	// ordered pass, so an evaluable cell that reads one sees #CYCLE! rather
	// than a stale number. Nothing in a cycle is ever evaluated, which is what
	// makes the pass terminate.
	for _, i := range stuck {
		src.put(g.nodes[i], ComputedCell{Ref: g.nodes[i], Computed: TokenCycle, Kind: KindError})
	}
	for _, i := range order {
		n := g.nodes[i]
		p := plans[i]
		var (
			computed string
			kind     Kind
			err      error
		)
		switch {
		case p.kind == KindFormula && p.err != nil:
			computed, kind = ErrorToken(p.err), KindError
		case p.kind == KindFormula:
			computed, kind, err = EvalFormula(p.expr, src.value)
		default:
			computed, kind, err = EvalRaw(p.raw, src.value)
		}
		if err != nil {
			return RecalcResult{}, fmt.Errorf("recalc %s: %w", n, err)
		}
		src.put(n, ComputedCell{Ref: n, Computed: computed, Kind: kind})
	}

	// Assemble in discovery order (the written cells in the order they were
	// given, then breadth-first from them), which keeps the output stable for
	// tests and for diffing.
	for i, n := range g.nodes {
		cc, ok := src.result(n)
		if !ok {
			continue
		}
		old, err := src.stored(n)
		if err != nil {
			return RecalcResult{}, err
		}
		changed := cc.Computed != old.Computed || cc.Kind != old.Kind
		if changed {
			res.Dirty = append(res.Dirty, n)
		}
		// Changed-value-only dirtying in practice: every written cell is
		// persisted (the write needs its value), everything else only when it
		// moved.
		if i < roots || changed {
			res.Computed = append(res.Computed, cc)
		}
	}

	res.Stats = RecalcStats{
		Nodes:     len(g.nodes),
		Depth:     depth,
		Cycled:    len(stuck),
		Queries:   src.queries,
		CellsRead: src.cellsRead,
		Elapsed:   time.Since(start),
	}
	return res, nil
}

// dedupeWrites validates a batch and collapses repeated addresses to last-wins,
// keeping each address at the position it first appeared. Writing A1 twice in
// one operation must leave A1 holding the second value; the intermediate state
// is never computed, which is why a cell the first write moved and the second
// moved back is not reported dirty — see RecalcBatch's contract.
//
// The common case (every address distinct, which is what a rectangle produces)
// costs one map insert per write and no copy.
func dedupeWrites(writes []BatchWrite) ([]BatchWrite, error) {
	if len(writes) == 0 {
		return nil, nil
	}
	if len(writes) == 1 {
		if !writes[0].Ref.Valid() {
			return nil, fmt.Errorf("%w: %v out of grid", ErrBadRef, writes[0].Ref)
		}
		return writes, nil // the single-cell path allocates nothing here
	}
	at := make(map[CellRef]struct{}, len(writes))
	for i, w := range writes {
		if !w.Ref.Valid() {
			return nil, fmt.Errorf("%w: %v out of grid", ErrBadRef, w.Ref)
		}
		if _, seen := at[w.Ref]; seen {
			return collapseWrites(writes, i), nil
		}
		at[w.Ref] = struct{}{}
	}
	return writes, nil
}

// collapseWrites is dedupeWrites' slow half, entered only once a repeated
// address has been seen — at index i, with writes[:i] known distinct. It copies
// rather than compacting in place because the caller's slice is not ours to
// rewrite.
func collapseWrites(writes []BatchWrite, i int) []BatchWrite {
	out := append(make([]BatchWrite, 0, len(writes)), writes[:i]...)
	pos := make(map[CellRef]int, len(writes))
	for j := range out {
		pos[out[j].Ref] = j
	}
	for _, w := range writes[i:] {
		if j, seen := pos[w.Ref]; seen {
			out[j].Raw = w.Raw
			continue
		}
		pos[w.Ref] = len(out)
		out = append(out, w)
	}
	return out
}

// nodePlan is one affected cell's parsed formula, kept so a cell is parsed once
// per pass instead of once per evaluation.
type nodePlan struct {
	raw  string
	kind Kind
	expr Formula
	err  error
}

// depGraph is the subgraph of cells this edit can reach, plus the edges among
// them. nodes[0] is always the edited cell. Edges point precedent -> dependent,
// i.e. the direction change flows.
type depGraph struct {
	nodes []CellRef
	index map[CellRef]int
	out   [][]int
	indeg []int
}

func (g *depGraph) add(ref CellRef) (int, bool) {
	if i, ok := g.index[ref]; ok {
		return i, false
	}
	i := len(g.nodes)
	g.nodes = append(g.nodes, ref)
	g.index[ref] = i
	g.out = append(g.out, nil)
	g.indeg = append(g.indeg, 0)
	return i, true
}

func (g *depGraph) edge(from, to int) {
	g.out[from] = append(g.out[from], to)
	g.indeg[to]++
}

// discover walks DependentsOf transitively from every written cell to find the
// union of everything this batch can affect. writes[i] is node i, so the roots
// occupy 0..len(writes)-1 and everything after them was discovered.
//
// The subtle part is edges into a written cell. Storage still holds its old
// references — the pass runs before the write — and following them would report
// a cycle that this very edit is breaking: if A1 was `=B1*2` and B1 is `=A1+1`,
// then after setting A1 to a literal the stale edge A1->B1 would still close the
// loop. So stale edges into any written cell are dropped, and each written
// cell's incoming edges are re-derived from its new precedents. That is also
// what catches a newly introduced cycle (setting A1 to `=B1*2` when B1 reads
// A1), and what makes an intra-batch dependency (`B2 = B1+1`, both written) an
// ordinary edge for Kahn's algorithm to order.
//
// The cap counts discovered cells and the budget is maxRecalcNodes per written
// cell, so a batch is served exactly when every cell of it would have been
// served alone — the union of N cascades is never larger than their sum. A flat
// cap over the union would refuse legitimate work: clearing 10,000 cells of a
// column each read by two downstream formulas discovers ~20,000 dependents,
// which one pass handles far more cheaply than the per-cell equivalent. What
// actually bounds a user's gesture is the caller's own range cap.
func discover(sh *Sheet, src *cellSource, writes []BatchWrite, newPrec [][]CellRef) (*depGraph, error) {
	g := &depGraph{index: make(map[CellRef]int, len(writes)+16)}
	for _, w := range writes {
		g.add(w.Ref)
	}
	roots := len(g.nodes)
	budget := roots * maxRecalcNodes

	for i := 0; i < len(g.nodes); i++ {
		deps, err := sh.DependentsOf(g.nodes[i])
		src.queries++
		src.cellsRead += len(deps)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if j, ok := g.index[d]; ok && j < roots {
				continue // stale edge into a cell this batch is replacing
			}
			j, added := g.add(d)
			g.edge(i, j)
			if added && len(g.nodes)-roots > budget {
				return nil, fmt.Errorf("%w: %s reaches more than %d cells",
					ErrRecalcTooLarge, batchLabel(writes), budget)
			}
		}
	}
	for i, ps := range newPrec {
		for _, p := range ps {
			if j, ok := g.index[p]; ok {
				// A written cell reads a cell that it also affects: either a
				// cycle, or the intra-batch ordering constraint that makes
				// filling `B1:B10` down a chain work.
				g.edge(j, i)
			}
		}
	}
	return g, nil
}

// batchLabel names a batch in an error the way a single ref names an edit.
func batchLabel(writes []BatchWrite) string {
	if len(writes) == 1 {
		return writes[0].Ref.String()
	}
	lo, hi := writes[0].Ref, writes[0].Ref
	for _, w := range writes[1:] {
		lo = CellRef{Row: min(lo.Row, w.Ref.Row), Col: min(lo.Col, w.Ref.Col)}
		hi = CellRef{Row: max(hi.Row, w.Ref.Row), Col: max(hi.Col, w.Ref.Col)}
	}
	return fmt.Sprintf("%d cells in %s:%s", len(writes), lo, hi)
}

// topoSort returns the evaluable nodes in dependency order, plus the nodes that
// could not be ordered — which, since every node was reached from the root, is
// exactly the cycle members and everything downstream of them.
//
// Kahn's algorithm, iteratively: no recursion means no stack depth to blow, and
// the leftover set falls out of the algorithm for free instead of needing a
// separate cycle hunt.
func (g *depGraph) topoSort() (order, stuck []int, depth int) {
	indeg := make([]int, len(g.indeg))
	copy(indeg, g.indeg)
	level := make([]int, len(g.nodes))

	queue := make([]int, 0, len(g.nodes))
	for i, d := range indeg {
		if d == 0 {
			queue = append(queue, i)
		}
	}
	for h := 0; h < len(queue); h++ {
		i := queue[h]
		order = append(order, i)
		if level[i] > depth {
			depth = level[i]
		}
		for _, j := range g.out[i] {
			if level[i]+1 > level[j] {
				level[j] = level[i] + 1
			}
			indeg[j]--
			if indeg[j] == 0 {
				queue = append(queue, j)
			}
		}
	}
	if len(order) == len(g.nodes) {
		return order, nil, depth
	}
	done := make([]bool, len(g.nodes))
	for _, i := range order {
		done[i] = true
	}
	for i := range g.nodes {
		if !done[i] {
			stuck = append(stuck, i)
		}
	}
	return order, stuck, depth
}

// Batched reads. A node-at-a-time GetCell turns a cascade into hundreds of
// round trips inside a write path — a single SUM can read a hundred cells.
// cellSource instead collects the rows a pass will need and pulls them in a
// handful of windowed range scans, which is the read shape the cells table was
// clustered for.

type cellSource struct {
	sh *Sheet

	cells  map[CellRef]Cell         // non-empty cells, as stored
	loaded map[int]bool             // rows fully read; a miss in one means empty
	over   map[CellRef]string       // pending edit: ref -> NEW raw
	res    map[CellRef]ComputedCell // values produced by this pass

	queries   int
	cellsRead int
}

func newCellSource(sh *Sheet) *cellSource {
	return &cellSource{
		sh:     sh,
		cells:  make(map[CellRef]Cell, 128),
		loaded: make(map[int]bool, 16),
		over:   make(map[CellRef]string, 1),
		res:    make(map[CellRef]ComputedCell, 16),
	}
}

// overlay stages the pending edit. It changes what `raw` this pass sees for
// ref; the computed value comes from evaluating it, not from the database.
func (c *cellSource) overlay(ref CellRef, raw string) { c.over[ref] = raw }

func (c *cellSource) put(ref CellRef, cc ComputedCell) { c.res[ref] = cc }

func (c *cellSource) result(ref CellRef) (ComputedCell, bool) {
	cc, ok := c.res[ref]
	return cc, ok
}

// loadRows pulls every row in rows that is not already cached, coalescing them
// into as few window scans as possible.
func (c *cellSource) loadRows(rows []int) error {
	if len(rows) == 0 {
		return nil
	}
	want := make([]int, 0, len(rows))
	seen := make(map[int]bool, len(rows))
	for _, r := range rows {
		// RowCeiling, not the sheet's extent: a row past the bottom reads back
		// empty, which is the right answer for a formula naming one, and
		// re-asking the sheet for its height inside the loop would buy nothing.
		if r < 0 || r >= RowCeiling || c.loaded[r] || seen[r] {
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
		cells, err := c.sh.Window(lo, hi)
		c.queries++
		c.cellsRead += len(cells)
		if err != nil {
			return err
		}
		for _, cell := range cells {
			if cell.Kind != KindEmpty || cell.Raw != "" || cell.Computed != "" {
				c.cells[cell.Ref] = cell
			}
		}
		for r := lo; r <= hi; r++ {
			c.loaded[r] = true
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

// stored returns the cell as the database has it — no overlay, no results. This
// is the "did it actually change" baseline, and it is why the overlay is kept
// separate from the cache instead of being written into it.
func (c *cellSource) stored(ref CellRef) (Cell, error) {
	if cell, ok := c.cells[ref]; ok {
		return cell, nil
	}
	if c.loaded[ref.Row] {
		return Cell{Ref: ref}, nil
	}
	cell, err := c.sh.GetCell(ref)
	c.queries++
	c.cellsRead++
	if err != nil {
		return Cell{}, err
	}
	c.cells[ref] = cell
	return cell, nil
}

// cell is the cell's current source text: the overlay wins, so an edited cell
// reads as its new raw while the database still holds the old one.
func (c *cellSource) cell(ref CellRef) (Cell, error) {
	cell, err := c.stored(ref)
	if err != nil {
		return Cell{}, err
	}
	if raw, ok := c.over[ref]; ok {
		cell.Ref, cell.Raw = ref, raw
	}
	return cell, nil
}

// value is the CellLookup handed to the evaluator: a cell already recomputed by
// this pass reads as its new value, everything else reads through to storage.
// Topological order is what guarantees a cell is in res before anything reads
// it — including the edited cell, which is always evaluated first.
func (c *cellSource) value(ref CellRef) (Cell, error) {
	if cc, ok := c.res[ref]; ok {
		return Cell{Ref: ref, Computed: cc.Computed, Kind: cc.Kind}, nil
	}
	return c.stored(ref)
}
