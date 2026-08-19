package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestCache gives each test its own on-disk store and tears it down.
func newTestCache(t *testing.T, capacity int, idle time.Duration) *SheetCache {
	t.Helper()
	c := NewSheetCache(t.TempDir(), capacity, idle)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func mustOpen(t *testing.T, c *SheetCache, id string) *Sheet {
	t.Helper()
	sh, err := c.Open(id)
	if err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
	return sh
}

func countRows(t *testing.T, sh *Sheet, table string) int {
	t.Helper()
	var n int
	if err := sh.use(func(db *sql.DB) error {
		return db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
	}); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

func mustSeed(t *testing.T, c *SheetCache, id string, rows, cols int) *Sheet {
	t.Helper()
	if err := c.Seed(id, rows, cols); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	return mustOpen(t, c, id)
}

// ─── Basic read/write ─────────────────────────────────────────────────────────

func TestGetCellEmptyAndWritten(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	sh := mustOpen(t, c, "t1")

	got, err := sh.GetCell(CellRef{5, 5})
	if err != nil {
		t.Fatalf("GetCell on never-written cell: %v", err)
	}
	if got.Kind != KindEmpty || got.Raw != "" || got.Ref != (CellRef{5, 5}) {
		t.Fatalf("empty cell = %+v, want zero-value with ref set", got)
	}

	if _, err := sh.GetCell(CellRef{-1, 0}); err == nil {
		t.Fatalf("GetCell out of grid should error")
	}

	if err := sh.WriteCell(CellRef{5, 5}, "42", nil); err != nil {
		t.Fatalf("WriteCell: %v", err)
	}
	got, err = sh.GetCell(CellRef{5, 5})
	if err != nil {
		t.Fatalf("GetCell: %v", err)
	}
	if got.Raw != "42" || got.Computed != "42" || got.Kind != KindNumber {
		t.Fatalf("after literal write = %+v, want raw/computed 42 kind number", got)
	}

	// Overwrite with text.
	if err := sh.WriteCell(CellRef{5, 5}, "hello", nil); err != nil {
		t.Fatalf("WriteCell text: %v", err)
	}
	got, _ = sh.GetCell(CellRef{5, 5})
	if got.Kind != KindText || got.Computed != "hello" {
		t.Fatalf("after text write = %+v", got)
	}
}

func TestInferKind(t *testing.T) {
	tests := []struct {
		raw  string
		want Kind
	}{
		{"", KindEmpty},
		{"0", KindNumber},
		{"-12.5", KindNumber},
		{" 7 ", KindNumber},
		{"hello", KindText},
		{"=A1*2", KindFormula},
		{"=SUM(A1:A10)", KindFormula},
		{"=nonsense", KindFormula}, // validity is recalc's problem
	}
	for _, tc := range tests {
		if got := InferKind(tc.raw); got != tc.want {
			t.Errorf("InferKind(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// ─── Window ───────────────────────────────────────────────────────────────────

func TestWindowDenseAndOrdered(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	sh := mustSeed(t, c, "win", 300, MaxCols)

	cells, err := sh.Window(0, 9)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if len(cells) != 10*MaxCols {
		t.Fatalf("Window(0,9) returned %d cells, want %d", len(cells), 10*MaxCols)
	}
	for i, cell := range cells {
		wantRef := CellRef{Row: i / MaxCols, Col: i % MaxCols}
		if cell.Ref != wantRef {
			t.Fatalf("cell %d has ref %v, want %v (window must be dense + row-major)", i, cell.Ref, wantRef)
		}
	}
	// A1 is a literal from the seed; Z1 is the deliberately-empty column.
	if got := cells[0]; got.Computed != fmtNum(seedLiteral(0, 0)) || got.Kind != KindNumber {
		t.Fatalf("A1 = %+v, want seeded literal", got)
	}
	if got := cells[25]; got.Kind != KindEmpty || got.Raw != "" {
		t.Fatalf("Z1 = %+v, want empty", got)
	}
}

func TestWindowBoundaries(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	sh := mustSeed(t, c, "bounds", 300, MaxCols)

	tests := []struct {
		name           string
		lo, hi         int
		wantRows       int
		wantFirstRow   int
		wantLastRowIdx int
	}{
		{"single row", 0, 0, 1, 0, 0},
		{"band 0 exactly", 0, 49, 50, 0, 49},
		{"straddles a band edge", 49, 50, 2, 49, 50},
		{"past seeded rows", 290, 320, 31, 290, 320},
		{"clamped to grid end", DefaultRows - 10, DefaultRows * 2, 10, DefaultRows - 10, DefaultRows - 1},
		{"clamped at zero", -20, 1, 2, 0, 1},
		{"swapped args", 9, 0, 10, 0, 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cells, err := sh.Window(tc.lo, tc.hi)
			if err != nil {
				t.Fatalf("Window(%d,%d): %v", tc.lo, tc.hi, err)
			}
			if len(cells) != tc.wantRows*MaxCols {
				t.Fatalf("Window(%d,%d) = %d cells, want %d rows worth (%d)",
					tc.lo, tc.hi, len(cells), tc.wantRows, tc.wantRows*MaxCols)
			}
			if cells[0].Ref.Row != tc.wantFirstRow {
				t.Fatalf("first row = %d, want %d", cells[0].Ref.Row, tc.wantFirstRow)
			}
			if last := cells[len(cells)-1].Ref; last.Row != tc.wantLastRowIdx || last.Col != MaxCols-1 {
				t.Fatalf("last cell = %v, want row %d col %d", last, tc.wantLastRowIdx, MaxCols-1)
			}
		})
	}

	// A window entirely past the end of the grid is empty, not an error and
	// not a silently-clamped last row.
	if cells, err := sh.Window(DefaultRows, DefaultRows+10); err != nil || len(cells) != 0 {
		t.Fatalf("Window past end = %d cells, err %v; want 0 cells and no error", len(cells), err)
	}
}

// TestHotPathQueryPlans guards the schema claims in store.go: the windowed
// read must be a primary-key range scan (not a table scan, and with no sort
// step), and both dependency directions must be index lookups. A regression
// here is invisible in correctness tests and fatal to the whole design.
func TestHotPathQueryPlans(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "plan", 500, MaxCols)

	plan := func(query string, args ...any) string {
		t.Helper()
		var out string
		err := sh.use(func(db *sql.DB) error {
			rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var a, b, cc int
				var detail string
				if err := rows.Scan(&a, &b, &cc, &detail); err != nil {
					return err
				}
				out += detail + "\n"
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatalf("explain %q: %v", query, err)
		}
		return out
	}

	cases := []struct {
		name    string
		query   string
		args    []any
		want    string
		notWant []string
	}{
		{
			name:    "windowed read",
			query:   `SELECT k, col, raw, computed, kind FROM cells WHERE k BETWEEN 100 AND 349 ORDER BY k, col`,
			want:    "SEARCH cells USING PRIMARY KEY",
			notWant: []string{"SCAN cells", "TEMP B-TREE"},
		},
		// The reverse dependency walk is three index seeks now, one per arm of
		// dependentsSQL. Each arm must find its own partial index: an arm that
		// falls back to a table scan is a 220,000-row scan PER HOP of a
		// cascade, which is the failure this test exists to catch.
		{
			name: "reverse dep walk, single-ref arm 0",
			query: `SELECT k, col FROM cells
			          WHERE ref_span = 0 AND ref0_k IS NOT NULL
			            AND ref0_k = 5 AND ref0_col = 0`,
			want:    "COVERING INDEX cells_ref0",
			notWant: []string{"SCAN cells", "TEMP B-TREE"},
		},
		{
			name: "reverse dep walk, single-ref arm 1",
			query: `SELECT k, col FROM cells
			          WHERE ref_span = 0 AND ref1_k IS NOT NULL
			            AND ref1_k = 5 AND ref1_col = 0`,
			want:    "COVERING INDEX cells_ref1",
			notWant: []string{"SCAN cells", "TEMP B-TREE"},
		},
		{
			// The interval arm. It cannot be a single seek — "which ranges
			// contain this cell" is not a b-tree question — so what is being
			// pinned is that the scan is over a SPAN index (202 entries on a
			// seeded sheet, bounded by the number of range formulas) and never
			// over the cells table. Both directions, because the whole point of
			// having two is that either may be the one that runs.
			name:    "reverse dep walk, span arm from the low endpoint",
			query:   dependentsSQLLo,
			args:    []any{5, 0},
			want:    "COVERING INDEX cells_span_lo",
			notWant: []string{"SCAN cells", "TEMP B-TREE"},
		},
		{
			name:    "reverse dep walk, span arm from the high endpoint",
			query:   dependentsSQLHi,
			args:    []any{9500, 0},
			want:    "COVERING INDEX cells_span_hi",
			notWant: []string{"SCAN cells", "TEMP B-TREE"},
		},
		// The three probes a STRUCTURAL mutation is made of. Each one replaced a
		// scan of the whole table, and each one silently becomes a scan again if
		// its predicate stops implying its partial index — which is invisible in
		// a correctness test and is the entire cost of a row insert.
		{
			name: "structural: references naming a destroyed key range",
			query: `SELECT k, col FROM cells
			          WHERE ref_span = 0 AND ref0_k IS NOT NULL AND ref0_k BETWEEN 4096 AND 4145`,
			want:    "COVERING INDEX cells_ref0",
			notWant: []string{"SCAN cells", "TEMP B-TREE"},
		},
		{
			// A delete does not break a range that merely CONTAINS one of the
			// rows it removes — both endpoints survive — but it does change its
			// value, so the overlap has to be found. It is an interval query and
			// therefore a scan of the SPAN index, bounded by the number of range
			// formulas, never by the sheet.
			name: "structural: ranges overlapping a deleted run",
			query: `SELECT k, col, raw FROM cells INDEXED BY cells_span_hi
			          WHERE ref_span = 1 AND ref0_k <= 4145 AND ref1_k >= 4096`,
			want:    "INDEX cells_span_hi",
			notWant: []string{"SCAN cells", "TEMP B-TREE"},
		},
		{
			name: "structural: formulas above the mutation naming a row below it",
			query: `SELECT k FROM cells
			          WHERE ref_span = 0 AND ref0_k IS NOT NULL AND ref0_k >= 4096 AND k < 4096`,
			want:    "COVERING INDEX cells_ref0",
			notWant: []string{"SCAN cells", "TEMP B-TREE"},
		},
		{
			name:    "structural: first content at or past the mutation point",
			query:   `SELECT k FROM cells WHERE k >= 4096 ORDER BY k LIMIT 1`,
			want:    "SEARCH cells USING PRIMARY KEY",
			notWant: []string{"SCAN cells", "TEMP B-TREE"},
		},
		{
			name: "forward dep walk",
			query: `SELECT ` + slotCols + `, ref_span FROM cells
			          WHERE k = 5 AND col = 20`,
			want:    "SEARCH cells USING PRIMARY KEY",
			notWant: []string{"SCAN cells"},
		},
		{
			// The USED extent. There is no index on "holds anything", so the
			// planner reads this as a scan — but a BACKWARD PRIMARY KEY scan,
			// which stops at the first row it finds and therefore pays only for
			// the trailing blank-but-stored cells. What must never appear is a
			// TEMP B-TREE: written as `SELECT MAX(k) ... WHERE ...` it sorts,
			// and then finding the end of a sheet costs a sheet.
			name: "used extent",
			query: `SELECT k FROM cells
			          WHERE raw <> '' OR computed <> '' OR kind <> 0 OR style <> 0
			          ORDER BY k DESC LIMIT 1`,
			want:    "SCAN cells",
			notWant: []string{"TEMP B-TREE", "USING INDEX"},
		},
		// Styling. The whole placement argument for putting the style id in a
		// COLUMN rather than a side table is that the hot read stays ONE range
		// scan; if this ever grows a second table, the design has silently
		// become the one that measured 45-81% slower.
		{
			name: "windowed read carries the style id",
			query: `SELECT k, col, raw, computed, kind, ` + slotCols + `, style FROM cells
			          WHERE k BETWEEN 100 AND 349 ORDER BY k, col`,
			want:    "SEARCH cells USING PRIMARY KEY",
			notWant: []string{"SCAN cells", "TEMP B-TREE", "LEFT-JOIN"},
		},
		{
			// SetStyle's read of the current styles over the selected range.
			name: "style read over a range",
			query: `SELECT k, col, style FROM cells
			          WHERE k BETWEEN 100 AND 349 AND style <> 0`,
			want:    "SEARCH cells USING PRIMARY KEY",
			notWant: []string{"SCAN cells", "TEMP B-TREE"},
		},
		{
			// ClearStyle's sweep of rows it left holding nothing.
			name: "blank-cell sweep after a style clear",
			query: `DELETE FROM cells
			          WHERE k BETWEEN 100 AND 349
			            AND raw = '' AND computed = '' AND kind = 0 AND style = 0`,
			want:    "SEARCH cells USING PRIMARY KEY",
			notWant: []string{"SCAN cells", "TEMP B-TREE"},
		},
		{
			// The style garbage collector. This is the ONE query in the styling
			// path that is not bounded by the selection, so it has to be bounded
			// by the partial index instead: MEASURED at 0.31 ms over 11,000
			// styled cells with cells_style and 12.00 ms without it, where the
			// planner falls back to SCAN cells + USE TEMP B-TREE FOR DISTINCT.
			name:    "style garbage collection",
			query:   `SELECT DISTINCT style FROM cells WHERE style <> 0`,
			want:    "COVERING INDEX cells_style",
			notWant: []string{"TEMP B-TREE"},
		},
		// The CASCADE. The whole argument for a column style being one record
		// is that the read stays what it was, so what is pinned here is that
		// the row level costs ONE MORE RANGE SCAN over a small table and never
		// a join, a sort or a probe per cell. If any of these becomes a scan,
		// styling a column has quietly become more expensive per push than the
		// 10,000 cell writes it replaced.
		{
			// The row level over a window — the one extra query the cascade
			// adds to the hot read path, and only on a sheet that has a row
			// style at all.
			name:    "row level over a window",
			query:   `SELECT k, style FROM rows WHERE k BETWEEN 100 AND 349 AND style <> 0`,
			want:    "SEARCH rows USING PRIMARY KEY",
			notWant: []string{"SCAN rows", "TEMP B-TREE", "LEFT-JOIN"},
		},
		{
			// GetCell's single-row resolution. A seek, not the range scan the
			// window uses, because a scan for one answer is a scan.
			name:    "row level for one row",
			query:   `SELECT style FROM rows WHERE k = 100`,
			want:    "SEARCH rows USING PRIMARY KEY",
			notWant: []string{"SCAN rows", "TEMP B-TREE"},
		},
		{
			// The resident predicate that decides whether the window read runs
			// the query above at all. It has to cost less than the query it
			// avoids or it is not worth having.
			name:    "does any row carry a style",
			query:   `SELECT 1 FROM rows WHERE style <> 0 LIMIT 1`,
			want:    "COVERING INDEX rows_style",
			notWant: []string{"TEMP B-TREE"},
		},
		{
			// The insert's tail probe, row-level arm: is there a styled row in
			// the last n rows, which decides whether the sheet grows.
			name:    "structural: row styles in the reusable tail",
			query:   `SELECT k FROM rows WHERE k >= 4096 AND style <> 0 LIMIT 1`,
			want:    "SEARCH rows USING PRIMARY KEY",
			notWant: []string{"SCAN rows", "TEMP B-TREE"},
		},
		{
			// The used extent, row-level arm. Same shape as the cells arm and
			// for the same reason: a BACKWARD primary-key scan stops at the
			// first hit, where MAX() over a predicate sorts.
			name:    "used extent, row level",
			query:   `SELECT k FROM rows WHERE style <> 0 ORDER BY k DESC LIMIT 1`,
			want:    "SCAN rows",
			notWant: []string{"TEMP B-TREE", "USING INDEX"},
		},
		{
			// The garbage collector's row arm. `rows` also holds heights, so it
			// is not already restricted to styled rows and needs the partial
			// index for the same reason cells_style is needed.
			name:    "style garbage collection, row level",
			query:   `SELECT DISTINCT style FROM rows WHERE style <> 0`,
			want:    "COVERING INDEX rows_style",
			notWant: []string{"TEMP B-TREE"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := plan(tc.query, tc.args...)
			if !strings.Contains(got, tc.want) {
				t.Errorf("query plan missing %q:\n%s", tc.want, got)
			}
			for _, bad := range tc.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("query plan contains %q:\n%s", bad, got)
				}
			}
		})
	}
}

// ─── The write transaction ────────────────────────────────────────────────────

func TestWriteCellWritesCellDepsAndEvent(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	sh := mustOpen(t, c, "tx")

	a1 := CellRef{0, 0}
	b1 := CellRef{0, 1}

	if err := sh.WriteCell(a1, "10", nil); err != nil {
		t.Fatalf("write A1: %v", err)
	}
	// B1 = A1*2, with the recalc result for B1 supplied by the "engine".
	err := sh.WriteCell(b1, "=A1*2",
		[]ComputedCell{{Ref: b1, Computed: "20", Kind: KindFormula}})
	if err != nil {
		t.Fatalf("write B1: %v", err)
	}

	got, _ := sh.GetCell(b1)
	if got.Raw != "=A1*2" || got.Computed != "20" || got.Kind != KindFormula {
		t.Fatalf("B1 = %+v", got)
	}

	deps, err := sh.DependentsOf(a1)
	if err != nil {
		t.Fatalf("DependentsOf: %v", err)
	}
	if len(deps) != 1 || deps[0] != b1 {
		t.Fatalf("DependentsOf(A1) = %v, want [B1]", deps)
	}
	prec, err := sh.PrecedentsOf(b1)
	if err != nil {
		t.Fatalf("PrecedentsOf: %v", err)
	}
	if len(prec) != 1 || prec[0] != a1 {
		t.Fatalf("PrecedentsOf(B1) = %v, want [A1]", prec)
	}

	// Editing A1 now recomputes B1 through the computed slice; B1's raw must
	// survive untouched.
	err = sh.WriteCell(a1, "50",
		[]ComputedCell{{Ref: b1, Computed: "100", Kind: KindFormula}})
	if err != nil {
		t.Fatalf("re-write A1: %v", err)
	}
	got, _ = sh.GetCell(b1)
	if got.Raw != "=A1*2" || got.Computed != "100" {
		t.Fatalf("B1 after cascade = %+v, want raw preserved and computed 100", got)
	}

	evs, err := sh.Events(0, 100)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3", len(evs))
	}
	want := []struct{ ref, raw, prev string }{
		{"A1", "10", ""},
		{"B1", "=A1*2", ""},
		{"A1", "50", "10"},
	}
	for i, w := range want {
		e := evs[i]
		if e.Seq != int64(i+1) {
			t.Errorf("event %d seq = %d, want %d", i, e.Seq, i+1)
		}
		if e.Ref != w.ref || e.Raw != w.raw || e.Prev != w.prev {
			t.Errorf("event %d = %+v, want ref %s raw %s prev %s", i, e, w.ref, w.raw, w.prev)
		}
		if e.TS.IsZero() {
			t.Errorf("event %d has zero timestamp", i)
		}
	}

	// Rewriting B1 as a literal must clear its outgoing edges.
	if err := sh.WriteCell(b1, "7", nil); err != nil {
		t.Fatalf("clear B1: %v", err)
	}
	if deps, _ := sh.DependentsOf(a1); len(deps) != 0 {
		t.Fatalf("DependentsOf(A1) after B1 became a literal = %v, want none", deps)
	}
}

// TestWriteCellAtomicity is the load-bearing test for the SPEC commitment that
// a mutation and its event commit together. A write that fails partway must
// leave NO event and NO dependency change behind.
func TestWriteCellAtomicity(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	sh := mustOpen(t, c, "atomic")

	a1 := CellRef{0, 0}
	b1 := CellRef{0, 1}
	c1 := CellRef{0, 2}

	// Establish a known good state: B1 = A1*2.
	if err := sh.WriteCell(a1, "10", nil); err != nil {
		t.Fatalf("seed A1: %v", err)
	}
	if err := sh.WriteCell(b1, "=A1*2",
		[]ComputedCell{{Ref: b1, Computed: "20", Kind: KindFormula}}); err != nil {
		t.Fatalf("seed B1: %v", err)
	}
	seqBefore, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	depsBefore, _ := sh.DependentsOf(a1)

	// Now a write that dies mid-transaction: the computed slice carries a ref
	// outside the grid, which the cells CHECK constraint rejects. By then the
	// event row is already inserted and B1's edges are already replaced, so
	// only a rollback can put things back.
	badErr := sh.WriteCell(b1, "=A1*3",
		[]ComputedCell{
			{Ref: b1, Computed: "30", Kind: KindFormula},
			{Ref: CellRef{Row: -1, Col: 0}, Computed: "boom", Kind: KindNumber},
		})
	if badErr == nil {
		t.Fatalf("write with an out-of-grid computed ref should have failed")
	}

	seqAfter, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if seqAfter != seqBefore {
		t.Errorf("failed write appended an event: seq %d -> %d", seqBefore, seqAfter)
	}
	evs, _ := sh.Events(0, 100)
	for _, e := range evs {
		if e.Raw == "=A1*3" {
			t.Errorf("failed write left event %+v behind", e)
		}
	}

	// The cell must be untouched.
	got, _ := sh.GetCell(b1)
	if got.Raw != "=A1*2" || got.Computed != "20" {
		t.Errorf("B1 = %+v after failed write, want the pre-write state", got)
	}

	// The dependency edges must be untouched: the delete-then-insert inside
	// the transaction must have rolled back too.
	depsAfter, _ := sh.DependentsOf(a1)
	if len(depsAfter) != len(depsBefore) || len(depsAfter) != 1 || depsAfter[0] != b1 {
		t.Errorf("DependentsOf(A1) = %v after failed write, want %v", depsAfter, depsBefore)
	}
	if d, _ := sh.DependentsOf(c1); len(d) != 0 {
		t.Errorf("failed write leaked an edge B1->C1: %v", d)
	}
}

// ─── LRU ──────────────────────────────────────────────────────────────────────

func TestLRUEvictionAndReopen(t *testing.T) {
	c := newTestCache(t, 2, time.Hour)

	s1 := mustOpen(t, c, "s1")
	if err := s1.WriteCell(CellRef{0, 0}, "one", nil); err != nil {
		t.Fatalf("write s1: %v", err)
	}
	mustOpen(t, c, "s2")
	if c.Len() != 2 {
		t.Fatalf("cache len = %d, want 2", c.Len())
	}

	// Touch s1 so s2 becomes the least recently used, then overflow.
	mustOpen(t, c, "s1")
	mustOpen(t, c, "s3")

	if c.Len() != 2 {
		t.Fatalf("cache len after overflow = %d, want 2 (cap)", c.Len())
	}
	if c.IsOpen("s2") {
		t.Fatalf("s2 should have been evicted as least-recently-used")
	}
	if !c.IsOpen("s1") || !c.IsOpen("s3") {
		t.Fatalf("s1 and s3 should still be open (open: s1=%v s3=%v)", c.IsOpen("s1"), c.IsOpen("s3"))
	}

	// Evict s1 too, then prove the stale handle reports closure rather than
	// panicking or returning garbage.
	mustOpen(t, c, "s4")
	mustOpen(t, c, "s5")
	if c.IsOpen("s1") {
		t.Fatalf("s1 should be evicted by now")
	}
	if _, err := s1.GetCell(CellRef{0, 0}); err == nil {
		t.Fatalf("stale handle should return ErrSheetClosed")
	}

	// Reopening must return the persisted data.
	reopened := mustOpen(t, c, "s1")
	got, err := reopened.GetCell(CellRef{0, 0})
	if err != nil {
		t.Fatalf("GetCell after reopen: %v", err)
	}
	if got.Raw != "one" {
		t.Fatalf("after reopen A1 = %+v, want raw \"one\"", got)
	}
	evs, err := reopened.Events(0, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events after reopen = %v (err %v), want 1", evs, err)
	}
}

func TestIdleEviction(t *testing.T) {
	c := newTestCache(t, 64, 100*time.Millisecond)
	sh := mustOpen(t, c, "idle")
	if err := sh.WriteCell(CellRef{1, 1}, "x", nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for c.IsOpen("idle") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if c.IsOpen("idle") {
		t.Fatalf("sheet still open after idle TTL elapsed")
	}
	// And it comes back intact.
	got, err := mustOpen(t, c, "idle").GetCell(CellRef{1, 1})
	if err != nil {
		t.Fatalf("after idle eviction: %v", err)
	}
	if got.Raw != "x" {
		t.Fatalf("after idle eviction B2 = %+v", got)
	}
}

func TestBadSheetID(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	for _, id := range []string{"", "../escape", "a/b", "with space", "ünicode"} {
		if _, err := c.Open(id); err == nil {
			t.Errorf("Open(%q) should have been rejected", id)
		}
	}
	if _, err := c.Open("ok_id-123"); err != nil {
		t.Errorf("Open(ok_id-123): %v", err)
	}
}

// ─── Actors ───────────────────────────────────────────────────────────────────

// TestActorSerializesWritesToOneSheet does an unsynchronized read-modify-write
// from many goroutines. If the actor did not serialize per sheet, increments
// would be lost and the final value would be under N.
func TestActorSerializesWritesToOneSheet(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	actors := NewActors(c)
	defer actors.Close()

	if err := actors.Do("counter", func(sh *Sheet) error {
		return sh.WriteCell(CellRef{0, 0}, "0", nil)
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- actors.Do("counter", func(sh *Sheet) error {
				cur, err := sh.GetCell(CellRef{0, 0})
				if err != nil {
					return err
				}
				v, err := strconv.Atoi(cur.Raw)
				if err != nil {
					return fmt.Errorf("parse %q: %w", cur.Raw, err)
				}
				return sh.WriteCell(CellRef{0, 0}, strconv.Itoa(v+1), nil)
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}

	sh := mustOpen(t, c, "counter")
	got, err := sh.GetCell(CellRef{0, 0})
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if got.Raw != strconv.Itoa(n) {
		t.Fatalf("counter = %q after %d serialized increments, want %d (lost update: writes were not serialized)", got.Raw, n, n)
	}

	// The log must show one strictly increasing sequence — the total order.
	evs, err := sh.Events(0, n+10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != n+1 {
		t.Fatalf("got %d events, want %d", len(evs), n+1)
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d, want %d", i, e.Seq, i+1)
		}
		if i > 0 && e.Prev != evs[i-1].Raw {
			t.Fatalf("event %d prev = %q, want %q — writes interleaved", i, e.Prev, evs[i-1].Raw)
		}
	}
}

// TestActorsDoNotSerializeAcrossSheets holds one sheet's lane hostage and
// requires another sheet to make progress anyway.
func TestActorsDoNotSerializeAcrossSheets(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	actors := NewActors(c)
	defer actors.Close()

	entered := make(chan struct{})
	release := make(chan struct{})
	blocked := make(chan error, 1)
	go func() {
		blocked <- actors.Do("slow", func(*Sheet) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	done := make(chan error, 1)
	go func() {
		done <- actors.Do("fast", func(sh *Sheet) error {
			return sh.WriteCell(CellRef{0, 0}, "1", nil)
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write to second sheet: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("a write to a different sheet blocked behind another sheet's lane")
	}

	close(release)
	if err := <-blocked; err != nil {
		t.Fatalf("blocked command: %v", err)
	}
	if actors.Lanes() != 2 {
		t.Fatalf("lanes = %d, want 2", actors.Lanes())
	}
}

// TestActorsCloseDrains checks that commands already queued when Close is
// called still run, and that new commands are rejected afterwards.
func TestActorsCloseDrains(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	actors := NewActors(c)

	entered := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- actors.Do("drain", func(*Sheet) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	// Queue a handful behind the blocked one.
	const queued = 5
	rest := make(chan error, queued)
	var wg sync.WaitGroup
	for i := 0; i < queued; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rest <- actors.Do("drain", func(sh *Sheet) error {
				return sh.WriteCell(CellRef{i, 0}, strconv.Itoa(i), nil)
			})
		}(i)
	}
	// Give the senders a moment to actually enqueue.
	time.Sleep(100 * time.Millisecond)

	closed := make(chan struct{})
	go func() {
		close(release)
		actors.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("Close did not return")
	}
	wg.Wait()
	close(rest)
	if err := <-first; err != nil {
		t.Fatalf("first command: %v", err)
	}
	for err := range rest {
		if err != nil {
			t.Fatalf("queued command dropped by shutdown: %v", err)
		}
	}

	sh := mustOpen(t, c, "drain")
	evs, err := sh.Events(0, 100)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != queued {
		t.Fatalf("drained %d writes, want %d", len(evs), queued)
	}

	if err := actors.Do("drain", func(*Sheet) error { return nil }); err != ErrActorsClosed {
		t.Fatalf("Do after Close = %v, want ErrActorsClosed", err)
	}
	actors.Close() // idempotent
}

func TestActorRecoversFromPanic(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	actors := NewActors(c)
	defer actors.Close()

	if err := actors.Do("panicky", func(*Sheet) error { panic("boom") }); err == nil {
		t.Fatalf("panicking command should return an error")
	}
	// The lane must still be alive.
	if err := actors.Do("panicky", func(sh *Sheet) error {
		return sh.WriteCell(CellRef{0, 0}, "still here", nil)
	}); err != nil {
		t.Fatalf("lane died after a panic: %v", err)
	}
}

// ─── Seed ─────────────────────────────────────────────────────────────────────

func TestSeedLayout(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	sh := mustSeed(t, c, "layout", 300, MaxCols)

	// A literal column.
	got, _ := sh.GetCell(CellRef{7, 3})
	if got.Kind != KindNumber || got.Computed != fmtNum(seedLiteral(7, 3)) {
		t.Errorf("D8 = %+v, want literal %v", got, seedLiteral(7, 3))
	}
	// U = A*2.
	got, _ = sh.GetCell(CellRef{7, seedColU})
	if got.Raw != "=A8*2" || got.Computed != fmtNum(seedLiteral(7, 0)*2) {
		t.Errorf("U8 = %+v", got)
	}
	// V = U + B.
	got, _ = sh.GetCell(CellRef{7, seedColV})
	if got.Raw != "=U8+B8" {
		t.Errorf("V8 raw = %q", got.Raw)
	}
	// W on the first row of band 1 only.
	got, _ = sh.GetCell(CellRef{50, seedColW})
	if got.Raw != "=SUM(A51:A100)" {
		t.Errorf("W51 raw = %q, want =SUM(A51:A100)", got.Raw)
	}
	if got, _ := sh.GetCell(CellRef{51, seedColW}); got.Kind != KindEmpty {
		t.Errorf("W52 = %+v, want empty (W is first-row-of-band only)", got)
	}
	// X is the cross-band edge: X51 (band 1) reads W1 (band 0).
	got, _ = sh.GetCell(CellRef{50, seedColX})
	if got.Raw != "=W1+W51" {
		t.Errorf("X51 raw = %q, want =W1+W51", got.Raw)
	}
	if got, _ := sh.GetCell(CellRef{0, seedColX}); got.Kind != KindEmpty {
		t.Errorf("X1 = %+v, want empty (band 0 has no previous band)", got)
	}
	// The Y chain.
	for _, tc := range []struct {
		row int
		raw string
	}{
		{0, "=SUM(A1:A10)"},
		{50, "=Y1*2"},
		{100, "=Y51+A1"},
		{150, "=SUM(Y1:Y101)"},
		{200, "=Y151*2"},
	} {
		got, _ := sh.GetCell(CellRef{tc.row, seedColY})
		if got.Raw != tc.raw {
			t.Errorf("Y%d raw = %q, want %q", tc.row+1, got.Raw, tc.raw)
		}
		if got.Kind != KindFormula {
			t.Errorf("Y%d kind = %v, want formula", tc.row+1, got.Kind)
		}
	}
	// Z is deliberately blank.
	if got, _ := sh.GetCell(CellRef{0, 25}); got.Kind != KindEmpty {
		t.Errorf("Z1 = %+v, want empty", got)
	}
	// Seeding writes no events.
	if seq, err := sh.LastSeq(); err != nil || seq != 0 {
		t.Errorf("LastSeq after seed = %d (err %v), want 0", seq, err)
	}
}

// TestSeedCascadeSpansBands is the fixture guarantee recalc and the benchmark
// depend on: editing A1 must reach cells in more than one band.
func TestSeedCascadeSpansBands(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	sh := mustSeed(t, c, "cascade", 300, MaxCols)

	a1 := CellRef{0, 0}
	seen := map[CellRef]bool{a1: true}
	queue := []CellRef{a1}
	var dirty []CellRef
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		dirty = append(dirty, cur)
		deps, err := sh.DependentsOf(cur)
		if err != nil {
			t.Fatalf("DependentsOf(%s): %v", cur, err)
		}
		for _, d := range deps {
			if !seen[d] {
				seen[d] = true
				queue = append(queue, d)
			}
		}
	}

	bands := BandsFor(dirty)
	if len(bands) < 5 {
		t.Fatalf("editing A1 dirties bands %v (%d cells); want at least 5 bands", bands, len(dirty))
	}
	for _, want := range []int{0, 1, 2, 3, 4} {
		found := false
		for _, b := range bands {
			if b == want {
				found = true
			}
		}
		if !found {
			t.Errorf("band %d not in cascade from A1: %v", want, bands)
		}
	}
	// The specific chain cells must all be in the dirty set.
	for _, want := range []CellRef{
		{0, seedColU}, {0, seedColV}, {0, seedColW}, {50, seedColX},
		{0, seedColY}, {50, seedColY}, {100, seedColY}, {150, seedColY}, {200, seedColY},
	} {
		if !seen[want] {
			t.Errorf("%s missing from the cascade set", want)
		}
	}
	t.Logf("editing A1 dirties %d cells across bands %v", len(dirty), bands)
}

func TestSeedRejectsBadDimensions(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	for _, tc := range []struct{ rows, cols int }{{0, 26}, {-1, 26}, {RowCeiling + 1, 26}, {10, 0}, {10, 27}} {
		if err := c.Seed("dims", tc.rows, tc.cols); err == nil {
			t.Errorf("Seed(%d,%d) should have been rejected", tc.rows, tc.cols)
		}
	}
	// A narrow sheet must seed without panicking on the missing columns.
	if err := c.Seed("narrow", 60, 3); err != nil {
		t.Fatalf("narrow seed: %v", err)
	}
	sh := mustOpen(t, c, "narrow")
	cells, err := sh.Window(0, 0)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	for col := 3; col < MaxCols; col++ {
		if cells[col].Kind != KindEmpty {
			t.Errorf("col %d should be empty in a 3-column seed: %+v", col, cells[col])
		}
	}
}

// TestSeedFullGrid seeds the canonical 10,000 x 26 sheet and reports how long
// the seed and a windowed read take. Skipped under -short.
func TestSeedFullGrid(t *testing.T) {
	if testing.Short() {
		t.Skip("full-grid seed is slow; skipped under -short")
	}
	c := newTestCache(t, 4, time.Minute)

	start := time.Now()
	if err := c.Seed("full", DefaultRows, MaxCols); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedDur := time.Since(start)

	sh := mustOpen(t, c, "full")

	// The hot read path: a buffer of 5 bands (250 rows), as the renderer will
	// ask for it.
	var windowDur time.Duration
	const iters = 20
	for i := 0; i < iters; i++ {
		lo := (i * 137) % (DefaultRows - 250)
		s := time.Now()
		cells, err := sh.Window(lo, lo+249)
		windowDur += time.Since(s)
		if err != nil {
			t.Fatalf("window: %v", err)
		}
		if len(cells) != 250*MaxCols {
			t.Fatalf("window returned %d cells", len(cells))
		}
	}

	nCells := countRows(t, sh, "cells")
	nRefs := countRows(t, sh, `cells WHERE ref0_k IS NOT NULL`)
	nSpans := countRows(t, sh, `cells WHERE ref_span = 1`)
	size := fileSize(t, sh.Path())
	t.Logf("seed 10000x26: %v | %d cells, %d with references (%d of them ranges), %.1f MB on disk | Window(250 rows) avg %v",
		seedDur.Round(time.Millisecond), nCells, nRefs, nSpans, float64(size)/(1<<20),
		(windowDur / iters).Round(time.Microsecond))

	// A cascade from A1 on the full grid must still span 5 bands.
	deps, err := sh.DependentsOf(CellRef{0, 0})
	if err != nil {
		t.Fatalf("DependentsOf(A1): %v", err)
	}
	if len(deps) < 3 {
		t.Fatalf("A1 has %d direct dependents on the full grid, want >= 3", len(deps))
	}
}

// TestWindowRawMaterializationCost measures what the template representation
// costs the HOT READ PATH.
//
// Storing a formula as a template plus reference columns is only a good trade
// if turning it back into A1 text is cheap, because a window read runs on every
// push to every viewer. This is the number that decides that: a ~13,000-cell
// window against the isolated cost of materializing the ~1,000 formulas in it.
//
// It logs rather than asserts a threshold: a wall-clock bound would be flaky on
// a shared machine, and the interesting fact is the RATIO — materialization
// against the read it rides on.
func TestWindowRawMaterializationCost(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds a full sheet; skipped under -short")
	}
	c := newTestCache(t, 4, time.Minute)
	// Pinned to the 9,999 x 26 fixture every read number in DATA-MODEL.md was
	// taken on, and stated rather than inherited from DefaultRows — see
	// costFixtureRows.
	sh := mustSeed(t, c, "matcost", costFixtureRows-1, MaxCols)

	const loRow, hiRow = 1000, 1499 // 500 rows x 26 cols = 13,000 cells

	// The templates and slots of exactly the cells the window will materialize.
	type tmplRow struct {
		tmpl  string
		slots []CellRef
	}
	var tmpls []tmplRow
	if err := sh.use(func(db *sql.DB) error {
		rows, err := db.Query(
			`SELECT raw, `+slotCols+` FROM cells
			  WHERE k BETWEEN ? AND ? AND ref0_k IS NOT NULL
			  ORDER BY k, col`, sh.index().keyOf(loRow), sh.index().keyOf(hiRow))
		if err != nil {
			return err
		}
		defer rows.Close()
		var slots nullSlots
		for rows.Next() {
			var r tmplRow
			if err := rows.Scan(append([]any{&r.tmpl}, slots.scanArgs()...)...); err != nil {
				return err
			}
			r.slots = slots.refs(sh.index())
			tmpls = append(tmpls, r)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read templates: %v", err)
	}
	if len(tmpls) == 0 {
		t.Fatal("no templated formulas in the window; the measurement would be meaningless")
	}

	cells, err := sh.Window(loRow, hiRow) // warm the pool and the page cache
	if err != nil {
		t.Fatalf("Window: %v", err)
	}

	const n = 20
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, err := sh.Window(loRow, hiRow); err != nil {
			t.Fatalf("Window: %v", err)
		}
	}
	perWindow := time.Since(start) / n

	var sink int
	start = time.Now()
	for i := 0; i < n; i++ {
		for _, r := range tmpls {
			sink += len(RenderTemplate(r.tmpl, r.slots))
		}
	}
	perMaterialize := time.Since(start) / n
	if sink == 0 {
		t.Fatal("materialization produced nothing")
	}

	t.Logf("window %d rows = %d cells, %d of them templated formulas",
		hiRow-loRow+1, len(cells), len(tmpls))
	t.Logf("Window() total:            %v", perWindow.Round(time.Microsecond))
	t.Logf("A1 materialization alone:  %v (%.1f%% of the read, %v per formula)",
		perMaterialize.Round(time.Microsecond),
		100*float64(perMaterialize)/float64(perWindow),
		(perMaterialize / time.Duration(len(tmpls))).Round(time.Nanosecond))

	// And it has to be RIGHT, not merely fast: the window's Raw must be the A1
	// text the renderer expects, for both a plain reference and a range.
	at := func(a1 string) Cell {
		ref := mustRef(t, a1)
		return cells[(ref.Row-loRow)*MaxCols+ref.Col]
	}
	if got := at("U1001").Raw; got != "=A1001*2" {
		t.Errorf("U1001 raw from Window = %q, want %q", got, "=A1001*2")
	}
	if got := at("W1001").Raw; got != "=SUM(A1001:A1050)" {
		t.Errorf("W1001 raw from Window = %q, want %q", got, "=SUM(A1001:A1050)")
	}
	if got := at("A1001").Raw; got != fmtNum(seedLiteral(1000, 0)) {
		t.Errorf("a literal must not go near the template path: A1001 raw = %q", got)
	}
}

// v1SchemaDDL is the schema as it was BEFORE references became structural: a
// formula's references lived as A1 text inside cells.raw and nowhere else.
// Kept here, in the test, because that is the only place it is still needed —
// to build a file that a running server may already have on disk.
const v1SchemaDDL = `
CREATE TABLE cells (
  "row"    INTEGER NOT NULL CHECK ("row" >= 0 AND "row" < 10000),
  col      INTEGER NOT NULL CHECK (col >= 0 AND col < 26),
  raw      TEXT    NOT NULL DEFAULT '',
  computed TEXT    NOT NULL DEFAULT '',
  kind     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY ("row", col)
) WITHOUT ROWID;
CREATE TABLE deps (
  from_row INTEGER NOT NULL, from_col INTEGER NOT NULL,
  to_row   INTEGER NOT NULL, to_col   INTEGER NOT NULL,
  PRIMARY KEY (from_row, from_col, to_row, to_col)
) WITHOUT ROWID;
CREATE INDEX deps_reverse ON deps (to_row, to_col, from_row, from_col);
CREATE TABLE events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
  ref TEXT NOT NULL, raw TEXT NOT NULL, prev TEXT NOT NULL
);
CREATE TABLE cols (col INTEGER PRIMARY KEY, width INTEGER NOT NULL) WITHOUT ROWID;
`

// TestMigratesV1SheetOnOpen is the promise that an existing sheet file is
// CONVERTED and never misread.
//
// The hazard is specific and quiet: `=A1*2` is a perfectly well-formed v2
// template that happens to contain no holes. Read a v1 file as v2 and nothing
// looks wrong — the text renders, the values are right — but the formula has no
// structural references, so it never moves again and every later insert
// silently corrupts the sheet. So the version is stamped in PRAGMA
// user_version and an older file is converted on open.
func TestMigratesV1SheetOnOpen(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	path := filepath.Join(c.dir, "legacy.db")
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A v1 file, written the way v1 wrote them: references as text only.
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(v1SchemaDDL); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}
	for _, row := range [][]any{
		{4, 0, "10", "10", int(KindNumber)},           // A5
		{0, 1, "=A5*2", "20", int(KindFormula)},       // B1 = A5*2
		{0, 2, "= a5 * 2", "20", int(KindFormula)},    // C1, odd spacing and case
		{0, 3, "=SUM(A1:A5)", "10", int(KindFormula)}, // D1, a range
		{0, 4, "=SUM(#REF!)", "#REF!", int(KindError)},
	} {
		if _, err := old.Exec(
			`INSERT INTO cells ("row", col, raw, computed, kind) VALUES (?, ?, ?, ?, ?)`,
			row...); err != nil {
			t.Fatalf("seed v1 cell: %v", err)
		}
	}
	for _, e := range [][4]int{{0, 1, 4, 0}, {0, 2, 4, 0},
		{0, 3, 0, 0}, {0, 3, 1, 0}, {0, 3, 2, 0}, {0, 3, 3, 0}, {0, 3, 4, 0}} {
		if _, err := old.Exec(
			`INSERT INTO deps (from_row, from_col, to_row, to_col) VALUES (?, ?, ?, ?)`,
			e[0], e[1], e[2], e[3]); err != nil {
			t.Fatalf("seed v1 dep: %v", err)
		}
	}
	var v int
	if err := old.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 0 {
		t.Fatalf("v1 file user_version = %d (err %v), want 0", v, err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	sh := mustOpen(t, c, "legacy")

	// 1. The text survives the conversion byte for byte, spacing and case
	//    included. A migration that normalized formulas would be rewriting the
	//    user's documents to suit the storage.
	for a1, want := range map[string]string{
		"B1": "=A5*2", "C1": "= a5 * 2", "D1": "=SUM(A1:A5)", "E1": "=SUM(#REF!)",
	} {
		if got := mustCell(t, sh, a1).Raw; got != want {
			t.Errorf("after migration %s = %q, want %q", a1, got, want)
		}
	}

	// 2. The version is stamped, so a second open does no work and — more to
	//    the point — cannot re-template an already-templated formula.
	if err := sh.use(func(db *sql.DB) error {
		return db.QueryRow(`PRAGMA user_version`).Scan(&v)
	}); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version after migration = %d, want %d", v, schemaVersion)
	}

	// 3. THE POINT OF ALL THIS: the references are now structural, so they
	//    move. A file that had been misread as v2 would leave every one of
	//    these unchanged, which is exactly the silent corruption the version
	//    check exists to prevent.
	if _, err := sh.InsertRows(1, 1); err != nil {
		t.Fatalf("InsertRows on the migrated sheet: %v", err)
	}
	wantCell(t, sh, "B1", "=A6*2", "20")
	// C1's spacing survives but its `a5` comes back as `A6`: a reference that
	// MOVED is re-rendered canonically, because the text the user typed is no
	// longer true of it. Case is preserved only while the reference has not
	// moved — which is the contract, and is what every other line here checks.
	wantCell(t, sh, "C1", "= A6 * 2", "20")
	wantCell(t, sh, "D1", "=SUM(A1:A6)", "10") // the range expanded over the blank row
	wantCell(t, sh, "E1", "=SUM(#REF!)", TokenRef)
	wantCell(t, sh, "A6", "10", "10")
}

// ─── The edit path, measured ──────────────────────────────────────────────────

// BenchmarkEditPath is one committed edit end to end: Recalc (walk the graph,
// evaluate in topological order) plus WriteCell (the single transaction that
// persists the cell, the recomputed values and the event). That pair IS the
// hot write path — http.go's cell command does exactly these two calls.
//
// The three cases are chosen to separate what an edit costs from what its
// CASCADE costs, and to expose dependency write amplification:
//
//	literal, no dependents — the floor. T9500 is a seeded literal that nothing
//	                         reads, so the pass discovers one node and writes
//	                         one row.
//	A1 cascade             — the headline case: 11 cells across 5 bands, four
//	                         hops, discovered during the write.
//	range formula rewrite  — the cell being edited IS a range formula whose
//	                         extent changes, so its own edge set is replaced.
//	                         This is where a separate dependency table costs
//	                         the most per edit: ten or twenty edges deleted and
//	                         re-inserted for one keystroke.
//
// Each case alternates between two values so that every iteration is a REAL
// edit — recalc's changed-value-only rule would otherwise make every second
// iteration a no-op and halve the reported cost for the wrong reason.
func BenchmarkEditPath(b *testing.B) {
	cases := []struct {
		name string
		a1   string
		vals [2]string
	}{
		{"literal no dependents", "T9500", [2]string{"1", "2"}},
		{"A1 cascade", "A1", [2]string{"424242", "5"}},
		{"range formula rewrite", "Z1", [2]string{"=SUM(A1:A10)", "=SUM(A1:A20)"}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			c := NewSheetCache(b.TempDir(), 4, time.Minute)
			defer c.Close()
			if err := c.Seed("bench", DefaultRows, MaxCols); err != nil {
				b.Fatal(err)
			}
			sh, err := c.Open("bench")
			if err != nil {
				b.Fatal(err)
			}
			ref, err := ParseRef(tc.a1)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				raw := tc.vals[i%2]
				res, err := Recalc(sh, ref, raw)
				if err != nil {
					b.Fatal(err)
				}
				if err := sh.WriteCell(ref, raw, res.Computed); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// v2SchemaDDL is the schema as it was when dependency edges were a table of
// their own: references already structural on cells, but every edge ALSO
// materialized in `deps` and indexed in both directions. Kept here because
// files in this shape are on disk right now.
const v2SchemaDDL = `
CREATE TABLE cells (
  "row"    INTEGER NOT NULL CHECK ("row" >= 0 AND "row" < 10000),
  col      INTEGER NOT NULL CHECK (col >= 0 AND col < 26),
  raw      TEXT    NOT NULL DEFAULT '',
  computed TEXT    NOT NULL DEFAULT '',
  kind     INTEGER NOT NULL DEFAULT 0,
  ref0_row INTEGER, ref0_col INTEGER,
  ref1_row INTEGER, ref1_col INTEGER,
  ref_span INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY ("row", col)
) WITHOUT ROWID;
CREATE TABLE deps (
  from_row INTEGER NOT NULL, from_col INTEGER NOT NULL,
  to_row   INTEGER NOT NULL, to_col   INTEGER NOT NULL,
  PRIMARY KEY (from_row, from_col, to_row, to_col)
) WITHOUT ROWID;
CREATE INDEX deps_reverse ON deps (to_row, to_col, from_row, from_col);
CREATE TABLE events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
  ref TEXT NOT NULL, raw TEXT NOT NULL, prev TEXT NOT NULL
);
CREATE TABLE cols (col INTEGER PRIMARY KEY, width INTEGER NOT NULL) WITHOUT ROWID;
PRAGMA user_version = 2;
`

// TestMigratesV2SheetOnOpen is the other half of "never misread a file on
// disk". Version 2 files exist right now, and the difference between v2 and v3
// is a whole table.
//
// The hazard here is the mirror image of the v1 one. A v2 file opened as v3
// would look perfect — the cells are byte-identical between the versions, so
// every value, every formula and every reference reads correctly — while a
// `deps` table nobody maintains any more sat in the file growing stale, and the
// reverse lookup silently answered from the reference columns instead. Nothing
// would break; the file would just carry a few hundred kilobytes of lies. So
// the version is stamped and the table is dropped on open, once.
//
// What is NOT done is equally deliberate: the edges are not read, checked or
// rebuilt. They were derived from the reference columns that survive, so there
// is nothing in them to recover — which is exactly why the migration is O(1)
// and not O(edges).
func TestMigratesV2SheetOnOpen(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	path := filepath.Join(c.dir, "v2.db")
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(v2SchemaDDL); err != nil {
		t.Fatalf("apply v2 schema: %v", err)
	}
	// Cells exactly as v2 stored them: templates with holes, slots in columns.
	for _, row := range [][]any{
		{4, 0, "10", "10", int(KindNumber), nil, nil, nil, nil, 0},           // A5
		{0, 1, "={0|A5}*2", "20", int(KindFormula), 4, 0, nil, nil, 0},       // B1 = A5*2
		{0, 3, "=SUM({0|A1}:{1|A5})", "10", int(KindFormula), 0, 0, 4, 0, 1}, // D1 = SUM(A1:A5)
		{0, 4, "={0|A5}+{1|B1}", "30", int(KindFormula), 4, 0, 0, 1, 0},      // E1 = A5+B1
	} {
		// Row-keyed reference columns: this is what a v2 file holds.
		if _, err := old.Exec(
			`INSERT INTO cells ("row", col, raw, computed, kind,
			     ref0_row, ref0_col, ref1_row, ref1_col, ref_span)
			   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, row...); err != nil {
			t.Fatalf("seed v2 cell: %v", err)
		}
	}
	for _, e := range [][4]int{{0, 1, 4, 0}, {0, 4, 4, 0}, {0, 4, 0, 1},
		{0, 3, 0, 0}, {0, 3, 1, 0}, {0, 3, 2, 0}, {0, 3, 3, 0}, {0, 3, 4, 0}} {
		if _, err := old.Exec(
			`INSERT INTO deps (from_row, from_col, to_row, to_col) VALUES (?, ?, ?, ?)`,
			e[0], e[1], e[2], e[3]); err != nil {
			t.Fatalf("seed v2 dep: %v", err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	sh := mustOpen(t, c, "v2")

	// 1. The version is stamped and the table is GONE — not emptied, not left
	//    to rot.
	var v int
	if err := sh.use(func(db *sql.DB) error {
		return db.QueryRow(`PRAGMA user_version`).Scan(&v)
	}); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version after migration = %d, want %d", v, schemaVersion)
	}
	var leftovers []string
	if err := sh.use(func(db *sql.DB) error {
		rows, err := db.Query(
			`SELECT name FROM sqlite_master WHERE name IN ('deps', 'deps_reverse')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			leftovers = append(leftovers, n)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Errorf("v2 objects survived the migration: %v", leftovers)
	}

	// 2. Every edge the dropped table held is still answerable, from the
	//    columns it was derived from — the single refs and the range alike.
	got := refStrings(mustDependents(t, sh, "A5"))
	sort.Strings(got)
	if strings.Join(got, ",") != "B1,D1,E1" {
		t.Errorf("DependentsOf(A5) = %v, want [B1 D1 E1]", got)
	}
	if got := refStrings(mustDependents(t, sh, "A3")); len(got) != 1 || got[0] != "D1" {
		t.Errorf("DependentsOf(A3) = %v, want [D1] (inside the range)", got)
	}
	if got := refStrings(mustDependents(t, sh, "B1")); len(got) != 1 || got[0] != "E1" {
		t.Errorf("DependentsOf(B1) = %v, want [E1]", got)
	}

	// 3. And the sheet still moves: the references were never text, so a
	//    mutation shifts them and the range expands over the blank row.
	if _, err := sh.InsertRows(1, 1); err != nil {
		t.Fatalf("InsertRows on the migrated sheet: %v", err)
	}
	wantCell(t, sh, "B1", "=A6*2", "20")
	wantCell(t, sh, "D1", "=SUM(A1:A6)", "10")
	wantCell(t, sh, "A6", "10", "10")
}

func mustDependents(t *testing.T, sh *Sheet, a1 string) []CellRef {
	t.Helper()
	d, err := sh.DependentsOf(mustRef(t, a1))
	if err != nil {
		t.Fatalf("DependentsOf(%s): %v", a1, err)
	}
	return d
}

// v3SchemaDDL is the schema as it was when a cell was addressed by its DISPLAY
// ROW: references already structural and the edge table already gone, but
// `cells` still clustered on (row, col). Every file written before storage keys
// looks like this, including the ones on disk right now.
const v3SchemaDDL = `
CREATE TABLE cells (
  "row"    INTEGER NOT NULL CHECK ("row" >= 0 AND "row" < 10000),
  col      INTEGER NOT NULL CHECK (col >= 0 AND col < 26),
  raw      TEXT    NOT NULL DEFAULT '',
  computed TEXT    NOT NULL DEFAULT '',
  kind     INTEGER NOT NULL DEFAULT 0,
  ref0_row INTEGER, ref0_col INTEGER,
  ref1_row INTEGER, ref1_col INTEGER,
  ref_span INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY ("row", col)
) WITHOUT ROWID;
CREATE INDEX cells_ref0 ON cells (ref0_row, ref0_col)
  WHERE ref_span = 0 AND ref0_row IS NOT NULL;
CREATE INDEX cells_span_lo ON cells (ref0_row, ref1_row, ref0_col, ref1_col)
  WHERE ref_span = 1;
CREATE TABLE events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
  ref TEXT NOT NULL, raw TEXT NOT NULL, prev TEXT NOT NULL
);
CREATE TABLE cols (col INTEGER PRIMARY KEY, width INTEGER NOT NULL) WITHOUT ROWID;
PRAGMA user_version = 3;
`

// TestMigratesV3SheetOnOpen is the promise for the change that moved the
// primary key. A v3 file addresses a cell by its display row; a v4 file
// addresses it by a storage key, and 4,096 of those keys stand between one
// display row and the next.
//
// THE HAZARD IS TOTAL AND SILENT. Read a v3 file as v4 and every cell is filed
// at a key that means a different row: row 50 reads as row 1 of band 0 rather
// than row 0 of band 1, references land on cells nobody named, and nothing
// errors because both numbers are perfectly legal keys. So the version is
// stamped and the table is REBUILT on open, once, inside the transaction that
// stamps it.
func TestMigratesV3SheetOnOpen(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	path := filepath.Join(c.dir, "v3.db")
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(v3SchemaDDL); err != nil {
		t.Fatalf("apply v3 schema: %v", err)
	}
	// Rows chosen to cross band boundaries in the new layout, plus the two
	// reference shapes that are not a plain row number: an ABSENT slot (NULL)
	// and a DESTROYED one (-1, which renders #REF!).
	for _, row := range [][]any{
		{0, 0, "10", "10", int(KindNumber), nil, nil, nil, nil, 0},       // A1
		{49, 0, "20", "20", int(KindNumber), nil, nil, nil, nil, 0},      // A50, last row of band 0
		{50, 0, "30", "30", int(KindNumber), nil, nil, nil, nil, 0},      // A51, first of band 1
		{8999, 0, "40", "40", int(KindNumber), nil, nil, nil, nil, 0},    // A9000
		{1, 1, "={0|A51}*2", "60", int(KindFormula), 50, 0, nil, nil, 0}, // B2 reads across a band
		{2, 1, "=SUM({0|A1}:{1|A9000})", "100", int(KindFormula), 0, 0, 8999, 0, 1},
		{3, 1, "={0|#REF!}*2", "#REF!", int(KindError), -1, -1, nil, nil, 0}, // a dead reference
		{4, 1, "={0|A1}+{1|A51}", "40", int(KindFormula), 0, 0, 50, 0, 0},
	} {
		if _, err := old.Exec(
			`INSERT INTO cells ("row", col, raw, computed, kind,
			     ref0_row, ref0_col, ref1_row, ref1_col, ref_span)
			   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, row...); err != nil {
			t.Fatalf("seed v3 cell: %v", err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	sh := mustOpen(t, c, "v3")

	var v int
	if err := sh.use(func(db *sql.DB) error {
		return db.QueryRow(`PRAGMA user_version`).Scan(&v)
	}); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version after migration = %d, want %d", v, schemaVersion)
	}

	// 1. Every cell is where it was, and every reference still names what it
	//    named — including the one that names nothing.
	for a1, want := range map[string][2]string{
		"A1":    {"10", "10"},
		"A50":   {"20", "20"},
		"A51":   {"30", "30"},
		"A9000": {"40", "40"},
		"B2":    {"=A51*2", "60"},
		"B3":    {"=SUM(A1:A9000)", "100"},
		"B4":    {"=#REF!*2", TokenRef},
		"B5":    {"=A1+A51", "40"},
	} {
		wantCell(t, sh, a1, want[0], want[1])
	}

	// 2. The band index exists and is canonical, so keys and rows agree from the
	//    first write.
	bi := sh.index()
	if err := bi.validate(); err != nil {
		t.Fatalf("migrated band index: %v", err)
	}
	if got := bi.keyOf(50); got != keyStride {
		t.Errorf("row 51 has key %d after migration, want %d", got, keyStride)
	}

	// 3. THE POINT: the sheet moves, and the reference that crosses a band
	//    boundary follows the row it names rather than the key it sat at.
	if _, err := sh.InsertRows(0, 1); err != nil {
		t.Fatalf("InsertRows on the migrated sheet: %v", err)
	}
	wantCell(t, sh, "A1", "", "")
	wantCell(t, sh, "A2", "10", "10")
	wantCell(t, sh, "A52", "30", "30")
	wantCell(t, sh, "B3", "=A52*2", "60")
	wantCell(t, sh, "B4", "=SUM(A2:A9001)", "100")
	wantCell(t, sh, "B5", "=#REF!*2", TokenRef)
	wantCell(t, sh, "B6", "=A2+A52", "40")
	checkStoreInvariants(t, sh)
}

// TestMigratesRealV3File converts an actual file from disk and checks it cell
// for cell against what the v3 table said before the conversion.
//
//	SS_V3_DIR=/tmp/ss-run-copy go test -run TestMigratesRealV3File -v
//
// Env-gated because it needs a file nobody else is writing to — copy the
// server's data directory first, never point it at a live one.
func TestMigratesRealV3File(t *testing.T) {
	dir := os.Getenv("SS_V3_DIR")
	if dir == "" {
		t.Skip("set SS_V3_DIR to a COPY of a sheet directory to check migration")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := NewSheetCache(dir, 4, time.Minute)
	defer c.Close()
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".db")
		t.Run(id, func(t *testing.T) {
			// Read the file exactly as it is on disk, before anything opens it
			// through the store.
			raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var version int
			if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			type before struct{ raw, computed string }
			was := map[CellRef]before{}
			rows, err := raw.Query(`SELECT "row", col, raw, computed, ` + slotCols[:0] +
				`ref0_row, ref0_col, ref1_row, ref1_col FROM cells`)
			if err != nil {
				t.Skipf("%s is not a v3 file (%v)", id, err)
			}
			for rows.Next() {
				var ref CellRef
				var r, cmp string
				var slots nullSlots
				if err := rows.Scan(append([]any{&ref.Row, &ref.Col, &r, &cmp}, slots.scanArgs()...)...); err != nil {
					t.Fatal(err)
				}
				// Materialize the A1 text the v3 way: slot rows are display rows.
				refs := make([]CellRef, 0, maxSlots)
				for i := 0; i < maxSlots; i++ {
					if !slots[2*i].Valid {
						break
					}
					refs = append(refs, CellRef{Row: int(slots[2*i].Int64), Col: int(slots[2*i+1].Int64)})
				}
				if len(refs) > 0 {
					r = RenderTemplate(r, refs)
				}
				was[ref] = before{r, cmp}
			}
			rows.Close()
			raw.Close()

			start := time.Now()
			sh, err := c.Open(id)
			if err != nil {
				t.Fatalf("open (and migrate) %s: %v", id, err)
			}
			t.Logf("%s: v%d -> v%d, %d cells, open+migrate %v",
				id, version, schemaVersion, len(was), time.Since(start).Round(time.Millisecond))

			bad := 0
			for ref, want := range was {
				got, err := sh.GetCell(ref)
				if err != nil {
					t.Fatalf("get %s: %v", ref, err)
				}
				if got.Raw != want.raw || got.Computed != want.computed {
					if bad++; bad < 6 {
						t.Errorf("%s after migration = {%q, %q}, was {%q, %q}",
							ref, got.Raw, got.Computed, want.raw, want.computed)
					}
				}
			}
			if bad > 0 {
				t.Errorf("%d of %d cells changed meaning across the migration", bad, len(was))
			}
			checkStoreInvariants(t, sh)

			// A second open must do nothing: the version is stamped, so the
			// conversion cannot run twice.
			if _, err := c.Open(id); err != nil {
				t.Fatalf("reopen %s: %v", id, err)
			}
		})
	}
}

// ─── Growth ───────────────────────────────────────────────────────────────────

// A write below the bottom of the sheet extends it. This is the second of the
// two ways a sheet gets taller (the other is an insert) and it is what makes
// "type into C15000" work on a sheet that has never been 15,000 rows tall.
func TestWriteBelowTheExtentGrowsTheSheet(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "grow")
	mustSet(t, sh, "A1", "top")

	if got := sh.Rows(); got != DefaultRows {
		t.Fatalf("new sheet is %d rows, want %d", got, DefaultRows)
	}
	mustSet(t, sh, "C15000", "deep")
	if got := sh.Rows(); got != 15000 {
		t.Fatalf("extent after writing C15000 = %d, want 15000", got)
	}

	// It reads back through both read paths...
	wantCell(t, sh, "C15000", "deep", "deep")
	cells, err := sh.Window(14999, 14999)
	if err != nil {
		t.Fatalf("window at the new bottom: %v", err)
	}
	if len(cells) != MaxCols {
		t.Fatalf("window returned %d cells, want %d", len(cells), MaxCols)
	}
	if cells[2].Raw != "deep" || cells[2].Ref != (CellRef{14999, 2}) {
		t.Errorf("window cell = %+v, want C15000 = \"deep\"", cells[2])
	}
	// ...a window that runs past the bottom comes back short rather than
	// erroring, which is the same contract it always had at 10,000.
	short, err := sh.Window(14990, 15100)
	if err != nil {
		t.Fatalf("window past the bottom: %v", err)
	}
	if got, want := len(short), 10*MaxCols; got != want {
		t.Errorf("window past the bottom returned %d cells, want %d", got, want)
	}

	// ...and it SURVIVES A REOPEN, which is the part that says the extent is
	// persisted rather than living in the handle. The band index is the only
	// place it lives and the bands table is where that lands.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2 := NewSheetCache(c.dir, 4, time.Minute)
	defer c2.Close()
	sh2, err := c2.Open("grow")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := sh2.Rows(); got != 15000 {
		t.Fatalf("extent after reopen = %d, want 15000", got)
	}
	wantCell(t, sh2, "C15000", "deep", "deep")
	wantCell(t, sh2, "A1", "top", "top")

	// A ref past the CEILING is still refused, and refused as a bad reference
	// rather than by growing to a million rows.
	if _, err := ParseRef("A1000001"); !errors.Is(err, ErrBadRef) {
		t.Errorf("ParseRef past the ceiling = %v, want ErrBadRef", err)
	}
}

// A formula naming a row past the bottom of the sheet is a well-formed
// reference that resolves to nothing — it must not grow the sheet by being
// mentioned, and it must not be an error either.
func TestReferenceBelowTheExtentReadsEmpty(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "farref")
	mustSet(t, sh, "A1", "=A50000*2")
	if got := sh.Rows(); got != DefaultRows {
		t.Errorf("naming row 50000 grew the sheet to %d rows", got)
	}
	got, err := sh.GetCell(CellRef{49999, 0})
	if err != nil {
		t.Fatalf("GetCell past the extent: %v", err)
	}
	if got.Kind != KindEmpty || got.Raw != "" {
		t.Errorf("cell past the extent = %+v, want empty", got)
	}
}

// ─── v4 -> v5: the migration that changes no bytes ────────────────────────────
//
// v4 already stored the row extent (the sum of bands.nrows); it merely required
// that sum to be 10,000. v5 accepts 1..RowCeiling. So a v4 file is a valid v5
// file saying "10,000 rows", which is what it meant — there is no
// reinterpretation to get wrong, and this test is what makes that claim
// checkable rather than asserted.
func TestMigratesV4SheetOnOpen(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "v4", 300, MaxCols)
	before := dumpSheet(t, sh)

	// Put the file back to exactly what a v4 build would have left: the same
	// bytes, stamped 4.
	if err := sh.use(func(db *sql.DB) error {
		_, err := db.Exec(`PRAGMA user_version = 4`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := NewSheetCache(c.dir, 4, time.Minute)
	defer c2.Close()
	sh2, err := c2.Open("v4")
	if err != nil {
		t.Fatalf("open (and migrate) a v4 file: %v", err)
	}
	var v int
	if err := sh2.use(func(db *sql.DB) error {
		return db.QueryRow(`PRAGMA user_version`).Scan(&v)
	}); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version after migration = %d, want %d", v, schemaVersion)
	}
	if got := sh2.Rows(); got != DefaultRows {
		t.Errorf("a migrated v4 file reports %d rows, want %d", got, DefaultRows)
	}
	if got := dumpSheet(t, sh2); got != before {
		t.Error("a v4 file changed when it was migrated to v5")
	}
	// And it can now do the thing it could not do before.
	mustSet(t, sh2, fmt.Sprintf("A%d", DefaultRows), "last")
	if _, err := sh2.InsertRows(0, 1); err != nil {
		t.Fatalf("insert on a migrated v4 sheet whose last row has content: %v", err)
	}
	if got := sh2.Rows(); got != DefaultRows+1 {
		t.Errorf("migrated sheet is %d rows after growing, want %d", got, DefaultRows+1)
	}
}

// A band index that does not describe a usable sheet must stop the file from
// opening. The row extent is now variable, so "the bands are wrong" is no
// longer catchable by comparing against a constant and this is the check that
// replaces it.
func TestRefusesToOpenASheetWithAnUnusableBandIndex(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "bad", 100, MaxCols)
	if err := sh.use(func(db *sql.DB) error {
		// Past the ceiling: a corrupt or hostile file, not a big sheet.
		_, err := db.Exec(`UPDATE bands SET nrows = ? WHERE idx = 0`, RowCeiling+1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2 := NewSheetCache(c.dir, 4, time.Minute)
	defer c2.Close()
	if _, err := c2.Open("bad"); err == nil {
		t.Fatal("a sheet with an out-of-range extent opened anyway")
	}
}

// TestMigratesRealV4File runs the v4 -> v5 open against real files written by a
// shipped binary. Set SS_V4_DIR to a COPY of a data directory.
//
// It reads every cell out of the file BEFORE the store touches it, by the same
// key arithmetic v4 used, and compares cell for cell afterwards — the same
// standard TestMigratesRealV3File holds the v3 conversion to, because "the
// bytes did not change" is exactly the kind of claim that is worth checking
// rather than believing.
func TestMigratesRealV4File(t *testing.T) {
	dir := os.Getenv("SS_V4_DIR")
	if dir == "" {
		t.Skip("set SS_V4_DIR to a COPY of a sheet directory to check v4 migration")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := NewSheetCache(dir, 4, time.Minute)
	defer c.Close()
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".db")
		t.Run(id, func(t *testing.T) {
			raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var version int
			if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != 4 {
				raw.Close()
				t.Skipf("%s is user_version %d, not a v4 file", id, version)
			}
			bi, err := loadBandIndex(raw)
			if err != nil || bi == nil {
				raw.Close()
				t.Fatalf("read bands: %v", err)
			}
			wasRows := bi.rows
			type before struct{ raw, computed string }
			was := map[CellRef]before{}
			rows, err := raw.Query(`SELECT k, col, raw, computed, ` + slotCols + ` FROM cells`)
			if err != nil {
				raw.Close()
				t.Fatal(err)
			}
			for rows.Next() {
				var k int64
				var col int
				var r, cmp string
				var slots nullSlots
				if err := rows.Scan(append([]any{&k, &col, &r, &cmp}, slots.scanArgs()...)...); err != nil {
					t.Fatal(err)
				}
				if refs := slots.refs(bi); refs != nil {
					r = RenderTemplate(r, refs)
				}
				was[CellRef{Row: bi.rankOf(k), Col: col}] = before{r, cmp}
			}
			rows.Close()
			raw.Close()

			start := time.Now()
			sh, err := c.Open(id)
			if err != nil {
				t.Fatalf("open (and migrate) %s: %v", id, err)
			}
			took := time.Since(start)

			var v int
			if err := sh.use(func(db *sql.DB) error {
				return db.QueryRow(`PRAGMA user_version`).Scan(&v)
			}); err != nil {
				t.Fatal(err)
			}
			if v != schemaVersion {
				t.Errorf("user_version = %d, want %d", v, schemaVersion)
			}
			if got := sh.Rows(); got != wasRows {
				t.Errorf("extent = %d after migration, want the %d it had", got, wasRows)
			}
			bad := 0
			for ref, w := range was {
				got, err := sh.GetCell(ref)
				if err != nil {
					t.Fatal(err)
				}
				if got.Raw != w.raw || got.Computed != w.computed {
					if bad++; bad < 5 {
						t.Errorf("%s = {%q,%q}, was {%q,%q}", ref, got.Raw, got.Computed, w.raw, w.computed)
					}
				}
			}
			if bad > 0 {
				t.Errorf("%d of %d cells differ after migration", bad, len(was))
			}
			t.Logf("v4 -> v5 %s: %d cells, %d rows, %v, all identical",
				id, len(was), wasRows, took.Round(time.Millisecond))
		})
	}
}

// ─── The two extents ──────────────────────────────────────────────────────────

// A sheet has an ALLOCATED extent (how tall the grid is, what sizes the scroll
// container) and a USED extent (the last row holding data, what a jump-to-end
// gesture means). They are different numbers and neither is derived from the
// other.
func TestAllocatedAndUsedExtentsAreDifferentNumbers(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "extents")

	used := func() int {
		t.Helper()
		n, err := sh.UsedRows()
		if err != nil {
			t.Fatalf("UsedRows: %v", err)
		}
		return n
	}

	// A blank sheet: full height, no data, nothing stored.
	if got := sh.Rows(); got != DefaultRows {
		t.Errorf("blank sheet allocates %d rows, want %d", got, DefaultRows)
	}
	if got := used(); got != 0 {
		t.Errorf("blank sheet uses %d rows, want 0", got)
	}
	var stored int
	if err := sh.use(func(db *sql.DB) error {
		return db.QueryRow(`SELECT COUNT(*) FROM cells`).Scan(&stored)
	}); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Errorf("blank sheet stores %d cells, want 0", stored)
	}

	mustSet(t, sh, "A1", "1")
	mustSet(t, sh, "B300", "2")
	if got := used(); got != 300 {
		t.Errorf("used extent = %d, want 300", got)
	}
	if got := sh.Rows(); got != DefaultRows {
		t.Errorf("data at row 300 changed the allocated extent to %d", got)
	}

	// A write past the bottom moves BOTH.
	mustSet(t, sh, "B20000", "3")
	if got, want := sh.Rows(), 20000; got != want {
		t.Errorf("allocated = %d, want %d", got, want)
	}
	if got, want := used(), 20000; got != want {
		t.Errorf("used = %d, want %d", got, want)
	}

	// CLEARING that cell must move the used extent back. This is the Excel
	// defect stated as a test: a used range that only grows means the end of
	// the sheet is wherever it once was, forever.
	mustSet(t, sh, "B20000", "")
	if got := used(); got != 300 {
		t.Errorf("used extent after clearing the far cell = %d, want 300", got)
	}
	// The allocated extent does NOT come back on its own — clearing a cell is
	// not a request to resize the grid, and a scroll container that shrank
	// under the user because they emptied a cell would be worse than one that
	// stayed. Deleting rows is what takes it back down.
	if got, want := sh.Rows(), 20000; got != want {
		t.Errorf("allocated = %d after a clear, want %d", got, want)
	}
}

// The allocated extent must be able to come DOWN, or it is Excel's bug: a sheet
// grown to 20,000 rows and then emptied would describe 20,000 rows of nothing
// forever.
func TestDeletingRowsTakesTheAllocatedExtentBackDown(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "shrink")
	mustSet(t, sh, "A20000", "deep")
	if got := sh.Rows(); got != 20000 {
		t.Fatalf("allocated = %d, want 20000", got)
	}

	d, err := sh.DeleteRows(0, 5000)
	if err != nil {
		t.Fatalf("DeleteRows: %v", err)
	}
	if got, want := sh.Rows(), 15000; got != want {
		t.Fatalf("allocated after deleting 5,000 rows = %d, want %d", got, want)
	}
	if d.Stats.Grew != -5000 {
		t.Errorf("Stats.Grew = %d, want -5000", d.Stats.Grew)
	}
	wantCell(t, sh, "A15000", "deep", "deep")

	// It stops at the DefaultRows floor: a sheet must keep somewhere to scroll
	// and start typing however much is deleted out of it. Written as "delete
	// everything above the floor" rather than as a literal, because the floor
	// is what is under test and the arithmetic that reaches it is not.
	if _, err := sh.DeleteRows(0, sh.Rows()-DefaultRows); err != nil {
		t.Fatalf("DeleteRows to the floor: %v", err)
	}
	if got := sh.Rows(); got != DefaultRows {
		t.Fatalf("allocated = %d after deleting down past the floor, want %d", got, DefaultRows)
	}
	// And a delete AT the floor gives every row straight back, so the extent
	// does not move at all. That is the same symmetry an insert has on a
	// default-sized sheet, and it is what keeps the scrollbar still.
	if _, err := sh.DeleteRows(0, DefaultRows/2); err != nil {
		t.Fatalf("DeleteRows at the floor: %v", err)
	}
	if got := sh.Rows(); got != DefaultRows {
		t.Errorf("allocated = %d, want the %d floor to hold", got, DefaultRows)
	}
	if err := sh.index().validate(); err != nil {
		t.Errorf("band index after shrinking: %v", err)
	}
}

// Insert and delete must not drift the allocated extent when they are working
// inside the height the sheet already has — otherwise a user tapping
// insert/delete would watch their scrollbar walk away.
func TestInsertDeleteCyclesDoNotDriftTheExtent(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "drift")
	mustSet(t, sh, "A1", "x")

	for i := 0; i < 50; i++ {
		if _, err := sh.InsertRows(10, 1); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		if _, err := sh.DeleteRows(10, 1); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
		if got := sh.Rows(); got != DefaultRows {
			t.Fatalf("after %d insert/delete cycles the extent is %d, want %d",
				i+1, got, DefaultRows)
		}
	}
	wantCell(t, sh, "A1", "x", "x")

	// The same above the floor, where every insert grows and every delete
	// shrinks: they still have to cancel exactly.
	mustSet(t, sh, "A30000", "deep")
	for i := 0; i < 50; i++ {
		if _, err := sh.InsertRows(10, 1); err != nil {
			t.Fatalf("grown insert %d: %v", i, err)
		}
		if _, err := sh.DeleteRows(10, 1); err != nil {
			t.Fatalf("grown delete %d: %v", i, err)
		}
		if got := sh.Rows(); got != 30000 {
			t.Fatalf("after %d cycles above the floor the extent is %d, want 30000", i+1, got)
		}
	}
	wantCell(t, sh, "A30000", "deep", "deep")
}

// ─── The event-log trim, measured before it is believed ───────────────────────

// padEvents fills the log to n rows in ONE transaction, so a test that needs a
// log at the cap does not pay a hundred thousand commits to get one. It writes
// through raw SQL on purpose: the point is to reach a log size, not to exercise
// the write path on the way there.
func padEvents(t *testing.T, sh *Sheet, n int) {
	t.Helper()
	err := sh.use(func(db *sql.DB) error {
		return inTx(db, func(tx *sql.Tx) error {
			st, err := tx.Prepare(
				`INSERT INTO events (ts, ref, raw, prev) VALUES (?, 'A1', 'pad', '')`)
			if err != nil {
				return err
			}
			defer st.Close()
			ts := time.Now().UnixMilli()
			for i := 0; i < n; i++ {
				if _, err := st.Exec(ts); err != nil {
					return err
				}
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("pad events: %v", err)
	}
}

func eventCount(t *testing.T, sh *Sheet) int {
	t.Helper()
	return countRows(t, sh, "events")
}

// ─── The cell-length cap ─────────────────────────────────────────────────────

// TestOversizeCellIsRefused pins the three things the cap has to be: a refusal,
// a refusal the HTTP layer already turns into a 400, and a refusal that leaves
// nothing behind.
func TestOversizeCellIsRefused(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "toolong")
	ref := CellRef{Row: 3, Col: 2}
	mustSet(t, sh, "C4", "keep me")
	seqBefore, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}

	huge := strings.Repeat("x", MaxCellBytes+1)
	err = sh.WriteCell(ref, huge, nil)
	if err == nil {
		t.Fatalf("a %d-byte cell was accepted", len(huge))
	}
	if !errors.Is(err, ErrCellTooLong) {
		t.Errorf("error %v does not wrap ErrCellTooLong", err)
	}
	// THE CLASSIFICATION IS THE POINT. handleCell, the fill/paste handlers and
	// the structural handlers all map an ErrBadRef-wrapped error to 400 and
	// everything else to 500; a refusal the user cannot read is barely better
	// than no refusal.
	if !errors.Is(err, ErrBadRef) {
		t.Errorf("error %v does not classify as ErrBadRef, so the HTTP layer would answer 500", err)
	}
	for _, want := range []string{"C4", "4096"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q — it is shown to the user verbatim", err, want)
		}
	}

	// Nothing written: not the cell, not an event.
	wantCell(t, sh, "C4", "keep me", "keep me")
	seqAfter, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if seqAfter != seqBefore {
		t.Errorf("refused write appended an event: seq %d -> %d", seqBefore, seqAfter)
	}

	// The boundary, both sides, because an off-by-one here is a cell that
	// cannot be re-saved with the text it already holds.
	atCap := strings.Repeat("y", MaxCellBytes)
	if err := sh.WriteCell(ref, atCap, nil); err != nil {
		t.Fatalf("a cell of exactly MaxCellBytes was refused: %v", err)
	}
	got, err := sh.GetCell(ref)
	if err != nil {
		t.Fatalf("GetCell: %v", err)
	}
	if len(got.Raw) != MaxCellBytes {
		t.Errorf("stored %d bytes, want %d", len(got.Raw), MaxCellBytes)
	}
}

// TestOversizeCellIsRefusedOnEveryPathIntoTheStore is the reason the cap lives
// in WriteCell and not in the handler: fill, paste and the clear loop
// (rangeops.go) call WriteCell directly, so a handler-side check would police
// one of four doors.
func TestOversizeCellIsRefusedOnEveryPathIntoTheStore(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustOpen(t, c, "toolongpaths")
	huge := strings.Repeat("x", MaxCellBytes+1)

	// WriteCellCtx is the entry point applyWrites (fill, paste) and the clear
	// loop use; it must refuse on exactly the same terms as WriteCell, which it
	// does by being the same function underneath.
	err := sh.WriteCellCtx(context.Background(), CellRef{Row: 0, Col: 0}, huge, nil)
	if !errors.Is(err, ErrCellTooLong) || !errors.Is(err, ErrBadRef) {
		t.Errorf("WriteCellCtx accepted an oversize cell: %v", err)
	}
	if n := countRows(t, sh, "cells"); n != 0 {
		t.Errorf("refused write left %d cells behind", n)
	}
	if n := countRows(t, sh, "events"); n != 0 {
		t.Errorf("refused write left %d events behind", n)
	}

	// And a cascade cannot smuggle one in either: the cap is checked before
	// anything is written, so an oversize RAW with a legal computed set is
	// still nothing at all.
	ref := CellRef{Row: 1, Col: 1}
	if err := sh.WriteCell(ref, huge,
		[]ComputedCell{{Ref: ref, Computed: "1", Kind: KindNumber}}); !errors.Is(err, ErrCellTooLong) {
		t.Errorf("oversize write with a computed set = %v, want ErrCellTooLong", err)
	}
	if n := countRows(t, sh, "cells"); n != 0 {
		t.Errorf("refused cascade write left %d cells behind", n)
	}
}

// TestEventTrimCost is the instrument for the one thing the trim could get
// wrong: it runs on the hot single-cell path, on every write, forever.
//
// Two arms alternating in ONE process, per the rule this project has arrived at
// twice: a sheet whose log is AT the cap (so the delete matches a row and does
// real work) against a sheet whose log is far below it (so the delete matches
// nothing and is a bounded index probe). Same seed, same cell, same process,
// same load — the only difference is the state of the log.
func TestEventTrimCost(t *testing.T) {
	if testing.Short() {
		t.Skip("cost measurement")
	}
	c := newTestCache(t, 4, time.Minute)
	full := mustSeed(t, c, "trimfull", 300, MaxCols)
	small := mustSeed(t, c, "trimsmall", 300, MaxCols)
	padEvents(t, full, maxEvents+5000)

	ref := CellRef{Row: 250, Col: 25}
	const n = 200
	fullD := make([]time.Duration, 0, n)
	smallD := make([]time.Duration, 0, n)
	edit := func(sh *Sheet, i int) time.Duration {
		v := strconv.Itoa(i)
		start := time.Now()
		if err := sh.WriteCell(ref, v,
			[]ComputedCell{{Ref: ref, Computed: v, Kind: KindNumber}}); err != nil {
			t.Fatalf("edit: %v", err)
		}
		return time.Since(start)
	}
	for i := 0; i < n; i++ {
		fullD = append(fullD, edit(full, i))
		smallD = append(smallD, edit(small, i))
	}
	t.Logf("single-cell edit, median of %d: log at the cap %d µs (%d events) | "+
		"log far below it %d µs (%d events)",
		n, median(fullD).Microseconds(), eventCount(t, full),
		median(smallD).Microseconds(), eventCount(t, small))
	t.Logf("  fastest sample: at the cap %d µs | below it %d µs",
		fastest(fullD).Microseconds(), fastest(smallD).Microseconds())

	// And what the gate costs when it does fire: one write in eventTrimEvery
	// removes rows instead of none. The FIRST one is the outlier — it deletes
	// everything the log is over by — and every one after it deletes exactly
	// eventTrimEvery rows, which is the number that recurs forever.
	trimAt := func() time.Duration {
		seq, err := full.LastSeq()
		if err != nil {
			t.Fatalf("LastSeq: %v", err)
		}
		for i := 0; int64(i) <= eventTrimEvery; i++ {
			d := edit(full, i)
			if (seq+int64(i)+1)%eventTrimEvery == 0 {
				return d
			}
		}
		t.Fatalf("no write in %d hit the trim gate", eventTrimEvery)
		return 0
	}
	first := trimAt()
	t.Logf("  the write that first pulls the log back to the cap: %d µs (log now %d)",
		first.Microseconds(), eventCount(t, full))
	steady := trimAt()
	t.Logf("  the one write in %d that trims in steady state: %d µs (log %d events)",
		eventTrimEvery, steady.Microseconds(), eventCount(t, full))
	t.Logf("  amortized over %d writes that is %d ns each",
		eventTrimEvery, steady.Nanoseconds()/eventTrimEvery)
}

// TestEventLogStaysBounded is the property the trim exists for, plus the two
// readers that a trimmed head could have broken.
func TestEventLogStaysBounded(t *testing.T) {
	c := newTestCache(t, 4, time.Minute)
	sh := mustSeed(t, c, "bounded", 100, MaxCols)

	// Start just under the cap so the writes below have to cross it. Padding
	// rather than writing a hundred thousand cells is the difference between a
	// test that runs in a second and one that runs in five.
	padEvents(t, sh, maxEvents-5)
	ref := CellRef{Row: 50, Col: 3}
	for i := 0; i < 3*eventTrimEvery; i++ {
		v := strconv.Itoa(i)
		if err := sh.WriteCell(ref, v,
			[]ComputedCell{{Ref: ref, Computed: v, Kind: KindNumber}}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if n := eventCount(t, sh); n > maxEvents+eventTrimEvery {
			t.Fatalf("after %d writes the log holds %d events, past the %d bound",
				i+1, n, maxEvents+eventTrimEvery)
		}
	}
	n := eventCount(t, sh)
	if n <= maxEvents-5 {
		t.Fatalf("the log SHRANK to %d — the trim is taking more than the head", n)
	}
	t.Logf("log settled at %d events after %d writes past the cap", n, 3*eventTrimEvery)

	// LastSeq is unaffected by a trimmed head: it reads MAX(seq), and the trim
	// never touches the tail.
	last, err := sh.LastSeq()
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if want := int64(maxEvents - 5 + 3*eventTrimEvery); last != want {
		t.Errorf("LastSeq = %d, want %d — a trim must not renumber anything", last, want)
	}

	// Events(0, …) now starts PAST 1, which is the visible consequence of a
	// bounded log and is the behaviour a reader gets: the oldest surviving
	// event, ascending, contiguous from there.
	head, err := sh.Events(0, 5)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(head) != 5 {
		t.Fatalf("Events(0,5) returned %d rows", len(head))
	}
	if head[0].Seq <= 1 {
		t.Errorf("Events(0,5) starts at seq %d — nothing was trimmed", head[0].Seq)
	}
	if head[0].Seq != last-int64(n)+1 {
		t.Errorf("the log starts at seq %d but holds %d rows ending at %d — there is a hole in it",
			head[0].Seq, n, last)
	}
	for i := 1; i < len(head); i++ {
		if head[i].Seq != head[i-1].Seq+1 {
			t.Errorf("gap in the surviving log: %d then %d", head[i-1].Seq, head[i].Seq)
		}
	}

	// The tail — which is the only part anything live reads — is untouched.
	tail, err := sh.Events(last-3, 10)
	if err != nil {
		t.Fatalf("Events tail: %v", err)
	}
	if len(tail) != 3 || tail[len(tail)-1].Seq != last {
		t.Fatalf("Events(last-3, 10) = %d rows ending at %d, want 3 ending at %d",
			len(tail), tail[len(tail)-1].Seq, last)
	}
	if tail[len(tail)-1].Ref != "D51" {
		t.Errorf("last event is for %s, want D51", tail[len(tail)-1].Ref)
	}

	// A seq a trimmed reader still holds asks for rows that no longer exist and
	// gets what survives, not an error — "the log is bounded, so history CAN be
	// lost" has to degrade to fewer rows rather than to a failure.
	old, err := sh.Events(1, 10)
	if err != nil {
		t.Fatalf("Events from a trimmed seq: %v", err)
	}
	if len(old) != 10 || old[0].Seq != head[0].Seq {
		t.Errorf("Events(1,10) = %d rows starting at %d, want 10 starting at %d",
			len(old), old[0].Seq, head[0].Seq)
	}
}

// TestEveryMutationTrimsTheLog is the reason the trim sits in appendEvent
// rather than in writeCellTx: a bound that held for cell edits alone is a bound
// a script walks past by holding down insert-row, or by dragging a column edge.
func TestEveryMutationTrimsTheLog(t *testing.T) {
	// An HOUR of idle, not the usual minute. This test pads the log to
	// maxEvents and then runs eventTrimEvery+10 mutations against ONE handle it
	// holds for the whole subtest, and the cache's idle sweep counts time since
	// the last Open rather than time since the last USE (see evictIdle) — the
	// production path re-Opens per actor turn, a test that holds a handle does
	// not. Under -race on a loaded machine the row subtest takes ~75 s, walks
	// past a one-minute window, and fails with "sheet handle closed (evicted)"
	// on a property it is not testing.
	c := newTestCache(t, 4, time.Hour)
	rowOp := func(sh *Sheet, i int) error {
		if i%2 == 0 {
			_, err := sh.InsertRows(10, 1)
			return err
		}
		_, err := sh.DeleteRows(10, 1)
		return err
	}
	width := func(sh *Sheet, i int) error {
		return sh.SetColWidth(4, 100+i%50)
	}
	style := func(sh *Sheet, i int) error {
		_, err := sh.SetColStyle([]int{2}, StylePatch{Bold: Set(i%2 == 0)})
		return err
	}
	for _, tc := range []struct {
		name string
		op   func(*Sheet, int) error
	}{
		{"insert/delete rows", rowOp},
		{"column width", width},
		{"column style", style},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh := mustSeed(t, c, "trim-"+strings.NewReplacer("/", "", " ", "-").Replace(tc.name), 60, MaxCols)
			padEvents(t, sh, maxEvents-5)
			for i := 0; i < eventTrimEvery+10; i++ {
				if err := tc.op(sh, i); err != nil {
					t.Fatalf("%s %d: %v", tc.name, i, err)
				}
			}
			if n := eventCount(t, sh); n > maxEvents+eventTrimEvery {
				t.Errorf("%s left the log at %d events, past the %d bound",
					tc.name, n, maxEvents+eventTrimEvery)
			} else {
				t.Logf("log at %d events after %d %s", n, eventTrimEvery+10, tc.name)
			}
		})
	}
}
