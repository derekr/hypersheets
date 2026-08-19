package main

import (
	"context"
	"strings"
	"testing"
)

func refs(s ...string) []CellRef {
	out := make([]CellRef, 0, len(s))
	for _, r := range s {
		c, err := ParseRef(r)
		if err != nil {
			panic(err)
		}
		out = append(out, c)
	}
	return out
}

func names(cells []CellRef) []string {
	out := make([]string, 0, len(cells))
	for _, c := range cells {
		out = append(out, c.String())
	}
	return out
}

func TestEditLogSinceUnionsCoalescedEdits(t *testing.T) {
	// THE REASON THIS IS A LOG. Wakes coalesce — the wake channel holds one
	// slot — so two edits between one connection's renders produce ONE wake.
	// A "last dirty set" would lose the first edit's cells entirely and the
	// viewer would sit on a stale value indefinitely.
	var l editLog
	l.Append(context.Background(), refs("A1", "B1"))
	l.Append(context.Background(), refs("C5"))

	got, _, _, ok := l.Since(0)
	if !ok {
		t.Fatal("Since(0) not ok with two entries in the ring")
	}
	if want := "A1 B1 C5"; strings.Join(names(got), " ") != want {
		t.Errorf("Since(0) = %v, want %s", names(got), want)
	}

	// A screen that already applied the first edit only needs the second.
	got, _, _, _ = l.Since(1)
	if want := "C5"; strings.Join(names(got), " ") != want {
		t.Errorf("Since(1) = %v, want %s", names(got), want)
	}
	if got, _, _, _ := l.Since(2); len(got) != 0 {
		t.Errorf("Since(2) = %v, want nothing", names(got))
	}
}

func TestEditLogDeduplicates(t *testing.T) {
	var l editLog
	l.Append(context.Background(), refs("A1", "B1"))
	l.Append(context.Background(), refs("B1", "C1"))
	got, _, _, _ := l.Since(0)
	if want := "A1 B1 C1"; strings.Join(names(got), " ") != want {
		t.Errorf("Since(0) = %v, want %s — a cell dirtied twice is still one patch", names(got), want)
	}
}

func TestEditLogGapDegradesToFullRender(t *testing.T) {
	// Falling off the back of the ring must report "I cannot tell you", so the
	// caller re-renders everything. Reporting an incomplete set would leave the
	// screen permanently wrong with no signal anywhere.
	var l editLog
	for i := 0; i < editLogDepth+5; i++ {
		l.Append(context.Background(), refs("A1"))
	}
	if _, _, _, ok := l.Since(0); ok {
		t.Error("Since(0) claimed to be complete after the ring wrapped")
	}
	if _, _, _, ok := l.Since(l.Seq() - 1); !ok {
		t.Error("Since(seq-1) must still be answerable")
	}
	if n := len(l.ring); n != editLogDepth {
		t.Errorf("ring holds %d entries, want %d", n, editLogDepth)
	}
}

func TestEditLogAppendCopiesTheDirtySet(t *testing.T) {
	// Recalc hands back a slice the caller keeps using. Retaining it would let
	// a later append rewrite history under a reader.
	var l editLog
	cells := refs("A1", "B1")
	l.Append(context.Background(), cells)
	cells[0] = CellRef{Row: 999, Col: 25}
	got, _, _, _ := l.Since(0)
	if got[0].String() != "A1" {
		t.Errorf("log entry mutated to %s when the caller reused its slice", got[0])
	}
}

func TestEditLogSeqAndLast(t *testing.T) {
	var l editLog
	if l.Seq() != 0 {
		t.Errorf("empty log Seq() = %d, want 0", l.Seq())
	}
	if ctx, at := l.Last(); ctx == nil || !at.IsZero() {
		t.Error("empty log must return a usable context and a zero time, not nil")
	}
	if seq := l.Append(context.Background(), refs("A1")); seq != 1 {
		t.Errorf("first append returned seq %d, want 1", seq)
	}
	if _, at := l.Last(); at.IsZero() {
		t.Error("Last() lost the timestamp")
	}
}

// ─── Read coalescing ──────────────────────────────────────────────────────────

func TestRowRuns(t *testing.T) {
	cases := []struct {
		name string
		rows []int
		want []rowRange
	}{
		{"empty", nil, nil},
		{"single", []int{7}, []rowRange{{7, 7}}},
		{"unsorted and duplicated", []int{9, 7, 9, 8}, []rowRange{{7, 9}}},
		{"small hole is read across", []int{0, 3}, []rowRange{{0, 3}}},
		{
			// The A1 cascade shape: a tight cluster plus a distant tail. One
			// range from 0 to 200 would read most of the buffer back.
			name: "distant cells stay separate",
			rows: []int{0, 1, 50, 100, 150, 200},
			want: []rowRange{{0, 1}, {50, 50}, {100, 100}, {150, 150}, {200, 200}},
		},
		{
			name: "a run is capped",
			rows: []int{0, 1, 2, 3, 4, 5},
			want: []rowRange{{0, 4}, {5, 5}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rowRuns(tc.rows, cellRunGap, 5)
			if len(got) != len(tc.want) {
				t.Fatalf("rowRuns = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("rowRuns = %v, want %v", got, tc.want)
				}
			}
			// Every input row must be covered, or the patch silently drops
			// cells it was asked to send.
			for _, r := range tc.rows {
				covered := false
				for _, run := range got {
					if r >= run.Lo && r <= run.Hi {
						covered = true
					}
				}
				if !covered {
					t.Errorf("row %d is not covered by %v", r, got)
				}
			}
		})
	}
}

