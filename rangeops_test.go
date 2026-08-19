package main

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// A FILLED FORMULA MUST MOVE ITS REFERENCES. This is the whole difference
// between a spreadsheet's fill and a block copy, and it is the one part of
// fill/paste that can be wrong without looking wrong.
func TestTranslateFormula(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		dr, dc int
		want   string
	}{
		{"literal number is untouched", "42", 3, 0, "42"},
		{"literal text is untouched", "hello", 3, 0, "hello"},
		{"empty stays empty", "", 3, 0, ""},
		{"zero offset is identity", "=A1*2", 0, 0, "=A1*2"},
		{"fill down one row", "=A1*2", 1, 0, "=A2*2"},
		{"fill down ten rows", "=A1*2", 10, 0, "=A11*2"},
		{"fill right one column", "=A1*2", 0, 1, "=B1*2"},
		{"both axes", "=A1*2", 2, 3, "=D3*2"},
		{"both operands are refs", "=A1+B2", 1, 1, "=B2+C3"},
		{"a literal operand does not move", "=A1+7", 1, 0, "=A2+7"},
		{"a SUM range moves whole", "=SUM(A1:A10)", 1, 0, "=SUM(A2:A11)"},
		{"a SUM range moves sideways", "=SUM(A1:A10)", 0, 2, "=SUM(C1:C10)"},
		{"whitespace and case survive", "=a1 * 2", 1, 0, "=A2 * 2"},
		{"off the top becomes #REF!", "=A1*2", -1, 0, "=#REF!*2"},
		{"off the left becomes #REF!", "=A1*2", 0, -1, "=#REF!*2"},
		// A formula that never parsed has nothing to translate and its text is
		// what its author needs in order to fix it.
		{"an unparseable formula is copied verbatim", "=A1*2*3", 1, 0, "=A1*2*3"},
		{"a bare ref moves", "=C7", 2, 0, "=C9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := translateFormula(c.raw, c.dr, c.dc); got != c.want {
				t.Errorf("translateFormula(%q, %d, %d) = %q, want %q", c.raw, c.dr, c.dc, got, c.want)
			}
		})
	}
}

// A translated formula must still PARSE to the references it claims — a string
// that merely looks right but re-parses to something else would recalc wrong.
func TestTranslatedFormulaReparses(t *testing.T) {
	got := translateFormula("=SUM(A1:A10)", 5, 1)
	f, err := ParseFormula(got)
	if err != nil {
		t.Fatalf("translated %q does not parse: %v", got, err)
	}
	if f.Lo != (CellRef{Row: 5, Col: 1}) || f.Hi != (CellRef{Row: 14, Col: 1}) {
		t.Errorf("%q parsed to %v:%v, want B6:B15", got, f.Lo, f.Hi)
	}
}

func cellsOf(t *testing.T, rows int, put map[string]Cell) []Cell {
	t.Helper()
	out := make([]Cell, rows*MaxCols)
	for r := 0; r < rows; r++ {
		for c := 0; c < MaxCols; c++ {
			out[r*MaxCols+c] = Cell{Ref: CellRef{Row: r, Col: c}}
		}
	}
	for ref, cell := range put {
		r, err := ParseRef(ref)
		if err != nil {
			t.Fatalf("bad ref in fixture: %v", err)
		}
		cell.Ref = r
		out[r.Row*MaxCols+r.Col] = cell
	}
	return out
}

