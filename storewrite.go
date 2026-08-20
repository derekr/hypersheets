package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	_ "modernc.org/sqlite"
)

// storewrite.go — the two ways a cell changes.
// ─── The write ────────────────────────────────────────────────────────────────

// MaxCellBytes caps the text one cell may hold. The formula grammar tops out at
// two operands or one range, so the longest legal formula is a couple of dozen
// bytes and the longest plausible label is a sentence.
//
// The number that matters is not the cell but the window: a buffer is ~6,500
// cells and every one is re-rendered into every push, so the cap is really "how
// much can one cell cost every viewer on its band". At 4 KB one cell is ~1.5%
// of a 264 KB page; an uncapped 60,000-byte cell would be 23%.
const MaxCellBytes = 4096

// ErrCellTooLong is the sentinel for a value past MaxCellBytes.
var ErrCellTooLong = errors.New("cell value too long")

// cellTooLongError is that refusal with the numbers in it. It answers errors.Is
// for ErrBadRef as well as ErrCellTooLong because every HTTP path that reaches
// WriteCell maps an ErrBadRef-wrapped error to 400 and everything else to 500,
// and a refusal arriving as a 500 would reach the editor as "something broke"
// rather than as a sentence the user can act on. The Error() text is the body
// of that 400, shown verbatim by the editor's failure path.
type cellTooLongError struct {
	ref CellRef
	n   int
}

func (e cellTooLongError) Error() string {
	return fmt.Sprintf("%v holds %d bytes of text; a cell is limited to %d",
		e.ref, e.n, MaxCellBytes)
}

func (e cellTooLongError) Is(target error) bool {
	return target == ErrCellTooLong || target == ErrBadRef
}

// WriteCell commits one edit: the cell mutation, every recalculated value the
// formula engine produced, and the event describing it — all in one
// transaction. There is no partial state a reader can observe and no way to get
// an event without its mutation.
//
// computed carries the recalc results from the formula engine (recalc.go), and
// may be empty for a literal with no dependents. An entry for ref itself wins
// for ref's computed/kind; otherwise the value is inferred from raw. Entries
// for other cells update computed+kind only and leave raw untouched.
//
// deps is vestigial and ignored: a cell's outgoing edges are its reference
// columns, written by the upsert in step 3.
//
// Ordering contract: recalc runs before this call, so the database still holds
// ref's old value while it runs. Evaluate the graph with an overlay of
// {ref: raw} and include ref's own result in computed; writing first and
// recalculating after would need a second transaction, the split SPEC.md
// forbids.
//
// Must be called from the sheet's actor goroutine — see actor.go.
func (s *Sheet) WriteCell(ref CellRef, raw string, computed []ComputedCell) error {
	if !ref.Valid() {
		return fmt.Errorf("%w: %v out of grid", ErrBadRef, ref)
	}
	// The bound lives here rather than in a handler because three commands reach
	// this function — the cell edit, applyWrites (rangeops.go) and the clear
	// loop. The 64 KB request-body cap in limits.go bounds it only by accident:
	// middleware sees how many bytes arrived, not how many are one cell's value.
	if len(raw) > MaxCellBytes {
		return cellTooLongError{ref: ref, n: len(raw)}
	}
	// A write below the bottom of the sheet grows the sheet — one of the two
	// ways a sheet gets taller, the other being an insert at the bottom. Without
	// it the ref maps to deadKey and the CHECK constraint refuses the write,
	// which is a refusal where a spreadsheet should simply have more rows. The
	// recalculated cells count too: a cascade may land on a row the edit did not
	// name.
	need := ref.Row + 1
	for _, cc := range computed {
		if !cc.Ref.Valid() {
			return fmt.Errorf("%w: computed %v out of grid", ErrBadRef, cc.Ref)
		}
		need = max(need, cc.Ref.Row+1)
	}
	if err := s.ensureRows(need); err != nil {
		return err
	}
	return s.use(func(db *sql.DB) error {
		// The mapping is held for the whole transaction: an edit does not move
		// rows, so it needs the ranks it started with to still mean the same
		// keys when it finishes.
		return s.readIndex(func(bi *bandIndex) error {
			return s.writeCellTx(db, bi, ref, raw, computed)
		})
	})
}

