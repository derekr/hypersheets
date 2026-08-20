// Command duckspike measures whether DuckDB earns a place beside the sheet
// store, using the queries a column-analytics feature would actually issue.
//
// It is a separate module on purpose. go-duckdb is cgo with a bundled static
// library, and the parent module's whole deploy story is
// `CGO_ENABLED=0 GOOS=linux go build` producing one static binary — so the
// question "would DuckDB pay?" has to be answerable without putting that at
// risk. Nothing here is imported by the app.
//
// THE FEATURE BEING STRESSED is a column-analytics surface: the aggregate a
// status bar shows for a selection, the group-by behind a pivot, the sort
// behind "order the sheet by this column", and the filter behind an autofilter.
// Those are the four shapes the current design cannot answer at scale, and the
// only ones that could justify a second engine — everything else the sheet does
// is a windowed range scan, which is what the band-key layout already makes a
// single contiguous b-tree read.
//
// The comparison is four-way, and the third and fourth are the point:
//
//	sqlite      the app's own driver against the app's own file
//	attach      DuckDB reading that file in place, no copy, no migration
//	tall        DuckDB with the cells COPYed into a native table, same shape
//	wide        DuckDB with the cells PIVOTED into one column per sheet column
//
// `tall` and `wide` are the same data. The gap between them is the honest cost
// of the storage model: `cells` is (k, col, value), which is an entity-attribute
// -value layout, and a column store handed an EAV table cannot do the thing a
// column store is for. Whether a DuckDB read model is worth having is mostly a
// question about that gap, not about DuckDB.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/marcboeker/go-duckdb/v2"
	_ "modernc.org/sqlite"
)

const (
	kindNumber = 1

	// bandHeight and keyStride mirror the store's band-key layout so the window
	// control reads a real span of display rows rather than a made-up key range.
	bandHeight = 50
	keyStride  = 4096
)

// keyOf is the canonical display-rank-to-storage-key map (bandkey.go).
func keyOf(row int) int64 {
	return int64(row/bandHeight)*keyStride + int64(row%bandHeight)
}

var (
	dbPath = flag.String("db", "/tmp/bigdata/big.db", "a sheet file written by the app")
	reps   = flag.Int("reps", 5, "timed repetitions per query, after one warm-up")
	col    = flag.Int("col", 3, "the numeric column the aggregates read")
	group  = flag.Int("group", 20, "the column the group-by groups on")
)

// query is one thing the feature asks for, spelled for each engine. Two
// spellings because the wide layout has different column names, not because the
// question differs.
type query struct {
	name string
	why  string
	tall string // sqlite, attach and the tall DuckDB table
	wide string // the pivoted DuckDB table
}

func queries(c, g int) []query {
	cv := fmt.Sprintf("CAST(computed AS DOUBLE)")
	return []query{
		{
			name: "aggregate",
			why:  "the status bar over a whole column",
			tall: fmt.Sprintf(`SELECT count(*), sum(%s), avg(%s), min(%s), max(%s)
			                     FROM cells WHERE col = %d AND kind = %d`, cv, cv, cv, cv, c, kindNumber),
			wide: fmt.Sprintf(`SELECT count(c%d), sum(c%d), avg(c%d), min(c%d), max(c%d) FROM wide`, c, c, c, c, c),
		},
		{
			name: "group-by",
			why:  "a pivot: bucket by one column, aggregate another",
			// EAV makes this a self-join, which is the shape's real cost.
			tall: fmt.Sprintf(`SELECT g.computed AS bucket, count(*) AS n, sum(CAST(v.computed AS DOUBLE)) AS total
			                     FROM cells g JOIN cells v ON v.k = g.k
			                    WHERE g.col = %d AND v.col = %d AND v.kind = %d
			                    GROUP BY 1 ORDER BY total DESC LIMIT 20`, g, c, kindNumber),
			wide: fmt.Sprintf(`SELECT c%d AS bucket, count(*) AS n, sum(c%d) AS total
			                     FROM wide GROUP BY 1 ORDER BY total DESC LIMIT 20`, g, c),
		},
		{
			name: "sort-top-n",
			why:  "order the sheet by a column, show the first screen",
			tall: fmt.Sprintf(`SELECT k, %s AS v FROM cells
			                    WHERE col = %d AND kind = %d ORDER BY v DESC LIMIT 250`, cv, c, kindNumber),
			wide: fmt.Sprintf(`SELECT k, c%d AS v FROM wide ORDER BY v DESC LIMIT 250`, c),
		},
		{
			name: "filter+agg",
			why:  "an autofilter, then the status bar again",
			tall: fmt.Sprintf(`SELECT count(*), sum(%s) FROM cells
			                    WHERE col = %d AND kind = %d AND %s > 500`, cv, c, kindNumber, cv),
			wide: fmt.Sprintf(`SELECT count(*), sum(c%d) FROM wide WHERE c%d > 500`, c, c),
		},
		{
			name: "window (control)",
			why:  "THE APP'S HOT PATH — one screen of cells, what every push reads",
			tall: fmt.Sprintf(`SELECT k, col, raw, computed, kind FROM cells
			                    WHERE k BETWEEN %d AND %d ORDER BY k, col`, keyOf(120000), keyOf(120250)),
			wide: fmt.Sprintf(`SELECT * FROM wide WHERE k BETWEEN %d AND %d ORDER BY k`, keyOf(120000), keyOf(120250)),
		},
	}
}

