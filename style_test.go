package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── The value types ──────────────────────────────────────────────────────────

func TestNormalizeColor(t *testing.T) {
	cases := []struct {
		in   string
		want string
		bad  bool
	}{
		{in: "", want: ""},
		{in: "   ", want: ""},
		{in: "#f00", want: "#ff0000"},
		{in: "#F00", want: "#ff0000"},
		{in: "#Ff0000", want: "#ff0000"},
		{in: "#123456", want: "#123456"},
		{in: "red", bad: true},
		{in: "#12345", bad: true},
		{in: "#gggggg", bad: true},
		{in: "ff0000", bad: true},
		// The reason this is a whitelist: the render layer writes the result
		// into a <style> element.
		{in: "#f00;}body{display:none", bad: true},
		{in: "rgb(1,2,3)", bad: true},
	}
	for _, tc := range cases {
		got, err := normalizeColor(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("normalizeColor(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeColor(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeColor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStyleCSS(t *testing.T) {
	cases := []struct {
		s    Style
		want string
	}{
		{Style{}, ""},
		{Style{Bold: true}, "font-weight:700"},
		{Style{Bold: true, Italic: true}, "font-weight:700;font-style:italic"},
		{Style{FG: "#cc0000"}, "color:#cc0000"},
		{Style{BG: "#ffff00", Align: AlignRight}, "background:#ffff00;text-align:right"},
		{Style{Align: AlignCenter}, "text-align:center"},
		// A number format is a TEXT transform, so it contributes no CSS at all.
		{Style{Fmt: FmtCurrency}, ""},
		{Style{Bold: true, Fmt: FmtPercent}, "font-weight:700"},
	}
	for _, tc := range cases {
		if got := tc.s.CSS(); got != tc.want {
			t.Errorf("%+v.CSS() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestFormatValue(t *testing.T) {
	cases := []struct {
		v    string
		f    NumFmt
		want string
	}{
		{"1234.5", FmtPlain, "1234.5"},
		{"1234.5", FmtInteger, "1,235"},
		{"1234.5", FmtTwoDP, "1,234.50"},
		{"1234.5", FmtCurrency, "$1,234.50"},
		{"-1234.5", FmtCurrency, "-$1,234.50"},
		{"0.1234", FmtPercent, "12.34%"},
		{"1", FmtInteger, "1"},
		{"999", FmtInteger, "999"},
		{"1000", FmtInteger, "1,000"},
		{"1000000", FmtInteger, "1,000,000"},
		{"-1000000.5", FmtTwoDP, "-1,000,000.50"},
		{"0", FmtCurrency, "$0.00"},
		// Spreadsheet serial days. 45000 is 2023-03-15.
		{"45000", FmtDate, "2023-03-15"},
		{"45000.5", FmtDateTime, "2023-03-15 12:00"},
		{"1", FmtDate, "1899-12-31"},
		{"61", FmtDate, "1900-03-01"},
		// Half away from zero, both directions, because FormatFloat's
		// round-half-to-even would send these two opposite ways.
		{"1235.5", FmtInteger, "1,236"},
		{"-1234.5", FmtInteger, "-1,235"},
		// EVERY format is a no-op on something that is not a number. This is
		// the property that makes it safe to run over a whole window without
		// asking what is in each cell.
		{"", FmtCurrency, ""},
		{"hello", FmtCurrency, "hello"},
		{"#CYCLE!", FmtCurrency, "#CYCLE!"},
		{"#REF!", FmtPercent, "#REF!"},
		{"#ERR!", FmtDate, "#ERR!"},
		// Out of the renderable calendar: show the number, not a nonsense year.
		{"-5", FmtDate, "-5"},
		{"99999999", FmtDate, "99999999"},
	}
	for _, tc := range cases {
		if got := FormatValue(tc.v, tc.f); got != tc.want {
			t.Errorf("FormatValue(%q, %s) = %q, want %q", tc.v, tc.f, got, tc.want)
		}
	}
}

func TestParseStylePatch(t *testing.T) {
	p, err := ParseStylePatch(map[string]string{
		"bold": "1", "fg": "#F00", "align": "right", "fmt": "currency",
	})
	if err != nil {
		t.Fatalf("ParseStylePatch: %v", err)
	}
	if p.Bold == nil || !*p.Bold {
		t.Errorf("bold not set")
	}
	if p.Italic != nil {
		t.Errorf("italic should be absent, not false — absent means leave alone")
	}
	if p.FG == nil || *p.FG != "#ff0000" {
		t.Errorf("fg = %v, want #ff0000", p.FG)
	}
	if p.Align == nil || *p.Align != AlignRight {
		t.Errorf("align not right")
	}
	if p.Fmt == nil || *p.Fmt != FmtCurrency {
		t.Errorf("fmt not currency")
	}
	// An EMPTY value is a reset, not an absence.
	p, err = ParseStylePatch(map[string]string{"fg": ""})
	if err != nil {
		t.Fatalf("ParseStylePatch reset: %v", err)
	}
	if p.FG == nil || *p.FG != "" {
		t.Errorf("empty fg should be a present reset, got %v", p.FG)
	}
	for _, bad := range []map[string]string{
		{"bold": "maybe"}, {"fg": "red"}, {"align": "middle"},
		{"fmt": "#,##0.00"}, {"colour_of_the_sky": "blue"},
	} {
		if _, err := ParseStylePatch(bad); err == nil {
			t.Errorf("ParseStylePatch(%v) should fail", bad)
		}
	}
}

// ─── SetStyle ─────────────────────────────────────────────────────────────────

func styleOf(t *testing.T, sh *Sheet, ref CellRef) Style {
	t.Helper()
	c, err := sh.GetCell(ref)
	if err != nil {
		t.Fatalf("GetCell %s: %v", ref, err)
	}
	return sh.StyleByID(c.Style)
}

func mustSetStyle(t *testing.T, sh *Sheet, refs []CellRef, p StylePatch) Dirty {
	t.Helper()
	d, err := sh.SetStyle(refs, p)
	if err != nil {
		t.Fatalf("SetStyle: %v", err)
	}
	return d
}

func refRange(t *testing.T, s string) []CellRef {
	t.Helper()
	refs, err := ParseRange(s)
	if err != nil {
		t.Fatalf("ParseRange %q: %v", s, err)
	}
	return refs
}

// TestSetStyleMergesRatherThanReplaces is the whole reason StylePatch has
// pointers. "Bold this range" over cells that already have colours must keep
// the colours; a caller that could only pass a whole Style would be a toolbar
// that silently deletes formatting.
func TestSetStyleMergesRatherThanReplaces(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "merge")

	red := refRange(t, "A1:A3")
	mustSetStyle(t, sh, red, StylePatch{FG: Set("#c00000")})
	mustSetStyle(t, sh, refRange(t, "A2:A2"), StylePatch{Align: Set(AlignRight)})
	// Now bold the whole run, which spans three DIFFERENT existing styles.
	mustSetStyle(t, sh, red, StylePatch{Bold: Set(true)})

	want := []Style{
		{Bold: true, FG: "#c00000"},
		{Bold: true, FG: "#c00000", Align: AlignRight},
		{Bold: true, FG: "#c00000"},
	}
	for i, ref := range red {
		if got := styleOf(t, sh, ref); got != want[i] {
			t.Errorf("%s = %+v, want %+v", ref, got, want[i])
		}
	}
	// Three distinct looks were ever asked for: red, red+right, red+bold,
	// red+right+bold. The dedup means A1 and A3 share one id.
	a1, _ := sh.GetCell(red[0])
	a3, _ := sh.GetCell(red[2])
	if a1.Style == 0 || a1.Style != a3.Style {
		t.Errorf("A1 style %d, A3 style %d: identical looks must share an id", a1.Style, a3.Style)
	}
}

// TestSetStyleDedups is the XLSX claim: a thousand cells with one look cost one
// style row, so the CSS is O(distinct styles) and the markup is O(cells).
func TestSetStyleDedups(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "dedup")

	refs := refRange(t, "A1:Z40") // 1,040 cells
	mustSetStyle(t, sh, refs, StylePatch{Bold: Set(true), BG: Set("#ffffcc")})
	if n := countRows(t, sh, "styles"); n != 1 {
		t.Fatalf("styles table holds %d rows for one look, want 1", n)
	}
	if rules := sh.StyleRules(); len(rules) != 1 ||
		rules[0].Style != (Style{Bold: true, BG: "#ffffcc"}) {
		t.Fatalf("StyleRules() = %+v", rules)
	}
	if got := StyleClass(sh.StyleRules()[0].ID); got != "s1" {
		t.Errorf("StyleClass = %q, want s1", got)
	}
	// A no-op re-apply changes nothing and reports nothing dirty, so
	// re-bolding an already-bold range is not a re-render.
	d := mustSetStyle(t, sh, refs, StylePatch{Bold: Set(true)})
	if !d.IsEmpty() {
		t.Errorf("re-applying an identical style reported %d dirty cells", len(d.Cells))
	}
}

func TestSetStyleDirtySet(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "dirty")

	d := mustSetStyle(t, sh, []CellRef{{Row: 3, Col: 0}, {Row: 120, Col: 4}},
		StylePatch{Italic: Set(true)})
	if len(d.Cells) != 2 {
		t.Fatalf("dirty cells = %v, want 2", d.Cells)
	}
	if d.Structural {
		t.Errorf("a style change is a per-cell patch, not a structural change")
	}
	wantBands := []int{0, 2}
	if fmt.Sprint(d.Bands) != fmt.Sprint(wantBands) {
		t.Errorf("dirty bands = %v, want %v", d.Bands, wantBands)
	}
}

// TestStyleSurvivesACellEdit: typing over a styled cell must not lose the
// style. It holds by construction — the write upsert never names the column —
// and this is the test that says so.
func TestStyleSurvivesACellEdit(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "edit")

	ref := CellRef{Row: 4, Col: 2}
	mustSetStyle(t, sh, []CellRef{ref}, StylePatch{Bold: Set(true), Fmt: Set(FmtCurrency)})
	if err := sh.WriteCell(ref, "1234.5", nil); err != nil {
		t.Fatalf("WriteCell: %v", err)
	}
	got, err := sh.GetCell(ref)
	if err != nil {
		t.Fatalf("GetCell: %v", err)
	}
	if s := sh.StyleByID(got.Style); !s.Bold || s.Fmt != FmtCurrency {
		t.Fatalf("style after edit = %+v", s)
	}
	if got.Computed != "1234.5" {
		t.Errorf("Computed = %q, want the machine value 1234.5", got.Computed)
	}
	if got.Display != "$1,234.50" {
		t.Errorf("Display = %q, want $1,234.50", got.Display)
	}
	// Clearing the cell keeps the style: the formatting outlives the content,
	// which is what every spreadsheet does and what makes "format this column
	// then type into it" work.
	if err := sh.WriteCell(ref, "", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := styleOf(t, sh, ref); !got.Bold {
		t.Errorf("style lost when the cell was cleared: %+v", got)
	}
}

func TestClearStyle(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "clear")

	refs := refRange(t, "B2:C3")
	mustSetStyle(t, sh, refs, StylePatch{BG: Set("#00ff00")})
	if n := countRows(t, sh, "cells"); n != 4 {
		t.Fatalf("styling 4 empty cells stored %d rows, want 4", n)
	}
	// A styled empty cell is content, so it moves the used extent.
	if used, err := sh.UsedRows(); err != nil || used != 3 {
		t.Fatalf("UsedRows = %d (%v), want 3 — a yellow empty cell is content", used, err)
	}
	d, err := sh.ClearStyle(refs)
	if err != nil {
		t.Fatalf("ClearStyle: %v", err)
	}
	if len(d.Cells) != 4 {
		t.Errorf("ClearStyle dirty cells = %d, want 4", len(d.Cells))
	}
	// Nothing left to store: the rows go away and the table is sparse again.
	if n := countRows(t, sh, "cells"); n != 0 {
		t.Errorf("cells table holds %d rows after a clear, want 0", n)
	}
	if used, err := sh.UsedRows(); err != nil || used != 0 {
		t.Errorf("UsedRows = %d (%v), want 0", used, err)
	}
	// A cell with content keeps its row.
	if err := sh.WriteCell(CellRef{Row: 1, Col: 1}, "x", nil); err != nil {
		t.Fatal(err)
	}
	mustSetStyle(t, sh, refs, StylePatch{Bold: Set(true)})
	if _, err := sh.ClearStyle(refs); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, sh, "cells"); n != 1 {
		t.Errorf("cells = %d after clearing around a written cell, want 1", n)
	}
}

func TestSetStyleRejectsBadValues(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "bad")
	for _, p := range []StylePatch{
		{FG: Set("red")},
		{BG: Set("#12")},
		{Align: Set(Align(9))},
		{Fmt: Set(NumFmt(99))},
	} {
		if _, err := sh.SetStyle([]CellRef{{}}, p); err == nil {
			t.Errorf("SetStyle(%v) should have failed", p)
		}
	}
	if _, err := sh.SetStyle([]CellRef{{Row: -1}}, StylePatch{Bold: Set(true)}); err == nil {
		t.Errorf("SetStyle on an out-of-grid ref should fail")
	}
	// Nothing was written by any of them.
	if n := countRows(t, sh, "styles"); n != 0 {
		t.Errorf("styles table holds %d rows after only failures", n)
	}
	if n := countRows(t, sh, "cells"); n != 0 {
		t.Errorf("cells table holds %d rows after only failures", n)
	}
}

