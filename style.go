package main

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// style.go — cell styling and number formats.
//
// A cell does not store its appearance; it stores an integer indexing a
// per-sheet table of distinct (bold, italic, fg, bg, align, numfmt) tuples, the
// way XLSX's `<c r="B5" s="3"/>` indexes a shared style table. A sheet has
// thousands of cells and a dozen distinct looks, so the look stays O(distinct
// styles) while the markup stays O(cells): the render layer emits
// `.s3{font-weight:700;color:#cc0000}` once into the page shell and every cell
// wearing it carries the class `s3` — the same O(config)-in-CSS /
// O(cells)-in-markup rule the column widths follow.
//
// The number format lives in the same record, as XLSX folds it into the same
// `xf`: no caller wants a cell's colour without its format, and splitting them
// would cost a second lookup per styled cell on the read path.
//
// Formatting is a display transform applied in the store on the way out.
// `1234.5` under a currency format is stored as `1234.5` — what SUM has to
// add — and read as `$1,234.50`. Window/GetCell apply it rather than handing
// the renderer a style id to resolve: the format is a function of the style
// table only the store holds, and doing it on the way out makes "never touch
// storage" structural, since nothing writes Display. It lands on Cell.Display,
// never Cell.Computed, so recalc, the golden fixtures and the event log are
// untouched by formatting.
//
// The format set is closed and there is no format-string parser: a parser for
// `#,##0.00_);[Red]\(#,##0.00\)` is a project, not a feature.

// ErrBadStyle rejects a style value that cannot be stored: an unparseable
// colour, an unknown alignment or an unknown number format.
var ErrBadStyle = errors.New("bad cell style")

// Align is horizontal alignment. AlignDefault means "whatever the stylesheet
// says" — for a spreadsheet, numbers right and text left — and is not the same
// as AlignLeft: a cell that has never been aligned must not freeze itself to
// the left the first time someone bolds it.
type Align int

const (
	AlignDefault Align = 0
	AlignLeft    Align = 1
	AlignCenter  Align = 2
	AlignRight   Align = 3
)

func (a Align) String() string {
	switch a {
	case AlignLeft:
		return "left"
	case AlignCenter:
		return "center"
	case AlignRight:
		return "right"
	case AlignDefault:
		return "default"
	}
	return "align(" + strconv.Itoa(int(a)) + ")"
}

func (a Align) valid() bool { return a >= AlignDefault && a <= AlignRight }

// ParseAlign parses the wire spelling of an alignment. "" and "default" both
// mean AlignDefault.
func ParseAlign(s string) (Align, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "default", "0":
		return AlignDefault, nil
	case "left":
		return AlignLeft, nil
	case "center", "centre":
		return AlignCenter, nil
	case "right":
		return AlignRight, nil
	}
	return AlignDefault, fmt.Errorf("%w: alignment %q (want left, center, right or default)",
		ErrBadStyle, s)
}

// NumFmt is the closed set of number formats. A format is a display transform
// over a cell's computed value; the stored value is always the machine one.
// Every format is a no-op on a value that does not parse as a number, which
// keeps `#CYCLE!` from rendering as `$0.00` and keeps a text cell from being
// mangled by a format left over from an earlier value.
type NumFmt int

const (
	// FmtPlain is the computed value verbatim: the look of an unformatted
	// cell.
	FmtPlain NumFmt = 0
	// FmtInteger rounds to a whole number and groups thousands: 1234.5 -> 1,235.
	FmtInteger NumFmt = 1
	// FmtTwoDP is two decimal places with grouping: 1234.5 -> 1,234.50.
	FmtTwoDP NumFmt = 2
	// FmtCurrency is FmtTwoDP with a leading $ inside the sign: -$1,234.50.
	FmtCurrency NumFmt = 3
	// FmtPercent multiplies by 100 and appends %: 0.1234 -> 12.34%.
	FmtPercent NumFmt = 4
	// FmtDate reads the value as a spreadsheet serial day and renders
	// YYYY-MM-DD.
	FmtDate NumFmt = 5
	// FmtDateTime is FmtDate plus HH:MM from the fractional part.
	FmtDateTime NumFmt = 6
)

func (f NumFmt) String() string {
	switch f {
	case FmtPlain:
		return "plain"
	case FmtInteger:
		return "integer"
	case FmtTwoDP:
		return "2dp"
	case FmtCurrency:
		return "currency"
	case FmtPercent:
		return "percent"
	case FmtDate:
		return "date"
	case FmtDateTime:
		return "datetime"
	}
	return "fmt(" + strconv.Itoa(int(f)) + ")"
}

func (f NumFmt) valid() bool { return f >= FmtPlain && f <= FmtDateTime }

// ParseNumFmt parses the wire spelling of a number format.
func ParseNumFmt(s string) (NumFmt, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "plain", "general", "0":
		return FmtPlain, nil
	case "integer", "int":
		return FmtInteger, nil
	case "2dp", "decimal", "number":
		return FmtTwoDP, nil
	case "currency", "money":
		return FmtCurrency, nil
	case "percent", "%":
		return FmtPercent, nil
	case "date":
		return FmtDate, nil
	case "datetime":
		return FmtDateTime, nil
	}
	return FmtPlain, fmt.Errorf(
		"%w: number format %q (want plain, integer, 2dp, currency, percent, date or datetime)",
		ErrBadStyle, s)
}

// Style is one distinct cell appearance. It is comparable, so the dedup the
// whole design rests on is `==` and a map key rather than a hand-written hash.
//
// FG and BG are "" (inherit) or lowercase "#rrggbb", normalized on the way in
// at SetStyle: that makes "#F00", "#ff0000" and "#FF0000" one style row instead
// of three, and keeps arbitrary text out of a string the render layer
// interpolates into a <style> element.
type Style struct {
	Bold   bool
	Italic bool
	FG     string
	BG     string
	Align  Align
	Fmt    NumFmt
}

// DefaultStyle is style id 0: the look a cell has when nobody has styled it. It
// is never stored in the styles table and never emitted as a CSS rule.
var DefaultStyle = Style{}

// IsDefault reports the style that needs no id, no row and no CSS rule.
func (s Style) IsDefault() bool { return s == DefaultStyle }

// validate checks a style can be stored, and returns it normalized.
func (s Style) validate() (Style, error) {
	fg, err := normalizeColor(s.FG)
	if err != nil {
		return Style{}, fmt.Errorf("foreground: %w", err)
	}
	bg, err := normalizeColor(s.BG)
	if err != nil {
		return Style{}, fmt.Errorf("background: %w", err)
	}
	if !s.Align.valid() {
		return Style{}, fmt.Errorf("%w: alignment %d", ErrBadStyle, int(s.Align))
	}
	if !s.Fmt.valid() {
		return Style{}, fmt.Errorf("%w: number format %d", ErrBadStyle, int(s.Fmt))
	}
	s.FG, s.BG = fg, bg
	return s, nil
}

// normalizeColor accepts "" (inherit), "#rgb" and "#rrggbb" in any case and
// returns "" or lowercase "#rrggbb". It is an allowlist, not a sanitizer: the
// render layer writes these bytes into a stylesheet, so anything that is not
// six hex digits behind a '#' is refused here rather than escaped downstream.
func normalizeColor(c string) (string, error) {
	c = strings.TrimSpace(c)
	if c == "" {
		return "", nil
	}
	if c[0] != '#' || (len(c) != 4 && len(c) != 7) {
		return "", fmt.Errorf("%w: colour %q (want #rgb or #rrggbb)", ErrBadStyle, c)
	}
	var sb strings.Builder
	sb.Grow(7)
	sb.WriteByte('#')
	for i := 1; i < len(c); i++ {
		d := c[i]
		switch {
		case d >= '0' && d <= '9':
		case d >= 'a' && d <= 'f':
		case d >= 'A' && d <= 'F':
			d += 'a' - 'A'
		default:
			return "", fmt.Errorf("%w: colour %q (want #rgb or #rrggbb)", ErrBadStyle, c)
		}
		sb.WriteByte(d)
		if len(c) == 4 {
			sb.WriteByte(d) // #f00 -> #ff0000
		}
	}
	return sb.String(), nil
}

