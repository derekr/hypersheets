package main

// http.go — the page, the stream, and the commands. Routes() lists them all;
// these four carry the design:
//
//	GET  /s/{sheetID}           page shell + initial buffer render + SSE connect
//	GET  /s/{sheetID}/live      held-open Datastar SSE, one per screen
//	POST /s/{sheetID}/cell      commit an edit (signals: ref, raw)
//	POST /s/{sheetID}/viewport  client reports a new buffer (debounced, edge-triggered)
//
// CQRS is the load-bearing split: commands mutate and return nothing
// renderable, and every read arrives on the one long-lived stream per screen.
// A viewer's own edit reaches them by the same push that delivers it to
// everyone else, so no code path treats the editor's view as a special case.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/starfederation/datastar-go/datastar"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ─── Compression instrumentation ──────────────────────────────────────────────
//
// htmlBytes is what the SDK is handed; wireBytes is what leaves the socket after
// compression. Both are counted rather than estimated because bytes on the wire
// per committed edit is a headline property of this design.

var (
	htmlBytes atomic.Uint64
	wireBytes atomic.Uint64
)

type countingResponseWriter struct {
	http.ResponseWriter
	counter *atomic.Uint64
	// conn is this stream's own counter. The global one above is shared by every
	// open stream, so it cannot say what a single patch cost on the wire.
	conn *atomic.Uint64
}

func (c *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	if n > 0 {
		c.counter.Add(uint64(n))
		if c.conn != nil {
			c.conn.Add(uint64(n))
		}
	}
	return n, err
}

func (c *countingResponseWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// newSSE wraps the response writer so post-compression bytes are counted, then
// hands it to the SDK with brotli/zstd/gzip/deflate negotiated by server
// priority. Every SSE handler in this binary goes through here.
//
// The compressor wraps the stream once, not each event, so brotli keeps its
// window across pushes: two buffer renders differing by one cell cost a
// fraction of the first, which is what makes a fat morph affordable.
func newSSE(w http.ResponseWriter, r *http.Request) *datastar.ServerSentEventGenerator {
	return newSSEFor(w, r, nil)
}

// newSSEFor is newSSE with a per-connection byte counter threaded through, so a
// single push can be measured after compression.
func newSSEFor(w http.ResponseWriter, r *http.Request, conn *atomic.Uint64) *datastar.ServerSentEventGenerator {
	return datastar.NewSSE(
		&countingResponseWriter{ResponseWriter: w, counter: &wireBytes, conn: conn},
		r,
		datastar.WithCompression(),
	)
}

// patch sends an HTML region and bumps the uncompressed counter. Use instead of
// sse.PatchElements so the byte accounting stays honest.
func patch(sse *datastar.ServerSentEventGenerator, html string, opts ...datastar.PatchElementOption) error {
	htmlBytes.Add(uint64(len(html)))
	return sse.PatchElements(html, opts...)
}

// ─── The recalc seam ──────────────────────────────────────────────────────────

// RecalcFunc is recalc.go's contract, held as a value so tests can drive the
// edit handler with a known dirty set instead of the real dependency graph.
//
// It runs inside the write transaction's actor turn and before WriteCell, so it
// must evaluate against an overlay of {ref: raw}: the database still holds the
// old value while it runs. See store.go's WriteCell ordering contract.
type RecalcFunc func(sh *Sheet, ref CellRef, raw string) (RecalcResult, error)

// literalRecalc is the degenerate engine: no formulas, no cascade, the edited
// cell is the only dirty cell. Used when no engine is wired in.
func literalRecalc(_ *Sheet, ref CellRef, _ string) (RecalcResult, error) {
	return RecalcResult{Dirty: []CellRef{ref}}, nil
}

// ─── Server ───────────────────────────────────────────────────────────────────

// defaultViewportRows is the assumed on-screen row count before a client has
// reported a real one.
const (
	latencyMoveRatio = 0.20
	latencyMoveFloor = 0.05 // ms
	latencyMoveGap   = 10 * time.Second
)

// takeLatency reports the server's command p50 if this screen has to be told
// about it. A p50 of 0 means nothing has been measured yet and is never sent:
// the page shell's seeded figure stands until a real one displaces it.
type ServerOptions struct {
	Registry *Registry
	Bus      *Bus
	Recalc   RecalcFunc
	// RecalcBatch is the same seam for a bulk write — a range clear, fill or
	// paste — so a Server built without an engine behaves the same way on a
	// range command as on a keystroke.
	//
	// It defaults off `Recalc` rather than off nil, which is why it is a field
	// and not a direct call to (*Sheet).ApplyBatch: calling ApplyBatch directly
	// would give an engine-less server the real dependency graph for a paste and
	// the degenerate one for a keystroke.
	RecalcBatch  BatchRecalcFunc
	DefaultSheet string
	// BufferBands is how many bands to over-fetch on each side of the viewport,
	// so ordinary scrolling never touches the network.
	BufferBands int
	// Latency is a symmetric artificial delay for the benchmark: a request pays
	// it inbound and outbound, a push on an open stream pays one leg.
	Latency time.Duration
}

// Server wires the registry, the bus and the store into the routes.
type Server struct {
	// cells caches the window READ, which is what every render path shares and
	// where the contention was. windows caches the rendered halves on top of it
	// for the one path whose markup is viewer-independent. See windowcache.go.
	cells   *flightCache[windowKey, []Cell]
	windows *flightCache[windowKey, windowHalves]

	reg          *Registry
	bus          *Bus
	recalc       RecalcFunc
	recalcBatch  BatchRecalcFunc
	defaultSheet string
	bufferBands  int
	latency      time.Duration

	mu      sync.RWMutex
	screens map[string]*screen

	// edits is the per-sheet dirty-set history. The bus carries no payload, so a
	// woken connection cannot learn what changed; this holds both answers — the
	// trace context, so one edit stays one trace across the fan-out, and the cells,
	// so a push can be the dirty set rather than the buffer. See editlog.go for why
	// it is a bounded log and not a single slot.
	editsMu sync.Mutex
	edits   map[string]*editLog
}

// NewServer builds the HTTP layer.
func NewServer(opts ServerOptions) *Server {
	// See ServerOptions.RecalcBatch: the batch engine follows the single-cell one,
	// so an engine-less server is degenerate on both paths and a real one is real
	// on both.
	rc, rb := opts.Recalc, opts.RecalcBatch
	switch {
	case rc == nil && rb == nil:
		rc, rb = literalRecalc, literalRecalcBatch
	case rb == nil:
		rb = RecalcBatch
	case rc == nil:
		rc = literalRecalc
	}
	bb := opts.BufferBands
	if bb < 0 {
		bb = 0
	}
	return &Server{
		cells:        newFlightCache[windowKey, []Cell](),
		windows:      newFlightCache[windowKey, windowHalves](),
		reg:          opts.Registry,
		bus:          opts.Bus,
		recalc:       rc,
		recalcBatch:  rb,
		defaultSheet: opts.DefaultSheet,
		bufferBands:  bb,
		latency:      opts.Latency,
		screens:      make(map[string]*screen),
		edits:        make(map[string]*editLog),
	}
}

// editLogFor returns the sheet's dirty-set history, creating it on first use.
func (s *Server) editLogFor(sheetID string) *editLog {
	s.editsMu.Lock()
	defer s.editsMu.Unlock()
	l := s.edits[sheetID]
	if l == nil {
		l = &editLog{}
		s.edits[sheetID] = l
	}
	return l
}

// Routes returns the mux. Method+pattern routing is stdlib (Go 1.22+); there is
// no router dependency and there does not need to be.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /s/{sheetID}", s.handlePage)
	mux.HandleFunc("GET /s/{sheetID}/live", s.handleLive)
	mux.HandleFunc("POST /s/{sheetID}/cell", s.handleCell)
	mux.HandleFunc("POST /s/{sheetID}/viewport", s.handleViewport)
	mux.HandleFunc("POST /s/{sheetID}/clear", s.handleClear)
	// Where the committed selection reaches the server: the counterpart of
	// `/viewport`, manipulated locally at pointer rate and committed once. Reads
	// that depend on it (the toolbar aggregate) then follow the screen's own
	// selection rather than taking a range per request. See presenceui.go.
	mux.HandleFunc("POST /s/{sheetID}/sel", s.handleSel)
	mux.HandleFunc("POST /s/{sheetID}/fill", s.handleFill)
	mux.HandleFunc("POST /s/{sheetID}/paste", s.handlePaste)
	// Styling is a write that publishes a per-cell dirty set, so it rides the
	// ordinary pushCells path. Its own element issues it, for the cancellation
	// reason `#cl`/`#fl`/`#pv` exist for — see styleui.go.
	mux.HandleFunc("POST /s/{sheetID}/style", s.handleStyle)
	mux.HandleFunc("POST /s/{sheetID}/colwidth", s.handleColWidth)
	mux.HandleFunc("POST /s/{sheetID}/rowheight", s.handleRowHeight)
	mux.HandleFunc("POST /s/{sheetID}/rows", s.handleRows)
	mux.HandleFunc("POST /s/{sheetID}/cols", s.handleCols)
	// The hashed, immutable assets: this page's own script bundle, the vendored
	// Datastar runtime, and Datastar's source map. See assets.go.
	mux.HandleFunc("GET /a/{hash}/{name}", s.handleAsset)
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("POST /sheets", s.handleNewSheet)
	return s.withLatency(mux)
}

// withLatency applies the inbound leg of the artificial delay to every request.
// Commands apply their own outbound leg (respondCommand); a push applies one
// leg only, since it rides a stream that is already open.
func (s *Server) withLatency(h http.Handler) http.Handler {
	if s.latency <= 0 {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(s.latency)
		h.ServeHTTP(w, r)
	})
}

// ─── Screens ──────────────────────────────────────────────────────────────────

func (s *Server) addScreen(scr *screen) {
	s.mu.Lock()
	s.screens[scr.id] = scr
	s.mu.Unlock()
}

func (s *Server) dropScreen(id string) {
	s.mu.Lock()
	delete(s.screens, id)
	s.mu.Unlock()
}

func (s *Server) screen(id string) *screen {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.screens[id]
}

// Screens is the number of live SSE connections this server is holding.
func (s *Server) Screens() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.screens)
}

// ─── Geometry ─────────────────────────────────────────────────────────────────

