package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Grid geometry.
//
// The column axis is fixed at MaxCols. The row axis is not: a sheet's row extent
// grows on demand and is a property of the sheet rather than a compile-time
// constant — see (*Sheet).Rows and bandIndex.rows in bandkey.go.
//
// Everything internal is 0-indexed: CellRef{Row:0, Col:0} is what a user sees as
// "A1". A1 notation is a presentation format only ParseRef/String know about; no
// other file should do arithmetic on letters or 1-based row numbers.
//
// A band is a fixed run of BandHeight rows, and bands are the invalidation and
// subscription granularity: a dirty cell set becomes a band set becomes a set of
// NATS subjects. Band size is a constant rather than a parameter because the
// publisher and the subscriber must agree on it without negotiating.

const (
	// RowCeiling is the hard sanity ceiling on a sheet's row extent. It is the
	// only row limit, and it exists so a typo'd formula or a bad request cannot
	// ask for 2^63 rows and take the process with it.
	//
	// One million because it is ~100x any workload this prototype has, it is the
	// number a spreadsheet user recognizes (Excel stops at 1,048,576), and the
	// storage arithmetic stays comfortable: 1,000,000 rows is 20,000 canonical
	// storage bands of 4,096 slots, so the largest key is ~82 million — six
	// orders of magnitude inside int64 — and the band index is a 20,000-entry
	// array searched in 15 probes.
	//
	// The rendering ceiling is lower and is not this file's problem. At 22px a
	// row, a million rows is a 22,000,000px scroll container: legal in Chrome
	// (~33.5M px per element) and not in Firefox (~17.9M, so ~813k rows). Storage
	// refuses at RowCeiling; whether the browser can draw that far is a
	// render-layer question.
	RowCeiling = 1000000

	// DefaultRows is the extent a freshly created or freshly seeded sheet starts
	// at. It is a starting size, not a limit: the sheet extends past it on demand
	// (an insert at the bottom, a write below the current extent) and shrinks
	// back down to it on delete. growrows.go's foot control is the door out.
	//
	// The size itself is nearly free — storage is sparse and rendering is
	// windowed, so a blank 10,000-row sheet stores no cells and renders the same
	// node count a 1,000-row one does; only the band table and the scroll
	// container's pixel height scale with it, both by trivial amounts. The real
	// cost is the scrollbar: twenty rows of data in a 10,000-row container leave
	// the thumb at 0.2% of the track, which is why Google Sheets ships 1,000 and
	// makes you ask for more. Sizing the container from UsedRows() plus slack
	// would be the better answer.
	//
	// Moving this number is not a one-line edit:
	//
	//   - Cache.Seed sizes a sheet to max(seededRows, DefaultRows), so a fixture
	//     that does not state its own grid changes height with this constant.
	//     The hash-pinned ones state it explicitly — goldenExtent in
	//     golden_test.go, costFixtureRows in mutate_test.go.
	//   - Deletes are the sensitive case, because the delete floor is this
	//     constant: below the fixture extent a delete shrinks the sheet, at or
	//     above it the row comes straight back. Golden hashes move accordingly
	//     and must be verified rather than regenerated blind — see the note in
	//     golden_test.go.
	//   - Pre-v5 files all have a fixed 10,000-row grid, so the migration in
	//     store.go must lay them out at legacyGridRows, never at this constant.
	DefaultRows = 10000

	// MaxCols is the fixed column count: A..Z. Deliberately single-letter —
	// SPEC.md scopes the prototype to 26 columns, and that keeps ParseRef a
	// dozen lines instead of a base-26 decoder. This one really is a maximum:
	// cells are placed by generated `[id^=A]{grid-column:2}` rules and `[id^=A]`
	// would also match `AA1`, so growing this axis is a render-layer change.
	MaxCols = 26

	// BandHeight is the number of rows in one band. band(row) = row / 50.
	BandHeight = 50

	// maxRangeCells caps ParseRange so a typo like A1:Z9999 can't allocate
	// a quarter-million CellRefs inside a request handler.
	maxRangeCells = 100000

	// maxRangeRows caps how many rows one range may span, separately from the
	// cell count, because rows go to a million: `A1:A100000` is inside
	// maxRangeCells but a range is evaluated by materializing a dense window
	// (recalc.go), so its cost is rows x MaxCols however narrow the range is.
	// This bounds that worst case.
	maxRangeRows = 10000
)

