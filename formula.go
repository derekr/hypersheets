package main

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// formula.go — a deliberately tiny formula language.
//
// SPEC.md scopes this to exactly three shapes, and the scope is the point: the
// prototype is testing dependency-driven invalidation, not expression parsing.
// Anything beyond these is out of scope by design, not by omission:
//
//	<literal>          a number or a string; not a formula at all
//	=A1        =7      a single operand (the degenerate case of the next line)
//	=A1*2      =A1+B2  one binary op (+ - * /) over two refs/number literals
//	=SUM(A1:A10)       one range
//
// So the parser is a hand-written scanner with no tokenizer, no precedence
// table and no recursion. Parentheses and a second operator are out — see
// SPEC.md.
//
// Errors are values, not Go errors. A cell that fails to evaluate stores an
// error token as its computed value and KindError as its kind, so the token
// flows through the dependency graph exactly like a number does: any cell that
// reads an errored cell is itself errored, with the same token, and the whole
// downstream cone lights up without any special-casing in recalc.go.

// Error tokens. These are user-visible strings stored in cells.computed; the
// set is closed so IsErrorToken can recognise a propagated error on the way
// back in.
const (
	// TokenRef marks an operand that is not a number and not a valid cell
	// reference — including a syntactically fine ref that is off the grid.
	TokenRef = "#REF!"
	// TokenDiv0 marks division by zero (an empty divisor counts as zero).
	TokenDiv0 = "#DIV/0!"
	// TokenValue marks arithmetic on a cell holding text, or a result that is
	// not a finite number.
	TokenValue = "#VALUE!"
	// TokenCycle marks a cell that is inside — or downstream of — a
	// dependency cycle. Produced by recalc.go, never by this file.
	TokenCycle = "#CYCLE!"
	// TokenErr marks a structural problem with the formula itself: an empty
	// body, a malformed SUM, more than one operator.
	TokenErr = "#ERR!"
)

// IsErrorToken reports whether a computed value is one of the error tokens.
// This is how error propagation works on the read side: a cell whose computed
// value is a token is not a number, it is that error.
func IsErrorToken(s string) bool {
	switch s {
	case TokenRef, TokenDiv0, TokenValue, TokenCycle, TokenErr:
		return true
	}
	return false
}

// ErrBadFormula is the sentinel every FormulaError wraps.
var ErrBadFormula = errors.New("bad formula")

// FormulaError is a failure that has a user-visible token. It is a Go error so
// that Precedents can report it to a caller, but on the evaluation path it is
// converted straight into a stored value — a bad formula is a state of a cell,
// not a failure of the write.
type FormulaError struct {
	Token string // the value the cell will display
	Msg   string // the developer-facing detail
}

func (e *FormulaError) Error() string { return e.Token + ": " + e.Msg }
func (e *FormulaError) Unwrap() error { return ErrBadFormula }

func badFormula(token, format string, a ...any) *FormulaError {
	return &FormulaError{Token: token, Msg: fmt.Sprintf(format, a...)}
}

// ErrorToken is the token a failed parse or evaluation should display. Errors
// that are not FormulaErrors (an ErrBadRef escaping from ParseRef, or anything
// unexpected) degrade to #ERR! rather than leaking a Go error string into a
// spreadsheet cell.
func ErrorToken(err error) string {
	if err == nil {
		return ""
	}
	var fe *FormulaError
	if errors.As(err, &fe) {
		return fe.Token
	}
	if errors.Is(err, ErrBadRef) {
		return TokenRef
	}
	return TokenErr
}

// FormulaKind is which of the three supported shapes a formula is.
type FormulaKind int

const (
	// FormulaValue is a single operand: `=A1` or `=7`.
	FormulaValue FormulaKind = iota
	// FormulaBinary is `<operand> <op> <operand>`.
	FormulaBinary
	// FormulaSum is `=SUM(A1:A10)`.
	FormulaSum
)