func (s *Sheet) writeCellTx(db *sql.DB, bi *bandIndex, ref CellRef, raw string, computed []ComputedCell) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin write %s: %w", ref, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	key := bi.keyOf(ref.Row)

	// 1. Read the previous raw so the event records what it replaced.
	var prev string
	err = tx.QueryRow(`SELECT raw FROM cells WHERE k = ? AND col = ?`,
		key, ref.Col).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read prev %s: %w", ref, err)
	}

	// 2. Append the event first. Everything after this point is a mutation
	//    that can fail, and having the log row already in the transaction
	//    means the rollback path is what removes it, so "log written but
	//    mutation lost" is unreachable.
	if err := appendEvent(tx, ref.String(), raw, prev); err != nil {
		return fmt.Errorf("append event %s: %w", ref, err)
	}

	// 3. The cell itself. computed/kind come from the engine when it had
	//    something to say about this cell, otherwise from the literal.
	selfComputed, selfKind := raw, InferKind(raw)
	if selfKind == KindFormula {
		// A formula with no engine result yet: show the formula rather
		// than a stale value. recalc.go normally supplies an entry.
		selfComputed = raw
	}
	for _, cc := range computed {
		if cc.Ref == ref {
			selfComputed, selfKind = cc.Computed, cc.Kind
			break
		}
	}
	// The reference split happens on the way in, so everything downstream of a
	// write already has structural references to move. FormulaTemplate is a
	// no-op for a literal and for a formula that does not parse, both of which
	// store their text verbatim with no slots.
	tmpl, slots, span, _ := FormulaTemplate(raw)
	sa := slotArgs(bi, slots)
	if _, err := tx.Exec(
		`INSERT INTO cells (k, col, raw, computed, kind, `+slotCols+`, ref_span)
			   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			   ON CONFLICT(k, col) DO UPDATE SET
			     raw = excluded.raw, computed = excluded.computed, kind = excluded.kind,
			     ref0_k = excluded.ref0_k, ref0_col = excluded.ref0_col,
			     ref1_k = excluded.ref1_k, ref1_col = excluded.ref1_col,
			     ref_span = excluded.ref_span`,
		key, ref.Col, tmpl, selfComputed, int(selfKind),
		sa[0], sa[1], sa[2], sa[3], boolInt(span),
	); err != nil {
		return fmt.Errorf("upsert cell %s: %w", ref, err)
	}

	// 4. Every other recalculated cell: computed+kind only, raw preserved.
	for _, cc := range computed {
		if cc.Ref == ref {
			continue
		}
		// An out-of-grid ref has no key, so it binds deadKey and the CHECK
		// constraint refuses it — which is what keeps a bad computed entry a
		// loud mid-transaction failure instead of a cell filed at a
		// nonexistent rank.
		if _, err := tx.Exec(
			`INSERT INTO cells (k, col, raw, computed, kind) VALUES (?, ?, '', ?, ?)
				   ON CONFLICT(k, col) DO UPDATE SET
				     computed = excluded.computed, kind = excluded.kind`,
			bi.keyOf(cc.Ref.Row), cc.Ref.Col, cc.Computed, int(cc.Kind),
		); err != nil {
			return fmt.Errorf("apply computed %s: %w", cc.Ref, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit write %s: %w", ref, err)
	}
	return nil
}

// ─── The batched write ────────────────────────────────────────────────────────
//
// A range clear, fill or paste builds a []BatchWrite and calls ApplyBatchCtx,
// so the cascade is walked once for the whole range and the range commits once:
// one actor turn, one dirty set, one publish, one transaction — and still one
// event per cell. A caller wanting the two halves separately (a test
// substituting an engine, an actor that wants the recalc outside the write)
// drives RecalcBatch, WriteCellsCtx and DirtyWithWrites itself.

// BatchResult is what a batched write reports back.
type BatchResult struct {
	// Dirty is the publish set: every cell whose computed value moved, plus
	// every cell the batch wrote whatever its value did. The second half is
	// needed because a viewer holding that band must see the new raw text even
	// when the number it renders to did not change.
	Dirty []CellRef

	// Wrote is how many cell writes were committed — len(writes), since the
	// batch commits every one of them and the range commands have already
	// dropped the cells whose text would not change.
	Wrote int

	Stats RecalcStats
}

// Bands is the dirty set expressed as subscription topics.
func (r BatchResult) Bands() []int { return BandsFor(r.Dirty) }

// ApplyBatch is the whole bulk write: recalculate the union of everything the
// batch touches once, then commit every raw value, every recalculated value and
// every event in one transaction. The ordering contract is the single-cell
// one — the recalc pass runs first, against an overlay of the batch, so the
// database still holds the old values while it runs and a pass that refuses
// (ErrRecalcTooLarge) has written nothing.
//
// Must be called from the sheet's actor goroutine — see actor.go.
func (s *Sheet) ApplyBatch(writes []BatchWrite) (BatchResult, error) {
	res, err := RecalcBatch(s, writes)
	if err != nil {
		return BatchResult{}, err
	}
	// The write gets the list as given, repeats included, while the recalc
	// collapsed them: a repeated address is two user edits and the event log
	// says so. Only the value is computed once, for the state the batch ends
	// in.
	if err := s.WriteCells(writes, res.Computed); err != nil {
		return BatchResult{}, err
	}
	return BatchResult{
		Dirty: DirtyWithWrites(res.Dirty, writes),
		Wrote: len(writes),
		Stats: res.Stats,
	}, nil
}

// DirtyWithWrites is withRef (http.go) for a whole batch: the recalc's dirty
// set plus every cell the batch wrote, deduped, in that order. Recalc reports
// what changed; a range command needs what moved on screen, and rewriting
// `=1+1` over `2` changes no value and still has to be pushed. It uses a set
// rather than withRef's linear scan, which at a thousand writes would be
// O(N*M).
func DirtyWithWrites(dirty []CellRef, writes []BatchWrite) []CellRef {
	if len(writes) == 0 {
		return dirty
	}
	in := make(map[CellRef]struct{}, len(dirty)+len(writes))
	for _, c := range dirty {
		in[c] = struct{}{}
	}
	for _, w := range writes {
		if _, dup := in[w.Ref]; dup {
			continue
		}
		in[w.Ref] = struct{}{}
		dirty = append(dirty, w.Ref)
	}
	return dirty
}

// ApplyBatchCtx is ApplyBatch with spans around its two halves, so a
// `clear.command` / `fill.command` / `paste.command` trace shows the recalc and
// the write separately — the split RESULTS.md reports per-cell numbers against.
func (s *Sheet) ApplyBatchCtx(ctx context.Context, writes []BatchWrite) (BatchResult, error) {
	_, span := tracer.Start(ctx, "store.apply_batch")
	defer span.End()
	span.SetAttributes(attribute.Int("writes", len(writes)))
	res, err := s.ApplyBatch(writes)
	span.SetAttributes(
		attribute.Int("cells.written", res.Wrote),
		attribute.Int("dirty_cells", len(res.Dirty)),
		attribute.Int("nodes_visited", res.Stats.Nodes),
		attribute.Int("depth", res.Stats.Depth),
		attribute.Int("cycled", res.Stats.Cycled),
		attribute.Int("queries", res.Stats.Queries),
		attribute.Int("cells_read", res.Stats.CellsRead),
		attribute.Float64("recalc_ms", msf(res.Stats.Elapsed)),
	)
	if err != nil {
		span.RecordError(err)
	}
	return res, err
}

// WriteCells commits a whole batch: every cell's new text, every recalculated
// value the engine produced for the union of their cascades, and one event per
// cell — all in one transaction. It is WriteCell's contract with the loop
// inside the transaction rather than in the caller.
//
// computed comes from RecalcBatch. Entries whose Ref is one of the written
// cells supply that cell's computed/kind; every other entry updates
// computed+kind only, exactly as WriteCell does.
//
// Everything is validated before the transaction opens, so a batch naming one
// cell past MaxCellBytes or one ref off the grid writes nothing: half a paste
// is not a state a user can reason about.
//
// Must be called from the sheet's actor goroutine — see actor.go.
func (s *Sheet) WriteCells(writes []BatchWrite, computed []ComputedCell) error {
	if len(writes) == 0 {
		return nil
	}
	need := 0
	for _, w := range writes {
		if !w.Ref.Valid() {
			return fmt.Errorf("%w: %v out of grid", ErrBadRef, w.Ref)
		}
		// The same bound WriteCell applies, applied to every cell of the batch;
		// see WriteCell on why it lives in the store.
		if len(w.Raw) > MaxCellBytes {
			return cellTooLongError{ref: w.Ref, n: len(w.Raw)}
		}
		need = max(need, w.Ref.Row+1)
	}
	for _, cc := range computed {
		if !cc.Ref.Valid() {
			return fmt.Errorf("%w: computed %v out of grid", ErrBadRef, cc.Ref)
		}
		need = max(need, cc.Ref.Row+1)
	}
	// A write below the bottom of the sheet grows the sheet, once for the whole
	// batch rather than once per cell — a paste anchored at row 15,000 on a
	// 1,000-row sheet grows it exactly once.
	if err := s.ensureRows(need); err != nil {
		return err
	}
	return s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			return s.writeCellsTx(db, bi, writes, computed)
		})
	})
}