func main() {
	flag.Parse()
	if _, err := os.Stat(*dbPath); err != nil {
		log.Fatalf("no sheet file at %s — seed one first:\n"+
			"  go build -o /tmp/ssbig . && /tmp/ssbig -data /tmp/bigdata -sheet big -seed-rows 200000 -limits=false", *dbPath)
	}
	info, _ := os.Stat(*dbPath)
	fmt.Printf("sheet file: %s (%s)\n", *dbPath, mib(info.Size()))

	lite := open(t(), "sqlite", "file:"+*dbPath+"?mode=ro")
	defer lite.Close()
	var cells, rows int
	must(lite.QueryRow(`SELECT count(*), count(DISTINCT k) FROM cells`).Scan(&cells, &rows))
	fmt.Printf("cells: %s across %s rows\n\n", commas(cells), commas(rows))

	duck := open(t(), "duckdb", "")
	defer duck.Close()

	// ATTACH reads the SQLite file in place: no copy, no migration, and the app
	// keeps writing to it. This is the cheapest possible version of the idea and
	// therefore the one worth measuring first.
	tAttach := timeIt(func() {
		mustExec(duck, `INSTALL sqlite; LOAD sqlite;`)
		mustExec(duck, fmt.Sprintf(`ATTACH '%s' AS lite (TYPE SQLITE, READ_ONLY)`, *dbPath))
	})
	fmt.Printf("duckdb ATTACH sqlite:            %8s  (no copy)\n", ms(tAttach))

	tTall := timeIt(func() {
		mustExec(duck, `CREATE TABLE cells AS SELECT k, col, raw, computed, kind FROM lite.cells`)
	})
	fmt.Printf("duckdb materialise tall:         %8s\n", ms(tTall))

	tWide := timeIt(func() { buildWide(duck) })
	fmt.Printf("duckdb materialise wide (pivot): %8s\n\n", ms(tWide))

	qs := queries(*col, *group)
	type row struct {
		q                              query
		sqlite, attach, dtall, dwide   time.Duration
		nSqlite, nAttach, nTall, nWide int
	}
	var out []row
	for _, q := range qs {
		var r row
		r.q = q
		r.sqlite, r.nSqlite = bench(lite, q.tall)
		r.attach, r.nAttach = bench(duck, strings.ReplaceAll(q.tall, "FROM cells", "FROM lite.cells"))
		r.dtall, r.nTall = bench(duck, q.tall)
		r.dwide, r.nWide = bench(duck, q.wide)
		out = append(out, r)
	}

	fmt.Printf("%-16s %12s %12s %12s %12s   %s\n", "query", "sqlite", "duck/attach", "duck/tall", "duck/wide", "what it is")
	fmt.Println(strings.Repeat("-", 110))
	for _, r := range out {
		fmt.Printf("%-16s %12s %12s %12s %12s   %s\n",
			r.q.name, ms(r.sqlite), ms(r.attach), ms(r.dtall), ms(r.dwide), r.q.why)
	}
	fmt.Println()
	for _, r := range out {
		if r.nSqlite != r.nAttach || r.nSqlite != r.nTall {
			fmt.Printf("!! %s returned different row counts: sqlite=%d attach=%d tall=%d\n",
				r.q.name, r.nSqlite, r.nAttach, r.nTall)
		}
	}
	fmt.Printf("speedups vs sqlite (higher = duckdb wins):\n")
	for _, r := range out {
		fmt.Printf("  %-16s attach %5.1fx   tall %6.1fx   wide %7.1fx\n",
			r.q.name, ratio(r.sqlite, r.attach), ratio(r.sqlite, r.dtall), ratio(r.sqlite, r.dwide))
	}

	freshness(duck, tWide)

	// What holding one sheet's read model costs, since the answer decides whether
	// it can exist per sheet or only per open analytics panel.
	var all, wideOnly string
	_ = duck.QueryRow(`SELECT memory_usage FROM pragma_database_size()`).Scan(&all)
	// A real read model keeps only the pivoted table: the tall copy is scaffolding
	// for this comparison and the attachment is not data. The difference decides
	// whether a model can exist per sheet or only per open panel.
	mustExec(duck, `DROP TABLE cells`)
	mustExec(duck, `DETACH lite`)
	mustExec(duck, `CHECKPOINT`)
	_ = duck.QueryRow(`SELECT memory_usage FROM pragma_database_size()`).Scan(&wideOnly)
	fmt.Printf("\nduckdb memory: %s holding all three, %s holding only the wide model\n", all, wideOnly)
}

