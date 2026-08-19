package main

// screen.go — one held-open SSE connection's view of a sheet.
//
// A screen is what the server remembers about a viewer: the rows it is buffered
// on, the digests of what it already holds, and the small pieces of chrome it is
// owed. Almost every method here is a take-or-forget pair, because the question
// they answer is always "does this viewer already have this?" — and the answer
// has to be consumed exactly once, by the push that acts on it.

import (
	"strings"
	"sync"
	"time"
)

const defaultViewportRows = 40

// heartbeatEvery keeps proxies from reaping an idle stream. It patches a
// local-only signal (`_hb`, underscore-prefixed) so the tick is never echoed
// back to the server on the next command and never touches the DOM.
const heartbeatEvery = 15 * time.Second

// screen is one held-open SSE connection's view state, owned by http.go.
//
// The buffer row window is deliberately not read back off the *Conn: the
// registry guards Conn.LoBand/HiBand with its own mutex, and the viewport
// command writes them from a different goroutine than the render loop reads
// them. A copy behind this struct's mutex is cheaper than borrowing the
// registry's lock per render.
type screen struct {
	id      string
	sheetID string
	conn    *Conn

	// obs carries the tracing context of whatever caused the next wake, plus
	// this connection's own wire-byte counter. See otel.go.
	obs *screenObs

	mu           sync.Mutex
	loRow, hiRow int

	// heldLo/heldHi is the window the browser's DOM currently holds, a different
	// fact from the window this screen wants (loRow/hiRow above): a viewport
	// command moves the wanted window synchronously, the held window only once
	// bytes have reached the socket. The difference between the two is the patch,
	// so it has to be state — the wake that services a scroll runs on another
	// goroutine and has no memory of what the command asked for.
	heldLo, heldHi int
	held           bool

	// heldMask is which cells of that window exist in the browser, one 26-bit
	// column mask per held row (heldMask[i] is row heldLo+i).
	//
	// It is the price sparse rendering charges: with only non-empty cells in the
	// DOM, "morph or insert", "remove or nothing" and "which ids does this dropped
	// band contain" have no structural answer. The alternatives are worse — always
	// remove-then-append doubles the frames on the hottest path (a one-cell edit),
	// re-reading the dropped rows reintroduces the read the incremental scroll path
	// exists to avoid, and naming all 26 columns of every dropped row costs 26x the
	// selector on a mostly-empty sheet. It costs 4 bytes per buffer row, and every
	// path that emits or removes a cell must update it.
	heldMask []uint32

	// sel is the linked region this screen renders with, and heldSel is the one
	// its DOM was built with. Both are always zero: selection is client state
	// (four local signals and one box in the page shell, see keys.go), and the
	// server's only involvement is the first paint, which handlePage renders
	// before any screen exists. Nothing writes them — a writer is exactly how a
	// stale URL range would come back.
	sel, heldSel selRange

	// widths is set when this screen owes its viewer a column-width frame. A flag
	// rather than a wake cause: a wake carries no payload (bus.go) and the cause
	// slot holds one value, so a viewport command landing between a width command
	// and the render would overwrite the cause and the widths would never be sent.
	widths bool

	// sentRows is the allocated row extent this screen has been told about — the
	// number its `--rows` custom property holds, and so how tall its scroll
	// container is.
	//
	// A remembered value rather than a flag, which is what makes the extent
	// self-healing: it is compared against the sheet on every push, so a screen
	// whose height is wrong is corrected by the next frame it receives whatever
	// caused that frame, and the wakes mutation handlers send are only about
	// promptness. Zero means "nothing sent yet" and never matches a real extent,
	// so the first push always states it.
	sentRows int

	// sentStyles is the sheet's stylesheet as this screen holds it — the exact
	// text inside its `<style id="sy">`. A remembered value rather than a flag,
	// for the same reason sentRows is, which is what lets a viewer scroll into a
	// region styled while they were looking elsewhere and find the rules already
	// there. The compare is one string against a dozen rules, and free on a sheet
	// nobody has styled.
	sentStyles string
	// styled says sentStyles has been written at least once. "" is a legal
	// stylesheet, so the zero value cannot double as "nothing sent yet".
	styled bool

	// notice is why the command this viewer just sent was refused. Per-screen
	// rather than per-sheet: a refusal is a fact about one person's click.
	notice string

	// full forces the next push down the whole-window path. Set by a structural
	// mutation (insert/delete row or column), where no incremental patch can be
	// right — see push().
	full bool

	// chip is owed a `p:false` — the pending marker raised by a command slow
	// enough to need one, whose result arrives as an ordinary patch. A structural
	// change clears it on the full render it forces; a range clear has no such
	// render to hang it on.
	chip bool

	// sentP50 is the server-side command p50 this screen's `_lp` signal holds,
	// sentP50At is when it was last restated, and sentP50Set distinguishes 0 the
	// value from 0 the absence of one.
	//
	// It is the one remembered value here allowed to be stale: keeping it fresh
	// would mean a per-viewer tick, which would cost the property that an idle
	// viewer costs zero. See screen.takeLatency and latency.go.
	sentP50    float64
	sentP50At  time.Time
	sentP50Set bool

	// seenSeq is the edit-log sequence this screen's DOM already reflects — the
	// read side of heldLo/heldHi's idea, that the client's state is a position in a
	// history and the patch is the difference.
	//
	// It is recorded from before a database read, never after: an edit committing
	// while a window read is in flight may or may not be in the rows that come
	// back, and assuming it was would drop it forever, where replaying it costs one
	// redundant cell.
	seenSeq uint64

	// ─── Presence ────────────────────────────────────────────────────────────
	//
	// Remembered values rather than flags, for the same reason sentRows and
	// sentStyles are. Two are HTML — the avatar strip and the collaborator
	// overlay, exactly as they sit in the browser — and the third is the
	// aggregate line.
	sentChips   string
	sentOverlay string

	// aggText is what this screen's `_ag` signal holds, aggRng is the rectangle
	// it was computed for, and aggSet distinguishes "" the answer from "" the
	// absence of one. aggRng is what makes this cheap: the aggregate is a
	// windowed scan on the hot path, so it is recomputed only when the rectangle
	// moved or when a cell inside it is dirty — see patchAgg.
	aggText string
	aggRng  selRange
	aggSet  bool

	// aggBig says the last committed selection was past ParseRangeBounds's
	// 100,000-cell ceiling. Ctrl/Cmd+A reaches it immediately, so it is a normal
	// answer: the cell is recorded, the range is not, and the toolbar says why.
	aggBig bool

	// flashAge is the animation delay this screen has already been given for each
	// flashed cell. A negative animation delay is relative to the animation's own
	// start and not to a clock, so `--d:-900ms` means "begin 900ms in" and is right
	// exactly once: re-render the overlay for any other reason and the same element
	// is handed a larger delay, which the browser adds to an animation already
	// running, and the fade jumps forward.
	//
	// So the delay is remembered per cell and re-stated rather than recomputed:
	// byte-identical style attributes leave the running animation alone and keep the
	// overlay off the wire (takeOverlay compares text). A cell edited again comes
	// back with a smaller age, which is the one case where the delay is rewritten
	// and the fade restarts.
	flashAge map[CellRef]int

	// primed is the sha256, in hex, of the grid the document already carried —
	// handed over on the `/live` URL by the page that opened this stream.
	//
	// It is one-shot and it is a claim, not a fact: it describes the DOM as the
	// document delivered it, so it is consumed by the first full render, and it
	// can only ever skip a write whose bytes were compared and found identical.
	// A client that lies, a page that raced a mutation or a reconnect holding a
	// stale value all take the ordinary path and receive the whole window.
	primed string
}

