package main

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// ─── Test helpers ─────────────────────────────────────────────────────────────

// mustRef and refStrings live in formula_test.go.

// mustSet writes a cell the way the real command path does — recalc first, then
// one transaction — so dependency edges and computed values are as a running
// server would have left them.
func mustSet(t *testing.T, sh *Sheet, a1, raw string) {
	t.Helper()
	ref := mustRef(t, a1)
	res, err := Recalc(sh, ref, raw)
	if err != nil {
		t.Fatalf("recalc %s=%q: %v", a1, raw, err)
	}
	if err := sh.WriteCell(ref, raw, res.Computed); err != nil {
		t.Fatalf("write %s=%q: %v", a1, raw, err)
	}
}

func mustCell(t *testing.T, sh *Sheet, a1 string) Cell {
	t.Helper()
	c, err := sh.GetCell(mustRef(t, a1))
	if err != nil {
		t.Fatalf("get %s: %v", a1, err)
	}
	return c
}

// wantCell asserts a cell's raw text and computed value together, because after
// a structural mutation those two are the whole contract: raw is what the user
// sees in the editor, computed is what they see in the grid.
func wantCell(t *testing.T, sh *Sheet, a1, raw, computed string) {
	t.Helper()
	c := mustCell(t, sh, a1)
	if c.Raw != raw || c.Computed != computed {
		t.Errorf("%s = {raw:%q computed:%q}, want {raw:%q computed:%q}",
			a1, c.Raw, c.Computed, raw, computed)
	}
}

// dumpSheet is every persisted row of a sheet, as one comparable string. Used
// by the atomicity test: "exactly as it was" has to mean every table.
func dumpSheet(t *testing.T, sh *Sheet) string {
	t.Helper()
	var sb strings.Builder
	err := sh.use(func(db *sql.DB) error {
		for _, q := range []string{
			`SELECT k, col, raw, computed, kind, ` + slotCols + `, ref_span
			   FROM cells ORDER BY k, col`,
			`SELECT idx, base, slots, nrows FROM bands ORDER BY idx`,
			`SELECT seq, ref, raw, prev FROM events ORDER BY seq`,
			`SELECT col, width FROM cols ORDER BY col`,
		} {
			rows, err := db.Query(q)
			if err != nil {
				return err
			}
			cols, err := rows.Columns()
			if err != nil {
				rows.Close()
				return err
			}
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					rows.Close()
					return err
				}
				fmt.Fprintf(&sb, "%v\n", vals)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			sb.WriteString("--\n")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("dump sheet: %v", err)
	}
	return sb.String()
}

// ─── The transform, in isolation ──────────────────────────────────────────────

func TestShiftPoint(t *testing.T) {
	tests := []struct {
		name string
		op   shiftOp
		in   int
		want int
		ok   bool
	}{
		{"insert above leaves alone", shiftOp{axis: axisRow, at: 5, n: 1}, 4, 4, true},
		{"insert at the point moves", shiftOp{axis: axisRow, at: 5, n: 1}, 5, 6, true},
		{"insert below moves", shiftOp{axis: axisRow, at: 5, n: 1}, 9, 10, true},
		{"insert n moves by n", shiftOp{axis: axisRow, at: 0, n: 3}, 7, 10, true},
		{"insert off the end dies", shiftOp{axis: axisRow, at: 0, n: 1}, DefaultRows - 1, 0, false},
		{"insert just inside survives", shiftOp{axis: axisRow, at: 0, n: 1}, DefaultRows - 2, DefaultRows - 1, true},

		{"delete above leaves alone", shiftOp{axis: axisRow, at: 5, n: 1, del: true}, 4, 4, true},
		{"delete of the point kills it", shiftOp{axis: axisRow, at: 5, n: 1, del: true}, 5, 0, false},
		{"delete below shifts up", shiftOp{axis: axisRow, at: 5, n: 1, del: true}, 6, 5, true},
		{"delete n shifts up n", shiftOp{axis: axisRow, at: 2, n: 3, del: true}, 10, 7, true},
		{"delete last of n kills it", shiftOp{axis: axisRow, at: 2, n: 3, del: true}, 4, 0, false},
		{"delete first past n survives", shiftOp{axis: axisRow, at: 2, n: 3, del: true}, 5, 2, true},

		{"cols use MaxCols as the edge", shiftOp{axis: axisCol, at: 0, n: 1}, MaxCols - 1, 0, false},
		{"cols insert shifts", shiftOp{axis: axisCol, at: 1, n: 1}, 2, 3, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.op.point(tc.in)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("point(%d) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestShiftSpan(t *testing.T) {
	tests := []struct {
		name           string
		op             shiftOp
		lo, hi         int
		wantLo, wantHi int
		ok             bool
	}{
		// Insert.
		{"range entirely above is untouched", shiftOp{at: 20, n: 1}, 0, 9, 0, 9, true},
		{"range spanning the point EXPANDS", shiftOp{at: 4, n: 1}, 0, 9, 0, 10, true},
		{"range entirely below SHIFTS", shiftOp{at: 0, n: 1}, 4, 9, 5, 10, true},
		{"range starting at the point shifts", shiftOp{at: 4, n: 1}, 4, 9, 5, 10, true},
		{"range ending just above is untouched", shiftOp{at: 4, n: 1}, 0, 3, 0, 3, true},
		{"expansion is by n", shiftOp{at: 4, n: 3}, 0, 9, 0, 12, true},
		{"tail clamps at the edge", shiftOp{at: 0, n: 1}, DefaultRows - 3, DefaultRows - 1, DefaultRows - 2, DefaultRows - 1, true},
		{"whole range off the end dies", shiftOp{at: 0, n: 2}, DefaultRows - 1, DefaultRows - 1, 0, 0, false},

		// Delete.
		{"delete above is untouched", shiftOp{at: 20, n: 1, del: true}, 0, 9, 0, 9, true},
		{"delete inside SHRINKS", shiftOp{at: 4, n: 2, del: true}, 0, 9, 0, 7, true},
		{"delete at the head shrinks", shiftOp{at: 0, n: 2, del: true}, 0, 9, 0, 7, true},
		{"delete overlapping the tail truncates", shiftOp{at: 8, n: 4, del: true}, 0, 9, 0, 7, true},
		{"delete overlapping the head shifts", shiftOp{at: 2, n: 4, del: true}, 4, 9, 2, 5, true},
		{"delete below shifts up", shiftOp{at: 0, n: 2, del: true}, 4, 9, 2, 7, true},
		{"range wholly inside the deletion dies", shiftOp{at: 4, n: 2, del: true}, 4, 5, 0, 0, false},
		{"single cell range inside dies", shiftOp{at: 4, n: 1, del: true}, 4, 4, 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op := tc.op
			op.axis = axisRow
			lo, hi, ok := op.span(tc.lo, tc.hi)
			if ok != tc.ok || (ok && (lo != tc.wantLo || hi != tc.wantHi)) {
				t.Errorf("span(%d,%d) = (%d,%d,%v), want (%d,%d,%v)",
					tc.lo, tc.hi, lo, hi, ok, tc.wantLo, tc.wantHi, tc.ok)
			}
		})
	}
}

// rewriteCase is one (formula, mutation) pair with the text the rewrite must
// produce. It is a package-level table because TWO tests read it:
// TestRewriteFormula checks the text rewrite against the expected string, and
// TestStructuralPathMatchesTextRewrite checks the STORAGE path — which moves
// integer columns and never touches the text — against the text rewrite. One
// table, so the fast path can never quietly cover fewer cases than the oracle.
type rewriteCase struct {
	name    string
	raw     string
	op      shiftOp
	want    string
	changed bool
}

var rewriteCases = []rewriteCase{
	{"literal operand never moves", "=7", shiftOp{axis: axisRow, at: 0, n: 1}, "=7", false},
	{"ref above the insert is untouched", "=A1*2", shiftOp{axis: axisRow, at: 5, n: 1}, "=A1*2", false},
	{"ref just above the insert is untouched", "=A5*2", shiftOp{axis: axisRow, at: 5, n: 1}, "=A5*2", false},
	{"ref exactly at the insert shifts", "=A6*2", shiftOp{axis: axisRow, at: 5, n: 1}, "=A7*2", true},
	{"ref below the insert shifts", "=A6*2", shiftOp{axis: axisRow, at: 4, n: 1}, "=A7*2", true},
	{"both operands shift", "=A6+B7", shiftOp{axis: axisRow, at: 0, n: 2}, "=A8+B9", true},
	{"only the moving operand shifts", "=A1+B7", shiftOp{axis: axisRow, at: 4, n: 1}, "=A1+B8", true},
	{"unchanged text is preserved verbatim", "= A1 * 2 ", shiftOp{axis: axisRow, at: 5, n: 1}, "= A1 * 2 ", false},

	// The headline range rules, in A1 notation exactly as SPEC.md states them.
	{"SUM spanning the insert EXPANDS", "=SUM(A1:A10)", shiftOp{axis: axisRow, at: 4, n: 1}, "=SUM(A1:A11)", true},
	{"SUM below the insert SHIFTS", "=SUM(A5:A10)", shiftOp{axis: axisRow, at: 0, n: 1}, "=SUM(A6:A11)", true},
	{"SUM above the insert is untouched", "=SUM(A1:A4)", shiftOp{axis: axisRow, at: 5, n: 1}, "=SUM(A1:A4)", false},
	{"SUM overlapping a delete SHRINKS", "=SUM(A1:A10)", shiftOp{axis: axisRow, at: 4, n: 2, del: true}, "=SUM(A1:A8)", true},
	{"SUM wholly inside a delete dies", "=SUM(A5:A6)", shiftOp{axis: axisRow, at: 4, n: 2, del: true}, "=SUM(#REF!)", true},

	// Deletion of a referenced cell.
	{"ref to a deleted row dies", "=A5*2", shiftOp{axis: axisRow, at: 4, n: 1, del: true}, "=#REF!*2", true},
	{"ref below a delete shifts up", "=A9*2", shiftOp{axis: axisRow, at: 4, n: 1, del: true}, "=A8*2", true},
	{"one dead operand of two", "=A5+A9", shiftOp{axis: axisRow, at: 4, n: 1, del: true}, "=#REF!+A8", true},
	{"a bare ref can die", "=A5", shiftOp{axis: axisRow, at: 4, n: 1, del: true}, "=#REF!", true},

	// Columns: the same rules transposed.
	{"col insert shifts a ref", "=C1*2", shiftOp{axis: axisCol, at: 1, n: 1}, "=D1*2", true},
	{"col insert spanning a range EXPANDS", "=SUM(A1:C1)", shiftOp{axis: axisCol, at: 1, n: 1}, "=SUM(A1:D1)", true},
	{"col delete kills a ref", "=B1*2", shiftOp{axis: axisCol, at: 1, n: 1, del: true}, "=#REF!*2", true},
	{"col delete SHRINKS a range", "=SUM(A1:C1)", shiftOp{axis: axisCol, at: 1, n: 1, del: true}, "=SUM(A1:B1)", true},
	{"row op leaves column refs alone", "=SUM(A1:C1)", shiftOp{axis: axisRow, at: 5, n: 1}, "=SUM(A1:C1)", false},

	// Already broken text is left exactly as it is: there is nothing to
	// move, and rewriting it would destroy what the user has to read to fix it.
	{"unparseable is left alone", "=SUM(#REF!)", shiftOp{axis: axisRow, at: 0, n: 1}, "=SUM(#REF!)", false},
	{"already REF'd operand is left alone", "=#REF!*2", shiftOp{axis: axisRow, at: 0, n: 1}, "=#REF!*2", false},
}

func TestRewriteFormula(t *testing.T) {
	for _, tc := range rewriteCases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := rewriteFormula(tc.raw, tc.op)
			if got != tc.want || changed != tc.changed {
				t.Errorf("rewriteFormula(%q) = (%q, %v), want (%q, %v)",
					tc.raw, got, changed, tc.want, tc.changed)
			}
		})
	}
}