// TestStyleGrowsTheSheet: styling row 15,000 of a 10,000-row sheet is a legal
// thing to ask, for the same reason writing to it is.
func TestStyleGrowsTheSheet(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "grow")
	if _, err := sh.SetStyle([]CellRef{{Row: 14999, Col: 2}}, StylePatch{Bold: Set(true)}); err != nil {
		t.Fatalf("SetStyle below the extent: %v", err)
	}
	if got := sh.Rows(); got != 15000 {
		t.Fatalf("extent = %d, want 15000", got)
	}
	if s := styleOf(t, sh, CellRef{Row: 14999, Col: 2}); !s.Bold {
		t.Fatalf("style at row 15000 = %+v", s)
	}
}

// ─── Persistence ──────────────────────────────────────────────────────────────

func TestStylesSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	c := NewSheetCache(dir, 4, time.Minute)
	sh := mustOpen(t, c, "reopen")
	mustSetStyle(t, sh, refRange(t, "A1:B2"),
		StylePatch{Bold: Set(true), FG: Set("#123456"), Fmt: Set(FmtPercent)})
	mustSetStyle(t, sh, refRange(t, "C1:C1"), StylePatch{Italic: Set(true)})
	before := sh.StyleRules()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := NewSheetCache(dir, 4, time.Minute)
	defer c2.Close()
	sh2 := mustOpen(t, c2, "reopen")
	after := sh2.StyleRules()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("style table after reopen = %v, want %v", after, before)
	}
	if s := styleOf(t, sh2, CellRef{Row: 1, Col: 1}); !s.Bold || s.FG != "#123456" {
		t.Fatalf("B2 style after reopen = %+v", s)
	}
	// And a NEW style allocated after reopening must not collide with an id
	// that is already in the file.
	mustSetStyle(t, sh2, refRange(t, "D1:D1"), StylePatch{BG: Set("#eeeeee")})
	seen := map[int]bool{}
	for _, r := range sh2.StyleRules() {
		if seen[r.ID] {
			t.Fatalf("style id %d allocated twice", r.ID)
		}
		seen[r.ID] = true
	}
}

// ─── Structural mutations ─────────────────────────────────────────────────────

// TestStylesSurviveStructuralMutations is the requirement in one test: insert a
// row and the styles below it move with their cells, because the style IS a
// column of the cell and rides the shift.
func TestStylesSurviveStructuralMutations(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "shift", 300, MaxCols)

	// A distinct look per row, so a cell landing on the wrong style is visible
	// rather than accidentally right.
	marks := []CellRef{{Row: 10, Col: 1}, {Row: 60, Col: 1}, {Row: 200, Col: 1}}
	for i, ref := range marks {
		mustSetStyle(t, sh, []CellRef{ref},
			StylePatch{FG: Set(fmt.Sprintf("#%02x0000", 0x10*(i+1)))})
	}
	want := make([]Style, len(marks))
	for i, ref := range marks {
		want[i] = styleOf(t, sh, ref)
	}

	check := func(what string, moved []CellRef) {
		t.Helper()
		for i, ref := range moved {
			if got := styleOf(t, sh, ref); got != want[i] {
				t.Errorf("%s: %s = %+v, want %+v", what, ref, got, want[i])
			}
		}
	}

	if _, err := sh.InsertRows(5, 3); err != nil {
		t.Fatalf("InsertRows: %v", err)
	}
	check("after InsertRows(5,3)", []CellRef{{Row: 13, Col: 1}, {Row: 63, Col: 1}, {Row: 203, Col: 1}})

	if _, err := sh.DeleteRows(0, 2); err != nil {
		t.Fatalf("DeleteRows: %v", err)
	}
	check("after DeleteRows(0,2)", []CellRef{{Row: 11, Col: 1}, {Row: 61, Col: 1}, {Row: 201, Col: 1}})

	if _, err := sh.InsertCols(0, 1); err != nil {
		t.Fatalf("InsertCols: %v", err)
	}
	check("after InsertCols(0,1)", []CellRef{{Row: 11, Col: 2}, {Row: 61, Col: 2}, {Row: 201, Col: 2}})

	if _, err := sh.DeleteCols(0, 1); err != nil {
		t.Fatalf("DeleteCols: %v", err)
	}
	check("after DeleteCols(0,1)", []CellRef{{Row: 11, Col: 1}, {Row: 61, Col: 1}, {Row: 201, Col: 1}})

	// A DELETE destroys the styled cell along with its row, and the style row
	// itself survives (nothing collects it below the threshold) — what must not
	// happen is another cell inheriting it.
	if _, err := sh.DeleteRows(11, 1); err != nil {
		t.Fatalf("DeleteRows over a styled cell: %v", err)
	}
	if got := styleOf(t, sh, CellRef{Row: 11, Col: 1}); got != want[0] && !got.IsDefault() {
		// The row that moved up into 11 must not have picked up row 11's look.
		t.Errorf("after deleting the styled row, row 11 = %+v", got)
	}
}

// TestStylesSurviveARebalance exercises the one O(sheet) path: every key in the
// sheet changes, so every styled cell has to arrive at its new key still
// carrying its style.
func TestStylesSurviveARebalance(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "rebal", 400, MaxCols)

	mustSetStyle(t, sh, refRange(t, "A300:D300"), StylePatch{Bold: Set(true), BG: Set("#ff0")})
	want := styleOf(t, sh, CellRef{Row: 299, Col: 0})

	rebalanced := false
	for i := 0; i < 400 && !rebalanced; i++ {
		d, err := sh.InsertRows(1, 1)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		rebalanced = d.Stats.Rebalanced
		if rebalanced {
			t.Logf("rebalanced after %d inserts", i+1)
		}
	}
	if !rebalanced {
		t.Skip("no rebalance in 400 inserts; nothing to check")
	}
	// The marked row has moved down by however many inserts happened, so find
	// it by content instead of by arithmetic.
	found := 0
	cells, err := sh.Window(0, sh.Rows()-1)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	for _, cell := range cells {
		if cell.Style == 0 {
			continue
		}
		found++
		if got := sh.StyleByID(cell.Style); got != want {
			t.Fatalf("%s carries %+v after a rebalance, want %+v", cell.Ref, got, want)
		}
	}
	if found != 4 {
		t.Fatalf("found %d styled cells after a rebalance, want 4", found)
	}
}