// freshness is the measurement the speedups are worthless without.
//
// A materialised read model is stale the instant somebody types, and this app's
// whole point is that somebody is always typing. So the question is never "how
// fast is the query" — it is "what does one edit cost to fold in", against a
// write path that currently commits a cell in about 0.1ms and a push that has to
// go out within a frame.
//
// Three answers, because they are three different orders of magnitude: rebuild
// everything, restate one row, restate a screen's worth.
func freshness(db *sql.DB, fullRebuild time.Duration) {
	fmt.Printf("\nkeeping the wide model fresh (the write path is ~0.1ms/cell):\n")
	fmt.Printf("  %-28s %9s\n", "full rebuild", ms(fullRebuild))

	one := median(*reps, func() {
		mustExec(db, fmt.Sprintf(`DELETE FROM wide WHERE k = %d`, keyOf(120000)))
		mustExec(db, buildWideSQL(fmt.Sprintf(`WHERE k = %d`, keyOf(120000)), "INSERT INTO wide "))
	})
	fmt.Printf("  %-28s %9s\n", "one row (one edit)", ms(one))

	lo, hi := keyOf(130000), keyOf(130250)
	win := median(*reps, func() {
		mustExec(db, fmt.Sprintf(`DELETE FROM wide WHERE k BETWEEN %d AND %d`, lo, hi))
		mustExec(db, buildWideSQL(fmt.Sprintf(`WHERE k BETWEEN %d AND %d`, lo, hi), "INSERT INTO wide "))
	})
	fmt.Printf("  %-28s %9s\n", "250 rows (a paste)", ms(win))

	// Both of those re-read the SQLite file, and the cost says the predicate is
	// not reaching it: one row costs almost what all 200,000 cost. So a read
	// model cannot be refreshed by asking the source what changed.
	//
	// It does not have to be. The write path already knows — it has the cell, the
	// row and the value in hand at the moment it commits, which is the same
	// reason the push knows what to send. Fed rather than polled, the update is a
	// single-column write.
	fed := median(*reps, func() {
		mustExec(db, fmt.Sprintf(`UPDATE wide SET c3 = 42.0 WHERE k = %d`, keyOf(120000)))
	})
	fmt.Printf("  %-28s %9s   <- fed by the write path\n", "one cell (UPDATE)", ms(fed))

	// And the same thing for a paste, as one statement rather than 250.
	fedBatch := median(*reps, func() {
		mustExec(db, fmt.Sprintf(
			`UPDATE wide SET c3 = c3 + 1 WHERE k BETWEEN %d AND %d`, lo, hi))
	})
	fmt.Printf("  %-28s %9s   <- fed by the write path\n", "250 cells (UPDATE)", ms(fedBatch))
}

func median(n int, f func()) time.Duration {
	f()
	ds := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		ds = append(ds, timeIt(f))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

// buildWide pivots (k, col, value) into one column per sheet column. A column
// store handed an EAV table has nothing to be good at; this is what a read model
// would actually materialise.
func buildWide(db *sql.DB) { mustExec(db, buildWideSQL("", "CREATE TABLE wide AS ")) }

func buildWideSQL(where, prefix string) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(`SELECT k`)
	for c := 0; c < 26; c++ {
		fmt.Fprintf(&b, `, max(CASE WHEN col = %d AND kind = %d THEN CAST(computed AS DOUBLE) END) AS c%d`,
			c, kindNumber, c)
	}
	b.WriteString(` FROM lite.cells `)
	b.WriteString(where)
	b.WriteString(` GROUP BY k`)
	return b.String()
}

// bench runs a query once to warm, then *reps times, and reports the median so
// one unlucky page fault does not become the headline.
func bench(db *sql.DB, q string) (time.Duration, int) {
	n := drain(db, q)
	ds := make([]time.Duration, 0, *reps)
	for i := 0; i < *reps; i++ {
		start := time.Now()
		drain(db, q)
		ds = append(ds, time.Since(start))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2], n
}

// drain reads every row, because a query that is never consumed is a query that
// was never really run.
func drain(db *sql.DB, q string) int {
	rows, err := db.Query(q)
	if err != nil {
		log.Fatalf("query failed:\n%s\n%v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	must(err)
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for rows.Next() {
		must(rows.Scan(ptrs...))
		n++
	}
	must(rows.Err())
	return n
}

func open(_ struct{}, driver, dsn string) *sql.DB {
	db, err := sql.Open(driver, dsn)
	must(err)
	must(db.Ping())
	return db
}

func t() struct{} { return struct{}{} }

func mustExec(db *sql.DB, q string) {
	if _, err := db.Exec(q); err != nil {
		log.Fatalf("exec failed:\n%s\n%v", q, err)
	}
}

func timeIt(f func()) time.Duration { start := time.Now(); f(); return time.Since(start) }

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func ms(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
}

func ratio(base, other time.Duration) float64 {
	if other == 0 {
		return 0
	}
	return float64(base) / float64(other)
}

func mib(n int64) string { return fmt.Sprintf("%.0f MB", float64(n)/(1<<20)) }

func commas(n int) string {
	s := fmt.Sprint(n)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
