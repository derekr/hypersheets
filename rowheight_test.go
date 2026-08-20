package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func heightSheet(t *testing.T) *Sheet {
	t.Helper()
	c := newTestCache(t, 4, time.Minute)
	return mustOpen(t, c, "heights")
}

func TestRowHeightRoundTrips(t *testing.T) {
	sh := heightSheet(t)
	if err := sh.SetRowHeight([]int{3, 7}, 60); err != nil {
		t.Fatal(err)
	}
	got, err := sh.RowHeights(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got[3] != 60 || got[7] != 60 {
		t.Fatalf("heights = %v, want 3 and 7 at 60", got)
	}
	if len(got) != 2 {
		t.Errorf("heights = %v, want only the two rows that were set", got)
	}
}

// A row at the default must be ABSENT, not stored as 22. That is what keeps the
// render's per-row rules proportional to the number of resized rows.
func TestADefaultHeightIsNotStored(t *testing.T) {
	sh := heightSheet(t)
	if err := sh.SetRowHeight([]int{4}, 60); err != nil {
		t.Fatal(err)
	}
	if err := sh.SetRowHeight([]int{4}, rowHeightPx); err != nil {
		t.Fatal(err)
	}
	got, err := sh.RowHeights(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("heights = %v, want none: a row reset to the default is not a record", got)
	}
}

// Resetting a height must not take the row's style with it.
func TestAResetHeightKeepsTheRowStyle(t *testing.T) {
	sh := heightSheet(t)
	bold := true
	if _, err := sh.SetRowStyle([]int{5}, StylePatch{Bold: &bold}); err != nil {
		t.Fatal(err)
	}
	if err := sh.SetRowHeight([]int{5}, 80); err != nil {
		t.Fatal(err)
	}
	if err := sh.SetRowHeight([]int{5}, rowHeightPx); err != nil {
		t.Fatal(err)
	}
	styles, err := sh.RowStyles(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if styles[5] == 0 {
		t.Error("resetting the height deleted the row's style; the two ride the same record but are independent")
	}
}

// And the mirror: restyling a row must not disturb its height.
func TestRestylingARowKeepsItsHeight(t *testing.T) {
	sh := heightSheet(t)
	if err := sh.SetRowHeight([]int{6}, 90); err != nil {
		t.Fatal(err)
	}
	bold := true
	if _, err := sh.SetRowStyle([]int{6}, StylePatch{Bold: &bold}); err != nil {
		t.Fatal(err)
	}
	got, err := sh.RowHeights(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got[6] != 90 {
		t.Fatalf("height after a restyle = %v, want 90", got)
	}
}

func TestRowHeightIsClamped(t *testing.T) {
	sh := heightSheet(t)
	if err := sh.SetRowHeight([]int{1}, 5); err != nil {
		t.Fatal(err)
	}
	if err := sh.SetRowHeight([]int{2}, 99999); err != nil {
		t.Fatal(err)
	}
	got, _ := sh.RowHeights(0, 10)
	if got[1] != MinRowHeight {
		t.Errorf("row 1 = %d, want clamped to %d", got[1], MinRowHeight)
	}
	if got[2] != MaxRowHeight {
		t.Errorf("row 2 = %d, want clamped to %d", got[2], MaxRowHeight)
	}
}

func TestZeroHeightIsRefused(t *testing.T) {
	sh := heightSheet(t)
	if err := sh.SetRowHeight([]int{1}, 0); !errors.Is(err, ErrBadHeight) {
		t.Fatalf("err = %v, want ErrBadHeight: zero is ambiguous between "+
			"'default' and 'invisible'", err)
	}
}

func TestTotalHeightCountsOnlyTheDifference(t *testing.T) {
	sh := heightSheet(t)
	base, err := sh.TotalHeight()
	if err != nil {
		t.Fatal(err)
	}
	if want := sh.Rows() * rowHeightPx; base != want {
		t.Fatalf("a fresh sheet is %d tall, want %d", base, want)
	}
	if err := sh.SetRowHeight([]int{2}, 62); err != nil { // +40
		t.Fatal(err)
	}
	if err := sh.SetRowHeight([]int{9}, 2); err != nil { // below the floor, clamps
		t.Fatal(err)
	}
	got, err := sh.TotalHeight()
	if err != nil {
		t.Fatal(err)
	}
	if want := base + 40 + (MinRowHeight - rowHeightPx); got != want {
		t.Fatalf("total = %d, want %d (base %d, +40, %+d)", got, want, base, MinRowHeight-rowHeightPx)
	}
}

// A height is keyed by storage key, so it must follow its row through an insert
// exactly as a row style does.
func TestAHeightFollowsItsRowThroughAnInsert(t *testing.T) {
	sh := heightSheet(t)
	if err := sh.SetRowHeight([]int{5}, 70); err != nil {
		t.Fatal(err)
	}
	if _, err := sh.InsertRows(2, 3); err != nil {
		t.Fatal(err)
	}
	got, err := sh.RowHeights(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got[8] != 70 {
		t.Fatalf("heights = %v, want the 70 to have moved from row 5 to row 8", got)
	}
	if got[5] != 0 {
		t.Errorf("row 5 kept a height it should have handed to row 8: %v", got)
	}
}

func TestRowOffsetAccountsForRowsAbove(t *testing.T) {
	sh := heightSheet(t)
	if off, err := sh.RowOffset(10); err != nil || off != 10*rowHeightPx {
		t.Fatalf("offset(10) = %d, %v; want %d on an unresized sheet", off, err, 10*rowHeightPx)
	}
	if err := sh.SetRowHeight([]int{2}, 62); err != nil { // +40
		t.Fatal(err)
	}
	if err := sh.SetRowHeight([]int{20}, 62); err != nil { // below row 10, must not count
		t.Fatal(err)
	}
	off, err := sh.RowOffset(10)
	if err != nil {
		t.Fatal(err)
	}
	if want := 10*rowHeightPx + 40; off != want {
		t.Fatalf("offset(10) = %d, want %d: only rows ABOVE shift a row down", off, want)
	}
	// The resized row's own height must not count toward its own top.
	if off, _ := sh.RowOffset(2); off != 2*rowHeightPx {
		t.Errorf("offset(2) = %d, want %d: a row's own height is below its top", off, 2*rowHeightPx)
	}
}

// ─── how geometry reaches the page ────────────────────────────────────────────

// A sheet nobody has resized must emit no geometry at all. Everything lays out by
// multiplying, exactly as before, and the feature costs nothing until used.
func TestAUniformWindowEmitsNoGeometry(t *testing.T) {
	if css := rowGeometryCSS(0, 249, 0, 250*rowHeightPx, 250, nil); css != "" {
		t.Fatalf("an unresized window emitted %d bytes of geometry:\n  %s", len(css), css)
	}
}

func TestGeometryPlacesEveryRowBelowAResize(t *testing.T) {
	css := rowGeometryCSS(0, 5, 0, 0, 0, map[int]int{2: 70})
	// Row 2 is where it always was, but 70 tall; every row after it has moved
	// down by the 48px difference.
	for _, want := range []string{
		`{--t:44px;--hr:70px}`,  // row 2
		`{--t:114px;--hr:22px}`, // row 3: 44 + 70
		`{--t:136px;--hr:22px}`, // row 4
	} {
		if !strings.Contains(css, want) {
			t.Errorf("missing %s in:\n  %s", want, css)
		}
	}
	// Rows above the resize are untouched and emit nothing.
	if strings.Contains(css, `{--t:0px`) || strings.Contains(css, `{--t:22px`) {
		t.Errorf("a row above the resize emitted geometry it does not need:\n  %s", css)
	}
}

// A window that starts below a resize inherits the shift as one number, so the
// rules do not have to restate every row above it.
func TestAWindowBelowAResizeStartsAtItsRealTop(t *testing.T) {
	css := rowGeometryCSS(100, 102, 100*rowHeightPx+48, 0, 0, nil)
	if want := `{--t:` + strconv.Itoa(100*rowHeightPx+48) + `px;--hr:22px}`; !strings.Contains(css, want) {
		t.Errorf("window base not applied, want %s in:\n  %s", want, css)
	}
}

func TestTheContainerGrowsWithTheRows(t *testing.T) {
	css := rowGeometryCSS(0, 5, 0, 250*rowHeightPx+48, 250, map[int]int{2: 70})
	if want := `#g{height:` + strconv.Itoa(250*rowHeightPx+48) + `px}`; !strings.Contains(css, want) {
		t.Errorf("no container height rule (%s) in:\n  %s", want, css)
	}
	// And not when the sheet is still uniform.
	if css := rowGeometryCSS(0, 5, 0, 250*rowHeightPx, 250, nil); strings.Contains(css, "#g{height:") {
		t.Errorf("a uniform sheet restated its own height:\n  %s", css)
	}
}

// The client binary-searches this, so it must be flat and sorted whatever order
// the map iterates in.
func TestHeightPairsAreFlatAndSorted(t *testing.T) {
	got := rowHeightPairs(map[int]int{9: 40, 2: 70, 5: 50})
	if got != "[2,70,5,50,9,40]" {
		t.Fatalf("pairs = %s, want [2,70,5,50,9,40]", got)
	}
	if rowHeightPairs(nil) != "[]" {
		t.Errorf("an unresized sheet must still emit a valid empty array")
	}
	// Stable across renders, or the shell's bytes change for no reason.
	for i := 0; i < 6; i++ {
		if again := rowHeightPairs(map[int]int{9: 40, 2: 70, 5: 50}); again != got {
			t.Fatalf("encoding is not stable: %s then %s", got, again)
		}
	}
}

// The client's offset arithmetic is the half the server cannot check, so at least
// pin that the page ships what that arithmetic needs.
func TestTheShellShipsTheHeightsAndTheReader(t *testing.T) {
	page := pageShellWidths("demo", 0, 9, "", zeroAnchor(), nil, DefaultRows, "", map[int]int{2: 70})
	if !strings.Contains(page, heightSignal+":[2,70]") {
		t.Error("the shell does not seed the resized rows")
	}
	if !strings.Contains(page, "setHeights") {
		t.Error("nothing wires the height signal into the client")
	}
}

// THE FLOOR MUST NOT SIT NEXT TO THE DEFAULT. It was 16 against a 22px default,
// which gave a downward drag six pixels of travel before it clamped — and
// clamping is silent, because the clamped height equals the height the row
// already has, so the commit is skipped and nothing happens at all. Every
// downward drag therefore looked broken, and rows piled up at exactly 16.
//
// The number itself is a judgement call; the ratio is not.
func TestTheRowFloorLeavesRoomToDragDownTo(t *testing.T) {
	if travel := rowHeightPx - MinRowHeight; travel < rowHeightPx/2 {
		t.Errorf("a downward drag has only %dpx of travel from the default (%d) before it clamps at %d",
			travel, rowHeightPx, MinRowHeight)
	}
}

// And the drag says what height it is on, so a guide that has stopped moving
// reads as "this is the minimum" rather than as "this is broken".
func TestTheResizeGuideReportsItsHeight(t *testing.T) {
	js := anchorScript()
	for _, want := range []string{
		"T.gdMove=function(pos,px)",
		"gdEl.dataset.px=px+' px'",
		"T.gdShow('y',e.clientY,rzHh)",
		"T.gdMove(rzYy+(h-rzHh),h)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the row drag has no live readout: %q is missing", want)
		}
	}
	// The column drag shares the guide and gets the same readout for free.
	for _, want := range []string{"T.gdShow('x',x,w)", "T.gdMove(rzX+(w-rzW),w)"} {
		if !strings.Contains(js, want) {
			t.Errorf("the column drag has no live readout: %q is missing", want)
		}
	}
	if !strings.Contains(gridCSS, `content:attr(data-px)`) {
		t.Error("the guide's readout has no rule, so the number never renders")
	}
}
