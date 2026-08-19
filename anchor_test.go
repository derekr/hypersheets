package main

// anchor_test.go — the `at` parameter, the window it produces, and the one
// property that keeps it affordable: an unselected cell must be byte-identical
// to what it was before linkable regions existed.

import (
	"strconv"
	"strings"
	"testing"
)

func TestParseAt(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantOK  bool
		wantRow int
		wantCol int
		wantSel bool
		selLo   CellRef
		selHi   CellRef
		wantRaw string
	}{
		{name: "single cell", in: "D500", wantOK: true, wantRow: 499, wantCol: 3, wantRaw: "D500"},
		{name: "lowercase", in: "d500", wantOK: true, wantRow: 499, wantCol: 3, wantRaw: "D500"},
		{name: "whitespace", in: "  D500 ", wantOK: true, wantRow: 499, wantCol: 3, wantRaw: "D500"},
		{name: "A1", in: "A1", wantOK: true, wantRow: 0, wantCol: 0, wantRaw: "A1"},
		{name: "last row", in: "Z10000", wantOK: true, wantRow: 9999, wantCol: 25, wantRaw: "Z10000"},
		{
			name: "range", in: "A1:D20", wantOK: true, wantRow: 0, wantCol: 0, wantSel: true,
			selLo: CellRef{0, 0}, selHi: CellRef{19, 3}, wantRaw: "A1:D20",
		},
		{
			// Endpoints in either order normalize to the same rectangle, and the
			// anchor is the TOP-LEFT of the normalized form, not whatever the
			// author typed first.
			name: "reversed range", in: "D20:A1", wantOK: true, wantRow: 0, wantCol: 0, wantSel: true,
			selLo: CellRef{0, 0}, selHi: CellRef{19, 3}, wantRaw: "A1:D20",
		},
		{
			name: "degenerate range", in: "C3:C3", wantOK: true, wantRow: 2, wantCol: 2, wantSel: true,
			selLo: CellRef{2, 2}, selHi: CellRef{2, 2}, wantRaw: "C3:C3",
		},
		// Everything below must be treated as ABSENT, never as an error. A
		// shared link is pasted through chat clients that mangle it, and the
		// reader has no idea what an A1 reference is.
		{name: "empty", in: ""},
		{name: "junk", in: "zzz"},
		{name: "no row", in: "D"},
		{name: "multi-letter column", in: "AA1"},
		{name: "row zero", in: "A0"},
		{name: "row past the ceiling", in: "A1000001"},
		{name: "negative", in: "A-5"},
		{name: "half a range", in: "A1:"},
		{name: "range with junk end", in: "A1:zz"},
		{name: "not a ref at all", in: "../../etc/passwd"},
		{name: "html", in: `A1"><script>`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, ok := parseAt(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("parseAt(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if !tc.wantOK {
				// The zero anchor is the top of the sheet with nothing
				// selected — exactly the pre-`at` behaviour.
				if a != zeroAnchor() {
					t.Errorf("parseAt(%q) = %+v, want the zero anchor", tc.in, a)
				}
				return
			}
			if a.Ref.Row != tc.wantRow || a.Ref.Col != tc.wantCol {
				t.Errorf("parseAt(%q) anchor = %v (row %d col %d), want row %d col %d",
					tc.in, a.Ref, a.Ref.Row, a.Ref.Col, tc.wantRow, tc.wantCol)
			}
			if a.Sel.On != tc.wantSel {
				t.Errorf("parseAt(%q) selected = %v, want %v", tc.in, a.Sel.On, tc.wantSel)
			}
			if tc.wantSel && (a.Sel.Lo != tc.selLo || a.Sel.Hi != tc.selHi) {
				t.Errorf("parseAt(%q) range = %v..%v, want %v..%v", tc.in, a.Sel.Lo, a.Sel.Hi, tc.selLo, tc.selHi)
			}
			if a.Raw != tc.wantRaw {
				t.Errorf("parseAt(%q) raw = %q, want %q", tc.in, a.Raw, tc.wantRaw)
			}
		})
	}
}

