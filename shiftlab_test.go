package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ─── Stage 1: where does the 300ms of a structural shift actually go? ─────────
//
// DATA-MODEL.md reports a top-of-sheet InsertRows at ~331ms of which ~299ms is
// "shift cells", and calls that "~90% pure b-tree rewriting". This file exists
// to break that 299ms into its individual SQL statements and to test, one
// variable at a time, whether it is really b-tree work or something cheaper to
// fix (a spilling page cache, a temp table landing on disk, a page size that
// makes every row a partial write).
//
// The variant sweep it used to hold is DELETED, not moved: cache_size,
// temp_store, mmap_size, synchronous, page_size and an in-memory database were
// each measured against the row-keyed shift and every one of them landed inside
// run noise, which is what established that the cost was b-tree work and not
// I/O and sent the design to band-local keys. Those numbers are recorded in
// /tmp/ssobs/mutcost.md; re-running them against a shift that now moves 1,300
// rows instead of 220,000 would measure nothing.
//
// What is here instead is the claim the new model makes: the cost of a
// structural mutation does not depend on WHERE in the sheet it happens.
//
// Gated behind SHIFTLAB=1 because it seeds a full 9,999 x 26 sheet per
// position. `go test ./...` skips it.

func shiftLabEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("SHIFTLAB") == "" {
		t.Skip("set SHIFTLAB=1 to run the structural-shift cost lab")
	}
}

// goldenSheet seeds a full sheet once and returns the path to the closed
// database file. Every variant starts from a byte-identical copy of it, so the
// only thing that differs between runs is the variable under test.
func goldenSheet(t *testing.T, rows int) string {
	t.Helper()
	dir := t.TempDir()
	c := NewSheetCache(dir, 4, time.Minute)
	start := time.Now()
	if err := c.Seed("golden", rows, MaxCols); err != nil {
		t.Fatalf("seed golden: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close golden: %v", err)
	}
	path := filepath.Join(dir, "golden.db")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat golden: %v", err)
	}
	t.Logf("golden: %d x %d seeded in %v, %.2f MB on disk",
		rows, MaxCols, time.Since(start).Round(time.Millisecond),
		float64(st.Size())/(1<<20))
	return path
}

// copyDB copies the main database file (and any sidecars) to a fresh path.
func copyDB(t *testing.T, src, dst string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		in, err := os.Open(src + suffix)
		if err != nil {
			if suffix == "" {
				t.Fatalf("open %s: %v", src, err)
			}
			continue
		}
		out, err := os.Create(dst + suffix)
		if err != nil {
			in.Close()
			t.Fatalf("create %s: %v", dst+suffix, err)
		}
		if _, err := io.Copy(out, in); err != nil {
			t.Fatalf("copy %s: %v", src+suffix, err)
		}
		in.Close()
		if err := out.Close(); err != nil {
			t.Fatalf("close %s: %v", dst+suffix, err)
		}
	}
}

func labOpen(t *testing.T, path, pragmas string) *sql.DB {
	t.Helper()
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(off)" + pragmas
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	return db
}

// labWindow times the hot read path against a raw *sql.DB, doing exactly the
// work Sheet.Window does (including the A1 materialization) so a change cannot
// look good on the write path by wrecking the read path unnoticed.
func labWindow(db *sql.DB, bi *bandIndex, loRow, hiRow int) (time.Duration, int, error) {
	start := time.Now()
	rows, err := db.Query(
		`SELECT k, col, raw, computed, kind, `+slotCols+` FROM cells
		  WHERE k BETWEEN ? AND ?
		  ORDER BY k, col`, bi.keyOf(loRow), bi.keyOf(hiRow))
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
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	return time.Since(start), n, nil
}

