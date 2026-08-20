package main

import (
	"strings"
	"testing"
)

// THE GUTTER SERVES THREE GESTURES ON OVERLAPPING PIXELS: click a row number to
// select the row, drag its bottom edge to resize, double-click that edge to fit.
// `#rn` is inside `#vp`, so a pointerdown that starts a resize also reaches the
// selection unless the resize claims it — and the way that failure presents is
// not "the resize is dead". The resize works; a row selection is dragged along
// behind it, repainting on every row crossed while the resize shows nothing
// until it commits, so what the user sees is the selection.
//
// Both sides are pinned because both are load-bearing: the resize stops the
// event, and the selection independently declines the same band.
func TestTheGutterArbitratesItsThreeGestures(t *testing.T) {
	// THE GESTURE LIVES ON `#vp`, WHICH IS PAGE SHELL. On the gutter it sat
	// inside `#g`, which every push re-renders — so a live drag was morphed
	// underneath itself, and the element issuing the commit was one a push could
	// replace, which aborts its in-flight request. Sharing `#vp`'s handlers
	// leaves nothing to race and nothing to stop propagating.
	page := pageShellWidths("demo", 0, 3, "", zeroAnchor(), nil, DefaultRows, "", nil)
	for _, want := range []string{"rzDownR(evt)", "rzMoveR(evt.clientY)", "rzEndR(evt.clientY)", "fitR(rw)"} {
		if !strings.Contains(page, want) {
			t.Errorf("the row gesture is not on the page shell: %q is missing", want)
		}
	}
	grid := renderWindow(blankCells(0, 3), 0, 3, "demo", selRange{})
	for _, bad := range []string{"rzDownR", "rzEndR", "fitR(", "/rowheight"} {
		if strings.Contains(grid, bad) {
			t.Errorf("the row gesture is back on markup a push replaces: %q", bad)
		}
	}

	// Resizing is asked first and answers for itself, which is what gives the
	// selection its turn on every other pixel.
	down := vpPointerDownExpr
	if i, j := strings.Index(down, "rzDownR"), strings.Index(down, "selDown"); i < 0 || j < 0 || i > j {
		t.Errorf("the selection is consulted before the resize: %q", down)
	}

	// THE RELEASE IS ON THE WINDOW, and that is the half that made the bug
	// user-visible rather than merely fragile. On `#rn` the drag ended only if
	// the pointer was still over a 56px-wide gutter, so drifting sideways — which
	// is what a hand does over 60px of vertical travel — lost the release. The
	// gesture then stayed live with its anchor frozen at the original press, and
	// the NEXT unrelated click anywhere committed a height computed from that
	// stale anchor: reliably MinRowHeight. Two of those reached production and
	// are in the log as `rowheight height=16`.
	if !strings.Contains(page, `data-on:pointerup__window="`+rowResizeUpExpr("demo")) {
		t.Error("the resize does not end on the window, so a drag that drifts off the gutter never ends")
	}

	// The grip straddles the edge it draws. A band reaching only upwards leaves
	// half the pixels around the visible line inside the row below, where the
	// same press means "select this row" — which is how aiming at a boundary
	// produced a nine-row selection.
	js2 := anchorScript()
	i2 := strings.Index(js2, "T.gripAt=function")
	grip := js2[i2 : i2+strings.Index(js2[i2:], "\nvar ")]
	if !strings.Contains(grip, "q.bottom-GRIP") || !strings.Contains(grip, "q.top+GRIP") {
		t.Errorf("the grip does not straddle the row edge: %s", grip)
	}

	// And the selection's own hit test declines the grip band, which is the row
	// axis's counterpart to hdrAt ignoring the column grips.
	js := selectScript()
	i := strings.Index(js, "T.rowAt=function")
	if i < 0 {
		t.Fatal("T.rowAt is gone")
	}
	body := js[i:]
	if j := strings.Index(body, "\nT."); j >= 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "T.gripAt(e)") {
		t.Errorf("row selection claims the resize grip: %s", body)
	}

	// One definition of where the grip is. Two would be two chances to disagree,
	// and disagreeing is exactly the bug above.
	if n := strings.Count(anchorScript(), "T.gripAt=function"); n != 1 {
		t.Errorf("T.gripAt is defined %d times", n)
	}
}