// TestParseAtRawRoundTrips is what lets the normalized value be handed back to
// the client as a signal and parsed again on the next command without drifting.
func TestParseAtRawRoundTrips(t *testing.T) {
	for _, in := range []string{"D500", "a1", "  z10000 ", "D20:A1", "b2:b2"} {
		first, ok := parseAt(in)
		if !ok {
			t.Fatalf("parseAt(%q) rejected a valid ref", in)
		}
		second, ok := parseAt(first.Raw)
		if !ok {
			t.Fatalf("parseAt(%q) rejected its own normalized form %q", in, first.Raw)
		}
		if first != second {
			t.Errorf("parseAt(%q) = %+v but re-parsing %q gives %+v", in, first, first.Raw, second)
		}
	}
}

func TestSelRangeContains(t *testing.T) {
	sel := selRange{Lo: CellRef{Row: 10, Col: 2}, Hi: CellRef{Row: 12, Col: 4}, On: true}
	in := []CellRef{{10, 2}, {12, 4}, {11, 3}}
	out := []CellRef{{9, 3}, {13, 3}, {11, 1}, {11, 5}}
	for _, c := range in {
		if !sel.contains(c.Row, c.Col) {
			t.Errorf("%v should be inside %v..%v", c, sel.Lo, sel.Hi)
		}
	}
	for _, c := range out {
		if sel.contains(c.Row, c.Col) {
			t.Errorf("%v should be outside %v..%v", c, sel.Lo, sel.Hi)
		}
	}
	// An off selection contains nothing, including the cells its zero-value
	// corners would otherwise cover.
	if (selRange{}).contains(0, 0) {
		t.Error("the zero selRange must contain nothing, not A1")
	}
}

// testExtent is the sheet height these anchor cases are written against. It is
// a LOCAL constant on purpose: the row extent is a property of a sheet now, not
// a compile-time number, so these tests state the height they mean instead of
// tracking whatever DefaultRows happens to be. Held at the old fixed grid's
// 10,000 so the arithmetic below (and the numbers pinned in
// TestAnchorBufferExactRows) still reads as the same case it always did.
const testExtent = 10000

// TestAnchorViewportPutsTheAnchorAtTheTop pins the positioning rule the client
// depends on: scrollTop = row * rowHeight, with no centring correction that the
// browser would then have to agree with.
func TestAnchorViewportPutsTheAnchorAtTheTop(t *testing.T) {
	tests := []struct {
		at         string
		wantLo     int
		wantHiAtMo int // the minimum acceptable viewHi
	}{
		{at: "", wantLo: 0, wantHiAtMo: defaultViewportRows - 1},
		{at: "D500", wantLo: 499, wantHiAtMo: 499 + defaultViewportRows - 1},
		{at: "A1:D20", wantLo: 0, wantHiAtMo: defaultViewportRows - 1},
		// A selection taller than a screen extends the viewport so the whole
		// linked range is inside the rendered window.
		{at: "A100:D250", wantLo: 99, wantHiAtMo: 249},
		// Clamped at the end of the SHEET rather than overrunning it — and the
		// end of the sheet is a value now, not a constant, so it is passed in.
		{at: "A10000", wantLo: 9999, wantHiAtMo: 9999},
	}
	for _, tc := range tests {
		a, _ := parseAt(tc.at)
		lo, hi := anchorViewport(a, testExtent)
		if lo != tc.wantLo {
			t.Errorf("anchorViewport(%q) lo = %d, want %d", tc.at, lo, tc.wantLo)
		}
		if hi < tc.wantHiAtMo {
			t.Errorf("anchorViewport(%q) hi = %d, want at least %d", tc.at, hi, tc.wantHiAtMo)
		}
		if hi > testExtent-1 || lo > hi {
			t.Errorf("anchorViewport(%q) = [%d,%d] is not a valid window", tc.at, lo, hi)
		}
	}

	// A pathologically tall range must NOT turn one shared link into a
	// 100,000-cell first paint. The rest of it is marked as the reader scrolls
	// into it, which the incremental patch already does for free.
	huge, ok := parseAt("A1:Z3846")
	if !ok {
		t.Fatal("A1:Z3846 is a legal range and must parse")
	}
	lo, hi := anchorViewport(huge, testExtent)
	if n := hi - lo + 1; n > anchorViewportMaxRows {
		t.Errorf("a %d-row selection produced a %d-row viewport, want at most %d",
			huge.Sel.Hi.Row-huge.Sel.Lo.Row+1, n, anchorViewportMaxRows)
	}
}