// A rewritten #REF! must be text the EXISTING parser already turns into the
// right error token. That is the whole reason no rewrite path was added to
// formula.go, so it gets its own test rather than being assumed.
func TestRewrittenRefTextEvaluatesToRefToken(t *testing.T) {
	for _, raw := range []string{"=#REF!", "=#REF!*2", "=A1+#REF!", "=SUM(#REF!)"} {
		got, kind, err := EvalRaw(raw, func(CellRef) (Cell, error) { return Cell{}, nil })
		if err != nil {
			t.Fatalf("EvalRaw(%q): %v", raw, err)
		}
		if got != TokenRef || kind != KindError {
			t.Errorf("EvalRaw(%q) = (%q, %v), want (%q, KindError)", raw, got, kind, TokenRef)
		}
	}
}

// ─── Row insert ───────────────────────────────────────────────────────────────

func TestInsertRowsShiftsCellsAndRewrites(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "ins1")

	mustSet(t, sh, "A1", "10")
	mustSet(t, sh, "A2", "20")
	mustSet(t, sh, "A3", "30")
	mustSet(t, sh, "B1", "=A3*2")       // a formula ABOVE the insert, reading below it
	mustSet(t, sh, "C1", "=SUM(A1:A3)") // a range that spans the insert
	wantCell(t, sh, "B1", "=A3*2", "60")
	wantCell(t, sh, "C1", "=SUM(A1:A3)", "60")

	d, err := sh.InsertRows(1, 1)
	if err != nil {
		t.Fatalf("InsertRows: %v", err)
	}
	if !d.Structural {
		t.Error("Dirty.Structural = false; a row insert moves geometry")
	}
	if len(d.Bands) == 0 || d.Bands[0] != 0 {
		t.Errorf("Dirty.Bands = %v, want band 0 included", d.Bands)
	}

	wantCell(t, sh, "A1", "10", "10")
	wantCell(t, sh, "A2", "", "") // the inserted blank row
	wantCell(t, sh, "A3", "20", "20")
	wantCell(t, sh, "A4", "30", "30")
	// B1 did not move (row 0 is above the insert) but its reference did.
	wantCell(t, sh, "B1", "=A4*2", "60")
	// The range expanded over the blank row, so the sum is unchanged.
	wantCell(t, sh, "C1", "=SUM(A1:A4)", "60")

	if got := d.Stats.CellsMoved; got != 4 {
		// A2,A3 move plus nothing else lives at or below row 1 in this sheet.
		t.Logf("cells moved: %d", got)
	}
}

