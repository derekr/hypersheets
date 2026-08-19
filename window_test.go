package main

import (
	"strings"
	"testing"
)

// ─── The band delta ───────────────────────────────────────────────────────────

func TestDiffWindow(t *testing.T) {
	cases := []struct {
		name                                  string
		oldLo, oldHi, newLo, newHi            int
		full                                  bool
		prepend, appended, dropHead, dropTail rowRange
	}{
		{
			name:  "scroll down one band",
			oldLo: 0, oldHi: 349, newLo: 50, newHi: 399,
			appended: rowRange{350, 399},
			dropHead: rowRange{0, 49},
		},
		{
			name:  "scroll up one band",
			oldLo: 50, oldHi: 399, newLo: 0, newHi: 349,
			prepend:  rowRange{0, 49},
			dropTail: rowRange{350, 399},
		},
		{
			name:  "scroll down several bands, still overlapping",
			oldLo: 0, oldHi: 349, newLo: 200, newHi: 549,
			appended: rowRange{350, 549},
			dropHead: rowRange{0, 199},
		},
		{
			// Both ends move outward at once — the browser reported a taller
			// viewport. A model that assumes the buffer only ever slides one
			// way loses one of these two ranges.
			name:  "grow at both ends",
			oldLo: 100, oldHi: 199, newLo: 50, newHi: 299,
			prepend:  rowRange{50, 99},
			appended: rowRange{200, 299},
		},
		{
			name:  "shrink at both ends",
			oldLo: 50, oldHi: 299, newLo: 100, newHi: 199,
			dropHead: rowRange{50, 99},
			dropTail: rowRange{200, 299},
		},
		{
			name:  "grow above, shrink below",
			oldLo: 100, oldHi: 299, newLo: 50, newHi: 199,
			prepend:  rowRange{50, 99},
			dropTail: rowRange{200, 299},
		},
		{
			name:  "disjoint jump forward",
			oldLo: 0, oldHi: 349, newLo: 5000, newHi: 5349,
			full: true,
		},
		{
			name:  "disjoint jump backward",
			oldLo: 5000, oldHi: 5349, newLo: 0, newHi: 349,
			full: true,
		},
		{
			// Exactly adjacent shares no row, so there is nothing to preserve
			// and nothing to be incremental about.
			name:  "adjacent but not overlapping",
			oldLo: 0, oldHi: 349, newLo: 350, newHi: 699,
			full: true,
		},
		{
			name:  "one row of overlap is still an overlap",
			oldLo: 0, oldHi: 349, newLo: 349, newHi: 699,
			appended: rowRange{350, 699},
			dropHead: rowRange{0, 348},
		},
		{
			name:  "no movement",
			oldLo: 0, oldHi: 349, newLo: 0, newHi: 349,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := diffWindow(tc.oldLo, tc.oldHi, tc.newLo, tc.newHi)
			if d.Full != tc.full {
				t.Fatalf("Full = %v, want %v (%+v)", d.Full, tc.full, d)
			}
			if tc.full {
				return
			}
			// An unset expectation is the zero rowRange, which is a legal
			// one-row range — so "expected empty" is the zero VALUE, not
			// rowRange.empty().
			eq := func(what string, got, want rowRange) {
				t.Helper()
				if want == (rowRange{}) {
					if !got.empty() {
						t.Errorf("%s = %v, want empty", what, got)
					}
					return
				}
				if got != want {
					t.Errorf("%s = %v, want %v", what, got, want)
				}
			}
			eq("Prepend", d.Prepend, tc.prepend)
			eq("Append", d.Append, tc.appended)
			eq("DropHead", d.DropHead, tc.dropHead)
			eq("DropTail", d.DropTail, tc.dropTail)

			// THE INVARIANT THE WHOLE THING RESTS ON. Whatever the four ranges
			// say, applying them to the old window must produce exactly the new
			// window — no row kept twice, none lost.
			held := map[int]bool{}
			for r := tc.oldLo; r <= tc.oldHi; r++ {
				held[r] = true
			}
			for _, drop := range []rowRange{d.DropHead, d.DropTail} {
				for r := drop.Lo; r <= drop.Hi; r++ {
					if !held[r] {
						t.Errorf("dropping row %d that the client does not have", r)
					}
					delete(held, r)
				}
			}
			for _, add := range []rowRange{d.Prepend, d.Append} {
				for r := add.Lo; r <= add.Hi; r++ {
					if held[r] {
						t.Errorf("adding row %d that the client already has — duplicate id", r)
					}
					held[r] = true
				}
			}
			for r := tc.newLo; r <= tc.newHi; r++ {
				if !held[r] {
					t.Errorf("row %d missing after the patch", r)
				}
				delete(held, r)
			}
			for r := range held {
				t.Errorf("row %d left over after the patch", r)
			}
		})
	}
}

