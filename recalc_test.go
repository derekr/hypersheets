package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// recalc_test.go — the fan-out ceiling, and what happens at it.
//
// maxRecalcNodes is the only thing standing between one edit and an unbounded
// pause of the sheet's single writer, so the two questions worth a test are
// "does it refuse" and "what does it leave behind". The second is the one that
// matters: a cap that stopped a cascade halfway and committed the half would be
// worse than no cap at all, because a partially recalculated sheet is wrong in a
// way no later edit repairs.

// fanOut gives ref n dependents, written straight through WriteCell so that
// building the graph does not itself run the pass under test. They start well
// below the ref so nothing overlaps.
func fanOut(t *testing.T, sh *Sheet, ref CellRef, n int) {
	t.Helper()
	fanOutAt(t, sh, ref, n, 10)
}

// fanOutAt is fanOut with the block of dependents placed at a chosen row, so a
// test can build two fan-outs on one sheet without them overlapping.
func fanOutAt(t *testing.T, sh *Sheet, ref CellRef, n, row0 int) {
	t.Helper()
	raw := "=" + ref.String() + "*2"
	for i := 0; i < n; i++ {
		d := CellRef{Row: row0 + i/MaxCols, Col: i % MaxCols}
		if err := sh.WriteCell(d, raw,
			[]ComputedCell{{Ref: d, Computed: "2", Kind: KindFormula}}); err != nil {
			t.Fatalf("build dependent %d (%s): %v", i, d, err)
		}
	}
}

// TestRecalcCapRefusesCleanly walks the ceiling from just under it to just over
// it on ONE sheet, so the only difference between the pass that succeeds and
// the pass that refuses is two cells.
func TestRecalcCapRefusesCleanly(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "fanout")
	a1 := CellRef{Row: 0, Col: 0}
	if err := sh.WriteCell(a1, "1",
		[]ComputedCell{{Ref: a1, Computed: "1", Kind: KindNumber}}); err != nil {
		t.Fatalf("seed A1: %v", err)
	}

	// Just under the cap: a legitimate — if absurd — sheet must still commit.
	fanOut(t, sh, a1, maxRecalcNodes-1)
	res, err := Recalc(sh, a1, "5")
	if err != nil {
		t.Fatalf("a cascade of %d cells was refused: %v", maxRecalcNodes-1, err)
	}
	t.Logf("cascade of %d nodes: %d dirty, depth %d, %d queries, %.1f ms",
		res.Stats.Nodes, len(res.Dirty), res.Stats.Depth, res.Stats.Queries,
		msf(res.Stats.Elapsed))
	if res.Stats.Nodes != maxRecalcNodes {
		t.Errorf("pass evaluated %d nodes, want %d (the edited cell plus its dependents)",
			res.Stats.Nodes, maxRecalcNodes)
	}

	// What the ceiling actually costs the sheet is the recalc PLUS the write
	// that persists it, since both happen inside the one turn that holds the
	// actor. This is the number the cap is chosen against.
	start := time.Now()
	if err := sh.WriteCell(a1, "5", res.Computed); err != nil {
		t.Fatalf("commit the capped cascade: %v", err)
	}
	commit := time.Since(start)
	t.Logf("committing it: %.1f ms — the whole worst-case turn is %.1f ms",
		msf(commit), msf(res.Stats.Elapsed+commit))
	wantCell(t, sh, "A1", "5", "5")
	wantCell(t, sh, "A11", "=A1*2", "10")

	// Two more dependents and the same edit is past it.
	seqBefore, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	for i := 0; i < 2; i++ {
		d := CellRef{Row: 900 + i, Col: 0}
		if err := sh.WriteCell(d, "=A1*2",
			[]ComputedCell{{Ref: d, Computed: "2", Kind: KindFormula}}); err != nil {
			t.Fatalf("extra dependent: %v", err)
		}
	}
	start = time.Now()
	_, err = Recalc(sh, a1, "9")
	refusal := time.Since(start)
	if !errors.Is(err, ErrRecalcTooLarge) {
		t.Fatalf("a cascade past the cap = %v, want ErrRecalcTooLarge", err)
	}
	t.Logf("refusal in %.1f ms: %v", msf(refusal), err)

	// THE REFUSAL IS CLEAN, and it is clean by construction rather than by
	// care: recalc runs BEFORE WriteCell, so a pass that gives up has not been
	// near the database in write mode. A1 still holds what it held, no
	// dependent moved, and no event was appended.
	wantCell(t, sh, "A1", "5", "5")
	wantCell(t, sh, "A11", "=A1*2", "10")
	seqAfter, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if seqAfter != seqBefore+2 {
		t.Errorf("event log moved by %d over the refused edit, want 2 (the two dependents)",
			seqAfter-seqBefore)
	}
}

// TestStructuralRecalcCapRollsBack is the other user of the same constant, and
// the dangerous one: reachFrom runs INSIDE the mutation's transaction, after
// rows have already been shifted and references repaired. If the cap were
// reached there and the transaction committed anyway, the sheet would be
// shifted but not recalculated — every formula the mutation disturbed holding a
// value for a layout that no longer exists.
func TestStructuralRecalcCapRollsBack(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "structcap")

	// A range formula, so that deleting a row inside it changes its EXTENT —
	// which is what seeds the recompute — and a fan-out from that one formula
	// wide enough to walk past the cap.
	for i := 0; i < 10; i++ {
		r := CellRef{Row: i, Col: 0}
		v := strconv.Itoa(i + 1)
		if err := sh.WriteCell(r, v,
			[]ComputedCell{{Ref: r, Computed: v, Kind: KindNumber}}); err != nil {
			t.Fatalf("seed A%d: %v", i+1, err)
		}
	}
	z1 := CellRef{Row: 0, Col: 25}
	if err := sh.WriteCell(z1, "=SUM(A1:A10)",
		[]ComputedCell{{Ref: z1, Computed: "55", Kind: KindFormula}}); err != nil {
		t.Fatalf("seed Z1: %v", err)
	}
	fanOut(t, sh, z1, maxRecalcNodes+1)

	rowsBefore := sh.Rows()
	eventsBefore := countRows(t, sh, "events")
	cellsBefore := countRows(t, sh, "cells")

	start := time.Now()
	_, err := sh.DeleteRows(4, 1)
	refusal := time.Since(start)
	if !errors.Is(err, ErrRecalcTooLarge) {
		t.Fatalf("DeleteRows into a %d-cell cascade = %v, want ErrRecalcTooLarge",
			maxRecalcNodes+1, err)
	}
	t.Logf("structural refusal in %.1f ms: %v", msf(refusal), err)

	// Nothing survived the rollback: not the shift, not the event the mutation
	// appends as its first act, not the reference repairs.
	if got := sh.Rows(); got != rowsBefore {
		t.Errorf("extent moved %d -> %d over a refused mutation", rowsBefore, got)
	}
	if got := countRows(t, sh, "events"); got != eventsBefore {
		t.Errorf("refused mutation left %d events behind", got-eventsBefore)
	}
	if got := countRows(t, sh, "cells"); got != cellsBefore {
		t.Errorf("cell count moved %d -> %d over a refused mutation", cellsBefore, got)
	}
	// A5 is the row the delete would have removed; A6 is the row that would
	// have taken its place.
	wantCell(t, sh, "A5", "5", "5")
	wantCell(t, sh, "A6", "6", "6")
	wantCell(t, sh, "Z1", "=SUM(A1:A10)", "55")

	// And the sheet still WORKS afterwards — a refused mutation must not leave
	// the handle's in-memory band index disagreeing with the file.
	if _, err := sh.Window(0, 20); err != nil {
		t.Fatalf("window after a refused mutation: %v", err)
	}
	if err := sh.WriteCell(CellRef{Row: 0, Col: 1}, "ok",
		[]ComputedCell{{Ref: CellRef{Row: 0, Col: 1}, Computed: "ok", Kind: KindText}}); err != nil {
		t.Fatalf("write after a refused mutation: %v", err)
	}
	wantCell(t, sh, "B1", "ok", "ok")
}