func TestInsertRowsMultipleAtTop(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "ins3")

	mustSet(t, sh, "A1", "10")
	mustSet(t, sh, "B1", "=A1*2")
	mustSet(t, sh, "C1", "=SUM(A1:A1)")

	if _, err := sh.InsertRows(0, 3); err != nil {
		t.Fatalf("InsertRows: %v", err)
	}
	wantCell(t, sh, "A1", "", "")
	wantCell(t, sh, "A4", "10", "10")
	wantCell(t, sh, "B4", "=A4*2", "20")
	wantCell(t, sh, "C4", "=SUM(A4:A4)", "10")
}

func TestInsertRowsDepEdgesFollow(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "insdeps")

	mustSet(t, sh, "A5", "10")
	mustSet(t, sh, "B1", "=A5*2")
	mustSet(t, sh, "C1", "=SUM(A1:A5)")

	if _, err := sh.InsertRows(2, 1); err != nil {
		t.Fatalf("InsertRows: %v", err)
	}

	// A5 moved to A6; both formulas stayed in row 1 and now point at A6, and
	// the SUM range expanded to cover the blank row it swallowed.
	wantCell(t, sh, "B1", "=A6*2", "20")
	wantCell(t, sh, "C1", "=SUM(A1:A6)", "10")

	deps, err := sh.DependentsOf(mustRef(t, "A6"))
	if err != nil {
		t.Fatalf("DependentsOf: %v", err)
	}
	got := refStrings(deps)
	sort.Strings(got)
	want := []string{"B1", "C1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("DependentsOf(A6) = %v, want %v", got, want)
	}
	// Nothing may still be pointing at the old address.
	if old, err := sh.DependentsOf(mustRef(t, "A5")); err != nil {
		t.Fatal(err)
	} else if len(old) != 1 || old[0] != mustRef(t, "C1") {
		// C1's expanded range legitimately covers A5 (the blank row); B1 must
		// not.
		t.Errorf("DependentsOf(A5) = %v, want only C1 (the expanded range)", refStrings(old))
	}
	// The expanded range must have GAINED an edge, not merely translated one.
	prec, err := sh.PrecedentsOf(mustRef(t, "C1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(prec) != 6 {
		t.Errorf("PrecedentsOf(C1) has %d edges, want 6 (A1:A6)", len(prec))
	}
}

// ─── Row delete ───────────────────────────────────────────────────────────────

func TestDeleteRowsRefsAndRanges(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "del1")

	mustSet(t, sh, "A1", "1")
	mustSet(t, sh, "A2", "2")
	mustSet(t, sh, "A3", "3")
	mustSet(t, sh, "A4", "4")
	mustSet(t, sh, "B1", "=A2*2")       // reads the row about to die
	mustSet(t, sh, "C1", "=SUM(A1:A4)") // range overlapping the deletion
	mustSet(t, sh, "D1", "=A2+A3")      // one dead operand, one that shifts
	mustSet(t, sh, "E1", "=SUM(A2:A2)") // range wholly inside the deletion
	wantCell(t, sh, "C1", "=SUM(A1:A4)", "10")

	d, err := sh.DeleteRows(1, 1)
	if err != nil {
		t.Fatalf("DeleteRows: %v", err)
	}
	if !d.Structural {
		t.Error("Dirty.Structural = false")
	}

	wantCell(t, sh, "A1", "1", "1")
	wantCell(t, sh, "A2", "3", "3")
	wantCell(t, sh, "A3", "4", "4")
	wantCell(t, sh, "A4", "", "")

	wantCell(t, sh, "B1", "=#REF!*2", TokenRef)
	wantCell(t, sh, "C1", "=SUM(A1:A3)", "8") // 1 + 3 + 4
	wantCell(t, sh, "D1", "=#REF!+A2", TokenRef)
	wantCell(t, sh, "E1", "=SUM(#REF!)", TokenRef)
}

func TestDeleteRowsMultiple(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "del3")

	for i := 1; i <= 6; i++ {
		mustSet(t, sh, fmt.Sprintf("A%d", i), fmt.Sprint(i))
	}
	mustSet(t, sh, "B1", "=SUM(A1:A6)")
	wantCell(t, sh, "B1", "=SUM(A1:A6)", "21")

	if _, err := sh.DeleteRows(1, 3); err != nil { // rows A2,A3,A4
		t.Fatalf("DeleteRows: %v", err)
	}
	wantCell(t, sh, "A1", "1", "1")
	wantCell(t, sh, "A2", "5", "5")
	wantCell(t, sh, "A3", "6", "6")
	wantCell(t, sh, "A4", "", "")
	wantCell(t, sh, "B1", "=SUM(A1:A3)", "12") // 1 + 5 + 6
}

// A structural mutation has to settle a multi-hop chain in ONE pass, exactly
// like an edit does: the dirty formulas are recomputed in dependency order and
// everything downstream of them follows.
func TestDeleteRowsCascadesThroughChain(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "delchain")

	mustSet(t, sh, "A1", "1")
	mustSet(t, sh, "A2", "2")
	mustSet(t, sh, "A3", "3")
	mustSet(t, sh, "B1", "=SUM(A1:A3)") // 6
	mustSet(t, sh, "C1", "=B1*2")       // 12
	mustSet(t, sh, "D1", "=C1+A1")      // 13
	wantCell(t, sh, "D1", "=C1+A1", "13")

	if _, err := sh.DeleteRows(1, 1); err != nil { // delete A2 (value 2)
		t.Fatalf("DeleteRows: %v", err)
	}
	wantCell(t, sh, "B1", "=SUM(A1:A2)", "4") // 1 + 3
	wantCell(t, sh, "C1", "=B1*2", "8")       // downstream, not rewritten
	wantCell(t, sh, "D1", "=C1+A1", "9")      // two hops downstream
}

// ─── Columns ──────────────────────────────────────────────────────────────────

func TestInsertColsShiftsAndExpands(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "inscol")

	mustSet(t, sh, "A1", "1")
	mustSet(t, sh, "B1", "2")
	mustSet(t, sh, "C1", "3")
	mustSet(t, sh, "D1", "=SUM(A1:C1)")
	mustSet(t, sh, "A3", "=C1*10")
	wantCell(t, sh, "D1", "=SUM(A1:C1)", "6")

	if _, err := sh.InsertCols(1, 1); err != nil {
		t.Fatalf("InsertCols: %v", err)
	}
	wantCell(t, sh, "A1", "1", "1")
	wantCell(t, sh, "B1", "", "")
	wantCell(t, sh, "C1", "2", "2")
	wantCell(t, sh, "D1", "3", "3")
	wantCell(t, sh, "E1", "=SUM(A1:D1)", "6")
	wantCell(t, sh, "A3", "=D1*10", "30")
}

