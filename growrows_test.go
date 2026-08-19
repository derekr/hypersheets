package main

// growrows_test.go — the foot control's contracts.
//
// The bug it exists for was "I can't scroll past 1001": a sheet grew on WRITE
// and never on SCROLL, so a new sheet was a box with no door. What is pinned
// here is that the door is free (it costs the grid and the cell nothing), that
// it is a COMMAND rather than a read with a side effect, and that it refuses at
// the ceiling with a sentence.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// IT COSTS THE GRID NOTHING. `#g` is what every push re-sends and what the
// measured byte budget is quoted against, so the control has to be shell markup
// — outside the patch region entirely — exactly like `#sb`, `#cb` and `#pc`.
func TestFootControlIsShellMarkupAndNotGridMarkup(t *testing.T) {
	win := renderWindow(sampleCells(4), 0, 3, "demo", selRange{})
	if strings.Contains(win, `id="`+footID+`"`) {
		t.Error("the foot control is inside #g, where every push would re-send it and a morph could remove it")
	}
	page := pageShell("demo", 0, 3, win, zeroAnchor())
	if n := strings.Count(page, `id="`+footID+`"`); n != 1 {
		t.Fatalf("the page has %d foot controls, want exactly 1", n)
	}
	// Inside `#vp` — it has to scroll with the sheet and be positioned against
	// the same container the rows are — but AFTER `#g`, so it is a sibling and
	// not a child.
	vp := strings.Index(page, `<div id="vp"`)
	g := strings.Index(page, `<div id="`+gridID+`"`)
	fo := strings.Index(page, `<div id="`+footID+`"`)
	end := strings.Index(page, `<div id="cl"`) // the first element after `#vp` closes
	if vp < 0 || g < 0 || fo < 0 || end < 0 {
		t.Fatalf("page landmarks missing: vp=%d g=%d fo=%d end=%d", vp, g, fo, end)
	}
	if !(vp < g && g < fo && fo < end) {
		t.Errorf("the foot control is not a later sibling of #g inside #vp: vp=%d g=%d fo=%d end=%d", vp, g, fo, end)
	}
}

// IT FOLLOWS THE EXTENT WITHOUT BEING TOLD. `--rows` is already patched down by
// the server (patchExtent) and already on `#vp`, so seating the bar in the same
// coordinate space means one signal moves the scroll container AND the control.
// A `top` in pixels, or a proximity rule driven by a scroll handler, would be a
// second thing to keep in step with the first.
func TestFootControlIsSeatedByTheRowExtent(t *testing.T) {
	rule := cssRule(t, gridCSS, "#"+footID+"{")
	if !strings.Contains(rule, "top:calc(var(--ch) + var(--rows) * var(--rh))") {
		t.Errorf("the foot control is not seated one pixel past the last row: %q", rule)
	}
	if !strings.Contains(rule, "position:absolute") {
		t.Errorf("the foot control is not positioned in the scroll container's space: %q", rule)
	}
	// It must not be pinned to the window. A bar that floats over the grid at
	// every scroll position covers cells for the 99% of the time nobody wants
	// it, and "scroll to the end and it is there" is the gesture people have.
	if strings.Contains(rule, "position:fixed") || strings.Contains(rule, "position:sticky") {
		t.Errorf("the foot control floats instead of sitting at the foot of the sheet: %q", rule)
	}
}

// THE COUNT NEVER RIDES A REQUEST. Datastar sends every ordinary signal with
// every request, so a count declared as one would be on every scroll command
// the page ever issues. Local signal, explicit payload — and the payload
// REPLACES the signal set in the v1.0.1 bundle, so `conn` has to be named or the
// server cannot tell whose chip to take down.
func TestFootControlSendsAnExplicitPayloadAndNoNewSignal(t *testing.T) {
	if !strings.HasPrefix(growSignal, "_") {
		t.Errorf("the count signal %q is not local, so it rides every request", growSignal)
	}
	html := growRowsHTML("demo")
	for _, want := range []string{
		`@post('/s/demo/rows'`,
		`payload:{conn:$conn`,
		`mop:'` + growRowsOp + `'`,
		`mn:+$` + growSignal,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the foot control's command is missing %q: %s", want, html)
		}
	}
	// The pending chip's backstop marker, EXACTLY — the class attribute is
	// counted verbatim by TestEveryPendingWriterWearsTheBackstopClass, so the
	// bar's appearance has to come from id-scoped rules instead.
	if !strings.Contains(html, `class="`+pendingWriteCl+`"`) {
		t.Errorf("the foot control raises $p without the backstop class: %s", html)
	}
	// One issuer, and it is a shell element nothing else posts from — the
	// `#cl`/`#fl`/`#pv`/`#st` rule, because Datastar keys request cancellation on
	// the element.
	if n := strings.Count(html, "@post("); n != 1 {
		t.Errorf("the foot control issues %d requests from one element, want 1", n)
	}
	// THE KEYBOARD HAS TO REACH IT. vpKeyExpr is on the WINDOW and excuses
	// itself for INPUT/TEXTAREA/SELECT but not for a button, so without this the
	// grid handler answers Enter with preventDefault (move down one row) and the
	// keydown never becomes a click — the control would be mouse-only. Verified
	// in a real browser: focus the button, press Enter, the sheet grows by 1,000
	// and the selection does not move.
	if !strings.Contains(html, `data-on:keydown="evt.stopPropagation()"`) {
		t.Errorf("the foot control lets its keydown reach the window handler, which prevents the click: %s", html)
	}
	// The count is a real number input, which the window handler DOES excuse
	// itself for — so typing a count is a keyboard interaction that already
	// worked and must keep working.
	if !strings.Contains(html, `type="number"`) {
		t.Errorf("the count is not a number input: %s", html)
	}
}