// flashDelays maps each live flash to the animation delay this screen already
// holds for it, minting one for a cell it has not seen and forgetting cells that
// stopped flashing. Rounded to 100ms so an unrelated re-render does not rewrite
// a style attribute — and so restart an animation — over an invisible change.
func (s *screen) flashDelays(set map[CellRef]Flash) map[CellRef]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(set) == 0 {
		s.flashAge = nil
		return nil
	}
	if s.flashAge == nil {
		s.flashAge = make(map[CellRef]int, len(set))
	}
	for ref := range s.flashAge {
		if _, ok := set[ref]; !ok {
			delete(s.flashAge, ref)
		}
	}
	out := make(map[CellRef]int, len(set))
	for ref, f := range set {
		ms := int(f.Age.Milliseconds()/100) * 100
		if held, ok := s.flashAge[ref]; !ok || ms < held {
			s.flashAge[ref] = ms
		}
		out[ref] = s.flashAge[ref]
	}
	return out
}

// primeGrid records the digest of the grid the document already contains.
func (s *screen) primeGrid(hexDigest string) {
	s.mu.Lock()
	s.primed = hexDigest
	s.mu.Unlock()
}

// takePrimedMatch consumes the handover and reports whether the payload about
// to be written is byte-for-byte what the document already holds.
func (s *screen) takePrimedMatch(body string) bool {
	s.mu.Lock()
	want := s.primed
	s.primed = ""
	s.mu.Unlock()
	return want != "" && want == digestHex(body)
}

