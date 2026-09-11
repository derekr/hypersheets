package main

// rangeops.go — the operations that take the selection as an argument.
//
//	POST /s/{id}/fill   fill down / fill right                   (a command)
//	POST /s/{id}/paste  copy a rectangle to a destination anchor (a command)
//
// plus the SUM/AVG/COUNT/MIN/MAX arithmetic behind the toolbar's summary of a
// selection. That summary is not an endpoint: presence already holds the
// selection rectangle on the connection — a collaborator's cursor has to be
// drawn by a server that knows where it is — so the aggregate belongs to this
// screen's read model and is recomputed on the same push that carries the edit.
// presenceui.go's patchAgg owns the "when"; the arithmetic stays here.
//
// Fill and paste have handleClear's shape: build a list of (ref, text) pairs and
// hand it to applyWrites inside one actor turn, which buys one dependency walk
// over the union of what they touch, one transaction, one dirty set, one
// publish, one edit-log entry and one event per cell.
//
// What makes them spreadsheet operations rather than block copies is that a
// formula's references translate: filling `=A1*2` from B1 down to B2 yields
// `=A2*2`. That is done on the A1 text (Cell.Raw, which the store already
// materializes in display notation) and written back through the ordinary write
// path, so recalc re-parses it as if the user had typed it — no store API
// changes and no second dependency-graph path.

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"
	"go.opentelemetry.io/otel/attribute"
)

// translateFormula shifts every cell reference in a formula by (dr, dc) and
// returns the new A1 text. Anything that is not a formula this parser accepts
// comes back unchanged, which covers a literal, an empty cell, and a formula
// that never parsed — a broken formula's text is what its author needs in order
// to fix it, so filling one copies it verbatim rather than mangling it.
//
// It works on text, not on the stored slots. Cell.Raw already comes out of
// Window() as rendered A1 notation, so a fill can re-spell the references there
// and hand the result to the ordinary write path; the recalc pass re-parses and
// re-stores the slots itself. That keeps one place knowing how a reference is
// persisted (formula.go) instead of two.
//
// A reference that lands off the grid renders as `#REF!` (CellRef.String does
// that already), so the filled formula stops parsing and is stored verbatim as
// text — which is exactly what a spreadsheet shows for the same mistake.
func translateFormula(raw string, dr, dc int) string {
	if dr == 0 && dc == 0 {
		return raw
	}
	if InferKind(raw) != KindFormula {
		return raw
	}
	f, err := ParseFormula(raw)
	if err != nil || len(f.Slots) == 0 {
		return raw
	}
	// Source order. Slots come back in role order (a SUM stores lo before hi
	// whichever way the user spelled it), and rewriting spans out of order would
	// splice the string at the wrong offsets.
	order := make([]int, len(f.Slots))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return f.Slots[order[a]].Pos < f.Slots[order[b]].Pos
	})

	var b strings.Builder
	b.Grow(len(raw) + 4*len(f.Slots))
	at := 0
	for _, i := range order {
		s := f.Slots[i]
		if s.Pos < at || s.Pos+s.Len > len(raw) {
			// The offsets disagree with the text. Same guard FormulaTemplate
			// keeps, for the same reason: emit the original rather than a splice
			// built on coordinates that cannot be right.
			return raw
		}
		b.WriteString(raw[at:s.Pos])
		b.WriteString(CellRef{Row: s.Ref.Row + dr, Col: s.Ref.Col + dc}.String())
		at = s.Pos + s.Len
	}
	b.WriteString(raw[at:])
	return b.String()
}

// aggSignal is the answer: one local signal. Underscore-prefixed, so the string
// the server computed is patched down and never rides back up on the next
// command.
type aggSignal struct {
	Ag string `json:"_ag"`
}

// aggTooBig is what a selection past ParseRangeBounds's limit is told. Ctrl/Cmd+A
// is 260,000 cells and reaches it immediately, so this is a normal answer rather
// than an edge case — and it is an answer, not silence.
const aggTooBig = "selection too large to summarise"

// aggregate is the status-bar summary of a rectangle.
//
// Count is non-empty cells; Sum/Avg/Min/Max are the numeric ones. That is
// Sheets' split: a column of labels above a column of numbers has a count of
// everything and a sum of the numbers. Text and error cells are not numbers; a
// formula cell is one when its computed value parses, which is the only value it
// has.
type aggregate struct {
	Count    int // non-empty cells
	Nums     int // cells contributing to Sum/Min/Max
	Sum      float64
	Min, Max float64
}