// Operand is one side of a binary formula: either a cell reference or a
// literal number.
type Operand struct {
	IsRef bool
	Ref   CellRef
	Num   float64
	Text  string // the source text, for error messages
	Pos   int    // byte offset of Text in the raw string that was parsed
}

// Slot is one structurally-stored cell reference of a formula, together with
// the byte span its source text occupies in the raw formula. Slots are what
// makes a structural mutation cheap: the reference lives as a pair of integers
// in its own column, so the formula's text never has to be rewritten to move
// it. See FormulaTemplate.
//
// Slot order is by role, not by source position: for a SUM, slot 0 is always
// the range's low corner and slot 1 its high corner, whichever way round the
// user wrote them. Pos then says where that role's text actually sits, so a
// reversed range still round-trips verbatim.
type Slot struct {
	Ref CellRef
	Pos int // byte offset into the raw formula
	Len int // byte length of the source text
}

// Formula is a parsed cell formula. Which fields are meaningful depends on
// Kind: FormulaValue uses Left, FormulaBinary uses Left/Op/Right, FormulaSum
// uses Lo/Hi.
type Formula struct {
	Kind   FormulaKind
	Op     byte // '+', '-', '*' or '/'; only for FormulaBinary
	Left   Operand
	Right  Operand
	Lo, Hi CellRef // inclusive, normalized range corners; only for FormulaSum

	// Slots are the formula's cell references in role order, at most two.
	// Span reports whether they are the endpoints of a range (a SUM) rather
	// than independent operands — the two map differently through a
	// structural mutation, so the distinction has to survive into storage.
	Slots []Slot
	Span  bool
}

// ParseFormula parses raw (which must start with '=') into a Formula. Every
// error it returns is a *FormulaError, so callers can display ErrorToken(err)
// without inspecting the failure further.
func ParseFormula(raw string) (Formula, error) {
	s, base := trimAt(raw, 0)
	if !strings.HasPrefix(s, "=") {
		return Formula{}, badFormula(TokenErr, "%q is not a formula (must start with '=')", raw)
	}
	body, bodyAt := trimAt(s[1:], base+1)
	if body == "" {
		return Formula{}, badFormula(TokenErr, "empty formula")
	}

	// SUM must be the entire formula. `=SUM(A1:A2)*2` is rejected rather than
	// silently truncated — a wrong answer is worse than an error token.
	if len(body) >= 4 && strings.EqualFold(body[:4], "SUM(") {
		if body[len(body)-1] != ')' || len(body) < 5 {
			return Formula{}, badFormula(TokenErr,
				"SUM must be the whole formula and end with ')': %q", raw)
		}
		inner := body[4 : len(body)-1]
		if strings.TrimSpace(inner) == "" {
			return Formula{}, badFormula(TokenErr, "SUM needs a range: %q", raw)
		}
		lo, hi, slots, err := parseRangeSlots(inner, bodyAt+4)
		if err != nil {
			return Formula{}, badFormula(TokenRef, "SUM range %q: %v", inner, err)
		}
		return Formula{Kind: FormulaSum, Lo: lo, Hi: hi, Slots: slots, Span: true}, nil
	}

	if i := findOp(body); i >= 0 {
		left, err := parseOperand(body[:i], bodyAt)
		if err != nil {
			return Formula{}, err
		}
		rest, restAt := trimAt(body[i+1:], bodyAt+i+1)
		if rest == "" {
			return Formula{}, badFormula(TokenErr, "%q is missing its right operand", raw)
		}
		if rest[0] == '*' || rest[0] == '/' {
			return Formula{}, badFormula(TokenErr, "%q has two operators in a row", raw)
		}
		if findOp(rest) >= 0 {
			return Formula{}, badFormula(TokenErr,
				"%q has more than one operator (only one is supported)", raw)
		}
		right, err := parseOperand(rest, restAt)
		if err != nil {
			return Formula{}, err
		}
		f := Formula{Kind: FormulaBinary, Op: body[i], Left: left, Right: right}
		f.Slots = operandSlots(left, right)
		return f, nil
	}

	only, err := parseOperand(body, bodyAt)
	if err != nil {
		return Formula{}, err
	}
	f := Formula{Kind: FormulaValue, Left: only}
	f.Slots = operandSlots(only, Operand{})
	return f, nil
}

