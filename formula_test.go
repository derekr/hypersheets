package main

import (
	"errors"
	"reflect"
	"testing"
)

// lookupFrom builds a CellLookup over an A1-keyed map of computed values. A
// missing key is an empty cell, which is a normal state, not a miss.
func lookupFrom(t *testing.T, m map[string]string) CellLookup {
	t.Helper()
	cells := make(map[CellRef]Cell, len(m))
	for k, v := range m {
		ref, err := ParseRef(k)
		if err != nil {
			t.Fatalf("lookupFrom: bad key %q: %v", k, err)
		}
		kind := InferKind(v)
		if IsErrorToken(v) {
			kind = KindError
		}
		cells[ref] = Cell{Ref: ref, Raw: v, Computed: v, Display: v, Kind: kind}
	}
	return func(ref CellRef) (Cell, error) {
		if c, ok := cells[ref]; ok {
			return c, nil
		}
		return Cell{Ref: ref}, nil
	}
}

func mustRef(t *testing.T, s string) CellRef {
	t.Helper()
	r, err := ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return r
}

// ─── The scanner ──────────────────────────────────────────────────────────────

func TestParseFormulaShapes(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		kind   FormulaKind
		op     byte
		left   string // "" for a number operand
		leftN  float64
		right  string
		rightN float64
		lo, hi string // for SUM
	}{
		{name: "ref times literal", in: "=A1*2", kind: FormulaBinary, op: '*', left: "A1", rightN: 2},
		{name: "ref plus ref", in: "=A1+B2", kind: FormulaBinary, op: '+', left: "A1", right: "B2"},
		{name: "minus", in: "=C3-D4", kind: FormulaBinary, op: '-', left: "C3", right: "D4"},
		{name: "divide", in: "=A1/B1", kind: FormulaBinary, op: '/', left: "A1", right: "B1"},
		{name: "literal on the left", in: "=100-A1", kind: FormulaBinary, op: '-', leftN: 100, right: "A1"},
		{name: "two literals", in: "=2*3", kind: FormulaBinary, op: '*', leftN: 2, rightN: 3},
		{name: "negative literal operand", in: "=A1*-2", kind: FormulaBinary, op: '*', left: "A1", rightN: -2},
		{name: "leading sign is not an operator", in: "=-5+A1", kind: FormulaBinary, op: '+', leftN: -5, right: "A1"},
		{name: "exponent sign is not an operator", in: "=1e-3+1", kind: FormulaBinary, op: '+', leftN: 0.001, rightN: 1},
		{name: "whitespace tolerated", in: " = A1 * 2 ", kind: FormulaBinary, op: '*', left: "A1", rightN: 2},
		{name: "lowercase ref", in: "=a1*2", kind: FormulaBinary, op: '*', left: "A1", rightN: 2},
		{name: "bare ref", in: "=A1", kind: FormulaValue, left: "A1"},
		{name: "bare number", in: "=42", kind: FormulaValue, leftN: 42},
		{name: "sum", in: "=SUM(A1:A10)", kind: FormulaSum, lo: "A1", hi: "A10"},
		{name: "sum lowercase", in: "=sum(a1:a10)", kind: FormulaSum, lo: "A1", hi: "A10"},
		{name: "sum rectangle", in: "=SUM(A1:C3)", kind: FormulaSum, lo: "A1", hi: "C3"},
		{name: "sum reversed corners normalize", in: "=SUM(C3:A1)", kind: FormulaSum, lo: "A1", hi: "C3"},
		{name: "sum single cell", in: "=SUM(B2:B2)", kind: FormulaSum, lo: "B2", hi: "B2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseFormula(tc.in)
			if err != nil {
				t.Fatalf("ParseFormula(%q): %v", tc.in, err)
			}
			if f.Kind != tc.kind {
				t.Fatalf("ParseFormula(%q).Kind = %v, want %v", tc.in, f.Kind, tc.kind)
			}
			switch tc.kind {
			case FormulaSum:
				if f.Lo != mustRef(t, tc.lo) || f.Hi != mustRef(t, tc.hi) {
					t.Fatalf("ParseFormula(%q) range = %v:%v, want %s:%s",
						tc.in, f.Lo, f.Hi, tc.lo, tc.hi)
				}
			default:
				if tc.kind == FormulaBinary && f.Op != tc.op {
					t.Errorf("ParseFormula(%q).Op = %q, want %q", tc.in, f.Op, tc.op)
				}
				checkOperand(t, tc.in, "left", f.Left, tc.left, tc.leftN)
				if tc.kind == FormulaBinary {
					checkOperand(t, tc.in, "right", f.Right, tc.right, tc.rightN)
				}
			}
		})
	}
}