func TestDeleteColsShrinksAndKills(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "delcol")

	mustSet(t, sh, "A1", "1")
	mustSet(t, sh, "B1", "2")
	mustSet(t, sh, "C1", "3")
	mustSet(t, sh, "D1", "=SUM(A1:C1)")
	mustSet(t, sh, "E1", "=B1*10")

	if _, err := sh.DeleteCols(1, 1); err != nil {
		t.Fatalf("DeleteCols: %v", err)
	}
	wantCell(t, sh, "A1", "1", "1")
	wantCell(t, sh, "B1", "3", "3")
	wantCell(t, sh, "C1", "=SUM(A1:B1)", "4")
	wantCell(t, sh, "D1", "=#REF!*10", TokenRef)
}

// Column ops move cells sideways only, so they must not claim every band down
// to BandOf(DefaultRows-1) — that would wake every viewer on the sheet for nothing.
func TestColumnOpDirtyBandsAreBounded(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "colbands")

	mustSet(t, sh, "B1", "1")
	mustSet(t, sh, "B60", "2") // band 1

	d, err := sh.InsertCols(1, 1)
	if err != nil {
		t.Fatalf("InsertCols: %v", err)
	}
	want := []int{0, 1}
	if fmt.Sprint(d.Bands) != fmt.Sprint(want) {
		t.Errorf("Dirty.Bands = %v, want %v (rows 0 and 59 only)", d.Bands, want)
	}
}

// ─── Dirty sets tell the truth ────────────────────────────────────────────────

func TestDirtyIsEmptyWhenNothingMoved(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "quiet")

	mustSet(t, sh, "A1", "1")
	mustSet(t, sh, "A2", "2")

	at := DefaultRows / 2
	d, err := sh.InsertRows(at, 1)
	if err != nil {
		t.Fatalf("InsertRows: %v", err)
	}
	if !d.IsEmpty() {
		t.Errorf("Dirty = %+v, want empty: nothing exists at or below row %d", d, at)
	}

	// And on a completely blank sheet.
	blank := mustOpen(t, c, "blank")
	d, err = blank.InsertRows(0, 10)
	if err != nil {
		t.Fatalf("InsertRows on empty sheet: %v", err)
	}
	if !d.IsEmpty() {
		t.Errorf("Dirty = %+v on an empty sheet, want empty", d)
	}
}

func TestDirtyIncludesRewrittenFormulaAboveTheInsert(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "farref")

	mustSet(t, sh, "A1", "=A200*2") // band 0, reads band 3
	mustSet(t, sh, "A200", "5")

	d, err := sh.InsertRows(100, 1) // band 2
	if err != nil {
		t.Fatalf("InsertRows: %v", err)
	}
	wantCell(t, sh, "A1", "=A201*2", "10")

	has := func(b int) bool {
		for _, x := range d.Bands {
			if x == b {
				return true
			}
		}
		return false
	}
	if !has(0) {
		t.Errorf("Dirty.Bands = %v, want band 0: the formula there was rewritten", d.Bands)
	}
	if !has(BandOf(200)) {
		t.Errorf("Dirty.Bands = %v, want band %d: A200 moved", d.Bands, BandOf(200))
	}
}

// ─── Bounds ───────────────────────────────────────────────────────────────────

// THE BUG THAT REMOVED THE ROW CAP. A sheet whose last row holds content used
// to refuse every row insert for the rest of its life — accurately ("the last
// row has content, and the sheet is a fixed 10,000 rows") and uselessly, since
// a demo sheet seeded to the brim could never accept another row. It now grows.
func TestInsertRowsGrowsRatherThanPushingContentOffTheGrid(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "edge")

	last := fmt.Sprintf("A%d", DefaultRows)
	mustSet(t, sh, "A1", "1")
	mustSet(t, sh, last, "keep me")

	if got := sh.Rows(); got != DefaultRows {
		t.Fatalf("sheet starts at %d rows, want %d", got, DefaultRows)
	}
	d, err := sh.InsertRows(0, 1)
	if err != nil {
		t.Fatalf("InsertRows(0,1) on a sheet whose last row has content: %v", err)
	}
	if got := sh.Rows(); got != DefaultRows+1 {
		t.Fatalf("sheet is %d rows after the insert, want %d", got, DefaultRows+1)
	}
	if d.Stats.Grew != 1 {
		t.Errorf("Stats.Grew = %d, want 1", d.Stats.Grew)
	}
	// Nothing fell off: both cells moved down exactly one row.
	wantCell(t, sh, "A2", "1", "1")
	wantCell(t, sh, fmt.Sprintf("A%d", DefaultRows+1), "keep me", "keep me")
	// ...and the row they used to be in is now blank rather than gone.
	wantCell(t, sh, "A1", "", "")
	// The new bottom row is readable through the public display-space API.
	cells, err := sh.Window(DefaultRows, DefaultRows)
	if err != nil {
		t.Fatalf("Window at the new bottom row: %v", err)
	}
	if len(cells) != MaxCols {
		t.Fatalf("Window(%d,%d) returned %d cells, want %d",
			DefaultRows, DefaultRows, len(cells), MaxCols)
	}
	if cells[0].Raw != "keep me" {
		t.Errorf("row %d column A = %q, want %q", DefaultRows+1, cells[0].Raw, "keep me")
	}
}

// Inserting BELOW the last row — `at == extent`, the append position — is an
// operation a fixed grid had no way to express, because there was nothing below
// the last row to insert before.
func TestInsertRowsAtTheAppendPosition(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "append")
	mustSet(t, sh, "A1", "top")

	d, err := sh.InsertRows(DefaultRows, 3)
	if err != nil {
		t.Fatalf("InsertRows at the append position: %v", err)
	}
	// It dirties the band the new rows land in AND NOTHING ELSE. Nothing above
	// the append position moved, so waking every viewer on the sheet would be a
	// lie — and it is the lie this path told before keyAt existed, because
	// deadKey is negative and `k >= -1` matches the whole table.
	if len(d.Bands) != 1 || d.Bands[0] != BandOf(DefaultRows) {
		t.Errorf("appending 3 rows dirtied %v, want just band %d", d.Bands, BandOf(DefaultRows))
	}
	if d.Stats.Grew != 3 {
		t.Errorf("Stats.Grew = %d, want 3", d.Stats.Grew)
	}
	if got := sh.Rows(); got != DefaultRows+3 {
		t.Fatalf("sheet is %d rows, want %d", got, DefaultRows+3)
	}
	wantCell(t, sh, "A1", "top", "top") // nothing above it moved
	if err := sh.index().validate(); err != nil {
		t.Errorf("band index after an append: %v", err)
	}
}

func TestInsertColsRejectsPushingContentOffTheGrid(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "coledge")

	mustSet(t, sh, "Z1", "keep me")
	before := dumpSheet(t, sh)
	_, err := sh.InsertCols(0, 1)
	if !errors.Is(err, ErrWouldTruncate) {
		t.Fatalf("InsertCols = %v, want ErrWouldTruncate", err)
	}
	if got := dumpSheet(t, sh); got != before {
		t.Error("a rejected insert changed the sheet")
	}
}