// TestShiftLabScaling is the claim the whole storage change was made for, and
// it is a different claim from the one this file used to test.
//
// The row-keyed version of this test swept the insertion point down the sheet
// and fitted a LINE through it: 1.42 µs per moved cell + 0.4 ms, dead linear
// over four orders of magnitude, because an insert at row N rewrote every row
// below N. The number that mattered was the SLOPE.
//
// Under storage keys the shift is bounded by one band, so the same sweep should
// produce no slope at all. That is the result: not "faster" but POSITION
// INDEPENDENT — the worst case stops existing rather than getting cheaper. This
// test prints the sweep and fails if the top of the sheet is materially more
// expensive than the bottom, which is exactly the old failure mode.
func TestShiftLabScaling(t *testing.T) {
	shiftLabEnabled(t)

	ats := []int{0, 1, 2500, 5000, 7500, 9000, 9500, 9900, 9990, 9998}

	type row struct {
		at    int
		total time.Duration
		moved int
		bands int
	}
	var out []row
	for _, at := range ats {
		// A fresh copy of the same seeded sheet for every position, so the only
		// thing that differs between runs is where the insert lands.
		dir := t.TempDir()
		c := NewSheetCache(dir, 4, time.Minute)
		if err := c.Seed("sweep", DefaultRows-1, MaxCols); err != nil {
			t.Fatalf("seed: %v", err)
		}
		sh, err := c.Open("sweep")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		d, err := sh.InsertRows(at, 1)
		if err != nil {
			t.Fatalf("insert at %d: %v", at, err)
		}
		out = append(out, row{at, d.Stats.Elapsed, d.Stats.CellsMoved, len(d.Bands)})
		c.Close()
	}

	t.Log("| insert at row | cells moved | total | dirty bands |")
	t.Log("| --- | --- | --- | --- |")
	var worst, best time.Duration
	for i, r := range out {
		t.Logf("| %d | %d | %.2f ms | %d |", r.at, r.moved,
			float64(r.total.Microseconds())/1000, r.bands)
		if i == 0 || r.total > worst {
			worst = r.total
		}
		if i == 0 || r.total < best {
			best = r.total
		}
	}
	// The row-keyed implementation spanned 0.4 ms to 335 ms across this sweep.
	// Anything beyond a small multiple here means the shift is escaping its
	// band again.
	if worst > 8*best {
		t.Errorf("insert cost spans %v..%v across the sheet; it is supposed to be "+
			"position independent", best.Round(time.Microsecond), worst.Round(time.Microsecond))
	}
}

// TestShiftLabSplitCost measures what a band split costs and how often one
// happens, by hammering a single position — the amortization claim, checked
// rather than asserted.
func TestShiftLabSplitCost(t *testing.T) {
	shiftLabEnabled(t)

	const inserts = 500

	dir := t.TempDir()
	c := NewSheetCache(dir, 4, time.Minute)
	defer c.Close()
	// Room for the inserts: every one of them pushes the bottom row down, and
	// the grid is a fixed DefaultRows tall, so a sheet seeded to the brim would
	// (correctly) refuse the second insert.
	if err := c.Seed("split", DefaultRows-inserts-1, MaxCols); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sh, err := c.Open("split")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	var plain, split, rebal []time.Duration
	for i := 0; i < inserts; i++ {
		d, err := sh.InsertRows(25, 1)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		switch {
		case d.Stats.Rebalanced:
			rebal = append(rebal, d.Stats.Elapsed)
		case d.Stats.BandsSplit > 0:
			split = append(split, d.Stats.Elapsed)
		default:
			plain = append(plain, d.Stats.Elapsed)
		}
	}
	mean := func(ds []time.Duration) string {
		if len(ds) == 0 {
			return "none"
		}
		var sum time.Duration
		for _, d := range ds {
			sum += d
		}
		return fmt.Sprintf("%.2f ms", float64((sum/time.Duration(len(ds))).Microseconds())/1000)
	}
	t.Logf("%d inserts at one position on a full sheet:", inserts)
	t.Logf("  %4d plain      mean %s", len(plain), mean(plain))
	t.Logf("  %4d with split mean %s", len(split), mean(split))
	t.Logf("  %4d rebalanced mean %s", len(rebal), mean(rebal))
	t.Logf("  storage bands now %d", len(sh.index().bands))
}

