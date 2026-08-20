package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.opentelemetry.io/otel/attribute"
	_ "modernc.org/sqlite"
)

// storeread.go — reading cells out of a sheet.
// ─── Reads ────────────────────────────────────────────────────────────────────

// Window returns a dense rectangle of cells for the inclusive display-row range
// [loRow, hiRow] across all MaxCols columns, ordered row-major. Cells with no
// stored row come back as KindEmpty rather than being omitted, so the renderer
// can walk the slice as a grid without checking for gaps. hiRow is clamped to
// the sheet's current extent, so a caller asking beyond the bottom gets a short
// window rather than an error.
//
// This is the hot read path — it runs on every push to every viewer — and it
// deliberately does not go through the actor: WAL readers are concurrent with
// the writer, so serializing them would add latency and buy nothing.
func (s *Sheet) Window(loRow, hiRow int) ([]Cell, error) {
	if loRow > hiRow {
		loRow, hiRow = hiRow, loRow
	}
	if loRow < 0 {
		loRow = 0
	}
	if hiRow < 0 {
		return nil, nil
	}

	var out []Cell
	err := s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			// The clamp is inside the held index, with the query, for the same
			// reason the rank translation is: the extent and the key mapping are
			// two halves of one fact, and reading them a moment apart is how a
			// window renders the right cells against the wrong row numbers.
			hiRow := min(hiRow, bi.rows-1)
			if loRow > hiRow {
				return nil
			}
			nRows := hiRow - loRow + 1
			// The style state comes from the same held lock as the mapping (see
			// Sheet.sty), so the ids these rows carry are resolvable against the
			// table this loop uses, and the column level they resolve through is
			// the one current when the keys were read.
			sty := s.styles()
			// The row level is the only extra query the cascade adds, skipped
			// unless the sheet has a row style at all. It is one primary-key
			// range scan over `rows`, deliberately not a join: a join measured
			// 45-81% slower on exactly this path.
			var rowSty []int
			if sty.anyRow {
				var err error
				if rowSty, err = readRowStylesFor(db, bi, loRow, hiRow); err != nil {
					return err
				}
			}
			cascade := sty.anyCol || rowSty != nil
			out = make([]Cell, nRows*MaxCols)
			for r := 0; r < nRows; r++ {
				// A cell with no stored row still inherits: a blank cell in a
				// yellow row is yellow, and the store says so rather than
				// leaving the render layer to re-derive it. Resolved once per
				// row, since the row half is constant across the 26 columns.
				rs := 0
				if rowSty != nil {
					rs = rowSty[r]
				}
				for c := 0; c < MaxCols; c++ {
					cell := Cell{Ref: CellRef{Row: loRow + r, Col: c}}
					if cascade {
						cell.Effective = resolve(0, rs, sty.col[c])
					}
					out[r*MaxCols+c] = cell
				}
			}
			// A window is a contiguous run of display ranks, so it is a
			// contiguous run of keys — the property the whole storage model is
			// built to preserve, and why this is one ordered primary-key range
			// scan and not a search per row.
			rows, err := db.Query(
				`SELECT k, col, raw, computed, kind, `+slotCols+`, style FROM cells
				  WHERE k BETWEEN ? AND ?
				  ORDER BY k, col`, bi.keyOf(loRow), bi.keyOf(hiRow))
			if err != nil {
				return fmt.Errorf("window %d..%d: %w", loRow, hiRow, err)
			}
			defer rows.Close()
			// The scan targets and the []any pointing at them are built once and
			// reused: they are stable addresses that Scan overwrites, and
			// rebuilding them per row would be two allocations per cell to
			// describe a list of ten pointers that never changes.
			var (
				k             int64
				col, kind     int
				style         int
				raw, computed string
				slots         nullSlots
			)
			args := make([]any, 0, 6+2*maxSlots)
			args = append(args, &k, &col, &raw, &computed, &kind)
			args = append(args, slots.scanArgs()...)
			args = append(args, &style)
			// Keys arrive ascending, so ranks come from one walk over the bands
			// rather than a binary search per cell.
			scan := bi.scanner()
			for rows.Next() {
				// Scan writes every target, NULL included (a NULL slot comes
				// back Valid == false), so there is nothing to reset here.
				if err := rows.Scan(args...); err != nil {
					return fmt.Errorf("window scan: %w", err)
				}
				row := scan.rank(k)
				if row < loRow || row > hiRow || col < 0 || col >= MaxCols {
					continue
				}
				rs := 0
				if rowSty != nil {
					rs = rowSty[row-loRow]
				}
				// A1 text is materialized only for a row that actually has
				// structural references, which is exactly the formula cells. A
				// literal's raw is already its display text.
				if refs := slots.refs(bi); refs != nil {
					raw = RenderTemplate(raw, refs)
				}
				// The cascade is two integer compares rather than a lookup.
				// `cascade` is false for every sheet nobody has styled by row or
				// column, so resolution costs one bool test there.
				eff := style
				if cascade && eff == 0 {
					eff = resolve(0, rs, sty.col[col])
				}
				// Runs only for a cell that actually has a number format, so an
				// unstyled sheet pays one integer compare per cell and no
				// allocation. It reads the effective style, so a
				// currency-formatted column formats its cells without any of
				// them storing a style id.
				display := computed
				if eff != 0 {
					if f := sty.tab.get(eff).Fmt; f != FmtPlain {
						display = FormatValue(computed, f)
					}
				}
				out[(row-loRow)*MaxCols+col] = Cell{
					Ref:       CellRef{Row: row, Col: col},
					Raw:       raw,
					Computed:  computed,
					Display:   display,
					Kind:      Kind(kind),
					Style:     style,
					Effective: eff,
				}
			}
			return rows.Err()
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// WindowCtx is Window with a span around it, so the hot read path shows up as
// `store.window` in a trace with the size of the read attached. It is separate
// rather than a signature change because Window is the documented read API and
// is called from tests and benchmarks that have no context to give it.
func (s *Sheet) WindowCtx(ctx context.Context, loRow, hiRow int) ([]Cell, error) {
	ctx, span := tracer.Start(ctx, "store.window")
	defer span.End()
	cells, err := s.Window(loRow, hiRow)
	span.SetAttributes(
		attribute.Int("rows", hiRow-loRow+1),
		attribute.Int("cells", len(cells)),
		attribute.Int("cols", MaxCols),
	)
	if err != nil {
		span.RecordError(err)
	}
	_ = ctx
	return cells, err
}

// WriteCellCtx is WriteCell with a span around it. Same reasoning as WindowCtx:
// the write path is most of what a `cell.command` trace is made of, so it needs
// to be visible, but WriteCell's contract (called from the actor goroutine,
// inside one transaction) must not change.
func (s *Sheet) WriteCellCtx(ctx context.Context, ref CellRef, raw string, computed []ComputedCell) error {
	_, span := tracer.Start(ctx, "store.write_cell")
	defer span.End()
	span.SetAttributes(
		attribute.String("cell.ref", ref.String()),
		attribute.Int("computed", len(computed)),
	)
	err := s.WriteCell(ref, raw, computed)
	if err != nil {
		span.RecordError(err)
	}
	return err
}

// GetCell returns one cell. A cell that was never written comes back as the
// zero value with its Ref set and Kind == KindEmpty, not as an error — "empty"
// is a normal state in a spreadsheet, not a miss.
func (s *Sheet) GetCell(ref CellRef) (Cell, error) {
	if !ref.Valid() {
		return Cell{}, fmt.Errorf("%w: %v out of grid", ErrBadRef, ref)
	}
	out := Cell{Ref: ref}
	err := s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			var kind, style int
			var slots nullSlots
			args := append([]any{&out.Raw, &out.Computed, &kind}, slots.scanArgs()...)
			args = append(args, &style)
			err := db.QueryRow(
				`SELECT raw, computed, kind, `+slotCols+`, style FROM cells WHERE k = ? AND col = ?`,
				bi.keyOf(ref.Row), ref.Col).Scan(args...)
			if errors.Is(err, sql.ErrNoRows) {
				// A cell that was never written still inherits from its row and
				// its column: under a cascade "empty" is a look as much as it is
				// a state, and a blank cell in a yellow row is yellow.
				eff, cerr := s.effectiveStyle(db, bi, ref, 0)
				if cerr != nil {
					return cerr
				}
				out.Effective = eff
				return nil
			}
			if err != nil {
				return fmt.Errorf("get cell %s: %w", ref, err)
			}
			out.Kind = Kind(kind)
			out.Style = style
			if refs := slots.refs(bi); refs != nil {
				out.Raw = RenderTemplate(out.Raw, refs)
			}
			eff, err := s.effectiveStyle(db, bi, ref, style)
			if err != nil {
				return err
			}
			out.Effective = eff
			out.Display = out.Computed
			if eff != 0 {
				if f := s.styles().tab.get(eff).Fmt; f != FmtPlain {
					out.Display = FormatValue(out.Computed, f)
				}
			}
			return nil
		})
	})
	if err != nil {
		return Cell{}, err
	}
	return out, nil
}