func checkOperand(t *testing.T, in, side string, got Operand, wantRef string, wantNum float64) {
	t.Helper()
	if wantRef != "" {
		if !got.IsRef {
			t.Errorf("ParseFormula(%q) %s operand = literal %v, want ref %s", in, side, got.Num, wantRef)
			return
		}
		if got.Ref != mustRef(t, wantRef) {
			t.Errorf("ParseFormula(%q) %s operand = %v, want %s", in, side, got.Ref, wantRef)
		}
		return
	}
	if got.IsRef {
		t.Errorf("ParseFormula(%q) %s operand = ref %v, want literal %v", in, side, got.Ref, wantNum)
		return
	}
	if got.Num != wantNum {
		t.Errorf("ParseFormula(%q) %s operand = %v, want %v", in, side, got.Num, wantNum)
	}
}

func TestParseFormulaErrors(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		token string
	}{
		{name: "not a formula", in: "5", token: TokenErr},
		{name: "bare equals", in: "=", token: TokenErr},
		{name: "equals whitespace", in: "=   ", token: TokenErr},
		{name: "missing right operand", in: "=A1+", token: TokenErr},
		{name: "double operator", in: "=A1**2", token: TokenErr},
		{name: "two operators", in: "=A1+B1+C1", token: TokenErr},
		{name: "three terms with mixed ops", in: "=A1*2+3", token: TokenErr},
		{name: "sum not the whole formula", in: "=SUM(A1:A2)*2", token: TokenErr},
		{name: "sum unclosed", in: "=SUM(A1:A2", token: TokenErr},
		{name: "sum empty", in: "=SUM()", token: TokenErr},
		{name: "parens unsupported", in: "=(A1+B1)*2", token: TokenRef},
		{name: "row zero", in: "=A0*2", token: TokenRef},
		{name: "row past the ceiling", in: "=A1000001*2", token: TokenRef},
		{name: "multi-letter column", in: "=AA1*2", token: TokenRef},
		{name: "cross-sheet ref rejected", in: "=Other!A1*2", token: TokenRef},
		{name: "gibberish operand", in: "=@@*2", token: TokenRef},
		{name: "right operand gibberish", in: "=A1*zz", token: TokenRef},
		{name: "sum with a bad ref", in: "=SUM(A0:A10)", token: TokenRef},
		{name: "sum of a single ref not a range", in: "=SUM(A1)", token: TokenRef},
		{name: "sum range past the ceiling", in: "=SUM(A1:A1000001)", token: TokenRef},
		{name: "unknown function", in: "=AVG(A1:A2)", token: TokenRef},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseFormula(tc.in)
			if err == nil {
				t.Fatalf("ParseFormula(%q) = %+v, want error", tc.in, f)
			}
			if !errors.Is(err, ErrBadFormula) {
				t.Errorf("ParseFormula(%q) error %v does not wrap ErrBadFormula", tc.in, err)
			}
			if got := ErrorToken(err); got != tc.token {
				t.Errorf("ParseFormula(%q) token = %s, want %s (%v)", tc.in, got, tc.token, err)
			}
			// Whatever the shape of the failure, a cell holding it must render
			// as that token and never as a Go error string.
			computed, kind, evalErr := EvalRaw(tc.in, lookupFrom(t, nil))
			if tc.in == "5" { // not a formula at all: a literal, not an error
				return
			}
			if evalErr != nil {
				t.Fatalf("EvalRaw(%q) unexpected error: %v", tc.in, evalErr)
			}
			if computed != tc.token || kind != KindError {
				t.Errorf("EvalRaw(%q) = (%q, %v), want (%s, KindError)", tc.in, computed, kind, tc.token)
			}
		})
	}
}