// primeStyles records that the document already carries this stylesheet, so the
// first push does not restate it.
func (s *screen) primeStyles(css string) {
	s.mu.Lock()
	s.sentStyles, s.styled = css, true
	s.mu.Unlock()
}

// primePresence records that the document already carries the empty avatar
// strip, the empty overlay and the empty aggregate line (`_ag:”`). The strip is
// still owed one frame: the document cannot name a connection that did not
// exist when it was written.
func (s *screen) primePresence(chips, overlay string) {
	s.mu.Lock()
	s.sentChips, s.sentOverlay = chips, overlay
	s.aggRng, s.aggText, s.aggSet = selRange{}, "", true
	s.mu.Unlock()
}

// takeChips compares the avatar strip against what this screen holds.
func (s *screen) takeChips(html string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sentChips == html {
		return "", false
	}
	s.sentChips = html
	return html, true
}

// forgetChips puts an unsent strip back on the books. A write that failed must
// leave the screen owing it, or the chips freeze until somebody else joins.
func (s *screen) forgetChips() {
	s.mu.Lock()
	s.sentChips = ""
	s.mu.Unlock()
}

// takeOverlay is takeChips for the collaborator overlay.
func (s *screen) takeOverlay(html string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sentOverlay == html {
		return "", false
	}
	s.sentOverlay = html
	return html, true
}

func (s *screen) forgetOverlay() {
	s.mu.Lock()
	s.sentOverlay = ""
	s.mu.Unlock()
}

// aggStale reports whether the toolbar's aggregate has to be recomputed: the
// rectangle moved, or a cell inside it changed. `ok` is false when this screen
// has fallen off the back of the edit log — "something changed and I cannot say
// what" — which is the degradation the cell patch makes too.
func (s *screen) aggStale(rng selRange, dirty []CellRef, ok bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.aggSet || s.aggRng != rng {
		return true
	}
	if !rng.On {
		// No rectangle, no answer, and no dirty cell can be inside one.
		return false
	}
	if !ok {
		return true
	}
	for _, c := range dirty {
		if rng.contains(c.Row, c.Col) {
			return true
		}
	}
	return false
}

// takeAgg records the rectangle the aggregate was computed for and reports the
// text if it has to be restated. Recording the rectangle even when the text is
// unchanged stops a selection that moved between two equal sums from being
// recomputed on every subsequent push.
func (s *screen) takeAgg(rng selRange, text string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.aggText
	set := s.aggSet
	s.aggRng, s.aggText, s.aggSet = rng, text, true
	if set && was == text {
		return "", false
	}
	return text, true
}

// setAggBig records whether the last committed selection was too large to
// summarise, and invalidates the aggregate when that answer changes. Without
// that, Ctrl/Cmd+A — which commits a bare cell and an oversize flag — would
// leave aggRng unmoved and the refusal would never be stated.
func (s *screen) setAggBig(big bool) {
	s.mu.Lock()
	if s.aggBig != big {
		s.aggSet = false
	}
	s.aggBig = big
	s.mu.Unlock()
}

func (s *screen) aggOversize() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aggBig
}

func (s *screen) seen() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seenSeq
}

// setSeen advances the applied sequence. It never moves backwards: two pushes
// can be in flight for one screen only in the sense that a render started
// before a newer one finished, and the newer state is the one on screen.
func (s *screen) setSeen(seq uint64) {
	s.mu.Lock()
	if seq > s.seenSeq {
		s.seenSeq = seq
	}
	s.mu.Unlock()
}

func (s *screen) window() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loRow, s.hiRow
}

func (s *screen) setWindow(lo, hi int) {
	s.mu.Lock()
	s.loRow, s.hiRow = lo, hi
	s.mu.Unlock()
}

