package main

// overlay_test.go — the geometry regression, pinned.
//
// THE BUG THIS FILE EXISTS FOR: the active-cell outline kept its old width and
// its old x after a column resize, so it sat narrower than its cell and one
// column to the left of it. The cause was that `$x`/`$w` were SNAPSHOTS, written
// at click time from the clicked column's offsetLeft/offsetWidth, and nothing
// recomputed them when a width changed. Every locally-owned overlay had the same
// shape: `#ac`, `#ed`, `#sb` and `#cb`. `#pc` did not, because a collaborator's
// cursor was always `--r` plus a `grid-column` and the browser resolved the rest.
//
// The fix was to delete the second implementation rather than to teach it to
// recompute, so what these tests guard is the ABSENCE of pixel geometry in the
// overlays and the PRESENCE of one shared set of column tracks.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// ONE COLUMN GEOMETRY, NOT TWO THAT AGREE TODAY. The cells (`#b`) and the three
// overlay grids are sized by the same `grid-template-columns`, so a width change
// moves the outline in the same layout pass that moves the cell under it.
func TestEveryOverlayIsPlacedByTheGridsOwnTracks(t *testing.T) {
	tracks := gridTracks()
	if !strings.Contains(tracks, colVar(0)) || !strings.Contains(tracks, colVar(MaxCols-1)) {
		t.Fatalf("gridTracks does not name the column width properties: %q", tracks)
	}
	// The buffer, and the one rule that carries all three overlay grids.
	want := []string{
		"#" + bufferID + "{",
		"#" + cellOverlayID + ",#" + boxOverlayID + ",#" + overlayID + "{",
	}
	for _, sel := range want {
		i := strings.Index(gridCSS, sel)
		if i < 0 {
			t.Fatalf("stylesheet has no rule starting %q", sel)
		}
		decl := gridCSS[i:]
		if j := strings.Index(decl, "}"); j >= 0 {
			decl = decl[:j]
		}
		if !strings.Contains(decl, "grid-template-columns:"+tracks) {
			t.Errorf("%s is not sized by the sheet's column tracks: %q", sel, decl)
		}
	}
	// Three declarations and no more: the cells, the row-group wrapper (the
	// SS_ROW_GROUPS rendering shape, measurement only) and the one rule that
	// carries all three overlay grids. A fourth would be a fourth coordinate
	// system waiting to drift out of step with the first.
	if n := strings.Count(gridCSS, "grid-template-columns:"+tracks); n != 3 {
		t.Errorf("the column tracks are declared %d times, want 3 (#b, the row-group wrapper, the overlay rule)", n)
	}
}

