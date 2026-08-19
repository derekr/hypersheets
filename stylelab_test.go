package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
)

// ─── Where does the style id live? ────────────────────────────────────────────
//
// Two designs, and the band-key work changed the arithmetic that used to decide
// between them, so this measures rather than reasons:
//
//  1. a `style` INTEGER column ON cells. Free on read — the window query already
//     returns the row — and it costs WIDTH in the clustered b-tree that a
//     structural shift rewrites.
//  2. a sparse side table keyed (k, col). No shift cost for unstyled cells, but
//     the window read — the hot path, run on every push to every viewer — needs
//     a join.
//
// The cost that used to favour the side table was measured on a shift that
// moved 220,382 cells. A shift now moves ~1,100, so the same width cost is
// ~200x smaller, while the read cost of a join is unchanged and is paid on
// every push rather than on the rare mutation.
//
// Both arms run in ONE process against byte-identical data, alternating, which
// is the only read-path instrument this project has found trustworthy (see
// DATA-MODEL.md's warning about contended measurements).

// stylePopulate gives one cell in every `every` a style id, in both
// representations, so the two arms describe the same sheet.
func stylePopulate(t *testing.T, db *sql.DB, every int) (styled int) {
	t.Helper()
	if _, err := db.Exec(fmt.Sprintf(
		`UPDATE cells SET style = 1 + (k %% 8) WHERE (k * 26 + col) %% %d = 0`, every)); err != nil {
		t.Fatalf("populate style column: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM cells WHERE style <> 0`).Scan(&styled); err != nil {
		t.Fatalf("count styled: %v", err)
	}
	// The side-table design: only styled cells get a row, which is the whole
	// point of it.
	if _, err := db.Exec(`
		CREATE TABLE style_side (
		  k     INTEGER NOT NULL,
		  col   INTEGER NOT NULL,
		  style INTEGER NOT NULL,
		  PRIMARY KEY (k, col)
		) WITHOUT ROWID`); err != nil {
		t.Fatalf("create side table: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO style_side SELECT k, col, style FROM cells WHERE style <> 0`); err != nil {
		t.Fatalf("fill side table: %v", err)
	}
	return styled
}

// styleWindowColumn is the shipped read: one range scan, style included.
func styleWindowColumn(db *sql.DB, bi *bandIndex, lo, hi int) (time.Duration, int, error) {
	start := time.Now()
	rows, err := db.Query(
		`SELECT k, col, raw, computed, kind, `+slotCols+`, style FROM cells
		  WHERE k BETWEEN ? AND ?
		  ORDER BY k, col`, bi.keyOf(lo), bi.keyOf(hi))
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var (
		k             int64
		col, kind     int
		style         int
		raw, computed string
		slots         nullSlots
	)
	args := make([]any, 0, 6+2*maxSlots)
	args = append(args, &k, &col, &raw, &computed, &kind)
	args = append(args, slots.scanArgs()...)
	args = append(args, &style)
	n := 0
	for rows.Next() {
		if err := rows.Scan(args...); err != nil {
			return 0, 0, err
		}
		if refs := slots.refs(bi); refs != nil {
			raw = RenderTemplate(raw, refs)
		}
		_, _ = raw, style
		n++
	}
	return time.Since(start), n, rows.Err()
}

// styleWindowJoin is the same read against the side-table design.
func styleWindowJoin(db *sql.DB, bi *bandIndex, lo, hi int) (time.Duration, int, error) {
	start := time.Now()
	rows, err := db.Query(
		`SELECT c.k, c.col, c.raw, c.computed, c.kind,
		        c.ref0_k, c.ref0_col, c.ref1_k, c.ref1_col,
		        COALESCE(s.style, 0)
		   FROM cells c
		   LEFT JOIN style_side s ON s.k = c.k AND s.col = c.col
		  WHERE c.k BETWEEN ? AND ?
		  ORDER BY c.k, c.col`, bi.keyOf(lo), bi.keyOf(hi))
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var (
		k             int64
		col, kind     int
		style         int
		raw, computed string
		slots         nullSlots
	)
	args := make([]any, 0, 6+2*maxSlots)
	args = append(args, &k, &col, &raw, &computed, &kind)
	args = append(args, slots.scanArgs()...)
	args = append(args, &style)
	n := 0
	for rows.Next() {
		if err := rows.Scan(args...); err != nil {
			return 0, 0, err
		}
		if refs := slots.refs(bi); refs != nil {
			raw = RenderTemplate(raw, refs)
		}
		_, _ = raw, style
		n++
	}
	return time.Since(start), n, rows.Err()
}

func explain(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	out := ""
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		out += "\n      " + detail
	}
	return out
}

// TestStyleLabPlacement is the measurement that chose the column.
//
// Run: SHIFTLAB=1 go test -run TestStyleLabPlacement -v
func TestStyleLabPlacement(t *testing.T) {
	shiftLabEnabled(t)

	for _, every := range []int{20, 1} {
		name := fmt.Sprintf("1 cell in %d styled", every)
		t.Run(name, func(t *testing.T) {
			golden := goldenSheet(t, 9999)
			path := filepath.Join(t.TempDir(), "sty.db")
			copyDB(t, golden, path)
			db := labOpen(t, path, "")
			defer db.Close()
			bi, err := loadBandIndex(db)
			if err != nil || bi == nil {
				t.Fatalf("load band index: %v", err)
			}
			styled := stylePopulate(t, db, every)

			var bestCol, bestJoin time.Duration
			var nCol, nJoin int
			// Alternating, best-of-N. Best rather than mean because the thing
			// being compared is the query, and a scheduler hiccup only ever
			// makes a run slower.
			for i := 0; i < 12; i++ {
				lo := 5000
				c, cn, err := styleWindowColumn(db, bi, lo, lo+249)
				if err != nil {
					t.Fatalf("column window: %v", err)
				}
				j, jn, err := styleWindowJoin(db, bi, lo, lo+249)
				if err != nil {
					t.Fatalf("join window: %v", err)
				}
				if i == 0 || c < bestCol {
					bestCol, nCol = c, cn
				}
				if i == 0 || j < bestJoin {
					bestJoin, nJoin = j, jn
				}
			}
			if nCol != nJoin {
				t.Fatalf("the two arms read different sheets: %d vs %d cells", nCol, nJoin)
			}
			ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
			t.Logf("%d of 220,382 cells styled; the window returns %d", styled, nCol)
			t.Logf("window 250 rows / %d cells", nCol)
			t.Logf("  style COLUMN on cells : %.2f ms", ms(bestCol))
			t.Logf("  sparse SIDE TABLE join: %.2f ms  (%+.0f%%)",
				ms(bestJoin), 100*(ms(bestJoin)-ms(bestCol))/ms(bestCol))
			t.Logf("  column plan:%s", explain(t,
				db, `SELECT k, col, style FROM cells WHERE k BETWEEN 1 AND 2 ORDER BY k, col`))
			t.Logf("  join plan:%s", explain(t, db,
				`SELECT c.k, c.col, COALESCE(s.style,0) FROM cells c
				   LEFT JOIN style_side s ON s.k = c.k AND s.col = c.col
				  WHERE c.k BETWEEN 1 AND 2 ORDER BY c.k, c.col`))
		})
	}
}

// TestStyleLabGCCost measures the one operation that is not bounded by the
// number of styles: the sweep that collects unreferenced style rows. It is what
// justifies the cells_style partial index and the styleGCLimit threshold.
func TestStyleLabGCCost(t *testing.T) {
	shiftLabEnabled(t)

	golden := goldenSheet(t, 9999)
	path := filepath.Join(t.TempDir(), "gc.db")
	copyDB(t, golden, path)
	db := labOpen(t, path, "")
	defer db.Close()

	styled := stylePopulate(t, db, 20)
	if _, err := db.Exec(indexDDL); err != nil {
		t.Fatalf("indexes: %v", err)
	}
	t.Logf("distinct-style probe plan:%s", explain(t, db,
		`SELECT DISTINCT style FROM cells WHERE style <> 0`))

	var best time.Duration
	for i := 0; i < 5; i++ {
		start := time.Now()
		rows, err := db.Query(`SELECT DISTINCT style FROM cells WHERE style <> 0`)
		if err != nil {
			t.Fatalf("gc probe: %v", err)
		}
		n := 0
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			n++
		}
		rows.Close()
		d := time.Since(start)
		if i == 0 || d < best {
			best = d
		}
	}
	t.Logf("style GC sweep over %d styled cells (220,382 total): %.2f ms",
		styled, float64(best.Microseconds())/1000)

	// And what it costs WITHOUT the partial index, which is the number that
	// says whether the index earns its place.
	if _, err := db.Exec(`DROP INDEX cells_style`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	t.Logf("without the index, plan:%s", explain(t, db,
		`SELECT DISTINCT style FROM cells WHERE style <> 0`))
	var bare time.Duration
	for i := 0; i < 5; i++ {
		start := time.Now()
		rows, err := db.Query(`SELECT DISTINCT style FROM cells WHERE style <> 0`)
		if err != nil {
			t.Fatalf("gc probe: %v", err)
		}
		for rows.Next() {
			var id int
			_ = rows.Scan(&id)
		}
		rows.Close()
		if d := time.Since(start); i == 0 || d < bare {
			bare = d
		}
	}
	t.Logf("style GC sweep WITHOUT cells_style: %.2f ms", float64(bare.Microseconds())/1000)
}

// TestStyleLabColumnCost is the OTHER half of the placement decision: what the
// extra column costs the two paths it is on, measured as a one-process A/B
// against a byte-identical copy of the same cells WITHOUT it.
//
// This is the instrument the band-key work settled on, and it exists because
// wall-clock numbers taken minutes apart on this machine move by 25% with the
// load. Both arms run alternating, in one process, over the same data.
func TestStyleLabColumnCost(t *testing.T) {
	shiftLabEnabled(t)

	golden := goldenSheet(t, 9999)
	path := filepath.Join(t.TempDir(), "cost.db")
	copyDB(t, golden, path)
	db := labOpen(t, path, "")
	defer db.Close()
	bi, err := loadBandIndex(db)
	if err != nil || bi == nil {
		t.Fatalf("load band index: %v", err)
	}

	// The same cells, one column narrower: the v5 shape.
	if _, err := db.Exec(`
		CREATE TABLE cells_narrow (
		  k        INTEGER NOT NULL CHECK (k >= 0),
		  col      INTEGER NOT NULL CHECK (col >= 0 AND col < 26),
		  raw      TEXT    NOT NULL DEFAULT '',
		  computed TEXT    NOT NULL DEFAULT '',
		  kind     INTEGER NOT NULL DEFAULT 0,
		  ref0_k   INTEGER, ref0_col INTEGER,
		  ref1_k   INTEGER, ref1_col INTEGER,
		  ref_span INTEGER NOT NULL DEFAULT 0,
		  PRIMARY KEY (k, col)
		) WITHOUT ROWID;
		INSERT INTO cells_narrow
		  SELECT k, col, raw, computed, kind, ref0_k, ref0_col, ref1_k, ref1_col, ref_span
		    FROM cells;
		CREATE INDEX cells_narrow_ref0 ON cells_narrow (ref0_k, ref0_col)
		  WHERE ref_span = 0 AND ref0_k IS NOT NULL;
		CREATE INDEX cells_narrow_ref1 ON cells_narrow (ref1_k, ref1_col)
		  WHERE ref_span = 0 AND ref1_k IS NOT NULL;
		CREATE INDEX cells_narrow_span_lo ON cells_narrow (ref0_k, ref1_k, ref0_col, ref1_col)
		  WHERE ref_span = 1;
		CREATE INDEX cells_narrow_span_hi ON cells_narrow (ref1_k, ref0_k, ref0_col, ref1_col)
		  WHERE ref_span = 1;`); err != nil {
		t.Fatalf("build narrow copy: %v", err)
	}
	if _, err := db.Exec(indexDDL); err != nil {
		t.Fatalf("indexes: %v", err)
	}

	// ── The SHIFT. One band's worth of keys moved down by one and back, which
	//    is exactly what InsertRows does to `cells`, run against each shape.
	shift := func(table, cols string, lo, hi int64, delta int64) (time.Duration, error) {
		start := time.Now()
		tx, err := db.Begin()
		if err != nil {
			return 0, err
		}
		defer tx.Rollback() //nolint:errcheck
		if _, err := tx.Exec(fmt.Sprintf(
			`CREATE TEMP TABLE sh AS SELECT k + %d AS nk, %s FROM %s WHERE k BETWEEN %d AND %d`,
			delta, cols, table, lo, hi)); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(fmt.Sprintf(
			`DELETE FROM %s WHERE k BETWEEN %d AND %d`, table, lo, hi)); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(fmt.Sprintf(
			`INSERT INTO %s (k, %s) SELECT nk, %s FROM temp.sh`, table, cols, cols)); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DROP TABLE temp.sh`); err != nil {
			return 0, err
		}
		d := time.Since(start)
		return d, tx.Rollback()
	}
	narrowCols := `col, raw, computed, kind, ref0_k, ref0_col, ref1_k, ref1_col, ref_span`
	wideCols := narrowCols + `, style`

	var bestWide, bestNarrow time.Duration
	for i := 0; i < 10; i++ {
		w, err := shift("cells", wideCols, 0, 49, 1)
		if err != nil {
			t.Fatalf("wide shift: %v", err)
		}
		n, err := shift("cells_narrow", narrowCols, 0, 49, 1)
		if err != nil {
			t.Fatalf("narrow shift: %v", err)
		}
		if i == 0 || w < bestWide {
			bestWide = w
		}
		if i == 0 || n < bestNarrow {
			bestNarrow = n
		}
	}

	// ── The WINDOW READ, same alternation.
	narrowWindow := func(lo, hi int) (time.Duration, int, error) {
		start := time.Now()
		rows, err := db.Query(
			`SELECT k, col, raw, computed, kind, ref0_k, ref0_col, ref1_k, ref1_col
			   FROM cells_narrow WHERE k BETWEEN ? AND ? ORDER BY k, col`,
			bi.keyOf(lo), bi.keyOf(hi))
		if err != nil {
			return 0, 0, err
		}
		defer rows.Close()
		var (
			k             int64
			col, kind     int
			raw, computed string
			slots         nullSlots
		)
		args := make([]any, 0, 5+2*maxSlots)
		args = append(args, &k, &col, &raw, &computed, &kind)
		args = append(args, slots.scanArgs()...)
		n := 0
		for rows.Next() {
			if err := rows.Scan(args...); err != nil {
				return 0, 0, err
			}
			if refs := slots.refs(bi); refs != nil {
				raw = RenderTemplate(raw, refs)
			}
			_ = raw
			n++
		}
		return time.Since(start), n, rows.Err()
	}
	// The v5 read path EXACTLY as it shipped, per-row argument slice included.
	// This is the arm that answers "did the hot path regress", because it is
	// what the render layer was actually paying before styling existed.
	v5Window := func(lo, hi int) (time.Duration, int, error) {
		start := time.Now()
		rows, err := db.Query(
			`SELECT k, col, raw, computed, kind, ref0_k, ref0_col, ref1_k, ref1_col
			   FROM cells_narrow WHERE k BETWEEN ? AND ? ORDER BY k, col`,
			bi.keyOf(lo), bi.keyOf(hi))
		if err != nil {
			return 0, 0, err
		}
		defer rows.Close()
		var slots nullSlots
		n := 0
		for rows.Next() {
			var k int64
			var col, kind int
			var raw, computed string
			args := append([]any{&k, &col, &raw, &computed, &kind}, slots.scanArgs()...)
			if err := rows.Scan(args...); err != nil {
				return 0, 0, err
			}
			if refs := slots.refs(bi); refs != nil {
				raw = RenderTemplate(raw, refs)
			}
			_ = raw
			n++
		}
		return time.Since(start), n, rows.Err()
	}

	var readWide, readNarrow, readV5 time.Duration
	var cells int
	for i := 0; i < 12; i++ {
		w, wn, err := styleWindowColumn(db, bi, 5000, 5249)
		if err != nil {
			t.Fatalf("wide window: %v", err)
		}
		n, _, err := narrowWindow(5000, 5249)
		if err != nil {
			t.Fatalf("narrow window: %v", err)
		}
		if i == 0 || w < readWide {
			readWide, cells = w, wn
		}
		if i == 0 || n < readNarrow {
			readNarrow = n
		}
		v, _, err := v5Window(5000, 5249)
		if err != nil {
			t.Fatalf("v5 window: %v", err)
		}
		if i == 0 || v < readV5 {
			readV5 = v
		}
	}

	// ── The SINGLE-CELL EDIT, the third hot path. The write upsert does not
	//    NAME the style column (which is what makes a style survive an edit),
	//    so the only thing the column can cost here is table width.
	edit := func(table string) time.Duration {
		const n = 200
		samples := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			start := time.Now()
			if _, err := db.Exec(fmt.Sprintf(
				`INSERT INTO %s (k, col, raw, computed, kind) VALUES (?, ?, ?, ?, 1)
				   ON CONFLICT(k, col) DO UPDATE SET raw = excluded.raw,
				     computed = excluded.computed, kind = excluded.kind`, table),
				bi.keyOf(5000), 25, fmt.Sprint(i), fmt.Sprint(i)); err != nil {
				t.Fatalf("edit %s: %v", table, err)
			}
			samples = append(samples, time.Since(start))
		}
		for i := 1; i < len(samples); i++ {
			for j := i; j > 0 && samples[j] < samples[j-1]; j-- {
				samples[j], samples[j-1] = samples[j-1], samples[j]
			}
		}
		return samples[len(samples)/2] // median
	}
	editWide, editNarrow := edit("cells"), edit("cells_narrow")

	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	pct := func(a, b time.Duration) float64 { return 100 * (ms(a) - ms(b)) / ms(b) }
	t.Logf("A/B, one process, alternating, over identical cells:")
	t.Logf("  shift 50 keys x 26 cols  with style %.2f ms | without %.2f ms | %+.1f%%",
		ms(bestWide), ms(bestNarrow), pct(bestWide, bestNarrow))
	t.Logf("  window 250 rows (%d cells) with style %.2f ms | without %.2f ms | %+.1f%%",
		cells, ms(readWide), ms(readNarrow), pct(readWide, readNarrow))
	t.Logf("  ...against the read path AS IT SHIPPED in v5: %.2f ms | %+.1f%%",
		ms(readV5), pct(readWide, readV5))
	t.Logf("  single-cell edit (median of 200) with style %d us | without %d us",
		editWide.Microseconds(), editNarrow.Microseconds())
}

// ─── What the CASCADE costs the hot read path ─────────────────────────────────
//
// The window read runs on every push to every viewer, and the whole argument
// for the three-level cascade is that a column style is one record INSTEAD OF
// 10,000 cell writes — which is worthless if it makes the read slower on every
// push forever.
//
// Four arms over identical data, in ONE process, alternating, which is the only
// read instrument this project trusts (see the warning at the end of
// DATA-MODEL.md):
//
//	none  no level style at all — the resident predicates are both false and
//	      the read is the one that shipped, plus one bool test per window
//	col   one column styled — 26 resident integers, no query
//	row   every row of the window styled — one extra range scan over `rows`
//	both  the full cascade
//
// It reports rather than asserts: the number that matters is whether the
// cascade is inside the noise of a 3 ms read, and the machine decides that.
func TestCascadeLabReadCost(t *testing.T) {
	shiftLabEnabled(t)

	const (
		lo   = 4000
		hi   = 4249 // a buffer-sized window: 250 rows x 26 = 6,500 cells
		runs = 15
	)
	golden := goldenSheet(t, 9999)
	dir := t.TempDir()
	arms := []string{"none", "col", "row", "both"}
	for _, a := range arms {
		copyDB(t, golden, filepath.Join(dir, a+".db"))
	}
	c := NewSheetCache(dir, 8, time.Minute)
	defer c.Close()

	rowsIn := make([]int, 0, hi-lo+1)
	for r := lo; r <= hi; r++ {
		rowsIn = append(rowsIn, r)
	}
	sheets := map[string]*Sheet{}
	for _, a := range arms {
		sh, err := c.Open(a)
		if err != nil {
			t.Fatalf("open %s: %v", a, err)
		}
		sheets[a] = sh
		if a == "col" || a == "both" {
			if _, err := sh.SetColStyle([]int{3, 7}, StylePatch{BG: Set("#ffffcc")}); err != nil {
				t.Fatalf("%s: SetColStyle: %v", a, err)
			}
		}
		if a == "row" || a == "both" {
			start := time.Now()
			if _, err := sh.SetRowStyle(rowsIn, StylePatch{Bold: Set(true)}); err != nil {
				t.Fatalf("%s: SetRowStyle: %v", a, err)
			}
			t.Logf("%s: SetRowStyle over %d rows in %v (%d rows of `rows`)",
				a, len(rowsIn), time.Since(start).Round(time.Microsecond),
				countRows(t, sh, "rows"))
		}
	}

	read := func(sh *Sheet) (time.Duration, int) {
		start := time.Now()
		cells, err := sh.Window(lo, hi)
		if err != nil {
			t.Fatalf("window: %v", err)
		}
		return time.Since(start), len(cells)
	}
	for _, a := range arms { // warm the page cache for every arm before timing
		read(sheets[a])
	}

	times := map[string][]time.Duration{}
	for i := 0; i < runs; i++ {
		for _, a := range arms {
			d, n := read(sheets[a])
			if n != (hi-lo+1)*MaxCols {
				t.Fatalf("%s: window returned %d cells", a, n)
			}
			times[a] = append(times[a], d)
		}
	}
	base := median(times["none"])
	for _, a := range arms {
		m, lo95 := median(times[a]), fastest(times[a])
		t.Logf("window %d..%d, %s: median %.2f ms, fastest %.2f ms, %+.1f%% vs none",
			lo, hi, a, msf(m), msf(lo95), 100*(float64(m)-float64(base))/float64(base))
	}

	// And the thing the cascade exists to make cheap, against the thing it
	// replaces: styling a whole column.
	t.Run("styling a column", func(t *testing.T) {
		sh := sheets["none"]
		start := time.Now()
		d, err := sh.SetColStyle([]int{12}, StylePatch{BG: Set("#ccffcc")})
		if err != nil {
			t.Fatalf("SetColStyle: %v", err)
		}
		level := time.Since(start)
		t.Logf("SetColStyle over a 9,999-row column: %v, %d cells written, %d bands dirty",
			level.Round(time.Microsecond), len(d.Cells), len(d.Bands))

		// The per-cell equivalent, at the cap the toolbar actually enforces.
		refs := make([]CellRef, 0, 2000)
		for r := 0; r < 2000; r++ {
			refs = append(refs, CellRef{Row: r, Col: 13})
		}
		start = time.Now()
		if _, err := sh.SetStyle(refs, StylePatch{BG: Set("#ccffcc")}); err != nil {
			t.Fatalf("SetStyle: %v", err)
		}
		perCell := time.Since(start)
		t.Logf("SetStyle over %d cells of the same column: %v (%d cells now stored)",
			len(refs), perCell.Round(time.Microsecond), countRows(t, sh, "cells"))
		t.Logf("one record vs %d writes: %.0fx", len(refs),
			float64(perCell)/float64(level))
	})
}

func median(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func fastest(ds []time.Duration) time.Duration {
	out := ds[0]
	for _, d := range ds[1:] {
		if d < out {
			out = d
		}
	}
	return out
}

// ─── Did the cascade regress the read it did not use? ─────────────────────────
//
// The four-arm test above says what a LEVEL STYLE costs. This one asks the
// question the constraint actually cares about: on a sheet with no level style
// at all — which is every sheet until someone formats a row or a column — is
// the window read still the read that shipped?
//
// windowV6 is the pre-cascade Window body, verbatim, so both arms run in ONE
// process against the SAME open sheet, alternating. The only differences are
// the ones the cascade introduced: a `Cell` eight bytes wider, one bool test
// per window, and two predictable branches per cell.
func windowV6(s *Sheet, loRow, hiRow int) ([]Cell, error) {
	var out []Cell
	err := s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			hiRow := min(hiRow, bi.rows-1)
			if loRow > hiRow {
				return nil
			}
			nRows := hiRow - loRow + 1
			out = make([]Cell, nRows*MaxCols)
			for r := 0; r < nRows; r++ {
				for c := 0; c < MaxCols; c++ {
					out[r*MaxCols+c] = Cell{Ref: CellRef{Row: loRow + r, Col: c}}
				}
			}
			sty := s.styles()
			rows, err := db.Query(
				`SELECT k, col, raw, computed, kind, `+slotCols+`, style FROM cells
				  WHERE k BETWEEN ? AND ?
				  ORDER BY k, col`, bi.keyOf(loRow), bi.keyOf(hiRow))
			if err != nil {
				return err
			}
			defer rows.Close()
			var (
				k             int64
				col, kind     int
				style         int
				raw, computed string
				slots         nullSlots
			)
			args := make([]any, 0, 6+2*maxSlots)
			args = append(args, &k, &col, &raw, &computed, &kind)
			args = append(args, slots.scanArgs()...)
			args = append(args, &style)
			scan := bi.scanner()
			for rows.Next() {
				if err := rows.Scan(args...); err != nil {
					return err
				}
				row := scan.rank(k)
				if row < loRow || row > hiRow || col < 0 || col >= MaxCols {
					continue
				}
				if refs := slots.refs(bi); refs != nil {
					raw = RenderTemplate(raw, refs)
				}
				display := computed
				if style != 0 {
					if f := sty.tab.get(style).Fmt; f != FmtPlain {
						display = FormatValue(computed, f)
					}
				}
				out[(row-loRow)*MaxCols+col] = Cell{
					Ref:      CellRef{Row: row, Col: col},
					Raw:      raw,
					Computed: computed,
					Display:  display,
					Kind:     Kind(kind),
					Style:    style,
				}
			}
			return rows.Err()
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func TestCascadeLabWindowRegression(t *testing.T) {
	shiftLabEnabled(t)

	const (
		lo   = 4000
		hi   = 4249
		runs = 25
	)
	golden := goldenSheet(t, 9999)
	dir := t.TempDir()
	copyDB(t, golden, filepath.Join(dir, "ab.db"))
	c := NewSheetCache(dir, 4, time.Minute)
	defer c.Close()
	sh, err := c.Open("ab")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// 5% of cells styled, which is what TestStyleLabPlacement measured on, so
	// the format transform runs on the same population in both arms.
	if err := sh.use(func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE cells SET style = 1 WHERE (k * 26 + col) % 20 = 0`)
		return err
	}); err != nil {
		t.Fatalf("populate: %v", err)
	}
	if _, err := sh.SetStyle([]CellRef{{Row: 0, Col: 0}}, StylePatch{Bold: Set(true)}); err != nil {
		t.Fatalf("intern a style: %v", err)
	}

	var now, v6 []time.Duration
	sh.Window(lo, hi)    //nolint:errcheck // warm
	windowV6(sh, lo, hi) //nolint:errcheck
	for i := 0; i < runs; i++ {
		start := time.Now()
		a, err := sh.Window(lo, hi)
		if err != nil {
			t.Fatalf("Window: %v", err)
		}
		now = append(now, time.Since(start))

		start = time.Now()
		b, err := windowV6(sh, lo, hi)
		if err != nil {
			t.Fatalf("windowV6: %v", err)
		}
		v6 = append(v6, time.Since(start))

		if len(a) != len(b) {
			t.Fatalf("arms disagree: %d vs %d cells", len(a), len(b))
		}
	}
	mn, mv := median(now), median(v6)
	t.Logf("window %d..%d, %d cells, %d runs alternating:", lo, hi, (hi-lo+1)*MaxCols, runs)
	t.Logf("  with the cascade : median %.2f ms, fastest %.2f ms", msf(mn), msf(fastest(now)))
	t.Logf("  pre-cascade body : median %.2f ms, fastest %.2f ms", msf(mv), msf(fastest(v6)))
	t.Logf("  delta            : %+.1f%% median, %+.1f%% fastest",
		100*(float64(mn)-float64(mv))/float64(mv),
		100*(float64(fastest(now))-float64(fastest(v6)))/float64(fastest(v6)))
}