// RELOAD IS THE KEY SOMEBODY REACHES FOR WHEN A PAGE IS MISBEHAVING. Binding
// fill-right to Cmd+R means that recovery keystroke silently writes to their
// sheet instead — which is how this was reported: "cmd+r seems to paste/repeat".
// The Excel spelling (Ctrl) stays; the Command spelling goes.
func TestFillIsNotOnTheCommandKey(t *testing.T) {
	js := gridKeysScript()
	i := strings.Index(js, "T.key=function")
	if i < 0 {
		t.Fatal("T.key is gone")
	}
	body := js[i : i+strings.Index(js[i:], "T.edit=function")]

	fill := strings.Index(body, "{fill:'r'}")
	if fill < 0 {
		t.Fatal("fill-right is gone")
	}
	// The switch that reaches fill must be guarded on ctrlKey WITHOUT metaKey.
	guard := strings.LastIndex(body[:fill], "switch(k)")
	if guard < 0 {
		t.Fatal("no switch guards the fill actions")
	}
	line := body[strings.LastIndex(body[:guard], "\n")+1 : guard]
	if !strings.Contains(line, "e.ctrlKey") || !strings.Contains(line, "!e.metaKey") {
		t.Errorf("fill is reachable with the Command key held: %q", line)
	}

	// And nothing else may preventDefault on a chord that resolved to no action,
	// which is what actually lets the browser reload.
	if !strings.Contains(vpKeyExpr("demo"), "if(!a)return;evt.preventDefault();") {
		t.Error("the keymap swallows chords it does not handle, so reload never happens")
	}
}

// ENTER MEANS THE SAME THING IN BOTH HALVES OF THE GESTURE: open the cell, then
// commit it. Excel steps past a selected cell instead and reserves F2 for
// opening, which makes Enter mean two unrelated things depending on state.
func TestEnterOpensTheCellAndShiftEnterStillMoves(t *testing.T) {
	js := gridKeysScript()
	i := strings.Index(js, "T.key=function")
	body := js[i : i+strings.Index(js[i:], "T.edit=function")]
	want := "case 'Enter':      return e.shiftKey?T.mv(r-1,c):{edit:1};"
	if !strings.Contains(body, want) {
		t.Errorf("Enter does not open the cell: want %q", want)
	}
}

// A cell can hold a line break, so the editor has to be an element that can
// carry one. An <input> silently drops the character, which would have made
// Shift+Enter look like it did nothing.
func TestTheEditorCanHoldALineBreak(t *testing.T) {
	ed := editorHTML("demo")
	if !strings.HasPrefix(ed, `<textarea id="`+editorID+`"`) {
		t.Errorf("the editor cannot hold a newline: %q", ed)
	}
	if !strings.HasSuffix(ed, "</textarea>") {
		t.Errorf("the editor element is not closed: %q", ed)
	}
	// Soft wrapping off, so the value the server receives has only the breaks
	// the user typed.
	if !strings.Contains(ed, `wrap="off"`) {
		t.Errorf("the editor may invent line breaks of its own: %q", ed)
	}
	// And the keydown handler stands aside for that chord rather than
	// committing on it.
	k := editorKeyExpr("demo")
	if !strings.Contains(k, "if(k==='Enter'&&evt.shiftKey)return;") {
		t.Errorf("Shift+Enter commits instead of breaking the line: %q", k)
	}
	if strings.Index(k, "evt.shiftKey)return;") > strings.Index(k, "$editing=false") {
		t.Error("the commit runs before the line-break check, so the newline is never typed")
	}
}