// ─── The batched write ────────────────────────────────────────────────────────
//
// A range clear/fill/paste used to be a loop over the single-cell path: one
// recalc pass and one transaction per cell. RecalcBatch/WriteCells replace that
// with one cascade walk, one topological order and one transaction — so the
// only question that matters is whether the two agree.
//
// They are compared DIFFERENTIALLY: the same operation run over two sheets
// seeded identically, one through the loop and one through the batch, then
// dumped cell for cell. That technique has already caught a real discrepancy in
// this codebase (the band-key migration, where three golden cases differed in
// exactly two cells and the OLD code turned out to be wrong), which is why it
// is the instrument here rather than a list of expected values.

// loopWrites is the OLD path, kept in the test file as the thing the batch is
// measured against: applyWrites (rangeops.go) and handleClear's body, which are
// the same loop. It stays here so that deleting the production loop cannot
// delete the baseline with it.
func loopWrites(t *testing.T, sh *Sheet, writes []BatchWrite) []CellRef {
	t.Helper()
	var dirty []CellRef
	seen := make(map[CellRef]struct{}, len(writes)*2)
	for _, w := range writes {
		res, err := Recalc(sh, w.Ref, w.Raw)
		if err != nil {
			t.Fatalf("loop recalc %s: %v", w.Ref, err)
		}
		if err := sh.WriteCell(w.Ref, w.Raw, res.Computed); err != nil {
			t.Fatalf("loop write %s: %v", w.Ref, err)
		}
		for _, c := range withRef(res.Dirty, w.Ref) {
			if _, dup := seen[c]; dup {
				continue
			}
			seen[c] = struct{}{}
			dirty = append(dirty, c)
		}
	}
	return dirty
}

func sortedRefs(refs []CellRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.String())
	}
	sort.Strings(out)
	return out
}

// batchCase is one differential scenario: a sheet shape and the writes to make.
type batchCase struct {
	name    string
	rows    int
	seed    func(t *testing.T, sh *Sheet)
	writes  func() []BatchWrite
	windows [][2]int
}

// TestBatchEqualsLoop is the whole correctness argument. For each scenario two
// byte-identical sheets are built, one is driven through the loop and the other
// through the batch, and then every user-visible fact about them is compared:
// the cells, the event log, and both dependency directions.
func TestBatchEqualsLoop(t *testing.T) {
	chain := func(lo, n int) []BatchWrite {
		// B1 is a literal; B2..Bn each read the cell above. Written top-down,
		// which is the order a fill produces and the order that makes the loop
		// settle in one pass — the batch has to reach the same answer from a
		// topological sort it derives itself.
		out := []BatchWrite{{CellRef{Row: lo, Col: 1}, "7"}}
		for i := 1; i < n; i++ {
			out = append(out, BatchWrite{
				CellRef{Row: lo + i, Col: 1},
				"=" + CellRef{Row: lo + i - 1, Col: 1}.String() + "+1",
			})
		}
		return out
	}

	cases := []batchCase{{
		// A range clear over the seeded grid's column A, which feeds the
		// per-band SUM in W, the cross-band X, and the five-cell Y cascade.
		name: "clear column A",
		rows: 300,
		writes: func() []BatchWrite {
			var out []BatchWrite
			for r := 0; r < 40; r++ {
				out = append(out, BatchWrite{CellRef{Row: r, Col: 0}, ""})
			}
			return out
		},
		windows: [][2]int{{0, 60}, {95, 105}, {145, 155}, {195, 205}},
	}, {
		// A clear that includes the formulas that read the cells it clears, so
		// a written cell is also a dependent of another written cell through
		// STALE storage edges the batch has to drop.
		name: "clear a block including its own aggregates",
		rows: 300,
		writes: func() []BatchWrite {
			var out []BatchWrite
			for r := 0; r < 12; r++ {
				for c := 0; c < MaxCols; c++ {
					out = append(out, BatchWrite{CellRef{Row: r, Col: c}, ""})
				}
			}
			return out
		},
		windows: [][2]int{{0, 60}, {95, 105}, {145, 205}},
	}, {
		// Fill down: every written cell is a formula reading the row above it
		// in another column. Nothing intra-batch, but 100 formulas landing in
		// one column feeding one SUM — the overlapping-cascade shape.
		name: "fill down formulas into column Z",
		rows: 300,
		writes: func() []BatchWrite {
			var out []BatchWrite
			for r := 1; r < 100; r++ {
				out = append(out, BatchWrite{
					CellRef{Row: r, Col: 25},
					"=" + CellRef{Row: r, Col: 0}.String() + "*2",
				})
			}
			return out
		},
		windows: [][2]int{{0, 120}, {145, 205}},
	}, {
		// INTRA-BATCH DEPENDENCIES. Ten cells in a chain, each reading the one
		// above, all written in the same batch.
		name: "chain down column B",
		rows: 300,
		writes: func() []BatchWrite {
			return chain(0, 10)
		},
		windows: [][2]int{{0, 60}, {95, 105}, {145, 205}},
	}, {
		// The same chain written BOTTOM-UP, so discovery order and topological
		// order disagree. The loop settles it in one pass only because each
		// cell's cascade repairs the ones already written; the batch has to get
		// there from the sort alone.
		name: "chain written bottom-up",
		rows: 300,
		writes: func() []BatchWrite {
			w := chain(0, 10)
			for i, j := 0, len(w)-1; i < j; i, j = i+1, j-1 {
				w[i], w[j] = w[j], w[i]
			}
			return w
		},
		windows: [][2]int{{0, 60}, {95, 105}, {145, 205}},
	}, {
		// A batch that introduces a cycle among cells it writes.
		name: "cycle introduced by the batch",
		rows: 300,
		writes: func() []BatchWrite {
			return []BatchWrite{
				{CellRef{Row: 0, Col: 25}, "=Z2*2"},
				{CellRef{Row: 1, Col: 25}, "=Z1+1"},
			}
		},
		windows: [][2]int{{0, 60}, {145, 205}},
	}, {
		// A batch that BREAKS a cycle: two cells already reading each other,
		// one of them replaced by a literal. Storage still holds the stale
		// edge, so this is the case the dropped-edge rule exists for.
		name: "cycle broken by the batch",
		rows: 300,
		seed: func(t *testing.T, sh *Sheet) {
			t.Helper()
			writeRaw(t, sh, CellRef{Row: 0, Col: 25}, "=Z2*2")
			writeRaw(t, sh, CellRef{Row: 1, Col: 25}, "=Z1+1")
		},
		writes: func() []BatchWrite {
			return []BatchWrite{{CellRef{Row: 0, Col: 25}, "3"}}
		},
		windows: [][2]int{{0, 60}},
	}, {
		// A self-reference, which must still be #CYCLE! inside a batch.
		name: "self reference inside a batch",
		rows: 300,
		writes: func() []BatchWrite {
			return []BatchWrite{
				{CellRef{Row: 0, Col: 25}, "1"},
				{CellRef{Row: 1, Col: 25}, "=Z2+1"},
				{CellRef{Row: 2, Col: 25}, "=Z2*3"},
			}
		},
		windows: [][2]int{{0, 60}},
	}, {
		// A cell written twice by one batch. The loop ends holding the second
		// value; so does the batch.
		name: "repeated address takes the last value",
		rows: 300,
		writes: func() []BatchWrite {
			return []BatchWrite{
				{CellRef{Row: 0, Col: 25}, "1"},
				{CellRef{Row: 1, Col: 25}, "=Z1*2"},
				{CellRef{Row: 0, Col: 25}, "9"},
			}
		},
		windows: [][2]int{{0, 60}},
	}, {
		// A formula that does not parse, a number, a label and an empty string
		// in one batch — the four kinds, so nothing about classification is
		// special-cased by batch size.
		name: "mixed kinds",
		rows: 300,
		writes: func() []BatchWrite {
			return []BatchWrite{
				{CellRef{Row: 0, Col: 25}, "=NOPE("},
				{CellRef{Row: 1, Col: 25}, "12.5"},
				{CellRef{Row: 2, Col: 25}, "label"},
				{CellRef{Row: 0, Col: 0}, ""},
			}
		},
		windows: [][2]int{{0, 60}, {95, 105}, {145, 205}},
	}, {
		// A write past the bottom of the sheet, which grows it. The batch grows
		// it once; the loop grew it on the first write that needed it.
		name: "batch past the last row grows the sheet",
		rows: 300,
		writes: func() []BatchWrite {
			var out []BatchWrite
			for r := 1195; r < 1205; r++ {
				out = append(out, BatchWrite{CellRef{Row: r, Col: 3}, strconv.Itoa(r)})
			}
			return out
		},
		windows: [][2]int{{1190, 1210}},
	}}

	probes := []string{"A1", "A2", "B1", "Z1", "Z2", "U1", "V1", "W1", "X51", "Y1", "Y51", "Y151"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCache(t, 4, time.Minute)
			build := func(id string) *Sheet {
				sh := mustSeed(t, c, id, tc.rows, MaxCols)
				if tc.seed != nil {
					tc.seed(t, sh)
				}
				return sh
			}
			loopSheet, batchSheet := build("loop"), build("batch")

			// The two sheets must start identical, or nothing below means
			// anything.
			if a, b := goldenDump(t, loopSheet, tc.windows, probes), goldenDump(t, batchSheet, tc.windows, probes); a != b {
				t.Fatalf("the two fixtures differ BEFORE the operation")
			}

			writes := tc.writes()
			loopDirty := loopWrites(t, loopSheet, writes)
			res, err := batchSheet.ApplyBatch(writes)
			if err != nil {
				t.Fatalf("ApplyBatch: %v", err)
			}

			// CELL FOR CELL, plus the event log and both dependency directions.
			want := goldenDump(t, loopSheet, tc.windows, probes)
			got := goldenDump(t, batchSheet, tc.windows, probes)
			if want != got {
				dumpDiff(t, want, got)
				t.Fatalf("batch and loop disagree")
			}
			if a, b := loopSheet.Rows(), batchSheet.Rows(); a != b {
				t.Errorf("row extent: loop %d, batch %d", a, b)
			}
			// The dirty sets agree too, in every scenario that does not move a
			// value and move it back — see TestBatchDirtyDropsCellsThatCameBack.
			lw, bw := sortedRefs(loopDirty), sortedRefs(res.Dirty)
			if !slices.Equal(lw, bw) {
				t.Errorf("dirty sets differ\n loop  (%d): %v\n batch (%d): %v",
					len(lw), lw, len(bw), bw)
			}
			t.Logf("%d writes: %d dirty, %d nodes, depth %d, %d cycled, %d queries",
				res.Wrote, len(res.Dirty), res.Stats.Nodes, res.Stats.Depth,
				res.Stats.Cycled, res.Stats.Queries)
		})
	}
}