// TestCascadeLabMigrationCost is the claim "v6 -> v7 rewrites no cell", stated
// as a number on the biggest file this project has: 220,382 cells. The ALTER is
// on `cols` (26 rows at most) and the CREATE is of an empty table, so the cost
// should be independent of the cell count — which is exactly the property that
// is worth measuring rather than asserting.
func TestCascadeLabMigrationCost(t *testing.T) {
	shiftLabEnabled(t)

	golden := goldenSheet(t, 9999)
	dir := t.TempDir()
	path := filepath.Join(dir, "big.db")
	copyDB(t, golden, path)

	// Walk the file back to v6: drop the column and the table v7 added, and
	// re-stamp. The cells are untouched, which is the point.
	db := labOpen(t, path, "")
	var cells int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cells`).Scan(&cells); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cols (col, width, style) VALUES (2, 200, 0), (7, 48, 0)`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE cols DROP COLUMN style`,
		`DROP TABLE rows`,
		`PRAGMA user_version = 6`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	c := NewSheetCache(dir, 4, time.Minute)
	defer c.Close()
	start := time.Now()
	sh, err := c.Open("big")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	took := time.Since(start)
	widths, err := sh.ColWidths()
	if err != nil {
		t.Fatal(err)
	}
	if widths[2] != 200 || widths[7] != 48 {
		t.Fatalf("widths after migration: %v", widths)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("v6 -> v%d on %d cells: %v, %d -> %d bytes (%+d)",
		schemaVersion, cells, took.Round(time.Millisecond),
		before.Size(), after.Size(), after.Size()-before.Size())
}

// ─── What the CASCADE costs a MUTATION ────────────────────────────────────────
//
// The row level is the half that lives in the key space, so it is the half that
// a structural mutation has to move — and the constraint is that InsertRows and
// DeleteRows must not get slower for a table most sheets do not have a row in.
//
// Two identical sheets, alternating, one process: one with no level style at
// all, one carrying two column styles and 200 row styles spread across the
// sheet. The plain arm is the regression check (shiftRowMeta's existence probe
// against a mutation that shipped without one); the levelled arm is what
// carrying row state through a shift actually costs.
func TestCascadeLabMutationCost(t *testing.T) {
	shiftLabEnabled(t)

	const rounds = 7
	golden := goldenSheet(t, 9999)
	dir := t.TempDir()
	for _, a := range []string{"plain", "level"} {
		copyDB(t, golden, filepath.Join(dir, a+".db"))
	}
	c := NewSheetCache(dir, 8, time.Minute)
	defer c.Close()
	plain, err := c.Open("plain")
	if err != nil {
		t.Fatal(err)
	}
	level, err := c.Open("level")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := level.SetColStyle([]int{3, 7}, StylePatch{BG: Set("#ffffcc")}); err != nil {
		t.Fatal(err)
	}
	styled := make([]int, 0, 200)
	for i := 0; i < 200; i++ {
		styled = append(styled, i*47)
	}
	if _, err := level.SetRowStyle(styled, StylePatch{Bold: Set(true)}); err != nil {
		t.Fatal(err)
	}
	t.Logf("levelled arm carries %d column styles and %d rows of `rows`",
		len(level.ColStyles()), countRows(t, level, "rows"))

	ins := map[string][]time.Duration{}
	del := map[string][]time.Duration{}
	arms := []struct {
		name string
		sh   *Sheet
	}{{"plain", plain}, {"level", level}}
	for i := 0; i < rounds; i++ {
		for _, a := range arms {
			d, err := a.sh.InsertRows(0, 1)
			if err != nil {
				t.Fatalf("%s insert: %v", a.name, err)
			}
			ins[a.name] = append(ins[a.name], d.Stats.Elapsed)
		}
		for _, a := range arms {
			d, err := a.sh.DeleteRows(0, 1)
			if err != nil {
				t.Fatalf("%s delete: %v", a.name, err)
			}
			del[a.name] = append(del[a.name], d.Stats.Elapsed)
		}
	}

	// The single-cell edit, through the store rather than through raw SQL, on a
	// row that IS styled in the levelled arm.
	edit := func(sh *Sheet) time.Duration {
		const n = 200
		var out []time.Duration
		ref := CellRef{Row: 47, Col: 25}
		for i := 0; i < n; i++ {
			v := strconv.Itoa(i)
			start := time.Now()
			if err := sh.WriteCell(ref, v, []ComputedCell{{Ref: ref, Computed: v, Kind: KindNumber}}); err != nil {
				t.Fatalf("edit: %v", err)
			}
			out = append(out, time.Since(start))
		}
		return median(out)
	}
	editPlain, editLevel := edit(plain), edit(level)

	for _, a := range arms {
		t.Logf("%-6s InsertRows(0,1) median %.2f ms, fastest %.2f ms | "+
			"DeleteRows(0,1) median %.2f ms, fastest %.2f ms",
			a.name, msf(median(ins[a.name])), msf(fastest(ins[a.name])),
			msf(median(del[a.name])), msf(fastest(del[a.name])))
	}
	t.Logf("single-cell edit, median of 200: plain %d µs | levelled %d µs",
		editPlain.Microseconds(), editLevel.Microseconds())
}