func TestFormulaRefs(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{in: "=A1*2", want: []string{"A1"}},
		{in: "=2*A1", want: []string{"A1"}},
		{in: "=A1+B2", want: []string{"A1", "B2"}},
		{in: "=A1+A1", want: []string{"A1"}}, // deduped
		{in: "=2*3", want: nil},
		{in: "=A1", want: []string{"A1"}},
		{in: "=7", want: nil},
		{in: "=SUM(A1:A3)", want: []string{"A1", "A2", "A3"}},
		{in: "=SUM(A1:B2)", want: []string{"A1", "B1", "A2", "B2"}},
	}
	for _, tc := range tests {
		f, err := ParseFormula(tc.in)
		if err != nil {
			t.Fatalf("ParseFormula(%q): %v", tc.in, err)
		}
		if got := refStrings(f.Refs()); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseFormula(%q).Refs() = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func refStrings(refs []CellRef) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.String()
	}
	return out
}

// ─── Evaluation ───────────────────────────────────────────────────────────────

func TestEvalRaw(t *testing.T) {
	cells := map[string]string{
		"A1": "10",
		"A2": "2.5",
		"A3": "0",
		"B1": "hello",
		"B2": "-4",
		"C1": TokenDiv0, // an already-errored cell
	}
	tests := []struct {
		name string
		in   string
		want string
		kind Kind
	}{
		{name: "empty cell", in: "", want: "", kind: KindEmpty},
		{name: "number literal passes through verbatim", in: "42", want: "42", kind: KindNumber},
		{name: "number literal keeps its formatting", in: "0.50", want: "0.50", kind: KindNumber},
		{name: "text literal", in: "hello", want: "hello", kind: KindText},
		{name: "text that looks like an error is still text", in: "#REF!", want: "#REF!", kind: KindText},

		{name: "ref times literal", in: "=A1*2", want: "20", kind: KindFormula},
		{name: "ref plus ref", in: "=A1+A2", want: "12.5", kind: KindFormula},
		{name: "ref minus ref", in: "=A1-B2", want: "14", kind: KindFormula},
		{name: "division", in: "=A1/A2", want: "4", kind: KindFormula},
		{name: "bare ref", in: "=A1", want: "10", kind: KindFormula},
		{name: "bare literal", in: "=7", want: "7", kind: KindFormula},
		{name: "empty cell reads as zero", in: "=Z9+A1", want: "10", kind: KindFormula},
		{name: "fractional result", in: "=A2*2", want: "5", kind: KindFormula},

		{name: "sum of a column", in: "=SUM(A1:A3)", want: "12.5", kind: KindFormula},
		{name: "sum skips empty cells", in: "=SUM(Z1:Z9)", want: "0", kind: KindFormula},
		{name: "sum of one cell", in: "=SUM(A1:A1)", want: "10", kind: KindFormula},

		{name: "division by zero", in: "=A1/A3", want: TokenDiv0, kind: KindError},
		{name: "division by a zero literal", in: "=A1/0", want: TokenDiv0, kind: KindError},
		{name: "division by an empty cell", in: "=A1/Z9", want: TokenDiv0, kind: KindError},
		{name: "arithmetic on text", in: "=B1*2", want: TokenValue, kind: KindError},
		{name: "text on the right", in: "=2*B1", want: TokenValue, kind: KindError},
		// MOVED, DELIBERATELY: this used to be #VALUE!. A range with a text
		// header over a column of numbers is the normal shape of a sheet, and
		// Excel and Sheets both skip the text. See TestSumSkipsWhatItCannotAdd.
		{name: "text inside a sum is skipped", in: "=SUM(B1:B2)", want: "-4", kind: KindFormula},
		{name: "bad ref", in: "=A0*2", want: TokenRef, kind: KindError},
		{name: "syntax error", in: "=A1**2", want: TokenErr, kind: KindError},

		// Error propagation: reading an errored cell errors with the SAME token.
		{name: "propagates through arithmetic", in: "=C1*2", want: TokenDiv0, kind: KindError},
		{name: "propagates through sum", in: "=SUM(C1:C1)", want: TokenDiv0, kind: KindError},
	}
	look := lookupFrom(t, cells)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, kind, err := EvalRaw(tc.in, look)
			if err != nil {
				t.Fatalf("EvalRaw(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.want || kind != tc.kind {
				t.Errorf("EvalRaw(%q) = (%q, %v), want (%q, %v)", tc.in, got, kind, tc.want, tc.kind)
			}
		})
	}
}

