package main

import (
	"strconv"
	"strings"
	"testing"
)

// The row strip is the substrate variable row heights need: an element per row
// that carries the rule now, and its height and background later. These tests
// pin the two properties the rest of the design rests on.

func TestEveryBufferedRowGetsAStrip(t *testing.T) {
	const lo, hi = 40, 89
	// A dense rectangle of blank cells is what Window returns; clampWindow takes
	// the row count from the slice, so a nil one renders no rows at all.
	win := renderWindow(blankCells(lo, hi), lo, hi, "demo", selRange{})
	for row := lo; row <= hi; row++ {
		want := `<i class="` + stripClass + `" style="--r:` + strconv.Itoa(row) + `">`
		if !strings.Contains(win, want) {
			t.Fatalf("row %d has no strip; an empty row would have no rule, no height and "+
				"no background", row)
		}
	}
	if n := strings.Count(win, `class="`+stripClass+`"`); n != hi-lo+1 {
		t.Errorf("%d strips for %d rows", n, hi-lo+1)
	}
}

// Strips paint the row's background, so they must come BEFORE the cells in
// document order — both are absolutely positioned children of the same element,
// so paint order is document order and a strip after a cell would hide it.
func TestStripsComeBeforeCells(t *testing.T) {
	cells := blankCells(0, 9)
	cells[2*MaxCols+1] = Cell{Ref: CellRef{Row: 2, Col: 1}, Computed: "x", Kind: KindText}
	win := renderWindow(cells, 0, 9, "demo", selRange{})
	firstStrip := strings.Index(win, `class="`+stripClass+`"`)
	firstCell := strings.Index(win, `<b id="B3"`)
	if firstStrip < 0 || firstCell < 0 {
		t.Fatalf("strip at %d, cell at %d", firstStrip, firstCell)
	}
	if firstStrip > firstCell {
		t.Error("a strip is emitted after a cell; it would paint over the cell's text")
	}
}

// The tiled gradient is gone. It only ever worked because every row was the same
// height, which is the first thing variable heights break — and it could not
// paint a row background at all.
func TestNoTiledGradientRemains(t *testing.T) {
	page := pageShell("demo", 0, 249, renderWindow(blankCells(0, 249), 0, 249, "demo", selRange{}), zeroAnchor())
	if strings.Contains(page, "repeating-linear-gradient") {
		t.Error("the tiled row-line gradient is still in the stylesheet")
	}
}

// The gutter needs no new elements: it already emits one row number per buffered
// row, so its rule is a border on an element that exists.
func TestTheGutterDrawsItsRuleWithoutExtraElements(t *testing.T) {
	page := pageShell("demo", 0, 249, renderWindow(blankCells(0, 249), 0, 249, "demo", selRange{}), zeroAnchor())
	i := strings.Index(page, "#"+gutterID+">b{")
	if i < 0 {
		t.Fatal("no gutter row-number rule in the stylesheet")
	}
	rule := page[i : i+strings.IndexByte(page[i:], '}')]
	if !strings.Contains(rule, "border-bottom") {
		t.Errorf("the gutter row number does not draw the rule:\n  %s", rule)
	}
}

// Row groups measured 7.8x slower to morph than flat and must stay off.
func TestRowGroupsAreOffByDefault(t *testing.T) {
	if rowGroupMode {
		t.Error("row groups are on; they cost 49.4ms per morph against 6.3ms flat")
	}
}

// ─── axis fills ───────────────────────────────────────────────────────────────

// A row fill has to cover the whole row, including the columns holding nothing.
// Before the strip existed a row style could only be expressed as a selector over
// cells, so "make this row yellow" coloured the cells that happened to hold a
// value and left the rest white — the feature failing, not merely looking odd.
func TestARowFillCoversTheWholeRow(t *testing.T) {
	rules := []StyleRule{{ID: 1, Style: Style{BG: "#ffd966"}}}
	css := cascadeCSS(rules, nil, map[int]int{2: 1})

	if !strings.Contains(css, `>i.`+stripClass+`[style="--r:2"]`) {
		t.Errorf("no strip backdrop for row 2; the fill would only reach cells that exist:\n  %s", css)
	}
	if !strings.Contains(css, "background:#ffd966") {
		t.Errorf("the row's fill is missing:\n  %s", css)
	}
}

// The column twin already existed; this pins that both axes are covered and that
// they are covered the same way.
func TestBothAxesFillEmptySpace(t *testing.T) {
	rules := []StyleRule{{ID: 1, Style: Style{BG: "#b6d7a8"}}}
	col := cascadeCSS(rules, map[int]int{4: 1}, nil)
	row := cascadeCSS(rules, nil, map[int]int{4: 1})
	if !strings.Contains(col, `>i:nth-of-type(6){background:#b6d7a8}`) {
		t.Errorf("column 4 has no backdrop:\n  %s", col)
	}
	if !strings.Contains(row, `>i.`+stripClass+`[style="--r:4"]`) {
		t.Errorf("row 4 has no backdrop:\n  %s", row)
	}
}

// A fill with no colour must emit no backdrop, or every bold-only row style
// would paint a transparent box and grow the stylesheet for nothing.
func TestAColourlessRowStyleEmitsNoBackdrop(t *testing.T) {
	bold := true
	_ = bold
	rules := []StyleRule{{ID: 1, Style: Style{Bold: true}}}
	css := cascadeCSS(rules, nil, map[int]int{7: 1})
	if strings.Contains(css, `i.`+stripClass+`[style="--r:7"]`) {
		t.Errorf("a bold-only row style emitted a backdrop:\n  %s", css)
	}
}

// Two renders of an unchanged window must produce identical bytes, or the
// screen's stylesheet compare patches on every push forever.
func TestTheStylesheetIsStableAcrossRenders(t *testing.T) {
	rules := []StyleRule{{ID: 1, Style: Style{BG: "#ffd966"}}, {ID: 2, Style: Style{BG: "#b6d7a8"}}}
	rows := map[int]int{9: 1, 2: 1, 5: 2}
	cols := map[int]int{3: 2, 1: 1}
	first := cascadeCSS(rules, cols, rows)
	for i := 0; i < 8; i++ {
		if got := cascadeCSS(rules, cols, rows); got != first {
			t.Fatalf("render %d differs; the stylesheet would be patched on every push", i)
		}
	}
}