func TestDiffWindowCounts(t *testing.T) {
	d := diffWindow(0, 349, 50, 399)
	if got := d.Rows(); got != 50 {
		t.Errorf("Rows() = %d, want 50 — a one-band scroll renders one band", got)
	}
	if got := d.Dropped(); got != 50 {
		t.Errorf("Dropped() = %d, want 50", got)
	}
	if d.NoOp() {
		t.Error("a one-band scroll is not a no-op")
	}
	if still := diffWindow(0, 349, 0, 349); !still.NoOp() {
		t.Errorf("an unmoved buffer must be a no-op, got %+v", still)
	}
}

func TestDropRowNums(t *testing.T) {
	// Scrolling down drops only the head.
	if got, want := diffWindow(0, 149, 50, 199).DropRowNums(), rowNumSelector(0, 49); got != want {
		t.Errorf("head-only selector = %q, want %q", got, want)
	}
	// Shrinking drops both ends, and both must arrive in ONE selector or the
	// second frame's removal races the first frame's morph.
	sel := diffWindow(50, 299, 100, 199).DropRowNums()
	if !strings.Contains(sel, "#n51") || !strings.Contains(sel, "#n100") ||
		!strings.Contains(sel, "#n201") || !strings.Contains(sel, "#n300") {
		t.Errorf("both-ends selector missing a boundary row: %q", sel)
	}
	// 50 rows off the top (50..99) plus 100 off the bottom (200..299).
	if n := strings.Count(sel, "#n"); n != 150 {
		t.Errorf("%d row selectors, want 150", n)
	}
	if diffWindow(0, 349, 0, 349).DropRowNums() != "" {
		t.Error("an unmoved buffer must drop nothing")
	}
}

func TestRowNumSelectorIsOneIndexed(t *testing.T) {
	// Row ids are human (1-based) but every row number in Go is 0-based. Off by
	// one here removes the wrong rows and leaves the grid one row out of step
	// with the scrollbar forever.
	if got, want := rowNumSelector(0, 2), "#n1,#n2,#n3"; got != want {
		t.Errorf("rowNumSelector(0,2) = %q, want %q", got, want)
	}
	if got := rowNumSelector(5, 4); got != "" {
		t.Errorf("empty range must select nothing, got %q", got)
	}
}

// TestMaskSelectorNamesOnlyCellsThatExist is the sparse half of a drop. There
// is no row element to take its cells with it any more, so the selector has to
// name each one — and only the ones the client actually has, which is the whole
// reason screen.heldMask exists.
func TestMaskSelectorNamesOnlyCellsThatExist(t *testing.T) {
	// row 10 holds A and C; row 11 holds nothing; row 12 holds Z.
	mask := []uint32{1<<0 | 1<<2, 0, 1 << 25}
	got := maskSelector(mask, 10, 10, 12)
	if want := "#A11,#C11,#Z13"; got != want {
		t.Errorf("maskSelector = %q, want %q", got, want)
	}
	if got := maskSelector(mask, 10, 11, 11); got != "" {
		t.Errorf("an empty row must select nothing, got %q", got)
	}
	// Out of range asks for rows the mask does not describe. Naming nothing is
	// the safe answer: a stale element left behind is repaired by the next full
	// morph, a wrongly-named one removes live data.
	if got := maskSelector(mask, 10, 100, 120); got != "" {
		t.Errorf("out-of-range rows must select nothing, got %q", got)
	}
}