// dumpDiff prints the first few differing lines of two golden dumps, because a
// "they differ" with no location is a fact you cannot act on.
func dumpDiff(t *testing.T, want, got string) {
	t.Helper()
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	shown := 0
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x == y {
			continue
		}
		t.Logf("line %d:\n  loop  %q\n  batch %q", i+1, x, y)
		if shown++; shown >= 12 {
			t.Logf("... and more")
			return
		}
	}
}

// writeRaw commits one cell through the ordinary single-cell path, for building
// a fixture that the operation under test then acts on.
func writeRaw(t *testing.T, sh *Sheet, ref CellRef, raw string) {
	t.Helper()
	res, err := Recalc(sh, ref, raw)
	if err != nil {
		t.Fatalf("seed recalc %s: %v", ref, err)
	}
	if err := sh.WriteCell(ref, raw, res.Computed); err != nil {
		t.Fatalf("seed write %s: %v", ref, err)
	}
}

// TestDedupeWrites pins the collapse a batch does before it walks the graph.
// It is a helper, but it is the one place where "the batch's state equals the
// loop's state" is a claim about ORDER rather than about the dependency graph.
func TestDedupeWrites(t *testing.T) {
	w := func(a1, raw string) BatchWrite { return BatchWrite{mustRef(t, a1), raw} }
	cases := []struct {
		name string
		in   []BatchWrite
		want []BatchWrite
	}{
		{"empty", nil, nil},
		{"one", []BatchWrite{w("A1", "1")}, []BatchWrite{w("A1", "1")}},
		{"all distinct — the rectangle case, no copy",
			[]BatchWrite{w("A1", "1"), w("A2", "2"), w("B1", "3")},
			[]BatchWrite{w("A1", "1"), w("A2", "2"), w("B1", "3")}},
		{"one repeat: last wins, first position kept",
			[]BatchWrite{w("A1", "1"), w("A2", "2"), w("A1", "9")},
			[]BatchWrite{w("A1", "9"), w("A2", "2")}},
		{"three of the same",
			[]BatchWrite{w("A1", "1"), w("A1", "2"), w("A1", "3")},
			[]BatchWrite{w("A1", "3")}},
		{"repeat first, then new addresses",
			[]BatchWrite{w("A1", "1"), w("A1", "2"), w("B2", "b"), w("A1", "3"), w("B2", "c")},
			[]BatchWrite{w("A1", "3"), w("B2", "c")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]BatchWrite(nil), tc.in...)
			got, err := dedupeWrites(in)
			if err != nil {
				t.Fatalf("dedupeWrites: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			// The caller's slice is never rewritten: rangeops.go builds the
			// write list once and hands the SAME slice to the write path after
			// the recalc has looked at it.
			if !slices.Equal(in, tc.in) {
				t.Errorf("dedupeWrites rewrote the caller's slice: %v, was %v", in, tc.in)
			}
		})
	}
	if _, err := dedupeWrites([]BatchWrite{{CellRef{Row: 0, Col: 0}, "ok"}, {CellRef{Row: 5, Col: 99}, "x"}}); !errors.Is(err, ErrBadRef) {
		t.Errorf("an off-grid ref = %v, want ErrBadRef", err)
	}
}

// TestBatchValuesAreRightOnTheirOwnTerms is the other half of the differential.
// "The batch agrees with the loop" is only worth having if the loop is right,
// so the two cases where that is least obvious — an intra-batch chain and a
// cycle the batch introduces — are also checked against values written out by
// hand.
func TestBatchValuesAreRightOnTheirOwnTerms(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)

	t.Run("intra-batch chain settles in one pass", func(t *testing.T) {
		sh := mustSeed(t, c, "chainvals", 300, MaxCols)
		// Z1 = 7, and Z2..Z10 each read the cell above. Column Z is the one
		// nothing in the seed reads, so the chain IS the whole graph and the
		// reported depth is the chain's own. Written BOTTOM-UP, so nothing
		// about the answer can come from the order they arrived in.
		writes := []BatchWrite{}
		for i := 9; i >= 1; i-- {
			writes = append(writes, BatchWrite{
				CellRef{Row: i, Col: 25},
				"=" + CellRef{Row: i - 1, Col: 25}.String() + "+1",
			})
		}
		writes = append(writes, BatchWrite{CellRef{Row: 0, Col: 25}, "7"})
		res, err := sh.ApplyBatch(writes)
		if err != nil {
			t.Fatalf("ApplyBatch: %v", err)
		}
		if res.Stats.Depth != 9 {
			t.Errorf("topological depth %d, want 9 — the chain was not ordered", res.Stats.Depth)
		}
		wantCell(t, sh, "Z1", "7", "7")
		for i := 1; i < 10; i++ {
			ref := CellRef{Row: i, Col: 25}
			wantCell(t, sh, ref.String(),
				"="+CellRef{Row: i - 1, Col: 25}.String()+"+1", strconv.Itoa(7+i))
		}
	})

	t.Run("cycle inside a batch is #CYCLE!", func(t *testing.T) {
		sh := mustSeed(t, c, "cyclevals", 300, MaxCols)
		writes := []BatchWrite{
			{CellRef{Row: 0, Col: 25}, "=Z2*2"},
			{CellRef{Row: 1, Col: 25}, "=Z1+1"},
			{CellRef{Row: 2, Col: 25}, "=Z1*10"}, // downstream of the cycle
		}
		res, err := sh.ApplyBatch(writes)
		if err != nil {
			t.Fatalf("ApplyBatch: %v", err)
		}
		if res.Stats.Cycled != 3 {
			t.Errorf("cycled %d, want 3 (the two members and the cell reading them)",
				res.Stats.Cycled)
		}
		wantCell(t, sh, "Z1", "=Z2*2", TokenCycle)
		wantCell(t, sh, "Z2", "=Z1+1", TokenCycle)
		wantCell(t, sh, "Z3", "=Z1*10", TokenCycle)

		// And the sheet still works: breaking the cycle in a second batch
		// settles all three.
		if _, err := sh.ApplyBatch([]BatchWrite{{CellRef{Row: 0, Col: 25}, "4"}}); err != nil {
			t.Fatalf("break the cycle: %v", err)
		}
		wantCell(t, sh, "Z1", "4", "4")
		wantCell(t, sh, "Z2", "=Z1+1", "5")
		wantCell(t, sh, "Z3", "=Z1*10", "40")
	})
}