// COUNT IS NON-EMPTY, SUM/AVG/MIN/MAX ARE NUMERIC. That split is Sheets' and it
// is the only thing about the aggregate a user will notice being wrong.
func TestSummarize(t *testing.T) {
	num := func(v string) Cell { return Cell{Raw: v, Computed: v, Display: v, Kind: KindNumber} }
	txt := func(v string) Cell { return Cell{Raw: v, Computed: v, Display: v, Kind: KindText} }

	cells := cellsOf(t, 5, map[string]Cell{
		"A1": num("10"),
		"B1": txt("label"),
		"A2": num("-4"),
		"B2": {Raw: "=A1*2", Computed: "20", Display: "20", Kind: KindFormula},
		"A3": {Raw: "=1/0", Computed: "#DIV0!", Display: "#DIV0!", Kind: KindError},
		"B4": num("2.5"),
	})
	lo, hi := CellRef{0, 0}, CellRef{4, 1} // A1:B5

	a := summarize(cells, lo, hi)
	if a.Count != 6 {
		t.Errorf("Count = %d, want 6 (every non-empty cell)", a.Count)
	}
	if a.Nums != 4 {
		t.Errorf("Nums = %d, want 4 (text and error excluded)", a.Nums)
	}
	if a.Sum != 28.5 {
		t.Errorf("Sum = %v, want 28.5", a.Sum)
	}
	if a.Min != -4 || a.Max != 20 {
		t.Errorf("Min/Max = %v/%v, want -4/20", a.Min, a.Max)
	}
	if got, want := a.String(), "Sum 28.5 · Avg 7.125 · Count 6 · Min -4 · Max 20"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// An empty rectangle says NOTHING, not "Sum 0" — zero is a claim about data and
// there is none. A rectangle of only text has a count and no arithmetic.
func TestSummarizeDegenerate(t *testing.T) {
	empty := summarize(cellsOf(t, 3, nil), CellRef{0, 0}, CellRef{2, 2})
	if got := empty.String(); got != "" {
		t.Errorf("an empty range summarised as %q, want the empty string", got)
	}
	only := summarize(cellsOf(t, 2, map[string]Cell{
		"A1": {Raw: "x", Computed: "x", Display: "x", Kind: KindText},
		"A2": {Raw: "y", Computed: "y", Display: "y", Kind: KindText},
	}), CellRef{0, 0}, CellRef{1, 0})
	if got, want := only.String(), "Count 2"; got != want {
		t.Errorf("a text-only range summarised as %q, want %q", got, want)
	}
}

// A short read (the range runs past the sheet's extent) must contribute the rows
// it has rather than panicking on the ones it does not.
func TestSummarizeShortWindow(t *testing.T) {
	cells := cellsOf(t, 2, map[string]Cell{"A1": {Raw: "3", Computed: "3", Display: "3", Kind: KindNumber}})
	a := summarize(cells, CellRef{0, 0}, CellRef{99, 0})
	if a.Count != 1 || a.Sum != 3 {
		t.Errorf("short window summarised as %+v, want one cell summing 3", a)
	}
}

func TestFmtAgg(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{0, "0"},
		{42, "42"},
		{-7, "-7"},
		{2.5, "2.5"},
		{1.0 / 3.0, "0.3333333333"},
	}
	for _, c := range cases {
		if got := fmtAgg(c.v); got != c.want {
			t.Errorf("fmtAgg(%v) = %q, want %q", c.v, got, c.want)
		}
	}
	if got := fmtAgg(math.Trunc(1e16)); strings.Contains(got, "e") == false && len(got) > 20 {
		t.Errorf("fmtAgg on a huge value produced %q", got)
	}
}

// THE CAP IS ONE NUMBER FOR ALL THREE LARGE-RANGE WRITES. A fill and a paste
// are the same batch through the same write path as a clear, so a separate
// limit for each would be three numbers to keep in step and two of them would
// drift. The number is read from the constant rather than spelled here, so
// moving the cap does not mean editing a string in a test.
func TestWriteCapIsShared(t *testing.T) {
	if maxWriteCells != maxClearCells {
		t.Errorf("maxWriteCells = %d but maxClearCells = %d — the write path is the same batch",
			maxWriteCells, maxClearCells)
	}
	limit := strconv.Itoa(maxWriteCells)
	for _, msg := range []string{fillRefusal, pasteRefusal, clearRefusal} {
		if !strings.Contains(msg, limit) {
			t.Errorf("refusal %q does not name the limit %s", msg, limit)
		}
		// The old refusals explained the cap with "writes every cell one at a
		// time", which stopped being true the moment the batch landed.
		if strings.Contains(msg, "one at a time") {
			t.Errorf("refusal %q still describes the loop the batch replaced", msg)
		}
	}
}