// ─── The read path ────────────────────────────────────────────────────────────

func TestWindowCarriesStyleAndDisplay(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "win")

	if err := sh.WriteCell(CellRef{Row: 2, Col: 0}, "1234.5", nil); err != nil {
		t.Fatal(err)
	}
	if err := sh.WriteCell(CellRef{Row: 2, Col: 1}, "hello", nil); err != nil {
		t.Fatal(err)
	}
	mustSetStyle(t, sh, refRange(t, "A3:B3"), StylePatch{Fmt: Set(FmtCurrency), Bold: Set(true)})

	cells, err := sh.Window(0, 4)
	if err != nil {
		t.Fatal(err)
	}
	at := func(row, col int) Cell { return cells[row*MaxCols+col] }

	a3 := at(2, 0)
	if a3.Style == 0 {
		t.Fatalf("A3 has no style id")
	}
	if a3.Computed != "1234.5" {
		t.Errorf("A3.Computed = %q — storage must stay the machine value", a3.Computed)
	}
	if a3.Display != "$1,234.50" {
		t.Errorf("A3.Display = %q, want $1,234.50", a3.Display)
	}
	b3 := at(2, 1)
	if b3.Display != "hello" {
		t.Errorf("B3.Display = %q — a currency format must not mangle text", b3.Display)
	}
	// An unstyled cell's Display is exactly its Computed, so a renderer can
	// print Display unconditionally.
	for _, cell := range cells {
		if cell.Style == 0 && cell.Display != cell.Computed {
			t.Fatalf("%s: unstyled Display %q != Computed %q",
				cell.Ref, cell.Display, cell.Computed)
		}
	}
}

// TestFormattingNeverReachesStorage is the invariant the whole "format on the
// way out" decision exists to make structural: a formatted cell is still a
// number to everything that computes with it.
func TestFormattingNeverReachesStorage(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "nofmt")

	for i := 0; i < 3; i++ {
		if err := sh.WriteCell(CellRef{Row: i, Col: 0}, "1000.5", nil); err != nil {
			t.Fatal(err)
		}
	}
	mustSetStyle(t, sh, refRange(t, "A1:A3"), StylePatch{Fmt: Set(FmtCurrency)})

	res, err := Recalc(sh, CellRef{Row: 0, Col: 1}, "=SUM(A1:A3)")
	if err != nil {
		t.Fatalf("recalc: %v", err)
	}
	var got string
	for _, cc := range res.Computed {
		if cc.Ref == (CellRef{Row: 0, Col: 1}) {
			got = cc.Computed
		}
	}
	if got != "3001.5" {
		t.Fatalf("SUM over currency-formatted cells = %q, want 3001.5", got)
	}
	// And the stored bytes are untouched.
	var stored string
	if err := sh.use(func(db *sql.DB) error {
		return db.QueryRow(`SELECT computed FROM cells WHERE col = 0 LIMIT 1`).Scan(&stored)
	}); err != nil {
		t.Fatal(err)
	}
	if stored != "1000.5" {
		t.Fatalf("stored computed = %q, want 1000.5", stored)
	}
}

// ─── Garbage collection ───────────────────────────────────────────────────────

// TestStyleGC pins the growth bound. The table is swept when it passes
// styleGCLimit, and what the sweep is FOR is the stylesheet: an uncollected
// style is a CSS rule on every first paint with nothing pointing at it.
func TestStyleGC(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "gc")

	ref := []CellRef{{Row: 0, Col: 0}}
	// Cycle one cell through many distinct looks. Every previous look is
	// immediately unreferenced.
	for i := 0; i <= styleGCLimit+4; i++ {
		mustSetStyle(t, sh, ref, StylePatch{FG: Set(fmt.Sprintf("#%06x", i+1))})
	}
	n := countRows(t, sh, "styles")
	if n > styleGCLimit {
		t.Fatalf("styles table holds %d rows after %d distinct looks on one cell, want <= %d",
			n, styleGCLimit+5, styleGCLimit)
	}
	if n != len(sh.StyleRules()) {
		t.Fatalf("in-memory table has %d rules, database has %d rows", len(sh.StyleRules()), n)
	}
	// The look that is actually IN USE must have survived.
	if got := styleOf(t, sh, ref[0]); got.FG != fmt.Sprintf("#%06x", styleGCLimit+5) {
		t.Fatalf("live style = %+v, collected while still referenced", got)
	}
	t.Logf("%d distinct looks applied to one cell, %d style rows survive", styleGCLimit+5, n)
}

// ─── Migration ────────────────────────────────────────────────────────────────