// TestBatchOverlappingPasteIsUnaffected pins the one property the range layer
// gets from reading first rather than from the write path: a paste onto its own
// source copies the ORIGINAL values. Batching must not change that, and the way
// to be sure is to build the write list from a snapshot exactly as handlePaste
// does and then check the answer against the snapshot rather than against the
// sheet.
func TestBatchOverlappingPasteIsUnaffected(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "overlap", 300, MaxCols)

	// Copy A1:A5, paste at A3 — the destination covers rows 3..7 and overlaps
	// the source at rows 3..5.
	win, err := sh.Window(0, 4)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	src := make([]string, 5)
	for r := 0; r < 5; r++ {
		src[r] = win[r*MaxCols].Raw
	}
	var writes []BatchWrite
	for r := 0; r < 5; r++ {
		writes = append(writes, BatchWrite{CellRef{Row: r + 2, Col: 0}, src[r]})
	}
	if _, err := sh.ApplyBatch(writes); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}
	for r := 0; r < 5; r++ {
		wantCell(t, sh, CellRef{Row: r + 2, Col: 0}.String(), src[r], src[r])
	}
	// And the source rows the paste did NOT cover are untouched.
	wantCell(t, sh, "A1", src[0], src[0])
	wantCell(t, sh, "A2", src[1], src[1])
}

// TestBatchDirtyDropsCellsThatCameBack is the one place the batch's answer is
// deliberately not the loop's, recorded as a test rather than as a comment
// because it is a behaviour change the render layer can see.
//
// Two writes that move a SUM and then move it back. The loop unions its
// per-cell dirty sets, so the SUM is dirty there — and re-rendering it is
// wasted bytes, because its value is the one the viewer already has. The batch
// computes the net effect once and reports only what moved.
func TestBatchDirtyDropsCellsThatCameBack(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	loopSheet := mustSeed(t, c, "loop", 300, MaxCols)
	batchSheet := mustSeed(t, c, "batch", 300, MaxCols)

	// W1 = SUM(A1:A50). Swap A1's and A2's values: the sum is unchanged, but
	// after the first write alone it is not.
	a1, a2 := CellRef{Row: 0, Col: 0}, CellRef{Row: 1, Col: 0}
	v1 := fmtNum(seedLiteral(0, 0))
	v2 := fmtNum(seedLiteral(1, 0))
	writes := []BatchWrite{{a1, v2}, {a2, v1}}

	loopDirty := loopWrites(t, loopSheet, writes)
	res, err := batchSheet.ApplyBatch(writes)
	if err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}

	// The sheets themselves still agree — this is a difference in what is
	// REPORTED, not in what is stored.
	windows := [][2]int{{0, 60}, {145, 205}}
	if a, b := goldenDump(t, loopSheet, windows, nil), goldenDump(t, batchSheet, windows, nil); a != b {
		dumpDiff(t, a, b)
		t.Fatalf("stored state differs, which is not what this test is about")
	}

	w1 := CellRef{Row: 0, Col: seedColW}
	if !slices.Contains(loopDirty, w1) {
		t.Fatalf("the loop did not dirty %s, so this test is not testing what it says", w1)
	}
	if slices.Contains(res.Dirty, w1) {
		t.Errorf("batch reported %s dirty; its value never moved across the batch", w1)
	}
	// And everything the batch DOES report, the loop reported too.
	for _, cell := range res.Dirty {
		if !slices.Contains(loopDirty, cell) {
			t.Errorf("batch dirtied %s and the loop did not", cell)
		}
	}
	t.Logf("loop dirty %d, batch dirty %d", len(loopDirty), len(res.Dirty))
}

// TestBatchEventLogMatchesTheLoop pins that batching did not turn N user-visible
// mutations into one log entry, and that the trim still fires where it fired.
func TestBatchEventLogMatchesTheLoop(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "events", 300, MaxCols)

	before, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	var writes []BatchWrite
	for r := 0; r < 25; r++ {
		writes = append(writes, BatchWrite{CellRef{Row: r, Col: 0}, strconv.Itoa(100 + r)})
	}
	if _, err := sh.ApplyBatch(writes); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}
	evs, err := sh.Events(before, 1000)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != len(writes) {
		t.Fatalf("batch of %d cells appended %d events, want one per cell",
			len(writes), len(evs))
	}
	for i, e := range evs {
		if e.Ref != writes[i].Ref.String() || e.Raw != writes[i].Raw {
			t.Errorf("event %d = (%s, %q), want (%s, %q)",
				i, e.Ref, e.Raw, writes[i].Ref, writes[i].Raw)
		}
		if want := fmtNum(seedLiteral(i, 0)); e.Prev != want {
			t.Errorf("event %d prev = %q, want %q (the value the cell held)", i, e.Prev, want)
		}
	}
}

// TestBatchEventTrimStillFires walks the log across a multiple of
// eventTrimEvery inside ONE batch, because that gate is now crossed inside a
// transaction rather than between them. maxEvents is far above what this test
// writes, so the trim runs and removes nothing — which is the case that must
// not error, since it is the one a 1,000-cell paste on a young sheet hits.
func TestBatchEventTrimStillFires(t *testing.T) {
	if testing.Short() {
		t.Skip("writes eventTrimEvery events; skipped under -short")
	}
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "trim", 1200, MaxCols)

	// One batch tall enough to step over seq 1000.
	writes := make([]BatchWrite, 0, eventTrimEvery+50)
	for i := 0; i < eventTrimEvery+50; i++ {
		writes = append(writes, BatchWrite{CellRef{Row: i, Col: 25}, strconv.Itoa(i)})
	}
	start := time.Now()
	if _, err := sh.ApplyBatch(writes); err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}
	t.Logf("%d cells across the trim gate in %.1f ms", len(writes), msf(time.Since(start)))
	seq, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if seq != int64(len(writes)) {
		t.Errorf("LastSeq = %d after %d events", seq, len(writes))
	}
	if got := countRows(t, sh, "events"); got != len(writes) {
		t.Errorf("log holds %d rows, want %d — the trim removed rows it should not have",
			got, len(writes))
	}
}