// TestAnchorBufferContainsTheAnchor is the property that makes a link work at
// all: whatever the buffer maths does, the rows the reader was sent to must be
// inside the window the server renders, and the window must still be band
// snapped or the subscription would cover bands it does not draw.
func TestAnchorBufferContainsTheAnchor(t *testing.T) {
	for _, bands := range []int{0, 2, 4} {
		srv := NewServer(ServerOptions{BufferBands: bands})
		for _, at := range []string{"", "A1", "D500", "A1:D20", "Z9999", "A9990:Z10000", "M2500"} {
			a, _ := parseAt(at)
			viewLo, viewHi := anchorViewport(a, testExtent)
			lo, hi := srv.bufferRows(testExtent, viewLo, viewHi)

			if a.Ref.Row < lo || a.Ref.Row > hi {
				t.Errorf("bands=%d at=%q: anchor row %d outside buffer [%d,%d]", bands, at, a.Ref.Row, lo, hi)
			}
			if lo%BandHeight != 0 {
				t.Errorf("bands=%d at=%q: buffer lo %d is not band snapped", bands, at, lo)
			}
			if (hi+1)%BandHeight != 0 && hi != testExtent-1 {
				t.Errorf("bands=%d at=%q: buffer hi %d is not band snapped", bands, at, hi)
			}
			if lo < 0 || hi > testExtent-1 {
				t.Errorf("bands=%d at=%q: buffer [%d,%d] leaves the grid", bands, at, lo, hi)
			}
		}
	}
}

// TestAnchorBufferExactRows pins the concrete numbers for the default
// configuration, so a change to bufferRows or to anchorViewport shows up as a
// diff instead of as a slightly-off scroll position in a browser.
func TestAnchorBufferExactRows(t *testing.T) {
	srv := NewServer(ServerOptions{BufferBands: 4})
	tests := []struct {
		at             string
		wantLo, wantHi int
	}{
		// No anchor: rows 0..39 -> bands 0..0, widened to 0..4 (clamped at 0).
		{at: "", wantLo: 0, wantHi: 249},
		// D500 -> row 499 -> viewport 499..538 -> bands 9..10 -> 5..14.
		{at: "D500", wantLo: 250, wantHi: 749},
		// A1:D20 anchors at A1, so it is the same buffer as no anchor at all.
		{at: "A1:D20", wantLo: 0, wantHi: 249},
	}
	for _, tc := range tests {
		a, _ := parseAt(tc.at)
		viewLo, viewHi := anchorViewport(a, testExtent)
		lo, hi := srv.bufferRows(testExtent, viewLo, viewHi)
		if lo != tc.wantLo || hi != tc.wantHi {
			t.Errorf("at=%q buffer = [%d,%d], want [%d,%d]", tc.at, lo, hi, tc.wantLo, tc.wantHi)
		}
	}
}

// TestSelectionIsOneBoxNotAClassPerCell is the byte-budget constraint, and
// under sparse rendering it is also a correctness constraint. Most of the cells
// in a range have no element, so a per-cell `class="s"` could not express the
// region at all — it would highlight only the cells that happen to hold data.
//
// One positioned box costs less than the class attributes alone used to.
func TestSelectionIsOneBoxNotAClassPerCell(t *testing.T) {
	cells := blankCells(0, 29)
	sel := selRange{Lo: CellRef{Row: 0, Col: 0}, Hi: CellRef{Row: 19, Col: 3}, On: true}

	plain := renderWindow(cells, 0, 29, "demo", selRange{})
	marked := renderWindow(cells, 0, 29, "demo", sel)

	// Exactly one element expresses the whole region.
	if n := strings.Count(marked, `id="`+selID+`"`); n != 1 {
		t.Errorf("%d selection elements, want exactly 1", n)
	}
	if strings.Contains(marked, `class="s"`) {
		t.Error("cells still carry a selection class; the overlay is supposed to replace it")
	}
	// The vertical extent is rows; the horizontal extent is a grid span,
	// because a column is not a constant width and the grid already knows all
	// 26 of them.
	if want := `style="--r:0;--n:20;grid-column:2/6"`; !strings.Contains(marked, want) {
		t.Errorf("selection box geometry wrong; want %q in %q", want, selHTML(sel))
	}
	// The whole feature is cheaper than the 792 bytes 80 `class="s"` cost.
	if over := len(marked) - len(plain); over > 80 {
		t.Errorf("selection cost %d bytes, want under 80", over)
	}
}

