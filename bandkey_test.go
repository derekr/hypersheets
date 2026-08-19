package main

import (
	"database/sql"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// ─── The band index, on its own ───────────────────────────────────────────────
//
// This file was the Stage 3 SPIKE: a parallel `xcells` table beside the real
// one, driven by hand-written SQL, checked against the row-keyed implementation
// cell for cell. It measured 2.59 ms against 328 ms for InsertRows(0,1) — 127x —
// with the read path's query plan unchanged, and that is why the design was
// promoted.
//
// The spike is gone because it IS the implementation now. What is left here is
// the part the spike deliberately did not build and therefore did not test:
// the rank<->key arithmetic on its own, BAND SPLITS, and the invariant that
// every stored key names exactly one display row. The cell-for-cell
// differential the spike used to provide lives in golden_test.go, pinned from
// the row-keyed implementation before the switch.

func TestBandIndexRankKeyRoundTrip(t *testing.T) {
	bi := canonicalBandIndex(keyStride)
	if err := bi.validate(); err != nil {
		t.Fatalf("canonical index: %v", err)
	}
	if bi.rows != DefaultRows {
		t.Fatalf("canonical index holds %d rows, want %d", bi.rows, DefaultRows)
	}
	for _, rank := range []int{0, 1, 49, 50, 51, 99, 100,
		DefaultRows / 2, DefaultRows - 2, DefaultRows - 1} {
		k := bi.keyOf(rank)
		wantK := int64(rank/BandHeight)*keyStride + int64(rank%BandHeight)
		if k != wantK {
			t.Errorf("keyOf(%d) = %d, want %d", rank, k, wantK)
		}
		if got := bi.rankOf(k); got != rank {
			t.Errorf("rankOf(keyOf(%d)) = %d", rank, got)
		}
	}
	// Off the grid has no key, and a key in a band's slack names no row. Both
	// come back as "no such thing" rather than as a plausible wrong answer.
	if k := bi.keyOf(-1); k != deadKey {
		t.Errorf("keyOf(-1) = %d, want deadKey", k)
	}
	if k := bi.keyOf(DefaultRows); k != deadKey {
		t.Errorf("keyOf(DefaultRows) = %d, want deadKey", k)
	}
	if r := bi.rankOf(BandHeight + 1); r != -1 {
		t.Errorf("rankOf(a key in band 0's slack) = %d, want -1", r)
	}
	if r := bi.rankOf(deadKey); r != -1 {
		t.Errorf("rankOf(deadKey) = %d, want -1", r)
	}
	// Keys ascend with ranks — the property the contiguous window read rests on.
	for rank := 1; rank < DefaultRows; rank++ {
		if bi.keyOf(rank) <= bi.keyOf(rank-1) {
			t.Fatalf("keys are not ascending at rank %d", rank)
		}
	}
}

// TestBandIndexRowMath exercises the three in-memory reshapes a mutation is
// made of, because each of them has to keep the total row count at exactly
// DefaultRows — a fixed grid is the whole reason an insert has a bottom edge to
// push things off.
func TestBandIndexRowMath(t *testing.T) {
	t.Run("insert shifts ranks by n without moving other bands", func(t *testing.T) {
		bi := canonicalBandIndex(keyStride)
		before := bi.keyOf(5000)
		bi.dropTail(1)
		bi.bands[0].nrows++
		bi.reindex()
		if err := bi.validate(); err != nil {
			t.Fatalf("after insert: %v", err)
		}
		// The cell that was rank 5000 is now rank 5001 and its key never moved.
		if got := bi.keyOf(5001); got != before {
			t.Errorf("rank 5001 has key %d, want the untouched %d", got, before)
		}
	})

	t.Run("dropTail crosses empty bands", func(t *testing.T) {
		bi := canonicalBandIndex(keyStride)
		dead := bi.dropTail(BandHeight + 10)
		if len(dead) != 2 {
			t.Fatalf("dropping %d rows spanned %d ranges, want 2", BandHeight+10, len(dead))
		}
		if bi.rows != DefaultRows-BandHeight-10 {
			t.Errorf("rows = %d after dropping %d", bi.rows, BandHeight+10)
		}
		last := bi.bands[len(bi.bands)-1]
		if last.nrows != 0 {
			t.Errorf("last band holds %d rows, want 0", last.nrows)
		}
		bi.appendTail(BandHeight + 10)
		if bi.rows != DefaultRows {
			t.Errorf("rows = %d after putting them back", bi.rows)
		}
		if err := bi.validate(); err != nil {
			t.Fatalf("after append: %v", err)
		}
	})

	t.Run("appendTail allocates a new band when the last is full", func(t *testing.T) {
		bi := canonicalBandIndex(keyStride)
		bi.bands[len(bi.bands)-1].slots = int64(BandHeight) // no slack at all
		bi.reindex()
		n := len(bi.bands)
		bi.dropTail(10)
		bi.appendTail(10)
		if len(bi.bands) != n {
			t.Errorf("band count %d -> %d for a tail that fits", n, len(bi.bands))
		}
		// Fill the last band to its brim, then ask for more: the only place the
		// index allocates a band it was not born with.
		last := &bi.bands[len(bi.bands)-1]
		grew := int(last.slots) - last.nrows
		last.nrows = int(last.slots)
		bi.reindex()
		bi.appendTail(5)
		if len(bi.bands) != n+1 {
			t.Fatalf("band count %d -> %d, want a new band", n, len(bi.bands))
		}
		if got := bi.bands[n].base; got != bi.bands[n-1].end() {
			t.Errorf("the new band starts at %d, want %d (just past the previous one)",
				got, bi.bands[n-1].end())
		}
		if bi.bands[n].nrows != 5 {
			t.Errorf("the new band holds %d rows, want 5", bi.bands[n].nrows)
		}
		if bi.rows != DefaultRows+grew+5 {
			t.Errorf("rows = %d, want %d", bi.rows, DefaultRows+grew+5)
		}
	})

	t.Run("split halves the interval and preserves every rank", func(t *testing.T) {
		bi := canonicalBandIndex(keyStride)
		bi.bands[3].nrows = 100
		bi.bands[4].nrows = 0
		bi.reindex()
		want := make([]int64, DefaultRows)
		for r := 0; r < DefaultRows; r++ {
			want[r] = bi.keyOf(r)
		}
		lo, hi, delta := bi.splitAt(3)
		if delta == 0 {
			t.Fatalf("splitting a 100-row band moved nothing")
		}
		if err := bi.validate(); err != nil {
			t.Fatalf("after split: %v", err)
		}
		if want := canonicalBands(DefaultRows) + 1; len(bi.bands) != want {
			t.Errorf("band count = %d, want %d", len(bi.bands), want)
		}
		for r := 0; r < DefaultRows; r++ {
			got := bi.keyOf(r)
			old := want[r]
			if old >= lo && old <= hi {
				old += delta
			}
			if got != old {
				t.Fatalf("rank %d moved to %d, want %d (shift range %d..%d by %d)",
					r, got, old, lo, hi, delta)
			}
		}
	})
}

// ─── Storage invariants ───────────────────────────────────────────────────────

// checkStoreInvariants asserts the two properties every read depends on and
// that no test asserts directly: every stored cell sits at a key that names a
// live display row, and every stored REFERENCE names one too (or is explicitly
// dead). A violation is the silent-misread failure mode — the sheet still
// renders, it just renders the wrong rows — so it is checked rather than hoped
// for.
func checkStoreInvariants(t *testing.T, sh *Sheet) {
	t.Helper()
	bi := sh.index()
	if err := bi.validate(); err != nil {
		t.Fatalf("band index: %v", err)
	}
	err := sh.use(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT k, col, ` + slotCols + ` FROM cells ORDER BY k, col`)
		if err != nil {
			return err
		}
		defer rows.Close()
		bad, cells := 0, 0
		for rows.Next() {
			var k int64
			var col int
			var slots nullSlots
			if err := rows.Scan(append([]any{&k, &col}, slots.scanArgs()...)...); err != nil {
				return err
			}
			cells++
			if bi.rankOf(k) < 0 {
				if bad++; bad < 5 {
					t.Errorf("cell at key %d col %d names no display row", k, col)
				}
			}
			for i := 0; i < maxSlots; i++ {
				rk := slots[2*i]
				if !rk.Valid || rk.Int64 < 0 {
					continue
				}
				if bi.rankOf(rk.Int64) < 0 {
					if bad++; bad < 5 {
						t.Errorf("cell at key %d col %d has slot %d naming key %d, which is no row",
							k, col, i, rk.Int64)
					}
				}
			}
		}
		if bad > 0 {
			t.Errorf("%d invariant violations over %d cells", bad, cells)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("invariant scan: %v", err)
	}
}

// ─── Band splits, which the spike did not build ───────────────────────────────

// TestBandSplitsUnderRepeatedInserts is the test for the part with no prior
// art. A band starts with BandHeight rows in a 4,096-slot interval, so inserts
// into it are free until it reaches bandSplitRows, at which point it is split in
// two — and a split HALVES the interval, so doing it over and over eventually
// runs the band out of slack and forces a whole-sheet rebalance.
//
// So this hammers one position hundreds of times and checks, after every single
// insert, that the sheet still says exactly what a spreadsheet should: the
// blank row appeared, the row that was there moved down, and the formulas that
// name them still name them. Then it checks that splits and a rebalance
// actually happened — a test that never triggered them would pass on a build
// where the whole mechanism was dead code.
func TestBandSplitsUnderRepeatedInserts(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "splits", 400, MaxCols)

	// A formula ABOVE the insertion point that reads BELOW it: the case where a
	// reference must follow a row through every split without being rewritten.
	mustSet(t, sh, "Z1", "=A30*2")
	mustSet(t, sh, "Z2", "=SUM(A25:A35)")
	base := seedLiteral(29, 0)

	var splits int
	var rebalanced int
	const inserts = 400
	for i := 0; i < inserts; i++ {
		d, err := sh.InsertRows(25, 1)
		if err != nil {
			t.Fatalf("insert %d at row 25: %v", i, err)
		}
		splits += d.Stats.BandsSplit
		if d.Stats.Rebalanced {
			rebalanced++
		}
		// The blank row is where it was asked for, and the row that used to be
		// there has moved down by exactly the number of inserts so far.
		if got := mustCell(t, sh, "A26"); got.Raw != "" {
			t.Fatalf("after %d inserts A26 = %q, want the blank row", i+1, got.Raw)
		}
		moved := fmt.Sprintf("A%d", 30+i+1)
		if got := mustCell(t, sh, moved); got.Computed != fmtNum(base) {
			t.Fatalf("after %d inserts %s = %q, want %v", i+1, moved, got.Computed, base)
		}
		// The formula never moved, was never rewritten, and still points at the
		// row it always pointed at.
		wantRef := fmt.Sprintf("=A%d*2", 30+i+1)
		if got := mustCell(t, sh, "Z1"); got.Raw != wantRef || got.Computed != fmtNum(base*2) {
			t.Fatalf("after %d inserts Z1 = {%q, %q}, want {%q, %v}",
				i+1, got.Raw, got.Computed, wantRef, base*2)
		}
		// And the range that straddles the insertion point expands, which is
		// the case that costs nothing at all under storage keys.
		wantSum := fmt.Sprintf("=SUM(A25:A%d)", 35+i+1)
		if got := mustCell(t, sh, "Z2"); got.Raw != wantSum {
			t.Fatalf("after %d inserts Z2 = %q, want %q", i+1, got.Raw, wantSum)
		}
	}
	checkStoreInvariants(t, sh)

	if splits == 0 {
		t.Errorf("%d inserts into one band caused no splits", inserts)
	}
	if rebalanced == 0 {
		t.Errorf("%d inserts into one band never exhausted a band's slack; "+
			"the rebalance path was not exercised", inserts)
	}
	t.Logf("%d inserts at one position: %d band splits, %d rebalances, %d storage bands",
		inserts, splits, rebalanced, len(sh.index().bands))

	// Everything below still reads correctly after all that reshaping.
	if got := mustCell(t, sh, "U1"); got.Raw != "=A1*2" {
		t.Errorf("U1 = %q after %d inserts, want =A1*2", got.Raw, inserts)
	}
	sum := 0.0
	for r := 50; r < 100; r++ {
		sum += seedLiteral(r, 0)
	}
	// W51 was the first row of band 1 and has been pushed down by every insert.
	w := fmt.Sprintf("W%d", 51+inserts)
	if got := mustCell(t, sh, w); got.Computed != fmtNum(sum) {
		t.Errorf("%s = %q, want %v", w, got.Computed, sum)
	}
}

// TestBandSplitsSurviveDeletes runs the other direction over the same ground:
// a sheet that has been split many times must delete correctly too, including
// deletes that span several storage bands at once.
func TestBandSplitsSurviveDeletes(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "splitdel", 400, MaxCols)

	for i := 0; i < 150; i++ {
		if _, err := sh.InsertRows(10, 1); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if len(sh.index().bands) <= BandOf(DefaultRows-1)+1 {
		t.Fatalf("150 inserts at one position produced no extra bands (%d)", len(sh.index().bands))
	}
	checkStoreInvariants(t, sh)

	// Take the inserted blank rows back out in one go: a delete spanning many
	// storage bands, most of them wholly consumed.
	if _, err := sh.DeleteRows(10, 150); err != nil {
		t.Fatalf("delete 150: %v", err)
	}
	checkStoreInvariants(t, sh)

	// The sheet is exactly the seeded one again.
	c2 := newTestCache(t, 4, time.Minute)
	fresh := mustSeed(t, c2, "fresh", 400, MaxCols)
	// Except for the one value the seed itself gets wrong: Y151 is planted with
	// a value that omits Y101, which IS inside SUM(Y1:Y101), and the delete
	// above legitimately recomputes it. Correct it on both sides so the
	// comparison is about the mutation and not about the fixture.
	mustSet(t, fresh, "Y151", "=SUM(Y1:Y101)")
	mustSet(t, sh, "Y151", "=SUM(Y1:Y101)")
	// Windows and the dependency graph only: the event log legitimately differs,
	// because this sheet has been mutated 151 times and that one has not.
	got := goldenDump(t, sh, goldenWindows, nil)
	want := goldenDump(t, fresh, goldenWindows, nil)
	if got != want {
		t.Errorf("insert 150 then delete 150 did not return the sheet to its seeded state")
		for i := 0; i < min(len(got), len(want)); i++ {
			if got[i] != want[i] {
				lo := max(0, i-120)
				t.Errorf("first difference at byte %d:\n got %q\nwant %q", i, got[lo:i+80], want[lo:i+80])
				break
			}
		}
	}
}

// TestRebalancePreservesEverything drives the O(sheet) fallback directly rather
// than waiting for a band to fill: it is the one path that re-keys every cell
// and every reference in the file, so "the sheet afterwards is the sheet
// before" is the only thing worth asserting about it.
func TestRebalancePreservesEverything(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "rebal", 400, MaxCols)
	mustSet(t, sh, "Z1", "=SUM(A1:A200)")
	mustSet(t, sh, "Z2", "=Z1*2")
	before := goldenDump(t, sh, goldenWindows, goldenProbes)

	err := sh.use(func(db *sql.DB) error {
		bi := sh.index().clone()
		var st MutateStats
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		// A stride that is not the canonical one, so every key really moves.
		if err := rebalance(tx, bi, 6000, &st); err != nil {
			return err
		}
		if err := writeBandIndex(tx, bi); err != nil {
			return err
		}
		if !st.Rebalanced || st.CellsMoved == 0 {
			t.Errorf("rebalance reported %d cells moved, rebalanced=%v", st.CellsMoved, st.Rebalanced)
		}
		return sh.commitIndex(tx, bi)
	})
	if err != nil {
		t.Fatalf("rebalance: %v", err)
	}
	if got := sh.index().bands[0].slots; got != 8192 {
		t.Errorf("rebalanced stride = %d, want 8192 (the next power of two above 6000+50)", got)
	}
	checkStoreInvariants(t, sh)
	if after := goldenDump(t, sh, goldenWindows, goldenProbes); after != before {
		t.Error("a rebalance changed what the sheet says")
	}

	// And the sheet still mutates correctly on its new layout.
	if _, err := sh.InsertRows(0, 1); err != nil {
		t.Fatalf("insert after rebalance: %v", err)
	}
	wantCell(t, sh, "A1", "", "")
	wantCell(t, sh, "U2", "=A2*2", fmtNum(seedLiteral(0, 0)*2))
	// Z1/Z2 moved down a row with everything else, and Z2's reference to Z1
	// moved with it.
	wantCell(t, sh, "Z3", "=Z2*2", mustCell(t, sh, "Z3").Computed)
	checkStoreInvariants(t, sh)
}

// TestBigInsertForcesRebalance is the other way a band runs out of slack: not
// hundreds of small inserts but one enormous one. 5,000 rows do not fit in a
// 4,096-slot band however empty it is, so the sheet has to be re-keyed with
// wider spacing first.
func TestBigInsertForcesRebalance(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "bigins", 300, MaxCols)

	d, err := sh.InsertRows(10, 5000)
	if err != nil {
		t.Fatalf("InsertRows(10, 5000): %v", err)
	}
	if !d.Stats.Rebalanced {
		t.Errorf("a 5,000-row insert did not rebalance; band slack cannot have held it")
	}
	checkStoreInvariants(t, sh)
	wantCell(t, sh, "A11", "", "")
	wantCell(t, sh, "A5011", fmtNum(seedLiteral(10, 0)), fmtNum(seedLiteral(10, 0)))
	wantCell(t, sh, "U5011", "=A5011*2", fmtNum(seedLiteral(10, 0)*2))
	// A formula above the insert whose reference is below it followed the row.
	wantCell(t, sh, "U1", "=A1*2", fmtNum(seedLiteral(0, 0)*2))
}

// TestStructuralKeepsInvariants runs the mutation shapes that golden_test.go
// hashes and additionally checks the storage-level invariants after each — the
// golden dump can only see what a reader can see, and a cell filed at a key
// that names no row is invisible to it until something else moves.
func TestStructuralKeepsInvariants(t *testing.T) {
	for _, tc := range structuralGoldenCases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCache(t, 4, time.Minute)
			sh := mustSeed(t, c, "inv", tc.rows, MaxCols)
			tc.apply(t, sh)
			checkStoreInvariants(t, sh)
		})
	}
}

// TestExpandedRangeKeepsItsValue is the licence for the thing the band-key model
// does NOT do.
//
// When an insert lands inside a range, the range expands to cover the new rows —
// and under storage keys that costs nothing at all: the low endpoint's key did
// not move, the high endpoint's key did not move, and the rows between them were
// renumbered by one integer. Nothing is written, so nothing is recomputed. That
// is only safe if an expanded range cannot change value, which is true because
// the rows it swallows are blank by construction: the insert PUT them there.
//
// The row-keyed implementation recomputed these anyway, because it had to
// rewrite the endpoints regardless. This test is what replaces that accident.
func TestExpandedRangeKeepsItsValue(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "expand")

	for i := 1; i <= 10; i++ {
		mustSet(t, sh, fmt.Sprintf("A%d", i), fmt.Sprint(i))
	}
	mustSet(t, sh, "C1", "=SUM(A1:A10)")
	mustSet(t, sh, "D1", "=C1*2")
	wantCell(t, sh, "C1", "=SUM(A1:A10)", "55")
	wantCell(t, sh, "D1", "=C1*2", "110")

	for _, at := range []int{5, 3, 9} {
		if _, err := sh.InsertRows(at, 2); err != nil {
			t.Fatalf("InsertRows(%d, 2): %v", at, err)
		}
	}
	// Six blank rows are now inside the range, which has grown to cover them.
	c1 := mustCell(t, sh, "C1")
	if c1.Raw != "=SUM(A1:A16)" {
		t.Errorf("C1 = %q, want =SUM(A1:A16) (the range expanded over the blanks)", c1.Raw)
	}
	if c1.Computed != "55" {
		t.Errorf("C1 = %q, want 55: expanding over blank rows cannot change a SUM", c1.Computed)
	}
	wantCell(t, sh, "D1", "=C1*2", "110")

	// And filling one of those blank rows does dirty the SUM, so the expansion
	// is a real dependency and not a cosmetic text change.
	mustSet(t, sh, "A6", "100")
	deps := refStrings(mustDependents(t, sh, mustRef(t, "A6").String()))
	if len(deps) == 0 {
		t.Errorf("filling a row inside the expanded range reaches nothing: %v", deps)
	}
}

// TestMutationAddsNoStaleValues is the general form of the same property, and
// the reason the three regenerated fixtures in golden_test.go are trustworthy:
// a structural mutation may leave a stale value it INHERITED, but it must never
// create one.
//
// "Stale" here means the stored computed value disagrees with evaluating the
// cell's own formula against the sheet as stored. The seeded fixture starts with
// exactly one (Y151), which is pinned below so that the day someone fixes the
// seed, this test says so instead of quietly passing.
func TestMutationAddsNoStaleValues(t *testing.T) {
	ops := []struct {
		name string
		run  func(sh *Sheet) error
	}{
		{"InsertRows(1,3)", func(sh *Sheet) error { _, err := sh.InsertRows(1, 3); return err }},
		{"InsertRows(75,2)", func(sh *Sheet) error { _, err := sh.InsertRows(75, 2); return err }},
		{"InsertRows(0,1)", func(sh *Sheet) error { _, err := sh.InsertRows(0, 1); return err }},
		{"DeleteRows(0,1)", func(sh *Sheet) error { _, err := sh.DeleteRows(0, 1); return err }},
		{"DeleteRows(48,4)", func(sh *Sheet) error { _, err := sh.DeleteRows(48, 4); return err }},
		{"InsertCols(1,1)", func(sh *Sheet) error { _, err := sh.InsertCols(1, 1); return err }},
		{"DeleteCols(0,2)", func(sh *Sheet) error { _, err := sh.DeleteCols(0, 2); return err }},
	}
	for _, tc := range ops {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCache(t, 4, time.Minute)
			sh := mustSeed(t, c, "stale", 300, MaxCols)
			before := staleCells(t, sh)
			if err := tc.run(sh); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			after := staleCells(t, sh)
			if len(after) > len(before) {
				t.Errorf("%s left %d stale values, up from %d:\n before %v\n after  %v",
					tc.name, len(after), len(before), before, after)
			}
		})
	}
}

func TestSeedFixtureStaleValue(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "seedstale", 300, MaxCols)
	got := staleCells(t, sh)
	// Y151 = SUM(Y1:Y101) covers rows 0..100 and Y101 (row 100) holds 2790, but
	// the seed plants v1+v51 and calls it a day. Nothing in the prototype reads
	// that value for anything, and correcting it would invalidate every fixture
	// taken from the previous storage model — so it is pinned instead.
	if len(got) != 1 || got[0] != "Y151" {
		t.Errorf("seeded sheet has stale values %v, want exactly [Y151]", got)
	}
}

// staleCells lists the formula cells whose stored value disagrees with a fresh
// evaluation of their own formula against the sheet as stored.
func staleCells(t *testing.T, sh *Sheet) []string {
	t.Helper()
	var out []string
	for lo := 0; lo < 500; lo += 100 {
		cells, err := sh.Window(lo, lo+99)
		if err != nil {
			t.Fatalf("window: %v", err)
		}
		for _, cell := range cells {
			if cell.Kind != KindFormula && cell.Kind != KindError {
				continue
			}
			want, kind, err := EvalRaw(cell.Raw, func(r CellRef) (Cell, error) {
				return sh.GetCell(r)
			})
			if err != nil {
				t.Fatalf("evaluate %s (%q): %v", cell.Ref, cell.Raw, err)
			}
			if want != cell.Computed || kind != cell.Kind {
				out = append(out, cell.Ref.String())
			}
		}
	}
	return out
}

// TestRowMutationsAtTheGridEdge covers the bottom of the fixed grid, which is
// where a row mutation has to do something asymmetric: an insert DESTROYS the
// last n rows (they fall off a grid that is always DefaultRows tall) and a delete
// CREATES n blank ones. Under storage keys those are the two places rows are
// added to and taken from a band's slack rather than moved, and neither is
// exercised by a mutation in the middle of the sheet.
// rowA1 is column A of a 1-BASED display row, so a test about the bottom of
// the grid can be written in terms of the extent instead of in literals.
func rowA1(row1 int) string { return "A" + strconv.Itoa(row1) }

func TestRowMutationsAtTheGridEdge(t *testing.T) {
	t.Run("insert at the last legal position", func(t *testing.T) {
		c := newTestCache(t, 4, time.Minute)
		sh := mustOpen(t, c, "edge1")
		// Expressed relative to the extent, not as literals: this is a test
		// about the BOTTOM EDGE of whatever grid a new sheet starts with, and
		// a literal row number silently turns it into a test about the middle
		// of a grown one the moment DefaultRows moves.
		mustSet(t, sh, "A1", "top")
		mustSet(t, sh, rowA1(DefaultRows-2), "second last")
		mustSet(t, sh, rowA1(DefaultRows-1), "last but one")

		if _, err := sh.InsertRows(DefaultRows-1, 1); err != nil {
			t.Fatalf("InsertRows(DefaultRows-1, 1): %v", err)
		}
		wantCell(t, sh, rowA1(DefaultRows-2), "second last", "second last")
		wantCell(t, sh, rowA1(DefaultRows-1), "last but one", "last but one")
		wantCell(t, sh, rowA1(DefaultRows), "", "")
		checkStoreInvariants(t, sh)
	})

	t.Run("insert straddling the last band", func(t *testing.T) {
		c := newTestCache(t, 4, time.Minute)
		sh := mustOpen(t, c, "edge2")
		mustSet(t, sh, rowA1(DefaultRows-10), "21")
		mustSet(t, sh, "B1", "=A"+strconv.Itoa(DefaultRows-10)+"*2")

		if _, err := sh.InsertRows(DefaultRows-15, 5); err != nil {
			t.Fatalf("InsertRows(DefaultRows-15, 5): %v", err)
		}
		wantCell(t, sh, rowA1(DefaultRows-5), "21", "21")
		// The reference followed the row without being rewritten.
		wantCell(t, sh, "B1", "=A"+strconv.Itoa(DefaultRows-5)+"*2", "42")
		checkStoreInvariants(t, sh)
	})

	t.Run("delete opens blank rows at the bottom", func(t *testing.T) {
		c := newTestCache(t, 4, time.Minute)
		sh := mustOpen(t, c, "edge3")
		mustSet(t, sh, rowA1(DefaultRows-1), "near the end")
		mustSet(t, sh, rowA1(DefaultRows), "7")
		// Below the deleted rows, so it survives and moves up with them.
		mustSet(t, sh, "B100", "=A"+strconv.Itoa(DefaultRows))

		if _, err := sh.DeleteRows(0, 3); err != nil {
			t.Fatalf("DeleteRows(0, 3): %v", err)
		}
		wantCell(t, sh, rowA1(DefaultRows-4), "near the end", "near the end")
		wantCell(t, sh, rowA1(DefaultRows-3), "7", "7")
		wantCell(t, sh, rowA1(DefaultRows), "", "")
		wantCell(t, sh, "B97", "=A"+strconv.Itoa(DefaultRows-3), "7")
		checkStoreInvariants(t, sh)

		// The grid is still exactly DefaultRows tall: the three deleted rows came
		// back as blanks at the bottom.
		if got := sh.index().rows; got != DefaultRows {
			t.Errorf("grid is %d rows tall after a delete, want %d", got, DefaultRows)
		}
	})

	t.Run("delete the entire grid", func(t *testing.T) {
		c := newTestCache(t, 4, time.Minute)
		sh := mustSeed(t, c, "edge4", 300, MaxCols)
		if _, err := sh.DeleteRows(0, DefaultRows); err != nil {
			t.Fatalf("DeleteRows(0, DefaultRows): %v", err)
		}
		cells, err := sh.Window(0, 399)
		if err != nil {
			t.Fatalf("window: %v", err)
		}
		for _, cell := range cells {
			if cell.Kind != KindEmpty || cell.Raw != "" || cell.Computed != "" {
				t.Fatalf("%s survived deleting every row: %+v", cell.Ref, cell)
			}
		}
		checkStoreInvariants(t, sh)
		// And it still works afterwards.
		mustSet(t, sh, "A1", "=1+1")
		wantCell(t, sh, "A1", "=1+1", "2")
	})
}

// TestWindowIsConsistentWithConcurrentStructuralMutations is the test for the
// hazard the band index creates and a single-threaded spike cannot see.
//
// A window read does two things that must agree: it turns display rows into a
// KEY RANGE, and it turns the keys that come back into DISPLAY ROWS. Both use
// the band index. If a structural mutation commits between them, the reader
// asks for the right keys and then labels them with the wrong rows — every row
// of a screen off by one, no error anywhere, and the cell the user is typing in
// silently becomes a different cell.
//
// So the index is immutable, installed by the same call that commits, and read
// under a lock held for the whole query. This test hammers that: readers check
// that the column of consecutive integers they seeded is STILL consecutive,
// which is false the moment a read mixes one mapping with another's data.
func TestWindowIsConsistentWithConcurrentStructuralMutations(t *testing.T) {
	c := newTestCache(t, 4, time.Hour)
	sh := mustOpen(t, c, "concurrent")
	const rows = 300
	for r := 0; r < rows; r++ {
		if err := sh.WriteCell(CellRef{Row: r, Col: 0}, fmt.Sprint(r), nil); err != nil {
			t.Fatalf("seed row %d: %v", r, err)
		}
	}

	const inserts = 120
	done := make(chan struct{})
	var wg sync.WaitGroup

	// One writer, as the actor would be.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < inserts; i++ {
			if _, err := sh.InsertRows(0, 1); err != nil {
				t.Errorf("insert %d: %v", i, err)
				return
			}
		}
	}()

	// Readers, as every viewer would be.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for reads := 0; ; reads++ {
				select {
				case <-done:
					return
				default:
				}
				cells, err := sh.Window(0, 250)
				if err != nil {
					t.Errorf("window: %v", err)
					return
				}
				prev := -1
				for i := 0; i < len(cells); i += MaxCols {
					cell := cells[i] // column A
					if cell.Computed == "" {
						if prev >= 0 {
							t.Errorf("row %d is blank below a filled row (value %d): "+
								"the window mixed two layouts", cell.Ref.Row, prev)
							return
						}
						continue
					}
					var v int
					if _, err := fmt.Sscanf(cell.Computed, "%d", &v); err != nil {
						t.Errorf("row %d holds %q", cell.Ref.Row, cell.Computed)
						return
					}
					if prev >= 0 && v != prev+1 {
						t.Errorf("column A reads %d then %d at row %d: the window "+
							"translated keys with a different band index than it "+
							"selected them with", prev, v, cell.Ref.Row)
						return
					}
					prev = v
				}
			}
		}()
	}
	wg.Wait()

	// And the sheet is exactly what the inserts should have left.
	wantCell(t, sh, "A1", "", "")
	wantCell(t, sh, fmt.Sprintf("A%d", inserts+1), "0", "0")
	wantCell(t, sh, fmt.Sprintf("A%d", inserts+2), "1", "1")
	checkStoreInvariants(t, sh)
}

// ─── Growth ───────────────────────────────────────────────────────────────────
//
// The grid used to be a fixed DefaultRows tall and every band-index invariant
// was stated against that constant. The extent is now a property of the sheet,
// so the tests below are the ones that could not exist before: they take a sheet
// well past 10,000 rows and check that the rank<->key mapping still says
// EXACTLY the right thing about every row, cell for cell, not merely that
// nothing errored.

// growTo lays out new rows the way a fresh sheet is laid out. That matters for
// a reason the fixed grid could never surface: a band that arrives holding
// thousands of rows makes every future insert into it move thousands of cells,
// which is the exact cost bandSplitRows exists to bound.
func TestGrowToLaysOutCanonicalBands(t *testing.T) {
	bi := canonicalBandIndex(keyStride)
	before := len(bi.bands)

	if bi.growTo(bi.rows) {
		t.Error("growTo to the current extent reported a change")
	}
	if !bi.growTo(DefaultRows + 5000) {
		t.Fatal("growTo did not report growing")
	}
	if bi.rows != DefaultRows+5000 {
		t.Fatalf("extent = %d, want %d", bi.rows, DefaultRows+5000)
	}
	if err := bi.validate(); err != nil {
		t.Fatalf("grown index: %v", err)
	}
	if got, want := len(bi.bands), before+5000/BandHeight; got != want {
		t.Errorf("grown index has %d bands, want %d", got, want)
	}
	for i, b := range bi.bands {
		if b.nrows > bandSplitRows {
			t.Fatalf("band %d arrived holding %d rows, past the %d-row split threshold",
				i, b.nrows, bandSplitRows)
		}
	}
	// Every rank still round-trips through its key, including across the seam
	// between the original layout and the rows that were added.
	for _, rank := range []int{0, DefaultRows - 2, DefaultRows - 1, DefaultRows,
		DefaultRows + 1, DefaultRows + 2500, bi.rows - 1} {
		if got := bi.rankOf(bi.keyOf(rank)); got != rank {
			t.Errorf("rank %d -> key %d -> rank %d", rank, bi.keyOf(rank), got)
		}
	}
	if k := bi.keyOf(bi.rows); k != deadKey {
		t.Errorf("keyOf(one past the extent) = %d, want deadKey", k)
	}
}

// maxKeyBandsFor is a function of the extent, and this is the bug it prevents.
// The old constant was 4*(BandOf(DefaultRows-1)+1) = 800, four times the band count of a
// 10,000-row sheet. A 1,000,000-row sheet has 20,000 bands in its CANONICAL
// layout, so a fixed 800 would call a perfectly healthy sheet crowded and force
// a full re-key — the one O(sheet) operation left — on every single delete.
func TestBandCeilingScalesWithTheExtent(t *testing.T) {
	for _, rows := range []int{1, BandHeight, DefaultRows, 250000, RowCeiling} {
		canonical := len(layoutFor(rows, keyStride).bands)
		if got := maxKeyBandsFor(rows); got < canonical {
			t.Errorf("a %d-row sheet is allowed %d bands but its canonical layout has %d",
				rows, got, canonical)
		}
	}
}

// A sheet grown well past the old cap must have the right rows at the right
// ranks — asserted cell for cell over the whole seeded population, not sampled.
func TestGrownSheetKeepsDisplayRanksCellForCell(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "grown", 400, MaxCols)

	// Two independent ways of getting taller, applied together: a write far
	// below the bottom, and inserts that find content in the last row.
	mustSet(t, sh, "C15000", "far")
	if got := sh.Rows(); got != 15000 {
		t.Fatalf("a write at row 15000 left the sheet %d rows tall", got)
	}
	mustSet(t, sh, fmt.Sprintf("A%d", 15000), "last")

	const inserts = 120
	for i := 0; i < inserts; i++ {
		if _, err := sh.InsertRows(0, 1); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	want := 15000 + inserts
	if got := sh.Rows(); got != want {
		t.Fatalf("extent = %d after %d inserts over a full last row, want %d",
			got, inserts, want)
	}
	checkStoreInvariants(t, sh)

	// EVERY seeded literal, at the rank it must now be at. 400 rows x 20
	// literal columns, read through the public display-space API in windows.
	for lo := 0; lo < 400; lo += 100 {
		cells, err := sh.Window(lo+inserts, lo+99+inserts)
		if err != nil {
			t.Fatalf("window: %v", err)
		}
		for i, cell := range cells {
			r, col := lo+i/MaxCols, i%MaxCols
			if col >= seedColU {
				continue
			}
			if got, want := cell.Computed, fmtNum(seedLiteral(r, col)); got != want {
				t.Fatalf("row %d (seed row %d) col %d = %q, want %q",
					cell.Ref.Row, r, col, got, want)
			}
		}
	}
	// The two cells that made the sheet grow moved down by exactly the inserts.
	wantCell(t, sh, fmt.Sprintf("C%d", 15000+inserts), "far", "far")
	wantCell(t, sh, fmt.Sprintf("A%d", 15000+inserts), "last", "last")
	// And a formula above the insertion point still names the row it named,
	// renumbered for free.
	if got := mustCell(t, sh, fmt.Sprintf("U%d", 1+inserts)); got.Raw != fmt.Sprintf("=A%d*2", 1+inserts) {
		t.Errorf("U%d = %q, want =A%d*2", 1+inserts, got.Raw, 1+inserts)
	}
}

// Growth has to compose with the two things that reshape the key space. This
// hammers one position on a sheet that is already past the old cap, so every
// insert both grows the sheet (the last row holds content) and pushes the band
// at the insertion point through splits and eventually a rebalance.
func TestBandSplitsAndRebalanceAfterGrowth(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "grownsplits", 300, MaxCols)

	mustSet(t, sh, "A12000", "bottom") // the sheet is now 12,000 rows and full at the end
	mustSet(t, sh, "Z1", "=A30*2")
	base := seedLiteral(29, 0)

	var splits, rebalanced int
	const inserts = 400
	for i := 0; i < inserts; i++ {
		d, err := sh.InsertRows(25, 1)
		if err != nil {
			t.Fatalf("insert %d at row 25: %v", i, err)
		}
		if d.Stats.Grew != 1 {
			t.Fatalf("insert %d did not grow the sheet (Grew=%d)", i, d.Stats.Grew)
		}
		splits += d.Stats.BandsSplit
		if d.Stats.Rebalanced {
			rebalanced++
		}
		if got := mustCell(t, sh, "A26"); got.Raw != "" {
			t.Fatalf("after %d inserts A26 = %q, want the blank row", i+1, got.Raw)
		}
		moved := fmt.Sprintf("A%d", 30+i+1)
		if got := mustCell(t, sh, moved); got.Computed != fmtNum(base) {
			t.Fatalf("after %d inserts %s = %q, want %v", i+1, moved, got.Computed, base)
		}
		wantRef := fmt.Sprintf("=A%d*2", 30+i+1)
		if got := mustCell(t, sh, "Z1"); got.Raw != wantRef {
			t.Fatalf("after %d inserts Z1 = %q, want %q", i+1, got.Raw, wantRef)
		}
		// The row that made every one of these inserts a growing one is still
		// at the bottom, and still there.
		if got := mustCell(t, sh, fmt.Sprintf("A%d", 12000+i+1)); got.Raw != "bottom" {
			t.Fatalf("after %d inserts the bottom row = %q, want \"bottom\"", i+1, got.Raw)
		}
	}
	checkStoreInvariants(t, sh)

	if got, want := sh.Rows(), 12000+inserts; got != want {
		t.Errorf("extent = %d, want %d", got, want)
	}
	if splits == 0 {
		t.Errorf("%d growing inserts into one band caused no splits", inserts)
	}
	if rebalanced == 0 {
		t.Errorf("%d growing inserts never forced a rebalance; the path that has "+
			"to rebuild a GROWN layout was not exercised", inserts)
	}
	// A rebalance re-keys into layoutFor(bi.rows) — the grown extent, not
	// DefaultRows — so the sheet must not have quietly shrunk back.
	if err := sh.index().validate(); err != nil {
		t.Errorf("band index after growth + splits + rebalance: %v", err)
	}
	t.Logf("%d growing inserts at one position on a %d-row sheet: %d splits, "+
		"%d rebalances, %d storage bands",
		inserts, sh.Rows(), splits, rebalanced, len(sh.index().bands))
}

// The ceiling is the only row refusal left, and it must refuse without moving
// anything.
func TestRowCeilingIsTheOnlyRefusal(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "ceiling")
	mustSet(t, sh, "A1", "keep")

	if _, err := sh.InsertRows(0, RowCeiling-DefaultRows+1); err == nil {
		t.Fatal("an insert past the ceiling was accepted")
	}
	if got := sh.Rows(); got != DefaultRows {
		t.Errorf("a refused insert left the sheet %d rows tall, want %d", got, DefaultRows)
	}
	wantCell(t, sh, "A1", "keep", "keep")

	// The largest legal insert is exactly the one that lands on the ceiling.
	if _, err := sh.InsertRows(0, RowCeiling-DefaultRows); err != nil {
		t.Fatalf("insert to exactly the ceiling: %v", err)
	}
	if got := sh.Rows(); got != RowCeiling {
		t.Fatalf("extent = %d, want %d", got, RowCeiling)
	}
	if err := sh.index().validate(); err != nil {
		t.Fatalf("band index at the ceiling: %v", err)
	}
	wantCell(t, sh, fmt.Sprintf("A%d", RowCeiling-DefaultRows+1), "keep", "keep")
}

// The worst case growth has: one write that takes a fresh sheet all the way to
// the ceiling. It allocates 20,000 storage bands in one go, which is the point
// at which "the band index is a small array and a binary search" stops being
// obviously true and has to be measured instead of assumed.
func TestGrowsToTheCeilingInOneWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a 1,000,000-row sheet; skipped under -short")
	}
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "ceilwrite")

	start := time.Now()
	mustSet(t, sh, fmt.Sprintf("C%d", RowCeiling), "edge")
	took := time.Since(start)

	if got := sh.Rows(); got != RowCeiling {
		t.Fatalf("extent = %d, want %d", got, RowCeiling)
	}
	if err := sh.index().validate(); err != nil {
		t.Fatalf("band index at the ceiling: %v", err)
	}
	wantCell(t, sh, fmt.Sprintf("C%d", RowCeiling), "edge", "edge")

	used, err := sh.UsedRows()
	if err != nil {
		t.Fatalf("UsedRows: %v", err)
	}
	if used != RowCeiling {
		t.Errorf("UsedRows = %d, want %d", used, RowCeiling)
	}
	// And it reads back through the windowed path, which is the one that has to
	// translate a key near 82 million into a display rank.
	cells, err := sh.Window(RowCeiling-2, RowCeiling-1)
	if err != nil {
		t.Fatalf("window at the ceiling: %v", err)
	}
	if len(cells) != 2*MaxCols {
		t.Fatalf("window returned %d cells, want %d", len(cells), 2*MaxCols)
	}
	if got := cells[MaxCols+2]; got.Raw != "edge" || got.Ref != (CellRef{RowCeiling - 1, 2}) {
		t.Errorf("last row of the grid = %+v, want C%d = \"edge\"", got, RowCeiling)
	}
	t.Logf("one write at row %d on a fresh sheet: %v, %d storage bands",
		RowCeiling, took.Round(time.Millisecond), len(sh.index().bands))
}