// trimAt trims surrounding whitespace from s and reports where the trimmed text
// starts, given that s itself starts at byte offset off in the original raw
// string. Offsets are what let a formula be stored as a template with holes
// punched at exactly the reference tokens.
func trimAt(s string, off int) (string, int) {
	t := strings.TrimSpace(s)
	if t == "" {
		return "", off
	}
	return t, off + strings.Index(s, t)
}

// operandSlots collects the reference operands of a non-SUM formula, in source
// order. A literal operand contributes no slot: it cannot move.
func operandSlots(l, r Operand) []Slot {
	var out []Slot
	if l.IsRef {
		out = append(out, Slot{Ref: l.Ref, Pos: l.Pos, Len: len(l.Text)})
	}
	if r.IsRef {
		out = append(out, Slot{Ref: r.Ref, Pos: r.Pos, Len: len(r.Text)})
	}
	return out
}

// parseRangeSlots parses "A1:C3" the way ParseRangeBounds does, and additionally
// reports where each endpoint's text lives so the range can be templated.
//
// Slot 0 is the low corner and slot 1 the high corner, but each keeps the
// position of the endpoint that actually spells it — so `SUM(C3:A1)` stores
// lo=A1/hi=C3 while its template still reads `C3:A1` and round-trips verbatim.
// A range written across the anti-diagonal (`C1:A3`) spells neither corner, so
// there the positions are assigned left-to-right and the text normalizes.
func parseRangeSlots(s string, off int) (lo, hi CellRef, slots []Slot, err error) {
	t, tAt := trimAt(s, off)
	i := strings.Index(t, ":")
	if i < 0 {
		return CellRef{}, CellRef{}, nil, fmt.Errorf("%w: %q is not a range (want A1:A10)", ErrBadRef, s)
	}
	aTxt, aAt := trimAt(t[:i], tAt)
	bTxt, bAt := trimAt(t[i+1:], tAt+i+1)
	a, err := ParseRef(aTxt)
	if err != nil {
		return CellRef{}, CellRef{}, nil, fmt.Errorf("range %q start: %w", s, err)
	}
	b, err := ParseRef(bTxt)
	if err != nil {
		return CellRef{}, CellRef{}, nil, fmt.Errorf("range %q end: %w", s, err)
	}
	lo = CellRef{Row: min(a.Row, b.Row), Col: min(a.Col, b.Col)}
	hi = CellRef{Row: max(a.Row, b.Row), Col: max(a.Col, b.Col)}
	if n := (hi.Row - lo.Row + 1) * (hi.Col - lo.Col + 1); n > maxRangeCells {
		return CellRef{}, CellRef{}, nil, fmt.Errorf("%w: range %q spans %d cells (max %d)",
			ErrBadRef, s, n, maxRangeCells)
	}
	loSlot := Slot{Ref: lo, Pos: aAt, Len: len(aTxt)}
	hiSlot := Slot{Ref: hi, Pos: bAt, Len: len(bTxt)}
	if b == lo && a != lo {
		loSlot.Pos, loSlot.Len = bAt, len(bTxt)
		hiSlot.Pos, hiSlot.Len = aAt, len(aTxt)
	}
	return lo, hi, []Slot{loSlot, hiSlot}, nil
}

// findOp returns the index of the binary operator in s, or -1.
//
// Two positions are deliberately not operators: index 0, because a leading
// '-'/'+' is the sign of the literal that follows; and a '+'/'-' immediately
// after an 'e' or 'E', because that is the exponent sign in `1e-3`. Without
// the second rule `=1e-3+1` would split in the wrong place and report a bogus
// bad reference.
func findOp(s string) int {
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c != '+' && c != '-' && c != '*' && c != '/' {
			continue
		}
		if (c == '+' || c == '-') && (s[i-1] == 'e' || s[i-1] == 'E') {
			continue
		}
		return i
	}
	return -1
}