// TestMigratesV5SheetOnOpen builds a v5 file by hand — the schema exactly as it
// was before styling — fills it, and checks that opening it converts without
// changing a single cell.
func TestMigratesV5SheetOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The v5 cells table, verbatim: no style column.
	if _, err := db.Exec(`
		CREATE TABLE cells (
		  k        INTEGER NOT NULL CHECK (k >= 0),
		  col      INTEGER NOT NULL CHECK (col >= 0 AND col < 26),
		  raw      TEXT    NOT NULL DEFAULT '',
		  computed TEXT    NOT NULL DEFAULT '',
		  kind     INTEGER NOT NULL DEFAULT 0,
		  ref0_k   INTEGER, ref0_col INTEGER,
		  ref1_k   INTEGER, ref1_col INTEGER,
		  ref_span INTEGER NOT NULL DEFAULT 0,
		  PRIMARY KEY (k, col)
		) WITHOUT ROWID;
		CREATE TABLE events (
		  seq INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
		  ref TEXT NOT NULL, raw TEXT NOT NULL, prev TEXT NOT NULL);
		CREATE TABLE cols (
		  col INTEGER PRIMARY KEY CHECK (col >= 0 AND col < 26),
		  width INTEGER NOT NULL CHECK (width >= 24 AND width <= 600)) WITHOUT ROWID;
		` + bandsDDL); err != nil {
		t.Fatal(err)
	}
	bi := canonicalBandIndex(keyStride)
	if err := inTx(db, func(tx *sql.Tx) error { return writeBandIndex(tx, bi) }); err != nil {
		t.Fatal(err)
	}
	type want struct{ raw, computed string }
	was := map[CellRef]want{}
	for row := 0; row < 200; row++ {
		for col := 0; col < 4; col++ {
			ref := CellRef{Row: row, Col: col}
			w := want{raw: fmt.Sprintf("r%dc%d", row, col), computed: fmt.Sprint(row*col + 7)}
			was[ref] = w
			if _, err := db.Exec(
				`INSERT INTO cells (k, col, raw, computed, kind) VALUES (?, ?, ?, ?, 2)`,
				bi.keyOf(row), col, w.raw, w.computed); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 5`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	c := NewSheetCache(dir, 4, time.Minute)
	defer c.Close()
	start := time.Now()
	sh := mustOpen(t, c, "old")
	t.Logf("v5 -> v%d on %d cells: %v", schemaVersion, len(was),
		time.Since(start).Round(time.Microsecond))

	var v int
	if err := sh.use(func(db *sql.DB) error {
		return db.QueryRow(`PRAGMA user_version`).Scan(&v)
	}); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	for ref, w := range was {
		got, err := sh.GetCell(ref)
		if err != nil {
			t.Fatalf("GetCell %s: %v", ref, err)
		}
		if got.Raw != w.raw || got.Computed != w.computed {
			t.Fatalf("%s = %q/%q, want %q/%q", ref, got.Raw, got.Computed, w.raw, w.computed)
		}
		if got.Style != 0 {
			t.Fatalf("%s came out of the migration with style %d, want 0", ref, got.Style)
		}
		if got.Display != got.Computed {
			t.Fatalf("%s Display %q != Computed %q", ref, got.Display, got.Computed)
		}
	}
	if len(sh.StyleRules()) != 0 {
		t.Fatalf("a migrated v5 file should have an empty style table")
	}
	// And the file is usable as a v6 file afterwards.
	mustSetStyle(t, sh, []CellRef{{Row: 1, Col: 1}}, StylePatch{Bold: Set(true)})
	if s := styleOf(t, sh, CellRef{Row: 1, Col: 1}); !s.Bold {
		t.Fatalf("styling a migrated file did not take")
	}
}

// TestMigratedAndFreshSchemasMatch: ADD COLUMN appends, so the style column has
// to be declared last, or a migrated file and a fresh one would disagree about
// column ORDER for the same version — invisible until someone writes an
// INSERT ... SELECT without a column list.
func TestMigratedAndFreshSchemasMatch(t *testing.T) {
	cols := func(sh *Sheet) []string {
		var out []string
		if err := sh.use(func(db *sql.DB) error {
			rows, err := db.Query(`SELECT name FROM pragma_table_info('cells') ORDER BY cid`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var n string
				if err := rows.Scan(&n); err != nil {
					return err
				}
				out = append(out, n)
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	c := newTestCache(t, 4, time.Minute)
	fresh := cols(mustOpen(t, c, "fresh"))
	if fresh[len(fresh)-1] != "style" {
		t.Fatalf("style is not the last column of a fresh cells table: %v", fresh)
	}
	t.Logf("cells columns: %s", strings.Join(fresh, ", "))
}

// TestMigratesRealV5File runs the v5 -> v6 open against real files written by a
// shipped binary. Set SS_V5_DIR to a COPY of a data directory.
func TestMigratesRealV5File(t *testing.T) {
	dir := os.Getenv("SS_V5_DIR")
	if dir == "" {
		t.Skip("set SS_V5_DIR to a COPY of a sheet directory to check v5 migration")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := NewSheetCache(dir, 4, time.Minute)
	defer c.Close()
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".db")
		t.Run(id, func(t *testing.T) {
			raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var version int
			if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != 5 {
				raw.Close()
				t.Skipf("%s is user_version %d, not a v5 file", id, version)
			}
			bi, err := loadBandIndex(raw)
			if err != nil || bi == nil {
				raw.Close()
				t.Fatalf("read bands: %v", err)
			}
			wasRows := bi.rows
			type before struct{ raw, computed string }
			was := map[CellRef]before{}
			rows, err := raw.Query(`SELECT k, col, raw, computed FROM cells`)
			if err != nil {
				raw.Close()
				t.Fatal(err)
			}
			for rows.Next() {
				var k int64
				var col int
				var b before
				if err := rows.Scan(&k, &col, &b.raw, &b.computed); err != nil {
					t.Fatal(err)
				}
				if r := bi.rankOf(k); r >= 0 {
					was[CellRef{Row: r, Col: col}] = b
				}
			}
			rows.Close()
			raw.Close()

			start := time.Now()
			sh := mustOpen(t, c, id)
			took := time.Since(start)

			if got := sh.Rows(); got != wasRows {
				t.Fatalf("extent %d -> %d", wasRows, got)
			}
			bad := 0
			for ref, b := range was {
				got, err := sh.GetCell(ref)
				if err != nil {
					t.Fatalf("GetCell %s: %v", ref, err)
				}
				if got.Computed != b.computed || got.Style != 0 {
					bad++
					if bad < 4 {
						t.Errorf("%s: computed %q -> %q, style %d",
							ref, b.computed, got.Computed, got.Style)
					}
				}
			}
			t.Logf("%s: %d cells, %d rows, migrated in %v, %d mismatches",
				id, len(was), wasRows, took.Round(time.Millisecond), bad)
		})
	}
}

// TestSetStyleRangeCost is the toolbar click: bold a screenful. It reports
// rather than asserts a threshold, because the number that matters is whether
// it is a frame or a stall, and the machine decides that.
func TestSetStyleRangeCost(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "cost", 500, MaxCols)

	// A buffer-sized selection: 250 rows x 26 columns, which is what a
	// select-all-visible then bold does.
	refs := refRange(t, "A1:Z250")
	start := time.Now()
	d, err := sh.SetStyle(refs, StylePatch{Bold: Set(true), BG: Set("#ffffcc")})
	if err != nil {
		t.Fatalf("SetStyle: %v", err)
	}
	took := time.Since(start)
	if len(d.Cells) != len(refs) {
		t.Fatalf("styled %d of %d cells", len(d.Cells), len(refs))
	}
	t.Logf("SetStyle over %d cells (%d bands dirty): %v, %d style rows",
		len(refs), len(d.Bands), took.Round(time.Microsecond), countRows(t, sh, "styles"))

	start = time.Now()
	if _, err := sh.ClearStyle(refs); err != nil {
		t.Fatalf("ClearStyle: %v", err)
	}
	t.Logf("ClearStyle over %d cells: %v", len(refs), time.Since(start).Round(time.Microsecond))
}

// ─── The three-level cascade ──────────────────────────────────────────────────
//
// The problem these tests exist for, stated as the user stated it: "you can't
// format more than 2k rows. ideally if i select a column it should be like
// applying the style/condition for all rows in that col". A column style is now
// ONE record, and the first test is the one that says so.

func mustSetColStyle(t *testing.T, sh *Sheet, cols []int, p StylePatch) Dirty {
	t.Helper()
	d, err := sh.SetColStyle(cols, p)
	if err != nil {
		t.Fatalf("SetColStyle %v: %v", cols, err)
	}
	return d
}

func mustSetRowStyle(t *testing.T, sh *Sheet, rows []int, p StylePatch) Dirty {
	t.Helper()
	d, err := sh.SetRowStyle(rows, p)
	if err != nil {
		t.Fatalf("SetRowStyle %v: %v", rows, err)
	}
	return d
}

// effectiveOf reads a cell's resolved look through the window read, which is
// the path the render layer uses.
func effectiveOf(t *testing.T, sh *Sheet, ref CellRef) (own, eff int) {
	t.Helper()
	cells, err := sh.Window(ref.Row, ref.Row)
	if err != nil {
		t.Fatalf("Window %d: %v", ref.Row, err)
	}
	for _, c := range cells {
		if c.Ref == ref {
			return c.Style, c.Effective
		}
	}
	t.Fatalf("%s not in its own window", ref)
	return 0, 0
}

// TestColumnStyleIsOneRecordAndMaterializesNoCells is the whole change in one
// assertion. Styling a column of a blank sheet through SetStyle would be 10,000
// cell writes and 10,000 rows in `cells` that did not exist; through
// SetColStyle it is one row in `cols` and zero cells.
func TestColumnStyleIsOneRecordAndMaterializesNoCells(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "colone")

	if n := countRows(t, sh, "cells"); n != 0 {
		t.Fatalf("a fresh sheet holds %d cells", n)
	}
	d := mustSetColStyle(t, sh, []int{3}, StylePatch{BG: Set("#ffff00")})

	if n := countRows(t, sh, "cells"); n != 0 {
		t.Fatalf("styling a column materialized %d cells", n)
	}
	if n := countRows(t, sh, "cols"); n != 1 {
		t.Fatalf("styling one column wrote %d rows of `cols`, want 1", n)
	}
	if len(d.Cells) != 0 {
		t.Fatalf("a column style reported %d dirty cells; it changed none", len(d.Cells))
	}
	if !d.Config {
		t.Fatalf("a column style must report Config: the render layer has to re-send the stylesheet")
	}
	if len(d.Bands) != len(sh.AllBands()) {
		t.Fatalf("a column style dirtied %d bands, want the whole grid (%d)",
			len(d.Bands), len(sh.AllBands()))
	}
	// And the sheet is NOT longer for it. A column style names no row.
	used, err := sh.UsedRows()
	if err != nil {
		t.Fatalf("UsedRows: %v", err)
	}
	if used != 0 {
		t.Fatalf("a column style took the used extent to %d; it applies to rows that do not exist", used)
	}

	// Every cell of that column, over a whole screen, resolves to it — with no
	// cell of its own.
	ids := sh.ColStyles()
	if len(ids) != 1 || ids[3] == 0 {
		t.Fatalf("ColStyles() = %v, want one entry for column 3", ids)
	}
	cells, err := sh.Window(0, 249)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	n := 0
	for _, cell := range cells {
		if cell.Ref.Col != 3 {
			if cell.Effective != 0 {
				t.Fatalf("%s outside the styled column resolved to %d", cell.Ref, cell.Effective)
			}
			continue
		}
		n++
		if cell.Style != 0 {
			t.Fatalf("%s carries its own style %d; the column style must not be written per cell",
				cell.Ref, cell.Style)
		}
		if cell.Effective != ids[3] {
			t.Fatalf("%s resolved to %d, want the column's %d", cell.Ref, cell.Effective, ids[3])
		}
	}
	if n != 250 {
		t.Fatalf("saw %d cells of the styled column in a 250-row window", n)
	}
	if got := sh.StyleByID(ids[3]).BG; got != "#ffff00" {
		t.Fatalf("column style resolves to BG %q", got)
	}
}

// TestCascadePrecedence pins the order the whole contract is stated in:
// cell ?: row ?: column ?: default.
func TestCascadePrecedence(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "prec", 300, MaxCols)

	mustSetColStyle(t, sh, []int{1}, StylePatch{FG: Set("#0000ff")})
	mustSetRowStyle(t, sh, []int{10}, StylePatch{FG: Set("#00ff00")})
	mustSetStyle(t, sh, []CellRef{{Row: 10, Col: 1}}, StylePatch{FG: Set("#ff0000")})

	cases := []struct {
		ref  CellRef
		want string
		why  string
	}{
		{CellRef{Row: 10, Col: 1}, "#ff0000", "the cell's own style wins over both levels"},
		{CellRef{Row: 10, Col: 2}, "#00ff00", "the row wins where the cell has none"},
		{CellRef{Row: 20, Col: 1}, "#0000ff", "the column applies where neither cell nor row does"},
		{CellRef{Row: 10, Col: 1}, "#ff0000", "stable on a re-read"},
		{CellRef{Row: 20, Col: 2}, "", "no level: the default"},
	}
	for _, tc := range cases {
		_, eff := effectiveOf(t, sh, tc.ref)
		if got := sh.StyleByID(eff).FG; got != tc.want {
			t.Errorf("%s resolved FG %q, want %q — %s", tc.ref, got, tc.want, tc.why)
		}
	}
	// The row level beats the column level even where the cell is inside BOTH.
	_, eff := effectiveOf(t, sh, CellRef{Row: 10, Col: 1})
	if eff == 0 {
		t.Fatalf("a cell inside a styled row and a styled column resolved to nothing")
	}
}

// TestRendererEmitsOnlyTheCellsOwnStyle is the contract the render layer is
// built against, asserted rather than described: Cell.Style stays the cell's
// OWN record so that emitting StyleClass(cell.Style) per cell and the column
// and row rules in CSS applies each level exactly once.
func TestRendererEmitsOnlyTheCellsOwnStyle(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "contract", 200, MaxCols)

	mustSetColStyle(t, sh, []int{0}, StylePatch{Bold: Set(true)})
	mustSetRowStyle(t, sh, []int{5}, StylePatch{Italic: Set(true)})

	cells, err := sh.Window(0, 20)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	for _, cell := range cells {
		if cell.Style != 0 {
			t.Fatalf("%s carries an own style %d after only LEVEL styling; the renderer "+
				"would emit a class for it and the level would be applied twice",
				cell.Ref, cell.Style)
		}
	}
}

// TestSetStyleMergesAgainstTheEffectiveStyle is the reason the record-level
// cascade and a per-property CSS cascade agree. Bolding a cell in a
// currency-formatted column has to produce a bold CURRENCY cell; if the merge
// were against the cell's own (absent) style the format would be silently lost
// the moment anyone touched the cell.
func TestSetStyleMergesAgainstTheEffectiveStyle(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "effmerge")

	if err := sh.WriteCell(CellRef{Row: 0, Col: 0}, "1234.5", nil); err != nil {
		t.Fatalf("WriteCell: %v", err)
	}
	mustSetColStyle(t, sh, []int{0}, StylePatch{Fmt: Set(FmtCurrency)})

	cell, err := sh.GetCell(CellRef{Row: 0, Col: 0})
	if err != nil {
		t.Fatalf("GetCell: %v", err)
	}
	if cell.Display != "$1,234.50" {
		t.Fatalf("a column number format did not reach Display: %q", cell.Display)
	}
	if cell.Computed != "1234.5" {
		t.Fatalf("formatting reached storage: computed %q", cell.Computed)
	}

	mustSetStyle(t, sh, []CellRef{{Row: 0, Col: 0}}, StylePatch{Bold: Set(true)})
	got := styleOf(t, sh, CellRef{Row: 0, Col: 0})
	if !got.Bold {
		t.Fatalf("bold did not take: %+v", got)
	}
	if got.Fmt != FmtCurrency {
		t.Fatalf("bolding a cell dropped the format its column was giving it: %+v", got)
	}
	cell, err = sh.GetCell(CellRef{Row: 0, Col: 0})
	if err != nil {
		t.Fatalf("GetCell: %v", err)
	}
	if cell.Display != "$1,234.50" {
		t.Fatalf("Display after bolding = %q", cell.Display)
	}
}

// TestSetStyleOverALevelThatAlreadySaysItWritesNothing is the other half of the
// saving: re-applying what a level already says must not materialize the cells
// the level exists to not materialize.
func TestSetStyleOverALevelThatAlreadySaysItWritesNothing(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "noop")

	mustSetColStyle(t, sh, []int{0}, StylePatch{Bold: Set(true)})
	before := countRows(t, sh, "cells")

	d, err := sh.SetStyle(refRange(t, "A1:A200"), StylePatch{Bold: Set(true)})
	if err != nil {
		t.Fatalf("SetStyle: %v", err)
	}
	if len(d.Cells) != 0 {
		t.Fatalf("bolding an already-bold column reported %d dirty cells", len(d.Cells))
	}
	if got := countRows(t, sh, "cells"); got != before {
		t.Fatalf("bolding an already-bold column materialized %d cells", got-before)
	}
}

// TestClearStyleFallsBackToTheLevel: clearing a CELL removes the cell's own
// override and reveals what the levels say, which is XLSX's semantic and the
// only one under which the cascade means anything. Clearing the LEVEL is a
// different verb.
func TestClearStyleFallsBackToTheLevel(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "clearfall")

	mustSetColStyle(t, sh, []int{0}, StylePatch{BG: Set("#ffff00")})
	col := sh.ColStyles()[0]
	mustSetStyle(t, sh, []CellRef{{Row: 0, Col: 0}}, StylePatch{FG: Set("#c00000")})

	if _, err := sh.ClearStyle([]CellRef{{Row: 0, Col: 0}}); err != nil {
		t.Fatalf("ClearStyle: %v", err)
	}
	own, eff := effectiveOf(t, sh, CellRef{Row: 0, Col: 0})
	if own != 0 {
		t.Fatalf("ClearStyle left an own style %d", own)
	}
	if eff != col {
		t.Fatalf("a cleared cell resolved to %d, want the column's %d", eff, col)
	}
	// And clearing the level clears the level.
	if _, err := sh.ClearColStyle([]int{0}); err != nil {
		t.Fatalf("ClearColStyle: %v", err)
	}
	if _, eff = effectiveOf(t, sh, CellRef{Row: 0, Col: 0}); eff != 0 {
		t.Fatalf("after ClearColStyle the cell still resolves to %d", eff)
	}
	if n := countRows(t, sh, "cols"); n != 0 {
		t.Fatalf("a cleared column left %d rows in `cols`", n)
	}
}

// ─── Levels through a structural mutation ─────────────────────────────────────

// rowStyleMap is the row level over the whole sheet, for a test that wants to
// see where a style landed rather than to ask about one row.
func rowStyleMap(t *testing.T, sh *Sheet) map[int]int {
	t.Helper()
	m, err := sh.RowStyles(0, sh.Rows()-1)
	if err != nil {
		t.Fatalf("RowStyles: %v", err)
	}
	return m
}

// TestRowStylesFollowTheirRowsThroughInsertAndDelete is the claim that keying
// the table by the STORAGE KEY buys the band-key result for free: the styled
// row moves with its rank, and the rows that did not move were not written.
func TestRowStylesFollowTheirRowsThroughInsertAndDelete(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "rowshift", 300, MaxCols)

	mustSetRowStyle(t, sh, []int{10}, StylePatch{BG: Set("#ff0000")})
	mustSetRowStyle(t, sh, []int{200}, StylePatch{BG: Set("#00ff00")})
	red, green := rowStyleMap(t, sh)[10], rowStyleMap(t, sh)[200]
	if red == 0 || green == 0 || red == green {
		t.Fatalf("row styles did not take: %v", rowStyleMap(t, sh))
	}

	if _, err := sh.InsertRows(5, 3); err != nil {
		t.Fatalf("InsertRows: %v", err)
	}
	m := rowStyleMap(t, sh)
	if m[13] != red || m[203] != green || len(m) != 2 {
		t.Fatalf("after InsertRows(5,3) the row level is %v, want {13:%d, 203:%d}", m, red, green)
	}

	if _, err := sh.DeleteRows(0, 2); err != nil {
		t.Fatalf("DeleteRows: %v", err)
	}
	m = rowStyleMap(t, sh)
	if m[11] != red || m[201] != green || len(m) != 2 {
		t.Fatalf("after DeleteRows(0,2) the row level is %v, want {11:%d, 201:%d}", m, red, green)
	}

	// Deleting the styled row destroys the record with the row, and the row
	// that moves up into its rank must not inherit it.
	if _, err := sh.DeleteRows(11, 1); err != nil {
		t.Fatalf("DeleteRows over a styled row: %v", err)
	}
	m = rowStyleMap(t, sh)
	if _, ok := m[11]; ok {
		t.Fatalf("the row that moved up into rank 11 inherited a style: %v", m)
	}
	if m[200] != green || len(m) != 1 {
		t.Fatalf("after deleting the styled row the level is %v, want {200:%d}", m, green)
	}
}

// TestRowStylesSurviveARebalance exercises the one path where "keyed by k is
// free" is false: a rebalance changes every key in the sheet.
func TestRowStylesSurviveARebalance(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "rowrebal", 400, MaxCols)

	mustSetRowStyle(t, sh, []int{299}, StylePatch{Bold: Set(true), BG: Set("#ff0")})
	want := rowStyleMap(t, sh)[299]
	if want == 0 {
		t.Fatalf("row style did not take")
	}

	rebalanced, inserts := false, 0
	for i := 0; i < 400 && !rebalanced; i++ {
		d, err := sh.InsertRows(1, 1)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		inserts++
		rebalanced = d.Stats.Rebalanced
	}
	if !rebalanced {
		t.Skip("no rebalance in 400 inserts; nothing to check")
	}
	t.Logf("rebalanced after %d inserts", inserts)
	m := rowStyleMap(t, sh)
	if len(m) != 1 {
		t.Fatalf("after a rebalance the row level is %v, want exactly one entry", m)
	}
	if got := m[299+inserts]; got != want {
		t.Fatalf("the styled row landed at %v, want rank %d carrying %d", m, 299+inserts, want)
	}
}

// TestColumnStylesFollowTheirColumns: the column axis is still O(sheet) and the
// column level rides the same statement the widths do.
func TestColumnStylesFollowTheirColumns(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "colshift", 200, MaxCols)

	mustSetColStyle(t, sh, []int{2}, StylePatch{BG: Set("#ff0000")})
	if err := sh.SetColWidth(2, 200); err != nil {
		t.Fatalf("SetColWidth: %v", err)
	}
	want := sh.ColStyles()[2]

	if _, err := sh.InsertCols(0, 1); err != nil {
		t.Fatalf("InsertCols: %v", err)
	}
	if got := sh.ColStyles(); got[3] != want || len(got) != 1 {
		t.Fatalf("after InsertCols(0,1) the column level is %v, want {3:%d}", got, want)
	}
	widths, err := sh.ColWidths()
	if err != nil {
		t.Fatalf("ColWidths: %v", err)
	}
	if widths[3] != 200 {
		t.Fatalf("the width did not move with the column: %v", widths)
	}

	if _, err := sh.DeleteCols(0, 1); err != nil {
		t.Fatalf("DeleteCols: %v", err)
	}
	if got := sh.ColStyles(); got[2] != want || len(got) != 1 {
		t.Fatalf("after DeleteCols(0,1) the column level is %v, want {2:%d}", got, want)
	}

	// A column pushed off the right-hand edge takes its style with it, exactly
	// as its width always has.
	if _, err := sh.DeleteCols(2, 1); err != nil {
		t.Fatalf("DeleteCols over the styled column: %v", err)
	}
	if got := sh.ColStyles(); len(got) != 0 {
		t.Fatalf("deleting the styled column left %v", got)
	}
}

// TestColumnStyleDoesNotBlockAColumnInsert is the decision recorded in
// checkOverflow, asserted: a styled column Z is not content, and refusing a
// structural edit because a column is yellow would be the row-cap bug in a new
// place.
func TestColumnStyleDoesNotBlockAColumnInsert(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "coloverflow")

	mustSetColStyle(t, sh, []int{MaxCols - 1}, StylePatch{BG: Set("#ffff00")})
	if _, err := sh.InsertCols(0, 1); err != nil {
		t.Fatalf("a column style blocked a column insert: %v", err)
	}
	if got := sh.ColStyles(); len(got) != 0 {
		t.Fatalf("the style of the column that fell off the edge survived: %v", got)
	}
	// A styled CELL in the last column still refuses, which is the contrast.
	mustSetStyle(t, sh, []CellRef{{Row: 0, Col: MaxCols - 1}}, StylePatch{BG: Set("#ffff00")})
	if _, err := sh.InsertCols(0, 1); !errors.Is(err, ErrWouldTruncate) {
		t.Fatalf("a styled CELL in the last column should still refuse, got %v", err)
	}
}

// TestRowStyleCountsAsContent covers the three places the previous change
// flagged: the used extent, the insert's tail probe, and (by contrast) the
// column overflow check.
func TestRowStyleCountsAsContent(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "rowcontent")

	if used, err := sh.UsedRows(); err != nil || used != 0 {
		t.Fatalf("fresh UsedRows = %d, %v", used, err)
	}
	mustSetRowStyle(t, sh, []int{499}, StylePatch{BG: Set("#ffff00")})
	used, err := sh.UsedRows()
	if err != nil {
		t.Fatalf("UsedRows: %v", err)
	}
	if used != 500 {
		t.Fatalf("UsedRows = %d after styling row 500; a yellow row is content", used)
	}

	// The insert's tail probe. Style the LAST row, then insert: the sheet must
	// GROW rather than reuse the tail and destroy the record.
	last := sh.Rows() - 1
	mustSetRowStyle(t, sh, []int{last}, StylePatch{BG: Set("#00ff00")})
	before := sh.Rows()
	d, err := sh.InsertRows(0, 1)
	if err != nil {
		t.Fatalf("InsertRows: %v", err)
	}
	if d.Stats.Grew != 1 {
		t.Fatalf("an insert under a styled last row grew by %d, want 1", d.Stats.Grew)
	}
	if sh.Rows() != before+1 {
		t.Fatalf("extent %d -> %d", before, sh.Rows())
	}
	m := rowStyleMap(t, sh)
	if _, ok := m[last+1]; !ok {
		t.Fatalf("the styled last row was pushed off the bottom: %v", m)
	}
}

// TestLevelStylesCountAsReferencesForGC: the sweep is over CELLS, and a look
// worn only by a column or a row has no cell pointing at it. Collecting it
// would delete a live rule out from under the stylesheet.
func TestLevelStylesCountAsReferencesForGC(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "levelgc")

	mustSetColStyle(t, sh, []int{0}, StylePatch{BG: Set("#ff0000")})
	mustSetRowStyle(t, sh, []int{0}, StylePatch{BG: Set("#00ff00")})
	colID, rowID := sh.ColStyles()[0], rowStyleMap(t, sh)[0]
	if colID == 0 || rowID == 0 {
		t.Fatalf("levels did not take")
	}

	// Push the table past the GC threshold with cell styles that are then
	// cleared, so a sweep runs and has plenty to collect.
	for i := 0; i < styleGCLimit+8; i++ {
		ref := []CellRef{{Row: 100 + i, Col: 5}}
		mustSetStyle(t, sh, ref, StylePatch{FG: Set(fmt.Sprintf("#%06x", 0x110000+i))})
		if _, err := sh.ClearStyle(ref); err != nil {
			t.Fatalf("ClearStyle: %v", err)
		}
	}
	// One more style command to trigger the sweep on a table over the limit.
	mustSetStyle(t, sh, []CellRef{{Row: 3, Col: 3}}, StylePatch{Italic: Set(true)})

	live := map[int]bool{}
	for _, r := range sh.StyleRules() {
		live[r.ID] = true
	}
	if !live[colID] {
		t.Fatalf("the garbage collector deleted a style worn by a COLUMN (%d): %v", colID, live)
	}
	if !live[rowID] {
		t.Fatalf("the garbage collector deleted a style worn by a ROW (%d): %v", rowID, live)
	}
	if sh.ColStyles()[0] != colID || rowStyleMap(t, sh)[0] != rowID {
		t.Fatalf("a sweep repointed a level")
	}
	// The sweep ran: 72 distinct looks were created and all but the ones made
	// since the last sweep are gone.
	if n := len(sh.StyleRules()); n > 24 {
		t.Fatalf("the sweep collected nothing: %d rules survive of %d created",
			n, styleGCLimit+9)
	}
}

// TestLevelStylesSurviveReopen: the levels are persisted, and the resident
// snapshot is rebuilt from the file rather than from memory.
func TestLevelStylesSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	c := NewSheetCache(dir, 4, time.Minute)
	sh := mustOpen(t, c, "persist")
	mustSetColStyle(t, sh, []int{4}, StylePatch{Bold: Set(true)})
	mustSetRowStyle(t, sh, []int{7}, StylePatch{Italic: Set(true)})
	wantCol, wantRow := sh.ColStyles()[4], rowStyleMap(t, sh)[7]
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	c2 := NewSheetCache(dir, 4, time.Minute)
	defer c2.Close()
	sh2 := mustOpen(t, c2, "persist")
	if got := sh2.ColStyles()[4]; got != wantCol {
		t.Fatalf("column level after reopen = %d, want %d", got, wantCol)
	}
	if got := rowStyleMap(t, sh2)[7]; got != wantRow {
		t.Fatalf("row level after reopen = %d, want %d", got, wantRow)
	}
	if !sh2.StyleByID(wantCol).Bold || !sh2.StyleByID(wantRow).Italic {
		t.Fatalf("the looks behind the levels did not survive")
	}
	// And the read path knows the row level exists again, which is the cached
	// predicate that would otherwise render a styled row unstyled.
	if _, eff := effectiveOf(t, sh2, CellRef{Row: 7, Col: 0}); eff != wantRow {
		t.Fatalf("after reopen row 7 resolves to %d, want %d", eff, wantRow)
	}
}

// TestLevelPatchesMergePerLevel: a level merges against its OWN current style,
// so "make column D currency" keeps the colour column D already had.
func TestLevelPatchesMergePerLevel(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "levelmerge")

	mustSetColStyle(t, sh, []int{3}, StylePatch{BG: Set("#ffff00")})
	mustSetColStyle(t, sh, []int{3}, StylePatch{Fmt: Set(FmtCurrency)})
	got := sh.StyleByID(sh.ColStyles()[3])
	if got.BG != "#ffff00" || got.Fmt != FmtCurrency {
		t.Fatalf("column level after two patches = %+v", got)
	}

	mustSetRowStyle(t, sh, []int{2}, StylePatch{Bold: Set(true)})
	mustSetRowStyle(t, sh, []int{2}, StylePatch{FG: Set("#0000ff")})
	got = sh.StyleByID(rowStyleMap(t, sh)[2])
	if !got.Bold || got.FG != "#0000ff" {
		t.Fatalf("row level after two patches = %+v", got)
	}

	// Setting a level to what it already is writes nothing.
	d := mustSetRowStyle(t, sh, []int{2}, StylePatch{Bold: Set(true)})
	if len(d.Bands) != 0 {
		t.Fatalf("a no-op row style reported %d dirty bands", len(d.Bands))
	}
}

// TestRowStyleGrowsTheSheet: "make row 15,000 yellow" is a legal thing to ask,
// exactly as writing a cell there is.
func TestRowStyleGrowsTheSheet(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "rowgrow")

	before := sh.Rows()
	mustSetRowStyle(t, sh, []int{before + 4999}, StylePatch{BG: Set("#ffff00")})
	if sh.Rows() < before+5000 {
		t.Fatalf("styling row %d left the sheet %d rows tall", before+5000, sh.Rows())
	}
	if _, eff := effectiveOf(t, sh, CellRef{Row: before + 4999, Col: 0}); eff == 0 {
		t.Fatalf("the grown row did not resolve to its style")
	}
	if _, err := sh.SetRowStyle([]int{RowCeiling + 1}, StylePatch{Bold: Set(true)}); err == nil {
		t.Fatalf("styling a row past the ceiling should fail")
	}
}

// TestWindowCarriesTheEffectiveStyleOnEmptyCells: a blank cell in a styled row
// is styled. The store says so; whether the render layer draws it is a render
// decision, but it must not have to re-derive the cascade to make it.
func TestWindowCarriesTheEffectiveStyleOnEmptyCells(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "emptyeff")

	mustSetRowStyle(t, sh, []int{3}, StylePatch{BG: Set("#ffff00")})
	want := rowStyleMap(t, sh)[3]
	cells, err := sh.Window(0, 5)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	n := 0
	for _, cell := range cells {
		if cell.Ref.Row != 3 {
			continue
		}
		n++
		if cell.Kind != KindEmpty {
			t.Fatalf("%s is not empty", cell.Ref)
		}
		if cell.Effective != want {
			t.Fatalf("blank %s resolved to %d, want %d", cell.Ref, cell.Effective, want)
		}
	}
	if n != MaxCols {
		t.Fatalf("saw %d cells of the styled row", n)
	}
	// GetCell agrees with Window about a cell that does not exist.
	cell, err := sh.GetCell(CellRef{Row: 3, Col: 0})
	if err != nil {
		t.Fatalf("GetCell: %v", err)
	}
	if cell.Effective != want || cell.Style != 0 {
		t.Fatalf("GetCell on a blank styled row: style %d, effective %d", cell.Style, cell.Effective)
	}
}

// TestColWidthResetKeepsAColumnStyle: resetting a column to the default width
// deletes its row — unless the row also carries a style, in which case deleting
// it would silently delete the colour.
func TestColWidthResetKeepsAColumnStyle(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "widthstyle")

	mustSetColStyle(t, sh, []int{1}, StylePatch{BG: Set("#ffff00")})
	want := sh.ColStyles()[1]
	if err := sh.SetColWidth(1, 200); err != nil {
		t.Fatalf("SetColWidth: %v", err)
	}
	if err := sh.SetColWidth(1, DefaultColWidth); err != nil {
		t.Fatalf("SetColWidth reset: %v", err)
	}
	if got := sh.ColStyles()[1]; got != want {
		t.Fatalf("resetting the width deleted the column style: %d -> %d", want, got)
	}
	widths, err := sh.ColWidths()
	if err != nil {
		t.Fatalf("ColWidths: %v", err)
	}
	if w, ok := widths[1]; ok && w != DefaultColWidth {
		t.Fatalf("column 1 width after reset = %d", w)
	}
	// An unstyled column still gives its row back.
	if err := sh.SetColWidth(2, 200); err != nil {
		t.Fatalf("SetColWidth: %v", err)
	}
	if err := sh.SetColWidth(2, DefaultColWidth); err != nil {
		t.Fatalf("SetColWidth reset: %v", err)
	}
	if n := countRows(t, sh, "cols"); n != 1 {
		t.Fatalf("`cols` holds %d rows, want just the styled one", n)
	}
}

// TestMigratesV6SheetOnOpen: the v6 -> v7 step adds the two cascade levels and
// must reinterpret nothing. A v6 file has widths and cell styles; it comes out
// with the identical widths, the identical cell styles, no column style and no
// row style — which is what "this file has no cascade levels" means.
func TestMigratesV6SheetOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The v6 schema, verbatim: cells HAS style, cols does NOT, and there is no
	// `rows` table at all.
	if _, err := db.Exec(`
		CREATE TABLE cells (
		  k        INTEGER NOT NULL CHECK (k >= 0),
		  col      INTEGER NOT NULL CHECK (col >= 0 AND col < 26),
		  raw      TEXT    NOT NULL DEFAULT '',
		  computed TEXT    NOT NULL DEFAULT '',
		  kind     INTEGER NOT NULL DEFAULT 0,
		  ref0_k   INTEGER, ref0_col INTEGER,
		  ref1_k   INTEGER, ref1_col INTEGER,
		  ref_span INTEGER NOT NULL DEFAULT 0,
		  style    INTEGER NOT NULL DEFAULT 0,
		  PRIMARY KEY (k, col)
		) WITHOUT ROWID;
		CREATE TABLE events (
		  seq INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
		  ref TEXT NOT NULL, raw TEXT NOT NULL, prev TEXT NOT NULL);
		CREATE TABLE cols (
		  col INTEGER PRIMARY KEY CHECK (col >= 0 AND col < 26),
		  width INTEGER NOT NULL CHECK (width >= 24 AND width <= 600)) WITHOUT ROWID;
		` + bandsDDL + stylesDDL); err != nil {
		t.Fatal(err)
	}
	bi := canonicalBandIndex(keyStride)
	if err := inTx(db, func(tx *sql.Tx) error { return writeBandIndex(tx, bi) }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO styles (id, bold, italic, fg, bg, align, numfmt) VALUES (1, 1, 0, '', '', 0, 0)`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cols (col, width) VALUES (2, 200), (5, 48)`); err != nil {
		t.Fatal(err)
	}
	type want struct {
		raw, computed string
		style         int
	}
	was := map[CellRef]want{}
	for row := 0; row < 200; row++ {
		for col := 0; col < 4; col++ {
			ref := CellRef{Row: row, Col: col}
			w := want{raw: fmt.Sprintf("r%dc%d", row, col), computed: fmt.Sprint(row*col + 7)}
			if row%17 == 0 {
				w.style = 1
			}
			was[ref] = w
			if _, err := db.Exec(
				`INSERT INTO cells (k, col, raw, computed, kind, style) VALUES (?, ?, ?, ?, 2, ?)`,
				bi.keyOf(row), col, w.raw, w.computed, w.style); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 6`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	c := NewSheetCache(dir, 4, time.Minute)
	defer c.Close()
	start := time.Now()
	sh := mustOpen(t, c, "old")
	t.Logf("v6 -> v%d on %d cells: %v", schemaVersion, len(was),
		time.Since(start).Round(time.Microsecond))

	var v int
	if err := sh.use(func(db *sql.DB) error {
		return db.QueryRow(`PRAGMA user_version`).Scan(&v)
	}); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	for ref, w := range was {
		got, err := sh.GetCell(ref)
		if err != nil {
			t.Fatalf("GetCell %s: %v", ref, err)
		}
		if got.Raw != w.raw || got.Computed != w.computed || got.Style != w.style {
			t.Fatalf("%s = %q/%q/s%d, want %q/%q/s%d",
				ref, got.Raw, got.Computed, got.Style, w.raw, w.computed, w.style)
		}
		// No level exists, so the effective style is the cell's own — the
		// property that makes a migrated file byte-identical to read.
		if got.Effective != w.style {
			t.Fatalf("%s resolved to %d, want its own %d", ref, got.Effective, w.style)
		}
	}
	widths, err := sh.ColWidths()
	if err != nil {
		t.Fatalf("ColWidths: %v", err)
	}
	if widths[2] != 200 || widths[5] != 48 || len(widths) != 2 {
		t.Fatalf("widths after migration: %v", widths)
	}
	if got := sh.ColStyles(); len(got) != 0 {
		t.Fatalf("a migrated v6 file grew column styles: %v", got)
	}
	if got := rowStyleMap(t, sh); len(got) != 0 {
		t.Fatalf("a migrated v6 file grew row styles: %v", got)
	}
	// And it is usable as a v7 file afterwards.
	mustSetColStyle(t, sh, []int{2}, StylePatch{BG: Set("#ffff00")})
	mustSetRowStyle(t, sh, []int{1}, StylePatch{Italic: Set(true)})
	if len(sh.ColStyles()) != 1 || len(rowStyleMap(t, sh)) != 1 {
		t.Fatalf("levelling a migrated file did not take")
	}
	if w, err := sh.ColWidths(); err != nil || w[2] != 200 {
		t.Fatalf("styling a column changed its width: %v %v", w, err)
	}
}

// TestMigratedAndFreshColsSchemasMatch is TestMigratedAndFreshSchemasMatch for
// the other ALTERed table: ADD COLUMN appends, so `style` has to be declared
// last in the cols DDL too.
func TestMigratedAndFreshColsSchemasMatch(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "colschema")
	var out []string
	if err := sh.use(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT name FROM pragma_table_info('cols') ORDER BY cid`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			out = append(out, n)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 || out[len(out)-1] != "style" {
		t.Fatalf("style is not the last column of a fresh cols table: %v", out)
	}
	t.Logf("cols columns: %s", strings.Join(out, ", "))
}

// TestMigratesRealFile runs the migration against real files written by a
// shipped binary, at whatever version they are, and verifies the sheet that
// comes out cell for cell. Set SS_MIGRATE_DIR to a COPY of a data directory.
//
// It is deliberately version-agnostic where TestMigratesRealV5File was pinned
// to one step: the files on this machine are v3 and v6, the chain has to work
// from either, and "a silent misread is unacceptable" is a claim about the
// whole chain rather than about its last link.
func TestMigratesRealFile(t *testing.T) {
	dir := os.Getenv("SS_MIGRATE_DIR")
	if dir == "" {
		t.Skip("set SS_MIGRATE_DIR to a COPY of a sheet directory to check the migration")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := NewSheetCache(dir, 4, time.Minute)
	defer c.Close()
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".db")
		t.Run(id, func(t *testing.T) {
			raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var version int
			if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version >= schemaVersion {
				raw.Close()
				t.Skipf("%s is already user_version %d", id, version)
			}
			// A v3 file is row-keyed and has no band table, so there is no
			// pre-migration key arithmetic to read it by; what is verified for
			// those is the cell POPULATION and the values, keyed the way the
			// file itself keys them.
			// A pre-v4 file has no `bands` table at all, so this is allowed
			// to fail: there is no key arithmetic to read such a file by, and
			// what is verified for it is the cell population and the values.
			bi, _ := loadBandIndex(raw)
			keyed := bi != nil
			wasRows := 0
			if keyed {
				wasRows = bi.rows
			}
			type before struct {
				computed string
				style    int
			}
			was := map[CellRef]before{}
			cols := "k, col, computed"
			if version < 4 {
				cols = `"row", col, computed`
			}
			if version >= 6 {
				cols += ", style"
			}
			rows, err := raw.Query(`SELECT ` + cols + ` FROM cells`)
			if err != nil {
				raw.Close()
				t.Fatal(err)
			}
			for rows.Next() {
				var k int64
				var col int
				var b before
				var scanErr error
				if version >= 6 {
					scanErr = rows.Scan(&k, &col, &b.computed, &b.style)
				} else {
					scanErr = rows.Scan(&k, &col, &b.computed)
				}
				if scanErr != nil {
					t.Fatal(scanErr)
				}
				r := int(k)
				if keyed {
					r = bi.rankOf(k)
				}
				if r >= 0 {
					was[CellRef{Row: r, Col: col}] = b
				}
			}
			rows.Close()
			wasWidths := map[int]int{}
			wrows, err := raw.Query(`SELECT col, width FROM cols`)
			if err == nil {
				for wrows.Next() {
					var col, w int
					if err := wrows.Scan(&col, &w); err != nil {
						t.Fatal(err)
					}
					wasWidths[col] = w
				}
				wrows.Close()
			}
			raw.Close()

			start := time.Now()
			sh := mustOpen(t, c, id)
			took := time.Since(start)

			if keyed && sh.Rows() != wasRows {
				t.Fatalf("extent %d -> %d", wasRows, sh.Rows())
			}
			bad := 0
			for ref, b := range was {
				got, err := sh.GetCell(ref)
				if err != nil {
					t.Fatalf("GetCell %s: %v", ref, err)
				}
				if got.Computed != b.computed || got.Style != b.style || got.Effective != b.style {
					bad++
					if bad < 4 {
						t.Errorf("%s: computed %q -> %q, style %d -> %d (effective %d)",
							ref, b.computed, got.Computed, b.style, got.Style, got.Effective)
					}
				}
			}
			gotWidths, err := sh.ColWidths()
			if err != nil {
				t.Fatalf("ColWidths: %v", err)
			}
			for col, w := range wasWidths {
				if gotWidths[col] != w {
					t.Errorf("column %d width %d -> %d", col, w, gotWidths[col])
				}
			}
			if got := sh.ColStyles(); len(got) != 0 {
				t.Errorf("the migration invented column styles: %v", got)
			}
			if got, err := sh.RowStyles(0, sh.Rows()-1); err != nil || len(got) != 0 {
				t.Errorf("the migration invented row styles: %v (%v)", got, err)
			}
			t.Logf("%s: v%d -> v%d, %d cells, %d rows, %d widths, migrated in %v, %d mismatches",
				id, version, schemaVersion, len(was), sh.Rows(), len(wasWidths),
				took.Round(time.Millisecond), bad)
		})
	}
}

// TestCellCanOverrideALevelBackToPlain is the hole a record-level cascade has
// and a per-cell design does not: style 0 means "ask the level above me", so it
// cannot also mean "I have deliberately chosen to look like nothing". Bold a
// row, un-bold one cell in it, and without internForce the cell would silently
// stay bold — the store insisting the command worked while the screen disagrees.
func TestCellCanOverrideALevelBackToPlain(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "override")

	mustSetRowStyle(t, sh, []int{4}, StylePatch{Bold: Set(true)})
	rowID := rowStyleMap(t, sh)[4]

	// A neighbouring cell inherits the row's bold, as it should.
	if _, eff := effectiveOf(t, sh, CellRef{Row: 4, Col: 1}); eff != rowID {
		t.Fatalf("a cell in the bold row resolved to %d, want the row's %d", eff, rowID)
	}

	mustSetStyle(t, sh, []CellRef{{Row: 4, Col: 0}}, StylePatch{Bold: Set(false)})
	own, eff := effectiveOf(t, sh, CellRef{Row: 4, Col: 0})
	if own == 0 {
		t.Fatalf("un-bolding a cell in a bold row left it with no style of its own, "+
			"so it still resolves to the row (%d)", rowID)
	}
	if eff != own {
		t.Fatalf("the cell's own style %d did not win: effective %d", own, eff)
	}
	if got := sh.StyleByID(own); got.Bold {
		t.Fatalf("the override is still bold: %+v", got)
	}
	if !sh.StyleByID(own).IsDefault() {
		t.Fatalf("the override should be the default tuple, got %+v", sh.StyleByID(own))
	}
	// It must be a real rule in the stylesheet, and CSSFull is what makes it an
	// override rather than an empty block.
	var found bool
	for _, r := range sh.StyleRules() {
		if r.ID == own {
			found = true
			if r.Style.CSS() != "" {
				t.Fatalf("the default tuple's CSS() is %q", r.Style.CSS())
			}
			if !strings.Contains(r.Style.CSSFull(), "font-weight:400") {
				t.Fatalf("CSSFull does not turn bold off: %q", r.Style.CSSFull())
			}
		}
	}
	if !found {
		t.Fatalf("the override style %d is not in StyleRules, so the render layer "+
			"has no rule to emit for it", own)
	}

	// It survives a reopen: loadStyleTable must not drop a stored default tuple.
	dir := filepath.Dir(sh.Path())
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	c2 := NewSheetCache(dir, 4, time.Minute)
	defer c2.Close()
	sh2 := mustOpen(t, c2, "override")
	if _, eff := effectiveOf(t, sh2, CellRef{Row: 4, Col: 0}); eff != own {
		t.Fatalf("after reopen the override resolved to %d, want %d", eff, own)
	}
	if sh2.StyleByID(own) != DefaultStyle {
		t.Fatalf("after reopen the override is %+v", sh2.StyleByID(own))
	}

	// And clearing the cell gives the row back, which is the other verb.
	if _, err := sh2.ClearStyle([]CellRef{{Row: 4, Col: 0}}); err != nil {
		t.Fatalf("ClearStyle: %v", err)
	}
	if _, eff := effectiveOf(t, sh2, CellRef{Row: 4, Col: 0}); eff != rowID {
		t.Fatalf("after ClearStyle the cell resolved to %d, want the row's %d", eff, rowID)
	}
}

// TestNoLevelMeansNoDefaultStyleRow: the override above must NOT happen on a
// cell with nothing above it, or every "un-bold an unbolded cell" would leave a
// dead rule in the page shell forever.
func TestNoLevelMeansNoDefaultStyleRow(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "nodefaultrow")

	mustSetStyle(t, sh, []CellRef{{Row: 0, Col: 0}}, StylePatch{Bold: Set(true)})
	mustSetStyle(t, sh, []CellRef{{Row: 0, Col: 0}}, StylePatch{Bold: Set(false)})
	own, eff := effectiveOf(t, sh, CellRef{Row: 0, Col: 0})
	if own != 0 || eff != 0 {
		t.Fatalf("un-bolding a cell with no level above it left style %d (effective %d)",
			own, eff)
	}
	for _, r := range sh.StyleRules() {
		if r.Style.IsDefault() {
			t.Fatalf("the style table grew a row for the default tuple: %+v", r)
		}
	}
}

func TestCSSFullStatesEveryProperty(t *testing.T) {
	cases := []struct {
		s    Style
		want string
	}{
		{Style{}, "font-weight:400;font-style:normal;color:inherit;background:transparent;" + noWrapCSS[:len(noWrapCSS)-1]},
		{Style{Bold: true}, "font-weight:700;font-style:normal;color:inherit;background:transparent;" + noWrapCSS[:len(noWrapCSS)-1]},
		{Style{FG: "#c00000", Align: AlignRight},
			"font-weight:400;font-style:normal;color:#c00000;background:transparent;text-align:right;" + noWrapCSS[:len(noWrapCSS)-1]},
		// Wrap is the property a cell most needs to be able to state in both
		// directions: a wrapped column with one cell forced back to a single
		// line is the whole reason CSSFull exists.
		{Style{Wrap: true},
			"font-weight:400;font-style:normal;color:inherit;background:transparent;" + wrapCSS[:len(wrapCSS)-1]},
	}
	for _, tc := range cases {
		if got := tc.s.CSSFull(); got != tc.want {
			t.Errorf("%+v.CSSFull() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// A v7 file's styles table has six columns and a six-column uniqueness
// constraint. Adding wrap to the record without widening that index would make
// interning hand back the id of the unwrapped style that matches on the other
// six — the sheet would accept the command, report success, and change nothing.
//
// The fixture is built by taking the current schema apart rather than by
// restating v7's DDL, so it cannot drift into describing a version that never
// shipped.
func TestMigratesV7StylesToWrap(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "v7wrap")
	mustSetStyle(t, sh, []CellRef{{Row: 0, Col: 0}}, StylePatch{Bold: Set(true)})
	path := sh.path
	c.Close()

	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP INDEX styles_tuple`,
		`ALTER TABLE styles DROP COLUMN wrap`,
		`CREATE UNIQUE INDEX styles_tuple ON styles (bold, italic, fg, bg, align, numfmt)`,
		`PRAGMA user_version = 7`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := newTestCache(t, 4, time.Minute)
	c2.dir = c.dir
	sh2 := mustOpen(t, c2, "v7wrap")

	var v int
	if err := sh2.use(func(db *sql.DB) error {
		return db.QueryRow(`PRAGMA user_version`).Scan(&v)
	}); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version after migration = %d, want %d", v, schemaVersion)
	}

	// The v7 style is intact and still unwrapped.
	_, eff := effectiveOf(t, sh2, CellRef{Row: 0, Col: 0})
	if st := sh2.StyleByID(eff); !st.Bold || st.Wrap {
		t.Fatalf("the v7 style did not survive: %+v", st)
	}
	// And its wrapped twin is a different row, which is the whole point of the
	// rebuilt index.
	mustSetStyle(t, sh2, []CellRef{{Row: 1, Col: 0}}, StylePatch{Bold: Set(true), Wrap: Set(true)})
	_, eff2 := effectiveOf(t, sh2, CellRef{Row: 1, Col: 0})
	if eff2 == eff {
		t.Fatal("a wrapped style interned onto the unwrapped one: the tuple index was not widened")
	}
	if st := sh2.StyleByID(eff2); !st.Bold || !st.Wrap {
		t.Fatalf("the wrapped style did not round-trip: %+v", st)
	}
}