func TestEvalPropagatesEveryToken(t *testing.T) {
	for _, tok := range []string{TokenRef, TokenDiv0, TokenValue, TokenCycle, TokenErr} {
		look := lookupFrom(t, map[string]string{"A1": tok})
		for _, form := range []string{"=A1*2", "=2+A1", "=SUM(A1:A2)", "=A1"} {
			got, kind, err := EvalRaw(form, look)
			if err != nil {
				t.Fatalf("EvalRaw(%q) with A1=%s: %v", form, tok, err)
			}
			if got != tok || kind != KindError {
				t.Errorf("EvalRaw(%q) with A1=%s = (%q, %v), want (%s, KindError)",
					form, tok, got, kind, tok)
			}
		}
	}
}

// A lookup failure is the ONE thing that is a Go error rather than a token: the
// database being unreachable is not a property of the spreadsheet.
func TestEvalLookupFailureIsAnError(t *testing.T) {
	boom := errors.New("db exploded")
	look := func(CellRef) (Cell, error) { return Cell{}, boom }
	for _, form := range []string{"=A1*2", "=SUM(A1:A5)", "=A1"} {
		if _, _, err := EvalRaw(form, look); !errors.Is(err, boom) {
			t.Errorf("EvalRaw(%q) error = %v, want %v", form, err, boom)
		}
	}
}

// ─── A bare reference is a pass-through, whatever the value's type ────────────
//
// `=A1` is the most basic thing a spreadsheet does and it used to fail for every
// non-numeric cell: the evaluator's value type was float64, so every reference
// was coerced with ParseFloat and text could only ever be #VALUE!. What it
// returns now is the referenced cell's value WITH ITS TYPE — and the kind of the
// cell holding the formula is KindFormula either way, because the kind describes
// the cell, not the type its value landed on.
func TestBareRefPassesTheValueThrough(t *testing.T) {
	look := lookupFrom(t, map[string]string{
		"A1": "hello",
		"A2": "5",
		"A3": "0.50",    // a number whose text is not its canonical form
		"A4": "  pad  ", // text with whitespace either side
		"B1": TokenDiv0, // an errored cell
		"B2": "#NAME?",  // TEXT that merely looks like a token
	})
	tests := []struct {
		name string
		in   string
		want string
		kind Kind
	}{
		{name: "text", in: "=A1", want: "hello", kind: KindFormula},
		{name: "number", in: "=A2", want: "5", kind: KindFormula},
		{name: "number is renormalized, not echoed", in: "=A3", want: "0.5", kind: KindFormula},
		{name: "text keeps its whitespace verbatim", in: "=A4", want: "  pad  ", kind: KindFormula},

		// THE DOCUMENTED CHOICE: a bare ref to a blank renders 0, which is what
		// Excel and Sheets both render, and what `=A9*2` has always done here.
		{name: "empty cell reads as zero", in: "=Z9", want: "0", kind: KindFormula},

		{name: "error propagates as that error", in: "=B1", want: TokenDiv0, kind: KindError},
		{name: "an unknown #token is only text", in: "=B2", want: "#NAME?", kind: KindFormula},

		// The blank rule has to be the SAME rule arithmetic uses, or `=A1` is
		// the one formula in the language that disagrees about what a blank is.
		{name: "arithmetic on a blank agrees", in: "=Z9*2", want: "0", kind: KindFormula},
		{name: "blank plus a number agrees", in: "=Z9+A2", want: "5", kind: KindFormula},

		// A lowercase ref is the same reference.
		{name: "lowercase ref", in: "=a1", want: "hello", kind: KindFormula},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, kind, err := EvalRaw(tc.in, look)
			if err != nil {
				t.Fatalf("EvalRaw(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.want || kind != tc.kind {
				t.Errorf("EvalRaw(%q) = (%q, %v), want (%q, %v)", tc.in, got, kind, tc.want, tc.kind)
			}
		})
	}
}