// parseOperand reads one operand: a number literal, or an A1-notation ref.
// Numbers are tried first because no valid ref parses as a float. off is where
// s begins in the raw formula, so the operand can report its own position.
func parseOperand(s string, off int) (Operand, error) {
	t, at := trimAt(s, off)
	if t == "" {
		return Operand{}, badFormula(TokenErr, "missing operand")
	}
	if v, err := strconv.ParseFloat(t, 64); err == nil {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Operand{}, badFormula(TokenValue, "%q is not a finite number", t)
		}
		return Operand{Num: v, Text: t, Pos: at}, nil
	}
	ref, err := ParseRef(t)
	if err != nil {
		return Operand{}, badFormula(TokenRef, "%q is neither a number nor a cell reference", t)
	}
	return Operand{IsRef: true, Ref: ref, Text: t, Pos: at}, nil
}

// Refs returns the cells this formula reads, deduplicated, in a stable order.
// For a SUM that is every cell of the range: the empty ones are real
// dependencies too, because filling one later must dirty the SUM.
func (f Formula) Refs() []CellRef {
	switch f.Kind {
	case FormulaSum:
		n := (f.Hi.Row - f.Lo.Row + 1) * (f.Hi.Col - f.Lo.Col + 1)
		out := make([]CellRef, 0, n)
		for r := f.Lo.Row; r <= f.Hi.Row; r++ {
			for c := f.Lo.Col; c <= f.Hi.Col; c++ {
				out = append(out, CellRef{Row: r, Col: c})
			}
		}
		return out
	case FormulaBinary:
		var out []CellRef
		if f.Left.IsRef {
			out = append(out, f.Left.Ref)
		}
		if f.Right.IsRef && (!f.Left.IsRef || f.Right.Ref != f.Left.Ref) {
			out = append(out, f.Right.Ref)
		}
		return out
	default:
		if f.Left.IsRef {
			return []CellRef{f.Left.Ref}
		}
		return nil
	}
}

// Templates: references out of the text, into columns.
//
// A row insert moves every reference in the sheet, and if a reference is A1 text
// inside cells.raw, moving it means re-reading, re-parsing, re-rendering and
// re-writing every formula — all to produce text that says the same thing about
// a cell that did not change value. So a formula is stored split: the literal
// text with its reference tokens replaced by holes (the template), and the
// references themselves as plain integer columns on the cells row. A uniform
// shift is then arithmetic on those columns, riding along on the row shift the
// clustered primary key already forces, and no formula text is touched.
//
// A hole is `{i|text}`: i is the slot index, text is exactly what the user typed
// there. Keeping the original text buys verbatim round-tripping, including case
// (`=a1*2`) and whitespace (`= A1 * 2`) — if the slot still points where its
// text says it does, the text is emitted unchanged.
//
// '{' and '}' need no escape because no formula that parses can contain them —
// this grammar is refs, numbers, four operators and SUM — and FormulaTemplate
// only templates formulas that parse.

// tmplOpen and tmplClose delimit a hole in a stored formula template.
const (
	tmplOpen  = '{'
	tmplClose = '}'
)