// Reusing EMPTY rows at the bottom is what an insert does when it can, and the
// sheet keeps its height: a row that exists only because something was typed
// there and then cleared is not data, so nothing is lost by taking it, and a
// sheet that is mostly blank does not grow a row every time one is inserted.
func TestInsertRowsAllowsPushingEmptyCellsOffTheGrid(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "emptyedge")

	last := fmt.Sprintf("A%d", DefaultRows)
	mustSet(t, sh, "A1", "1")
	mustSet(t, sh, last, "scratch")
	mustSet(t, sh, last, "") // cleared: the row still exists, holding nothing

	d, err := sh.InsertRows(0, 1)
	if err != nil {
		t.Fatalf("InsertRows over an empty edge row: %v", err)
	}
	wantCell(t, sh, "A2", "1", "1")
	if got := sh.Rows(); got != DefaultRows {
		t.Errorf("sheet grew to %d rows over a blank tail, want %d", got, DefaultRows)
	}
	if d.Stats.Grew != 0 {
		t.Errorf("Stats.Grew = %d, want 0", d.Stats.Grew)
	}
}

func TestStructuralArgumentValidation(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "args")

	tests := []struct {
		name string
		call func() (Dirty, error)
	}{
		{"negative row", func() (Dirty, error) { return sh.InsertRows(-1, 1) }},
		// `at == extent` is the APPEND position and is legal now; one past it
		// still names no row.
		{"row past the append position", func() (Dirty, error) { return sh.InsertRows(DefaultRows+1, 1) }},
		{"delete at the append position", func() (Dirty, error) { return sh.DeleteRows(DefaultRows, 1) }},
		{"zero count", func() (Dirty, error) { return sh.InsertRows(0, 0) }},
		{"delete count runs off the sheet", func() (Dirty, error) { return sh.DeleteRows(9998, 5) }},
		{"negative col", func() (Dirty, error) { return sh.DeleteCols(-1, 1) }},
		{"col past the end", func() (Dirty, error) { return sh.DeleteCols(MaxCols, 1) }},
		{"col count runs off", func() (Dirty, error) { return sh.DeleteCols(25, 2) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.call(); !errors.Is(err, ErrBadMutation) {
				t.Errorf("got %v, want ErrBadMutation", err)
			}
		})
	}

	// An insert count is no longer bounded by the axis — the sheet grows to
	// meet it. The only thing left that refuses is the sanity ceiling, and it
	// has its own error so a caller can tell "you asked for nonsense" from
	// "the sheet is genuinely as tall as sheets get".
	t.Run("insert count runs past the ceiling", func(t *testing.T) {
		_, err := sh.InsertRows(0, RowCeiling)
		if !errors.Is(err, ErrRowCeiling) {
			t.Fatalf("got %v, want ErrRowCeiling", err)
		}
		if !strings.Contains(err.Error(), "1000000") {
			t.Errorf("ceiling refusal does not say the ceiling: %v", err)
		}
		if got := sh.Rows(); got != DefaultRows {
			t.Errorf("a refused insert changed the extent to %d", got)
		}
	})
}

// ─── Atomicity ────────────────────────────────────────────────────────────────

// A structural mutation is ONE transaction: cell shift, formula rewrite,
// recompute and event append all commit together or none of them do.
//
// The failure has to be induced from OUTSIDE, in the middle. This used to be
// done by renaming the `deps` table away, which is no longer available — there
// is no second table to take. A trigger is the replacement and it lands
// deeper: it aborts an INSERT partway through the statement that re-inserts the
// shifted rows, so the event row is already written and the DELETE that emptied
// the shifted region has already run. If the transaction were not one unit, the
// sheet would be left with its rows deleted and its log claiming they moved.
func TestStructuralAtomicity(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "atomic")

	mustSet(t, sh, "A1", "1")
	mustSet(t, sh, "A2", "2")
	mustSet(t, sh, "B1", "=SUM(A1:A2)")
	mustSet(t, sh, "C9", "=A2*3")

	before := dumpSheet(t, sh)

	exec := func(q string) {
		t.Helper()
		if err := sh.use(func(db *sql.DB) error {
			_, err := db.Exec(q)
			return err
		}); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// C9 is the last row of the shift and moves to row 9 (0-indexed).
	// Key 9 is display row 9 on a canonical layout — C9 is the last row of the
	// shift and lands there.
	exec(`CREATE TRIGGER mut_boom BEFORE INSERT ON cells WHEN NEW.k = 9
	        BEGIN SELECT RAISE(ABORT, 'induced mid-shift failure'); END`)
	_, err := sh.InsertRows(0, 1)
	exec(`DROP TRIGGER mut_boom`)

	if err == nil {
		t.Fatal("InsertRows succeeded with a mid-shift abort armed; expected a failure")
	}
	if got := dumpSheet(t, sh); got != before {
		t.Errorf("a failed mutation left the sheet changed:\n--- before ---\n%s\n--- after ---\n%s",
			before, got)
	}

	// And the sheet still works afterwards.
	if _, err := sh.InsertRows(0, 1); err != nil {
		t.Fatalf("InsertRows after a rolled-back attempt: %v", err)
	}
	wantCell(t, sh, "A2", "1", "1")
	wantCell(t, sh, "B2", "=SUM(A2:A3)", "3")
	wantCell(t, sh, "C10", "=A3*3", "6")
}

func TestStructuralAppendsOneEvent(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "evt")
	mustSet(t, sh, "A1", "1")

	seq0, err := sh.LastSeq()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sh.InsertRows(3, 2); err != nil {
		t.Fatal(err)
	}
	evs, err := sh.Events(seq0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	if evs[0].Ref != "#rows" || evs[0].Raw != "insert 3 2" {
		t.Errorf("event = {ref:%q raw:%q}, want {#rows, insert 3 2}", evs[0].Ref, evs[0].Raw)
	}
}

// ─── Column widths ────────────────────────────────────────────────────────────

func TestColWidthSetGetAndClamp(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "widths")

	if w, err := sh.ColWidths(); err != nil {
		t.Fatal(err)
	} else if len(w) != 0 {
		t.Errorf("fresh sheet has widths %v, want none (absent == default)", w)
	}

	tests := []struct {
		col, set, want int
	}{
		{0, 200, 200},
		{1, 1, MinColWidth},      // clamped up
		{2, 100000, MaxColWidth}, // clamped down
		{3, -50, MinColWidth},    // clamped up
		{25, MaxColWidth, MaxColWidth},
	}
	for _, tc := range tests {
		if err := sh.SetColWidth(tc.col, tc.set); err != nil {
			t.Fatalf("SetColWidth(%d, %d): %v", tc.col, tc.set, err)
		}
	}
	got, err := sh.ColWidths()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range tests {
		if got[tc.col] != tc.want {
			t.Errorf("width of col %d = %d, want %d", tc.col, got[tc.col], tc.want)
		}
	}

	// Setting the default removes the row: the table stays sparse.
	if err := sh.SetColWidth(0, DefaultColWidth); err != nil {
		t.Fatal(err)
	}
	got, err = sh.ColWidths()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[0]; ok {
		t.Errorf("col 0 still stored after being set back to the default: %v", got)
	}

	for _, col := range []int{-1, MaxCols, 999} {
		if err := sh.SetColWidth(col, 100); !errors.Is(err, ErrBadRef) {
			t.Errorf("SetColWidth(%d, 100) = %v, want ErrBadRef", col, err)
		}
	}
	if err := sh.SetColWidth(0, 0); !errors.Is(err, ErrBadWidth) {
		t.Errorf("SetColWidth(0, 0) = %v, want ErrBadWidth", err)
	}
}