// ─── DOM order carries no information ─────────────────────────────────────────

// TestPositionIsAbsoluteNotOrdinal is the property that replaced the whole
// prepend/append ordering contract. Every element the grid renders states its
// own row in `--r`, so the client may insert it anywhere in the child list.
//
// It is worth pinning because it is what licenses three simplifications at
// once: one append frame instead of a prepend and an append, a cell insert that
// does not have to find its neighbours, and a scroll that cannot land the
// buffer out of order however its frames interleave.
func TestPositionIsAbsoluteNotOrdinal(t *testing.T) {
	cells := blankCells(10, 12)
	for i := range cells {
		cells[i].Kind = KindNumber
		cells[i].Computed = "x"
	}
	rows := renderCellRows(cells, 10, 12)
	for _, want := range []string{`id="A11" style="--r:10"`, `id="A13" style="--r:12"`} {
		if !strings.Contains(rows, want) {
			t.Errorf("rendered cells do not carry their absolute row: want %q in %q", want, rows[:120])
		}
	}
	// The gutter states its row the same way, and the two spellings must agree:
	// the id is 1-based (it is A1 notation) and `--r` is 0-based (it multiplies
	// the row height). Getting that backwards puts every row one line out.
	nums := renderRowNums(10, 12)
	if want := `<b id="n11" style="--r:10">11</b>`; !strings.Contains(nums, want) {
		t.Errorf("row number element = %q, want it to contain %q", nums, want)
	}
	// Nothing anywhere may depend on the order the rows come out in, so the
	// renderer is free to emit them ascending — and does — but the CLIENT must
	// not need that. Assert the only thing that matters: every element names
	// its row.
	if strings.Count(nums, "--r:") != 3 {
		t.Errorf("expected 3 positioned row numbers, got %q", nums)
	}
}

// TestEmptyCellsRenderNothing is the headline claim of sparse rendering.
func TestEmptyCellsRenderNothing(t *testing.T) {
	if got := renderCellRows(blankCells(0, 449), 0, 449); got != "" {
		t.Errorf("a blank 450-row buffer emitted %d bytes of cells, want 0", len(got))
	}
	// The gutter is what a blank sheet still costs, and it is irreducible: the
	// row number is data.
	if n := strings.Count(renderRowNums(0, 449), "<b "); n != 450 {
		t.Errorf("blank buffer rendered %d row numbers, want 450", n)
	}
}

// ─── The stale-bounds regression ──────────────────────────────────────────────

// TestBufferBoundsAreSignalsNotAttributes is the regression for the failure
// mode this change could most easily have introduced: scrolling keeps working
// for exactly one buffer's worth of rows and then stops, with no error
// anywhere, because the client is still reading first-paint bounds.
func TestBufferBoundsAreSignalsNotAttributes(t *testing.T) {
	shell := pageShell("demo", 100, 449, renderWindow(blankCells(100, 449), 100, 449, "demo", selRange{}), zeroAnchor())

	// The server-owned bounds are declared as signals, seeded with the first
	// buffer, so the very first scroll has something real to test against.
	for _, want := range []string{"blo:100", "bhi:449"} {
		if !strings.Contains(shell, want) {
			t.Errorf("page shell does not declare %q; $blo/$bhi would be undefined on the first scroll", want)
		}
	}
	// And they are NOT the client's viewport signals.
	if !strings.Contains(shell, "lo:100,hi:449") {
		t.Error("the client's own lo/hi signals are gone")
	}

	// The grid must no longer publish bounds the client could read instead.
	if strings.Contains(shell, "data-lo=") || strings.Contains(shell, "data-hi=") {
		t.Error("#g still carries data-lo/data-hi — a reader of those would freeze at first paint")
	}

	// The buffer no longer has a vertical offset to keep in step: every element
	// carries its own absolute row. `--lo` and the translate that read it are
	// gone, and their absence is the point — they were the thing a scroll patch
	// could desync from the rows it shipped.
	//
	// The check is against the STYLESHEET and the `--lo` custom property, not
	// against the string "translateY" anywhere in the page: the drag guide
	// translates itself on either axis (see T.gdMove) and is not the buffer.
	if strings.Contains(shell, "--lo:") {
		t.Error("the buffer still carries a --lo offset; rows position themselves now")
	}
	if strings.Contains(gridCSS, "translateY") {
		t.Error("the stylesheet still transforms the buffer; rows position themselves now")
	}
}