// bufferRows expands a reported viewport into the buffer the server renders and
// subscribes to: snapped out to band boundaries, then widened by BufferBands on
// each side. Snapping matters because bands are the subscription granularity.
//
// `rows` is the sheet's allocated extent, never a constant, so every caller
// reads it off the sheet immediately before calling. Both ends are clamped: a
// delete can shrink the extent out from under a screen parked below the new
// end, and clamping only viewHi would hand the renderer an inverted window,
// which paints as an empty grid.
func (s *Server) bufferRows(rows, viewLo, viewHi int) (loRow, hiRow int) {
	if rows < 1 {
		rows = 1
	}
	if viewLo < 0 {
		viewLo = 0
	}
	if viewLo > rows-1 {
		viewLo = rows - 1
	}
	if viewHi < viewLo {
		viewHi = viewLo + defaultViewportRows - 1
	}
	if viewHi > rows-1 {
		viewHi = rows - 1
	}
	loBand := BandOf(viewLo) - s.bufferBands
	hiBand := BandOf(viewHi) + s.bufferBands
	if loBand < 0 {
		loBand = 0
	}
	if last := BandOf(rows - 1); hiBand > last {
		hiBand = last
	}
	loRow, _ = BandRows(loBand)
	_, hiRow = BandRows(hiBand)
	// The band snap can overshoot the last row whenever the extent is not a whole
	// number of bands (10,001 rows ends four rows into band 200). Trimming keeps
	// screen.loRow/hiRow — which the mask, the held window and every incremental
	// patch are indexed against — inside the sheet.
	if hiRow > rows-1 {
		hiRow = rows - 1
	}
	return loRow, hiRow
}

// clampBuffer slides a remembered buffer back inside a sheet that has shrunk,
// keeping its height. It is deliberately not bufferRows: that widens a viewport
// into a buffer, and a remembered window is already a buffer, so feeding one
// back through would widen it again on every delete — compounding across a run
// of deletes, and entirely on the low side, so the rows under the reader's eyes
// are the ones that fall out. A buffer still ending inside the sheet is left
// alone, since the structural morph accompanying a delete re-renders it anyway.
func clampBuffer(lo, hi, rows int) (int, int) {
	last := max(rows, 1) - 1
	if hi <= last {
		return lo, hi
	}
	height := hi - lo
	hi = last
	lo = max(0, hi-height)
	lo -= lo % BandHeight // bands are the subscription granularity; stay snapped
	return lo, hi
}