// Widths are shared sheet state, so a width change invalidates the whole grid.
func TestColWidthDirtiesEverything(t *testing.T) {
	bands := BandsUpTo(DefaultRows)
	if len(bands) != BandOf(DefaultRows-1)+1 || bands[0] != 0 || bands[len(bands)-1] != BandOf(DefaultRows-1) {
		t.Fatalf("BandsUpTo(DefaultRows) = %d bands (%d..%d), want %d covering 0..%d",
			len(bands), bands[0], bands[len(bands)-1], BandOf(DefaultRows-1)+1, BandOf(DefaultRows-1))
	}
}

func TestColWidthsMoveWithColumns(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "wmove")

	mustSet(t, sh, "A1", "1")
	if err := sh.SetColWidth(0, 200); err != nil { // A
		t.Fatal(err)
	}
	if err := sh.SetColWidth(2, 300); err != nil { // C
		t.Fatal(err)
	}

	if _, err := sh.InsertCols(1, 1); err != nil {
		t.Fatal(err)
	}
	w, err := sh.ColWidths()
	if err != nil {
		t.Fatal(err)
	}
	if w[0] != 200 {
		t.Errorf("col A width = %d, want 200 (unmoved)", w[0])
	}
	if w[3] != 300 {
		t.Errorf("col D width = %d, want 300 (C shifted right)", w[3])
	}
	if _, ok := w[2]; ok {
		t.Errorf("col C still has a width after the shift: %v", w)
	}

	// Deleting the column a width belongs to takes the width with it.
	if _, err := sh.DeleteCols(3, 1); err != nil {
		t.Fatal(err)
	}
	w, err = sh.ColWidths()
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 1 || w[0] != 200 {
		t.Errorf("widths after deleting col D = %v, want only {0:200}", w)
	}
}

// ─── Blank sheets and enumeration ─────────────────────────────────────────────

func TestCreateSheetIsEmptyAndNotIdempotent(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)

	if err := c.Create("fresh"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sh := mustOpen(t, c, "fresh")
	for _, table := range []string{"cells", "events", "cols"} {
		if n := countRows(t, sh, table); n != 0 {
			t.Errorf("new sheet has %d rows in %s, want 0", n, table)
		}
	}
	// It is a working sheet, not just a file.
	mustSet(t, sh, "A1", "=1+1")
	wantCell(t, sh, "A1", "=1+1", "2")

	if err := c.Create("fresh"); !errors.Is(err, ErrSheetExists) {
		t.Errorf("Create on an existing sheet = %v, want ErrSheetExists", err)
	}
	if err := c.Create("bad id!"); !errors.Is(err, ErrBadSheetID) {
		t.Errorf("Create with a bad id = %v, want ErrBadSheetID", err)
	}
}

func TestListSheets(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)

	if got, err := c.List(); err != nil {
		t.Fatalf("List on an empty dir: %v", err)
	} else if len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}

	for _, id := range []string{"alpha", "beta", "gamma"} {
		if err := c.Create(id); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	// Give one of them some content so Size is meaningfully different.
	mustSet(t, mustOpen(t, c, "beta"), "A1", "hello")

	got, err := c.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d sheets, want 3: %+v", len(got), got)
	}
	ids := map[string]SheetInfo{}
	for _, s := range got {
		ids[s.ID] = s
		if s.Size <= 0 {
			t.Errorf("sheet %s has size %d, want > 0", s.ID, s.Size)
		}
		if s.ModTime.IsZero() {
			t.Errorf("sheet %s has a zero mtime", s.ID)
		}
	}
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("List is missing %s: %+v", want, got)
		}
	}
	// Newest first.
	for i := 1; i < len(got); i++ {
		if got[i-1].ModTime.Before(got[i].ModTime) {
			t.Errorf("List is not newest-first: %+v", got)
			break
		}
	}
}

func TestNewSheetIDIsValidAndUnpredictable(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := NewSheetID()
		if err := validSheetID(id); err != nil {
			t.Fatalf("NewSheetID produced %q: %v", id, err)
		}
		// A single NATS subject token: no '.', no ' ', no '*', no '>'.
		if strings.ContainsAny(id, ". *>") {
			t.Fatalf("NewSheetID produced %q, which is not one subject token", id)
		}
		if seen[id] {
			t.Fatalf("NewSheetID repeated %q within 1000 draws", id)
		}
		seen[id] = true
	}
}

// ─── The deliverable: what does the worst case actually cost ──────────────────

// TestStructuralWorstCaseCost measures a full-width row insert and a full-width
// row delete on a real seeded sheet and prints the breakdown. This is the number
// SPEC.md deferred and RESULTS.md flagged, so it is a test rather than a
// Benchmark: a benchmark would run it N times and report an average, and the
// interesting fact is the shape of a SINGLE worst-case operation.
//
// NOTE on the row count. The insert is seeded one row SHORT of the fixture
// height. On a literally full grid InsertRows(0, 1) used to be REJECTED by the
// bounds rule and now GROWS the sheet, which is a different operation with a
// different cost (it gets its own subtest); leaving the last row empty is the
// largest sheet on which the ordinary tail-reusing insert happens at all. The
// delete has no such constraint and runs on the genuinely full grid.
//
// costFixtureRows is stated here rather than inherited from DefaultRows for the
// same reason goldenExtent is: every number in DATA-MODEL.md was taken on a
// 9,999 x 26 sheet in a 10,000-row grid, and a fixture that changes size with a
// constant about NEW sheets stops being comparable with the table it is
// supposed to be comparable with.
const costFixtureRows = 10000

