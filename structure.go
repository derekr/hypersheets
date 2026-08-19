package main

// structure.go — insert and delete rows and columns.
//
//	POST /s/{sheetID}/rows   signals: mop (ia|ib|d|ap), mi (0-indexed row), mn (count, ap only)
//	POST /s/{sheetID}/cols   signals: mop (ia|ib|d), mi (0-indexed column)
//
// `ap` — append n rows at the bottom — is the odd one out: it is the only
// operation here that moves nothing. It is the foot control's command
// (growrows.go), and it shares this handler rather than having a route of its
// own so it inherits the refusal path, the pending chip, the extent broadcast
// and limits.go's classWrite rate limit.
//
// This is the large-range worst case and is supposed to look like one. SPEC.md
// lists insert/delete row as the operation this architecture is least suited to:
// inserting a row at 0 moves every cell below it — 220,000 cells and 40,000
// dependency edges — and every formula in the sheet has to be re-parsed in case
// a reference crossed the insertion point. mutate.go does that in one
// transaction and reports what it cost (MutateStats); this file's job is to make
// the wait honest rather than to pretend it is fast. Hence:
//
//  1. The command is synchronous, holding the sheet's actor for the length of
//     the transaction. Backgrounding it would let a second command interleave
//     against a half-shifted grid, and there is no version vector to catch that.
//  2. The push is a full window render per viewer. The dirty set of a row insert
//     is "everything below it", so the cell-patch path would be strictly worse
//     than a morph, and the scroll-delta path would be wrong: the rows it
//     preserves are the rows whose contents moved.
//  3. The client is told it is waiting. `$p` goes true on the click and is
//     cleared by the server on the push that carries the new grid — see push().

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/starfederation/datastar-go/datastar"
	"go.opentelemetry.io/otel/attribute"
)

// structSignals is what a menu item sends. `mop` is the operation and `mi` the
// 0-indexed row or column the menu was opened on.
type structSignals struct {
	Mop string `json:"mop"`
	Mi  int    `json:"mi"`
	// Mn is how many, and only the append operation reads it: the menu's three
	// operations are fixed at one because the menu offers one. Zero or missing
	// means growRowsDefault — see growrows.go for why an emptied input arrives
	// as 0.
	Mn int `json:"mn"`
	// Conn names the screen that asked. A refusal is reported to that one viewer
	// only; everybody else's grid is unchanged and has nothing to be told.
	Conn string `json:"conn"`
}

// handleRows inserts or deletes one row. The count is fixed at one because the
// menu offers one; the store API takes n, so nothing here would have to change
// to expose "insert 5".
func (s *Server) handleRows(w http.ResponseWriter, r *http.Request) {
	s.applyStructural(w, r, axisRow)
}

// handleCols is the same for columns.
func (s *Server) handleCols(w http.ResponseWriter, r *http.Request) {
	s.applyStructural(w, r, axisCol)
}