// TestBatchRecalcCapScalesWithTheBatch is the deliberate semantic choice, and
// it is the one the first attempt got wrong.
//
// maxRecalcNodes counts DISCOVERED cells and the budget is maxRecalcNodes PER
// WRITTEN CELL — exactly the bound the loop had, since the union of N cascades
// is never larger than their sum. So a batch is served if and only if every
// cell of it would have been served alone, and the batch that replaces a
// working loop cannot refuse.
//
// A flat cap over the union was tried first and REFUSED WORK THE LOOP SERVED:
// clearing 10,000 cells of the seeded sheet's column A discovers ~20,000
// dependents (U reads A, V reads U), where the loop saw a three-cell cascade ten
// thousand times over. TestBulkWriteCost runs exactly that case.
func TestBatchRecalcCapScalesWithTheBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two maxRecalcNodes-wide fan-outs; skipped under -short")
	}
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "batchcap")

	// Two roots, each with a fan-out just under the cap and DISJOINT from the
	// other's. Alone each is served; together their union is 9,998, which is
	// past maxRecalcNodes and inside 2*maxRecalcNodes.
	a1, b1 := CellRef{Row: 0, Col: 0}, CellRef{Row: 0, Col: 1}
	for _, r := range []CellRef{a1, b1} {
		if err := sh.WriteCell(r, "1",
			[]ComputedCell{{Ref: r, Computed: "1", Kind: KindNumber}}); err != nil {
			t.Fatalf("seed %s: %v", r, err)
		}
	}
	fanOutAt(t, sh, a1, maxRecalcNodes-1, 10)
	fanOutAt(t, sh, b1, maxRecalcNodes-1, 400)

	// Each alone: what the LOOP would have done, and it succeeds.
	for _, r := range []CellRef{a1, b1} {
		res, err := Recalc(sh, r, "5")
		if err != nil {
			t.Fatalf("the loop's own step (%s alone) was refused: %v", r, err)
		}
		if res.Stats.Nodes != maxRecalcNodes {
			t.Errorf("%s alone evaluated %d nodes, want %d", r, res.Stats.Nodes, maxRecalcNodes)
		}
	}

	// Together, as one batch. A flat maxRecalcNodes over the union would refuse
	// this; the per-written-cell budget serves it, which is the whole choice.
	writes := []BatchWrite{{a1, "5"}, {b1, "7"}}
	start := time.Now()
	res, err := RecalcBatch(sh, writes)
	if err != nil {
		t.Fatalf("a batch of two cells the loop would have served was refused: %v", err)
	}
	want := 2 * maxRecalcNodes
	if res.Stats.Nodes != want {
		t.Errorf("batch evaluated %d nodes, want %d", res.Stats.Nodes, want)
	}
	t.Logf("2 written + %d discovered = %d nodes in %.1f ms (the loop's two passes: %.1f ms)",
		res.Stats.Nodes-2, res.Stats.Nodes, msf(time.Since(start)), msf(res.Stats.Elapsed))

	// And the cap still bites: three more dependents put the union past
	// 2*maxRecalcNodes, which is past what the loop could have produced.
	fanOutAt(t, sh, a1, 3, 800)
	if _, err := RecalcBatch(sh, writes); !errors.Is(err, ErrRecalcTooLarge) {
		t.Fatalf("a batch past its scaled cap = %v, want ErrRecalcTooLarge", err)
	}

	// A SINGLE edit is len(writes) == 1 and therefore exactly maxRecalcNodes,
	// unchanged — TestRecalcCapRefusesCleanly is the same assertion from the
	// other side.
	if _, err := Recalc(sh, a1, "9"); !errors.Is(err, ErrRecalcTooLarge) {
		t.Fatalf("one edit past maxRecalcNodes = %v, want ErrRecalcTooLarge", err)
	}

	// THE REFUSAL IS CLEAN, and for a batch that matters more than for an edit:
	// the pass runs before ANY of the writes, so a refused 2,000-cell paste has
	// written none of them rather than a prefix.
	seqBefore, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if _, err := sh.ApplyBatch(writes); !errors.Is(err, ErrRecalcTooLarge) {
		t.Fatalf("ApplyBatch past the cap = %v, want ErrRecalcTooLarge", err)
	}
	seqAfter, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if seqAfter != seqBefore {
		t.Errorf("a refused batch appended %d events", seqAfter-seqBefore)
	}
	wantCell(t, sh, "A1", "1", "1")
	wantCell(t, sh, "B1", "1", "1")
}

// TestBatchRefusesBeforeWritingAnything pins the other refusal, which is the
// one a user can actually reach: a cell past MaxCellBytes somewhere in the
// middle of a batch. The loop would have written the prefix.
func TestBatchRefusesBeforeWritingAnything(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "toolong", 300, MaxCols)

	seqBefore, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	writes := []BatchWrite{
		{CellRef{Row: 0, Col: 25}, "fine"},
		{CellRef{Row: 1, Col: 25}, strings.Repeat("x", MaxCellBytes+1)},
		{CellRef{Row: 2, Col: 25}, "also fine"},
	}
	_, err = sh.ApplyBatch(writes)
	if !errors.Is(err, ErrCellTooLong) {
		t.Fatalf("ApplyBatch with an oversized cell = %v, want ErrCellTooLong", err)
	}
	if !errors.Is(err, ErrBadRef) {
		t.Errorf("the refusal must classify as ErrBadRef so callers answer 400: %v", err)
	}
	for _, r := range []string{"Z1", "Z2", "Z3"} {
		ref := mustRef(t, r)
		cell, cerr := sh.GetCell(ref)
		if cerr != nil {
			t.Fatalf("GetCell %s: %v", r, cerr)
		}
		if cell.Kind != KindEmpty {
			t.Errorf("%s = %q after a refused batch; nothing should have been written", r, cell.Raw)
		}
	}
	seqAfter, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if seqAfter != seqBefore {
		t.Errorf("a refused batch appended %d events", seqAfter-seqBefore)
	}
}

// TestLiteralRecalcBatch pins the degenerate engine a Server built without one
// gets, so that a range command on such a server behaves the way a single edit
// on it does: every written cell dirty, values inferred from their own text,
// nothing else touched.
func TestLiteralRecalcBatch(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "literal", 300, MaxCols)

	writes := []BatchWrite{
		{CellRef{Row: 0, Col: 25}, "5"},
		{CellRef{Row: 1, Col: 25}, "=A1*2"},
	}
	res, err := literalRecalcBatch(sh, writes)
	if err != nil {
		t.Fatalf("literalRecalcBatch: %v", err)
	}
	if len(res.Dirty) != len(writes) || len(res.Computed) != 0 {
		t.Fatalf("dirty %d / computed %d, want %d / 0", len(res.Dirty), len(res.Computed), len(writes))
	}
	if err := sh.WriteCells(writes, res.Computed); err != nil {
		t.Fatalf("WriteCells: %v", err)
	}
	// With no engine the formula stores its own text as its value, exactly as
	// WriteCell does for a computed-less literal write.
	wantCell(t, sh, "Z1", "5", "5")
	wantCell(t, sh, "Z2", "=A1*2", "=A1*2")

	if _, err := literalRecalcBatch(sh, []BatchWrite{{CellRef{Row: -1, Col: 0}, "x"}}); !errors.Is(err, ErrBadRef) {
		t.Errorf("an off-grid ref = %v, want ErrBadRef", err)
	}
}

// TestBatchEmptyIsANoOp — the range handlers skip cells that already say what
// they would write, so an idempotent second Ctrl+D arrives here with nothing.
func TestBatchEmptyIsANoOp(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "empty", 300, MaxCols)
	seqBefore, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	res, err := sh.ApplyBatch(nil)
	if err != nil {
		t.Fatalf("ApplyBatch(nil): %v", err)
	}
	if len(res.Dirty) != 0 || res.Wrote != 0 {
		t.Errorf("empty batch reported %d dirty / %d written", len(res.Dirty), res.Wrote)
	}
	seqAfter, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if seqAfter != seqBefore {
		t.Errorf("empty batch appended %d events", seqAfter-seqBefore)
	}
}