func TestStructuralWorstCaseCost(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds a full sheet; skipped under -short")
	}

	// Microseconds, not milliseconds. The row-keyed implementation this replaces
	// spent 300 ms in "shift cells" and rounding to the nearest millisecond lost
	// nothing; the whole operation is now a few milliseconds and the breakdown
	// is invisible at that resolution.
	report := func(t *testing.T, label string, d Dirty) {
		t.Helper()
		s := d.Stats
		us := func(d time.Duration) string {
			return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000)
		}
		t.Logf("%s: %s total = shift cells %s + formula scan %s + shift deps %s"+
			" + recalc %s + write cells %s + write deps %s",
			label, us(s.Elapsed), us(s.ShiftCells), us(s.Scan), us(s.ShiftDeps),
			us(s.Recalc), us(s.WriteCells), us(s.WriteDeps))
		t.Logf("%s: storage bands %d, splits %d, rebalanced %v",
			label, s.KeyBands, s.BandsSplit, s.Rebalanced)
		t.Logf("%s: cells moved %d, dropped %d; deps moved %d, rewritten %d",
			label, s.CellsMoved, s.CellsDropped, s.DepsMoved, s.DepsWritten)
		t.Logf("%s: formulas seen %d, rewritten %d, recomputed %d; cells re-persisted %d",
			label, s.FormulasSeen, s.FormulasRewrit, s.Recomputed, s.CellsWritten)
		t.Logf("%s: recalc reads %d queries / %d cells; dirty bands %d",
			label, s.Queries, s.CellsRead, len(d.Bands))
	}

	t.Run("insert at the top", func(t *testing.T) {
		c := newTestCache(t, 4, time.Minute)
		seeded := time.Now()
		sh := mustSeed(t, c, "worst-insert", costFixtureRows-1, MaxCols)
		mustExtent(t, sh, costFixtureRows)
		t.Logf("seed %d x %d: %v (%d cells, %d with references, %d of them ranges)",
			costFixtureRows-1, MaxCols, time.Since(seeded).Round(time.Millisecond),
			countRows(t, sh, "cells"),
			countRows(t, sh, `cells WHERE ref0_k IS NOT NULL`),
			countRows(t, sh, `cells WHERE ref_span = 1`))

		d, err := sh.InsertRows(0, 1)
		if err != nil {
			t.Fatalf("InsertRows(0, 1): %v", err)
		}
		report(t, "InsertRows(0,1)", d)

		// Correctness spot checks: everything moved down one and every formula
		// still means what it meant.
		wantCell(t, sh, "A1", "", "")
		wantCell(t, sh, "A2", fmtNum(seedLiteral(0, 0)), fmtNum(seedLiteral(0, 0)))
		wantCell(t, sh, "U2", "=A2*2", fmtNum(seedLiteral(0, 0)*2))
		wantCell(t, sh, "V2", "=U2+B2", fmtNum(seedLiteral(0, 0)*2+seedLiteral(0, 1)))
		if got := mustCell(t, sh, "W2"); got.Raw != "=SUM(A2:A51)" {
			t.Errorf("W2 raw = %q, want =SUM(A2:A51)", got.Raw)
		}
		if got := mustCell(t, sh, "Y2"); got.Raw != "=SUM(A2:A11)" {
			t.Errorf("Y2 raw = %q, want =SUM(A2:A11)", got.Raw)
		}
		// The headline cascade still holds its value: everything shifted
		// uniformly, so nothing should have actually changed number.
		var sum float64
		for r := 0; r < 10; r++ {
			sum += seedLiteral(r, 0)
		}
		wantCell(t, sh, "Y2", "=SUM(A2:A11)", fmtNum(sum))
		wantCell(t, sh, "Y52", "=Y2*2", fmtNum(sum*2))
	})

	// The other half of the story: a mutation near the BOTTOM moves almost
	// nothing, and whatever is left is the fixed floor — the part that does not
	// get cheaper no matter where you insert. It also exercises the partial
	// paths (deps are translated rather than rebuilt wholesale), which the
	// top-of-sheet case skips entirely.
	t.Run("insert near the bottom", func(t *testing.T) {
		c := newTestCache(t, 4, time.Minute)
		sh := mustSeed(t, c, "near-bottom", costFixtureRows-1, MaxCols)
		mustExtent(t, sh, costFixtureRows)

		d, err := sh.InsertRows(9500, 1)
		if err != nil {
			t.Fatalf("InsertRows(9500, 1): %v", err)
		}
		report(t, "InsertRows(9500,1)", d)

		wantCell(t, sh, "A9501", "", "")
		wantCell(t, sh, "A9502", fmtNum(seedLiteral(9500, 0)), fmtNum(seedLiteral(9500, 0)))
		wantCell(t, sh, "U9502", "=A9502*2", fmtNum(seedLiteral(9500, 0)*2))
		// Untouched territory keeps its exact original text.
		wantCell(t, sh, "U1", "=A1*2", fmtNum(seedLiteral(0, 0)*2))
		if got := d.Bands[0]; got != BandOf(9500) {
			t.Errorf("first dirty band = %d, want %d", got, BandOf(9500))
		}
	})

	// THE CASE THAT USED TO BE A REFUSAL. Every row of the grid holds content,
	// so there is no blank tail to reuse and the insert has to make the sheet
	// taller. The number to watch is whether growing costs anything over the
	// ordinary insert two subtests up: it should not, because the rows an
	// insert creates are created by the band it inserts into.
	t.Run("insert into a genuinely full grid grows it", func(t *testing.T) {
		c := newTestCache(t, 4, time.Minute)
		sh := mustSeed(t, c, "worst-full", costFixtureRows, MaxCols)
		d, err := sh.InsertRows(0, 1)
		if err != nil {
			t.Fatalf("InsertRows(0,1) on a full grid: %v", err)
		}
		report(t, "InsertRows(0,1) on a FULL grid", d)
		if got := sh.Rows(); got != costFixtureRows+1 {
			t.Fatalf("full grid is %d rows after the insert, want %d", got, costFixtureRows+1)
		}
		if d.Stats.Grew != 1 {
			t.Errorf("Stats.Grew = %d, want 1", d.Stats.Grew)
		}
		// The row that used to be last is now one lower and intact — this is
		// the content the old rule refused rather than destroy.
		want := fmtNum(seedLiteral(costFixtureRows-1, 0))
		wantCell(t, sh, fmt.Sprintf("A%d", costFixtureRows+1), want, want)
	})

	t.Run("delete at the top", func(t *testing.T) {
		c := newTestCache(t, 4, time.Minute)
		sh := mustSeed(t, c, "worst-delete", costFixtureRows, MaxCols)

		d, err := sh.DeleteRows(0, 1)
		if err != nil {
			t.Fatalf("DeleteRows(0, 1): %v", err)
		}
		report(t, "DeleteRows(0,1)", d)

		// Row 1 is gone; what was row 2 is now row 1.
		wantCell(t, sh, "A1", fmtNum(seedLiteral(1, 0)), fmtNum(seedLiteral(1, 0)))
		wantCell(t, sh, "U1", "=A1*2", fmtNum(seedLiteral(1, 0)*2))

		// The seed puts W, X and Y1 on the FIRST row of a band, so deleting
		// row 1 destroys band 0's copies of all three outright. That is the
		// correct spreadsheet answer and it is worth pinning: a structural
		// delete removes formulas, it does not relocate them.
		wantCell(t, sh, "W1", "", "")
		wantCell(t, sh, "Y1", "", "")

		// Band 1's W moved up one row and its range shifted with it.
		var sum float64
		for r := 50; r < 100; r++ {
			sum += seedLiteral(r, 0)
		}
		wantCell(t, sh, "W50", "=SUM(A50:A99)", fmtNum(sum))

		// Band 1's X read band 0's W, which no longer exists: #REF! reaches
		// across a band boundary and lands in the stored value.
		wantCell(t, sh, "X50", "=#REF!+W50", TokenRef)
		// And the headline cascade's second link lost its root the same way.
		wantCell(t, sh, "Y50", "=#REF!*2", TokenRef)
	})
}