// heldWindow reports what the client is showing. ok is false before the first
// paint has been written, which is the one case an incremental patch cannot
// serve: there is nothing on screen to be incremental against.
func (s *screen) heldWindow() (lo, hi int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heldLo, s.heldHi, s.held
}

// setHeldWindow records what was actually written to the socket: the window,
// the selection it was rendered with, and which cells of it exist. All three
// are parameters rather than fields read back here, because another command may
// already have moved any of them.
func (s *screen) setHeldWindow(lo, hi int, sel selRange, mask []uint32) {
	s.mu.Lock()
	s.heldLo, s.heldHi, s.held, s.heldSel, s.heldMask = lo, hi, true, sel, mask
	s.mu.Unlock()
}

// heldCell reports whether the browser currently has an element for this cell.
// A ref outside the held window answers false, which is the right answer for
// every caller: there is nothing on screen to morph or to remove.
func (s *screen) heldCell(ref CellRef) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := ref.Row - s.heldLo
	if !s.held || i < 0 || i >= len(s.heldMask) || ref.Col < 0 || ref.Col >= MaxCols {
		return false
	}
	return s.heldMask[i]&(1<<uint(ref.Col)) != 0
}

// setHeldCell records that a cell patch created or removed an element. The
// mask has to move with the DOM or the next scroll will name a cell that is not
// there (harmless) or fail to name one that is (a stale value left behind).
func (s *screen) setHeldCell(ref CellRef, present bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := ref.Row - s.heldLo
	if !s.held || i < 0 || i >= len(s.heldMask) || ref.Col < 0 || ref.Col >= MaxCols {
		return
	}
	if present {
		s.heldMask[i] |= 1 << uint(ref.Col)
	} else {
		s.heldMask[i] &^= 1 << uint(ref.Col)
	}
}

// heldRowMask is the column mask of one held row, or 0 for a row the client is
// not holding. In row-group mode a non-zero mask also means "this row has a
// wrapper", which decides whether a cell insert appends into the row or creates
// it.
func (s *screen) heldRowMask(row int) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := row - s.heldLo
	if !s.held || i < 0 || i >= len(s.heldMask) {
		return 0
	}
	return s.heldMask[i]
}

// heldRange copies the masks for the inclusive row range, or nil if any of it
// falls outside the held window.
func (s *screen) heldRange(lo, hi int) []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.held || hi < lo {
		return nil
	}
	a, b := lo-s.heldLo, hi-s.heldLo
	if a < 0 || b >= len(s.heldMask) {
		return nil
	}
	out := make([]uint32, b-a+1)
	copy(out, s.heldMask[a:b+1])
	return out
}

// maskOf builds the column mask of a dense row-major cell rectangle — the shape
// Sheet.Window returns. A short slice yields a short mask, matching the
// degrade-don't-panic contract the renderers keep.
func maskOf(cells []Cell, nRows int) []uint32 {
	if have := len(cells) / MaxCols; have < nRows {
		nRows = have
	}
	if nRows < 0 {
		nRows = 0
	}
	m := make([]uint32, nRows)
	for r := 0; r < nRows; r++ {
		var bits uint32
		row := cells[r*MaxCols : (r+1)*MaxCols]
		for c := 0; c < MaxCols; c++ {
			// cellRendered, not `Kind != KindEmpty`: a styled blank cell has an
			// element, so the mask has to say so or the next scroll drops a
			// yellow cell it cannot name and the next edit inserts a second one.
			if cellRendered(row[c]) {
				bits |= 1 << uint(c)
			}
		}
		m[r] = bits
	}
	return m
}

// maskSelector is the `remove` selector for every cell the mask says exists in
// rows [lo,hi]. maskLo is the row the mask starts at.
//
// This is where sparse rendering costs bytes: flat mode has no row element to
// remove, so dropping a band means naming every cell in it — on a full sheet,
// 1,100 ids and ~32 KB of selector against 1.4 KB for the row numbers.
// Row-group mode restores the O(rows) selector, which is most of why it exists.
func maskSelector(mask []uint32, maskLo, lo, hi int) string {
	var b strings.Builder
	for r := lo; r <= hi; r++ {
		i := r - maskLo
		if i < 0 || i >= len(mask) || mask[i] == 0 {
			continue
		}
		if rowGroupMode {
			if b.Len() > 0 {
				b.WriteByte(',')
			}
			b.WriteString("#" + groupID(r))
			continue
		}
		cellIDs(&b, r, mask[i])
	}
	return b.String()
}