// ErrBadRef is the sentinel wrapped by every ParseRef/ParseRange failure.
var ErrBadRef = errors.New("bad cell reference")

// CellRef is a 0-indexed cell address. The zero value is A1.
type CellRef struct {
	Row, Col int
}

// Valid reports whether the ref is addressable at all — inside the column axis
// and inside RowCeiling. It says nothing about whether the row exists on any
// particular sheet, because ParseRef has no sheet: a reference past a sheet's
// current extent is well-formed and resolves to nothing, and the band index
// turns it into #REF! where a sheet is actually in hand.
func (c CellRef) Valid() bool {
	return c.Row >= 0 && c.Row < RowCeiling && c.Col >= 0 && c.Col < MaxCols
}

// String renders the ref in A1 notation: {Row:6, Col:1} -> "B7". Out-of-range
// refs render as "#REF!" rather than panicking, so a corrupt value shows up in
// the UI instead of taking down a handler.
func (c CellRef) String() string {
	if !c.Valid() {
		return "#REF!"
	}
	return string(rune('A'+c.Col)) + strconv.Itoa(c.Row+1)
}

// Band is the band index this cell belongs to.
func (c CellRef) Band() int { return BandOf(c.Row) }

// ParseRef parses A1 notation into a 0-indexed CellRef. Columns are a single
// letter A..Z (case-insensitive); rows are 1-based decimal, 1..RowCeiling. The
// upper bound is the sanity ceiling and not any sheet's extent — see Valid.
func ParseRef(s string) (CellRef, error) {
	ref := strings.TrimSpace(s)
	if ref == "" {
		return CellRef{}, fmt.Errorf("%w: empty", ErrBadRef)
	}
	col := -1
	switch c := ref[0]; {
	case c >= 'A' && c <= 'Z':
		col = int(c - 'A')
	case c >= 'a' && c <= 'z':
		col = int(c - 'a')
	default:
		return CellRef{}, fmt.Errorf("%w: %q does not start with a column letter", ErrBadRef, s)
	}

	digits := ref[1:]
	if digits == "" {
		return CellRef{}, fmt.Errorf("%w: %q has no row number", ErrBadRef, s)
	}
	// Reject multi-letter columns explicitly rather than letting Atoi's
	// generic error surface — "AA1" is the mistake people actually make, and
	// "columns are A..Z only" is a more useful message than "invalid syntax".
	for i := 0; i < len(digits); i++ {
		if d := digits[i]; d < '0' || d > '9' {
			return CellRef{}, fmt.Errorf("%w: %q — columns are A..Z only", ErrBadRef, s)
		}
	}
	row, err := strconv.Atoi(digits)
	if err != nil {
		return CellRef{}, fmt.Errorf("%w: %q: %v", ErrBadRef, s, err)
	}
	if row < 1 || row > RowCeiling {
		return CellRef{}, fmt.Errorf("%w: %q row out of range 1..%d", ErrBadRef, s, RowCeiling)
	}
	return CellRef{Row: row - 1, Col: col}, nil
}