// ─── What the batch costs, against the loop it replaces ───────────────────────
//
// The baseline is RESULTS.md's table, taken end to end through the HTTP
// handlers on a seeded 10,000-row sheet: ~0.125 ms/cell for a range clear,
// ~0.130 for a fill, ~0.092 for a paste. What is measured here is the WRITE
// HALF of those commands — the loop the handlers run inside one actor turn —
// with both arms run back to back in ONE process against byte-identical copies
// of the same seeded file, which is the only instrument this project has found
// trustworthy (see DATA-MODEL.md's warning about measuring under load).
//
// The two shapes are reported separately and the difference between them is the
// point: writes into column A land on the per-band `SUM` in W and the cross-band
// X, so a thousand of them share one cascade; writes into column Z land on
// nothing, so there is no cascade to share.

// bulkFixtureRows states the grid these numbers were taken on, for the same
// reason costFixtureRows does: a fixture that changes size with a constant about
// NEW sheets stops being comparable with the table it is compared against.
const bulkFixtureRows = 10000

// bulkTemplate seeds a full sheet once and returns the path to the closed file.
// Every arm below starts from a byte-identical copy of it.
func bulkTemplate(t *testing.T, rows int) string {
	t.Helper()
	dir := t.TempDir()
	c := NewSheetCache(dir, 4, time.Minute)
	start := time.Now()
	if err := c.Seed("tmpl", rows, MaxCols); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close template: %v", err)
	}
	path := filepath.Join(dir, "tmpl.db")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat template: %v", err)
	}
	t.Logf("fixture: %d x %d seeded in %v, %.2f MB on disk",
		rows, MaxCols, time.Since(start).Round(time.Millisecond),
		float64(st.Size())/(1<<20))
	return path
}

// bulkCopy opens a fresh sheet from a byte-identical copy of the template.
func bulkCopy(t *testing.T, tmpl string) *Sheet {
	t.Helper()
	dir := t.TempDir()
	dst := filepath.Join(dir, "s.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		in, err := os.Open(tmpl + suffix)
		if err != nil {
			if suffix == "" {
				t.Fatalf("open template: %v", err)
			}
			continue
		}
		out, err := os.Create(dst + suffix)
		if err != nil {
			_ = in.Close()
			t.Fatalf("create %s: %v", dst+suffix, err)
		}
		if _, err := io.Copy(out, in); err != nil {
			t.Fatalf("copy template: %v", err)
		}
		_ = in.Close()
		if err := out.Close(); err != nil {
			t.Fatalf("close %s: %v", dst+suffix, err)
		}
	}
	// A long idle window, not the usual minute: the loop arm holds one handle
	// for the whole run and the cache's idle sweep does not count "in use" as
	// use, so under -race (which is ~40x slower) a minute expires mid-run and
	// the handle is closed underneath the test.
	c := NewSheetCache(dir, 4, time.Hour)
	t.Cleanup(func() { _ = c.Close() })
	return mustOpen(t, c, "s")
}

// bulkOp is one shape of bulk write: a name and a way to build n cells of it.
type bulkOp struct {
	name  string
	shape string // what the cascade looks like, which is what the cost tracks
	build func(n int) []BatchWrite
}

var bulkOps = []bulkOp{{
	// The shape RESULTS.md's table was taken on: a full-width rectangle, so
	// most of the cells are in columns nothing aggregates and only the column-A
	// ones reach the per-band SUM. This is the row that compares directly with
	// the ~0.125 ms/cell recorded there.
	name:  "clear rect",
	shape: "26 columns x n/26 rows — RESULTS.md's shape",
	build: func(n int) []BatchWrite {
		out := make([]BatchWrite, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, BatchWrite{CellRef{Row: i / MaxCols, Col: i % MaxCols}, ""})
		}
		return out
	},
}, {
	name:  "clear",
	shape: "column A — overlapping cascade (W, X, Y)",
	build: func(n int) []BatchWrite {
		out := make([]BatchWrite, 0, n)
		for r := 0; r < n; r++ {
			out = append(out, BatchWrite{CellRef{Row: r, Col: 0}, ""})
		}
		return out
	},
}, {
	name:  "fill",
	shape: "column A — overlapping cascade (W, X, Y)",
	build: func(n int) []BatchWrite {
		v := fmtNum(seedLiteral(0, 0))
		out := make([]BatchWrite, 0, n)
		for r := 1; r <= n; r++ {
			out = append(out, BatchWrite{CellRef{Row: r, Col: 0}, v})
		}
		return out
	},
}, {
	name:  "paste",
	shape: "columns B..K — partial cascade (V only)",
	build: func(n int) []BatchWrite {
		out := make([]BatchWrite, 0, n)
		for i := 0; i < n; i++ {
			row, col := i/10, 1+i%10
			out = append(out, BatchWrite{
				CellRef{Row: row, Col: col},
				fmtNum(seedLiteral(row+1000, col)),
			})
		}
		return out
	},
}, {
	name:  "independent",
	shape: "column Z — nothing reads it",
	build: func(n int) []BatchWrite {
		out := make([]BatchWrite, 0, n)
		for r := 0; r < n; r++ {
			out = append(out, BatchWrite{CellRef{Row: r, Col: 25}, strconv.Itoa(r)})
		}
		return out
	},
}}

// Gated behind SSBULK=1, the way shiftlab_test.go and stylelab_test.go gate
// their cost labs: the LOOP arm at 10,000 cells is ~9 seconds by construction
// (that is the number this whole change is about) and ~7 minutes under -race,
// which is not something every `go test` should pay.
//
//	SSBULK=1 go test -run TestBulkWriteCost -v
func TestBulkWriteCost(t *testing.T) {
	if testing.Short() || os.Getenv("SSBULK") == "" {
		t.Skip("set SSBULK=1 to run the bulk-write cost lab")
	}
	tmpl := bulkTemplate(t, bulkFixtureRows-1)
	sizes := []int{10, 100, 1000, 10000}

	t.Logf("%-12s %7s | %10s %8s | %10s %8s | %7s | %6s %6s | %9s %8s %8s",
		"op", "cells", "loop", "µs/cell", "batch", "µs/cell", "speedup", "dirtyL", "dirtyB",
		"recalc", "queries", "read")
	for _, op := range bulkOps {
		for _, n := range sizes {
			writes := op.build(n)

			// The loop: recalc and commit one cell at a time, which is exactly
			// what applyWrites and handleClear's body do.
			loopSheet := bulkCopy(t, tmpl)
			t0 := time.Now()
			loopDirty := loopWrites(t, loopSheet, writes)
			loopDur := time.Since(t0)

			// The batch: one cascade walk, one order, one transaction.
			batchSheet := bulkCopy(t, tmpl)
			t1 := time.Now()
			res, err := batchSheet.ApplyBatch(writes)
			batchDur := time.Since(t1)
			if err != nil {
				t.Fatalf("%s %d: ApplyBatch: %v", op.name, n, err)
			}

			// The numbers only mean anything if the two sheets agree, so this
			// is checked at every size rather than argued from the small ones.
			lo, hi := writes[0].Ref.Row, writes[0].Ref.Row
			for _, w := range writes {
				lo, hi = min(lo, w.Ref.Row), max(hi, w.Ref.Row)
			}
			windows := [][2]int{{lo, min(lo+120, hi)}, {max(lo, hi-120), hi}, {145, 205}}
			if a, b := goldenDump(t, loopSheet, windows, nil), goldenDump(t, batchSheet, windows, nil); a != b {
				dumpDiff(t, a, b)
				t.Fatalf("%s %d: batch and loop disagree", op.name, n)
			}

			t.Logf("%-12s %7d | %8.1f ms %8.0f | %8.1f ms %8.0f | %6.1fx | %6d %6d | %6.1f ms %8d %8d",
				op.name, n,
				msf(loopDur), float64(loopDur.Microseconds())/float64(n),
				msf(batchDur), float64(batchDur.Microseconds())/float64(n),
				float64(loopDur)/float64(batchDur),
				len(loopDirty), len(res.Dirty),
				msf(res.Stats.Elapsed), res.Stats.Queries, res.Stats.CellsRead)
		}
		t.Logf("%-12s   %s", "", op.shape)
	}
}