// Arithmetic on text stays #VALUE!, and that is not an oversight the pass-through
// forgot to fix — it is the rule. This grammar has no string concatenation
// (SPEC.md), so `=A1+B1` over two text cells is a user error and not a join.
func TestArithmeticOnTextIsStillAnError(t *testing.T) {
	look := lookupFrom(t, map[string]string{
		"A1": "hello", "B1": "world", "C1": "5",
	})
	for _, in := range []string{
		"=A1*2", "=2*A1", "=A1+B1", "=A1-C1", "=C1-A1", "=A1/2", "=2/A1", "=A1+A1",
	} {
		got, kind, err := EvalRaw(in, look)
		if err != nil {
			t.Fatalf("EvalRaw(%q): %v", in, err)
		}
		if got != TokenValue || kind != KindError {
			t.Errorf("EvalRaw(%q) = (%q, %v), want (%s, error)", in, got, kind, TokenValue)
		}
	}
}

// ─── SUM skips what it cannot add, and propagates what it must not ────────────
//
// The damaging half of the bug: a header row above a column of numbers is the
// normal shape of a sheet, and `=SUM(A1:A10)` over it used to be #VALUE! —
// which broke every SUM anybody would actually write. Excel and Sheets both
// skip text and blanks.
//
// An error inside the range is NOT skipped. A blank is a cell with nothing to
// contribute; an error is a cell whose contribution is UNKNOWN, and summing
// around it would report a total that is confidently wrong. Sheets propagates
// #REF!/#DIV/0! through SUM and so does this.
func TestSumSkipsWhatItCannotAdd(t *testing.T) {
	look := lookupFrom(t, map[string]string{
		"A1": "Header", // the text header that used to poison the column
		"A2": "1",
		"A3": "2",
		"A4": "", // written blank
		"A5": "3",

		"B1": "alpha",
		"B2": "beta",

		"C1": "1",
		"C2": TokenDiv0,
		"C3": "2",

		"D1": "2",
		"D2": TokenCycle,

		"E1": "-4",
		"E2": "  ", // whitespace only: blank as far as a value is concerned
	})
	tests := []struct {
		name string
		in   string
		want string
		kind Kind
	}{
		{name: "text header is skipped", in: "=SUM(A1:A5)", want: "6", kind: KindFormula},
		{name: "blanks inside the range are skipped", in: "=SUM(A1:A10)", want: "6", kind: KindFormula},
		{name: "all text sums to zero", in: "=SUM(B1:B2)", want: "0", kind: KindFormula},
		{name: "all blank sums to zero", in: "=SUM(Y1:Y9)", want: "0", kind: KindFormula},
		{name: "whitespace-only is a blank", in: "=SUM(E1:E2)", want: "-4", kind: KindFormula},
		{name: "an error inside propagates", in: "=SUM(C1:C3)", want: TokenDiv0, kind: KindError},
		{name: "a cycle inside propagates", in: "=SUM(D1:D2)", want: TokenCycle, kind: KindError},
		{name: "text and an error: the error wins", in: "=SUM(B1:C3)", want: TokenDiv0, kind: KindError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, kind, err := EvalRaw(tc.in, look)
			if err != nil {
				t.Fatalf("EvalRaw(%q): %v", tc.in, err)
			}
			if got != tc.want || kind != tc.kind {
				t.Errorf("EvalRaw(%q) = (%q, %v), want (%q, %v)", tc.in, got, kind, tc.want, tc.kind)
			}
		})
	}
}