// TestPatchTargetsExist is the other half: the ids an incremental patch selects
// have to be in the HTML the full path emits, or the first scroll after a
// disjoint jump patches into nothing. Datastar logs
// "PatchElementsNoTargetsFound" to the console and drops the frame, which is
// invisible from the server.
func TestPatchTargetsExist(t *testing.T) {
	cells := blankCells(0, 99)
	cells[0].Kind, cells[0].Computed = KindNumber, "1"
	grid := renderWindow(cells, 0, 99, "demo", selRange{})
	for _, id := range []string{`id="` + gutterID + `"`, `id="` + bufferID + `"`} {
		if !strings.Contains(grid, id) {
			t.Fatalf("no %s to append into", id)
		}
	}
	for _, id := range []string{`id="n1"`, `id="n100"`, `id="A1"`} {
		if !strings.Contains(grid, id) {
			t.Errorf("full render is missing %s — an incremental patch could not target it", id)
		}
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// blankCells is the dense row-major rectangle Window would return for
// [loRow,hiRow], with every cell empty. The renderers only care about shape.
func blankCells(loRow, hiRow int) []Cell {
	n := (hiRow - loRow + 1) * MaxCols
	if n < 0 {
		n = 0
	}
	cells := make([]Cell, 0, n)
	for r := loRow; r <= hiRow; r++ {
		for c := 0; c < MaxCols; c++ {
			cells = append(cells, Cell{Ref: CellRef{Row: r, Col: c}})
		}
	}
	return cells
}

// ─── Row groups ───────────────────────────────────────────────────────────────

// TestRowGroupShape pins the alternative rendering measured in RESULTS.md: the
// row states its position once and its cells state only their ids.
//
// It flips the package-level mode, which is safe because these tests do not run
// in parallel and the mode is read, never cached.
func TestRowGroupShape(t *testing.T) {
	defer func(was bool) { rowGroupMode = was }(rowGroupMode)
	rowGroupMode = true

	cells := blankCells(250, 250)
	cells[0] = Cell{Ref: CellRef{Row: 250, Col: 0}, Computed: "7", Display: "7", Kind: KindNumber}
	cells[3] = Cell{Ref: CellRef{Row: 250, Col: 3}, Computed: "hi", Display: "hi", Kind: KindText}

	got := renderCellRows(cells, 250, 250)
	want := `<div class="w" id="r251" style="--r:250"><b id="A251">7</b><b id="D251" class="t">hi</b></div>`
	if got != want {
		t.Errorf("row group =\n %q\nwant\n %q", got, want)
	}
	// The cells must NOT repeat the row — that saving is the whole point.
	if strings.Count(got, "--r:") != 1 {
		t.Errorf("the row is stated more than once: %q", got)
	}
	// An empty row costs nothing at all, wrapper included.
	if got := renderCellRows(blankCells(0, 99), 0, 99); got != "" {
		t.Errorf("blank rows produced %q", got)
	}
	// And a drop names the wrapper, not its cells: O(rows), not O(cells).
	mask := []uint32{0x3ffffff, 0, 0x3ffffff}
	if got, want := maskSelector(mask, 10, 10, 12), "#r11,#r13"; got != want {
		t.Errorf("row-group drop selector = %q, want %q", got, want)
	}
}