// FormulaTemplate splits a raw formula into the text to store and the slot
// coordinates to store beside it.
//
// ok is false when raw is not a formula that parses — an already-#REF!'d cell,
// a typo. Those are stored verbatim with no slots, which is exactly right:
// there is nothing in them to move, and rewriting them would destroy the text
// the user needs in order to fix them.
func FormulaTemplate(raw string) (tmpl string, slots []CellRef, span, ok bool) {
	if InferKind(raw) != KindFormula {
		return raw, nil, false, false // a literal has nothing to template
	}
	f, err := ParseFormula(raw)
	if err != nil || len(f.Slots) == 0 {
		return raw, nil, false, false
	}
	// Emit holes in source order; the index inside each hole carries the role,
	// so a reversed range keeps its text and still stores lo before hi.
	order := make([]int, len(f.Slots))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return f.Slots[order[a]].Pos < f.Slots[order[b]].Pos
	})

	var sb strings.Builder
	sb.Grow(len(raw) + 4*len(f.Slots))
	at := 0
	for _, i := range order {
		s := f.Slots[i]
		if s.Pos < at || s.Pos+s.Len > len(raw) {
			return raw, nil, false, false // offsets disagree with the text: store it as-is
		}
		sb.WriteString(raw[at:s.Pos])
		sb.WriteByte(tmplOpen)
		sb.WriteByte(byte('0' + i))
		sb.WriteByte('|')
		sb.WriteString(raw[s.Pos : s.Pos+s.Len])
		sb.WriteByte(tmplClose)
		at = s.Pos + s.Len
	}
	sb.WriteString(raw[at:])

	slots = make([]CellRef, len(f.Slots))
	for i, s := range f.Slots {
		slots[i] = s.Ref
	}
	return sb.String(), slots, f.Span, true
}

// RenderTemplate is the inverse: the A1 text a user should see, built from the
// stored template and the current slot coordinates.
//
// A slot whose coordinates still spell what its stored text says comes back
// byte-for-byte as it was typed. A slot that moved comes back in canonical A1
// notation, and a slot that was destroyed comes back as #REF! — CellRef.String
// already renders an off-grid ref that way, so a dead slot needs no special
// case here.
//
// A template with no holes (a literal, or a formula that never parsed) is
// returned untouched and costs one byte scan.
func RenderTemplate(tmpl string, slots []CellRef) string {
	i := strings.IndexByte(tmpl, tmplOpen)
	if i < 0 {
		return tmpl
	}
	var sb strings.Builder
	sb.Grow(len(tmpl))
	for {
		sb.WriteString(tmpl[:i])
		rest := tmpl[i:]
		end := strings.IndexByte(rest, tmplClose)
		// A hole is `{d|...}`: 4 bytes of frame at minimum.
		if end < 3 || rest[2] != '|' || rest[1] < '0' || rest[1] > '9' {
			sb.WriteByte(tmplOpen) // not a hole; pass the brace through
			tmpl = rest[1:]
		} else {
			slot := int(rest[1] - '0')
			text := rest[3:end]
			if slot < len(slots) {
				if want := slots[slot].String(); !strings.EqualFold(text, want) {
					text = want
				}
			}
			sb.WriteString(text)
			tmpl = rest[end+1:]
		}
		if i = strings.IndexByte(tmpl, tmplOpen); i < 0 {
			sb.WriteString(tmpl)
			return sb.String()
		}
	}
}

// CellLookup resolves a cell to its current value. recalc.go supplies one that
// reads from a batched snapshot with the in-flight results layered on top; a
// test can supply a map. A non-nil error means the lookup failed (the database
// is unreachable) — a cell that merely holds nonsense is not an error here, it
// is a #VALUE!.
type CellLookup func(CellRef) (Cell, error)

// EvalRaw evaluates a cell's raw contents to the (computed, kind) pair that
// belongs in the database. Literals pass through unchanged: the computed value
// of a literal is what the user typed, byte for byte, which is what makes
// "did the rendered output change" a plain string comparison.
//
// A malformed or failing formula is not an error return: it returns an error
// token and KindError. The error return is reserved for a failing lookup.
func EvalRaw(raw string, look CellLookup) (string, Kind, error) {
	switch InferKind(raw) {
	case KindEmpty:
		return "", KindEmpty, nil
	case KindNumber:
		return raw, KindNumber, nil
	case KindText:
		return raw, KindText, nil
	}
	f, err := ParseFormula(raw)
	if err != nil {
		return ErrorToken(err), KindError, nil
	}
	return EvalFormula(f, look)
}