// A text value has to survive being read back out of a formula cell, or the
// pass-through only works one hop deep. `=A1` produces a KindFormula cell whose
// computed value is text; `=B1` over THAT must see text again, not a number and
// not an error.
func TestTextPassesThroughTwoHops(t *testing.T) {
	look := lookupFrom(t, map[string]string{"A1": "hello"})

	b1, kind, err := EvalRaw("=A1", look)
	if err != nil {
		t.Fatalf("hop 1: %v", err)
	}
	if b1 != "hello" || kind != KindFormula {
		t.Fatalf("hop 1 = (%q, %v), want (\"hello\", formula)", b1, kind)
	}

	// B1 as recalc would have stored it: raw is the formula, computed is text,
	// kind is formula.
	chain := func(ref CellRef) (Cell, error) {
		if ref == mustRef(t, "B1") {
			return Cell{Ref: ref, Raw: "=A1", Computed: b1, Kind: kind}, nil
		}
		return look(ref)
	}
	c1, kind, err := EvalRaw("=B1", chain)
	if err != nil {
		t.Fatalf("hop 2: %v", err)
	}
	if c1 != "hello" || kind != KindFormula {
		t.Errorf("hop 2 = (%q, %v), want (\"hello\", formula)", c1, kind)
	}
	// And arithmetic two hops down still refuses it.
	got, kind, err := EvalRaw("=B1*2", chain)
	if err != nil {
		t.Fatalf("hop 2 arithmetic: %v", err)
	}
	if got != TokenValue || kind != KindError {
		t.Errorf("EvalRaw(\"=B1*2\") = (%q, %v), want (%s, error)", got, kind, TokenValue)
	}
}