// TestCellsAreSelectionIndependent is what the overlay buys everywhere else: a
// cell renders identically whether or not it is inside the linked region, so an
// edit inside a region ships the same bytes as an edit outside one and the
// incremental scroll path needs no notion of a selection at all.
func TestCellsAreSelectionIndependent(t *testing.T) {
	cells := []Cell{
		{Ref: CellRef{Row: 0, Col: 0}, Computed: "1", Display: "1", Kind: KindNumber},
		{Ref: CellRef{Row: 0, Col: 1}, Computed: "hi", Display: "hi", Kind: KindText},
		{Ref: CellRef{Row: 0, Col: 2}, Computed: "2", Display: "2", Kind: KindFormula, Raw: "=A1*2"},
		{Ref: CellRef{Row: 0, Col: 3}, Computed: "#CYCLE!", Display: "#CYCLE!", Kind: KindError, Raw: "=D1"},
	}
	got := renderCells(cells)
	for _, want := range []string{
		`<b id="A1" style="--r:0">1</b>`,
		`<b id="B1" class="t" style="--r:0">hi</b>`,
		`<b id="C1" class="f" style="--r:0" data-r="=A1*2">2</b>`,
		`<b id="D1" class="e" style="--r:0" data-r="=D1">#CYCLE!</b>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// TestSelectionSurvivesAnIncrementalPatch used to be about rows revealed by a
// scroll arriving pre-marked. They no longer need to be: the region is one
// absolutely positioned box covering rows the buffer may not even hold, so a
// range taller than the buffer is drawn correctly for free and a scroll patch
// carries no selection information whatsoever.
func TestSelectionSurvivesAnIncrementalPatch(t *testing.T) {
	sel := selRange{Lo: CellRef{Row: 100, Col: 0}, Hi: CellRef{Row: 400, Col: 1}, On: true}
	box := selHTML(sel)
	if !strings.Contains(box, "--r:100") || !strings.Contains(box, "--n:301") {
		t.Errorf("a 301-row region must be one box spanning all of it, got %q", box)
	}
	// The rows an append patch would carry, well below the first paint. They
	// say nothing about the selection, and do not have to.
	revealed := renderCellRows(blankCells(350, 399), 350, 399)
	if strings.Contains(revealed, "class=") {
		t.Errorf("a scroll patch is carrying selection state: %q", revealed[:80])
	}
}

// TestPageShellCarriesTheAnchor covers the three things that have to agree on
// first paint or the grid visibly jumps when Datastar boots.
func TestPageShellCarriesTheAnchor(t *testing.T) {
	at, ok := parseAt("D500")
	if !ok {
		t.Fatal("D500 must parse")
	}
	const lo, hi = 250, 749
	grid := renderWindow(blankCells(lo, hi), lo, hi, "demo", at.Sel)
	shell := pageShell("demo", lo, hi, grid, at)

	// 1. The rows are at their true absolute positions from the first byte —
	//    there is no offset to get wrong, which is why the anchored load can
	//    simply set scrollTop and be right.
	if !strings.Contains(shell, `id="n`+strconv.Itoa(lo+1)+`" style="--r:`+strconv.Itoa(lo)+`"`) {
		t.Errorf("shell does not position row %d absolutely", lo+1)
	}
	// 2. The server-owned buffer bound the edge test reads.
	if !strings.Contains(shell, `blo:`+strconv.Itoa(lo)) {
		t.Errorf("shell missing blo:%d", lo)
	}
	// 3. The window the live stream will re-derive its buffer from.
	if !strings.Contains(shell, `lo:`+strconv.Itoa(lo)) || !strings.Contains(shell, `hi:`+strconv.Itoa(hi)) {
		t.Errorf("shell missing lo/hi %d/%d", lo, hi)
	}
	// The region, so the stream's own first paint is marked too.
	if !strings.Contains(shell, `at:'D500'`) {
		t.Error("shell missing the at signal")
	}
	// The client-side positioning. THE ROW TRAVELS AS DATA NOW: it used to be
	// interpolated into an inline script as `T.seek(499)`, which made that script
	// different for every deep link and so impossible to cache. The document
	// carries the number and the cached bundle carries the code that reads it
	// (assets.go). Both halves are asserted, because either one alone is a
	// deep link that silently lands at row 0.
	if !strings.Contains(shell, `data-ar="499"`) {
		t.Error("shell does not carry the anchored row as data")
	}
	if !strings.Contains(appJS, "T.anchorRow=+((vp&&vp.dataset.ar)||0)") {
		t.Error("bundle does not read the anchor row from the document")
	}
	if !strings.Contains(appJS, "if(T.anchorRow>0)T.seek(T.anchorRow)") {
		t.Error("bundle does not seek to the anchored row")
	}
	// The go-to box, and the pushState that makes it a navigation.
	if !strings.Contains(shell, `id="go"`) {
		t.Error("shell has no go-to affordance")
	}
	if !strings.Contains(shell, "history.pushState") {
		t.Error("the go-to jump must push a history entry")
	}
	// In the bundle rather than the document, for the reason above.
	if !strings.Contains(appJS, "history.replaceState") {
		t.Error("scroll tracking must use replaceState")
	}
	// Back/forward has to move the view, which means the viewport command.
	if !strings.Contains(shell, "data-on:popstate__window") {
		t.Error("shell does not listen for popstate")
	}
	// AND IT MUST NOT SHARE AN ELEMENT WITH THE STREAM. Datastar keys request
	// cancellation on the element, so a `@post` from `#live` aborts `#live`'s
	// in-flight `@get` — the SSE stream. This was not a theory: the first back
	// press killed the connection and the grid never updated again.
	if strings.Contains(shell, `id="live" hidden data-on:popstate`) {
		t.Error("popstate must not post from the element that holds the SSE stream")
	}
	if !strings.Contains(shell, `id="nav" hidden data-on:popstate__window`) {
		t.Error("popstate needs its own element")
	}
}

// TestPageShellWithoutAnchorDoesNotSeek — the un-anchored page must not touch
// scrollTop at all. Seeking to row 0 would be a no-op that still fires a scroll
// event and arms the settle guard for a scroll that never needed guarding.
func TestPageShellWithoutAnchorDoesNotSeek(t *testing.T) {
	shell := pageShell("demo", 0, 249, "", zeroAnchor())
	// The absence is now spelled as a missing attribute rather than a missing
	// call: the bundle's seek is guarded by `T.anchorRow>0`, and with no
	// `data-ar` the anchor reads 0, so no seek happens.
	if strings.Contains(shell, "data-ar=") {
		t.Error("an unanchored load must not carry an anchor row")
	}
	if strings.Contains(shell, "T.seek(0)") {
		t.Error("an unanchored load must not seek")
	}
	if !strings.Contains(shell, `at:''`) {
		t.Error("an unanchored load must still declare an empty at signal")
	}
}

// ─── The row extent, in the two places the buffer is decided ─────────────────

// TestBufferRowsFollowsTheSheetsExtent is the whole of the render-side fix: the
// window a screen gets is bounded by how tall THIS SHEET is, not by a constant.
// A grown sheet must be reachable and a shrunken one must not hand back rows
// that are gone.
func TestBufferRowsFollowsTheSheetsExtent(t *testing.T) {
	srv := NewServer(ServerOptions{BufferBands: 4})
	tests := []struct {
		name           string
		rows           int
		viewLo, viewHi int
		wantLo, wantHi int
	}{
		// A grown sheet: the old code clamped this to 9,999 and the reader could
		// not reach the row they had linked to.
		{"deep into a grown sheet", 15000, 14999, 15038, 14750, 14999},
		// A sheet SHORTER than the default: nothing may run past its end.
		{"past the end of a small sheet", 1000, 5000, 5040, 750, 999},
		// An extent that is not a whole number of bands stops at the last row
		// rather than at the band boundary above it.
		{"a ragged extent", 10001, 9990, 10030, 9750, 10000},
		// The ordinary case is unchanged.
		{"an ordinary window", 10000, 499, 538, 250, 749},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi := srv.bufferRows(tc.rows, tc.viewLo, tc.viewHi)
			if lo != tc.wantLo || hi != tc.wantHi {
				t.Errorf("bufferRows(%d, %d, %d) = [%d,%d], want [%d,%d]",
					tc.rows, tc.viewLo, tc.viewHi, lo, hi, tc.wantLo, tc.wantHi)
			}
			if hi > tc.rows-1 {
				t.Errorf("buffer hi %d is past the end of a %d-row sheet", hi, tc.rows)
			}
			if lo < 0 || lo > hi {
				t.Errorf("buffer [%d,%d] is not a window", lo, hi)
			}
			if lo%BandHeight != 0 {
				t.Errorf("buffer lo %d is not band snapped", lo)
			}
		})
	}
}