// TestBulkWriteHotPaths re-measures what the batch was not allowed to regress,
// on the same fixture and in the same process as the table above.
func TestBulkWriteHotPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds a full sheet; skipped under -short")
	}
	tmpl := bulkTemplate(t, bulkFixtureRows-1)
	sh := bulkCopy(t, tmpl)

	// The window read: 250 rows, which is the buffer every push renders.
	var win time.Duration
	for i := 0; i < 8; i++ {
		start := time.Now()
		cells, err := sh.Window(500, 749)
		d := time.Since(start)
		if err != nil {
			t.Fatalf("window: %v", err)
		}
		if i == 0 || d < win {
			win = d
		}
		if len(cells) != 250*MaxCols {
			t.Fatalf("window returned %d cells", len(cells))
		}
	}
	t.Logf("window read, 250 rows (fastest of 8): %.2f ms", msf(win))

	// The single-cell edit, through the same path a keystroke takes. Median of
	// 200, which is how DATA-MODEL.md reports it.
	ds := make([]time.Duration, 0, 200)
	for i := 0; i < 200; i++ {
		ref := CellRef{Row: 300 + i%50, Col: 25}
		raw := strconv.Itoa(i)
		start := time.Now()
		res, err := Recalc(sh, ref, raw)
		if err != nil {
			t.Fatalf("recalc: %v", err)
		}
		if err := sh.WriteCell(ref, raw, res.Computed); err != nil {
			t.Fatalf("write: %v", err)
		}
		ds = append(ds, time.Since(start))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	t.Logf("single-cell edit, median of 200: %d µs (fastest %d µs)",
		ds[len(ds)/2].Microseconds(), ds[0].Microseconds())

	// And the cascade edit, which is the one the batch's graph code touches
	// most: A1 reaches U1, V1, W1, X51 and the whole five-band Y chain.
	ds = ds[:0]
	for i := 0; i < 100; i++ {
		a1 := CellRef{Row: 0, Col: 0}
		raw := strconv.Itoa(1000 + i)
		start := time.Now()
		res, err := Recalc(sh, a1, raw)
		if err != nil {
			t.Fatalf("recalc A1: %v", err)
		}
		if err := sh.WriteCell(a1, raw, res.Computed); err != nil {
			t.Fatalf("write A1: %v", err)
		}
		ds = append(ds, time.Since(start))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	t.Logf("cascade edit (A1, 5 bands), median of 100: %d µs (fastest %d µs)",
		ds[len(ds)/2].Microseconds(), ds[0].Microseconds())
}

// ─── The single-cell regression instrument ────────────────────────────────────
//
// Recalc is now a one-element RecalcBatch, and "the single-cell path must not
// regress" is a claim that needs an instrument rather than an argument. So the
// PRE-BATCH pass is compiled into the test file — the same technique
// stylelab_test.go used for `windowV6` — and both arms run alternately against
// the SAME open sheet in one process, which is the only comparison this project
// has found trustworthy.
//
// recalcV1 is the body Recalc had before the batch existed, verbatim apart from
// the name. If it stops compiling, the shared helpers it leans on (cellSource,
// depGraph, nodePlan) have changed and this instrument needs re-deriving rather
// than deleting.
func recalcV1(sh *Sheet, ref CellRef, raw string) (RecalcResult, error) {
	start := time.Now()
	if sh == nil {
		return RecalcResult{}, errors.New("recalc: nil sheet")
	}
	if !ref.Valid() {
		return RecalcResult{}, fmt.Errorf("%w: %v out of grid", ErrBadRef, ref)
	}

	src := newCellSource(sh)
	src.overlay(ref, raw)
	newPrec, _ := Precedents(raw)

	g, err := discoverV1(sh, src, ref, newPrec)
	if err != nil {
		return RecalcResult{}, err
	}

	rows := make([]int, 0, len(g.nodes))
	for _, n := range g.nodes {
		rows = append(rows, n.Row)
	}
	if err := src.loadRows(rows); err != nil {
		return RecalcResult{}, err
	}

	plans := make([]nodePlan, len(g.nodes))
	rows = rows[:0]
	for i, n := range g.nodes {
		p := nodePlan{raw: raw}
		if i > 0 {
			c, err := src.cell(n)
			if err != nil {
				return RecalcResult{}, err
			}
			p.raw = c.Raw
		}
		p.kind = InferKind(p.raw)
		if p.kind == KindFormula {
			p.expr, p.err = ParseFormula(p.raw)
		}
		if p.kind == KindFormula && p.err == nil {
			for _, r := range p.expr.Refs() {
				rows = append(rows, r.Row)
			}
		}
		plans[i] = p
	}
	if err := src.loadRows(rows); err != nil {
		return RecalcResult{}, err
	}

	order, stuck, depth := g.topoSort()

	var res RecalcResult
	for _, i := range stuck {
		src.put(g.nodes[i], ComputedCell{Ref: g.nodes[i], Computed: TokenCycle, Kind: KindError})
	}
	for _, i := range order {
		n := g.nodes[i]
		p := plans[i]
		var (
			computed string
			kind     Kind
			err      error
		)
		switch {
		case p.kind == KindFormula && p.err != nil:
			computed, kind = ErrorToken(p.err), KindError
		case p.kind == KindFormula:
			computed, kind, err = EvalFormula(p.expr, src.value)
		default:
			computed, kind, err = EvalRaw(p.raw, src.value)
		}
		if err != nil {
			return RecalcResult{}, fmt.Errorf("recalc %s: %w", n, err)
		}
		src.put(n, ComputedCell{Ref: n, Computed: computed, Kind: kind})
	}
	for i, n := range g.nodes {
		cc, ok := src.result(n)
		if !ok {
			continue
		}
		old, err := src.stored(n)
		if err != nil {
			return RecalcResult{}, err
		}
		changed := cc.Computed != old.Computed || cc.Kind != old.Kind
		if changed {
			res.Dirty = append(res.Dirty, n)
		}
		if i == 0 || changed {
			res.Computed = append(res.Computed, cc)
		}
	}
	res.Stats = RecalcStats{
		Nodes:     len(g.nodes),
		Depth:     depth,
		Cycled:    len(stuck),
		Queries:   src.queries,
		CellsRead: src.cellsRead,
		Elapsed:   time.Since(start),
	}
	return res, nil
}

func discoverV1(sh *Sheet, src *cellSource, ref CellRef, newPrec []CellRef) (*depGraph, error) {
	g := &depGraph{index: make(map[CellRef]int, 16)}
	root, _ := g.add(ref)
	for i := 0; i < len(g.nodes); i++ {
		deps, err := sh.DependentsOf(g.nodes[i])
		src.queries++
		src.cellsRead += len(deps)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if d == ref {
				continue
			}
			j, added := g.add(d)
			g.edge(i, j)
			if added && len(g.nodes) > maxRecalcNodes {
				return nil, fmt.Errorf("%w: %s reaches more than %d cells",
					ErrRecalcTooLarge, ref, maxRecalcNodes)
			}
		}
	}
	for _, p := range newPrec {
		if j, ok := g.index[p]; ok {
			g.edge(j, root)
		}
	}
	return g, nil
}