// WriteCellsCtx is WriteCells with a span around it, the same way WriteCellCtx
// wraps WriteCell.
func (s *Sheet) WriteCellsCtx(ctx context.Context, writes []BatchWrite, computed []ComputedCell) error {
	_, span := tracer.Start(ctx, "store.write_cells")
	defer span.End()
	span.SetAttributes(
		attribute.Int("writes", len(writes)),
		attribute.Int("computed", len(computed)),
	)
	err := s.WriteCells(writes, computed)
	if err != nil {
		span.RecordError(err)
	}
	return err
}

// writeCellsTx is writeCellTx's body with the statements prepared once for the
// whole batch instead of once per cell. Statement preparation dominates this
// path at small scale (DATA-MODEL.md records 16.1µs against 6.5µs for three
// statements versus one), and a thousand-cell paste issues thousands of them.
func (s *Sheet) writeCellsTx(db *sql.DB, bi *bandIndex, writes []BatchWrite, computed []ComputedCell) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin batch write: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	prevStmt, err := tx.Prepare(`SELECT raw FROM cells WHERE k = ? AND col = ?`)
	if err != nil {
		return fmt.Errorf("prepare read prev: %w", err)
	}
	defer prevStmt.Close() //nolint:errcheck // read-only statement
	evStmt, err := tx.Prepare(`INSERT INTO events (ts, ref, raw, prev) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare event: %w", err)
	}
	defer evStmt.Close() //nolint:errcheck // rolled back with the tx on failure
	cellStmt, err := tx.Prepare(
		`INSERT INTO cells (k, col, raw, computed, kind, ` + slotCols + `, ref_span)
		   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		   ON CONFLICT(k, col) DO UPDATE SET
		     raw = excluded.raw, computed = excluded.computed, kind = excluded.kind,
		     ref0_k = excluded.ref0_k, ref0_col = excluded.ref0_col,
		     ref1_k = excluded.ref1_k, ref1_col = excluded.ref1_col,
		     ref_span = excluded.ref_span`)
	if err != nil {
		return fmt.Errorf("prepare cell upsert: %w", err)
	}
	defer cellStmt.Close() //nolint:errcheck // rolled back with the tx on failure

	// The engine's answer for each written cell, so the loop below does not
	// rescan `computed` per cell: that is O(N*M), and M is the cascade.
	self := make(map[CellRef]ComputedCell, len(writes))
	written := make(map[CellRef]struct{}, len(writes))
	for _, w := range writes {
		written[w.Ref] = struct{}{}
	}
	for _, cc := range computed {
		if _, ok := written[cc.Ref]; ok {
			self[cc.Ref] = cc
		}
	}

	for _, w := range writes {
		key := bi.keyOf(w.Ref.Row)

		// 1. The previous raw, so the event records what it replaced.
		var prev string
		err := prevStmt.QueryRow(key, w.Ref.Col).Scan(&prev)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read prev %s: %w", w.Ref, err)
		}

		// 2. The event, before the mutation, for the reason writeCellTx gives.
		//    With one transaction for the whole batch, "log written but
		//    mutation lost" is unreachable for every cell of it.
		if err := appendEventStmt(tx, evStmt, w.Ref.String(), w.Raw, prev); err != nil {
			return fmt.Errorf("append event %s: %w", w.Ref, err)
		}

		// 3. The cell itself.
		selfComputed, selfKind := w.Raw, InferKind(w.Raw)
		if cc, ok := self[w.Ref]; ok {
			selfComputed, selfKind = cc.Computed, cc.Kind
		}
		tmpl, slots, span, _ := FormulaTemplate(w.Raw)
		sa := slotArgs(bi, slots)
		if _, err := cellStmt.Exec(
			key, w.Ref.Col, tmpl, selfComputed, int(selfKind),
			sa[0], sa[1], sa[2], sa[3], boolInt(span),
		); err != nil {
			return fmt.Errorf("upsert cell %s: %w", w.Ref, err)
		}
	}

	// 4. Every other recalculated cell: computed+kind only, raw preserved. The
	//    written cells are excluded because step 3 already carried their value,
	//    and re-issuing them here would overwrite the raw it just stored.
	if len(computed) > len(self) {
		valStmt, err := tx.Prepare(
			`INSERT INTO cells (k, col, raw, computed, kind) VALUES (?, ?, '', ?, ?)
			   ON CONFLICT(k, col) DO UPDATE SET
			     computed = excluded.computed, kind = excluded.kind`)
		if err != nil {
			return fmt.Errorf("prepare computed upsert: %w", err)
		}
		defer valStmt.Close() //nolint:errcheck // rolled back with the tx on failure
		for _, cc := range computed {
			if _, ok := written[cc.Ref]; ok {
				continue
			}
			if _, err := valStmt.Exec(
				bi.keyOf(cc.Ref.Row), cc.Ref.Col, cc.Computed, int(cc.Kind),
			); err != nil {
				return fmt.Errorf("apply computed %s: %w", cc.Ref, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit batch write: %w", err)
	}
	return nil
}

// InferKind classifies literal cell contents. Anything starting with '=' is a
// formula regardless of what follows — deciding whether it is a *valid*
// formula is recalc.go's job.
func InferKind(raw string) Kind {
	if raw == "" {
		return KindEmpty
	}
	if strings.HasPrefix(raw, "=") {
		return KindFormula
	}
	if _, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
		return KindNumber
	}
	return KindText
}