// summarize walks the dense row-major rectangle Window returns, restricted to
// the selection's columns. A short read — the range runs past the sheet's extent
// — contributes the rows it has, since the missing rows are empty.
func summarize(cells []Cell, lo, hi CellRef) aggregate {
	a := aggregate{Min: math.Inf(1), Max: math.Inf(-1)}
	rows := len(cells) / MaxCols
	for r := lo.Row; r <= hi.Row; r++ {
		i := r - lo.Row
		if i < 0 || i >= rows {
			continue
		}
		for c := lo.Col; c <= hi.Col && c < MaxCols; c++ {
			cell := cells[i*MaxCols+c]
			if cell.Kind == KindEmpty {
				continue
			}
			a.Count++
			if cell.Kind != KindNumber && cell.Kind != KindFormula {
				continue
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(cell.Computed), 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			a.Nums++
			a.Sum += v
			a.Min = math.Min(a.Min, v)
			a.Max = math.Max(a.Max, v)
		}
	}
	return a
}

// String is the status-bar line and deliberately the whole payload: the client
// does no formatting. An empty rectangle says nothing rather than "Sum 0",
// because zero is a claim about data and there is none.
func (a aggregate) String() string {
	if a.Count == 0 {
		return ""
	}
	if a.Nums == 0 {
		return "Count " + strconv.Itoa(a.Count)
	}
	var b strings.Builder
	b.Grow(64)
	b.WriteString("Sum ")
	b.WriteString(fmtAgg(a.Sum))
	b.WriteString(" · Avg ")
	b.WriteString(fmtAgg(a.Sum / float64(a.Nums)))
	b.WriteString(" · Count ")
	b.WriteString(strconv.Itoa(a.Count))
	b.WriteString(" · Min ")
	b.WriteString(fmtAgg(a.Min))
	b.WriteString(" · Max ")
	b.WriteString(fmtAgg(a.Max))
	return b.String()
}

// fmtAgg prints a number the way a status bar should: an integer stays an
// integer, and everything else gets at most 10 significant digits so an average
// of one third is `0.3333333333` rather than seventeen digits of float noise.
func fmtAgg(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', 10, 64)
}

// maxWriteCells caps every large-range write, and is deliberately one number for
// all of them: fill, paste, clear and style share it rather than each keeping a
// limit that has to be held in step with the others.
//
// The bound is really time on the sheet's single writer — one keystroke must not
// park it for longer than a person will wait. Through applyWrites, which is one
// cascade walk and one transaction, 10,000 cells is a few hundred milliseconds.
//
// Ctrl/Cmd+A over 26 columns of a 1,000-row sheet is 26,000 cells and is
// refused, which is the right answer for a cell command: styling a whole column
// or row applies at the level above the cell and is not capped (styleui.go).
const maxWriteCells = 10000

// rangeWrite is one cell's worth of a large-range operation: the address and the
// A1 text to put there. Building the whole list before writing anything is what
// makes an overlapping paste correct — the source is read once, up front, so a
// paste onto its own rows copies the original values rather than the ones the
// loop has just written.
//
// It is an alias, not a copy: recalc.go's BatchWrite is deliberately the same
// two fields, so a handler that has built this list has built that one and
// applyWrites hands it straight to the engine with no conversion. The name
// stays because it is what this file means.
type rangeWrite = BatchWrite

// applyWrites is the whole bulk write: one dependency walk over the union of
// everything the batch touches, then one transaction carrying every raw value,
// every recalculated value and one event per cell.
//
// The cost of a bulk write tracks the cascade, not the cell count, so a loop
// over the single-cell path would re-evaluate a band's SUM once per written cell
// and commit a transaction each time to store the same answer.
// `TestBatchEqualsLoop` (recalc_test.go) pins this path against that loop over
// 11 scenarios, hash-comparing the cells, the event log, the widths and both
// dependency directions.
//
// The recalc and the write are driven separately rather than through
// ApplyBatchCtx, which is the RecalcFunc seam: ApplyBatchCtx hard-wires the real
// engine where this goes through s.recalcBatch, so a Server built without one
// behaves the same way on a paste as on a keystroke. See
// ServerOptions.RecalcBatch.
//
// Runs inside one WriteSheet turn, so nothing else can interleave into a
// half-filled rectangle.
func (s *Server) applyWrites(ctx context.Context, sh *Sheet, writes []rangeWrite) ([]CellRef, error) {
	if len(writes) == 0 {
		return nil, nil
	}
	res, err := s.recalcBatch(sh, writes)
	if err != nil {
		return nil, err
	}
	if err := sh.WriteCellsCtx(ctx, writes, res.Computed); err != nil {
		return nil, err
	}
	// The written cells are always dirty even when the engine only reported
	// their cascade: a viewer holding that band must see the new raw text. This
	// is withRef's rule for a whole batch, done as a set rather than a linear
	// scan, which at a thousand writes would be O(N*M).
	return DirtyWithWrites(res.Dirty, writes), nil
}

// publishRange is the fan-out half every large-range command shares: record the
// dirty set, publish it once, and release the issuing screen's pending chip.
//
// One publish, not N: the bus is woken once whatever the size of the operation,
// so a 1,000-cell fill costs a viewer the same two frames a 10-cell one does,
// and each viewer patches only the cells it actually holds.
//
// grew is how far the write moved the sheet's allocated row extent — a fill or a
// paste that reaches past the last row extends the grid, and every screen on the
// sheet then owes its reader a taller scroll container.
func (s *Server) publishRange(ctx context.Context, sheetID, conn string, dirty []CellRef, grew int) uint64 {
	if scr := s.screen(conn); scr != nil && scr.sheetID == sheetID {
		// The chip comes down on the push that carries the new cells, not on this
		// command's response — the response returns before anything is on screen.
		scr.markChip()
	}
	seq := s.editLogFor(sheetID).Append(ctx, dirty)
	// Attribution for the change flash, on the same terms as a single edit's:
	// recorded before the publish, because the wake carries no payload and the
	// woken connection resolves "who" for itself. The ring is bounded at
	// attributionMax, so a 1,000-cell paste flashes only its tail — a thousand
	// simultaneously flashing cells would be a strobe rather than a hint.
	s.reg.NoteEdit(sheetID, conn, dirty)
	if s.bus != nil && len(dirty) > 0 {
		if err := s.bus.PublishDirty(sheetID, dirty); err != nil {
			obsLog.WarnContext(ctx, "publish.failed", "sheet", sheetID, "err", err.Error())
		}
	}
	if len(dirty) == 0 && grew == 0 {
		// Nothing was written, so nothing will wake this screen. Release its chip
		// directly or it hangs until the 20-second client timeout.
		s.notify(conn, "")
	}
	s.extentChanged(ctx, sheetID, grew)
	return seq
}

// fillSignals is what Ctrl+D / Ctrl+R sends: the selection, the direction, and
// the connection that asked. An explicit payload, so the selection is named on
// the one request that needs it and on no other.
type fillSignals struct {
	Conn string `json:"conn"`
	Rng  string `json:"rng"`
	Op   string `json:"op"` // "d" = fill down, "r" = fill right
}

// handleFill fills the selection from its top row (Ctrl+D) or its left column
// (Ctrl+R).
//
// The source row/column is read, never written, so a fill is idempotent and a
// second Ctrl+D over the same range writes nothing. Cells whose current text
// already equals what would be written are skipped for the same reason the clear
// skips empties: the dirty set is the patch, so keeping it to what actually
// changed is what keeps the push small.
func (s *Server) handleFill(w http.ResponseWriter, r *http.Request) {
	sheetID := r.PathValue("sheetID")
	if err := validSheetID(sheetID); err != nil {
		http.Error(w, "bad sheet id", http.StatusBadRequest)
		return
	}
	var sig fillSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	down := sig.Op != "r"
	lo, hi, err := ParseRangeBounds(sig.Rng)
	if err != nil {
		s.notify(sig.Conn, fillRefusal)
		obsLog.WarnContext(r.Context(), "fill.bad_range",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"range", sig.Rng, "op", sig.Op, "err", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, span := tracer.Start(r.Context(), "fill.command")
	defer span.End()
	started := time.Now()
	cells := (hi.Row - lo.Row + 1) * (hi.Col - lo.Col + 1)
	span.SetAttributes(
		attribute.String("sheet.id", sheetID),
		attribute.String("range", lo.String()+":"+hi.String()),
		attribute.String("fill.op", sig.Op),
		attribute.Int("range.cells", cells),
	)
	if cells > maxWriteCells {
		span.SetAttributes(attribute.Bool("refused", true))
		obsLog.WarnContext(ctx, "fill.too_big",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"range", lo.String()+":"+hi.String(), "op", sig.Op, "cells", cells)
		s.notify(sig.Conn, fillRefusal)
		http.Error(w, "range too large", http.StatusBadRequest)
		return
	}

	var dirty []CellRef
	wrote := 0
	var readDur, writeDur time.Duration
	// A fill can reach rows the sheet does not have yet, so the extent is
	// measured across the write. See writeSheetRows.
	var grew int
	grew, err = writeSheetRows(sheetID, func(sh *Sheet) error {
		t0 := time.Now()
		win, rerr := sh.WindowCtx(ctx, lo.Row, hi.Row)
		readDur = time.Since(t0)
		if rerr != nil {
			return rerr
		}
		at := func(row, col int) string {
			i := (row-lo.Row)*MaxCols + col
			if i < 0 || i >= len(win) {
				return ""
			}
			return win[i].Raw
		}

		writes := make([]rangeWrite, 0, cells)
		for row := lo.Row; row <= hi.Row; row++ {
			for col := lo.Col; col <= hi.Col; col++ {
				var src string
				var dr, dc int
				if down {
					if row == lo.Row {
						continue // the source row itself
					}
					src, dr = at(lo.Row, col), row-lo.Row
				} else {
					if col == lo.Col {
						continue // the source column itself
					}
					src, dc = at(row, lo.Col), col-lo.Col
				}
				raw := translateFormula(src, dr, dc)
				if at(row, col) == raw {
					continue // already says this — including empty over empty
				}
				writes = append(writes, rangeWrite{CellRef{Row: row, Col: col}, raw})
			}
		}
		wrote = len(writes)

		t1 := time.Now()
		defer func() { writeDur = time.Since(t1) }()
		dirty, rerr = s.applyWrites(ctx, sh, writes)
		return rerr
	})
	if err != nil {
		span.RecordError(err)
		s.notify(sig.Conn, humanCommandError("Fill failed", err))
		obsLog.WarnContext(ctx, "fill.failed",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"range", lo.String()+":"+hi.String(), "op", sig.Op, "err", err.Error())
		status := commandStatus(err)
		http.Error(w, commandErrorBody(err, status), status)
		return
	}

	editSeq := s.publishRange(ctx, sheetID, sig.Conn, dirty, grew)
	span.SetAttributes(
		attribute.Int("cells.written", wrote),
		attribute.Int("dirty_cells", len(dirty)),
		attribute.Int("dirty_bands", len(BandsFor(dirty))),
		attribute.Int64("edit.seq", int64(editSeq)),
		attribute.Float64("duration_ms", msf(time.Since(started))),
	)
	obsLog.InfoContext(ctx, "fill",
		"sheet", sheetID,
		"conn", sig.Conn,
		"name", s.authorName(sig.Conn),
		"range", lo.String()+":"+hi.String(),
		"op", sig.Op,
		"cells", cells,
		"written", wrote,
		"dirty_cells", len(dirty),
		"dirty_bands", len(BandsFor(dirty)),
		"read_ms", msf(readDur),
		"write_ms", msf(writeDur),
		"command_ms", msf(noteCommandNow(started)))

	s.respondCommand(w)
}

// pasteSignals is (sourceRange, destAnchor). The copy itself never reaches the
// server: a copy is client state, so Ctrl+C writes one local signal and issues
// nothing. This is the one request the whole gesture makes.
type pasteSignals struct {
	Conn string `json:"conn"`
	Src  string `json:"src"` // "B2:D5"
	Dst  string `json:"dst"` // "F10" — the destination's top-left corner
}

// handlePaste copies a rectangle to a destination anchor, translating relative
// references by the offset between the two.
//
// The source is snapshot before anything is written, which is what makes an
// overlapping paste (copy A1:A5, paste at A3) copy the original values instead
// of the ones the loop has just laid down. Reading first is also how a cell that
// already says what the paste would say is skipped.
func (s *Server) handlePaste(w http.ResponseWriter, r *http.Request) {
	sheetID := r.PathValue("sheetID")
	if err := validSheetID(sheetID); err != nil {
		http.Error(w, "bad sheet id", http.StatusBadRequest)
		return
	}
	var sig pasteSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	srcLo, srcHi, err := ParseRangeBounds(sig.Src)
	if err != nil {
		s.notify(sig.Conn, pasteRefusal)
		obsLog.WarnContext(r.Context(), "paste.bad_range",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"src", sig.Src, "dst", sig.Dst, "err", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dst, err := ParseRef(sig.Dst)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dr, dc := dst.Row-srcLo.Row, dst.Col-srcLo.Col
	dstHi := CellRef{Row: srcHi.Row + dr, Col: srcHi.Col + dc}
	if !dstHi.Valid() {
		// A paste that runs off the right edge or past the row ceiling. Sheets
		// refuses the same way, and refusing beats silently clipping half of it.
		s.notify(sig.Conn, pasteEdgeRefusal)
		obsLog.WarnContext(r.Context(), "paste.off_grid",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"src", srcLo.String()+":"+srcHi.String(), "dst", dst.String())
		http.Error(w, "paste would run off the sheet", http.StatusBadRequest)
		return
	}

	ctx, span := tracer.Start(r.Context(), "paste.command")
	defer span.End()
	started := time.Now()
	cells := (srcHi.Row - srcLo.Row + 1) * (srcHi.Col - srcLo.Col + 1)
	span.SetAttributes(
		attribute.String("sheet.id", sheetID),
		attribute.String("range", srcLo.String()+":"+srcHi.String()),
		attribute.String("paste.dst", dst.String()),
		attribute.Int("range.cells", cells),
	)
	if cells > maxWriteCells {
		span.SetAttributes(attribute.Bool("refused", true))
		obsLog.WarnContext(ctx, "paste.too_big",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"src", srcLo.String()+":"+srcHi.String(), "dst", dst.String(), "cells", cells)
		s.notify(sig.Conn, pasteRefusal)
		http.Error(w, "range too large", http.StatusBadRequest)
		return
	}

	var dirty []CellRef
	wrote := 0
	var readDur, writeDur time.Duration
	// A paste anchored below the last row extends the sheet, exactly as a cell
	// write past the bottom does. See writeSheetRows.
	var grew int
	grew, err = writeSheetRows(sheetID, func(sh *Sheet) error {
		t0 := time.Now()
		srcWin, rerr := sh.WindowCtx(ctx, srcLo.Row, srcHi.Row)
		if rerr != nil {
			return rerr
		}
		dstWin, rerr := sh.WindowCtx(ctx, dst.Row, dstHi.Row)
		readDur = time.Since(t0)
		if rerr != nil {
			return rerr
		}
		raw := func(win []Cell, base, row, col int) string {
			i := (row-base)*MaxCols + col
			if i < 0 || i >= len(win) {
				return ""
			}
			return win[i].Raw
		}

		writes := make([]rangeWrite, 0, cells)
		for row := srcLo.Row; row <= srcHi.Row; row++ {
			for col := srcLo.Col; col <= srcHi.Col; col++ {
				ref := CellRef{Row: row + dr, Col: col + dc}
				text := translateFormula(raw(srcWin, srcLo.Row, row, col), dr, dc)
				if raw(dstWin, dst.Row, ref.Row, ref.Col) == text {
					continue
				}
				writes = append(writes, rangeWrite{ref, text})
			}
		}
		wrote = len(writes)

		t1 := time.Now()
		defer func() { writeDur = time.Since(t1) }()
		dirty, rerr = s.applyWrites(ctx, sh, writes)
		return rerr
	})
	if err != nil {
		span.RecordError(err)
		s.notify(sig.Conn, humanCommandError("Paste failed", err))
		obsLog.WarnContext(ctx, "paste.failed",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"src", srcLo.String()+":"+srcHi.String(), "dst", dst.String(), "err", err.Error())
		status := commandStatus(err)
		http.Error(w, commandErrorBody(err, status), status)
		return
	}

	editSeq := s.publishRange(ctx, sheetID, sig.Conn, dirty, grew)
	span.SetAttributes(
		attribute.Int("cells.written", wrote),
		attribute.Int("dirty_cells", len(dirty)),
		attribute.Int("dirty_bands", len(BandsFor(dirty))),
		attribute.Int64("edit.seq", int64(editSeq)),
		attribute.Float64("duration_ms", msf(time.Since(started))),
	)
	obsLog.InfoContext(ctx, "paste",
		"sheet", sheetID,
		"conn", sig.Conn,
		"name", s.authorName(sig.Conn),
		"src", srcLo.String()+":"+srcHi.String(),
		"dst", dst.String(),
		"cells", cells,
		"written", wrote,
		"dirty_cells", len(dirty),
		"dirty_bands", len(BandsFor(dirty)),
		"read_ms", msf(readDur),
		"write_ms", msf(writeDur),
		"command_ms", msf(noteCommandNow(started)))

	s.respondCommand(w)
}

// The refusals. Each names the limit, because "it didn't work" leaves the reader
// with no way to pick a range that will work.
var (
	fillRefusal = "Can’t fill more than " + strconv.Itoa(maxWriteCells) +
		" cells at once — try a smaller range."
	pasteRefusal = "Can’t paste more than " + strconv.Itoa(maxWriteCells) +
		" cells at once — try a smaller range."
	pasteEdgeRefusal = "Can’t paste there — it would run off the edge of the sheet."
)