// TestClampBufferKeepsItsHeight is the property that separates clampBuffer from
// bufferRows, and it is not a detail: a screen's remembered window is ALREADY a
// buffer, so putting it back through the viewport-to-buffer widening grows it by
// BufferBands on every delete. Measured in a browser before this test existed —
// twelve deletes took one screen from 250 buffered rows to 5,450, all of the
// growth on the low side, so the rows under the reader's eyes fell out of the
// buffer and the viewport went blank.
func TestClampBufferKeepsItsHeight(t *testing.T) {
	// Still inside the sheet: untouched, byte for byte.
	if lo, hi := clampBuffer(250, 749, 10000); lo != 250 || hi != 749 {
		t.Errorf("a buffer inside the sheet = [%d,%d], want it left alone", lo, hi)
	}
	// Overhanging the end: slides up, still band snapped, and the height it
	// lands on is its own plus at most the snap — never plus a BufferBands
	// widening, which is what the bug was.
	lo, hi := clampBuffer(19750, 19999, 19988)
	if hi != 19987 {
		t.Errorf("clamped hi = %d, want the last row 19987", hi)
	}
	if got := hi - lo + 1; got < 250 || got > 250+BandHeight-1 {
		t.Errorf("clamped buffer spans %d rows, want 250..%d", got, 250+BandHeight-1)
	}
	if lo%BandHeight != 0 {
		t.Errorf("clamped lo %d is not band snapped", lo)
	}
	// A drastic shrink still lands on a real window rather than a negative one.
	if a, b := clampBuffer(19750, 19999, 100); a != 0 || b != 99 {
		t.Errorf("clamped into a 100-row sheet = [%d,%d], want [0,99]", a, b)
	}
	// IDEMPOTENT, which is the whole test. Twelve deletes at one extent must
	// leave the buffer exactly where the first one put it.
	a, b := lo, hi
	for i := 0; i < 12; i++ {
		a, b = clampBuffer(a, b, 19988)
	}
	if a != lo || b != hi {
		t.Errorf("twelve clamps at one extent walked the buffer [%d,%d] -> [%d,%d]", lo, hi, a, b)
	}
}