// effectiveStyle resolves one cell through the cascade — the single-cell form
// of what Window does in bulk, since the bulk form's range scan would be a scan
// for one answer. The column level is resident; the row level costs one
// primary-key seek, and only when the sheet carries a row style and the cell
// has none of its own.
//
// The caller must be inside readIndex, so the mapping it keys with and the
// style state it resolves against are the pair Window would have seen.
func (s *Sheet) effectiveStyle(q queryer, bi *bandIndex, ref CellRef, own int) (int, error) {
	if own != 0 {
		return own, nil
	}
	st := s.styles()
	if !st.cascades() {
		return 0, nil
	}
	row := 0
	if st.anyRow {
		var err error
		if row, err = rowStyleOf(q, bi, ref.Row); err != nil {
			return 0, err
		}
	}
	return resolve(0, row, st.col[ref.Col]), nil
}

// dependentsSQL is the reverse dependency lookup: which cells read the cell at
// (?1, ?2). Three arms — the two single-reference slots and the interval query
// over ranges — each on its own partial index, in one round trip.
//
// The repeated `ref_span = ... AND ... IS NOT NULL` terms are not redundant:
// they are what makes each arm eligible for its partial index. Drop them and
// SQLite scans the cells table once per hop of a cascade. The span arm names
// its index with INDEXED BY because both span indexes match the predicate
// equally well as far as the planner can tell — there are no statistics in
// these files — and the point of having two is to choose.
//
// UNION ALL with no ORDER BY, deliberately: `UNION ... ORDER BY 1, 2` makes the
// planner want each arm sorted by (k, col), which no span index can provide, so
// it reads the third arm as `SCAN cells`. Deduplication and ordering are a
// dozen refs in Go instead; see scanDependents.
//
// ?1 is a storage key, not a display row. The interval arm works because keys
// and ranks are ordered the same way, and asking of keys is what lets a range
// survive a row insert with no write at all.
const dependentsSQL = `
SELECT k, col FROM cells
  WHERE ref_span = 0 AND ref0_k IS NOT NULL AND ref0_k = ?1 AND ref0_col = ?2
UNION ALL
SELECT k, col FROM cells
  WHERE ref_span = 0 AND ref1_k IS NOT NULL AND ref1_k = ?1 AND ref1_col = ?2
UNION ALL
SELECT k, col FROM cells INDEXED BY %s
  WHERE ref_span = 1
    AND ref0_k <= ?1 AND ref1_k >= ?1 AND ref0_col <= ?2 AND ref1_col >= ?2`