// evalValue is what an evaluation produces. A spreadsheet value is not a
// number: an evaluator working in float64 coerces every reference through
// ParseFloat, which makes `=A1` over a header cell a #VALUE! when the most basic
// thing a spreadsheet does is hand back what the other cell holds.
//
// It is a two-case variant and not one case more. Errors stay Go errors carrying
// a token (see FormulaError), which is already how a failure travels out of eval
// into the stored cell; a third case here would give every operator two ways to
// be wrong. Blank is not a fourth case either — a blank read is `ok == false`
// from refValue, and each caller applies its own rule (zero for arithmetic,
// skipped for SUM), so those rules stay stated once each rather than smeared
// over the type.
//
// A plain struct by value: copied in registers, no interface, no pointer,
// nothing allocated per cell. This runs once per cell of every SUM inside every
// recalc — BenchmarkEvalHotPath pins it at 0 allocs/op.
type evalValue struct {
	text   string  // meaningful only when isText
	num    float64 // meaningful only when !isText
	isText bool
}

func numValue(f float64) evalValue { return evalValue{num: f} }
func textValue(s string) evalValue { return evalValue{text: s, isText: true} }
func (v evalValue) isNum() bool    { return !v.isText }

// EvalFormula evaluates an already-parsed formula. recalc.go uses this so a
// formula is parsed once per recalc rather than once per read of it.
//
// A text result is still KindFormula: the kind says what the cell is — the
// renderer paints a formula blue and puts the source in `data-r` on that basis
// — not what type its value happened to land on. `=A1` over a text cell is a
// working formula whose value is text, so it is KindFormula holding "hello",
// exactly as `=A1` over a number is KindFormula holding "10".
func EvalFormula(f Formula, look CellLookup) (string, Kind, error) {
	v, err := f.eval(look)
	if err != nil {
		var fe *FormulaError
		if errors.As(err, &fe) {
			return fe.Token, KindError, nil
		}
		return "", KindError, err // lookup failure: the caller's problem
	}
	if v.isText {
		return v.text, KindFormula, nil
	}
	if math.IsNaN(v.num) || math.IsInf(v.num, 0) {
		return TokenValue, KindError, nil
	}
	return fmtNum(v.num), KindFormula, nil
}

func (f Formula) eval(look CellLookup) (evalValue, error) {
	switch f.Kind {
	case FormulaValue:
		// A bare `=A1` is the one shape that does no arithmetic, so it is the
		// one shape that must not demand a number: whatever A1 holds is what
		// this cell holds.
		return operandValue(f.Left, look)

	case FormulaBinary:
		l, err := operandNumber(f.Left, look)
		if err != nil {
			return evalValue{}, err
		}
		r, err := operandNumber(f.Right, look)
		if err != nil {
			return evalValue{}, err
		}
		switch f.Op {
		case '+':
			return numValue(l + r), nil
		case '-':
			return numValue(l - r), nil
		case '*':
			return numValue(l * r), nil
		case '/':
			if r == 0 {
				return evalValue{}, badFormula(TokenDiv0, "division by zero")
			}
			return numValue(l / r), nil
		}
		return evalValue{}, badFormula(TokenErr, "unknown operator %q", string(f.Op))

	case FormulaSum:
		// SUM skips what it cannot add, matching every other spreadsheet: a
		// column of numbers under a text header is the normal shape of a sheet,
		// and a SUM that errors on it is wrong about every real column.
		//
		// Text and blanks are skipped; an error is propagated. A blank is a cell
		// with nothing to contribute, but an error is a cell whose contribution
		// is unknown, and summing around it would report a total that is
		// confidently wrong. refValue draws exactly that line.
		var sum float64
		for r := f.Lo.Row; r <= f.Hi.Row; r++ {
			for c := f.Lo.Col; c <= f.Hi.Col; c++ {
				v, ok, err := refValue(CellRef{Row: r, Col: c}, look)
				if err != nil {
					return evalValue{}, err
				}
				if ok && v.isNum() {
					sum += v.num
				}
			}
		}
		return numValue(sum), nil
	}
	return evalValue{}, badFormula(TokenErr, "unknown formula kind %d", f.Kind)
}