// renderScreen reads the buffer and renders it. This is the hot path: it runs
// once per viewer per push, and deliberately does not go through the actor
// (see actor.go — WAL readers are concurrent with the single writer).
//
// Its output is a pure function of (sheet, window, version), which is the
// precondition for sharing one render across every viewer on the same window
// and for the handover digest. That holds because everything differing between
// viewers is deliberately outside `#g`: the active cell, the interactive
// selection, the copy marquee, the collaborator cursors, and the change flash —
// per-viewer by construction, and drawn as positioned boxes in `#pc` rather
// than as a class on the cells it names. `sel` is always the zero selRange.
func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	sheetID := r.PathValue("sheetID")
	if err := validSheetID(sheetID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !SheetExists(sheetID) {
		http.Error(w, "no such sheet: "+sheetID, http.StatusNotFound)
		return
	}
	ctx, span := tracer.Start(r.Context(), "page.render")
	defer span.End()
	started := time.Now()

	// The anchor is answered before the first byte: `?at=D500` is in the request
	// line, so the buffer below is already the buffer around row 500 and the page
	// comes back correct rather than corrected. A fragment never reaches the
	// server, which is why `at` is a query parameter.
	//
	// A malformed value is not an error: chat clients mangle links, and a shared
	// link that half-survived should open the sheet at the top rather than 400 at
	// someone who has no idea what an A1 reference is.
	rawAt := r.URL.Query().Get("at")
	at, atOK := parseAt(rawAt)
	if rawAt != "" && !atOK {
		obsLog.WarnContext(ctx, "page.bad_at", "sheet", sheetID, "at", rawAt)
	}
	sh, err := OpenSheet(sheetID)
	if err != nil {
		span.RecordError(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The sheet is opened before the window is chosen because the row extent is a
	// fact about the sheet: `?at=C15000` is answerable only against the real
	// extent, and the shell has to carry the same number so the scroll container
	// is the right height in the first byte.
	rows := sh.Rows()
	// The anchor is confined to the extent before anything reads it. Five consumers
	// take the anchor rather than the clamped window — the `at` signal, the two
	// selection seeds, the active-cell box and the toolbar's readout — so
	// `?at=C115000` on a 10,000-row sheet would otherwise render a correct
	// container and a selection 105,000 rows outside it. See anchor.clampTo.
	at = at.clampTo(rows)
	viewLo, viewHi := anchorViewport(at, rows)
	loRow, hiRow := s.bufferRows(rows, viewLo, viewHi)
	// Shared with the stream's renders and with anyone else opening the same
	// link: a posted URL is opened by several people at once far more often than
	// a sheet is edited.
	cells, err := s.readWindow(ctx, sh, loRow, hiRow)
	if err != nil {
		span.RecordError(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Column widths are shared sheet state, so the first paint has to carry them
	// or the columns snap to their real widths once the stream connects. A read
	// failure is not fatal — the defaults are a correct-looking grid.
	widths, werr := sh.ColWidths()
	if werr != nil {
		obsLog.WarnContext(ctx, "page.widths", "sheet", sheetID, "err", werr.Error())
	}

	_, hspan := tracer.Start(ctx, "html.render")
	grid := renderWindow(cells, loRow, hiRow, sheetID, at.Sel)
	hspan.SetAttributes(attribute.Int("bytes", len(grid)), attribute.Int("cells", len(cells)))
	hspan.End()

	// The sheet's stylesheet goes in the first byte: a styled cell carries only
	// its class, so a page whose rules arrived a round trip later would paint every
	// styled cell unstyled and then correct itself.
	// The heights go in the first byte for the same reason the stylesheet does:
	// the CSS places the rows correctly either way, but the client's hit testing
	// is arithmetic, and a page that had to wait a round trip for them would
	// answer a click on a resized sheet with the wrong cell.
	allHeights, hErr := sh.RowHeights(0, rows-1)
	if hErr != nil {
		allHeights = nil
	}
	shell := pageShellWidths(sheetID, loRow, hiRow, grid, at, widths, rows,
		sheetStyleCSS(sh, loRow, hiRow), allHeights)
	span.SetAttributes(
		attribute.String("sheet.id", sheetID),
		attribute.Int("sheet.rows", rows),
		attribute.Int("buffer.lo_row", loRow),
		attribute.Int("buffer.hi_row", hiRow),
		attribute.String("anchor.at", at.Raw),
		attribute.Int("anchor.row", at.Ref.Row),
		attribute.Bool("anchor.selected", at.Sel.On),
		attribute.Int("bytes", len(shell)),
	)
	// no-store, not merely no-cache: this document is a live render whose shell
	// changes on every deploy and which embeds per-connection seeded signals
	// (buffer bounds, the latency p50, the anchor). Heuristic caching would serve
	// a stale shell after a deploy, and a shared cache could hand one reader
	// another reader's seed.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.outboundLeg()
	_, _ = io.WriteString(w, shell)
	obsLog.InfoContext(ctx, "page",
		"sheet", sheetID, "lo_row", loRow, "hi_row", hiRow, "at", at.Raw,
		"rows", rows,
		"grid_bytes", len(grid), "shell_bytes", len(shell),
		"duration_ms", msf(time.Since(started)))
}

// ─── GET /s/{sheetID}/live ────────────────────────────────────────────────────

// liveSignals is what the client sends when it opens the stream: its current
// viewport, if it has one. A cold load sends nothing and the defaults apply.
type liveSignals struct {
	Lo int `json:"lo"`
	Hi int `json:"hi"`
	// At is the linked region, seeded by the page shell from `?at=`. The stream's
	// first buffer has to be the anchored one: this connection re-derives its
	// window from lo/hi, which the shell set to the anchored buffer, and its
	// selection from here — otherwise the first push would replace a correctly
	// anchored first paint with rows 0..N and no highlight.
	At string `json:"at"`
}

// handleLive is the held-open stream, one per screen. It owns every byte of
// the grid this browser sees after first paint.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	sheetID := r.PathValue("sheetID")
	if err := validSheetID(sheetID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var sig liveSignals
	// A cold load has no signals; that is the default viewport, not an error.
	_ = datastar.ReadSignals(r, &sig)
	// The extent decides the buffer, so it is read first. A failure is not fatal
	// to the stream: the default size gives a working (possibly short) grid, and
	// the first push corrects both the window and `--rows` through patchExtent.
	rows := DefaultRows
	var sheet *Sheet
	if sh, serr := OpenSheet(sheetID); serr == nil {
		rows = sh.Rows()
		sheet = sh
	}
	// The handover from the document that opened this stream; see
	// liveHandoverQuery in render.go. A request without it — a reconnect, a curl —
	// falls back to deriving the window from the reported viewport.
	hand := r.URL.Query()
	loRow, hiRow := s.handoverWindow(hand, rows, sig.Lo, sig.Hi)
	// The stylesheet is windowed, so it is built after the window is known and
	// against the same rows the handing-over document rendered.
	var styleCSS string
	if sheet != nil {
		styleCSS = sheetStyleCSS(sheet, loRow, hiRow)
	}
	at, _ := parseAt(sig.At)

	connID := newConnID()
	c := &Conn{
		ID:      connID,
		SheetID: sheetID,
		LoBand:  BandOf(loRow),
		HiBand:  BandOf(hiRow),
	}
	if err := s.reg.Register(c); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The close log exists for the open/close balance: a held-open stream that
	// never returns is otherwise invisible, and "11 opens and 0 closes" — what a
	// bfcached page looks like from here — is indistinguishable from "11 people are
	// reading". It is registered before the teardown defers so it runs after them,
	// and it reports counts rather than the event.
	opened := time.Now()
	liveCtx := r.Context()
	defer func() {
		obsLog.InfoContext(liveCtx, "live.close",
			"conn", connID, "sheet", sheetID,
			"held_ms", msf(time.Since(opened)),
			"screens", s.Screens(),
			"conns", s.reg.Stats().Connections)
	}()
	defer s.reg.Unregister(connID)

	// The stream does not inherit the linked region: `?at=A1:D20` is answered by
	// the first paint (handlePage renders `#sl`) and the shell seeds the client's
	// selection signals from the same anchor, after which the range is client
	// state. A server that kept rendering its own copy would re-assert the URL's
	// range over the reader's on the next full morph. Leaving `sel` zero is what
	// makes the first push remove `#sl` — the client has already put `#sb` in its
	// place.
	scr := &screen{id: connID, sheetID: sheetID, conn: c, loRow: loRow, hiRow: hiRow, obs: newScreenObs()}
	// Priming happens before the first push. Each of these claims the document
	// already contains something, and each is verified rather than trusted — the
	// grid digest against the bytes the render produces, the stylesheet digest
	// against the sheet's current rules.
	primed := 0
	if d := hand.Get("d"); d != "" {
		scr.primeGrid(d)
		primed++
	}
	if sy := hand.Get("sy"); sy != "" && sy == digestHex(styleCSS) {
		scr.primeStyles(styleCSS)
		primed++
	}
	// The two presence elements are in the shell in their empty form, so a solo
	// viewer's overlay is the one the document already has. The chips are not: the
	// document could not name a connection that did not exist when it was written.
	scr.primePresence(presenceChipsSeed(), presenceOverlaySeed())
	s.addScreen(scr)
	defer s.dropScreen(connID)

	var liveSpan trace.Span
	liveCtx, liveSpan = tracer.Start(r.Context(), "sse.live")
	liveSpan.SetAttributes(connAttrs(scr)...)
	defer liveSpan.End()
	obsLog.InfoContext(liveCtx, "live.open", "conn", connID, "sheet", sheetID,
		"lo_row", loRow, "hi_row", hiRow, "at", at.Raw, "primed", primed)

	sse := newSSEFor(w, r, &scr.obs.wire)

	// Hand the client its connection id. Commands name the screen they act on, and
	// the browser has no way to invent it. Datastar ships every non-underscore
	// signal with every request, so one patch here is all the plumbing needed.
	if err := sse.MarshalAndPatchSignals(struct {
		Conn string `json:"conn"`
		Lo   int    `json:"lo"`
		Hi   int    `json:"hi"`
	}{connID, loRow, hiRow}); err != nil {
		return
	}

	// First paint over the stream. Deliver records the digest, so an unchanged
	// sheet suppresses the next wake to zero bytes.
	if err := s.push(liveCtx, sse, scr, "first-paint", time.Time{}, clientTiming{}); err != nil {
		return
	}

	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()
	ctx := r.Context()
	wakes := c.Wakes()
	// Presence has a channel of its own because a band wake means "re-render your
	// buffer", a windowed read of thousands of cells, while somebody else's cursor
	// moving must cost a few hundred bytes of overlay and no database access.
	pwakes := c.PresenceWakes()

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-pwakes:
			if !ok {
				// Unregister closed it, exactly as it closes `wakes`. Without
				// this test a closed channel is always ready and the loop spins.
				return
			}
			// A presence wake never means the grid is stale, so it must not reach
			// s.push: the answer is two page-shell elements, both compared first.
			if err := s.patchPresence(sse, scr); err != nil {
				return
			}
		case <-hb.C:
			if err := sse.MarshalAndPatchSignals(map[string]int64{
				"_hb": time.Now().Unix(),
			}); err != nil {
				return
			}
		case _, ok := <-wakes:
			if !ok {
				// Unregister closed the channel: the connection is being
				// torn down underneath us.
				return
			}
			// Recover the command that caused this wake so the render is a child of it
			// rather than an orphan trace. A viewport command stashes its context on this
			// screen; an edit stashes it per-sheet because it fans out.
			cause, kind, at, client := scr.obs.takeCause()
			if kind == "unknown" {
				cause, at = s.editLogFor(scr.sheetID).Last()
				kind = "edit"
			}
			if err := s.push(cause, sse, scr, kind, at, client); err != nil {
				return
			}
			// A wake carries no information (bus.go), so a scroll and an edit arriving
			// together are serviced as one wake — and a scroll patch only refreshes the
			// rows it added, leaving a dirty cell in a retained row stale. Rather than
			// make one push cover both, notice that this screen is still behind and arm
			// another. Every path advances seenSeq except the scroll patch, so this
			// terminates after one extra pass.
			if scr.seen() < s.editLogFor(scr.sheetID).Seq() {
				s.reg.markDirty(scr.conn)
			}
		}
	}
}

// handoverWindow is the buffer this stream starts on. It prefers the window the
// document says it holds, which is the handover's precondition: `lo`/`hi` are
// seeded by the shell with the page's buffer, and running a buffer back through
// bufferRows would widen it by BufferBands a second time, so the first push
// could never be byte-identical to the document.
//
// The claim is validated rather than trusted — ordered, inside the sheet,
// band-snapped — because everything downstream is indexed against it. Anything
// else falls back to deriving the buffer from the viewport.
func (s *Server) handoverWindow(q url.Values, rows, sigLo, sigHi int) (int, int) {
	lo, lerr := strconv.Atoi(q.Get("bl"))
	hi, herr := strconv.Atoi(q.Get("bh"))
	if lerr == nil && herr == nil &&
		lo >= 0 && hi >= lo && hi <= rows-1 &&
		lo%BandHeight == 0 && (hi+1)%BandHeight == 0 {
		return lo, hi
	}
	// A sheet whose extent is not a whole number of bands ends mid-band, so the
	// last buffer legitimately fails the snap test; accept it when it reaches the
	// end of the sheet exactly.
	if lerr == nil && herr == nil &&
		lo >= 0 && hi == rows-1 && hi >= lo && lo%BandHeight == 0 {
		return lo, hi
	}
	return s.bufferRows(rows, sigLo, sigHi)
}

// push renders the buffer and writes it only if it differs from what this
// connection last received. Digest suppression lives in the registry; the
// render still costs CPU, which is why subscription scoping (a viewer is only
// woken for bands in its own buffer) is the mechanism that actually matters.
func (s *Server) push(parent context.Context, sse *datastar.ServerSentEventGenerator, scr *screen, cause string, since time.Time, client clientTiming) error {
	ctx, span := tracer.Start(parent, "conn.wake")
	defer span.End()
	span.SetAttributes(connAttrs(scr)...)
	span.SetAttributes(attribute.String("wake.cause", cause))
	if !since.IsZero() {
		span.SetAttributes(attribute.Float64("wake.queued_ms", msf(time.Since(since))))
	}

	// The sheet's stylesheet comes first, for the reason the widths come early: it
	// is shared sheet state that none of the patch paths below would mention, and
	// it has to arrive before the cells that reference it or a styled cell paints
	// unstyled for a frame. One ~50-byte element patch, and only when the rules
	// moved.
	if err := s.patchStyles(sse, scr); err != nil {
		span.RecordError(err)
		log.Printf("styles %s [%s]: %v", scr.sheetID, scr.id, err)
	}

	// Who else is here, where they are looking, and what just changed. It rides
	// this frame for the reason the stylesheet and the widths do, and the change
	// flash in particular has to appear on the frame carrying the edit it
	// attributes — the wake came from the edited band, not the presence topic.
	//
	// It costs a comparison and nothing else when nothing moved: both halves are
	// remembered as sent text, and a sheet nobody has edited recently does not
	// reach the attribution ring at all.
	if err := s.patchPresence(sse, scr); err != nil {
		span.RecordError(err)
		log.Printf("presence %s [%s]: %v", scr.sheetID, scr.id, err)
	}

	// The toolbar aggregate is a read model over this screen's own selection,
	// recomputed only when the rectangle moved or when a cell inside it is in the
	// dirty set this push is about to service. The dirty set is peeked rather than
	// consumed; pushCells reads the same log and advances seenSeq itself.
	selNow, _ := s.reg.Selection(scr.id)
	var aggDirty []CellRef
	aggOK := true
	if selNow.Range.On {
		// Only asked for when there is a rectangle: Since() allocates a dedup map
		// whenever a screen is behind, and a screen with no range selected cannot
		// have a stale aggregate whatever changed.
		aggDirty, _, _, aggOK = s.editLogFor(scr.sheetID).Since(scr.seen())
	}
	if err := s.patchAgg(ctx, sse, scr, selNow.Range, aggDirty, aggOK); err != nil {
		span.RecordError(err)
		log.Printf("agg %s [%s]: %v", scr.sheetID, scr.id, err)
	}

	// Column widths are shared sheet geometry and part of no patch path below,
	// and the flag that says they are owed is independent of what woke this
	// connection (see screen.widths), so flushing here means a width change
	// reaches a viewer on whatever frame is going out.
	if scr.takeWidths() {
		if err := s.patchWidths(sse, scr.sheetID); err != nil {
			span.RecordError(err)
			log.Printf("widths %s [%s]: %v", scr.sheetID, scr.id, err)
		}
		span.SetAttributes(attribute.Bool("patch.widths", true))
		if cause == "colwidth" {
			// Nothing else changed — no rows moved, no cell has a new value.
			s.logPush(ctx, scr, cause, since, client, 0, 0, 0, 0, 0, false)
			return nil
		}
	}

	// The row extent is on the same terms: shared sheet geometry, changed by a
	// write past the bottom or a row delete, and mentioned by none of the patch
	// paths below. Restating it here means the scroll container is corrected by
	// whatever frame is going out instead of by a grid re-render whose only purpose
	// would be to carry one integer. It costs ~15 bytes, and only when it changed.
	if grew, err := s.patchExtent(sse, scr); err != nil {
		span.RecordError(err)
		log.Printf("extent %s [%s]: %v", scr.sheetID, scr.id, err)
	} else if grew > 0 {
		span.SetAttributes(attribute.Int("patch.rows", grew))
	}

	// The latency chip's server half, on the same terms as the extent with one
	// extra rule: it is allowed to be stale, so an idle viewer receives nothing.
	// See patchLatency and screen.takeLatency.
	if err := s.patchLatency(sse, scr); err != nil {
		span.RecordError(err)
		log.Printf("latency %s [%s]: %v", scr.sheetID, scr.id, err)
	}

	// A refused command owes its sender an answer: it produced no rows and no
	// cells, so nothing below would send anything and the viewer would be left
	// with a pending chip that never resolved. Two signals, ~80 bytes, to the one
	// screen that asked.
	if note := scr.takeNotice(); note != "" {
		if err := sse.MarshalAndPatchSignals(struct {
			Note string `json:"note"`
			P    bool   `json:"p"`
		}{note, false}); err != nil {
			return err
		}
		span.SetAttributes(attribute.Bool("patch.notice", true))
	}

	// A signals-only wake ends here. "notice" means a command wanted to tell one
	// viewer something and nothing about the grid changed; falling through would
	// spend a whole-window render for the digest to then suppress.
	if cause == "notice" {
		s.logPush(ctx, scr, cause, since, client, 0, 0, 0, 0, 0, false)
		return nil
	}

	// Slow work that was not structural still owes its chip a release. Deferred,
	// so it goes out whether the push patches cells, patches nothing or is
	// suppressed: a chip that outlives its command makes a working page look hung.
	if scr.takeChip() {
		defer func() {
			_ = sse.MarshalAndPatchSignals(map[string]bool{"p": false})
		}()
	}

	// A structural change cancels every incremental path. When rows move, "cell B7
	// changed" is not a fact about the DOM the client holds — its B7 is a different
	// cell from the server's — and the rows a scroll diff would ship are the only
	// rows it would not need. See mutate.go's Dirty.Structural.
	if scr.takeFull() {
		span.SetAttributes(attribute.Bool("patch.structural", true))
		// The pending chip goes out with the frame that carries the new grid, not with
		// the command's response, which returns long before the rows are on screen.
		// Deferred so a suppressed push clears it too.
		defer func() {
			_ = sse.MarshalAndPatchSignals(map[string]bool{"p": false})
		}()
		cause = "structural"
	}

	// A selection wake is signals-only and ends here: committing a rectangle
	// changes no cell and moves no row, and everything it owes this screen has gone
	// out already.
	//
	// It comes after takeFull on purpose. One wake can coalesce a structural change
	// and a selection commit, and a structural change rewrites `cause`, so
	// returning above takeFull would strand that flag until something else woke
	// this screen.
	if cause == "sel" {
		s.logPush(ctx, scr, cause, since, client, 0, 0, 0, 0, 0, false)
		return nil
	}

	// Both causes already know what changed: a scroll knows which rows appeared
	// and disappeared, an edit knows which cells moved. Only a first paint, a
	// disjoint jump, a structural change or a screen that has fallen off the back
	// of the edit log falls through to a full morph.
	switch cause {
	case "viewport":
		if done, err := s.pushWindow(ctx, span, sse, scr, cause, since, client); done {
			return err
		}
	case "edit":
		if done, err := s.pushCells(ctx, span, sse, scr, cause, since, client); done {
			return err
		}
	}
	span.SetAttributes(attribute.String("patch.path", "full"))

	// Read the sequence before the render, not after: an edit committing during the
	// read might not be in the rows that come back, and a screen that claimed to
	// have seen it would never be told again.
	seq := s.editLogFor(scr.sheetID).Seq()

	// Read the selection once and record the same value as held afterwards. A jump
	// landing between the render and the bookkeeping would mark this DOM as
	// holding a region it was never rendered with, and the push that should have
	// corrected it would take the incremental path and skip it.
	sel := scr.selection()

	t0 := time.Now()
	body, mask, err := s.renderScreen(ctx, scr, sel)
	renderDur := time.Since(t0)
	if err != nil {
		span.RecordError(err)
		log.Printf("render %s [%s]: %v", scr.sheetID, scr.id, err)
		return nil // a bad read must not kill the stream
	}
	// Read the window the render was actually against, before anything below
	// can move it. Both the suppressed path and the written path record it.
	lo, hi := scr.window()

	t1 := time.Now()
	send, err := s.reg.DeliverCtx(ctx, scr.id, []byte(body))
	deliverDur := time.Since(t1)
	// The document's own copy counts: if the page that opened this stream handed
	// over the digest of a grid it had already rendered, and these are those bytes,
	// there is nothing to write. Deliver has run first and recorded the digest,
	// which is the right bookkeeping either way — this connection does hold these
	// bytes, having got them in the document rather than over the stream.
	if err == nil && send && scr.takePrimedMatch(body) {
		send = false
		span.SetAttributes(attribute.Bool("patch.primed", true))
	}
	if err != nil || !send {
		span.SetAttributes(attribute.Bool("suppressed", err == nil))
		if err == nil {
			// Identical bytes means the DOM already shows the current database state, so
			// this screen has caught up even though nothing was sent. It also means the
			// browser holds this window with this mask, which has to be recorded or the
			// next edit falls back to a full morph for want of anything to be incremental
			// against — reachable because a primed first paint can be a connection's first
			// push, not just a repeat of one.
			scr.setSeen(seq)
			scr.setHeldWindow(lo, hi, sel, mask)
		}
		s.logPush(ctx, scr, cause, since, client, renderDur, deliverDur, 0, len(body), 0, true)
		return nil
	}

	// One leg of artificial latency: the stream is open, so a push pays only the
	// outbound trip.
	s.outboundLeg()

	_, pspan := tracer.Start(ctx, "sse.patch")
	before := scr.obs.wire.Load()
	t2 := time.Now()
	perr := patch(sse, body)
	patchDur := time.Since(t2)
	wire := scr.obs.wire.Load() - before
	pspan.SetAttributes(
		attribute.Int("bytes_raw", len(body)),
		attribute.Int64("bytes_compressed", int64(wire)),
		attribute.Float64("duration_ms", msf(patchDur)),
	)
	pspan.SetAttributes(connAttrs(scr)...)
	if perr != nil {
		pspan.RecordError(perr)
	}
	pspan.End()

	scr.setHeldWindow(lo, hi, sel, mask)
	scr.setSeen(seq)
	if serr := s.patchBounds(sse, lo, hi); serr != nil && perr == nil {
		perr = serr
	}

	span.SetAttributes(attribute.Bool("suppressed", false))
	s.logPush(ctx, scr, cause, since, client, renderDur, deliverDur, patchDur, len(body), int(wire), false)
	return perr
}

// pushCells services an edit by patching the individual cells the dirty set
// names — inserting, morphing or removing each, depending on what the client
// has (screen.heldMask) and what the value became. The dependency walk already
// knows which cells moved, so spending that on what to send and not only on
// whom to wake turns a 13,000-cell morph into ten cells.
//
// Three things make it cheap rather than merely smaller: cells outside this
// connection's buffer are dropped before anything is read, so a cascade
// reaching band 180 costs a viewer on band 0 not even a query; Datastar's
// `outer` mode resolves each element by its own id, so the payload is literally
// the cells; and digest suppression stays on for two edits producing the same
// bytes. Returns done=false to fall back to a full morph: before first paint,
// while the window is also moving, or when this screen has fallen off the back
// of the edit log.
func (s *Server) pushCells(ctx context.Context, span trace.Span, sse *datastar.ServerSentEventGenerator,
	scr *screen, cause string, since time.Time, client clientTiming,
) (bool, error) {
	heldLo, heldHi, ok := scr.heldWindow()
	if !ok {
		return false, nil
	}
	lo, hi := scr.window()
	if heldLo != lo || heldHi != hi || scr.selStale() {
		// A viewport command has moved the window (or the region) but its patch has
		// not gone out. Patching cells into rows the client does not have yet would
		// address nothing; let the full path resolve both at once.
		span.SetAttributes(attribute.Bool("patch.fallback", true))
		return false, nil
	}
	// The cells carry no selection class — it is one overlay element (selHTML) —
	// so an edit inside a linked region ships the same bytes as one outside it.

	l := s.editLogFor(scr.sheetID)
	seq := l.Seq()
	dirty, _, _, ok := l.Since(scr.seen())
	if !ok {
		// History gone. Losing the log must degrade to "send everything", never
		// to "send nothing".
		span.SetAttributes(attribute.Bool("patch.fallback", true), attribute.Bool("edit.log_gap", true))
		return false, nil
	}

	inBuffer := dirty[:0:0]
	for _, c := range dirty {
		if c.Row >= lo && c.Row <= hi {
			inBuffer = append(inBuffer, c)
		}
	}
	span.SetAttributes(
		attribute.String("patch.path", "cells"),
		attribute.Int("patch.dirty_cells", len(dirty)),
		attribute.Int("patch.cells", len(inBuffer)),
	)
	if len(inBuffer) == 0 {
		// Woken for a band we cover but with nothing in it for us, or already
		// caught up. Zero bytes, zero queries.
		scr.setSeen(seq)
		span.SetAttributes(attribute.Bool("suppressed", true))
		s.logPush(ctx, scr, cause, since, client, 0, 0, 0, 0, 0, true)
		return true, nil
	}

	t0 := time.Now()
	cp, err := s.renderCellPatch(ctx, scr, inBuffer)
	renderDur := time.Since(t0)
	if err != nil {
		span.RecordError(err)
		log.Printf("render cells %s [%s]: %v", scr.sheetID, scr.id, err)
		return true, nil // a bad read must not kill the stream
	}
	span.SetAttributes(
		attribute.Int("patch.morphed", cp.Morphed),
		attribute.Int("patch.inserted", cp.Inserted),
		attribute.Int("patch.removed", cp.Removed),
	)
	raw := len(cp.Morph) + cp.insertBytes() + len(cp.Remove)
	if raw == 0 {
		// Every dirty cell was empty and already absent, so the client has nothing to
		// change. Cheaper than the digest, and it happens on a sparse sheet whenever a
		// cascade clears cells that were never there.
		scr.setSeen(seq)
		span.SetAttributes(attribute.Bool("suppressed", true))
		s.logPush(ctx, scr, cause, since, client, renderDur, 0, 0, 0, 0, true)
		return true, nil
	}

	// The digest covers the morph payload only. Insert and remove name elements by
	// their existence in the client's DOM, so identical bytes on two occasions
	// mean two different DOM operations — the reason ForgetDigest exists.
	t1 := time.Now()
	send := true
	if len(cp.Inserts) == 0 && cp.Remove == "" {
		send, err = s.reg.DeliverCtx(ctx, scr.id, []byte(cp.Morph))
	} else {
		s.reg.DeliverIncremental(scr.id)
	}
	deliverDur := time.Since(t1)
	if err != nil || !send {
		span.SetAttributes(attribute.Bool("suppressed", err == nil))
		if err == nil {
			scr.setSeen(seq)
		}
		s.logPush(ctx, scr, cause, since, client, renderDur, deliverDur, 0, raw, 0, true)
		return true, nil
	}

	s.outboundLeg()

	_, pspan := tracer.Start(ctx, "sse.patch")
	before := scr.obs.wire.Load()
	t2 := time.Now()
	// Remove, then morph, then insert. The three sets are disjoint by construction,
	// so the order is not about correctness but about never letting a mid-frame
	// paint show a duplicate id.
	var perr error
	if cp.Remove != "" {
		perr = patch(sse, "", datastar.WithModeRemove(), datastar.WithSelector(cp.Remove))
	}
	if cp.Morph != "" && perr == nil {
		perr = patch(sse, cp.Morph)
	}
	for _, ins := range cp.Inserts {
		if perr != nil {
			break
		}
		perr = patch(sse, ins.HTML, datastar.WithModeAppend(), datastar.WithSelectorID(ins.Target))
	}
	patchDur := time.Since(t2)
	wire := scr.obs.wire.Load() - before
	pspan.SetAttributes(
		attribute.Int("bytes_raw", raw),
		attribute.Int64("bytes_compressed", int64(wire)),
		attribute.Float64("duration_ms", msf(patchDur)),
		attribute.String("patch.path", "cells"),
	)
	pspan.SetAttributes(connAttrs(scr)...)
	if perr != nil {
		pspan.RecordError(perr)
	}
	pspan.End()

	// The mask follows the DOM, and it is updated here rather than in
	// renderCellPatch so a patch that never went out cannot leave the server
	// believing it did.
	for _, ch := range cp.Changed {
		scr.setHeldCell(ch.Ref, ch.Present)
	}

	scr.setSeen(seq)
	span.SetAttributes(attribute.Bool("suppressed", false))
	s.logPush(ctx, scr, cause, since, client, renderDur, deliverDur, patchDur, raw, int(wire), false)
	return true, perr
}

// cellPatch is one edit's worth of DOM operations, sorted into the three shapes
// sparse rendering requires: a value arriving in a cell that was empty has no
// element to morph, and a value leaving one has to take its element with it.
type cellPatch struct {
	Morph                      string       // cells that exist and still have a value — outer, by id
	Inserts                    []cellInsert // cells that gained a value
	Remove                     string       // selector for cells that lost theirs
	Changed                    []cellPresence
	Morphed, Inserted, Removed int
}

// cellInsert is one append frame: the id to append into, and the elements.
//
// Flat rendering only ever needs one, into `#b`, because DOM order carries no
// information. Row-group rendering needs one per row that already has a
// wrapper, plus one for the rows that do not, because the wrapper is where a
// cell's position comes from.
type cellInsert struct{ Target, HTML string }

func (p cellPatch) insertBytes() int {
	n := 0
	for _, ins := range p.Inserts {
		n += len(ins.HTML)
	}
	return n
}

// cellPresence is a mask update owed once the patch is on the wire.
type cellPresence struct {
	Ref     CellRef
	Present bool
}

// renderCellPatch reads a scattered set of cells and sorts them. The reads are
// coalesced into row runs rather than issued per cell: cells are clustered on
// (row, col), so adjacent rows are one contiguous b-tree scan, while a cascade
// spread across the sheet stays separate scans instead of degenerating into one
// read of everything in between.
func (s *Server) renderCellPatch(ctx context.Context, scr *screen, refs []CellRef) (cellPatch, error) {
	ctx, span := tracer.Start(ctx, "render.cells")
	defer span.End()
	span.SetAttributes(connAttrs(scr)...)

	var cp cellPatch
	sh, err := OpenSheet(scr.sheetID)
	if err != nil {
		span.RecordError(err)
		return cp, err
	}

	rows := make([]int, 0, len(refs))
	for _, r := range refs {
		rows = append(rows, r.Row)
	}
	have := make(map[CellRef]Cell, len(refs))
	read := 0
	for _, run := range rowRuns(rows, cellRunGap, cellRunMax) {
		// Shared: every viewer woken by one edit wants the same rows, so the
		// first to arrive does the read and the rest wait on it rather than
		// queueing another behind the same eight connections.
		got, rerr := s.readWindow(ctx, sh, run.Lo, run.Hi)
		if rerr != nil {
			span.RecordError(rerr)
			return cellPatch{}, rerr
		}
		read += len(got)
		for _, c := range got {
			have[c.Ref] = c
		}
	}

	// Sorted per row, because in row-group mode the row decides whether an insert
	// needs a wrapper and whether a removal is one id or many.
	type rowOp struct {
		morph, insert []Cell
		gone          uint32
	}
	order := make([]int, 0, len(refs))
	ops := make(map[int]*rowOp, len(refs))
	op := func(row int) *rowOp {
		o, ok := ops[row]
		if !ok {
			o = &rowOp{}
			ops[row] = o
			order = append(order, row)
		}
		return o
	}

	var morph []Cell
	for _, r := range refs {
		c, ok := have[r]
		if !ok {
			// The read did not cover it (a ref outside the grid, or a short read).
			// Emitting an empty cell would blank a value that is probably fine;
			// skipping leaves the old value for the next full morph to correct.
			continue
		}
		held := scr.heldCell(r)
		switch {
		// The three piles are sorted by cellRendered, not by emptiness:
		// styling an empty cell is an insert and clearing that style is a
		// remove, which is the same shape a value arriving and leaving has.
		case !cellRendered(c) && held:
			op(r.Row).gone |= 1 << uint(r.Col)
			cp.Changed = append(cp.Changed, cellPresence{r, false})
			cp.Removed++
		case !cellRendered(c):
			// Nothing to draw and nothing drawn. Nothing to say.
		case held:
			op(r.Row).morph = append(op(r.Row).morph, c)
			morph = append(morph, c)
			cp.Morphed++
		default:
			op(r.Row).insert = append(op(r.Row).insert, c)
			cp.Changed = append(cp.Changed, cellPresence{r, true})
			cp.Inserted++
		}
	}

	var remove strings.Builder
	for _, row := range order {
		o := ops[row]
		old := scr.heldRowMask(row)
		var gained uint32
		for _, c := range o.insert {
			gained |= 1 << uint(c.Ref.Col)
		}
		next := (old &^ o.gone) | gained

		if rowGroupMode && next == 0 && old != 0 {
			// The row lost its last value, so the wrapper goes and takes the cells
			// with it — one id instead of one per cleared cell. It has to go: a
			// wrapper left behind with an empty mask would be recreated by the next
			// insert into this row, and two elements would share an id.
			if remove.Len() > 0 {
				remove.WriteByte(',')
			}
			remove.WriteString("#" + groupID(row))
			continue
		}
		cellIDs(&remove, row, o.gone)
		switch {
		case len(o.insert) == 0:
		case rowGroupMode && old != 0:
			cp.Inserts = append(cp.Inserts, cellInsert{groupID(row), renderCells(o.insert)})
		case rowGroupMode:
			cp.Inserts = append(cp.Inserts, cellInsert{bufferID, renderCellGroup(row, o.insert)})
		default:
			cp.Inserts = append(cp.Inserts, cellInsert{bufferID, renderCells(o.insert)})
		}
	}
	// Flat mode's inserts all go to the same place, so they collapse into one
	// frame: position comes from `--r`, so a cell may be appended anywhere.
	if !rowGroupMode && len(cp.Inserts) > 1 {
		var all strings.Builder
		for _, ins := range cp.Inserts {
			all.WriteString(ins.HTML)
		}
		cp.Inserts = []cellInsert{{bufferID, all.String()}}
	}
	cp.Morph = renderCells(morph)
	cp.Remove = remove.String()

	// Same span name as every other render path, so `-analyze` aggregates them.
	body := len(cp.Morph) + cp.insertBytes() + len(cp.Remove)
	_, hspan := tracer.Start(ctx, "html.render")
	hspan.SetAttributes(
		attribute.Int("bytes", body),
		attribute.Int("cells", cp.Morphed+cp.Inserted),
		attribute.Int("cells_read", read),
	)
	hspan.End()

	span.SetAttributes(attribute.Int("bytes", body))
	return cp, nil
}

// slideMask assembles the mask of the new window from the three pieces a scroll
// produces: the rows revealed above, the rows the client kept, and the rows
// revealed below. `kept` is the overlap, read before the held window moved.
func slideMask(kept []uint32, d windowDiff, dl delta, lo, hi int) []uint32 {
	out := make([]uint32, 0, hi-lo+1)
	out = append(out, dl.PreMask...)
	out = append(out, kept...)
	out = append(out, dl.PostMask...)
	// A short read anywhere leaves the tail unknown; pad rather than truncate so
	// index arithmetic against heldLo stays valid for the whole window.
	for len(out) < hi-lo+1 {
		out = append(out, 0)
	}
	return out[:hi-lo+1]
}

// heldSelection is the region the client's DOM was built with.
func (s *Server) patchSelection(sse *datastar.ServerSentEventGenerator, was, now selRange) error {
	switch {
	case !now.On && !was.On:
		return nil
	case !now.On:
		return patch(sse, "", datastar.WithModeRemove(), datastar.WithSelectorID(selID))
	case !was.On:
		return patch(sse, selHTML(now), datastar.WithModeAppend(), datastar.WithSelectorID(bufferID))
	default:
		return patch(sse, selHTML(now))
	}
}

// patchBounds tells the client where its buffer now starts and ends. They are
// signals rather than attributes on `#g` because a screen updated by row
// patches never re-morphs the wrapper, so bounds carried there would go stale.
// They also drive `#b`'s translation — see bufferTransform in render.go.
func (s *Server) patchBounds(sse *datastar.ServerSentEventGenerator, lo, hi int) error {
	return sse.MarshalAndPatchSignals(struct {
		BLo int `json:"blo"`
		BHi int `json:"bhi"`
	}{lo, hi})
}

// pushWindow services a scroll by patching the rows that changed.
//
// It returns done=false when it declines — no first paint yet, or the new
// window shares no rows with the old one — and the caller falls back to a full
// morph. Declining is not a failure mode to be minimised: a disjoint jump has
// nothing to preserve, so "remove 500 rows, then append 500 rows" is strictly
// worse than one morph in bytes, DOM work and paths that can be wrong.
//
// The digest is not consulted here. See Registry.ForgetDigest: identical bytes
// stop meaning "identical DOM" the moment the payload is a delta.
func commandStatus(err error) int {
	if errors.Is(err, ErrBadRef) || errors.Is(err, ErrRecalcTooLarge) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// humanCommandError is the sentence a refused write puts in the pending chip.
// The fan-out refusal gets one of its own because "recalc: affected set too
// large: A1 reaches more than 5000 cells" names an internal ceiling, where what
// the reader needs is what to do instead.
func humanCommandError(prefix string, err error) string {
	if errors.Is(err, ErrRecalcTooLarge) {
		return "That change affects too many cells to recalculate in one go — try a smaller range."
	}
	return prefix + ": " + err.Error()
}

type cellSignals struct {
	Ref string `json:"ref"`
	Raw string `json:"raw"`

	// Conn names the screen that issued the edit, because that screen has already
	// painted the value itself: the local echo in keys.go writes the committed
	// literal straight into the cell so the round trip is not a window showing the
	// old value.
	//
	// That paint is an insert or a remove when the cell was empty or is being
	// cleared, which moves the client's DOM out from under `screen.heldMask`. Left
	// unsaid, the next push would append a second element with the same id, the
	// mask still saying "absent". It rides free: `conn` is an ordinary signal.
	Conn string `json:"conn"`
}

// handleCell is the whole architecture in one handler.
//
//  1. Read the command's signals.
//  2. On the sheet's actor goroutine (writes to one sheet are serialized, which
//     is what buys a total order without OT): recalc against an overlay of the
//     new value, then commit the cell, its edges, every recalculated value and
//     the event in one transaction.
//  3. After the transaction commits, publish the dirty bands.
//  4. Respond with nothing renderable.
//
// Step 3's ordering is load-bearing: publishing before Commit returns lets a
// woken subscriber read the pre-commit snapshot and render the old value, then
// sit on it until something else dirties the same band. The bus carries no
// payload, so there is no second chance to correct it.
//
// Step 4 is the CQRS assertion: the response carries no grid, so the editor's
// own view is updated by the same push as everyone else's and there is exactly
// one rendering path to get right.
func (s *Server) handleCell(w http.ResponseWriter, r *http.Request) {
	sheetID := r.PathValue("sheetID")
	if err := validSheetID(sheetID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var sig cellSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		http.Error(w, "read signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	ref, err := ParseRef(sig.Ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, span := tracer.Start(r.Context(), "cell.command")
	defer span.End()
	span.SetAttributes(
		attribute.String("sheet.id", sheetID),
		attribute.String("cell.ref", ref.String()),
		attribute.Int("cell.band", ref.Band()),
		attribute.Int("raw.bytes", len(sig.Raw)),
	)
	started := time.Now()

	var dirty []CellRef
	var stats RecalcStats
	var recalcDur, writeDur time.Duration
	// A cell write can grow the sheet — `C15000` on a 1,000-row sheet extends it
	// rather than failing — so the extent is measured across every write.
	var grew int
	// Whether the committing screen's cell still has an element afterwards. It is
	// `raw != ""` for any commit that puts something in a cell, and a question
	// only the store can answer for one that empties it, since a cleared cell
	// carrying a background is still a live element. See the mask update below.
	rendered := InferKind(sig.Raw) != KindEmpty
	grew, err = writeSheetRows(sheetID, func(sh *Sheet) error {
		t0 := time.Now()
		_, rspan := tracer.Start(ctx, "recalc")
		res, rerr := s.recalc(sh, ref, sig.Raw)
		recalcDur = time.Since(t0)
		rspan.SetAttributes(
			attribute.Int("nodes_visited", res.Stats.Nodes),
			attribute.Int("dirty_cells", len(res.Dirty)),
			attribute.Int("dirty_bands", len(res.Bands())),
			attribute.Int("depth", res.Stats.Depth),
			attribute.Int("cycled", res.Stats.Cycled),
			attribute.Int("queries", res.Stats.Queries),
			attribute.Int("cells_read", res.Stats.CellsRead),
			attribute.Float64("duration_ms", msf(recalcDur)),
		)
		if rerr != nil {
			rspan.RecordError(rerr)
			rspan.End()
			return rerr
		}
		rspan.End()
		dirty, stats = res.Dirty, res.Stats

		t1 := time.Now()
		werr := sh.WriteCellCtx(ctx, ref, sig.Raw, res.Computed)
		writeDur = time.Since(t1)
		if werr != nil || rendered {
			return werr
		}
		// One extra read, on the clear path only. Delete on a styled cell leaves
		// a row behind (the style has to live somewhere), so the element stays,
		// and a mask saying "absent" would make the next push append a second
		// element with the same id. Reading rather than guessing costs ~10 µs on
		// the rare commit that empties a cell, against a duplicate no later patch
		// can resolve.
		cell, cerr := sh.GetCell(ref)
		if cerr != nil {
			return cerr
		}
		rendered = cellRendered(cell)
		return nil
	})
	if err != nil {
		span.RecordError(err)
		// A refused edit already un-paints itself: the client reverts its echo and
		// flashes the cell red (T.fail, keys.go), so a bad reference needs no words.
		// A cascade refusal does — the value typed was legal and came back anyway,
		// and the only honest answer is which limit it hit.
		if errors.Is(err, ErrRecalcTooLarge) {
			s.notify(sig.Conn, humanCommandError("Edit failed", err))
		}
		obsLog.WarnContext(ctx, "edit.failed",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"ref", ref.String(), "err", err.Error())
		http.Error(w, err.Error(), commandStatus(err))
		return
	}

	// The committing screen has already drawn this cell: its local echo (keys.go)
	// created the element for a value arriving in an empty cell and removed it for
	// a clear, so the mask has to follow the DOM here exactly as it does after a
	// patch goes out, or the next push inserts a duplicate id or removes nothing.
	// It is not conditional on the write having changed anything — an echo happens
	// on any commit, and the mask states presence, not novelty.
	//
	// `InferKind` rather than `raw != ""` because emptiness is the store's rule and
	// this must not become a second, drifting copy of it; and since a styled cell
	// survives being emptied, the clear case asks the store outright.
	//
	// A screen that cannot be found (a stale `conn`, or a commit racing `/live`)
	// records nothing, leaving at worst one duplicated element for the next full
	// render to resolve — better than a cell the client never sees because the
	// server believed it was already there.
	if scr := s.screen(sig.Conn); scr != nil && scr.sheetID == sheetID {
		scr.setHeldCell(ref, rendered)
	}

	// The edited cell is always dirty even if the engine only reported its
	// cascade — a viewer on that band must see the new raw value.
	cells := withRef(dirty, ref)
	bands := BandsFor(cells)

	// Record the dirty set and the trace context before publishing: the wake can
	// land on another goroutine before Publish returns, and since the dirty set is
	// the patch, a render that starts before it is recorded renders nothing.
	editSeq := s.editLogFor(sheetID).Append(ctx, cells)

	// Who changed these cells, for the change flash. It goes next to the publish
	// because it has to be true before the wake it accompanies: the bus carries a
	// subject and nothing else, so "Amber Otter changed B7" has nowhere to travel
	// and is resolved by the woken connection out of a model the write updated
	// first. It deliberately does not invalidate presence — the bands below wake
	// exactly the viewers who can see the flash.
	s.reg.NoteEdit(sheetID, sig.Conn, cells)

	var publishDur time.Duration
	if s.bus != nil {
		_, bspan := tracer.Start(ctx, "bus.publish")
		bspan.SetAttributes(
			attribute.String("sheet.id", sheetID),
			attribute.Int("bands", len(bands)),
			attribute.IntSlice("band.ids", bands),
			attribute.Int("cells", len(cells)),
		)
		t2 := time.Now()
		if err := s.bus.PublishDirty(sheetID, cells); err != nil {
			bspan.RecordError(err)
			log.Printf("publish dirty %s %s: %v", sheetID, ref, err)
		}
		publishDur = time.Since(t2)
		bspan.SetAttributes(attribute.Float64("duration_ms", msf(publishDur)))
		bspan.End()
	}

	// If this edit grew the sheet, every screen on it needs the new height,
	// including the ones nowhere near the changed band that the publish above does
	// not reach. It carries no cause of its own, so the edit's cell patch is not
	// demoted to a full morph.
	s.extentChanged(ctx, sheetID, grew)

	span.SetAttributes(
		attribute.Int("sheet.grew", grew),
		attribute.Int("dirty_cells", len(cells)),
		attribute.Int("dirty_bands", len(bands)),
		attribute.Int("nodes_visited", stats.Nodes),
		attribute.Int64("edit.seq", int64(editSeq)),
	)
	obsLog.InfoContext(ctx, "edit",
		"sheet", sheetID,
		"conn", sig.Conn,
		"name", s.authorName(sig.Conn),
		"ref", ref.String(),
		"seq", editSeq,
		"raw_bytes", len(sig.Raw),
		"nodes_visited", stats.Nodes,
		"depth", stats.Depth,
		"cycled", stats.Cycled,
		"queries", stats.Queries,
		"cells_read", stats.CellsRead,
		"dirty_cells", len(cells),
		"dirty_bands", len(bands),
		"recalc_ms", msf(recalcDur),
		"write_ms", msf(writeDur),
		"publish_ms", msf(publishDur),
		"command_ms", msf(noteCommandNow(started)),
	)

	s.respondCommand(w)
}

// ─── POST /s/{sheetID}/clear ──────────────────────────────────────────────────

// clearSignals is what Delete on a range sends. It arrives as an explicit
// Datastar `payload` rather than the usual sweep of every non-underscore
// signal, which is how the selection stays out of the steady-state signal set.
type clearSignals struct {
	Conn string `json:"conn"`
	Rng  string `json:"rng"`
}

// maxClearCells is the clear's share of the one large-range write cap. It is
// maxWriteCells (rangeops.go), where the number and the argument for it live:
// the clear, the fill, the paste and the per-cell style all take the same limit
// because they are the same batch through the same write path.
const maxClearCells = maxWriteCells

// handleClear empties a rectangle. It is handleCell for N cells at once:
//
//   - One actor turn, one transaction, so no other write can interleave into a
//     half-cleared range and a batch naming one bad ref writes nothing rather
//     than writing the prefix.
//   - Cells that are already empty are skipped, because clearing a blank cell
//     is a write of nothing. The region read that decides this is one scan.
//   - One dirty set, one publish, one edit-log entry. The fan-out cost is the
//     shape of a cascade's, not N times an edit's.
func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	sheetID := r.PathValue("sheetID")
	if err := validSheetID(sheetID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var sig clearSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		http.Error(w, "read signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	lo, hi, err := ParseRangeBounds(sig.Rng)
	if err != nil {
		// A range too big for ParseRangeBounds (Ctrl+A is 260,000 cells) lands here,
		// and the person who pressed Delete is owed the reason. It logs as well as
		// notifies: this is the refusal a whole-sheet selection reaches first, so "it
		// won't let me delete" needs a line to point at.
		s.notify(sig.Conn, clearRefusal)
		obsLog.WarnContext(r.Context(), "clear.bad_range",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"range", sig.Rng, "err", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, span := tracer.Start(r.Context(), "clear.command")
	defer span.End()
	started := time.Now()
	cells := (hi.Row - lo.Row + 1) * (hi.Col - lo.Col + 1)
	span.SetAttributes(
		attribute.String("sheet.id", sheetID),
		attribute.String("range", lo.String()+":"+hi.String()),
		attribute.Int("range.cells", cells),
	)
	if cells > maxClearCells {
		span.SetAttributes(attribute.Bool("refused", true))
		obsLog.WarnContext(ctx, "clear.too_big",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"range", lo.String()+":"+hi.String(), "cells", cells)
		s.notify(sig.Conn, clearRefusal)
		http.Error(w, "range too large", http.StatusBadRequest)
		return
	}

	var dirty []CellRef
	wrote := 0
	var readDur, writeDur time.Duration
	// Clearing cells does not move the allocated extent — only a row delete does —
	// but the measurement is two field reads, so this reports it like every other
	// write rather than assuming.
	var grew int
	grew, err = writeSheetRows(sheetID, func(sh *Sheet) error {
		t0 := time.Now()
		win, rerr := sh.WindowCtx(ctx, lo.Row, hi.Row)
		readDur = time.Since(t0)
		if rerr != nil {
			return rerr
		}
		// The list is built first and written once: this loop only decides what
		// to write — a cell that is already empty is a write of nothing — and
		// applyWrites does the rest in one pass.
		writes := make([]rangeWrite, 0, min(cells, 256))
		for row := lo.Row; row <= hi.Row; row++ {
			for col := lo.Col; col <= hi.Col; col++ {
				i := (row-lo.Row)*MaxCols + col
				if i >= len(win) || win[i].Kind == KindEmpty {
					continue
				}
				writes = append(writes, rangeWrite{Ref: CellRef{Row: row, Col: col}})
			}
		}
		wrote = len(writes)

		t1 := time.Now()
		defer func() { writeDur = time.Since(t1) }()
		var cerr error
		dirty, cerr = s.applyWrites(ctx, sh, writes)
		return cerr
	})
	if err != nil {
		span.RecordError(err)
		s.notify(sig.Conn, humanCommandError("Clear failed", err))
		obsLog.WarnContext(ctx, "clear.failed",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"range", lo.String()+":"+hi.String(), "err", err.Error())
		http.Error(w, err.Error(), commandStatus(err))
		return
	}

	// The chip goes down on the push that carries the emptied cells, not on this
	// response, which returns before anything is on screen. Same rule as the
	// structural commands.
	if scr := s.screen(sig.Conn); scr != nil && scr.sheetID == sheetID {
		scr.markChip()
	}

	editSeq := s.editLogFor(sheetID).Append(ctx, dirty)
	s.reg.NoteEdit(sheetID, sig.Conn, dirty)
	if s.bus != nil && len(dirty) > 0 {
		if perr := s.bus.PublishDirty(sheetID, dirty); perr != nil {
			span.RecordError(perr)
			log.Printf("publish dirty %s %s: %v", sheetID, sig.Rng, perr)
		}
	}
	// Nothing was written and nothing was published, so no push is coming on its
	// own; this wake is what the sender's pending chip has to come down on.
	if len(dirty) == 0 && grew == 0 {
		s.notify(sig.Conn, "")
	}
	s.extentChanged(ctx, sheetID, grew)

	bands := BandsFor(dirty)
	span.SetAttributes(
		attribute.Int("cells.written", wrote),
		attribute.Int("dirty_cells", len(dirty)),
		attribute.Int("dirty_bands", len(bands)),
		attribute.Int64("edit.seq", int64(editSeq)),
		attribute.Float64("duration_ms", msf(time.Since(started))),
	)
	obsLog.InfoContext(ctx, "clear",
		"sheet", sheetID,
		"conn", sig.Conn,
		"name", s.authorName(sig.Conn),
		"range", lo.String()+":"+hi.String(),
		"cells", cells,
		"written", wrote,
		"dirty_cells", len(dirty),
		"dirty_bands", len(bands),
		"read_ms", msf(readDur),
		"write_ms", msf(writeDur),
		"command_ms", msf(noteCommandNow(started)))

	s.respondCommand(w)
}

// clearRefusal is what a too-large range gets told. It names the limit, because
// "it didn't work" is not an answer and the number is the whole story.
var clearRefusal = "Can’t clear more than " + strconv.Itoa(maxClearCells) +
	" cells at once — try a smaller range."

// ─── POST /s/{sheetID}/colwidth ───────────────────────────────────────────────

type colResizeSignals struct {
	Rc int `json:"rc"` // the column being resized
	Rw int `json:"rw"` // its new width in px, as the drag left it

	// Conn names the screen that dragged, and it is here only for the log: it
	// turns "a column got wider" into "who widened it". It rides free, since this
	// command sweeps the ordinary signal set.
	Conn string `json:"conn"`
}

// handleColWidth commits a column resize. The drag itself never gets here: the
// client resizes by writing one signal and only the pointerup posts, so there
// is one command per drag — the rule scrolling follows, applied to the other
// 60Hz interaction.
//
// Column width is shared sheet state, so this fans out to everybody, but
// deliberately not over the bus: the bus's unit is a band, a width change
// affects all 200 of them, and waking every viewer would make each do a full
// windowed read to discover that 26 numbers changed. Marking the screens
// directly costs one flag each and ~260 bytes of signals on their next frame.
func (s *Server) handleColWidth(w http.ResponseWriter, r *http.Request) {
	sheetID := r.PathValue("sheetID")
	if err := validSheetID(sheetID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var sig colResizeSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		http.Error(w, "read signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if sig.Rc < 0 || sig.Rc >= MaxCols {
		http.Error(w, "column out of range", http.StatusBadRequest)
		return
	}

	ctx, span := tracer.Start(r.Context(), "colwidth.command")
	defer span.End()
	started := time.Now()
	px := ClampColWidth(sig.Rw)
	span.SetAttributes(
		attribute.String("sheet.id", sheetID),
		attribute.Int("col", sig.Rc),
		attribute.Int("width.requested", sig.Rw),
		attribute.Int("width.stored", px),
	)

	if err := WriteSheet(sheetID, func(sh *Sheet) error {
		return sh.SetColWidth(sig.Rc, px)
	}); err != nil {
		span.RecordError(err)
		status := http.StatusInternalServerError
		if errors.Is(err, ErrBadRef) || errors.Is(err, ErrBadWidth) {
			status = http.StatusBadRequest
		}
		obsLog.WarnContext(ctx, "colwidth.failed",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"col", sig.Rc, "width", px, "err", err.Error())
		http.Error(w, err.Error(), status)
		return
	}

	woke := s.markSheetWidths(ctx, sheetID)
	span.SetAttributes(attribute.Int("screens.woken", woke))
	obsLog.InfoContext(ctx, "colwidth",
		"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
		"col", sig.Rc, "width", px, "screens", woke,
		"command_ms", msf(noteCommandNow(started)))
	s.respondCommand(w)
}

// ─── POST /s/{sheetID}/rowheight ──────────────────────────────────────────────

// rowResizeSignals is what a row drag commits.
type rowResizeSignals struct {
	Rr int `json:"rr"` // the row being resized
	Rh int `json:"rh"` // its new height in px, as the drag left it

	Conn string `json:"conn"`
}

// handleRowHeight is the row's answer to handleColWidth.
//
// A resize needs no grid render. Every row's top and height is expressed in the
// per-window stylesheet, which the push re-derives and compares, so what reaches
// a viewer is one `<style>` patch of a few dozen bytes rather than a window of
// markup. That is the reason the geometry lives in CSS rather than in the cells'
// own style attributes, where the same two numbers would be repeated across
// every cell of the row.
func (s *Server) handleRowHeight(w http.ResponseWriter, r *http.Request) {
	sheetID := r.PathValue("sheetID")
	if err := validSheetID(sheetID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var sig rowResizeSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		http.Error(w, "read signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if sig.Rr < 0 || sig.Rr >= RowCeiling {
		http.Error(w, "row out of range", http.StatusBadRequest)
		return
	}

	ctx, span := tracer.Start(r.Context(), "rowheight.command")
	defer span.End()
	started := time.Now()
	px := ClampRowHeight(sig.Rh)
	span.SetAttributes(
		attribute.String("sheet.id", sheetID),
		attribute.Int("row", sig.Rr),
		attribute.Int("height.requested", sig.Rh),
		attribute.Int("height.stored", px),
	)

	grew, err := writeSheetRows(sheetID, func(sh *Sheet) error {
		return sh.SetRowHeight([]int{sig.Rr}, px)
	})
	if err != nil {
		span.RecordError(err)
		status := http.StatusInternalServerError
		if errors.Is(err, ErrBadRef) || errors.Is(err, ErrBadHeight) {
			status = http.StatusBadRequest
		}
		obsLog.WarnContext(ctx, "rowheight.failed",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"row", sig.Rr, "height", px, "err", err.Error())
		http.Error(w, err.Error(), status)
		return
	}

	woke := s.markSheetWidths(ctx, sheetID)
	s.extentChanged(ctx, sheetID, grew)
	span.SetAttributes(attribute.Int("screens.woken", woke))
	obsLog.InfoContext(ctx, "rowheight",
		"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
		"row", sig.Rr, "height", px, "screens", woke,
		"command_ms", msf(noteCommandNow(started)))
	s.respondCommand(w)
}

// markSheetWidths flags every screen on a sheet as owing a width frame and wakes
// it. Returns how many screens that was.
func (s *Server) markSheetWidths(ctx context.Context, sheetID string) int {
	s.mu.RLock()
	scrs := make([]*screen, 0, len(s.screens))
	for _, scr := range s.screens {
		if scr.sheetID == sheetID {
			scrs = append(scrs, scr)
		}
	}
	s.mu.RUnlock()
	for _, scr := range scrs {
		scr.markWidths()
		// The cause is set for the trace, but the flag above is what makes the frame
		// happen: another command can overwrite the cause slot before the render, and
		// a width change must not be lost to that race.
		scr.obs.setCause(ctx, "colwidth", clientTiming{})
		s.reg.markDirty(scr.conn)
	}
	return len(scrs)
}

// writeSheetRows is WriteSheet plus the one number the render layer has to be
// told about: how much the sheet's allocated row extent moved.
//
// It is measured inside the actor turn, from the sheet itself, so it cannot
// disagree with what was committed, and it catches growth from any write and
// not only from the structural commands — writing `C15000` on a 1,000-row sheet
// grows it without moving an existing row, which is why Dirty.Stats.Grew is not
// enough on its own. Zero is the common case, costing two field reads.
func writeSheetRows(sheetID string, fn func(*Sheet) error) (grew int, err error) {
	err = WriteSheet(sheetID, func(sh *Sheet) error {
		before := sh.Rows()
		ferr := fn(sh)
		grew = sh.Rows() - before
		return ferr
	})
	return grew, err
}

// extentChanged is the tail every mutation handler runs. It does nothing at all
// unless the write moved the sheet's allocated row extent, which is the case
// for all but a handful of writes in the life of a sheet.
func (s *Server) extentChanged(ctx context.Context, sheetID string, grew int) {
	if grew == 0 {
		return
	}
	sh, err := OpenSheet(sheetID)
	if err != nil {
		obsLog.WarnContext(ctx, "extent.open", "sheet", sheetID, "err", err.Error())
		return
	}
	rows := sh.Rows()
	woke := s.markSheetExtent(ctx, sheetID, rows)
	obsLog.InfoContext(ctx, "extent",
		"sheet", sheetID, "rows", rows, "grew", grew, "screens", woke)
}

// markSheetExtent is what a changed row extent owes every screen on the sheet:
// a wake, so the next frame can carry the new `--rows` (push -> patchExtent),
// and — when the sheet shrank — a window that is still inside it.
//
// Every screen, not just the ones whose bands are dirty: writing to row 15,000
// dirties one band, but a reader parked on row 3 is subscribed to nothing near
// it and their scroll container is now 14,000 rows too short.
//
// It sets no cause, because the mutation that grew the sheet has already
// claimed the one cause slot and overwriting it would demote a 55-byte cell
// patch to a full-window morph for the screen that made the edit.
//
// The re-clamp is for shrink: a buffer remembered in row numbers can name rows
// that no longer exist, and a window whose lo is past the end of the sheet
// paints as an empty grid rather than a visible error. The browser separately
// clamps its own scrollTop and fires a scroll event, which resolves to the same
// window, so the follow-up push is suppressed by the digest.
func (s *Server) markSheetExtent(ctx context.Context, sheetID string, rows int) int {
	s.mu.RLock()
	scrs := make([]*screen, 0, len(s.screens))
	for _, scr := range s.screens {
		if scr.sheetID == sheetID {
			scrs = append(scrs, scr)
		}
	}
	s.mu.RUnlock()
	for _, scr := range scrs {
		lo, hi := scr.window()
		if nlo, nhi := clampBuffer(lo, hi, rows); nlo != lo || nhi != hi {
			if _, _, err := s.reg.SetBufferCtx(ctx, scr.id, BandOf(nlo), BandOf(nhi)); err != nil {
				obsLog.WarnContext(ctx, "extent.rebuffer",
					"sheet", sheetID, "conn", scr.id, "err", err.Error())
			}
			scr.setWindow(nlo, nhi)
			// The DOM this screen holds was rendered for rows that are gone. Only a
			// whole-window render repairs that, and the digest has to be forgotten for
			// the same reason a structural change forgets it — see markSheetFull.
			scr.markFull()
			s.reg.ForgetDigest(scr.id)
		}
		s.reg.markDirty(scr.conn)
	}
	return len(scrs)
}

// patchWidths sends the whole width map as signals — all 26 columns, not just
// the one that moved, because resetting a column to the default deletes its row
// (store.go keeps the table sparse), so a patch of only what is stored could
// never tell a client to put a widened column back. 26 numbers is ~260 bytes
// and always right; a delta would be smaller and sometimes wrong.
//
// `rc: -1` rides the same frame, releasing the dragging client's local override
// in the patch that gives it the authoritative width, so the column never
// flashes back to where it started.
type viewportSignals struct {
	Conn string `json:"conn"`
	Lo   int    `json:"lo"`
	Hi   int    `json:"hi"`

	// At is the linked region. It rides every viewport command because a jump
	// (go-to box, or back/forward) is delivered as a viewport command with a new
	// `at` — the page is loaded once and never navigated again.
	At string `json:"at"`

	// The client's own clock, in performance.now() milliseconds; see otel.go for
	// what each measures. Optional — a client that omits them produces zeros.
	TScroll  float64 `json:"tScroll"`
	TPost    float64 `json:"tPost"`
	TQuiet   float64 `json:"tQuiet"`
	TWait    float64 `json:"tWait"`
	TMorph   float64 `json:"tMorph"`
	TRender  float64 `json:"tRender"`
	TBytes   float64 `json:"tBytes"`
	TScrolls float64 `json:"tScrolls"`
	TSkips   float64 `json:"tSkips"`
}

// timing turns the raw signals into the differences that mean something. The
// two absolute stamps are only ever subtracted from each other, so the client's
// clock never has to agree with the server's.
func (v viewportSignals) timing() clientTiming {
	t := clientTiming{
		ScrollAt:  v.TScroll,
		HandlerAt: v.TPost,
		QuietMs:   v.TQuiet,
		WaitMs:    v.TWait,
		MorphMs:   v.TMorph,
		RenderMs:  v.TRender,
		Bytes:     v.TBytes,
		Scrolls:   v.TScrolls,
		Skips:     v.TSkips,
	}
	if v.TScroll > 0 && v.TPost >= v.TScroll {
		t.DebounceMs = v.TPost - v.TScroll
	}
	return t
}

// handleViewport moves a screen's buffer. The client sends this only when the
// visible rows come within a band of a buffer edge, so ordinary scrolling
// issues nothing at all. Registry.SetBuffer diffs the band sets, so a one-band
// scroll is one subscribe and one unsubscribe however wide the buffer is.
func (s *Server) handleViewport(w http.ResponseWriter, r *http.Request) {
	sheetID := r.PathValue("sheetID")
	if err := validSheetID(sheetID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var sig viewportSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		http.Error(w, "read signals: "+err.Error(), http.StatusBadRequest)
		return
	}

	ctx, span := tracer.Start(r.Context(), "viewport.command")
	defer span.End()
	started := time.Now()
	client := sig.timing()
	span.SetAttributes(
		attribute.String("sheet.id", sheetID),
		attribute.String("conn.id", sig.Conn),
		attribute.Int("viewport.lo_row", sig.Lo),
		attribute.Int("viewport.hi_row", sig.Hi),
	)
	span.SetAttributes(client.attrs()...)

	scr := s.screen(sig.Conn)
	if scr == nil || scr.sheetID != sheetID {
		// The stream this command names is gone (reload, navigation, reaped proxy).
		// Nothing to move and nothing to report: the client's next stream will
		// register its own buffer.
		span.SetAttributes(attribute.Bool("conn.missing", true))
		obsLog.WarnContext(ctx, "viewport.orphan", "conn", sig.Conn, "sheet", sheetID)
		s.respondCommand(w)
		return
	}

	// `at` is parsed for the trace only. A jump arrives as a viewport command whose
	// lo/hi already describe the target buffer, and its range is deliberately not
	// applied to the screen: selection is client state (see handleLive), and
	// re-asserting the URL's range would undo an interactive selection whenever the
	// reader scrolled near a buffer edge.
	at, _ := parseAt(sig.At)

	prevLo, prevHi := scr.window()
	// The sheet's extent, read fresh on every viewport command. It answers a scroll
	// to the bottom of a grown sheet, and it is the recovery path after a shrink:
	// the browser clamps scrollTop, that fires a scroll event, and bufferRows
	// re-seats this screen inside the sheet that now exists.
	rows := DefaultRows
	if sh, serr := OpenSheet(sheetID); serr == nil {
		rows = sh.Rows()
	}
	loRow, hiRow := s.bufferRows(rows, sig.Lo, sig.Hi)
	added, dropped, err := s.reg.SetBufferCtx(ctx, sig.Conn, BandOf(loRow), BandOf(hiRow))
	if err != nil {
		span.RecordError(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	scr.setWindow(loRow, hiRow)

	span.SetAttributes(
		attribute.Int("buffer.lo_row", loRow),
		attribute.Int("buffer.hi_row", hiRow),
		attribute.Int("buffer.prev_lo_row", prevLo),
		attribute.Int("buffer.prev_hi_row", prevHi),
		attribute.Int("bands.added", added),
		attribute.Int("bands.dropped", dropped),
		attribute.String("anchor.at", at.Raw),
		attribute.Bool("anchor.selected", at.Sel.On),
	)

	// Hand the render loop this command's trace context and the client's numbers
	// before waking it, or the wake can be serviced before the cause is recorded
	// and the render becomes an orphan trace.
	scr.obs.setCause(ctx, "viewport", client)

	// Wake exactly this connection. Registry.Wake(sheet, band) would also wake
	// every other viewer covering that band; the digest would suppress their
	// renders, but each would still cost a ~6,500-cell read.
	s.reg.markDirty(scr.conn)

	handlerDur := time.Since(started)
	span.SetAttributes(attribute.Float64("duration_ms", msf(handlerDur)))
	obsLog.InfoContext(ctx, "viewport",
		"conn", sig.Conn,
		"sheet", sheetID,
		"view_lo", sig.Lo,
		"view_hi", sig.Hi,
		"lo_row", loRow,
		"hi_row", hiRow,
		"prev_lo_row", prevLo,
		"prev_hi_row", prevHi,
		"bands_added", added,
		"bands_dropped", dropped,
		"command_ms", msf(handlerDur),
		// The client's numbers. debounce_ms is the gap between the first scroll event
		// of the burst and the moment the debounced handler ran — the cost of not
		// asking — and quiet_ms is how much of that was the 120ms timer.
		"client_debounce_ms", client.DebounceMs,
		"client_quiet_ms", client.QuietMs,
		"client_scroll_events", client.Scrolls,
		"client_skipped_posts", client.Skips,
		// The previous patch, as the browser experienced it.
		"client_prev_wait_ms", client.WaitMs,
		"client_prev_morph_ms", client.MorphMs,
		"client_prev_render_ms", client.RenderMs,
		"client_prev_bytes", client.Bytes,
	)

	s.respondCommand(w)
}

// respondCommand ends a command with nothing renderable: no body, no SSE, no
// fragment. See handleCell for why.
func (s *Server) respondCommand(w http.ResponseWriter) {
	s.outboundLeg()
	w.WriteHeader(http.StatusNoContent)
}

// outboundLeg applies the response half of the symmetric artificial delay. With
// withLatency's inbound half, a request/response pair pays two legs and a push
// on an open stream pays one.
func (s *Server) outboundLeg() {
	if s.latency > 0 {
		time.Sleep(s.latency)
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// authorName is the presence display name behind a connection id, for the log.
// A connection id answers "which socket", not "who", and the display name is
// the only handle anybody in a multiplayer session has for anybody else.
//
// An empty answer is normal: a scripted write carries no conn, and a command
// can outlive its screen or race `/live`. Each still gets its line.
//
// Registry.PresenceOf takes and releases r.mu on its own and touches no hub
// lock (presence.go), so the two locks never nest. It is deliberately off the
// push path, which runs an order of magnitude more often than commands do.
func (s *Server) authorName(connID string) string {
	if connID == "" || s.reg == nil {
		return ""
	}
	u, ok := s.reg.PresenceOf(connID)
	if !ok {
		return ""
	}
	return u.Name
}

// withRef appends ref to dirty unless it is already there. Cheap linear scan:
// a dirty set is a handful of cells, and allocating a map for it would cost
// more than the scan.
func withRef(dirty []CellRef, ref CellRef) []CellRef {
	for _, c := range dirty {
		if c == ref {
			return dirty
		}
	}
	return append(dirty, ref)
}

func newConnID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a condition this process can serve through: a
		// colliding connection id would silently cross-wire two browsers' streams.
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b[:])
}