// selection is the linked region to render with.
func (s *screen) selection() selRange {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sel
}

// markWidths records that this screen's viewer has stale column widths.
func (s *screen) markWidths() {
	s.mu.Lock()
	s.widths = true
	s.mu.Unlock()
}

// takeWidths consumes the flag. Consuming rather than reading means two width
// commands between renders cost one frame, which is correct: the frame carries
// the current widths, not a delta.
func (s *screen) takeWidths() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.widths
	s.widths = false
	return was
}

// takeExtent compares the sheet's extent against what this screen was last told
// and reports the new value if it has to be restated.
func (s *screen) takeExtent(rows int) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sentRows == rows {
		return 0, false
	}
	s.sentRows = rows
	return rows, true
}

// takeStyles compares the sheet's stylesheet against what this screen holds and
// reports the new text if it has to be restated.
func (s *screen) takeStyles(css string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.styled && s.sentStyles == css {
		return "", false
	}
	s.sentStyles, s.styled = css, true
	return css, true
}

// forgetStyles puts an unsent stylesheet back on the books: a failed write must
// leave the screen owing it, or a cell keeps a class with no rule behind it.
func (s *screen) forgetStyles() {
	s.mu.Lock()
	s.styled = false
	s.mu.Unlock()
}

// forgetExtent puts an unsent extent back on the books: a failed write must
// leave the screen owing the number, or its scroll container stays the wrong
// height until the sheet happens to change again.
func (s *screen) forgetExtent() {
	s.mu.Lock()
	s.sentRows = 0
	s.mu.Unlock()
}

// ─── The latency chip's staleness rule ────────────────────────────────────────
//
// An idle viewer costs zero here because nothing wakes them, and keeping the
// p50 fresh would mean waking every viewer on an interval. So the figure rides
// pushes that were happening anyway, and is restated only on a material move:
// relative, because the reader holds 0.4 ms up against 180 ms and only a ratio
// is meaningful at both ends of that range; with an absolute floor, because a
// p50 near zero clears any ratio on noise; and with a gap, because the other
// two bound the size of a move and not its frequency.
func (s *screen) takeLatency(p50 float64, now time.Time) (float64, bool) {
	if p50 <= 0 {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sentP50Set {
		s.sentP50, s.sentP50At, s.sentP50Set = p50, now, true
		return p50, true
	}
	if now.Sub(s.sentP50At) < latencyMoveGap {
		return 0, false
	}
	d := p50 - s.sentP50
	if d < 0 {
		d = -d
	}
	if d < latencyMoveFloor || d < latencyMoveRatio*s.sentP50 {
		return 0, false
	}
	s.sentP50, s.sentP50At = p50, now
	return p50, true
}

// forgetLatency puts an unsent p50 back on the books, as forgetExtent does for
// the row extent.
func (s *screen) forgetLatency() {
	s.mu.Lock()
	s.sentP50Set = false
	s.mu.Unlock()
}

// markFull records that this screen's DOM can only be repaired by a whole-window
// render.
func (s *screen) markFull() {
	s.mu.Lock()
	s.full = true
	s.mu.Unlock()
}

func (s *screen) setNotice(msg string) {
	s.mu.Lock()
	s.notice = msg
	s.mu.Unlock()
}

func (s *screen) takeNotice() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := s.notice
	s.notice = ""
	return msg
}

// markChip records a pending chip for slow work that produced no structural
// change, such as a range clear. A flag rather than a cause, for the reason
// `widths` is: whatever the next wake is about, the chip comes down with it.
func (s *screen) markChip() {
	s.mu.Lock()
	s.chip = true
	s.mu.Unlock()
}

func (s *screen) takeChip() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.chip
	s.chip = false
	return was
}

func (s *screen) takeFull() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.full
	s.full = false
	return was
}

// selStale reports that the region has moved since the client's DOM was built,
// which no incremental patch can express: the cells that must lose the class
// are not in any range the diff knows about.
func (s *screen) selStale() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sel != s.heldSel
}

// ServerOptions configures the HTTP layer.
func (s *screen) heldSelection() selRange {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heldSel
}

// patchSelection moves the linked-region overlay. It is three cases rather than
// one because `#sl` only exists while a region is linked: appearing is an
// append, changing is a morph by id, and disappearing is a remove.
