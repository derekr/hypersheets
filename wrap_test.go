package main

import (
	"strings"
	"testing"
	"time"
)

// Wrap is where the style cascade and the row geometry meet: it is the only
// formatting property whose effect is that a row is the wrong height. So it has
// to be a level default like every other one — "wrap this column" cannot mean
// "wrap the cells that happen to hold a value today".
func TestWrapCascadesFromTheColumnLevel(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "wrapcol")
	mustSetColStyle(t, sh, []int{2}, StylePatch{Wrap: Set(true)})

	css := sheetStyleCSS(sh, 0, 40)
	want := `#` + bufferID + ` b[id^=C]{`
	i := strings.Index(css, want)
	if i < 0 {
		t.Fatalf("no column rule for the wrapped column: %q", css)
	}
	if rule := css[i : i+strings.Index(css[i:], "}")]; !strings.Contains(rule, "white-space:pre-wrap") {
		t.Errorf("the column rule does not wrap: %q", rule)
	}
	// And a cell in that column that has no style of its own grows no markup,
	// which is the whole reason the level exists.
	if got := renderCells([]Cell{{
		Ref: CellRef{Row: 1, Col: 2}, Kind: KindText, Raw: "x", Computed: "x", Display: "x",
	}}); strings.Contains(got, "white-space") || strings.Contains(got, "s1") {
		t.Errorf("a cell in a wrapped column carries the wrap itself: %q", got)
	}
}

// A cell must be able to say "not wrapped" over a wrapped column, which is only
// possible if the cell rule states the property in both directions. Style.CSS
// (the abbreviated form) is not enough here, and CSSFull is what the render
// layer uses.
func TestACellCanTurnAWrappedColumnOff(t *testing.T) {
	on, off := Style{Wrap: true}.CSSFull(), Style{}.CSSFull()
	if !strings.Contains(on, "white-space:pre-wrap") {
		t.Errorf("a wrapped style does not say so: %q", on)
	}
	if !strings.Contains(off, "white-space:nowrap") {
		t.Errorf("an unwrapped style leaves the column's wrap standing: %q", off)
	}
	// The line box goes with it. A wrapped cell whose lines were the row's
	// pitch apart would need three times the height it should.
	if !strings.Contains(on, "line-height:16px") || !strings.Contains(off, "line-height:calc(") {
		t.Errorf("wrap did not bring its line-height: on=%q off=%q", on, off)
	}
}