// TestSingleCellPathNotRegressed alternates the two implementations against one
// sheet, so load lands on both arms equally, and checks that they agree as well
// as that they cost the same.
func TestSingleCellPathNotRegressed(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds a full sheet; skipped under -short")
	}
	tmpl := bulkTemplate(t, bulkFixtureRows-1)
	sh := bulkCopy(t, tmpl)

	type arm struct {
		name string
		fn   RecalcFunc
		ds   []time.Duration
	}
	arms := []*arm{
		{name: "v1 (pre-batch)", fn: recalcV1},
		{name: "batch core", fn: Recalc},
	}

	// Two shapes: a cell nothing reads (the keystroke case) and A1 (the
	// five-band cascade, where the graph code does the most work).
	for _, shape := range []struct {
		name string
		ref  func(i int) CellRef
		reps int
	}{
		{"no dependents (Z)", func(i int) CellRef { return CellRef{Row: 300 + i%50, Col: 25} }, 200},
		{"five-band cascade (A1)", func(int) CellRef { return CellRef{Row: 0, Col: 0} }, 60},
	} {
		for _, a := range arms {
			a.ds = a.ds[:0]
		}
		for i := 0; i < shape.reps; i++ {
			ref := shape.ref(i)
			raw := strconv.Itoa(1000 + i)
			var want RecalcResult
			for j, a := range arms {
				start := time.Now()
				res, err := a.fn(sh, ref, raw)
				a.ds = append(a.ds, time.Since(start))
				if err != nil {
					t.Fatalf("%s: %v", a.name, err)
				}
				if j == 0 {
					want = res
					continue
				}
				// The two arms must produce the same answer as well as the
				// same cost, and they are run against the same unwritten
				// sheet so they are comparing like with like.
				if !slices.Equal(sortedRefs(want.Dirty), sortedRefs(res.Dirty)) {
					t.Fatalf("%s dirty = %v, v1 = %v", a.name, res.Dirty, want.Dirty)
				}
				if len(want.Computed) != len(res.Computed) || want.Stats.Nodes != res.Stats.Nodes {
					t.Fatalf("%s: %d computed / %d nodes, v1: %d / %d", a.name,
						len(res.Computed), res.Stats.Nodes,
						len(want.Computed), want.Stats.Nodes)
				}
			}
			// Commit one of them so the next iteration is not a no-op.
			res, err := Recalc(sh, ref, raw)
			if err != nil {
				t.Fatalf("commit recalc: %v", err)
			}
			if err := sh.WriteCell(ref, raw, res.Computed); err != nil {
				t.Fatalf("commit write: %v", err)
			}
		}
		med := func(ds []time.Duration) time.Duration {
			c := append([]time.Duration(nil), ds...)
			sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
			return c[len(c)/2]
		}
		fastest := func(ds []time.Duration) time.Duration {
			c := append([]time.Duration(nil), ds...)
			sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
			return c[0]
		}
		v1m, v2m := med(arms[0].ds), med(arms[1].ds)
		t.Logf("recalc only, %s, %d alternating reps: v1 median %d µs / fastest %d µs | batch core median %d µs / fastest %d µs | delta %+.1f%%",
			shape.name, shape.reps,
			v1m.Microseconds(), fastest(arms[0].ds).Microseconds(),
			v2m.Microseconds(), fastest(arms[1].ds).Microseconds(),
			100*(float64(v2m)/float64(v1m)-1))
	}
}

// ─── Text through the real graph ──────────────────────────────────────────────
//
// The evaluator's unit tests drive a map lookup. These drive the STORE: recalc
// discovers the dependents, orders them, evaluates them and persists them, and
// what comes back out of GetCell is what a viewer sees. That is the difference
// between "eval returns text" and "a text value survives a round trip through
// SQLite, the band-key storage and the changed-value-only dirty rule".
func TestTextFlowsThroughRecalc(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)

	t.Run("bare ref to text, two hops", func(t *testing.T) {
		sh := mustSeed(t, c, "textchain", 300, MaxCols)
		// Column Z is the one nothing in the seed reads, so the chain is the
		// whole graph.
		mustSet(t, sh, "Z1", "hello")
		mustSet(t, sh, "Z2", "=Z1")
		mustSet(t, sh, "Z3", "=Z2")
		wantCell(t, sh, "Z2", "=Z1", "hello")
		wantCell(t, sh, "Z3", "=Z2", "hello")

		// A formula cell holding text is still a FORMULA cell: the kind
		// describes the cell, not the type its value landed on. The renderer
		// paints it blue and puts the source in data-r on that basis.
		if k := mustCell(t, sh, "Z2").Kind; k != KindFormula {
			t.Errorf("Z2 kind = %v, want formula", k)
		}

		// The cascade is real: changing the head moves both hops in one pass.
		res, err := Recalc(sh, mustRef(t, "Z1"), "world")
		if err != nil {
			t.Fatalf("recalc: %v", err)
		}
		if got := refStrings(res.Dirty); !slices.Equal(got, []string{"Z1", "Z2", "Z3"}) {
			t.Errorf("dirty = %v, want [Z1 Z2 Z3]", got)
		}
		if err := sh.WriteCell(mustRef(t, "Z1"), "world", res.Computed); err != nil {
			t.Fatalf("write: %v", err)
		}
		wantCell(t, sh, "Z3", "=Z2", "world")

		// And arithmetic on the far end of a text chain is still #VALUE!.
		mustSet(t, sh, "Z4", "=Z3*2")
		wantCell(t, sh, "Z4", "=Z3*2", TokenValue)

		// Turning the head back into a number heals the whole chain, error
		// included — nothing latched.
		mustSet(t, sh, "Z1", "6")
		wantCell(t, sh, "Z2", "=Z1", "6")
		wantCell(t, sh, "Z3", "=Z2", "6")
		wantCell(t, sh, "Z4", "=Z3*2", "12")
	})

	t.Run("a text header no longer breaks the column's SUM", func(t *testing.T) {
		sh := mustSeed(t, c, "textheader", 300, MaxCols)
		// THE REPORTED BUG, on the shape that provokes it: a label above a
		// column of numbers. Z1 is the header, Z2..Z4 the data, Z5 the total.
		// The totals live below the range they read so that neither of them is
		// inside the other's span.
		mustSet(t, sh, "Z1", "Widgets")
		mustSet(t, sh, "Z2", "1")
		mustSet(t, sh, "Z3", "2")
		mustSet(t, sh, "Z4", "3")
		mustSet(t, sh, "Z25", "=SUM(Z1:Z4)")
		wantCell(t, sh, "Z25", "=SUM(Z1:Z4)", "6")

		// Blanks below the data are skipped too, and the total still tracks an
		// edit inside the range.
		mustSet(t, sh, "Z26", "=SUM(Z1:Z20)")
		wantCell(t, sh, "Z26", "=SUM(Z1:Z20)", "6")
		mustSet(t, sh, "Z3", "20")
		wantCell(t, sh, "Z25", "=SUM(Z1:Z4)", "24")
		wantCell(t, sh, "Z26", "=SUM(Z1:Z20)", "24")

		// Editing the HEADER's text does not change either total, so the
		// changed-value-only rule keeps both out of the dirty set: a viewer on
		// those cells costs zero bytes for a rename.
		res, err := Recalc(sh, mustRef(t, "Z1"), "Gadgets")
		if err != nil {
			t.Fatalf("recalc header: %v", err)
		}
		if got := refStrings(res.Dirty); !slices.Equal(got, []string{"Z1"}) {
			t.Errorf("dirty after renaming the header = %v, want [Z1]", got)
		}
	})

	t.Run("an error inside a SUM still propagates", func(t *testing.T) {
		sh := mustSeed(t, c, "sumerror", 300, MaxCols)
		mustSet(t, sh, "Z1", "Header")
		mustSet(t, sh, "Z2", "1")
		mustSet(t, sh, "Z3", "=Z2/0")
		mustSet(t, sh, "Z25", "=SUM(Z1:Z4)")
		// Text and blank skipped, #DIV/0! carried through: an error is a cell
		// whose contribution is UNKNOWN, not one with nothing to contribute.
		wantCell(t, sh, "Z25", "=SUM(Z1:Z4)", TokenDiv0)

		// Fix the errored cell and the total settles on the numbers.
		mustSet(t, sh, "Z3", "=Z2/1")
		wantCell(t, sh, "Z25", "=SUM(Z1:Z4)", "2")

		// A #CYCLE! inside the range propagates by the same route, which is the
		// point of errors being values: recalc needed no special case.
		mustSet(t, sh, "Z4", "=Z25*2")
		wantCell(t, sh, "Z4", "=Z25*2", TokenCycle)
		wantCell(t, sh, "Z25", "=SUM(Z1:Z4)", TokenCycle)
	})
}