// NO OVERLAY MAY CARRY PIXELS. This is the bug, stated as a property: the moment
// an overlay computes a left or a width for itself, it owns a copy of the column
// cascade and that copy can go stale.
func TestNoOverlayComputesItsOwnPixels(t *testing.T) {
	page := pageShell("demo", 0, 3, renderWindow(sampleCells(4), 0, 3, "demo", selRange{}), zeroAnchor())
	for _, bad := range []string{"$x", "$w ", "$x-", "$w-"} {
		if strings.Contains(page, bad) {
			t.Errorf("the page still carries the snapshotted pixel signal %q", bad)
		}
	}
	// The two signals are gone from the steady-state signal set as well, which
	// is what took 13 bytes off every request the page makes.
	for _, bad := range []string{",x:0,", ",w:" + fmt.Sprint(colWidthPx) + ","} {
		if strings.Contains(page, bad) {
			t.Errorf("data-signals still declares %q", bad)
		}
	}
	// HORIZONTALLY the overlays still carry an address, and that is the rule the
	// bug above was about: grid-column names a track and the browser resolves it
	// against grid-template-columns, so a column resize moves the overlay with no
	// copy of the cascade anywhere.
	if !strings.Contains(cellBoxStyle, "gridColumn:") {
		t.Errorf("the cell overlays are not placed by grid-column: %q", cellBoxStyle)
	}
	for _, bad := range []string{"offsetLeft", "offsetWidth", "$x", "$w "} {
		if strings.Contains(cellBoxStyle, bad) {
			t.Errorf("the cell overlays compute their own horizontal pixels (%q): %q", bad, cellBoxStyle)
		}
	}

	// VERTICALLY they cannot, and this is the one asymmetry in the model. Rows
	// are absolutely positioned rather than grid tracks, so there is no
	// grid-row to name and no way to say "row 7" to CSS from a data-style — an
	// expression writes `--r: 7` with a space, which the per-row rules the
	// server emits do not match. So the client computes the top, through the one
	// shared offset function, and NAMES THE HEIGHT SIGNAL so the expression
	// re-runs when a row is resized. Without that dependency it would compute a
	// top once and keep it after the row moved, which is the same staleness one
	// axis over.
	for _, want := range []string{"topOf($row", "hOf($row", "$" + heightSignal} {
		if !strings.Contains(cellBoxStyle, want) {
			t.Errorf("the cell overlays do not derive their top from %q: %q", want, cellBoxStyle)
		}
	}

	// Naming the signal is only half of it. The tables topOf reads are plain
	// state kept in step by a second effect on the same signal, and two effects
	// on one signal have no defined order — so an overlay that only named the
	// height would be free to run first and compute its top from tables that
	// still describe a uniform sheet. Both accessors rebuild from the argument.
	anchor := anchorScript()
	for _, want := range []string{
		"T.topOf=function(r,dep){if(dep!==undefined)T.setHeights(dep);",
		"T.hOf=function(r,dep){if(dep!==undefined)T.setHeights(dep);",
	} {
		if !strings.Contains(anchor, want) {
			t.Errorf("the shared offset function does not rebuild from its height argument: want %q", want)
		}
	}

	// The selection box follows the same split: grid-column across, the shared
	// offset function down, and the height signal named so it stays current.
	js := selectScript()
	i := strings.Index(js, "T.selBox=function")
	if i < 0 {
		t.Fatal("T.selBox is gone")
	}
	body := js[i:]
	if j := strings.Index(body, "\nT."); j >= 0 {
		body = body[:j]
	}
	for _, bad := range []string{"offsetLeft", "offsetWidth"} {
		if strings.Contains(body, bad) {
			t.Errorf("T.selBox reads the DOM for its horizontal extent (%q): %s", bad, body)
		}
	}
	if !strings.Contains(body, "gridColumn") {
		t.Errorf("T.selBox does not return a grid address across: %s", body)
	}
	if !strings.Contains(body, "T.topOf(") {
		t.Errorf("T.selBox does not use the shared offset function: %s", body)
	}
	// The height argument is the subscription AND the data. Naming it is what
	// makes the expression re-run on a resize; applying it is what keeps the
	// answer right when this effect runs before the one that fills the tables.
	if !strings.Contains(js, "T.selBox=function(ar,ac,fr,fc,dep)") {
		t.Error("T.selBox takes no height argument, so a resize will not move the selection")
	}
	if !strings.Contains(body, "T.setHeights(dep)") {
		t.Errorf("T.selBox names the height signal without applying it, so it can read half-built tables: %s", body)
	}
}