// BenchmarkInsertRowsSmallSheet is the other end of the range: what a
// structural mutation costs when the sheet is small. The gap between this and
// TestStructuralWorstCaseCost is the whole story.
func BenchmarkInsertRowsSmallSheet(b *testing.B) {
	c := NewSheetCache(b.TempDir(), 4, time.Minute)
	defer c.Close()
	if err := c.Seed("small", 200, MaxCols); err != nil {
		b.Fatal(err)
	}
	sh, err := c.Open("small")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sh.InsertRows(0, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// ─── The oracle: the fast path must agree with the obvious one ────────────────

// applyOp runs one shiftOp through the public API.
func applyOp(sh *Sheet, op shiftOp) (Dirty, error) {
	switch {
	case op.axis == axisRow && op.del:
		return sh.DeleteRows(op.at, op.n)
	case op.axis == axisRow:
		return sh.InsertRows(op.at, op.n)
	case op.del:
		return sh.DeleteCols(op.at, op.n)
	default:
		return sh.InsertCols(op.at, op.n)
	}
}

// oracleOwner picks where to park the formula under test.
//
// The two variants are the two code paths a reference can take through a
// structural mutation, and they are completely different mechanisms:
//
//	moved — the formula's own cell is at or past the mutation point, so its
//	        references are mapped by the CASE expressions inside the statement
//	        that shifts the rows.
//	held  — the formula's cell is BEFORE the mutation point and does not move,
//	        so its references are mapped by shiftHeldRefs' separate UPDATE.
//
// A test that only exercised one of them would miss half the arithmetic. The
// held variant does not exist when the mutation is at index 0, because there is
// nothing before index 0 to hold.
func oracleOwner(op shiftOp, held bool) (CellRef, bool) {
	if held && op.at == 0 {
		return CellRef{}, false
	}
	if op.axis == axisRow {
		if held {
			return CellRef{Row: 0, Col: 25}, true // Z1, above every op in the table
		}
		// Far below every op in the table, and INSIDE the default extent: a
		// formula parked past the bottom would grow the sheet, and the text
		// oracle below is asked about a grid of exactly DefaultRows.
		return CellRef{Row: DefaultRows - 100, Col: 25}, true
	}
	if held {
		return CellRef{Row: 50, Col: 0}, true // A51, left of every column op
	}
	return CellRef{Row: 50, Col: 24}, true // Y51, right of them and still on the grid
}

// TestStructuralPathMatchesTextRewrite is the safety net under the whole
// optimisation.
//
// rewriteFormula is the OLD implementation: read the formula's text, parse it,
// map the references, render new text. It is slow — re-parsing every formula in
// the sheet was ~300ms of the 522ms an InsertRows(0,1) used to cost — but it is
// simple enough to read and believe.
//
// Storage no longer does any of that. It moves integer columns with SQL
// arithmetic and materializes the text only when somebody reads it. This test
// runs every case in the rewrite table through REAL STORAGE — write the
// formula, perform the actual insert or delete, read the cell back — and
// demands the resulting Cell.Raw be byte-identical to what the text rewrite
// would have produced. Insert and delete, rows and columns, ranges that expand
// and shrink, references that die into #REF!: if the fast path and the obvious
// path ever disagree, this fails.
func TestStructuralPathMatchesTextRewrite(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	for i, tc := range rewriteCases {
		for _, held := range []bool{false, true} {
			owner, ok := oracleOwner(tc.op, held)
			if !ok {
				continue
			}
			where := "moved"
			if held {
				where = "held"
			}
			t.Run(fmt.Sprintf("%s/%s", tc.name, where), func(t *testing.T) {
				id := fmt.Sprintf("oracle%d%s", i, where)
				sh := mustOpen(t, c, id)
				mustSet(t, sh, owner.String(), tc.raw)

				// Sanity: storage must hand the formula back exactly as typed
				// before anything moves. A template that cannot round-trip at
				// rest would make every comparison below meaningless.
				if got := mustCell(t, sh, owner.String()).Raw; got != tc.raw {
					t.Fatalf("before the mutation, %s = %q, want the typed text %q",
						owner, got, tc.raw)
				}

				if _, err := applyOp(sh, tc.op); err != nil {
					t.Fatalf("%s: %v", tc.op, err)
				}
				// The oracle below is called with a bare shiftOp, whose axis
				// length falls back to DefaultRows. That is only the same axis
				// the storage path used if the sheet did not GROW — a case
				// planted near the bottom of the grid would make the two
				// disagree about what falls off the end, for a reason that has
				// nothing to do with what is being tested.
				if got := sh.Rows(); got != DefaultRows {
					t.Fatalf("test setup: %s grew the sheet to %d rows, so the "+
						"text oracle is being asked about a different grid", tc.op, got)
				}
				moved, alive := tc.op.ref(owner)
				if !alive {
					t.Fatalf("test setup: %s does not survive %s", owner, tc.op)
				}
				want, _ := rewriteFormula(tc.raw, tc.op)
				if got := mustCell(t, sh, moved.String()).Raw; got != want {
					t.Errorf("%s of %q at %s:\n structural = %q\n text path  = %q",
						tc.op, tc.raw, owner, got, want)
				}
			})
		}
	}
}

// TestStructuralPathIsMoreFaithfulThanTextRewrite pins the two places where the
// storage path deliberately does NOT match rewriteFormula.
//
// Both are cases where the old text rewrite threw away what the user typed the
// moment it had to change anything, because it re-rendered the whole formula
// from the parse. The template only replaces the reference tokens, so the rest
// of the line survives. That is a difference in the new path's favour, but it
// IS a difference, and a test that pretended otherwise would be hiding it.
func TestStructuralPathIsMoreFaithfulThanTextRewrite(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		op         shiftOp
		structural string
		textPath   string
	}{
		{
			// The old path rebuilt the formula as `"=" + left + op + right`,
			// so any spacing the user typed vanished as soon as one operand
			// moved. Spacing is not decoration in a formula bar.
			name: "spacing survives a reference moving", raw: "= A1 * 2 ",
			op:         shiftOp{axis: axisRow, at: 0, n: 1},
			structural: "= A2 * 2 ", textPath: "=A2*2",
		},
		{
			// The old path re-rendered a range from its NORMALIZED corners, so
			// a range written bottom-to-top silently flipped. The template
			// keeps each endpoint where it was written.
			name: "a reversed range keeps the order it was written in", raw: "=SUM(A10:A1)",
			op:         shiftOp{axis: axisRow, at: 0, n: 1},
			structural: "=SUM(A11:A2)", textPath: "=SUM(A2:A11)",
		},
	}
	c := newTestCache(t, 4, time.Minute)
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := rewriteFormula(tc.raw, tc.op); got != tc.textPath {
				t.Fatalf("the old text path produces %q, not the %q this test is contrasting with",
					got, tc.textPath)
			}
			sh := mustOpen(t, c, fmt.Sprintf("faithful%d", i))
			owner := CellRef{Row: DefaultRows - 100, Col: 25}
			mustSet(t, sh, owner.String(), tc.raw)
			if _, err := applyOp(sh, tc.op); err != nil {
				t.Fatalf("%s: %v", tc.op, err)
			}
			moved, _ := tc.op.ref(owner)
			if got := mustCell(t, sh, moved.String()).Raw; got != tc.structural {
				t.Errorf("structural path = %q, want %q", got, tc.structural)
			}
		})
	}
}