// CSS is the declaration body for this style, without the braces or selector:
// what goes inside `.s3{ … }`. Empty for the default style.
//
// The number format contributes nothing: CSS cannot turn 1234.5 into $1,234.50,
// so the format is applied to the text (see Cell.Display) and only the visual
// half of a style becomes a rule. That split is why the two can share one
// record without the render layer having to care.
func (s Style) CSS() string {
	var b strings.Builder
	b.Grow(64)
	if s.Bold {
		b.WriteString("font-weight:700;")
	}
	if s.Italic {
		b.WriteString("font-style:italic;")
	}
	if s.FG != "" {
		b.WriteString("color:")
		b.WriteString(s.FG)
		b.WriteByte(';')
	}
	if s.BG != "" {
		b.WriteString("background:")
		b.WriteString(s.BG)
		b.WriteByte(';')
	}
	switch s.Align {
	case AlignLeft:
		b.WriteString("text-align:left;")
	case AlignCenter:
		b.WriteString("text-align:center;")
	case AlignRight:
		b.WriteString("text-align:right;")
	}
	return strings.TrimSuffix(b.String(), ";")
}

// CSSFull is CSS with every property stated, including the ones this style
// leaves at their default. The render layer must use it for the cell-level
// rules: a cell that overrides a bold row back to plain has a style whose tuple
// is the default (see internForce), and `.s7{}` cannot turn the row's bold off.
// Using it for all three levels is safe and simpler; the extra bytes are
// O(distinct styles).
//
// Alignment is the exception. AlignDefault means "whatever the stylesheet
// says" — numbers right, text left — a rule this file does not know and must
// not overwrite with `text-align:left`. So a full rule states an alignment only
// when there is one, which makes explicitly-default alignment the one property
// a cell cannot use to override a level.
func (s Style) CSSFull() string {
	var b strings.Builder
	b.Grow(96)
	if s.Bold {
		b.WriteString("font-weight:700;")
	} else {
		b.WriteString("font-weight:400;")
	}
	if s.Italic {
		b.WriteString("font-style:italic;")
	} else {
		b.WriteString("font-style:normal;")
	}
	b.WriteString("color:")
	if s.FG != "" {
		b.WriteString(s.FG)
	} else {
		b.WriteString("inherit")
	}
	b.WriteString(";background:")
	if s.BG != "" {
		b.WriteString(s.BG)
	} else {
		b.WriteString("transparent")
	}
	b.WriteByte(';')
	switch s.Align {
	case AlignLeft:
		b.WriteString("text-align:left;")
	case AlignCenter:
		b.WriteString("text-align:center;")
	case AlignRight:
		b.WriteString("text-align:right;")
	}
	return strings.TrimSuffix(b.String(), ";")
}

// StyleClass is the class name the render layer emits for a style id, and the
// selector it emits the rule under. One function so the two cannot drift.
// Style 0 has no class.
func StyleClass(id int) string {
	if id <= 0 {
		return ""
	}
	return "s" + strconv.Itoa(id)
}

// ─── Patches ──────────────────────────────────────────────────────────────────

// StylePatch is a partial style edit: a nil field means "leave this alone".
// "Bold this range" applied to cells that already have colours must keep the
// colours, so SetStyle merges per cell rather than replacing a Style wholesale.
//
// Build one with Set: StylePatch{Bold: Set(true), FG: Set("#c00")}.
type StylePatch struct {
	Bold   *bool
	Italic *bool
	FG     *string
	BG     *string
	Align  *Align
	Fmt    *NumFmt
}

// Set returns a pointer to v, so a StylePatch can be written as a literal.
func Set[T any](v T) *T { return &v }

// Empty reports a patch that would change nothing.
func (p StylePatch) Empty() bool {
	return p.Bold == nil && p.Italic == nil && p.FG == nil && p.BG == nil &&
		p.Align == nil && p.Fmt == nil
}

// apply merges the patch over one cell's existing style.
func (p StylePatch) apply(s Style) Style {
	if p.Bold != nil {
		s.Bold = *p.Bold
	}
	if p.Italic != nil {
		s.Italic = *p.Italic
	}
	if p.FG != nil {
		s.FG = *p.FG
	}
	if p.BG != nil {
		s.BG = *p.BG
	}
	if p.Align != nil {
		s.Align = *p.Align
	}
	if p.Fmt != nil {
		s.Fmt = *p.Fmt
	}
	return s
}

// validate normalizes the patch's own values once, up front, so that a bad
// colour is one error before anything is written rather than N errors
// discovered per cell inside a transaction.
func (p StylePatch) validate() (StylePatch, error) {
	if p.FG != nil {
		c, err := normalizeColor(*p.FG)
		if err != nil {
			return p, fmt.Errorf("foreground: %w", err)
		}
		p.FG = &c
	}
	if p.BG != nil {
		c, err := normalizeColor(*p.BG)
		if err != nil {
			return p, fmt.Errorf("background: %w", err)
		}
		p.BG = &c
	}
	if p.Align != nil && !p.Align.valid() {
		return p, fmt.Errorf("%w: alignment %d", ErrBadStyle, int(*p.Align))
	}
	if p.Fmt != nil && !p.Fmt.valid() {
		return p, fmt.Errorf("%w: number format %d", ErrBadStyle, int(*p.Fmt))
	}
	return p, nil
}

// String describes the patch for the event log.
func (p StylePatch) String() string {
	var parts []string
	if p.Bold != nil {
		parts = append(parts, "bold="+strconv.FormatBool(*p.Bold))
	}
	if p.Italic != nil {
		parts = append(parts, "italic="+strconv.FormatBool(*p.Italic))
	}
	if p.FG != nil {
		parts = append(parts, "fg="+*p.FG)
	}
	if p.BG != nil {
		parts = append(parts, "bg="+*p.BG)
	}
	if p.Align != nil {
		parts = append(parts, "align="+p.Align.String())
	}
	if p.Fmt != nil {
		parts = append(parts, "fmt="+p.Fmt.String())
	}
	if len(parts) == 0 {
		return "(no change)"
	}
	return strings.Join(parts, " ")
}

// ParseStylePatch builds a patch from the wire form the HTTP layer receives: a
// map of field name to value in which an absent key means "leave alone" and an
// empty value means "reset to the default" — the wire spelling of StylePatch's
// pointers, and why signals cannot be a Style.
//
// Recognized keys: bold, italic, fg, bg, align, fmt.
func ParseStylePatch(fields map[string]string) (StylePatch, error) {
	var p StylePatch
	for k, v := range fields {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "bold", "italic":
			b, err := parseStyleBool(v)
			if err != nil {
				return StylePatch{}, fmt.Errorf("%s: %w", k, err)
			}
			if k == "bold" {
				p.Bold = &b
			} else {
				p.Italic = &b
			}
		case "fg", "color", "colour":
			c, err := normalizeColor(v)
			if err != nil {
				return StylePatch{}, err
			}
			p.FG = &c
		case "bg", "background":
			c, err := normalizeColor(v)
			if err != nil {
				return StylePatch{}, err
			}
			p.BG = &c
		case "align":
			a, err := ParseAlign(v)
			if err != nil {
				return StylePatch{}, err
			}
			p.Align = &a
		case "fmt", "format", "numfmt":
			f, err := ParseNumFmt(v)
			if err != nil {
				return StylePatch{}, err
			}
			p.Fmt = &f
		default:
			return StylePatch{}, fmt.Errorf("%w: unknown style field %q", ErrBadStyle, k)
		}
	}
	return p, nil
}

func parseStyleBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "off", "no":
		return false, nil
	case "1", "true", "on", "yes":
		return true, nil
	}
	return false, fmt.Errorf("%w: %q is not a boolean", ErrBadStyle, v)
}

// ─── The display transform ────────────────────────────────────────────────────

// FormatValue applies a number format to a computed value and returns the text
// to display. It is a pure function of (text, format) and the only place a
// value is formatted.
//
// A value that does not parse as a finite number comes back unchanged, which
// makes it safe to run over an entire window without asking what is in each
// cell: text, "" and every error token (#CYCLE!, #REF!, #ERR!) pass through as
// themselves rather than as $0.00.
func FormatValue(v string, f NumFmt) string {
	if f == FmtPlain || v == "" {
		return v
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return v
	}
	switch f {
	case FmtInteger:
		return fixed(n, 0)
	case FmtTwoDP:
		return fixed(n, 2)
	case FmtCurrency:
		// The sign goes outside the symbol: -$1,234.50, not $-1,234.50.
		if n < 0 {
			return "-$" + fixed(-n, 2)
		}
		return "$" + fixed(n, 2)
	case FmtPercent:
		return fixed(n*100, 2) + "%"
	case FmtDate:
		return serialDate(n, "2006-01-02")
	case FmtDateTime:
		return serialDate(n, "2006-01-02 15:04")
	}
	return v
}

