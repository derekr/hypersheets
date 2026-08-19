package main

// push.go — turning a woken screen into bytes on the wire.
//
// Everything here runs per viewer, after something has decided that viewer needs
// to hear about a change. It renders that screen's own window, compares it
// against what the screen already holds, and sends the difference — or, roughly
// half the time, nothing at all.

import (
	"context"
	"log"
	"math/bits"
	"strconv"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func (s *Server) renderScreen(ctx context.Context, scr *screen, sel selRange) (string, []uint32, error) {
	ctx, span := tracer.Start(ctx, "render.window")
	defer span.End()
	span.SetAttributes(connAttrs(scr)...)

	lo, hi := scr.window()
	sh, err := OpenSheet(scr.sheetID)
	if err != nil {
		span.RecordError(err)
		return "", nil, err
	}

	// The read and the render happen once per (window, sheet version) however
	// many viewers want them; everyone after the first waits on that one render
	// rather than queueing another database read behind it. See windowcache.go.
	//
	// The version is read BEFORE the render and travels with the entry, so a
	// write landing mid-render produces a cache miss next time rather than a
	// window that is silently one edit behind.
	ver := sh.Version()
	h, err := s.windows.get(ctx, windowKey{scr.sheetID, lo, hi}, sh, ver,
		func(ctx context.Context) (windowHalves, error) {
			cells, rerr := s.readWindow(ctx, sh, lo, hi)
			if rerr != nil {
				return windowHalves{}, rerr
			}
			_, hspan := tracer.Start(ctx, "html.render")
			head, tail := renderWindowParts(cells, lo, hi, scr.sheetID)
			m := maskOf(cells, hi-lo+1)
			hspan.SetAttributes(
				attribute.Int("bytes", len(head)+len(tail)),
				attribute.Int("rows", hi-lo+1),
				attribute.Int("cells", len(cells)),
				attribute.Int("cells_emitted", countMask(m)),
			)
			hspan.End()
			return windowHalves{head: head, tail: tail, mask: m}, nil
		})
	if err != nil {
		span.RecordError(err)
		return "", nil, err
	}
	head, tail, mask := h.head, h.tail, h.mask

	// The selection is this viewer's own, and is the only part of a window that
	// differs between two people looking at the same rows.
	html := head + selHTML(sel) + tail

	rh, rm, rw := s.cells.stats()
	span.SetAttributes(
		attribute.Int("bytes", len(html)),
		attribute.Int64("read.hits", int64(rh)),
		attribute.Int64("read.misses", int64(rm)),
		attribute.Int64("read.waits", int64(rw)),
	)
	return html, mask, nil
}

// countMask is how many cells a mask says exist, which is the number the
// measurement wants: how many elements the render actually emitted, not how
// many slots the rectangle has.
func countMask(mask []uint32) int {
	n := 0
	for _, m := range mask {
		n += bits.OnesCount32(m)
	}
	return n
}

// delta is what a scroll ships: the row numbers and the cells of the newly
// revealed rows, plus the masks that say which cells those are. The two ranges
// collapse into one pair of frames because every element carries its position
// as `--r` rather than taking it from DOM order.
type delta struct {
	Nums     string   // gutter numbers for both ranges
	Cells    string   // non-empty cells for both ranges
	PreMask  []uint32 // mask for d.Prepend
	PostMask []uint32 // mask for d.Append
	Read     int      // dense cells read from the store
	Emitted  int      // elements actually produced
}

// renderDelta reads and renders only the rows a scroll newly revealed: a
// one-band scroll reads 50 rows instead of 500.
//
// The two ranges are separate reads on purpose. A buffer that grows at both
// ends — the browser reporting a taller viewport than was assumed — reveals
// rows above and below with the client's existing rows in between, and one read
// spanning both would pull back everything the client already has.
func (s *Server) renderDelta(ctx context.Context, scr *screen, d windowDiff) (delta, error) {
	ctx, span := tracer.Start(ctx, "render.delta")
	defer span.End()
	span.SetAttributes(connAttrs(scr)...)

	var out delta
	sh, err := OpenSheet(scr.sheetID)
	if err != nil {
		span.RecordError(err)
		return out, err
	}
	var nums, cells strings.Builder
	read := func(r rowRange) ([]uint32, error) {
		if r.empty() {
			return nil, nil
		}
		got, rerr := s.readWindow(ctx, sh, r.Lo, r.Hi)
		if rerr != nil {
			return nil, rerr
		}
		out.Read += len(got)
		nums.WriteString(renderRowNums(r.Lo, r.Hi))
		cells.WriteString(renderCellRows(got, r.Lo, r.Hi))
		return maskOf(got, r.rows()), nil
	}
	if out.PreMask, err = read(d.Prepend); err != nil {
		span.RecordError(err)
		return delta{}, err
	}
	if out.PostMask, err = read(d.Append); err != nil {
		span.RecordError(err)
		return delta{}, err
	}
	out.Nums, out.Cells = nums.String(), cells.String()
	out.Emitted = countMask(out.PreMask) + countMask(out.PostMask)

	// Same span name as every other render path, so `-analyze` aggregates them.
	_, hspan := tracer.Start(ctx, "html.render")
	hspan.SetAttributes(
		attribute.Int("bytes", len(out.Nums)+len(out.Cells)),
		attribute.Int("rows", d.Rows()),
		attribute.Int("cells", out.Read),
		attribute.Int("cells_emitted", out.Emitted),
	)
	hspan.End()

	span.SetAttributes(attribute.Int("bytes", len(out.Nums)+len(out.Cells)))
	return out, nil
}

// GET / and POST /sheets live in index.go.

// ─── GET /s/{sheetID} ─────────────────────────────────────────────────────────

// handlePage serves the shell with the first buffer already rendered. First
// paint must not wait for the SSE round trip — the stream's job is to keep the
// grid current, not to draw it the first time.
func (s *Server) pushWindow(ctx context.Context, span trace.Span, sse *datastar.ServerSentEventGenerator,
	scr *screen, cause string, since time.Time, client clientTiming,
) (bool, error) {
	heldLo, heldHi, ok := scr.heldWindow()
	if !ok {
		return false, nil
	}
	lo, hi := scr.window()
	d := diffWindow(heldLo, heldHi, lo, hi)
	if d.Full {
		span.SetAttributes(attribute.Bool("patch.fallback", true))
		return false, nil
	}
	// A jump can change the region without moving the buffer — go to A1:D20 from
	// two rows above it and the window does not move at all. That is patchable
	// only because the region is one positioned box rather than a class on each
	// of its cells: as a class, the cells that had to lose it would be in no
	// range an incremental diff knows about, and the only correct answer would be
	// a full re-render. As one element it is a single ~60-byte patch.
	sel := scr.selection()
	selChanged := scr.selStale()
	span.SetAttributes(
		attribute.String("patch.path", "incremental"),
		attribute.Int("patch.rows_added", d.Rows()),
		attribute.Int("patch.rows_dropped", d.Dropped()),
	)
	if d.NoOp() && !selChanged {
		// A wake for a window that did not move. Nothing to say, and unlike the
		// full path there is no render to throw away first.
		span.SetAttributes(attribute.Bool("suppressed", true))
		s.logPush(ctx, scr, cause, since, client, 0, 0, 0, 0, 0, true)
		return true, nil
	}

	// The mask for the rows about to be dropped has to be read before the held
	// window moves: it is the only thing that knows which cell ids they contain.
	dropSel := appendSelector(d.DropRowNums(), appendSelector(
		maskSelector(scr.heldRange(d.DropHead.Lo, d.DropHead.Hi), d.DropHead.Lo, d.DropHead.Lo, d.DropHead.Hi),
		maskSelector(scr.heldRange(d.DropTail.Lo, d.DropTail.Hi), d.DropTail.Lo, d.DropTail.Lo, d.DropTail.Hi)))

	t0 := time.Now()
	dl, err := s.renderDelta(ctx, scr, d)
	renderDur := time.Since(t0)
	if err != nil {
		span.RecordError(err)
		log.Printf("render delta %s [%s]: %v", scr.sheetID, scr.id, err)
		return true, nil // a bad read must not kill the stream
	}

	s.reg.DeliverIncremental(scr.id)
	s.outboundLeg()

	_, pspan := tracer.Start(ctx, "sse.patch")
	before := scr.obs.wire.Load()
	t2 := time.Now()

	raw := 0
	// Remove first, so the DOM never holds two elements with one id: a buffer
	// can shrink at one end and grow at the other into rows it was already
	// holding.
	if dropSel != "" {
		raw += len(dropSel)
		if perr := patch(sse, "", datastar.WithModeRemove(), datastar.WithSelector(dropSel)); perr != nil {
			pspan.RecordError(perr)
			pspan.End()
			return true, perr
		}
	}
	// One append each, covering both ends at once: position comes from `--r` and
	// not from DOM order, so rows revealed above and below land correctly.
	if dl.Nums != "" {
		raw += len(dl.Nums)
		if perr := patch(sse, dl.Nums, datastar.WithModeAppend(), datastar.WithSelectorID(gutterID)); perr != nil {
			pspan.RecordError(perr)
			pspan.End()
			return true, perr
		}
	}
	if dl.Cells != "" {
		raw += len(dl.Cells)
		if perr := patch(sse, dl.Cells, datastar.WithModeAppend(), datastar.WithSelectorID(bufferID)); perr != nil {
			pspan.RecordError(perr)
			pspan.End()
			return true, perr
		}
	}
	if selChanged {
		if perr := s.patchSelection(sse, scr.heldSelection(), sel); perr != nil {
			pspan.RecordError(perr)
			pspan.End()
			return true, perr
		}
		raw += len(selHTML(sel))
	}
	// Read the retained part of the mask before the held window moves.
	newMask := slideMask(scr.heldRange(max(heldLo, lo), min(heldHi, hi)), d, dl, lo, hi)
	scr.setHeldWindow(lo, hi, sel, newMask)
	perr := s.patchBounds(sse, lo, hi)

	patchDur := time.Since(t2)
	wire := scr.obs.wire.Load() - before
	pspan.SetAttributes(
		attribute.Int("bytes_raw", raw),
		attribute.Int64("bytes_compressed", int64(wire)),
		attribute.Float64("duration_ms", msf(patchDur)),
		attribute.String("patch.path", "incremental"),
	)
	pspan.SetAttributes(connAttrs(scr)...)
	if perr != nil {
		pspan.RecordError(perr)
	}
	pspan.End()

	span.SetAttributes(attribute.Bool("suppressed", false))
	s.logPush(ctx, scr, cause, since, client, renderDur, 0, patchDur, raw, int(wire), false)
	return true, perr
}

// logPush writes the one line per delivered (or suppressed) window that the
// analysis reads. The client fields describe the previous patch, because a
// browser cannot report the cost of a patch it has not received yet.
func (s *Server) logPush(ctx context.Context, scr *screen, cause string, since time.Time, client clientTiming,
	render, deliver, patchDur time.Duration, raw, wire int, suppressed bool,
) {
	lo, hi := scr.window()
	attrs := []any{
		"conn", scr.id,
		"sheet", scr.sheetID,
		"lo_row", lo,
		"hi_row", hi,
		"cause", cause,
		"suppressed", suppressed,
		"render_ms", msf(render),
		"deliver_ms", msf(deliver),
		"patch_ms", msf(patchDur),
		"bytes_raw", raw,
		"bytes_wire", wire,
	}
	if !since.IsZero() {
		attrs = append(attrs, "since_command_ms", msf(time.Since(since)))
	}
	if client.any() {
		attrs = append(attrs,
			"client_debounce_ms", client.DebounceMs,
			"client_quiet_ms", client.QuietMs,
			"client_prev_wait_ms", client.WaitMs,
			"client_prev_morph_ms", client.MorphMs,
			"client_prev_render_ms", client.RenderMs,
		)
	}
	obsLog.InfoContext(ctx, "push", attrs...)
}

// ─── POST /s/{sheetID}/cell ───────────────────────────────────────────────────

// commandStatus maps a store error onto an HTTP status, in one place because
// every command handler asks the same question.
//
// ErrRecalcTooLarge is classified separately from ErrBadRef because the reader
// gets a different sentence for each: a malformed reference is bad input, while
// an edit whose cascade exceeds maxRecalcNodes is a resource limit. Both answer
// 400, since both are the client's to fix by asking for something smaller.
//
// ErrCellTooLong is deliberately absent: store.go makes it answer errors.Is for
// ErrBadRef, so it inherits this mapping — a 4 MB cell really is bad input.
func (s *Server) patchWidths(sse *datastar.ServerSentEventGenerator, sheetID string) error {
	sh, err := OpenSheet(sheetID)
	if err != nil {
		return err
	}
	widths, err := sh.ColWidths()
	if err != nil {
		return err
	}
	sig := make(map[string]any, MaxCols+2)
	for c := 0; c < MaxCols; c++ {
		sig["_w"+strconv.Itoa(c)] = colWidthOf(widths, c)
	}
	sig["rc"] = -1

	// The row heights ride the same frame. Both axes are woken by the same flag
	// (markSheetWidths), and a viewer that has the new geometry in CSS but not in
	// its offset table would paint the rows correctly and answer a click with the
	// wrong one — which is exactly the state the first version of this shipped in.
	if heights, herr := sh.RowHeights(0, sh.Rows()-1); herr == nil {
		rows := make([]int, 0, len(heights)*2)
		for _, r := range sortedKeys(heights) {
			rows = append(rows, r, heights[r])
		}
		sig[heightSignal] = rows
	}
	return sse.MarshalAndPatchSignals(sig)
}

// patchStyles sends the sheet's stylesheet if this screen's copy is out of date.
//
// One element, not a grid render, which is why styling is cheap on the wire:
// the rules are O(distinct styles) — a dozen on a real sheet — and every cell
// wearing one says only `s3`. It is a comparison rather than a flag set by
// whoever styled something, for the reason patchExtent is.
func (s *Server) patchStyles(sse *datastar.ServerSentEventGenerator, scr *screen) error {
	sh, err := OpenSheet(scr.sheetID)
	if err != nil {
		return err
	}
	// The window is part of the stylesheet: the row level of the cascade is scoped
	// to the rows this screen holds (sheetStyleCSS), so the compare is against what
	// this screen should have. It is also what makes a scroll into a styled region
	// arrive as a rule rather than a re-render.
	lo, hi := scr.window()
	css, changed := scr.takeStyles(sheetStyleCSS(sh, lo, hi))
	if !changed {
		return nil
	}
	// An `outer` morph resolved by the incoming element's own id, which reaches
	// `<head>` as happily as the grid: Datastar's element patch falls back to
	// document.getElementById for any fragment that is not a whole document.
	if err := patch(sse, styleSheetHTML(css)); err != nil {
		scr.forgetStyles()
		return err
	}
	return nil
}

// patchExtent sends the sheet's allocated row extent if this screen's copy is
// out of date, and returns what it sent (0 for "nothing to say").
//
// One signal, not a grid render: `--rows` sizes the scroll container and
// nothing else, so a sheet that grew from 1,000 rows to 15,000 under an
// hour-old screen is corrected by ~15 bytes.
//
// It compares the sheet's current extent against what this screen holds rather
// than using Dirty.Stats.Grew, because screens connect at different times, miss
// different frames and reconnect mid-history.
func (s *Server) patchExtent(sse *datastar.ServerSentEventGenerator, scr *screen) (int, error) {
	sh, err := OpenSheet(scr.sheetID)
	if err != nil {
		return 0, err
	}
	rows, changed := scr.takeExtent(sh.Rows())
	if !changed {
		return 0, nil
	}
	if err := sse.MarshalAndPatchSignals(map[string]int{"_rows": rows}); err != nil {
		scr.forgetExtent()
		return 0, err
	}
	return rows, nil
}

// patchLatency restates the server's own command p50 if it has moved enough to
// be worth saying — see screen.takeLatency for the threshold, latency.go for
// what the reader does with the number.
//
// It adds no wake: it is called from push() and nowhere else, so an idle
// viewer's chip goes on showing the figure their page shell was rendered with
// rather than costing a timer that wakes every viewer.
func (s *Server) patchLatency(sse *datastar.ServerSentEventGenerator, scr *screen) error {
	p50, changed := scr.takeLatency(commandLatency.p50(), time.Now())
	if !changed {
		return nil
	}
	if err := sse.MarshalAndPatchSignals(map[string]float64{latencySignal: p50}); err != nil {
		scr.forgetLatency()
		return err
	}
	return nil
}

// ─── POST /s/{sheetID}/viewport ───────────────────────────────────────────────
