package main

// styleui_test.go — the render and interaction contracts styling introduced,
// and the one property the whole byte budget rests on: an UNSTYLED cell must be
// byte-identical to what it was before any of this existed.

import (
	"strings"
	"testing"
)

// THE COMMON CASE MUST NOT HAVE MOVED. Styling adds a class to the cells that
// have one; every other cell in the sheet — which on a real sheet is almost all
// of them — has to render the exact bytes it always did, or the measured
// break-evens, the 55-byte edit patch and the 20 KB grid region are all stale.
func TestUnstyledCellMarkupIsUnchanged(t *testing.T) {
	cases := []struct {
		name string
		cell Cell
		want string
	}{
		{
			name: "a number",
			cell: Cell{Ref: CellRef{Row: 6, Col: 3}, Kind: KindNumber, Computed: "490", Display: "490"},
			want: `<b id="D7" style="--r:6">490</b>`,
		},
		{
			name: "text",
			cell: Cell{Ref: CellRef{Row: 6, Col: 3}, Kind: KindText, Computed: "hi", Display: "hi"},
			want: `<b id="D7" class="t" style="--r:6">hi</b>`,
		},
		{
			name: "a formula",
			cell: Cell{Ref: CellRef{Row: 0, Col: 0}, Kind: KindFormula, Raw: "=B1", Computed: "7", Display: "7"},
			want: `<b id="A1" class="f" style="--r:0" data-r="=B1">7</b>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderCells([]Cell{tc.cell}); got != tc.want {
				t.Errorf("cell markup grew:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// A STYLED CELL COSTS FOUR BYTES, and they are a class rather than a colour.
// The look itself is one rule in the page shell; this is the reference to it.
func TestStyledCellCarriesOnlyItsClass(t *testing.T) {
	got := renderCells([]Cell{
		{Ref: CellRef{Row: 6, Col: 3}, Kind: KindNumber, Computed: "490", Display: "490", Style: 3},
	})
	want := `<b id="D7" class="s3" style="--r:6">490</b>`
	if got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	// The kind class and the style class are two facts and both survive.
	got = renderCells([]Cell{
		{Ref: CellRef{Row: 6, Col: 3}, Kind: KindText, Computed: "hi", Display: "hi", Style: 12},
	})
	if want := `<b id="D7" class="t s12" style="--r:6">hi</b>`; got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "color") || strings.Contains(got, "font-weight") {
		t.Error("the cell is carrying its appearance instead of a reference to it")
	}
}

// THE PREDICATE IS ONE FUNCTION BECAUSE THREE PLACES HAVE TO AGREE. The
// renderers decide whether to emit an element, maskOf decides whether the server
// believes the browser has one, and renderCellPatch decides insert/morph/remove.
// A disagreement between any two is an orphan or a missing value.
func TestCellRenderedIsTheSameOnBothEdges(t *testing.T) {
	blank := Cell{Ref: CellRef{Row: 0, Col: 0}}
	styled := Cell{Ref: CellRef{Row: 0, Col: 0}, Style: 2}
	valued := Cell{Ref: CellRef{Row: 0, Col: 0}, Kind: KindNumber, Computed: "1", Display: "1"}

	if cellRendered(blank) {
		t.Error("an unstyled empty cell must not render")
	}
	if !cellRendered(styled) {
		t.Error("a styled empty cell must render — styling a cell makes it live")
	}
	if !cellRendered(valued) {
		t.Error("a cell with a value must render")
	}
	// And the renderers follow it, in both directions.
	if got := renderCells([]Cell{styled}); got == "" {
		t.Error("renderCells dropped a styled empty cell; a background on a blank cell would never be drawn")
	}
	if got := renderCells([]Cell{blank}); got != "" {
		t.Errorf("renderCells emitted %q for an unstyled empty cell", got)
	}
	// The mask is the server's model of the same fact.
	row := make([]Cell, MaxCols)
	row[0] = styled
	if m := maskOf(row, 1); m[0] != 1 {
		t.Errorf("maskOf says the browser has no element for a styled blank cell (mask %b)", m[0])
	}
	row[0] = blank
	if m := maskOf(row, 1); m[0] != 0 {
		t.Errorf("maskOf still claims an element after the style was cleared (mask %b)", m[0])
	}
}

// A styled EMPTY cell is an element with no text — which is the whole point of
// it, since what it carries is a background.
func TestStyledEmptyCellIsAnElementWithNoText(t *testing.T) {
	got := renderCells([]Cell{{Ref: CellRef{Row: 4, Col: 4}, Style: 7}})
	if want := `<b id="E5" class="s7" style="--r:4"></b>`; got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

// DISPLAY, NOT COMPUTED. The store keeps three texts and the split is
// load-bearing: recalc and the event log read Computed, the editor reads Raw,
// and only the renderer reads Display.
func TestTheCellRendersItsDisplayText(t *testing.T) {
	c := Cell{
		Ref: CellRef{Row: 1, Col: 1}, Kind: KindNumber,
		Raw: "1234.5", Computed: "1234.5", Display: "$1,234.50", Style: 4,
	}
	got := renderCells([]Cell{c})
	if !strings.Contains(got, ">$1,234.50<") {
		t.Errorf("the cell is not showing its formatted text: %q", got)
	}
	// ...and the RAW value has to come along, or F2 on a currency cell would
	// open the editor on "$1,234.50" and commit a string. T.rawOf prefers
	// data-r over textContent, which is the same route a formula already used.
	if !strings.Contains(got, `data-r="1234.5"`) {
		t.Errorf("a formatted cell must carry its raw value for the editor: %q", got)
	}
	// An unformatted cell must NOT grow the attribute.
	plain := Cell{Ref: CellRef{Row: 1, Col: 1}, Kind: KindNumber, Raw: "5", Computed: "5", Display: "5"}
	if strings.Contains(renderCells([]Cell{plain}), "data-r") {
		t.Error("an unformatted literal grew a data-r attribute")
	}
}

// THE RULE HAS TO OUT-SPECIFY THE GRID'S OWN. `#b b.t{text-align:left}` and
// `#b b.f{color:#0b57d0}` are (1,1,1); a bare `.s3` is (0,1,0) and would lose
// every time, so a red formula would stay blue. Matching their specificity and
// arriving later makes source order the tie-break.
func TestStyleRulesCanBeatTheKindRules(t *testing.T) {
	css := cascadeCSS([]StyleRule{
		{ID: 3, Style: Style{Bold: true, FG: "#cc0000"}},
	}, nil, nil)
	// EVERY PROPERTY, NOT JUST THE ONES THIS STYLE SETS. A cell rule sits at
	// the bottom of a three-level cascade and has to be able to override a row
	// or a column BACK to plain, and an empty declaration is not an override.
	// See Style.CSSFull and DATA-MODEL Change 6.
	want := `#` + bufferID + ` b.s3{font-weight:700;font-style:normal;color:#cc0000;background:` +
		sheetBG + `;` + noWrapCSS[:len(noWrapCSS)-1] + `}`
	if css != want {
		t.Errorf("\n got %q\nwant %q", css, want)
	}
	// And the sheet's stylesheet comes AFTER the build's in the document.
	page := pageShellWidths("demo", 0, 3, "", zeroAnchor(), nil, DefaultRows, css, nil, true)
	iGrid := strings.Index(page, "#"+bufferID+" b.t{")
	iSheet := strings.Index(page, want)
	if iGrid < 0 || iSheet < 0 {
		t.Fatalf("could not find both stylesheets (grid at %d, sheet at %d)", iGrid, iSheet)
	}
	if iSheet < iGrid {
		t.Error("the sheet's stylesheet comes BEFORE the grid's; equal specificity means the later one wins")
	}
}

// A number-format-only style STILL GETS A RULE, and the reason is the cascade
// rather than its appearance. CSS cannot turn 1234.5 into $1,234.50 — that
// happens to the text, in the store — but a currency cell inside a BOLD column
// has to render un-bold, because record-level resolution says the cell's own
// style wins outright. A rule that said nothing would leave the column's bold
// showing, which is the store and the screen disagreeing about the same cell.
func TestAFormatOnlyStyleStatesEveryProperty(t *testing.T) {
	css := cascadeCSS([]StyleRule{{ID: 1, Style: Style{Fmt: FmtCurrency}}}, nil, nil)
	for _, want := range []string{"font-weight:400", "font-style:normal", "color:inherit"} {
		if !strings.Contains(css, want) {
			t.Errorf("a format-only style must state %s so it can override a level: %q", want, css)
		}
	}
}

// A rule body must never leave a hole a level underneath it can shine through.
// `CSSFull` says `background:transparent` for a style with no fill, which is
// right over nothing and wrong over a COLUMN BACKDROP — and a cell with its own
// style ignores its column entirely, fill included.
func TestRuleBodiesNeverLeaveAHoleForALevel(t *testing.T) {
	css := cascadeCSS(
		[]StyleRule{{ID: 1, Style: Style{Bold: true}}, {ID: 2, Style: Style{BG: "#fff2cc"}}},
		map[int]int{3: 2}, map[int]int{7: 1})
	if strings.Contains(css, "background:transparent") {
		t.Errorf("a rule left the level underneath it visible: %q", css)
	}
	if !strings.Contains(css, "background:#fff2cc") {
		t.Errorf("a real fill was dropped: %q", css)
	}
}

// The three levels, in the one order that makes CSS agree with the store. All
// three selectors are (1,1,1), so source order IS the precedence.
func TestTheCascadeIsEmittedColumnRowCell(t *testing.T) {
	css := cascadeCSS(
		[]StyleRule{{ID: 1, Style: Style{Bold: true}}, {ID: 2, Style: Style{Italic: true}}},
		map[int]int{3: 1}, map[int]int{12: 2})
	iCol := strings.Index(css, `b[id^=D]`)
	iRow := strings.Index(css, `b[style="--r:12"]`)
	iCell := strings.Index(css, `b.s1{`)
	if iCol < 0 || iRow < 0 || iCell < 0 {
		t.Fatalf("missing a level (col %d, row %d, cell %d): %q", iCol, iRow, iCell, css)
	}
	if !(iCol < iRow && iRow < iCell) {
		t.Errorf("the cascade is out of order (col %d, row %d, cell %d): %q", iCol, iRow, iCell, css)
	}
}

// The row level reads the `--r` a cell already carries, so it costs no markup.
// If writeCell ever spells that attribute differently, the row level silently
// stops matching — this is the test that makes it loud.
func TestTheRowSelectorMatchesWhatWriteCellEmits(t *testing.T) {
	var b strings.Builder
	writeCell(&b, Cell{
		Ref: CellRef{Row: 12, Col: 3}, Kind: KindNumber,
		Raw: "5", Computed: "5", Display: "5",
	}, 'D', "13", "12")
	markup := b.String()
	if rowGroupMode || cellColMode {
		t.Skip("the row selector's exact-match arm is the default render mode")
	}
	if !strings.Contains(markup, `style="`+rowVarDecl("12")+`"`) {
		t.Fatalf("the cell does not carry the attribute the row level matches: %q", markup)
	}
	if !strings.Contains(rowLevelSelector(12), `[style="`+rowVarDecl("12")+`"]`) {
		t.Errorf("the row selector does not match it: %q", rowLevelSelector(12))
	}
	// And row 1 must not match row 12.
	if strings.Contains(markup, `style="`+rowVarDecl("1")+`"`) {
		t.Error("row 1's declaration is a prefix of row 12's without a terminator")
	}
}

// A styled column paints the rows that hold nothing, because a column style
// materializes no cell and sparse rendering draws no element for an empty one.
// The backdrop is the vertical grid-line element that was already there.
func TestAColumnFillPaintsTheWholeColumn(t *testing.T) {
	css := cascadeCSS([]StyleRule{{ID: 1, Style: Style{BG: "#d9ead3"}}}, map[int]int{3: 1}, nil)
	if !strings.Contains(css, `#`+bufferID+`>i:nth-of-type(5){background:#d9ead3}`) {
		t.Errorf("column D has no backdrop, so its empty rows stay white: %q", css)
	}
	// A column with no fill must not paint one.
	plain := cascadeCSS([]StyleRule{{ID: 1, Style: Style{Bold: true}}}, map[int]int{3: 1}, nil)
	if strings.Contains(plain, ">i:nth-of-type") {
		t.Errorf("a fill-less column style painted a backdrop: %q", plain)
	}
}

// The stylesheet element exists even when it is empty, because a patch needs
// something to morph over — the alternative is an append-or-morph decision on
// the one path where getting it wrong means two stylesheets.
func TestTheStyleSheetElementIsAlwaysPresent(t *testing.T) {
	page := pageShellWidths("demo", 0, 3, "", zeroAnchor(), nil, DefaultRows, "", nil, true)
	if !strings.Contains(page, `<style id="`+styleSheetID+`"></style>`) {
		t.Error("a sheet nobody has styled has no #sy to patch")
	}
}

// FOUR ELEMENTS, ONE REASON. Datastar keys request cancellation on the element,
// so a command sharing an issuer with another aborts it. Styling is a WRITE
// whose commands accumulate (bold then italic is two intentions), so it joins
// `#cl`/`#fl`/`#pv` in opting out; `#se` is the other side of the same rule and
// keeps `auto` deliberately, because a superseded selection is not a lost one.
func TestStyleCommandHasItsOwnUncancellableIssuer(t *testing.T) {
	page := pageShellWidths("demo", 0, 3, "", zeroAnchor(), nil, DefaultRows, "", nil, true)
	if !strings.Contains(page, `<div id="`+styleIssuerID+`" hidden data-on:`+styleEvent) {
		t.Fatal("the style command has no issuer element of its own")
	}
	post := stylePost("demo")
	if !strings.Contains(post, `requestCancellation:'disabled'`) {
		t.Error("a style command can be aborted by the next one; bold-then-italic is two intentions")
	}
	if !strings.Contains(post, "payload:{") {
		t.Error("the style command does not carry an explicit payload, so the selection would join the steady-state signal set")
	}
	// Nothing else may post from that element, and it must not be the aggregate's.
	for _, other := range []string{clearEvent, fillEvent, pasteEvent, selEvent} {
		if strings.Contains(post, other) {
			t.Errorf("the style command mentions %q; issuers must not be shared", other)
		}
	}
	// The selection commit keeps Datastar's default, which is the opposite
	// requirement: only the newest rectangle has an answer worth having.
	if strings.Contains(selPost("demo"), "requestCancellation") {
		t.Error("#se lost its 'auto' cancellation; a newer selection must supersede an older one")
	}
}

// The selection never rides up on an ordinary request: the four corner signals
// are underscore-prefixed and the style command names the range explicitly.
func TestStylingDoesNotPutTheSelectionInTheSignalSet(t *testing.T) {
	page := pageShellWidths("demo", 0, 3, "", zeroAnchor(), nil, DefaultRows, "", nil, true)
	// The toolbar controls dispatch an event; they do not post.
	tb := styleToolbarHTML()
	if strings.Contains(tb, "@post") || strings.Contains(tb, "@get") {
		t.Error("a toolbar control issues its own request instead of dispatching to #st")
	}
	if !strings.Contains(page, tb) {
		t.Error("the toolbar is not in the page shell")
	}
	// And no new ordinary signal was declared for any of it.
	for _, sig := range []string{",fg:", ",bg:", ",bold:", ",align:", ",fmt:"} {
		if strings.Contains(page, sig) {
			t.Errorf("styling declared %q as a steady-state signal", sig)
		}
	}
}

// ─── Column resize ────────────────────────────────────────────────────────────

// THE DRAG MUST WRITE NO SIGNAL. That is the whole fix: a signal write runs the
// data-style effect, which rewrites `--w-N`, which is a track of
// `grid-template-columns`, which relayouts every cell in the buffer. Measured
// before and after on a dense window: 298 dropped frames against 1.
func TestTheResizeDragWritesNoSignal(t *testing.T) {
	if strings.Contains(rzMoveExpr, "$") {
		t.Errorf("the resize drag still writes a signal: %q", rzMoveExpr)
	}
	if strings.Contains(rzDownExpr, "$rw") || strings.Contains(rzDownExpr, "$rc=") {
		t.Errorf("the drag START still writes the override pair, so the whole drag reflows: %q", rzDownExpr)
	}
	// The commit is the ONLY moment that touches a signal or the network.
	up := rzUpExpr("demo")
	if !strings.Contains(up, "$rc=") || !strings.Contains(up, "$rw=") || !strings.Contains(up, "@post") {
		t.Errorf("the release does not commit: %q", up)
	}
}

// The guide is shell markup with no Datastar binding at all — it is moved by a
// transform, which is the only way to preview a resize without laying out the
// buffer.
func TestTheResizeGuideIsInertShellMarkup(t *testing.T) {
	page := pageShellWidths("demo", 0, 3, "", zeroAnchor(), nil, DefaultRows, "", nil, true)
	if !strings.Contains(page, `<div id="`+guideID+`"></div>`) {
		t.Fatal("the drag guide is missing from the page shell")
	}
	if !strings.Contains(gridCSS, "#"+guideID+"{position:fixed") {
		t.Error("the guide is not position:fixed; an absolutely positioned one would need scroll correction")
	}
	if !strings.Contains(gridCSS, "will-change:transform") {
		t.Error("the guide is not hinted for compositing")
	}
	// Built for two axes, because variable row heights are coming and will have
	// the identical problem one axis over.
	js := gridKeysScript() + anchorScript()
	for _, want := range []string{"translateX(", "translateY(", "T.gdShow=function(axis"} {
		if !strings.Contains(js, want) {
			t.Errorf("the guide is single-axis: %q missing", want)
		}
	}
}