// THE SELECTION MUST NOT LEAK INTO THE STEADY-STATE SIGNAL SET, WHICH IS NOT THE
// SAME CLAIM AS "the selection never leaves the browser".
//
// The test this replaced asserted the stronger thing, and presence made it
// deliberately false: the settled rectangle is committed to the server, because
// a collaborator's cursor has to be drawn by something that knows where it is.
// The property that still holds — and the one every byte measurement in
// RESULTS.md rests on — is that the selection rides on NO request except the one
// whose whole subject it is. It is named in an explicit payload there; the six
// signals stay underscore-prefixed, so Datastar filters them out of every body,
// including the commit's own.
func TestSelectionSignalsStayLocal(t *testing.T) {
	page := pageShell("demo", 0, 3, "", zeroAnchor())
	for _, name := range []string{"_ag", "_cp", "_sar", "_sac", "_sfr", "_sfc"} {
		if !strings.Contains(page, name+":") {
			t.Errorf("signal %s is not declared in the page shell", name)
		}
		if strings.Contains(page, strings.TrimPrefix(name, "_")+":'") &&
			!strings.Contains(page, name+":'") {
			t.Errorf("signal %s appears without its underscore — it would ride on every request", name)
		}
	}
	// Every command that needs a rectangle names it explicitly, so no request
	// the page makes for any other reason carries one.
	for name, post := range map[string]string{
		"clear":     clearPost("demo"),
		"fill":      fillPost("demo"),
		"paste":     pastePost("demo"),
		"selection": selPost("demo"),
	} {
		if !strings.Contains(post, "payload:{") {
			t.Errorf("the %s command has no explicit payload", name)
		}
	}
	// And the two commands the page issues for reasons that are NOT the
	// selection still say nothing about it.
	for name, expr := range map[string]string{
		"viewport": scrollExpr("demo"),
		"cell":     cellPost("demo"),
	} {
		for _, sig := range []string{"_sar", "_sac", "_sfr", "_sfc", "_cp", "_ag"} {
			if strings.Contains(expr, sig) {
				t.Errorf("the %s command mentions %q", name, sig)
			}
		}
	}
}

// EACH OPERATION POSTS FROM AN ELEMENT OF ITS OWN. Datastar keys request
// cancellation on the element, so two commands sharing an issuer abort each
// other — which for a fill mid-write is data loss.
func TestEachRangeOpHasItsOwnIssuer(t *testing.T) {
	page := pageShell("demo", 0, 3, "", zeroAnchor())
	for _, want := range []string{
		`<div id="cl" hidden data-on:` + clearEvent,
		`<div id="fl" hidden data-on:` + fillEvent,
		`<div id="pv" hidden data-on:` + pasteEvent,
		`<div id="` + selIssuerID + `" hidden data-effect=`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page shell is missing %q", want)
		}
	}
	// THE AGGREGATE IS NO LONGER A REQUEST AT ALL. It was the page's only GET
	// besides the stream — a read model parameterized by a range the client sent
	// — and it could therefore only follow the SELECTION, never the DATA. With
	// the rectangle held on the connection it recomputes on the push that
	// carries the edit, so the endpoint and its element are gone.
	if strings.Contains(page, "/agg") {
		t.Error("the aggregate is still a request; it is a read model over server state now")
	}
	// The stream is still the only GET, and it is still issued from #live.
	// Its URL now carries the render-and-subscribe handover (see
	// liveHandoverQuery), so the match is on the prefix.
	if strings.Count(page, "@get") != 1 ||
		!strings.Contains(page, `id="live" hidden data-init="@get('/s/demo/live?`) {
		t.Error("something other than the stream issues a GET")
	}
}