// The editor and the outline stay INSIDE `#g`, because `data-ignore-morph` only
// opens a hole when both the live node and the incoming one carry it — which
// means the server has to keep emitting the editor in every full render.
func TestTheCellOverlayStaysInsideTheGrid(t *testing.T) {
	win := renderWindow(sampleCells(4), 0, 3, "demo", selRange{})
	if !strings.Contains(win, `<div id="`+cellOverlayID+`">`) {
		t.Fatal("the in-window overlay grid is not rendered with the window")
	}
	for _, id := range []string{editorID, activeCellID} {
		if !strings.Contains(win, `id="`+id+`"`) {
			t.Errorf("#%s is no longer part of the window render", id)
		}
	}
	// Matched as attributes, not as bare ids. `oc` and `ed` are two characters
	// long and turn up inside ordinary words — `disabled` contains one of them —
	// so a substring search finds whichever expression happens to be emitted
	// first and reports a nesting failure that is not there.
	if i, j := strings.Index(win, `id="`+cellOverlayID+`"`), strings.Index(win, `id="`+editorID+`"`); i < 0 || j < i {
		t.Error("the editor is not inside the overlay grid, so it has no grid area to be placed in")
	}
	// The selection box and the copy marquee stay OUT of it: nothing a push
	// sends may be able to remove them.
	page := pageShell("demo", 0, 3, win, zeroAnchor())
	shell := page[strings.Index(page, `id="`+boxOverlayID+`"`):]
	for _, id := range []string{selBoxID, copyBoxID} {
		if !strings.Contains(shell, `id="`+id+`"`) {
			t.Errorf("#%s is not inside the shell overlay grid", id)
		}
		if strings.Contains(win, `id="`+id+`"`) {
			t.Errorf("#%s is inside #g, where a push could remove it", id)
		}
	}
}

// THE PENDING CHIP CANNOT GET STUCK. Every element that raises `$p` wears the
// backstop class, and the listener that lowers it tests exactly that class —
// so a refusal the server could not explain (a body it could not read, a bad
// sheet id, a rate limit) still takes the chip down.
func TestEveryPendingWriterWearsTheBackstopClass(t *testing.T) {
	page := pageShellWidths("demo", 0, 3, "", zeroAnchor(), nil, DefaultRows, "", nil)
	raises := strings.Count(page, "$p=true")
	marks := strings.Count(page, `class="`+pendingWriteCl+`"`)
	if raises == 0 {
		t.Fatal("nothing in the page raises the pending chip; the scan is broken")
	}
	if marks != raises {
		t.Errorf("%d elements raise $p but %d wear %q — a refusal from the odd one out "+
			"leaves the chip up until the 20-second safety timer", raises, marks, pendingWriteCl)
	}
	if !strings.Contains(page, `data-on:`+failEvent+`__window=`) {
		t.Error("nothing listens for the backstop event")
	}
	js := chipFailScript()
	if !strings.Contains(js, "d.type!=='error'") {
		t.Errorf("the backstop does not key on a failed fetch: %s", js)
	}
	// AND IT DOES NOT GATE ON THE CLASS. It used to, which meant a command
	// posted from an element with no chip could be refused and leave no trace at
	// all — nothing on screen, and nothing in the log either, since a request
	// rejected while reading its signals never reaches the line that logs it. A
	// row resize lived in that gap for four rounds of debugging. The class marks
	// who needs a chip lowered; it does not decide who gets to be heard.
	if strings.Contains(js, pendingWriteCl) {
		t.Errorf("only pending writers can report a refusal, so every other command fails silently: %s", js)
	}
}

// A FAN-OUT REFUSAL IS A RESOURCE LIMIT, NOT BAD INPUT — and it used to reach
// the client as a 500, which says "the server broke" about a request that was
// perfectly well formed.
func TestTheFanOutRefusalIsClassifiedAndExplained(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"a malformed reference", fmt.Errorf("%w: %q", ErrBadRef, "A0"), http.StatusBadRequest},
		{"a cascade past the cap", fmt.Errorf("%w: A1 reaches more than %d cells", ErrRecalcTooLarge, maxRecalcNodes), http.StatusBadRequest},
		{"anything else", errors.New("disk on fire"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := commandStatus(c.err); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	// The two are separate classifications precisely so they can say different
	// things. "bad cell reference" would be a lie about a legal edit.
	fan := humanCommandError("Fill failed", fmt.Errorf("%w: A1 reaches more than %d cells", ErrRecalcTooLarge, maxRecalcNodes))
	if !strings.Contains(fan, "too many cells") || strings.Contains(fan, "Fill failed") {
		t.Errorf("the fan-out refusal reads like plumbing: %q", fan)
	}
	other := humanCommandError("Fill failed", errors.New("no such column"))
	if !strings.HasPrefix(other, "Fill failed: ") {
		t.Errorf("an ordinary refusal lost its context: %q", other)
	}
}