// A LINK CAN OUTLIVE THE ROWS IT NAMES. The row extent is dynamic, so `?at=`
// past the end of the sheet is not a malformed link — it is a link that was
// correct when it was made. The window was already clamped; the anchor was not,
// and everything the CLIENT is seeded with reads the anchor: the `at` signal,
// `_sar`/`_sfr`, the active-cell box and the toolbar's readout. The visible
// result was a correctly sized container with a selection 105,000 rows outside
// it.
func TestAnchorClampsToTheSheetsExtent(t *testing.T) {
	cases := []struct {
		name    string
		at      string
		rows    int
		wantRef CellRef
		wantSel selRange
		wantRaw string
	}{
		{
			name: "a cell past the end lands on the last row",
			at:   "C115000", rows: 10000,
			wantRef: CellRef{Row: 9999, Col: 2},
			wantRaw: "C10000",
		},
		{
			// The far corner is past the end; the near one is not. Clamping is
			// monotonic, so lo <= hi survives it.
			name: "a range clamps its FAR corner and keeps the near one",
			at:   "A900:D1200", rows: 1000,
			wantRef: CellRef{Row: 899, Col: 0},
			wantSel: selRange{Lo: CellRef{Row: 899, Col: 0}, Hi: CellRef{Row: 999, Col: 3}, On: true},
			wantRaw: "A900:D1000",
		},
		{
			// Both corners past the end. The rectangle survives as a legal
			// one-row selection on the last row rather than as nothing.
			name: "a range entirely past the end collapses onto the last row",
			at:   "A1100:D1200", rows: 1000,
			wantRef: CellRef{Row: 999, Col: 0},
			wantSel: selRange{Lo: CellRef{Row: 999, Col: 0}, Hi: CellRef{Row: 999, Col: 3}, On: true},
			wantRaw: "A1000:D1000",
		},
		{
			name: "an anchor inside the sheet is untouched",
			at:   "D500", rows: 10000,
			wantRef: CellRef{Row: 499, Col: 3},
			wantRaw: "D500",
		},
		{
			name: "a range inside the sheet is untouched",
			at:   "A1:D20", rows: 10000,
			wantRef: CellRef{Row: 0, Col: 0},
			wantSel: selRange{Lo: CellRef{Row: 0, Col: 0}, Hi: CellRef{Row: 19, Col: 3}, On: true},
			wantRaw: "A1:D20",
		},
		{
			name: "a one-row sheet collapses everything onto row 1",
			at:   "B4000", rows: 1,
			wantRef: CellRef{Row: 0, Col: 1},
			wantRaw: "B1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, ok := parseAt(tc.at)
			if !ok {
				t.Fatalf("parseAt(%q) refused it; the clamp is downstream of parsing", tc.at)
			}
			got := a.clampTo(tc.rows)
			if got.Ref != tc.wantRef {
				t.Errorf("ref = %v, want %v", got.Ref, tc.wantRef)
			}
			if got.Sel != tc.wantSel {
				t.Errorf("sel = %+v, want %+v", got.Sel, tc.wantSel)
			}
			if got.Raw != tc.wantRaw {
				t.Errorf("raw = %q, want %q", got.Raw, tc.wantRaw)
			}
			// The seeds are what the client actually starts with, and they are
			// the half that was wrong.
			lo, hi := selSeed(got)
			if lo.Row >= tc.rows || hi.Row >= tc.rows {
				t.Errorf("selection seeded at rows %d..%d on a %d-row sheet",
					lo.Row, hi.Row, tc.rows)
			}
			if lo.Col >= MaxCols || hi.Col >= MaxCols {
				t.Errorf("selection seeded at cols %d..%d, grid is %d wide", lo.Col, hi.Col, MaxCols)
			}
		})
	}
}