// TestShiftLabWindowCost is the read path, measured on the fixture the write
// numbers are taken on: the change is only worth having if the window read that
// the clustering exists for did not move.
func TestShiftLabWindowCost(t *testing.T) {
	shiftLabEnabled(t)

	golden := goldenSheet(t, DefaultRows-1)
	path := filepath.Join(t.TempDir(), "win.db")
	copyDB(t, golden, path)
	db := labOpen(t, path, "")
	defer db.Close()
	bi, err := loadBandIndex(db)
	if err != nil || bi == nil {
		t.Fatalf("load band index: %v", err)
	}

	var best time.Duration
	var n int
	for i := 0; i < 8; i++ {
		d, got, err := labWindow(db, bi, 5000, 5249)
		if err != nil {
			t.Fatalf("window: %v", err)
		}
		if i == 0 || d < best {
			best, n = d, got
		}
	}
	t.Logf("window 250 rows (%d cells): %.2f ms", n, float64(best.Microseconds())/1000)
}

// TestShiftLabWindowAB is the read-path regression check, run the only way the
// cost work found trustworthy: both shapes in ONE process, alternating, over
// byte-identical data.
//
// The window read is what the clustering exists for and what every push to
// every viewer pays, so a write win that quietly cost 2x here would be a bad
// trade made invisibly. `cells_row` is a copy of the same cells keyed the OLD
// way, (row, col), read with the OLD query.
func TestShiftLabWindowAB(t *testing.T) {
	shiftLabEnabled(t)

	golden := goldenSheet(t, DefaultRows-1)
	path := filepath.Join(t.TempDir(), "ab.db")
	copyDB(t, golden, path)
	db := labOpen(t, path, "")
	defer db.Close()
	bi, err := loadBandIndex(db)
	if err != nil || bi == nil {
		t.Fatalf("load band index: %v", err)
	}

	// The same cells, keyed the way they used to be.
	if _, err := db.Exec(`
		CREATE TABLE cells_row (
		  "row"    INTEGER NOT NULL, col INTEGER NOT NULL,
		  raw TEXT NOT NULL DEFAULT '', computed TEXT NOT NULL DEFAULT '',
		  kind INTEGER NOT NULL DEFAULT 0,
		  ref0_row INTEGER, ref0_col INTEGER, ref1_row INTEGER, ref1_col INTEGER,
		  ref_span INTEGER NOT NULL DEFAULT 0,
		  PRIMARY KEY ("row", col)
		) WITHOUT ROWID`); err != nil {
		t.Fatalf("create row-keyed copy: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(
		`INSERT INTO cells_row SELECT (k / %d) * %d + (k %% %d), col, raw, computed, kind,
		    ref0_k, ref0_col, ref1_k, ref1_col, ref_span FROM cells`,
		keyStride, BandHeight, keyStride)); err != nil {
		t.Fatalf("fill row-keyed copy: %v", err)
	}
	// The same four partial reference indexes, or the edit comparison below
	// would be measuring index maintenance that only one side pays.
	if _, err := db.Exec(`
		CREATE INDEX cells_row_ref0 ON cells_row (ref0_row, ref0_col)
		  WHERE ref_span = 0 AND ref0_row IS NOT NULL;
		CREATE INDEX cells_row_ref1 ON cells_row (ref1_row, ref1_col)
		  WHERE ref_span = 0 AND ref1_row IS NOT NULL;
		CREATE INDEX cells_row_span_lo ON cells_row (ref0_row, ref1_row, ref0_col, ref1_col)
		  WHERE ref_span = 1;
		CREATE INDEX cells_row_span_hi ON cells_row (ref1_row, ref0_row, ref0_col, ref1_col)
		  WHERE ref_span = 1;`); err != nil {
		t.Fatalf("index row-keyed copy: %v", err)
	}

	rowWindow := func(lo, hi int) (time.Duration, int, error) {
		start := time.Now()
		rows, err := db.Query(
			`SELECT "row", col, raw, computed, kind, ref0_row, ref0_col, ref1_row, ref1_col
			   FROM cells_row WHERE "row" BETWEEN ? AND ? ORDER BY "row", col`, lo, hi)
		if err != nil {
			return 0, 0, err
		}
		defer rows.Close()
		var slots nullSlots
		n := 0
		for rows.Next() {
			var row, col, kind int
			var raw, computed string
			args := append([]any{&row, &col, &raw, &computed, &kind}, slots.scanArgs()...)
			if err := rows.Scan(args...); err != nil {
				return 0, 0, err
			}
			// Same work as the key path, minus the rank lookup: slot rows ARE
			// display rows here.
			refs := make([]CellRef, 0, maxSlots)
			for i := 0; i < maxSlots; i++ {
				if !slots[2*i].Valid {
					break
				}
				refs = append(refs, CellRef{Row: int(slots[2*i].Int64), Col: int(slots[2*i+1].Int64)})
			}
			if len(refs) > 0 {
				raw = RenderTemplate(raw, refs)
			}
			_ = raw
			n++
		}
		return time.Since(start), n, rows.Err()
	}

	var bestKey, bestRow time.Duration
	var nKey, nRow int
	for i := 0; i < 10; i++ {
		lo := 5000
		k, kn, err := labWindow(db, bi, lo, lo+249)
		if err != nil {
			t.Fatalf("key window: %v", err)
		}
		r, rn, err := rowWindow(lo, lo+249)
		if err != nil {
			t.Fatalf("row window: %v", err)
		}
		if i == 0 || k < bestKey {
			bestKey, nKey = k, kn
		}
		if i == 0 || r < bestRow {
			bestRow, nRow = r, rn
		}
	}
	if nKey != nRow {
		t.Fatalf("the two windows returned %d and %d cells", nKey, nRow)
	}
	t.Logf("window 250 rows (%d cells), min of 10 alternating runs:", nKey)
	t.Logf("  storage keys  %.2f ms", float64(bestKey.Microseconds())/1000)
	t.Logf("  display rows  %.2f ms", float64(bestRow.Microseconds())/1000)

	// The single-cell edit, the other hot path, on the same two tables.
	edit := func(fn func(i int) error) time.Duration {
		const n = 200
		var samples []time.Duration
		for i := 0; i < n; i++ {
			start := time.Now()
			if err := fn(i); err != nil {
				t.Fatalf("edit: %v", err)
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
	keyEdit := edit(func(i int) error {
		_, err := db.Exec(
			`INSERT INTO cells (k, col, raw, computed, kind) VALUES (?, ?, ?, ?, 1)
			   ON CONFLICT(k, col) DO UPDATE SET raw = excluded.raw,
			     computed = excluded.computed, kind = excluded.kind`,
			bi.keyOf(5000), 25, fmt.Sprint(i), fmt.Sprint(i))
		return err
	})
	rowEdit := edit(func(i int) error {
		_, err := db.Exec(
			`INSERT INTO cells_row ("row", col, raw, computed, kind) VALUES (?, ?, ?, ?, 1)
			   ON CONFLICT("row", col) DO UPDATE SET raw = excluded.raw,
			     computed = excluded.computed, kind = excluded.kind`,
			5000, 25, fmt.Sprint(i), fmt.Sprint(i))
		return err
	})
	t.Logf("single-cell edit, median of 200:")
	t.Logf("  storage keys  %d µs", keyEdit.Microseconds())
	t.Logf("  display rows  %d µs", rowEdit.Microseconds())
}