// dependentsSQLLo drives the interval arm off the low endpoint: good for a
// probe near the top of the grid, where few ranges start above it.
var dependentsSQLLo = fmt.Sprintf(dependentsSQL, "cells_span_lo")

// dependentsSQLHi drives it off the high endpoint: good for a probe near the
// bottom, where few ranges end below it.
var dependentsSQLHi = fmt.Sprintf(dependentsSQL, "cells_span_hi")

// dependentsSQLFor picks the direction with less to scan: for a b-tree driven
// from one end, whichever endpoint is nearer its edge of the grid leaves fewer
// ranges between it and the probe. `rows` is the sheet's own extent, not a
// constant, and picking the wrong arm costs a longer scan, not a wrong answer.
func dependentsSQLFor(row, rows int) string {
	if row*2 > rows {
		return dependentsSQLHi
	}
	return dependentsSQLLo
}

// scanDependents drains a dependentsSQL result into ascending, deduplicated
// refs. The only possible duplicate is a formula that names the same cell in
// both slots (`=A1+A1`), so the dedup is a linear check over a handful of
// entries rather than a map.
func scanDependents(bi *bandIndex, rows *sql.Rows) ([]CellRef, error) {
	var out []CellRef
	for rows.Next() {
		var c CellRef
		var k int64
		if err := rows.Scan(&k, &c.Col); err != nil {
			return nil, fmt.Errorf("dependents scan: %w", err)
		}
		c.Row = bi.rankOf(k)
		dup := false
		for _, o := range out {
			if o == c {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Row != out[j].Row {
			return out[i].Row < out[j].Row
		}
		return out[i].Col < out[j].Col
	})
	return out, nil
}

// DependentsOf returns every cell whose formula reads ref — the reverse walk
// the recalc pass uses to grow a dirty set. One hop only; the caller does the
// transitive closure and the cycle detection. It reads the reference columns of
// the formulas themselves rather than a materialized edge list, so a cell
// cannot be listed as depending on something its own reference does not name.
func (s *Sheet) DependentsOf(ref CellRef) ([]CellRef, error) {
	if !ref.Valid() {
		return nil, fmt.Errorf("%w: %v out of grid", ErrBadRef, ref)
	}
	var out []CellRef
	err := s.use(func(*sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			stmt := s.depLo
			if ref.Row*2 > bi.rows {
				stmt = s.depHi
			}
			rows, err := stmt.Query(bi.keyOf(ref.Row), ref.Col)
			if err != nil {
				return fmt.Errorf("dependents of %s: %w", ref, err)
			}
			defer rows.Close()
			out, err = scanDependents(bi, rows)
			if err != nil {
				return fmt.Errorf("dependents of %s: %w", ref, err)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PrecedentsOf returns the cells that ref's own formula reads — the forward
// walk. It is one primary-key lookup followed by expanding the stored slots,
// because that row is the edge list: two operand refs, or the two endpoints of
// a range. A destroyed reference (stored off-grid, rendered #REF!) is skipped.
func (s *Sheet) PrecedentsOf(ref CellRef) ([]CellRef, error) {
	if !ref.Valid() {
		return nil, fmt.Errorf("%w: %v out of grid", ErrBadRef, ref)
	}
	var out []CellRef
	err := s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			var slots nullSlots
			var span int
			args := append(slots.scanArgs(), &span)
			err := db.QueryRow(
				`SELECT `+slotCols+`, ref_span FROM cells WHERE k = ? AND col = ?`,
				bi.keyOf(ref.Row), ref.Col).Scan(args...)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("precedents of %s: %w", ref, err)
			}
			out = expandSlots(slots.refs(bi), span != 0)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// expandSlots turns stored slots into the cells they name, ascending. A span
// enumerates its rectangle — every cell of a SUM's range is a real dependency,
// including the empty ones, because filling one later must dirty the SUM.
func expandSlots(slots []CellRef, span bool) []CellRef {
	if span {
		if len(slots) != 2 {
			return nil
		}
		lo, hi := slots[0], slots[1]
		if !lo.Valid() || !hi.Valid() {
			return nil
		}
		out := make([]CellRef, 0, (hi.Row-lo.Row+1)*(hi.Col-lo.Col+1))
		for r := lo.Row; r <= hi.Row; r++ {
			for c := lo.Col; c <= hi.Col; c++ {
				out = append(out, CellRef{Row: r, Col: c})
			}
		}
		return out
	}
	out := make([]CellRef, 0, len(slots))
	for _, s := range slots {
		if !s.Valid() {
			continue // a destroyed reference names nothing
		}
		if len(out) == 1 && out[0] == s {
			continue // `=A1+A1` reads A1 once
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Row != out[j].Row {
			return out[i].Row < out[j].Row
		}
		return out[i].Col < out[j].Col
	})
	return out
}

// Events returns log rows with seq > afterSeq, oldest first, capped at limit
// (limit <= 0 means 1000).
func (s *Sheet) Events(afterSeq int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	var out []Event
	err := s.use(func(db *sql.DB) error {
		rows, err := db.Query(
			`SELECT seq, ts, ref, raw, prev FROM events
			  WHERE seq > ? ORDER BY seq LIMIT ?`, afterSeq, limit)
		if err != nil {
			return fmt.Errorf("events: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e Event
			var ms int64
			if err := rows.Scan(&e.Seq, &ms, &e.Ref, &e.Raw, &e.Prev); err != nil {
				return fmt.Errorf("events scan: %w", err)
			}
			e.TS = time.UnixMilli(ms).UTC()
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LastSeq is the highest event sequence written to this sheet (0 if none).
func (s *Sheet) LastSeq() (int64, error) {
	var seq int64
	err := s.use(func(db *sql.DB) error {
		return db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&seq)
	})
	return seq, err
}

// maxEvents bounds the event log to its most recent rows. Every mutation
// appends one, so without a bound a script holding down a key grows the file
// until the per-sheet 256 MB disk cap (limits.go) notices.
//
// 100,000 is far above any human: at a fast typist's ~10 commits a second it is
// almost three hours of continuous editing, and at ~60 bytes a row it costs
// ~6 MB of the sheet's 256 MB. Trimming loses nothing that is replayed — the
// table is read only by Events() and LastSeq(), the live patch history is
// editlog.go's in-memory ring, undo is out of scope per SPEC.md, and the system
// of record is `cells`.
const maxEvents = 100000

// eventTrimEvery gates the trim to one write in a thousand. Running the delete
// on every write costs 3-6µs on a path that is 47-53µs, most of it statement
// preparation rather than the row removed, since a log far below the cap pays
// the same. Gated, the cost is an int64 modulo and the log is bounded by
// maxEvents + eventTrimEvery. The write that does trim removes a thousand rows
// at ~365µs, ~0.37µs a write amortized, and lands at ~0.4ms where its
// neighbours are ~0.05ms — still an eighth of what a row insert costs.
const eventTrimEvery = 1000

// trimEvents drops everything older than the most recent maxEvents rows, on the
// writes eventTrimEvery selects. It runs inside the mutation's transaction, so
// the log a reader sees is the log the committed sheet has and a rolled-back
// mutation cannot have trimmed anything.
//
// MAX(seq) is an index lookup on the INTEGER PRIMARY KEY and the delete a range
// scan over the head of that index, so the statement is bounded by the rows it
// removes. seq is AUTOINCREMENT, so trimming the head cannot make a later event
// reuse a trimmed number; on an empty log MAX(seq) is NULL and matches nothing.
func trimEvents(tx *sql.Tx, seq int64) error {
	if seq%eventTrimEvery != 0 {
		return nil
	}
	if _, err := tx.Exec(
		`DELETE FROM events WHERE seq <= (SELECT MAX(seq) - ? FROM events)`,
		maxEvents,
	); err != nil {
		return fmt.Errorf("trim events: %w", err)
	}
	return nil
}

// appendEvent writes one log row and trims the log. Every mutation in the
// system appends through here — the cell write, the row and column ops, the
// column width and all three levels of styling — because a bound that held on
// the cell path and nowhere else could still be walked past by holding down
// insert-row.
func appendEvent(tx *sql.Tx, ref, raw, prev string) error {
	res, err := tx.Exec(
		`INSERT INTO events (ts, ref, raw, prev) VALUES (?, ?, ?, ?)`,
		time.Now().UnixMilli(), ref, raw, prev,
	)
	if err != nil {
		return err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return err
	}
	return trimEvents(tx, seq)
}

// appendEventStmt is appendEvent with the INSERT already prepared, for the
// batch write. The batch appends one event per cell, because each cell is a
// user-visible mutation and the log is what "events == user edits" is a
// statement about. The trim is still gated at seq%eventTrimEvery, so a
// 1,000-cell paste crosses that gate at most once.
func appendEventStmt(tx *sql.Tx, ins *sql.Stmt, ref, raw, prev string) error {
	res, err := ins.Exec(time.Now().UnixMilli(), ref, raw, prev)
	if err != nil {
		return err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return err
	}
	return trimEvents(tx, seq)
}