// ParseRange parses "A1:A10" into every cell of the rectangle it spans,
// ordered row-major (row 0 left-to-right, then row 1, ...). Endpoints may be
// given in either order: "C3:A1" normalizes to the same rectangle as "A1:C3".
func ParseRange(s string) ([]CellRef, error) {
	lo, hi, err := ParseRangeBounds(s)
	if err != nil {
		return nil, err
	}
	n := (hi.Row - lo.Row + 1) * (hi.Col - lo.Col + 1)
	out := make([]CellRef, 0, n)
	for r := lo.Row; r <= hi.Row; r++ {
		for c := lo.Col; c <= hi.Col; c++ {
			out = append(out, CellRef{Row: r, Col: c})
		}
	}
	return out, nil
}

// ParseRangeBounds parses "A1:C3" into its normalized top-left and
// bottom-right corners without materializing the cells. Useful when a caller
// only wants to test membership or bounds.
func ParseRangeBounds(s string) (lo, hi CellRef, err error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return CellRef{}, CellRef{}, fmt.Errorf("%w: %q is not a range (want A1:A10)", ErrBadRef, s)
	}
	start, err := ParseRef(a)
	if err != nil {
		return CellRef{}, CellRef{}, fmt.Errorf("range %q start: %w", s, err)
	}
	end, err := ParseRef(b)
	if err != nil {
		return CellRef{}, CellRef{}, fmt.Errorf("range %q end: %w", s, err)
	}
	lo = CellRef{Row: min(start.Row, end.Row), Col: min(start.Col, end.Col)}
	hi = CellRef{Row: max(start.Row, end.Row), Col: max(start.Col, end.Col)}
	if r := hi.Row - lo.Row + 1; r > maxRangeRows {
		return CellRef{}, CellRef{}, fmt.Errorf("%w: range %q spans %d rows (max %d)", ErrBadRef, s, r, maxRangeRows)
	}
	if n := (hi.Row - lo.Row + 1) * (hi.Col - lo.Col + 1); n > maxRangeCells {
		return CellRef{}, CellRef{}, fmt.Errorf("%w: range %q spans %d cells (max %d)", ErrBadRef, s, n, maxRangeCells)
	}
	return lo, hi, nil
}

// BandOf returns the band index containing row. Negative rows clamp to 0 —
// callers should have validated by now, and returning -1 here would produce a
// nonsense subject name downstream.
func BandOf(row int) int {
	if row < 0 {
		return 0
	}
	return row / BandHeight
}

// BandRows returns the inclusive row range [lo, hi] covered by a band. hi is
// clamped to RowCeiling-1, not to any sheet's extent — a band is pure
// arithmetic on display rows and has no sheet in hand. A caller holding a sheet
// clamps to (*Sheet).Rows itself; Window does exactly that.
func BandRows(band int) (lo, hi int) {
	if band < 0 {
		band = 0
	}
	lo = band * BandHeight
	hi = lo + BandHeight - 1
	if hi > RowCeiling-1 {
		hi = RowCeiling - 1
	}
	return lo, hi
}

// BandsFor turns a dirty cell set into the deduped, ascending band set that
// becomes the published subjects: the handler discovers cells, this turns them
// into topics.
func BandsFor(cells []CellRef) []int {
	if len(cells) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(cells))
	out := make([]int, 0, len(cells))
	for _, c := range cells {
		b := BandOf(c.Row)
		if _, dup := seen[b]; dup {
			continue
		}
		seen[b] = struct{}{}
		out = append(out, b)
	}
	sort.Ints(out)
	return out
}

// BandRange returns every band index touched by the inclusive row window
// [loRow, hiRow]. Used by the viewport command to work out which subjects a
// connection's buffer needs.
func BandRange(loRow, hiRow int) []int {
	if hiRow < loRow {
		loRow, hiRow = hiRow, loRow
	}
	if loRow < 0 {
		loRow = 0
	}
	if hiRow > RowCeiling-1 {
		hiRow = RowCeiling - 1
	}
	lo, hi := BandOf(loRow), BandOf(hiRow)
	out := make([]int, 0, hi-lo+1)
	for b := lo; b <= hi; b++ {
		out = append(out, b)
	}
	return out
}