// The absence of an anchor stays the absence of an anchor. Clamping the zero
// value would seed a selection on A1 for every unanchored load, which is a
// different page from the one that has nothing selected.
func TestClampingAnAbsentAnchorChangesNothing(t *testing.T) {
	if got := zeroAnchor().clampTo(10); got != zeroAnchor() {
		t.Errorf("clampTo turned the zero anchor into %+v", got)
	}
}

// The whole point of clamping at the extent rather than in each consumer: the
// SHELL is what the browser receives, so this asserts the bytes.
func TestPageShellClampsAnAnchorPastTheEnd(t *testing.T) {
	at, ok := parseAt("C115000")
	if !ok {
		t.Fatal("parseAt refused C115000")
	}
	rows := 10000
	at = at.clampTo(rows)
	viewLo, viewHi := anchorViewport(at, rows)
	page := pageShellWidths("demo", viewLo, viewHi, "", at, nil, rows, "", nil)
	if strings.Contains(page, "115000") {
		t.Error("the shell still mentions row 115000 somewhere; the clamp is not covering every consumer")
	}
	if !strings.Contains(page, `at:'C10000'`) {
		t.Error("the at signal was not clamped")
	}
	if !strings.Contains(page, `_sar:9999`) || !strings.Contains(page, `_sfr:9999`) {
		t.Error("the selection seeds were not clamped")
	}
}

// A range so tall that it is not a range at all. `A9000:D115000` is 424,004
// cells and ParseRangeBounds refuses it before the clamp ever sees it, which is
// the pre-existing "a mangled link opens the sheet" rule doing its job — worth
// pinning, because it is why the clamp above is not the only defence.
func TestARangePastEveryLimitIsNoAnchorAtAll(t *testing.T) {
	a, ok := parseAt("A9000:D115000")
	if ok {
		t.Fatalf("parseAt accepted a 424,004-cell range: %+v", a)
	}
	if got := a.clampTo(10000); got != zeroAnchor() {
		t.Errorf("a refused anchor clamped to %+v, want the zero anchor", got)
	}
}