// BenchmarkEvalHotPath pins the cost of the variant. eval runs once per cell of
// every SUM inside every recalc, so the value type must not allocate: it is a
// struct by value (float64 + string header + tag), copied in registers, with no
// interface and no pointer anywhere on the path. 0 allocs/op is the assertion —
// if this ever reports otherwise, something started boxing.
func BenchmarkEvalHotPath(b *testing.B) {
	cells := make(map[CellRef]Cell, 64)
	for r := 0; r < 50; r++ {
		cells[CellRef{Row: r, Col: 0}] = Cell{
			Ref: CellRef{Row: r, Col: 0}, Computed: fmtNum(float64(r)), Kind: KindNumber,
		}
	}
	// A whole column of labels, so a SUM across it pays whatever a text cell
	// costs FIFTY times — which is where a per-cell allocation would show up.
	for r := 0; r < 50; r++ {
		cells[CellRef{Row: r, Col: 1}] = Cell{
			Ref: CellRef{Row: r, Col: 1}, Computed: "Header", Kind: KindText,
		}
	}
	look := func(ref CellRef) (Cell, error) {
		if c, ok := cells[ref]; ok {
			return c, nil
		}
		return Cell{Ref: ref}, nil
	}
	for _, raw := range []string{"=A1", "=B1", "=A1*2", "=A1+A2", "=SUM(A1:A50)", "=SUM(B1:B50)"} {
		f, err := ParseFormula(raw)
		if err != nil {
			b.Fatalf("parse %q: %v", raw, err)
		}
		b.Run(raw, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := EvalFormula(f, look); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestIsErrorToken(t *testing.T) {
	for _, tok := range []string{TokenRef, TokenDiv0, TokenValue, TokenCycle, TokenErr} {
		if !IsErrorToken(tok) {
			t.Errorf("IsErrorToken(%q) = false, want true", tok)
		}
	}
	for _, s := range []string{"", "0", "#REF", "REF!", "hello", "#NAME?"} {
		if IsErrorToken(s) {
			t.Errorf("IsErrorToken(%q) = true, want false", s)
		}
	}
}

// ─── Templates ────────────────────────────────────────────────────────────────

// A formula's stored form is a template plus slot coordinates; its DISPLAY form
// is what comes back out. Round-tripping with the slots untouched must return
// the user's bytes exactly — whitespace, case and all — because that text is
// what the editor puts back in front of them.
func TestFormulaTemplateRoundTrip(t *testing.T) {
	tests := []struct {
		raw       string
		wantSlots []string
		wantSpan  bool
		wantOK    bool
	}{
		{"=A1*2", []string{"A1"}, false, true},
		{"= A1 * 2 ", []string{"A1"}, false, true},
		{"=a1*2", []string{"A1"}, false, true},
		{"=A1+B2", []string{"A1", "B2"}, false, true},
		{"=U7+B7", []string{"U7", "B7"}, false, true},
		{"=A1", []string{"A1"}, false, true},
		{"=7", nil, false, false},
		{"=1e-3+1", nil, false, false},
		{"=SUM(A1:A10)", []string{"A1", "A10"}, true, true},
		{"=sum( a1 : a10 )", []string{"A1", "A10"}, true, true},
		{"=SUM(A10:A1)", []string{"A1", "A10"}, true, true}, // reversed, still lo-then-hi
		{"=SUM(#REF!)", nil, false, false},
		{"=#REF!*2", nil, false, false},
		{"123", nil, false, false},
		{"", nil, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			tmpl, slots, span, ok := FormulaTemplate(tc.raw)
			if ok != tc.wantOK || span != tc.wantSpan {
				t.Fatalf("FormulaTemplate(%q) = (%q, ok %v, span %v), want ok %v span %v",
					tc.raw, tmpl, ok, span, tc.wantOK, tc.wantSpan)
			}
			if got := refStrings(slots); len(got) != len(tc.wantSlots) {
				t.Fatalf("slots = %v, want %v", got, tc.wantSlots)
			} else {
				for i := range got {
					if got[i] != tc.wantSlots[i] {
						t.Fatalf("slots = %v, want %v", got, tc.wantSlots)
					}
				}
			}
			if back := RenderTemplate(tmpl, slots); back != tc.raw {
				t.Errorf("RenderTemplate(%q, %v) = %q, want the original %q",
					tmpl, refStrings(slots), back, tc.raw)
			}
		})
	}
}

// A slot that MOVED renders in canonical A1 notation; a slot that was destroyed
// renders as #REF!. Both are the whole point of storing the reference apart
// from the text.
func TestRenderTemplateWithMovedSlots(t *testing.T) {
	tests := []struct {
		raw   string
		slots []CellRef
		want  string
	}{
		{"=a1*2", []CellRef{{Row: 5, Col: 0}}, "=A6*2"},
		{"= A1 * 2 ", []CellRef{{Row: 5, Col: 0}}, "= A6 * 2 "},
		{"=A1+B2", []CellRef{{Row: 0, Col: 0}, {Row: 9, Col: 1}}, "=A1+B10"},
		{"=A1*2", []CellRef{{Row: -1, Col: -1}}, "=#REF!*2"},
		{"=SUM(a1:a10)", []CellRef{{Row: 1, Col: 0}, {Row: 10, Col: 0}}, "=SUM(A2:A11)"},
		{"=SUM(A10:A1)", []CellRef{{Row: 1, Col: 0}, {Row: 10, Col: 0}}, "=SUM(A11:A2)"},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			tmpl, _, _, ok := FormulaTemplate(tc.raw)
			if !ok {
				t.Fatalf("FormulaTemplate(%q) did not template", tc.raw)
			}
			if got := RenderTemplate(tmpl, tc.slots); got != tc.want {
				t.Errorf("RenderTemplate(%q, %v) = %q, want %q",
					tmpl, refStrings(tc.slots), got, tc.want)
			}
		})
	}
}

// Braces cannot appear in a formula this grammar accepts, so a hole needs no
// escape — but a brace that is NOT the start of a well-formed hole has to pass
// through untouched rather than eating the rest of the string.
//
// The complement is deliberate and worth stating: `{0|A1}` IS a hole, and a
// hole with no slot behind it renders as its stored text. Nothing calls
// RenderTemplate on a cell with no slots (see Window), so that path is
// unreachable in the store; it is here so the function is total.
func TestRenderTemplatePassesStrayBracesThrough(t *testing.T) {
	for _, s := range []string{"={not a hole}", "{", "}{", "{0", "{|A1}", "{0A1}"} {
		if got := RenderTemplate(s, nil); got != s {
			t.Errorf("RenderTemplate(%q, nil) = %q, want it unchanged", s, got)
		}
	}
	if got := RenderTemplate("a {0|A1} hole", nil); got != "a A1 hole" {
		t.Errorf("a hole with no slot = %q, want its stored text back", got)
	}
}