// operandValue reads one operand without demanding anything of its type.
//
// An empty cell reads as zero, so `=A1` over a blank renders `0` — what Excel
// and Sheets both render, and what `=A1*2` and `=A1+B1` do here. Rendering
// blank instead would put a bare ref at odds with every other formula in the
// language about what a blank is, and would make `=A1` the only formula whose
// result is indistinguishable from an empty cell.
func operandValue(o Operand, look CellLookup) (evalValue, error) {
	if !o.IsRef {
		return numValue(o.Num), nil
	}
	v, ok, err := refValue(o.Ref, look)
	if err != nil {
		return evalValue{}, err
	}
	if !ok {
		return numValue(0), nil // an empty cell reads as zero
	}
	return v, nil
}

// operandNumber is operandValue where a number is required — the two sides of a
// binary op. Text is #VALUE! here: this grammar has no string concatenation
// (SPEC.md), so `=A1+B1` over two text cells is a user error, not a join.
func operandNumber(o Operand, look CellLookup) (float64, error) {
	v, err := operandValue(o, look)
	if err != nil {
		return 0, err
	}
	if v.isText {
		return 0, badFormula(TokenValue, "%s holds text (%q)", o.Text, v.text)
	}
	return v.num, nil
}

// refValue reads one cell as an evaluator value. ok is false for an empty cell
// — the one thing evalValue cannot represent, because every caller has its own
// rule for a blank (zero for arithmetic, skipped for SUM) and encoding the rule
// in the type would pick one of them for everybody.
//
// An error token in the cell is returned as a Go error carrying that token.
// This is the whole of error propagation: a precedent holding a token becomes
// this cell's token too, with no special case anywhere in recalc.
func refValue(ref CellRef, look CellLookup) (v evalValue, ok bool, err error) {
	if !ref.Valid() {
		return evalValue{}, false, badFormula(TokenRef, "%v is off the grid", ref)
	}
	c, lerr := look(ref)
	if lerr != nil {
		return evalValue{}, false, lerr
	}
	t := strings.TrimSpace(c.Computed)
	if t == "" {
		return evalValue{}, false, nil
	}
	if IsErrorToken(t) {
		return evalValue{}, false, badFormula(t, "%s is %s", ref, t)
	}
	if !looksNumeric(t) {
		// Text passes through verbatim — the stored bytes, not the trimmed
		// ones, because `=A1` hands back what A1 holds.
		return textValue(c.Computed), true, nil
	}
	f, perr := strconv.ParseFloat(t, 64)
	if perr != nil {
		return textValue(c.Computed), true, nil
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return evalValue{}, false, badFormula(TokenValue, "%s is not a finite number (%q)", ref, t)
	}
	return numValue(f), true, nil
}

// looksNumeric is a one-byte reject for "this cannot be a number". It exists
// because strconv.ParseFloat allocates when it fails — a *NumError plus a clone
// of the offending string, 2 allocations per call — and SUM walks past text
// rather than failing on it, so a range of a hundred labels would allocate two
// hundred times to learn a hundred times that a label is not a number.
//
// The filter is deliberately loose: everything it lets through still goes to
// ParseFloat, which remains the only thing that decides, so it can only be wrong
// by being too permissive, never by calling a number text. The 'i'/'n' cases are
// there because ParseFloat accepts "Inf" and "NaN" and refValue answers #VALUE!
// for them; routing those to the text branch would change an answer.
func looksNumeric(s string) bool {
	switch c := s[0]; {
	case c >= '0' && c <= '9':
		return true
	case c == '-', c == '+', c == '.':
		return true
	case c == 'i', c == 'I', c == 'n', c == 'N':
		return true // Inf / NaN / Infinity, which ParseFloat accepts
	}
	return false
}