func (s *Server) applyStructural(w http.ResponseWriter, r *http.Request, axis axisKind) {
	sheetID := r.PathValue("sheetID")
	if err := validSheetID(sheetID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var sig structSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		http.Error(w, "read signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	// The bound here is the widest the axis could possibly be; the store checks
	// the index against the sheet's actual extent, which is the only place that
	// knows it. Rows have no fixed size any more, so this is the sanity ceiling.
	limit := RowCeiling
	if axis == axisCol {
		limit = MaxCols
	}
	if sig.Mi < 0 || sig.Mi >= limit {
		http.Error(w, "index out of range", http.StatusBadRequest)
		return
	}

	// "Insert below" is "insert above the next one". Doing the arithmetic here
	// rather than in the store keeps the store's contract to one rule — insert
	// BEFORE `at` — which is the rule every reference-rewrite in mutate.go is
	// written against.
	at, del, n := sig.Mi, false, 1
	// `ap` appends at the bottom (the foot control, growrows.go). `at` cannot be
	// computed here: the append position is the current extent, and reading it
	// and acting on it without a race is only possible inside the actor, so this
	// is a marker and the closure below resolves it.
	//
	// It is a row operation only: the column axis is fixed at MaxCols (grid.go)
	// and has no append position.
	appendRows := false
	switch sig.Mop {
	case "ia":
	case "ib":
		at = sig.Mi + 1
		// "Insert below the last row" is a real position on the row axis:
		// `at == extent` is the append position and the store grows the sheet to
		// make room. The column axis is fixed, so there it is a no-op.
		if axis == axisCol && at >= limit {
			s.respondCommand(w)
			return
		}
	case "d":
		del = true
	case growRowsOp:
		if axis != axisRow {
			http.Error(w, "unknown operation", http.StatusBadRequest)
			return
		}
		appendRows = true
		// Clamped here, before the actor is held, because both bounds are facts
		// about the request rather than about the sheet: zero is an emptied input
		// box and means "the default", and growRowsMax is how much work one click
		// may ask for. The remaining bound — the sheet's headroom below
		// RowCeiling — is not knowable yet and is applied inside the closure.
		n = sig.Mn
		if n <= 0 {
			n = growRowsDefault
		}
		if n > growRowsMax {
			n = growRowsMax
		}
	default:
		http.Error(w, "unknown operation", http.StatusBadRequest)
		return
	}

	ctx, span := tracer.Start(r.Context(), "structure.command")
	defer span.End()
	started := time.Now()
	span.SetAttributes(
		attribute.String("sheet.id", sheetID),
		attribute.String("axis", axis.String()),
		attribute.String("op", sig.Mop),
		attribute.Int("n", n),
	)

	var dirty Dirty
	err := WriteSheet(sheetID, func(sh *Sheet) error {
		var derr error
		switch {
		case appendRows:
			// The append position is the extent — `at == extent`, the one
			// asymmetry in the store's argument validation. No new store API:
			// this is the same InsertRows every other row op calls.
			at = sh.Rows()
			// Clamp to the headroom rather than passing the request through. The
			// store refuses the whole command for asking one row too many, which
			// would turn "add 1,000 more rows" into nothing at all when 400 were
			// available.
			if room := RowCeiling - at; n > room {
				n = room
			}
			if n < 1 {
				return errAtRowCeiling(at)
			}
			dirty, derr = sh.InsertRows(at, n)
		case axis == axisRow && del:
			dirty, derr = sh.DeleteRows(at, 1)
		case axis == axisRow:
			dirty, derr = sh.InsertRows(at, 1)
		case del:
			dirty, derr = sh.DeleteCols(at, 1)
		default:
			dirty, derr = sh.InsertCols(at, 1)
		}
		return derr
	})
	// `at` is recorded here rather than with the attributes above because an
	// append does not know it until the actor has read the extent. Set on both
	// paths so a refusal still says where.
	span.SetAttributes(attribute.Int("at", at))
	if err != nil {
		span.RecordError(err)
		// commandStatus covers ErrBadRef and the fan-out refusal; the three
		// below are this axis's own, and they are all "you asked for
		// something the sheet cannot do", not "the server broke".
		status := commandStatus(err)
		if errors.Is(err, ErrWouldTruncate) ||
			errors.Is(err, ErrRowCeiling) || errors.Is(err, ErrBadMutation) {
			status = http.StatusBadRequest
		}
		obsLog.WarnContext(ctx, "structure.failed",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"axis", axis.String(), "op", sig.Mop, "at", at, "err", err.Error())
		// The command produced no push, so without a notice the asker's pending
		// chip would simply disappear and the menu would look broken — when in
		// fact the store refused for a good reason.
		s.notify(sig.Conn, humanStructError(err, axis, del, appendRows))
		http.Error(w, err.Error(), status)
		return
	}

	// Every viewer of this sheet is marked, not just the ones whose bands are
	// dirty: a shift changes what row N means, so a viewer parked 5,000 rows
	// below the insertion point is stale even though its own band's contents did
	// not change. The bands are published too — that is the SPEC contract and
	// what a second process would need — but the direct marking is what makes
	// this correct in one process.
	//
	// An append moves nothing (`at == extent`), so no cell changed address, no
	// formula was rewritten and every rendered window is still right. What those
	// screens are owed is a taller scroll container, which is the small `_rows`
	// patch extentChanged already sends; a whole-window morph per viewer to
	// deliver one integer is the mistake this prototype exists to measure. The
	// issuing screen still gets its chip taken down, on the frame that carries
	// the new extent.
	woke := 0
	if appendRows {
		if scr := s.screen(sig.Conn); scr != nil && scr.sheetID == sheetID {
			scr.markChip()
		}
	} else {
		woke = s.markSheetFull(ctx, sheetID)
	}

	// The scroll container is the one thing the full morph above cannot fix:
	// `--rows` lives on `#vp`, page-shell markup no push touches, so a grid
	// re-render restates every row and says nothing about how tall the sheet is.
	// Dirty.Stats.Grew is the net change the mutation committed — zero for almost
	// every insert and delete, since a default-sized sheet takes its rows from
	// the blank tail and gives them back — so this is a compare against an
	// integer the store already had.
	//
	// On a delete it is negative, which is the case to be careful about:
	// markSheetExtent re-seats any screen whose buffer is now past the end of the
	// sheet before the structural render reads it, so what goes out is the new
	// bottom of the sheet rather than an empty window over rows that are gone.
	s.extentChanged(ctx, sheetID, dirty.Stats.Grew)

	if s.bus != nil && len(dirty.Bands) > 0 {
		if perr := s.bus.Publish(sheetID, dirty.Bands); perr != nil {
			span.RecordError(perr)
		}
	}

	st := dirty.Stats
	span.SetAttributes(
		attribute.Int("dirty.bands", len(dirty.Bands)),
		attribute.Bool("dirty.structural", dirty.Structural),
		attribute.Int("cells.moved", st.CellsMoved),
		attribute.Int("cells.dropped", st.CellsDropped),
		attribute.Int("sheet.grew", st.Grew),
		attribute.Int("formulas.rewritten", st.FormulasRewrit),
		attribute.Int("screens", woke),
		attribute.Float64("duration_ms", msf(time.Since(started))),
	)
	obsLog.InfoContext(ctx, "structure",
		"sheet", sheetID,
		"conn", sig.Conn,
		"name", s.authorName(sig.Conn),
		"axis", axis.String(),
		"op", sig.Mop,
		"at", at,
		"n", n,
		"bands", len(dirty.Bands),
		"cells_moved", st.CellsMoved,
		"cells_dropped", st.CellsDropped,
		"grew", st.Grew,
		"deps_moved", st.DepsMoved,
		"formulas_seen", st.FormulasSeen,
		"formulas_rewritten", st.FormulasRewrit,
		"recomputed", st.Recomputed,
		"screens", woke,
		"shift_ms", msf(st.Shift()),
		"scan_ms", msf(st.Scan),
		"recalc_ms", msf(st.Recalc),
		"persist_ms", msf(st.Persist()),
		"command_ms", msf(noteCommandNow(started)))

	s.respondCommand(w)
}

// errAtRowCeiling is the append's own refusal: the sheet is already as tall as a
// sheet gets, so there is no headroom to take even one row from.
//
// It is raised here rather than left to the store because the store refuses the
// command it was given — ask for 1,000 rows with 400 available and it declines
// all 1,000 — which is right for an API and wrong for a button. The clamp above
// takes what is there; this is the only case left. It wraps the store's own
// sentinel so applyStructural's classifier still calls it a 400, not a 500.
func errAtRowCeiling(rows int) error {
	return fmt.Errorf("%w: the sheet is already %d rows tall", ErrRowCeiling, rows)
}

// markSheetFull flags every screen on a sheet for a whole-window render and
// wakes it. Returns how many screens that was.
func (s *Server) markSheetFull(ctx context.Context, sheetID string) int {
	s.mu.RLock()
	scrs := make([]*screen, 0, len(s.screens))
	for _, scr := range s.screens {
		if scr.sheetID == sheetID {
			scrs = append(scrs, scr)
		}
	}
	s.mu.RUnlock()
	for _, scr := range scrs {
		scr.markFull()
		scr.obs.setCause(ctx, "structural", clientTiming{})
		// The digest has to be forgotten, for the same reason a scroll patch
		// forgets it: this screen's DOM no longer matches the bytes the registry
		// remembers sending. Without this, a shift that happens to produce the
		// same window bytes as last time (deleting a row in an empty region)
		// would be suppressed and the client would keep a stale grid.
		s.reg.ForgetDigest(scr.id)
		s.reg.markDirty(scr.conn)
	}
	return len(scrs)
}

// notify sends one screen a one-line explanation and releases its pending chip.
// It does not force a re-render: nothing about the grid changed, so spending a
// whole-window morph to deliver a sentence would be the exact mistake this
// prototype is about.
func (s *Server) notify(connID, msg string) {
	if connID == "" {
		return
	}
	scr := s.screen(connID)
	if scr == nil {
		return
	}
	scr.setNotice(msg)
	scr.obs.setCause(context.Background(), "notice", clientTiming{})
	s.reg.markDirty(scr.conn)
}

// humanStructError turns a store error into something worth reading. The store's
// own message names the cell it would have destroyed, which is genuinely useful,
// but it opens with plumbing.
func humanStructError(err error, axis axisKind, del, appendRows bool) string {
	switch {
	// There is no row-truncation refusal: a sheet whose last row has content
	// grows instead. The only row refusal is the sanity ceiling, and it states
	// numbers rather than policy, because at a million rows the useful
	// information is "you are already there".
	case errors.Is(err, ErrRowCeiling) && appendRows:
		// The foot control's own wording: "Can't insert" is right for a menu item
		// and wrong for a button that says "Add". `fixed` is the sheet's own
		// thousands grouping (style.go), so the number in the sentence is spelled
		// the way every number in a cell is.
		return "This sheet is already " + fixed(float64(RowCeiling), 0) +
			" rows tall, which is as tall as a sheet gets. No rows were added."
	case errors.Is(err, ErrRowCeiling):
		return "Can’t insert: a sheet stops at 1,000,000 rows."
	case errors.Is(err, ErrWouldTruncate):
		return "Can’t insert: the last column has content, and the sheet is a fixed 26 columns."
	// The fan-out refusal reaches this axis too: a shift re-parses every formula
	// in the sheet, and the affected set is capped exactly as an edit's is.
	case errors.Is(err, ErrRecalcTooLarge):
		return humanCommandError("", err)
	case del:
		return "Delete failed: " + err.Error()
	default:
		return "Insert failed: " + err.Error()
	}
}