// ─── The cell payload ─────────────────────────────────────────────────────────

func TestRenderCellsIsBareCellsWithIds(t *testing.T) {
	// Datastar's default `outer` mode with no selector resolves each incoming
	// element by its OWN id (verified in the v1.0.1 bundle:
	// `l=document.getElementById(c.id)`), so the payload must be nothing but
	// id-carrying cells. A wrapper element would make the whole thing one
	// patch against an id that does not exist.
	body := renderCells([]Cell{
		{Ref: CellRef{Row: 0, Col: 0}, Computed: "7", Display: "7", Kind: KindNumber},
		{Ref: CellRef{Row: 200, Col: 24}, Computed: "=A1*2", Display: "=A1*2", Kind: KindFormula, Raw: "=A1*2"},
	})
	if !strings.HasPrefix(body, `<b id="A1"`) {
		t.Errorf("payload does not start with a bare cell: %q", body)
	}
	if !strings.Contains(body, `<b id="Y201"`) {
		t.Errorf("payload lost the second cell: %q", body)
	}
	for _, banned := range []string{"<tr", "<table", "<div", "<tbody"} {
		if strings.Contains(body, banned) {
			t.Errorf("payload contains %s; it must be bare cells", banned)
		}
	}
	if renderCells(nil) != "" {
		t.Error("an empty dirty set must render nothing")
	}
	// An EMPTY cell is not a patch at all — it is a remove, and pushCells sorts
	// it into the remove selector before it ever gets here. Emitting an empty
	// element instead would leave an invisible node behind on every cleared
	// cell, and the held mask would then disagree with the DOM.
	if got := renderCells([]Cell{{Ref: CellRef{Row: 0, Col: 0}}}); got != "" {
		t.Errorf("an emptied cell must render nothing, got %q", got)
	}
}

// TestCellPatchMatchesFullRenderByteForByte is the correctness constraint that
// makes the two paths interchangeable: whatever a per-cell patch writes, the
// next full morph has to agree with it exactly, or every scroll after an edit
// would re-morph cells that already hold the right value.
func TestCellPatchMatchesFullRenderByteForByte(t *testing.T) {
	cells := []Cell{
		{Ref: CellRef{Row: 3, Col: 2}, Computed: "12", Display: "12", Kind: KindNumber},
		{Ref: CellRef{Row: 3, Col: 5}, Computed: "hi <there>", Display: "hi <there>", Kind: KindText},
		{Ref: CellRef{Row: 3, Col: 7}, Computed: "24", Display: "24", Kind: KindFormula, Raw: "=A1*2"},
		{Ref: CellRef{Row: 3, Col: 9}, Computed: "#CYCLE!", Display: "#CYCLE!", Kind: KindError, Raw: "=J4"},
	}
	// A full render of row 3 alone.
	window := blankCells(3, 3)
	for _, c := range cells {
		window[c.Ref.Col] = c
	}
	full := renderCellRows(window, 3, 3)

	for _, c := range cells {
		td := renderCells([]Cell{c})
		if !strings.Contains(full, td) {
			t.Errorf("cell patch %q does not appear verbatim in the full render", td)
		}
	}
}

// TestDirtyCellsAreFilteredToTheBuffer pins the rule that makes a far-away
// cascade free: a cell outside this connection's window is dropped before
// anything is read, so it costs no query and no bytes.
func TestDirtyCellsAreFilteredToTheBuffer(t *testing.T) {
	dirty := refs("A1", "A51", "A101", "A5001", "Z9999")
	const lo, hi = 50, 349

	var in []CellRef
	for _, c := range dirty {
		if c.Row >= lo && c.Row <= hi {
			in = append(in, c)
		}
	}
	if got, want := strings.Join(names(in), " "), "A51 A101"; got != want {
		t.Errorf("in-buffer dirty set = %q, want %q", got, want)
	}

	// And the rows those cells sit in are the only rows that get read.
	rows := make([]int, 0, len(in))
	for _, c := range in {
		rows = append(rows, c.Row)
	}
	runs := rowRuns(rows, cellRunGap, cellRunMax)
	total := 0
	for _, r := range runs {
		total += r.rows()
	}
	if total != 2 {
		t.Errorf("reading %d rows for 2 dirty cells (%v); a 300-row buffer must not be scanned", total, runs)
	}
}