// fixed renders n with exactly dp decimal places and thousands separators.
//
// It rounds half away from zero before formatting, because that is what a
// spreadsheet does and Go does not: strconv.FormatFloat rounds half to even, so
// 1234.5 at zero decimals would come out 1,234 while 1235.5 came out 1,236.
// The pre-round is skipped where the scaling would itself lose precision, which
// is where the value has no fractional part left to round.
func fixed(n float64, dp int) string {
	if pow := math.Pow(10, float64(dp)); math.Abs(n)*pow < 1<<52 {
		n = math.Round(n*pow) / pow
	}
	s := strconv.FormatFloat(n, 'f', dp, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, hasFrac := strings.Cut(s, ".")

	var b strings.Builder
	b.Grow(len(s) + len(s)/3 + 1)
	if neg {
		b.WriteByte('-')
	}
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if hasFrac {
		b.WriteByte('.')
		b.WriteString(frac)
	}
	return b.String()
}

// serialEpoch is day 0 of the spreadsheet serial calendar: 1899-12-30, so day 1
// is 1899-12-31 and day 45000 is 2023-03-15 — the Google Sheets and LibreOffice
// mapping, which agrees with Excel for every serial past 60.
//
// It disagrees with Excel below serial 61, deliberately: Excel's day 60 is
// 1900-02-29, a date that does not exist, kept for compatibility with a Lotus
// bug. Reproducing it would mean a leap-year special case in a display
// transform, to render six wrong dates in 1900 the same wrong way.
var serialEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

// serialSpan is the serial range that maps to a renderable year (1900-01-01 to
// 9999-12-31). Outside it the value is not a date, whatever the style says, and
// the number is shown instead of a nonsense year.
const (
	serialMin = 1.0
	serialMax = 2958465.0
)

func serialDate(n float64, layout string) string {
	if n < serialMin || n > serialMax {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	days := math.Floor(n)
	// Round to the minute rather than truncating, so 0.5 of a day is 12:00 and
	// a value one float ulp under noon is not 11:59.
	mins := math.Round((n - days) * 24 * 60)
	return serialEpoch.
		AddDate(0, 0, int(days)).
		Add(time.Duration(mins) * time.Minute).
		Format(layout)
}

// ─── The style table ──────────────────────────────────────────────────────────

const stylesDDL = `
CREATE TABLE IF NOT EXISTS styles (
  id     INTEGER PRIMARY KEY CHECK (id > 0),
  bold   INTEGER NOT NULL DEFAULT 0,
  italic INTEGER NOT NULL DEFAULT 0,
  fg     TEXT    NOT NULL DEFAULT '',
  bg     TEXT    NOT NULL DEFAULT '',
  align  INTEGER NOT NULL DEFAULT 0,
  numfmt INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS styles_tuple
  ON styles (bold, italic, fg, bg, align, numfmt);
`

// styleTable is a sheet's style index, held in memory. It is immutable and
// swapped wholesale under Sheet.idxMu — the band index's lock, not one of its
// own — so a window read takes one lock and gets a consistent (row mapping,
// style table) pair rather than half a change.
//
// A sheet has a handful of distinct looks and gcStyles caps the table at what
// is referenced, so holding it all resident costs a few kilobytes and keeps the
// style lookup off the read path.
type styleTable struct {
	byID  map[int]Style
	byKey map[Style]int
	next  int
}

func newStyleTable() *styleTable {
	return &styleTable{byID: map[int]Style{}, byKey: map[Style]int{}, next: 1}
}

func (t *styleTable) clone() *styleTable {
	n := &styleTable{
		byID:  make(map[int]Style, len(t.byID)+2),
		byKey: make(map[Style]int, len(t.byKey)+2),
		next:  t.next,
	}
	for id, s := range t.byID {
		n.byID[id] = s
		n.byKey[s] = id
	}
	return n
}

// get is the look for a style id. An id the table does not know — only
// possible from a newer snapshot or a corrupt file — reads as the default
// rather than failing a render.
func (t *styleTable) get(id int) Style {
	if id <= 0 {
		return DefaultStyle
	}
	return t.byID[id]
}

// intern returns the id for a style, allocating one if it is new. The default
// style is id 0 and is never allocated.
func (t *styleTable) intern(s Style) (id int, created bool) {
	if s.IsDefault() {
		return 0, false
	}
	if id, ok := t.byKey[s]; ok {
		return id, false
	}
	id = t.next
	t.next++
	t.byID[id] = s
	t.byKey[s] = id
	return id, true
}

// internForce is intern for the one case where the default tuple needs a real
// id: a cell that explicitly overrides a level back to plain. Style 0 means "I
// have no style of my own, ask the level above me", so it cannot also mean "I
// have deliberately chosen to look like nothing". The row it allocates gets a
// CSS rule — empty under CSS(), a complete reset under CSSFull(), which is why
// the render layer must use the latter.
func (t *styleTable) internForce(s Style) (id int, created bool) {
	if !s.IsDefault() {
		return t.intern(s)
	}
	if id, ok := t.byKey[s]; ok {
		return id, false
	}
	id = t.next
	t.next++
	t.byID[id] = s
	t.byKey[s] = id
	return id, true
}

func (t *styleTable) len() int { return len(t.byID) }

// ─── The three-level cascade ──────────────────────────────────────────────────
//
// A cell's look is resolved from three records, as XLSX resolves it from
// `<col style="5"/>`, `<row s="3"/>` and the cell's own `s`:
//
//	effective = cell.style ?: row.style ?: col.style ?: default
//
// The point is the write, not the read: a column style is one record, where
// styling a column cell by cell is 10,000 writes on a default sheet and, on a
// blank column, 10,000 materialized rows. The read pays two integer compares,
// and only on a sheet that has a level style at all.
//
// It is a record-level fallback, not a per-field merge: `effective` is an id
// already in the style table, so the cascade never interns a new tuple and
// never mixes half of one style with half of another. That keeps the id
// resolvable, the CSS class emittable and the format lookup a single map hit.
// The per-field merge a spreadsheet appears to do — "the column is yellow, I
// bolded a cell, it is now yellow and bold" — happens at the command instead:
// SetStyle merges against the cell's effective style, so the cell it writes is
// self-contained. See restyle.

// resolve is the cascade, and it is the only place the precedence is written
// down. Everything above the store asks this question through Cell.Effective.
func resolve(cell, row, col int) int {
	if cell != 0 {
		return cell
	}
	if row != 0 {
		return row
	}
	return col
}

// styleState is everything about a sheet's look held in memory: the table of
// distinct styles, the column level of the cascade, and whether the row level
// exists at all. The three travel together because the read path needs all
// three at once, and like the band index the whole thing is swapped wholesale
// under Sheet.idxMu with the transaction that produced it.
//
// The column level is resident and the row level is not: 26 columns is 26
// integers and an array index, while rows are unbounded and so are read per
// window — one primary-key range scan over `rows`, skipped when anyRow is
// false.
type styleState struct {
	tab *styleTable

	// col is the column level: col[c] is the style id column c carries, 0 for
	// none. anyCol is the fast path — a sheet with no column style pays one
	// bool test per read instead of 26 array reads per row.
	col    [MaxCols]int
	anyCol bool

	// anyRow says the `rows` table holds at least one style. It is a cached
	// predicate, and may only be wrong in one direction: true when the answer
	// is false costs one empty range scan per window, whereas false when the
	// answer is true silently renders a styled row unstyled. Every writer
	// therefore re-probes it rather than reasoning about what it just wrote.
	anyRow bool
}

func newStyleState() *styleState { return &styleState{tab: newStyleTable()} }

// clone copies the state for a writer to modify. The table is cloned deeply
// (the writer interns into it); the two levels are values and copy with the
// struct.
func (st *styleState) clone() *styleState {
	n := *st
	n.tab = st.tab.clone()
	return &n
}

// cascades reports whether this sheet has any level style. When it does not —
// every sheet nobody has formatted by row or column — the read path skips the
// cascade entirely and Cell.Effective is Cell.Style.
func (st *styleState) cascades() bool { return st.anyCol || st.anyRow }

// StyleRule is one row of the style table, ready to be emitted as CSS.
type StyleRule struct {
	ID    int
	Style Style
}

// rules returns the table ascending by id — the order the CSS is emitted in,
// fixed so that two renders of an unchanged sheet produce identical bytes.
func (t *styleTable) rules() []StyleRule {
	out := make([]StyleRule, 0, len(t.byID))
	for id, s := range t.byID {
		out = append(out, StyleRule{ID: id, Style: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func loadStyleTable(q queryer) (*styleTable, error) {
	rows, err := q.Query(
		`SELECT id, bold, italic, fg, bg, align, numfmt FROM styles ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read styles: %w", err)
	}
	defer rows.Close()
	t := newStyleTable()
	for rows.Next() {
		var id, bold, italic, align, numfmt int
		var s Style
		if err := rows.Scan(&id, &bold, &italic, &s.FG, &s.BG, &align, &numfmt); err != nil {
			return nil, fmt.Errorf("scan style: %w", err)
		}
		s.Bold, s.Italic = bold != 0, italic != 0
		s.Align, s.Fmt = Align(align), NumFmt(numfmt)
		// A row whose tuple is the default is kept, not skipped: under a
		// cascade it is a cell saying "I override the level back to plain"
		// (see internForce). Dropping it on reload would leave the cell
		// pointing at an id no longer in the table, which the next sweep
		// would then delete from the database as well.
		if id <= 0 {
			continue // id 0 is implicit and is never a row
		}
		t.byID[id] = s
		t.byKey[s] = id
		if id >= t.next {
			t.next = id + 1
		}
	}
	return t, rows.Err()
}

// loadStyleState reads the table and both cascade levels on open. The row level
// is read as an existence probe rather than a map: it is unbounded, and all the
// read path needs resident is whether to look at all.
func loadStyleState(q queryer) (*styleState, error) {
	tab, err := loadStyleTable(q)
	if err != nil {
		return nil, err
	}
	st := &styleState{tab: tab}
	cols, err := readColStyles(q)
	if err != nil {
		return nil, err
	}
	for c, id := range cols {
		if c < 0 || c >= MaxCols || id <= 0 {
			continue
		}
		st.col[c] = id
		st.anyCol = true
	}
	if st.anyRow, err = anyRowStyle(q); err != nil {
		return nil, err
	}
	return st, nil
}

// readColStyles reads the column level. At most 26 rows, so it is a map and not
// a scan target worth optimizing.
func readColStyles(q queryer) (map[int]int, error) {
	rows, err := q.Query(`SELECT col, style FROM cols WHERE style <> 0`)
	if err != nil {
		return nil, fmt.Errorf("read column styles: %w", err)
	}
	defer rows.Close()
	out := make(map[int]int, 4)
	for rows.Next() {
		var col, id int
		if err := rows.Scan(&col, &id); err != nil {
			return nil, fmt.Errorf("column style scan: %w", err)
		}
		out[col] = id
	}
	return out, rows.Err()
}

// anyRowStyle answers styleState.anyRow: does the sheet carry a row style
// anywhere. One probe on the rows_style partial index, stopping at the first
// hit — a question about whether to run a query, answered for less than the
// query costs.
func anyRowStyle(q queryer) (bool, error) {
	var one int
	err := q.QueryRow(`SELECT 1 FROM rows WHERE style <> 0 LIMIT 1`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe row styles: %w", err)
	}
	return true, nil
}

// readRowStylesFor reads the row level over an inclusive display-row window as
// one ordered primary-key range scan, indexed by `row - loRow`. A nil result
// means no row in the window carries a style, which lets the caller skip the
// cascade entirely.
//
// It is a slice and not a map because the caller indexes it once per cell of
// the window — up to 6,500 times — and a map lookup there would be exactly the
// per-cell probe the placement of cells.style exists to avoid.
func readRowStylesFor(q queryer, bi *bandIndex, loRow, hiRow int) ([]int, error) {
	rows, err := q.Query(
		`SELECT k, style FROM rows WHERE k BETWEEN ? AND ? AND style <> 0`,
		bi.keyOf(loRow), bi.keyOf(hiRow))
	if err != nil {
		return nil, fmt.Errorf("row styles %d..%d: %w", loRow, hiRow, err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var k int64
		var id int
		if err := rows.Scan(&k, &id); err != nil {
			return nil, fmt.Errorf("row style scan: %w", err)
		}
		r := bi.rankOf(k)
		if r < loRow || r > hiRow {
			continue
		}
		if out == nil {
			out = make([]int, hiRow-loRow+1)
		}
		out[r-loRow] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row styles %d..%d: %w", loRow, hiRow, err)
	}
	return out, nil
}

// readRowStylesAt is the row level for an arbitrary set of display rows, keyed
// by display row. It exists beside readRowStylesFor because that form allocates
// a slice as wide as the span it covers — right for a window or a capped
// selection, wrong for SetRowStyle on rows 1 and 1,000,000, a two-element
// request that would allocate eight megabytes. This form is bounded by the
// request: batched `k IN (…)`, at most maxBindsPerStatement keys per round
// trip, each a primary-key seek.
func readRowStylesAt(tx *sql.Tx, bi *bandIndex, rows []int) (map[int]int, error) {
	out := make(map[int]int, len(rows))
	for i := 0; i < len(rows); i += maxBindsPerStatement {
		batch := rows[i:min(i+maxBindsPerStatement, len(rows))]
		args := make([]any, 0, len(batch))
		var q strings.Builder
		q.WriteString(`SELECT k, style FROM rows WHERE style <> 0 AND k IN (`)
		for j, r := range batch {
			if j > 0 {
				q.WriteByte(',')
			}
			q.WriteByte('?')
			args = append(args, bi.keyOf(r))
		}
		q.WriteByte(')')
		res, err := tx.Query(q.String(), args...)
		if err != nil {
			return nil, fmt.Errorf("row styles for %d rows: %w", len(batch), err)
		}
		for res.Next() {
			var k int64
			var id int
			if err := res.Scan(&k, &id); err != nil {
				res.Close()
				return nil, fmt.Errorf("row style scan: %w", err)
			}
			if r := bi.rankOf(k); r >= 0 {
				out[r] = id
			}
		}
		err = res.Err()
		res.Close()
		if err != nil {
			return nil, fmt.Errorf("row styles for %d rows: %w", len(batch), err)
		}
	}
	return out, nil
}

// rowStyleOf is the row level for one display row — what GetCell needs, where a
// range scan would be a scan for a single answer.
func rowStyleOf(q queryer, bi *bandIndex, row int) (int, error) {
	var id int
	err := q.QueryRow(`SELECT style FROM rows WHERE k = ?`, bi.keyOf(row)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("row style of %d: %w", row, err)
	}
	return id, nil
}

func insertStyle(tx *sql.Tx, id int, s Style) error {
	_, err := tx.Exec(
		`INSERT INTO styles (id, bold, italic, fg, bg, align, numfmt)
		   VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, boolInt(s.Bold), boolInt(s.Italic), s.FG, s.BG, int(s.Align), int(s.Fmt))
	if err != nil {
		return fmt.Errorf("insert style %d: %w", id, err)
	}
	return nil
}

// ─── The API ──────────────────────────────────────────────────────────────────

// styleGCLimit is how many style rows a sheet may accumulate before the store
// sweeps the unreferenced ones. See gcStyles for why it is a threshold rather
// than a reference count.
const styleGCLimit = 64

// styles returns the current style state. The caller must hold idxMu — that is,
// must be inside readIndex — because the pointer is only ever swapped under
// idxMu.Lock. Bundling the two under one lock is what lets a window read see a
// consistent (row mapping, style table) pair with one acquisition.
func (s *Sheet) styles() *styleState { return s.sty }

// styleStateNow reads the state for the writer, which is the actor goroutine
// and is the only thing that installs a new one.
func (s *Sheet) styleStateNow() *styleState {
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	return s.sty
}

// StyleRules is the sheet's whole style table, ascending by id. The render
// layer generates the stylesheet from it — one
// `.{StyleClass(r.ID)}{ {r.Style.CSS()} }` rule per entry. It is O(distinct
// styles) and changes only when a style is created or garbage collected, so it
// belongs in the page shell and arrives as a small patch when it moves, like
// the column widths.
func (s *Sheet) StyleRules() []StyleRule {
	var out []StyleRule
	_ = s.readIndex(func(*bandIndex) error {
		out = s.styles().tab.rules()
		return nil
	})
	return out
}

// StyleByID is the look behind one style id, which is what Cell.Style points
// at. An unknown id reads as the default.
func (s *Sheet) StyleByID(id int) Style {
	var out Style
	_ = s.readIndex(func(*bandIndex) error {
		out = s.styles().tab.get(id)
		return nil
	})
	return out
}

// SetStyle merges a partial style patch into every cell in refs and returns the
// dirty set.
//
// It merges per cell against that cell's own style, so bolding a range of mixed
// colours keeps the colours. Cells that end up with the style they already had
// are not written and are not reported dirty.
//
// A styled cell with no content is a real row: `cells` gains a blank row whose
// only content is the style id, because a yellow empty cell is a thing a
// spreadsheet has. That is why "does this row hold anything" includes
// `style <> 0` everywhere it is asked.
//
// Must be called from the sheet's actor goroutine.
func (s *Sheet) SetStyle(refs []CellRef, patch StylePatch) (Dirty, error) {
	patch, err := patch.validate()
	if err != nil {
		return Dirty{}, err
	}
	if len(refs) == 0 || patch.Empty() {
		return Dirty{}, nil
	}
	return s.restyle(refs, patch, false)
}

// ClearStyle resets every cell in refs to the default style. A cell left with
// no content and no style is deleted rather than stored blank, so clearing the
// formatting of an empty range gives the sheet back the rows it took.
//
// Must be called from the sheet's actor goroutine.
func (s *Sheet) ClearStyle(refs []CellRef) (Dirty, error) {
	if len(refs) == 0 {
		return Dirty{}, nil
	}
	return s.restyle(refs, StylePatch{}, true)
}

// restyle is SetStyle and ClearStyle, which differ only in the new style they
// compute per cell and in whether they sweep blank rows afterwards.
func (s *Sheet) restyle(refs []CellRef, patch StylePatch, clear bool) (Dirty, error) {
	need := 0
	for _, r := range refs {
		if !r.Valid() {
			return Dirty{}, fmt.Errorf("%w: %v out of grid", ErrBadRef, r)
		}
		need = max(need, r.Row+1)
	}
	// Styling a cell below the bottom of the sheet grows the sheet, for the same
	// reason writing one does: "make row 15,000 yellow" is a legal thing to ask
	// of a spreadsheet.
	if err := s.ensureRows(need); err != nil {
		return Dirty{}, err
	}

	var out Dirty
	err := s.use(func(db *sql.DB) error {
		// The two snapshots are taken by pointer rather than held under
		// readIndex, the way structural() takes the band index: this runs on
		// the actor goroutine, so nothing else can install a replacement
		// underneath it, and commitStyles takes idxMu.Lock — which, inside
		// readIndex's RLock, is a self-deadlock.
		bi, cur := s.index(), s.styleStateNow()
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin style: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

		next := cur.clone()
		changed, created, err := applyStyle(tx, bi, next, refs, patch, clear)
		if err != nil {
			return err
		}
		if len(changed) == 0 {
			// Nothing moved. Roll back rather than committing an empty
			// transaction, and leave the current state installed rather than
			// swapping in a clone that says the same thing.
			return nil
		}
		if err := writeStyleEvent(tx, "#style", refsExtent(changed), patch, clear); err != nil {
			return err
		}
		for _, c := range created {
			if err := insertStyle(tx, c.ID, c.Style); err != nil {
				return err
			}
		}
		if next.tab.len() > styleGCLimit {
			if err := gcStyles(tx, next.tab); err != nil {
				return err
			}
		}
		if err := s.commitStyles(tx, next); err != nil {
			return fmt.Errorf("commit style: %w", err)
		}
		out = Dirty{Cells: changed, Bands: BandsFor(changed)}
		return nil
	})
	if err != nil {
		return Dirty{}, err
	}
	return out, nil
}

// commitStyles commits a transaction and installs the style state describing
// the sheet it leaves behind, as one step — the same contract commitIndex has,
// under the same lock, so a reader never sees a cell pointing at a style row it
// cannot resolve.
func (s *Sheet) commitStyles(tx *sql.Tx, next *styleState) error {
	return s.commitState(tx, nil, next)
}

// applyStyle does the per-cell merge and the writes, inside the caller's
// transaction. It returns the refs that actually changed and the style rows
// that had to be created.
func applyStyle(tx *sql.Tx, bi *bandIndex, next *styleState, refs []CellRef,
	patch StylePatch, clear bool) ([]CellRef, []StyleRule, error) {

	cur, err := readStyleIDs(tx, bi, refs)
	if err != nil {
		return nil, nil, err
	}
	// The two level assignments the merge resolves against. The column level is
	// resident; the row level is one range scan over the selection, the same
	// shape and bound as the cell read above it.
	loRow, hiRow := refsRowExtent(refs)
	var rowSty []int
	if next.anyRow {
		if rowSty, err = readRowStylesFor(tx, bi, loRow, hiRow); err != nil {
			return nil, nil, err
		}
	}
	rowStyleAt := func(row int) int {
		if rowSty == nil || row < loRow || row > hiRow {
			return 0
		}
		return rowSty[row-loRow]
	}

	changed := make([]CellRef, 0, len(refs))
	target := make([]int, 0, len(refs))
	var created []StyleRule
	seen := make(map[CellRef]bool, len(refs))
	// The merge result is cached per effective style, not per cell: a selection
	// is thousands of cells over a handful of distinct looks, so this is a
	// handful of map lookups instead of thousands of interns.
	//
	// The second half of the key is "is there a level underneath this cell",
	// which decides whether a merge landing on the default tuple needs a real id
	// (internForce) or is plain style 0. Two cells can share an effective style
	// and disagree about that.
	type mergeKey struct {
		base    int
		layered bool
	}
	memo := make(map[mergeKey]int, 8)

	for _, ref := range refs {
		if seen[ref] {
			continue // a caller may hand us overlapping ranges
		}
		seen[ref] = true
		own := cur[ref]
		// The merge is against the effective style, not the cell's own: bolding
		// a cell in a currency-formatted column has to produce a bold currency
		// cell, and merging against the cell's own (absent) style would drop
		// the column's format. It also makes every styled cell self-contained,
		// so the render layer can emit one class per cell and let CSS resolve
		// the levels underneath. See the contract note on Cell.Effective.
		//
		// `under` is what the cell would look like with no style of its own —
		// the levels alone. `base` is what it looks like now.
		under := resolve(0, rowStyleAt(ref.Row), next.col[ref.Col])
		base := resolve(own, under, 0)
		key := mergeKey{base: base, layered: under != 0}
		to, ok := memo[key]
		if !ok {
			if clear {
				to = 0
			} else {
				merged, err := patch.apply(next.tab.get(base)).validate()
				if err != nil {
					return nil, nil, err
				}
				// internForce only when there is a level underneath to
				// override: "un-bold this cell" in a bold row has to produce a
				// real id whose tuple is the default, because style 0 means
				// "ask the level" and would put the bold straight back. With no
				// level under it the default really is 0, and allocating a row
				// for it would be a dead CSS rule in the page shell forever.
				var id int
				var isNew bool
				if under != 0 {
					id, isNew = next.tab.internForce(merged)
				} else {
					id, isNew = next.tab.intern(merged)
				}
				if isNew {
					created = append(created, StyleRule{ID: id, Style: merged})
				}
				to = id
			}
			memo[key] = to
		}
		// Two ways to have nothing to do, and under a cascade they are not the
		// same test:
		//
		//	to == own   the cell's own record already says what we would write
		//	to == base  the cell already looks like that because a level says
		//	            so, so writing it would materialize a cell to repeat
		//	            what the column already said. "Bold a column the column
		//	            level already bolds" has to stay zero writes.
		if to == own || to == base {
			continue
		}
		changed = append(changed, ref)
		target = append(target, to)
	}
	// One batched upsert, not one statement per cell: a selection is thousands
	// of cells, and at ~15 µs a round trip through database/sql that would be a
	// visible pause for work six integers wide.
	//
	// A styled cell that does not exist yet is created blank, because the style
	// has to live somewhere; ON CONFLICT keeps everything else about a cell that
	// does.
	if err := bulkInsert(tx, len(changed), 6,
		`INSERT INTO cells (k, col, raw, computed, kind, style) VALUES `,
		` ON CONFLICT(k, col) DO UPDATE SET style = excluded.style`,
		func(i int, args []any) []any {
			return append(args, bi.keyOf(changed[i].Row), changed[i].Col, "", "", 0, target[i])
		}); err != nil {
		return nil, nil, fmt.Errorf("apply styles: %w", err)
	}
	if clear && len(changed) > 0 {
		if err := dropBlankCells(tx, bi, refs); err != nil {
			return nil, nil, err
		}
	}
	sortRefs(changed)
	return changed, created, nil
}

// readStyleIDs reads the current style id of every ref as one ordered
// primary-key range scan over the keys the refs span, rather than a query per
// cell. A selection is a rectangle, so the span is the selection; a scattered
// set over-reads rows it then ignores, which is still one scan.
func readStyleIDs(tx *sql.Tx, bi *bandIndex, refs []CellRef) (map[CellRef]int, error) {
	loRow, hiRow := refsRowExtent(refs)
	out := make(map[CellRef]int, len(refs))
	rows, err := tx.Query(
		`SELECT k, col, style FROM cells WHERE k BETWEEN ? AND ? AND style <> 0`,
		bi.keyOf(loRow), bi.keyOf(hiRow))
	if err != nil {
		return nil, fmt.Errorf("read styles for %d..%d: %w", loRow, hiRow, err)
	}
	defer rows.Close()
	for rows.Next() {
		var k int64
		var col, id int
		if err := rows.Scan(&k, &col, &id); err != nil {
			return nil, fmt.Errorf("style scan: %w", err)
		}
		if r := bi.rankOf(k); r >= 0 {
			out[CellRef{Row: r, Col: col}] = id
		}
	}
	return out, rows.Err()
}

// dropBlankCells removes rows that a style clear left holding nothing at all,
// bounded to the key range the clear touched. It is what keeps `cells` sparse:
// a styled empty cell has to be a row, so an unstyled empty cell must stop
// being one.
func dropBlankCells(tx *sql.Tx, bi *bandIndex, refs []CellRef) error {
	loRow, hiRow := refsRowExtent(refs)
	if _, err := tx.Exec(
		`DELETE FROM cells
		   WHERE k BETWEEN ? AND ?
		     AND raw = '' AND computed = '' AND kind = 0 AND style = 0`,
		bi.keyOf(loRow), bi.keyOf(hiRow)); err != nil {
		return fmt.Errorf("drop blank cells %d..%d: %w", loRow, hiRow, err)
	}
	return nil
}

// gcStyles deletes style rows nothing points at, and prunes the in-memory table
// to match.
//
// A sweep rather than a reference count: a count would have to be maintained by
// every path that writes a cell, including the shift and the rebalance, which
// re-insert rows wholesale. That is a counter to get wrong in six places for a
// table that holds tens of rows.
//
// Threshold-triggered because the sweep reads every styled cell (the
// cells_style partial index is what bounds it to those rather than the sheet),
// which does not belong on the hot path of a toolbar click. Even if it never
// ran the table would stay bounded by the user's palette, since it grows by at
// most one row per distinct tuple ever applied. The cost of not collecting is
// not disk but CSS: StyleRules goes into the page shell, so a dead style is
// bytes on every first paint.
func gcStyles(tx *sql.Tx, t *styleTable) error {
	live := map[int]bool{}
	// Three sources of a reference, and missing any of them deletes a live
	// style: a look can be worn by a cell, a column or a row, and the point of
	// the cascade is that the last two wear it without any cell mentioning it.
	// A column styled yellow on a sheet with no yellow cell is what a
	// cells-only sweep would collect out from under the CSS.
	//
	// Three statements rather than a UNION so each keeps its own plan: the cells
	// arm is a covering scan of cells_style, the rows arm a covering scan of
	// rows_style, and the cols arm reads a table with at most 26 rows.
	for _, q := range []string{
		`SELECT DISTINCT style FROM cells WHERE style <> 0`,
		`SELECT DISTINCT style FROM rows WHERE style <> 0`,
		`SELECT DISTINCT style FROM cols WHERE style <> 0`,
	} {
		rows, err := tx.Query(q)
		if err != nil {
			return fmt.Errorf("collect live styles: %w", err)
		}
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("live style scan: %w", err)
			}
			live[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("collect live styles: %w", err)
		}
	}
	for id, s := range t.byID {
		if live[id] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM styles WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete style %d: %w", id, err)
		}
		delete(t.byID, id)
		delete(t.byKey, s)
	}
	return nil
}

// ─── The column and row levels ────────────────────────────────────────────────
//
// A column style is one record and a row style is one record. Neither
// materializes a cell, and neither is bounded by what the user selected — which
// is the saving the cascade above exists for.

// SetColStyle merges a partial style patch into every column in cols.
//
// It merges against the column's own current style — per level, not against the
// cascade, since a column has nothing above it to inherit from. An absent field
// is left alone, so "make column D currency" keeps the colour column D had.
//
// The dirty set is the whole grid, as SetColWidth's is: every rendered row
// contains every column. It carries no dirty cells, which is why the render
// layer needs Dirty.Config to know anything happened.
//
// Must be called from the sheet's actor goroutine.
func (s *Sheet) SetColStyle(cols []int, patch StylePatch) (Dirty, error) {
	patch, err := patch.validate()
	if err != nil {
		return Dirty{}, err
	}
	if len(cols) == 0 || patch.Empty() {
		return Dirty{}, nil
	}
	return s.relevelCols(cols, patch, false)
}

// ClearColStyle resets every column in cols to the default look. A column left
// with no style and no explicit width stops being a row in `cols` at all, so
// clearing gives the table back to the sparse state it started in.
//
// Must be called from the sheet's actor goroutine.
func (s *Sheet) ClearColStyle(cols []int) (Dirty, error) {
	if len(cols) == 0 {
		return Dirty{}, nil
	}
	return s.relevelCols(cols, StylePatch{}, true)
}

// relevelCols is SetColStyle and ClearColStyle.
func (s *Sheet) relevelCols(cols []int, patch StylePatch, clear bool) (Dirty, error) {
	for _, c := range cols {
		if c < 0 || c >= MaxCols {
			return Dirty{}, fmt.Errorf("%w: column %d out of 0..%d", ErrBadRef, c, MaxCols-1)
		}
	}
	var out Dirty
	err := s.use(func(db *sql.DB) error {
		cur := s.styleStateNow()
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin column style: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

		next := cur.clone()
		var created []StyleRule
		var touched []int
		seen := [MaxCols]bool{}
		memo := make(map[int]int, 4)
		for _, c := range cols {
			if seen[c] {
				continue
			}
			seen[c] = true
			from := next.col[c]
			to, ok := memo[from]
			if !ok {
				if clear {
					to = 0
				} else {
					merged, err := patch.apply(next.tab.get(from)).validate()
					if err != nil {
						return err
					}
					id, isNew := next.tab.intern(merged)
					if isNew {
						created = append(created, StyleRule{ID: id, Style: merged})
					}
					to = id
				}
				memo[from] = to
			}
			if to == from {
				continue
			}
			next.col[c] = to
			touched = append(touched, c)
		}
		if len(touched) == 0 {
			return nil
		}
		sort.Ints(touched)
		if err := writeStyleEvent(tx, "#colstyle", colsExtent(touched), patch, clear); err != nil {
			return err
		}
		for _, cr := range created {
			if err := insertStyle(tx, cr.ID, cr.Style); err != nil {
				return err
			}
		}
		// A `cols` row exists for a width or for a style, so a column styled
		// for the first time is inserted carrying the default width — which is
		// what an absent row already meant, so no width changes.
		if err := bulkInsert(tx, len(touched), 3,
			`INSERT INTO cols (col, width, style) VALUES `,
			` ON CONFLICT(col) DO UPDATE SET style = excluded.style`,
			func(i int, args []any) []any {
				return append(args, touched[i], DefaultColWidth, next.col[touched[i]])
			}); err != nil {
			return fmt.Errorf("apply column styles: %w", err)
		}
		// And it stops existing when it says nothing: no style, and a width
		// equal to the one an absent row means. Same rule dropBlankCells
		// applies to a cleared cell, on the other axis.
		if _, err := tx.Exec(
			`DELETE FROM cols WHERE style = 0 AND width = ?`, DefaultColWidth); err != nil {
			return fmt.Errorf("drop empty column rows: %w", err)
		}
		next.anyCol = false
		for _, id := range next.col {
			if id != 0 {
				next.anyCol = true
				break
			}
		}
		if next.tab.len() > styleGCLimit {
			if err := gcStyles(tx, next.tab); err != nil {
				return err
			}
		}
		if err := s.commitStyles(tx, next); err != nil {
			return fmt.Errorf("commit column style: %w", err)
		}
		out = Dirty{Bands: s.AllBands(), Config: true}
		return nil
	})
	if err != nil {
		return Dirty{}, err
	}
	return out, nil
}

// SetRowStyle merges a partial style patch into every row in rows.
//
// It is one record per row, keyed by the storage key, so a row style survives
// an insert or a delete the way a cell does: rows that do not move are not
// written, however far their display rank shifts. See rowMetaDDL.
//
// It merges against the row's own current style, per level, as SetColStyle
// does — the row level does not inherit from the column level, and a cell in a
// styled row inside a styled column resolves to the row's style, which is
// XLSX's precedence. Styling a row past the bottom of the sheet grows the
// sheet, for the same reason writing a cell there does.
//
// Must be called from the sheet's actor goroutine.
func (s *Sheet) SetRowStyle(rows []int, patch StylePatch) (Dirty, error) {
	patch, err := patch.validate()
	if err != nil {
		return Dirty{}, err
	}
	if len(rows) == 0 || patch.Empty() {
		return Dirty{}, nil
	}
	return s.relevelRows(rows, patch, false)
}

// ClearRowStyle resets every row in rows to the default look, and deletes the
// row's record when nothing is left to say about it. Cells inside it keep their
// own styles: clearing a level clears the level, not the cells under it.
//
// Must be called from the sheet's actor goroutine.
func (s *Sheet) ClearRowStyle(rows []int) (Dirty, error) {
	if len(rows) == 0 {
		return Dirty{}, nil
	}
	return s.relevelRows(rows, StylePatch{}, true)
}

// relevelRows is SetRowStyle and ClearRowStyle.
func (s *Sheet) relevelRows(rows []int, patch StylePatch, clear bool) (Dirty, error) {
	need := 0
	for _, r := range rows {
		if r < 0 || r >= RowCeiling {
			return Dirty{}, fmt.Errorf("%w: row %d out of 0..%d", ErrBadRef, r, RowCeiling-1)
		}
		need = max(need, r+1)
	}
	if err := s.ensureRows(need); err != nil {
		return Dirty{}, err
	}

	var out Dirty
	err := s.use(func(db *sql.DB) error {
		bi, cur := s.index(), s.styleStateNow()
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin row style: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

		loRow, hiRow := rows[0], rows[0]
		for _, r := range rows[1:] {
			loRow, hiRow = min(loRow, r), max(hiRow, r)
		}
		have, err := readRowStylesAt(tx, bi, rows)
		if err != nil {
			return err
		}

		next := cur.clone()
		var created []StyleRule
		var touched []int
		var target []int
		seen := make(map[int]bool, len(rows))
		memo := make(map[int]int, 4)
		for _, r := range rows {
			if seen[r] {
				continue
			}
			seen[r] = true
			from := have[r]
			to, ok := memo[from]
			if !ok {
				if clear {
					to = 0
				} else {
					merged, err := patch.apply(next.tab.get(from)).validate()
					if err != nil {
						return err
					}
					id, isNew := next.tab.intern(merged)
					if isNew {
						created = append(created, StyleRule{ID: id, Style: merged})
					}
					to = id
				}
				memo[from] = to
			}
			if to == from {
				continue
			}
			touched = append(touched, r)
			target = append(target, to)
		}
		if len(touched) == 0 {
			return nil
		}
		if err := writeStyleEvent(tx, "#rowstyle", rowsExtent(touched), patch, clear); err != nil {
			return err
		}
		for _, cr := range created {
			if err := insertStyle(tx, cr.ID, cr.Style); err != nil {
				return err
			}
		}
		// ON CONFLICT names only `style`, so a row height survives a restyle
		// untouched — the same property that lets a cell edit leave a cell's
		// style alone.
		if err := bulkInsert(tx, len(touched), 3,
			`INSERT INTO rows (k, style, height) VALUES `,
			` ON CONFLICT(k) DO UPDATE SET style = excluded.style`,
			func(i int, args []any) []any {
				return append(args, bi.keyOf(touched[i]), target[i], 0)
			}); err != nil {
			return fmt.Errorf("apply row styles: %w", err)
		}
		if clear {
			if _, err := tx.Exec(
				`DELETE FROM rows WHERE k BETWEEN ? AND ? AND style = 0 AND height = 0`,
				bi.keyOf(loRow), bi.keyOf(hiRow)); err != nil {
				return fmt.Errorf("drop empty row records %d..%d: %w", loRow, hiRow, err)
			}
		}
		// Re-probed, not inferred: the writer knows what it wrote but not what
		// the rest of the table holds, and anyRow being false while a styled
		// row exists renders that row unstyled with no error anywhere. One
		// indexed probe is cheaper than the class of bug it removes.
		if next.anyRow, err = anyRowStyle(tx); err != nil {
			return err
		}
		if next.tab.len() > styleGCLimit {
			if err := gcStyles(tx, next.tab); err != nil {
				return err
			}
		}
		if err := s.commitStyles(tx, next); err != nil {
			return fmt.Errorf("commit row style: %w", err)
		}
		bands := make([]int, 0, len(touched))
		seenBand := make(map[int]struct{}, len(touched))
		for _, r := range touched {
			b := BandOf(r)
			if _, dup := seenBand[b]; dup {
				continue
			}
			seenBand[b] = struct{}{}
			bands = append(bands, b)
		}
		sort.Ints(bands)
		out = Dirty{Bands: bands, Config: true}
		return nil
	})
	if err != nil {
		return Dirty{}, err
	}
	return out, nil
}

// ColStyles is the column level of the cascade, keyed by 0-indexed column;
// columns absent from the map carry no style. The render layer generates the
// column half of the stylesheet from it. It is O(26) and served from memory —
// the assignment is resident beside the style table, under the same lock — so
// it costs no query.
func (s *Sheet) ColStyles() map[int]int {
	out := make(map[int]int, 4)
	_ = s.readIndex(func(*bandIndex) error {
		st := s.styles()
		for c, id := range st.col {
			if id != 0 {
				out[c] = id
			}
		}
		return nil
	})
	return out
}

// RowStyles is the row level of the cascade over an inclusive display-row
// window, keyed by display row; rows absent from the map carry no style. The
// render layer calls it for the row half of the stylesheet, with the window it
// just rendered. It takes a window rather than the whole sheet because the row
// level is unbounded; inside one it is a single primary-key range scan over
// `rows`, skipped entirely when the sheet has no row style.
func (s *Sheet) RowStyles(loRow, hiRow int) (map[int]int, error) {
	if loRow > hiRow {
		loRow, hiRow = hiRow, loRow
	}
	if loRow < 0 {
		loRow = 0
	}
	out := map[int]int{}
	err := s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			if !s.styles().anyRow {
				return nil
			}
			hi := min(hiRow, bi.rows-1)
			if loRow > hi {
				return nil
			}
			ids, err := readRowStylesFor(db, bi, loRow, hi)
			if err != nil {
				return err
			}
			for i, id := range ids {
				if id != 0 {
					out[loRow+i] = id
				}
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// relevelState rebuilds the two cascade levels of a style state from the
// database, leaving the style table alone. It is what a structural mutation
// installs: a shift creates and destroys no look, but it does change which
// column carries which id and whether any row still does.
//
// It re-reads rather than updating the array incrementally, because the
// alternative is applying shiftOp.point to 26 integers in Go and hoping it
// agrees with what shiftColMeta wrote in SQL. The two disagreeing is a whole
// column painted the wrong colour with no error anywhere.
func relevelState(q queryer, cur *styleState) (*styleState, error) {
	next := cur.clone()
	next.col = [MaxCols]int{}
	next.anyCol = false
	cols, err := readColStyles(q)
	if err != nil {
		return nil, err
	}
	for c, id := range cols {
		if c < 0 || c >= MaxCols || id <= 0 {
			continue
		}
		next.col[c] = id
		next.anyCol = true
	}
	if next.anyRow, err = anyRowStyle(q); err != nil {
		return nil, err
	}
	return next, nil
}

// writeStyleEvent appends the log row for a style command. One function for all
// three levels so that "set", "clear" and the patch description cannot drift
// between them.
func writeStyleEvent(tx *sql.Tx, ref, extent string, patch StylePatch, clear bool) error {
	verb, desc := "set", patch.String()
	if clear {
		verb, desc = "clear", ""
	}
	if err := appendEvent(tx, ref,
		strings.TrimSpace(fmt.Sprintf("%s %s %s", verb, extent, desc)), "",
	); err != nil {
		return fmt.Errorf("append %s event: %w", ref, err)
	}
	return nil
}

// colsExtent names a set of columns for the event log: "D" or "B..G(4)".
func colsExtent(cols []int) string {
	if len(cols) == 0 {
		return "(none)"
	}
	lo, hi := cols[0], cols[len(cols)-1]
	if lo == hi {
		return string(rune('A' + lo))
	}
	return fmt.Sprintf("%c..%c(%d)", 'A'+lo, 'A'+hi, len(cols))
}

// rowsExtent names a set of rows for the event log, 1-based as a user sees
// them: "7" or "7..500(12)".
func rowsExtent(rows []int) string {
	if len(rows) == 0 {
		return "(none)"
	}
	lo, hi := rows[0], rows[0]
	for _, r := range rows[1:] {
		lo, hi = min(lo, r), max(hi, r)
	}
	if lo == hi {
		return strconv.Itoa(lo + 1)
	}
	return fmt.Sprintf("%d..%d(%d)", lo+1, hi+1, len(rows))
}

// refsRowExtent is the inclusive display-row span a set of refs covers. Three
// queries in this file are bounded by it — the cell styles, the row styles and
// the blank sweep — so it is one function rather than three copies that have to
// agree.
func refsRowExtent(refs []CellRef) (lo, hi int) {
	lo, hi = refs[0].Row, refs[0].Row
	for _, r := range refs[1:] {
		lo, hi = min(lo, r.Row), max(hi, r.Row)
	}
	return lo, hi
}

// refsExtent describes a set of refs for the event log without printing
// thousands of them: the bounding box, and how many cells are in it.
func refsExtent(refs []CellRef) string {
	if len(refs) == 0 {
		return "(none)"
	}
	lo, hi := refs[0], refs[0]
	for _, r := range refs[1:] {
		lo = CellRef{Row: min(lo.Row, r.Row), Col: min(lo.Col, r.Col)}
		hi = CellRef{Row: max(hi.Row, r.Row), Col: max(hi.Col, r.Col)}
	}
	if lo == hi {
		return lo.String()
	}
	return fmt.Sprintf("%s:%s(%d)", lo, hi, len(refs))
}

// sortRefs orders a ref slice row-major, which is the order every dirty set in
// this codebase is in.
func sortRefs(refs []CellRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Row != refs[j].Row {
			return refs[i].Row < refs[j].Row
		}
		return refs[i].Col < refs[j].Col
	})
}