// THE TWO NUMBERS ARE NOT THE SAME NUMBER, and they were briefly indistinguishable
// because DefaultRows happened to be 1,000 too. DefaultRows is how big a sheet
// starts; growRowsDefault is how much a click extends it by. Re-coupling them
// makes the button silently offer whatever the starting size becomes.
func TestTheAddCountIsIndependentOfTheStartingSize(t *testing.T) {
	if growRowsDefault != 1000 {
		t.Errorf("growRowsDefault = %d, want 1000 (what Sheets offers)", growRowsDefault)
	}
	if !strings.Contains(growRowsSignalSeed(), ":1000") {
		t.Errorf("the box is not seeded with 1,000: %q", growRowsSignalSeed())
	}
	if growRowsMax > RowCeiling {
		t.Errorf("one click may ask for %d rows, past the %d-row ceiling", growRowsMax, RowCeiling)
	}
}

// THE STORE ALREADY HAD THIS. `at == extent` is the append position the row
// extent work opened up, so the whole command is one InsertRows and there is no
// new store API to get wrong. What is pinned is that it GROWS (rather than
// reusing a blank tail and staying the same height) and that it moves nothing.
func TestAppendingAtTheExtentGrowsAndMovesNothing(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "foot")
	mustSet(t, sh, "A1", "top")
	mustSet(t, sh, "B2", "=A1")

	before := sh.Rows()
	d, err := sh.InsertRows(before, growRowsDefault)
	if err != nil {
		t.Fatalf("append %d rows at %d: %v", growRowsDefault, before, err)
	}
	if d.Stats.Grew != growRowsDefault {
		t.Errorf("Stats.Grew = %d, want %d", d.Stats.Grew, growRowsDefault)
	}
	if got := sh.Rows(); got != before+growRowsDefault {
		t.Errorf("sheet is %d rows, want %d", got, before+growRowsDefault)
	}
	// Nothing above the append position moved, which is why applyStructural
	// does NOT mark every screen full for this one operation — the extent patch
	// is all any viewer is owed.
	if d.Stats.CellsMoved != 0 {
		t.Errorf("an append moved %d cells; it must move none", d.Stats.CellsMoved)
	}
	wantCell(t, sh, "A1", "top", "top")
	wantCell(t, sh, "B2", "=A1", "top")
	if err := sh.index().validate(); err != nil {
		t.Errorf("band index after an append: %v", err)
	}
}

// THE CEILING IS A SENTENCE, NOT A 500. It is the one refusal this control can
// produce, and the reader who sees it is looking at the bottom of a
// 1,000,000-row sheet — so what they need told is that they are already there.
func TestTheCeilingRefusalReadsLikeAnAnswer(t *testing.T) {
	err := errAtRowCeiling(RowCeiling)
	if !errors.Is(err, ErrRowCeiling) {
		t.Fatalf("the append's own ceiling error is not an ErrRowCeiling: %v", err)
	}
	msg := humanStructError(err, axisRow, false, true)
	if !strings.Contains(msg, "1,000,000 rows tall") {
		t.Errorf("the refusal does not say what happened: %q", msg)
	}
	for _, bad := range []string{"ErrRowCeiling", "%w", "insert", "at 1000000"} {
		if strings.Contains(msg, bad) {
			t.Errorf("the refusal reads like plumbing (%q): %q", bad, msg)
		}
	}
	// The MENU's wording for the same sentinel is a different sentence, because
	// "Can't insert" is right for a menu item and wrong for a button that says
	// "Add". One classifier, two audiences.
	if menu := humanStructError(err, axisRow, false, false); menu == msg {
		t.Errorf("the menu and the foot control share one sentence: %q", msg)
	}
}

// cssRule returns the declaration block that starts with `sel` in the sheet's
// one stylesheet. Kept here rather than shared because overlay_test.go's copy is
// inline and this file must not make that one harder to read.
func cssRule(t *testing.T, css, sel string) string {
	t.Helper()
	i := strings.Index(css, sel)
	if i < 0 {
		t.Fatalf("stylesheet has no rule starting %q", sel)
	}
	decl := css[i:]
	if j := strings.Index(decl, "}"); j >= 0 {
		decl = decl[:j]
	}
	return decl
}
