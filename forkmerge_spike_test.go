package main

// forkmerge_spike_test.go — the body of the fork/merge spike.
//
// Run it through its front door: `go run ./spike/forkmerge`. See that file for
// why the experiment lives here in the root package rather than in spike/.
// Findings are written up in SPIKE-FORK-MERGE.md.
//
// The question: if "offline" were a deliberate FORK of a sheet — its own id, its
// own SQLite file, nothing merging automatically — what does merging two forks
// back together actually cost in THIS codebase? Three claims are under test:
//
//  1. band keys mean rows never shift, so matching rows across a fork is a join
//     on the storage key;
//  2. only inputs need merging, because recalc.go re-derives every computed
//     value from the literals;
//  3. formula references are display coordinates, so two independent structural
//     mutations from a common base do not commute.
//
// Nothing here modifies the sheet package. It reads the band index directly
// (sh.readIndex) because storage keys are the subject of claim 1 and no exported
// read path exposes them.

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

var (
	fmSeedRows = flag.Int("fm.seed-rows", 250, "rows to seed the base sheet with")
	fmCase     = flag.String("fm.case", "", "run one scenario by name; empty runs all")
	fmKeep     = flag.String("fm.keep", "", "keep sheet files in this directory")
	fmVerbose  = flag.Bool("fm.verbose", false, "print every merged cell, not just the summary")
)

// ─── Harness ──────────────────────────────────────────────────────────────────

type fmEnv struct {
	t      *testing.T
	dir    string
	cache  *SheetCache
	actors *Actors
	n      int // fork counter, for unique ids
}

func fmSetup(t *testing.T) *fmEnv {
	t.Helper()
	dir := *fmKeep
	if dir == "" {
		dir = t.TempDir()
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cache := NewSheetCache(dir, 32, time.Minute)
	actors := NewActors(cache)
	t.Cleanup(func() {
		actors.Close()
		_ = cache.Close()
	})
	return &fmEnv{t: t, dir: dir, cache: cache, actors: actors}
}

// base seeds a fresh sheet under a unique id and returns it.
func (e *fmEnv) base(name string) string {
	e.t.Helper()
	e.n++
	id := fmt.Sprintf("%s-base-%d", name, e.n)
	if err := e.cache.Seed(id, *fmSeedRows, MaxCols); err != nil {
		e.t.Fatalf("seed %s: %v", id, err)
	}
	return id
}

// ─── The fork ─────────────────────────────────────────────────────────────────
//
// The cheapest honest fork is a copy of the SQLite file, and store.go opens
// every sheet with journal_mode(WAL). A WAL database is two files: committed
// pages may live in `<id>.db-wal` and not yet in `<id>.db`, so copying the .db
// alone copies the sheet as of the last checkpoint, silently. fmForkCopy is that
// mistake, made deliberately, so scenario "fork" can measure it.

// fmForkCopy copies only the main database file, with no checkpoint. Wrong on
// purpose.
func (e *fmEnv) fmForkCopy(src, dst string) {
	e.t.Helper()
	fmCopyFile(e.t, filepath.Join(e.dir, src+".db"), filepath.Join(e.dir, dst+".db"))
}

// fork is the sound version: checkpoint the WAL back into the main file, then
// copy it. The source's writer is idle (we are between actor commands), so the
// checkpointed file is a consistent snapshot.
func (e *fmEnv) fork(src, dst string) string {
	e.t.Helper()
	e.checkpoint(src)
	e.fmForkCopy(src, dst)
	return dst
}

// forkVacuum is the other sound version: SQLite writes the snapshot itself.
func (e *fmEnv) forkVacuum(src, dst string) string {
	e.t.Helper()
	sh, err := e.cache.Open(src)
	if err != nil {
		e.t.Fatal(err)
	}
	target := filepath.Join(e.dir, dst+".db")
	_ = os.Remove(target)
	if err := sh.use(func(db *sql.DB) error {
		_, err := db.Exec(`VACUUM INTO ?`, target)
		return err
	}); err != nil {
		e.t.Fatalf("VACUUM INTO: %v", err)
	}
	return dst
}

func (e *fmEnv) checkpoint(id string) {
	e.t.Helper()
	sh, err := e.cache.Open(id)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := sh.use(func(db *sql.DB) error {
		var busy, logPages, ckpt int
		return db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logPages, &ckpt)
	}); err != nil {
		e.t.Fatalf("checkpoint %s: %v", id, err)
	}
}

func fmCopyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}

func fmSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

// ─── Snapshots, keyed by storage key ──────────────────────────────────────────

// fmKey is the join key claim 1 proposes: the storage key of the row, and the
// column. If band keys really are row identities, this is stable across a fork.
type fmKey struct {
	k   int64
	col int
}

func (k fmKey) String() string { return fmt.Sprintf("k=%d,%s", k.k, string(rune('A'+k.col))) }

type fmSnap struct {
	id       string
	rows     int              // allocated extent
	used     int              // rows actually scanned
	keyOf    []int64          // display row -> storage key
	rowOf    map[int64]int    // storage key -> display row
	raw      map[fmKey]string // what was typed, by storage key
	computed map[fmKey]string // the derived value, by storage key
	dispRaw  map[CellRef]string
	dispComp map[CellRef]string
	lastSeq  int64
}

func fmSnapshot(t *testing.T, e *fmEnv, id string) *fmSnap {
	t.Helper()
	sh, err := e.cache.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	used, err := sh.UsedRows()
	if err != nil {
		t.Fatal(err)
	}
	// A couple of rows of margin so a mutation that pushed content down is still
	// inside the window.
	used = min(used+4, sh.Rows())
	s := &fmSnap{
		id: id, rows: sh.Rows(), used: used,
		rowOf:    make(map[int64]int, used),
		raw:      make(map[fmKey]string),
		computed: make(map[fmKey]string),
		dispRaw:  make(map[CellRef]string),
		dispComp: make(map[CellRef]string),
	}
	if err := sh.readIndex(func(bi *bandIndex) error {
		s.keyOf = make([]int64, used)
		for r := 0; r < used; r++ {
			s.keyOf[r] = bi.keyOf(r)
			s.rowOf[s.keyOf[r]] = r
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if used > 0 {
		cells, err := sh.Window(0, used-1)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cells {
			if c.Raw == "" && c.Computed == "" {
				continue
			}
			k := fmKey{s.keyOf[c.Ref.Row], c.Ref.Col}
			s.raw[k] = c.Raw
			s.computed[k] = c.Computed
			s.dispRaw[c.Ref] = c.Raw
			s.dispComp[c.Ref] = c.Computed
		}
	}
	if s.lastSeq, err = sh.LastSeq(); err != nil {
		t.Fatal(err)
	}
	return s
}

// ─── Three-way diff and merge, over (band key, column) ────────────────────────

// fmDiff is one side's changes against the base, expressed in the join key claim
// 1 proposes. An empty string means "the cell is now blank".
func fmDiff(base, side *fmSnap) map[fmKey]string {
	out := map[fmKey]string{}
	for k, v := range side.raw {
		if b, ok := base.raw[k]; !ok || b != v {
			out[k] = v
		}
	}
	for k, b := range base.raw {
		if _, ok := side.raw[k]; !ok && b != "" {
			out[k] = ""
		}
	}
	return out
}

type fmConflict struct {
	key                fmKey
	base, ours, theirs string
}

// fmMerge is the three-way merge: take a change made on exactly one side, and
// report a cell both sides moved to different values as a conflict. Computed
// values are never merged — they are not even looked at.
func fmMerge(base, ours, theirs *fmSnap) (map[fmKey]string, []fmConflict) {
	dOurs, dTheirs := fmDiff(base, ours), fmDiff(base, theirs)
	take := map[fmKey]string{}
	var conflicts []fmConflict
	for k, v := range dOurs {
		if tv, both := dTheirs[k]; both && tv != v {
			conflicts = append(conflicts, fmConflict{k, base.raw[k], v, tv})
			continue
		}
		take[k] = v
	}
	for k, v := range dTheirs {
		if _, done := dOurs[k]; done {
			continue
		}
		take[k] = v
	}
	sort.Slice(conflicts, func(i, j int) bool {
		a, b := conflicts[i].key, conflicts[j].key
		if a.k != b.k {
			return a.k < b.k
		}
		return a.col < b.col
	})
	return take, conflicts
}

// fmApply writes the merged raw text into a sheet and lets ApplyBatch re-derive
// everything downstream. This is claim 2 in one function call: the merge decides
// literals, the engine decides values.
func fmApply(t *testing.T, e *fmEnv, id string, take map[fmKey]string) (BatchResult, int) {
	t.Helper()
	var res BatchResult
	orphans := 0
	err := e.actors.Do(id, func(sh *Sheet) error {
		var writes []BatchWrite
		return sh.readIndex(func(bi *bandIndex) error {
			for k, v := range take {
				r := bi.rankOf(k.k)
				if r < 0 {
					// A key the merge produced that names no row on the target.
					// Not an error to investigate — a measurement.
					orphans++
					continue
				}
				writes = append(writes, BatchWrite{Ref: CellRef{r, k.col}, Raw: v})
			}
			sort.Slice(writes, func(i, j int) bool {
				if writes[i].Ref.Row != writes[j].Ref.Row {
					return writes[i].Ref.Row < writes[j].Ref.Row
				}
				return writes[i].Ref.Col < writes[j].Ref.Col
			})
			if len(writes) == 0 {
				return nil
			}
			var err error
			res, err = sh.ApplyBatch(writes)
			return err
		})
	})
	if err != nil {
		t.Fatalf("apply merge to %s: %v", id, err)
	}
	return res, orphans
}

// fmDisplayDiff compares two sheets the way a user would: same text and same
// value at the same A1 address.
func fmDisplayDiff(a, b *fmSnap) []string {
	seen := map[CellRef]bool{}
	var out []string
	cmp := func(ref CellRef) {
		if seen[ref] {
			return
		}
		seen[ref] = true
		ar, br := a.dispRaw[ref], b.dispRaw[ref]
		ac, bc := a.dispComp[ref], b.dispComp[ref]
		if ar != br || ac != bc {
			out = append(out, fmt.Sprintf("%s: %s=%q/%q  %s=%q/%q",
				ref, a.id, ar, ac, b.id, br, bc))
		}
	}
	for ref := range a.dispRaw {
		cmp(ref)
	}
	for ref := range b.dispRaw {
		cmp(ref)
	}
	sort.Strings(out)
	return out
}

// ─── Writes and structural mutations, through the actor ───────────────────────

func fmWrite(t *testing.T, e *fmEnv, id string, cells ...BatchWrite) {
	t.Helper()
	if err := e.actors.Do(id, func(sh *Sheet) error {
		_, err := sh.ApplyBatch(cells)
		return err
	}); err != nil {
		t.Fatalf("write to %s: %v", id, err)
	}
}

func fmInsertRows(t *testing.T, e *fmEnv, id string, at, n int) Dirty {
	t.Helper()
	var d Dirty
	if err := e.actors.Do(id, func(sh *Sheet) error {
		var err error
		d, err = sh.InsertRows(at, n)
		return err
	}); err != nil {
		t.Fatalf("InsertRows(%d,%d) on %s: %v", at, n, id, err)
	}
	return d
}

func fmDeleteRows(t *testing.T, e *fmEnv, id string, at, n int) Dirty {
	t.Helper()
	var d Dirty
	if err := e.actors.Do(id, func(sh *Sheet) error {
		var err error
		d, err = sh.DeleteRows(at, n)
		return err
	}); err != nil {
		t.Fatalf("DeleteRows(%d,%d) on %s: %v", at, n, id, err)
	}
	return d
}

// fmStructuralSince reads the durable event log for the structural mutations a
// fork made after the fork point. This is the divergence detector claim 3 asks
// for, and it is the only durable record of one.
func fmStructuralSince(t *testing.T, e *fmEnv, id string, afterSeq int64) []Event {
	t.Helper()
	sh, err := e.cache.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := sh.Events(afterSeq, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, ev := range evs {
		if ev.Ref == "#rows" || ev.Ref == "#cols" {
			out = append(out, ev)
		}
	}
	return out
}

func ref(s string) CellRef {
	r, err := ParseRef(s)
	if err != nil {
		panic(err)
	}
	return r
}

func w(a1, raw string) BatchWrite { return BatchWrite{Ref: ref(a1), Raw: raw} }

// ─── The spike ────────────────────────────────────────────────────────────────

func TestForkMergeSpike(t *testing.T) {
	run := func(name string) bool { return *fmCase == "" || *fmCase == name }

	if run("fork") {
		t.Run("fork-is-a-file-copy", func(t *testing.T) { fmScenarioFork(t) })
	}
	if run("keys") {
		t.Run("are-band-keys-row-identities", func(t *testing.T) { fmScenarioKeys(t) })
	}
	if run("collide") {
		t.Run("do-two-forks-mint-the-same-key", func(t *testing.T) { fmScenarioCollide(t) })
	}
	if run("log") {
		t.Run("where-does-the-fork-point-come-from", func(t *testing.T) { fmScenarioLog(t) })
	}
	if run("a") {
		t.Run("a-disjoint-value-edits", func(t *testing.T) { fmScenarioA(t) })
	}
	if run("b") {
		t.Run("b-same-cell-both-sides", func(t *testing.T) { fmScenarioB(t) })
	}
	if run("c") {
		t.Run("c-formula-here-input-there", func(t *testing.T) { fmScenarioC(t) })
	}
	if run("d") {
		t.Run("d-row-insert-one-side", func(t *testing.T) { fmScenarioD(t) })
	}
	if run("e") {
		t.Run("e-row-inserts-both-sides", func(t *testing.T) { fmScenarioE(t) })
	}
	if run("f") {
		t.Run("f-delete-here-edit-there", func(t *testing.T) { fmScenarioF(t) })
	}
}

// ─── fork: what a file copy actually copies ───────────────────────────────────

func fmScenarioFork(t *testing.T) {
	e := fmSetup(t)
	base := e.base("fork")

	// One ordinary edit after the seed, committed through the actor.
	fmWrite(t, e, base, w("A1", "12345"))

	db := filepath.Join(e.dir, base+".db")
	t.Logf("after one committed edit: %s.db=%d  -wal=%d  -shm=%d bytes",
		base, fmSize(db), fmSize(db+"-wal"), fmSize(db+"-shm"))

	// (1) the naive fork: copy the .db, leave the WAL behind.
	e.fmForkCopy(base, "naive")
	naive := fmSnapshot(t, e, "naive")
	got := naive.dispRaw[ref("A1")]
	t.Logf("naive .db-only copy: A1 = %q (want \"12345\"), used rows = %d", got, naive.used)

	// (2) checkpoint, then copy.
	e.fork(base, "ckpt")
	ck := fmSnapshot(t, e, "ckpt")
	t.Logf("checkpoint+copy:     A1 = %q, used rows = %d, %d bytes",
		ck.dispRaw[ref("A1")], ck.used, fmSize(filepath.Join(e.dir, "ckpt.db")))

	// (3) VACUUM INTO: SQLite writes the snapshot itself.
	start := time.Now()
	e.forkVacuum(base, "vac")
	vacTook := time.Since(start)
	vac := fmSnapshot(t, e, "vac")
	t.Logf("VACUUM INTO:         A1 = %q, used rows = %d, %d bytes, %v",
		vac.dispRaw[ref("A1")], vac.used, fmSize(filepath.Join(e.dir, "vac.db")), vacTook)

	if d := fmDisplayDiff(ck, vac); len(d) != 0 {
		t.Errorf("checkpoint-copy and VACUUM INTO forks differ in %d cells: %v", len(d), d[:min(5, len(d))])
	}
	if got != "12345" {
		t.Logf("FINDING: a bare .db copy lost the committed edit — the WAL is part of the sheet")
	}
}

// ─── keys: is a band key a row identity? ──────────────────────────────────────

func fmScenarioKeys(t *testing.T) {
	e := fmSetup(t)
	base := e.base("keys")

	before := fmSnapshot(t, e, base)
	// Something identifiable to follow: mark a row below the insertion point.
	fmWrite(t, e, base, w("Z10", "marker"))
	before = fmSnapshot(t, e, base)
	markKey := before.keyOf[9]

	for _, at := range []int{0, 3, 60, 175} {
		id := fmt.Sprintf("keys-ins-%d", at)
		e.fork(base, id)
		b4 := fmSnapshot(t, e, id)
		d := fmInsertRows(t, e, id, at, 1)
		af := fmSnapshot(t, e, id)

		// How many rows kept their key, and how many of the keys that survived
		// still name the same content?
		moved := 0
		for r := 0; r < min(len(b4.keyOf), len(af.keyOf))-1; r++ {
			// The logical row that was at rank r is at rank r (above the insert)
			// or r+1 (at or below it).
			nr := r
			if r >= at {
				nr = r + 1
			}
			if nr >= len(af.keyOf) {
				break
			}
			if b4.keyOf[r] != af.keyOf[nr] {
				moved++
			}
		}
		// The false-diff count: keys whose CONTENT changed although no cell was
		// edited. Every one of these is a row a key join would mismatch.
		false3way := 0
		for k, v := range b4.raw {
			if af.raw[k] != v {
				false3way++
			}
		}
		for k := range af.raw {
			if _, ok := b4.raw[k]; !ok {
				false3way++
			}
		}
		t.Logf("InsertRows(%d,1): %d of %d logical rows changed storage key; "+
			"a key-join three-way diff reports %d spurious cell changes (0 cells were edited)",
			at, moved, len(b4.keyOf)-1, false3way)
		t.Logf("  what the band model DID buy: %d cells rewritten, %d formulas rewritten, "+
			"%d bands split, rebalanced=%v, %v",
			d.Stats.CellsMoved, d.Stats.FormulasRewrit, d.Stats.BandsSplit, d.Stats.Rebalanced, d.Stats.Elapsed)
		if at <= 9 {
			t.Logf("  marker Z10 was k=%d before; after the insert k=%d holds %q and the marker is at k=%d",
				markKey, markKey, af.raw[fmKey{markKey, 25}], af.keyOf[10])
		}
	}

	// Keys are not even stable within one fork's own session: a band that
	// absorbs inserts splits, and a band with no slack forces a rebalance, which
	// re-keys the whole sheet.
	e.fork(base, "keys-hammer")
	firstSplit, firstRebalance, moves := -1, -1, 0
	for i := 0; i < 600; i++ {
		d := fmInsertRows(t, e, "keys-hammer", 3, 1)
		moves += d.Stats.CellsMoved
		if d.Stats.BandsSplit > 0 && firstSplit < 0 {
			firstSplit = i + 1
		}
		if d.Stats.Rebalanced && firstRebalance < 0 {
			firstRebalance = i + 1
		}
	}
	t.Logf("600 inserts at rank 3 on one fork: first band split at insert #%d, "+
		"first full rebalance at insert #%d (-1 = not reached), %d cells rewritten in total",
		firstSplit, firstRebalance, moves)
	t.Logf("  a rebalance re-keys the sheet, so every band key on that fork changes at once")

	// The column axis has no band model at all (mutate.go: "the column axis is
	// O(sheet)"), so the other half of the join key is worth measuring too.
	// A fresh seed, because column Z on `base` holds the marker and the seed
	// leaves Z empty precisely so a column insert has somewhere to push into.
	e.fork(e.base("keys-col"), "keys-col")
	colBefore := fmSnapshot(t, e, "keys-col")
	var cd Dirty
	if err := e.actors.Do("keys-col", func(sh *Sheet) error {
		var err error
		cd, err = sh.InsertCols(1, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	colAfter := fmSnapshot(t, e, "keys-col")
	keyMoved, falseCol := 0, 0
	for r := 0; r < min(len(colBefore.keyOf), len(colAfter.keyOf)); r++ {
		if colBefore.keyOf[r] != colAfter.keyOf[r] {
			keyMoved++
		}
	}
	for k, v := range colBefore.raw {
		if colAfter.raw[k] != v {
			falseCol++
		}
	}
	t.Logf("InsertCols(1,1): %d rows changed storage key, but %d (key,col) pairs changed "+
		"content — %d cells rewritten, %v",
		keyMoved, falseCol, cd.Stats.CellsMoved, cd.Stats.Elapsed)
	t.Logf("  the row half of the join key is untouched by a column op and the column half " +
		"is a bare index, so a column insert defeats the key join completely")
}

// ─── collide: do two forks mint the same key for independent inserts? ─────────

func fmScenarioCollide(t *testing.T) {
	e := fmSetup(t)
	base := e.base("collide")
	e.fork(base, "coll-ours")
	e.fork(base, "coll-theirs")

	fmInsertRows(t, e, "coll-ours", 3, 1)
	fmInsertRows(t, e, "coll-theirs", 3, 1)
	fmWrite(t, e, "coll-ours", w("A4", "ours-new-row"))
	fmWrite(t, e, "coll-theirs", w("A4", "theirs-new-row"))

	ours := fmSnapshot(t, e, "coll-ours")
	theirs := fmSnapshot(t, e, "coll-theirs")
	ko, kt := ours.keyOf[3], theirs.keyOf[3]
	t.Logf("both forks inserted a row at rank 3: ours minted k=%d, theirs minted k=%d (equal=%v)",
		ko, kt, ko == kt)
	t.Logf("  k=%d holds %q on ours and %q on theirs",
		ko, ours.raw[fmKey{ko, 0}], theirs.raw[fmKey{kt, 0}])

	// And the same at a different rank, to show it is arithmetic and not luck.
	e.fork(base, "coll-o2")
	e.fork(base, "coll-t2")
	fmInsertRows(t, e, "coll-o2", 120, 2)
	fmInsertRows(t, e, "coll-t2", 120, 2)
	o2, t2 := fmSnapshot(t, e, "coll-o2"), fmSnapshot(t, e, "coll-t2")
	t.Logf("InsertRows(120,2) on both: ours k=%v theirs k=%v",
		o2.keyOf[120:122], t2.keyOf[120:122])
}

// ─── log: where does the fork point come from? ────────────────────────────────

func fmScenarioLog(t *testing.T) {
	e := fmSetup(t)
	base := e.base("log")

	// editlog.go's ring is in-memory and lives on the Server, not on the sheet.
	// Demonstrate its shape rather than assume it.
	l := &editLog{}
	for i := 0; i < editLogDepth+50; i++ {
		l.Append(nil, []CellRef{{Row: i, Col: 0}})
	}
	_, _, _, ok := l.Since(1)
	t.Logf("editLog: depth=%d; after %d appends, Since(1) ok=%v (false means the history is gone)",
		editLogDepth, editLogDepth+50, ok)
	_, _, _, ok2 := l.Since(uint64(editLogDepth + 49))
	t.Logf("editLog: Since(%d) ok=%v — only the last %d edits are answerable",
		editLogDepth+49, ok2, editLogDepth)
	t.Logf("editLog is a field on Server (http.go: edits map[string]*editLog), in process memory. " +
		"It holds dirty CellRef sets, no values, and does not survive a restart, " +
		"so it cannot supply a fork point at ANY depth.")

	// The durable log is the events table, and it does carry structural ops.
	seq0, err := func() (int64, error) {
		sh, err := e.cache.Open(base)
		if err != nil {
			return 0, err
		}
		return sh.LastSeq()
	}()
	if err != nil {
		t.Fatal(err)
	}
	fmWrite(t, e, base, w("A1", "1"), w("B2", "2"))
	fmInsertRows(t, e, base, 5, 3)
	fmDeleteRows(t, e, base, 9, 1)
	sh, _ := e.cache.Open(base)
	evs, err := sh.Events(seq0, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("events table after seed(seq=%d) + 2 cell writes + insert + delete: %d rows", seq0, len(evs))
	for _, ev := range evs {
		t.Logf("  seq=%d ref=%q raw=%q prev=%q", ev.Seq, ev.Ref, ev.Raw, ev.Prev)
	}
	t.Logf("durable log holds %d events before trimming (maxEvents), gated every %d writes",
		maxEvents, eventTrimEvery)
}

// ─── a: disjoint value edits on both sides ────────────────────────────────────

func fmScenarioA(t *testing.T) {
	e := fmSetup(t)
	base := e.base("a")
	fork := func(n string) string { return e.fork(base, n) }
	b := fmSnapshot(t, e, base)

	fork("a-ours")
	fork("a-theirs")
	fork("a-merged")
	fork("a-linear")

	ourEdits := []BatchWrite{w("A5", "999"), w("C7", "111")}
	theirEdits := []BatchWrite{w("B7", "888"), w("A100", "222")}

	fmWrite(t, e, "a-ours", ourEdits...)
	fmWrite(t, e, "a-theirs", theirEdits...)
	fmWrite(t, e, "a-linear", ourEdits...)
	fmWrite(t, e, "a-linear", theirEdits...)

	ours, theirs := fmSnapshot(t, e, "a-ours"), fmSnapshot(t, e, "a-theirs")
	take, conflicts := fmMerge(b, ours, theirs)
	res, _ := fmApply(t, e, "a-merged", take)

	merged, linear := fmSnapshot(t, e, "a-merged"), fmSnapshot(t, e, "a-linear")
	diff := fmDisplayDiff(merged, linear)

	t.Logf("4 value edits on two forks: merge took %d raw cells, %d conflicts, "+
		"recalc re-derived %d cells in %v",
		len(take), len(conflicts), res.Stats.Nodes, res.Stats.Elapsed)
	if *fmVerbose {
		for _, k := range fmKeys(take) {
			t.Logf("  took %s", k)
		}
	}
	t.Logf("merged vs linear (ours-then-theirs applied to one sheet): %d differing cells", len(diff))
	if len(diff) != 0 {
		t.Errorf("disjoint value merge is not equal to the linear order: %v", diff[:min(10, len(diff))])
	}
	if len(take) != 4 {
		t.Errorf("expected exactly the 4 edited cells in the merge, got %d: %v", len(take), fmKeys(take))
	}
	// Claim 2: how many DERIVED cells changed, that nobody merged?
	derived := 0
	for k, v := range b.computed {
		if merged.computed[k] != v {
			if _, wasEdited := take[k]; !wasEdited {
				derived++
			}
		}
	}
	t.Logf("cells whose computed value moved but whose raw text nobody merged: %d "+
		"(these are the ones recalc re-derived rather than the merge deciding)", derived)
}

func fmKeys(m map[fmKey]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k.String())
	}
	sort.Strings(out)
	return out
}

// ─── b: the same cell edited on both sides ────────────────────────────────────

func fmScenarioB(t *testing.T) {
	e := fmSetup(t)
	base := e.base("b")
	b := fmSnapshot(t, e, base)
	e.fork(base, "b-ours")
	e.fork(base, "b-theirs")
	e.fork(base, "b-merged")

	fmWrite(t, e, "b-ours", w("A5", "1"), w("D9", "same"))
	fmWrite(t, e, "b-theirs", w("A5", "2"), w("D9", "same"))

	ours, theirs := fmSnapshot(t, e, "b-ours"), fmSnapshot(t, e, "b-theirs")
	take, conflicts := fmMerge(b, ours, theirs)
	t.Logf("A5 edited to 1 on ours and 2 on theirs; D9 edited to the same text on both")
	t.Logf("  conflicts=%d, auto-merged=%d", len(conflicts), len(take))
	for _, c := range conflicts {
		r := b.rowOf[c.key.k]
		t.Logf("  CONFLICT at %s (%s): base=%q ours=%q theirs=%q",
			CellRef{r, c.key.col}, c.key, c.base, c.ours, c.theirs)
	}
	if len(conflicts) != 1 {
		t.Errorf("want exactly 1 conflict (A5), got %d", len(conflicts))
	}
	if len(take) != 1 {
		t.Errorf("want D9 auto-merged as a convergent edit, got %d takes", len(take))
	}
}

// ─── c: a formula on one side whose input changed on the other ────────────────

func fmScenarioC(t *testing.T) {
	e := fmSetup(t)
	base := e.base("c")
	b := fmSnapshot(t, e, base)
	e.fork(base, "c-ours")
	e.fork(base, "c-theirs")
	e.fork(base, "c-merged")
	e.fork(base, "c-linear")

	// Ours writes a new formula in the blank column Z. Theirs changes its input,
	// having never seen the formula.
	ourEdits := []BatchWrite{w("Z10", "=A10*3")}
	theirEdits := []BatchWrite{w("A10", "500")}
	fmWrite(t, e, "c-ours", ourEdits...)
	fmWrite(t, e, "c-theirs", theirEdits...)
	fmWrite(t, e, "c-linear", ourEdits...)
	fmWrite(t, e, "c-linear", theirEdits...)

	ours, theirs := fmSnapshot(t, e, "c-ours"), fmSnapshot(t, e, "c-theirs")
	t.Logf("on ours alone:   Z10 raw=%q computed=%q (A10 is still %q)",
		ours.dispRaw[ref("Z10")], ours.dispComp[ref("Z10")], ours.dispRaw[ref("A10")])
	t.Logf("on theirs alone: A10 raw=%q, Z10 is %q", theirs.dispRaw[ref("A10")], theirs.dispRaw[ref("Z10")])

	take, conflicts := fmMerge(b, ours, theirs)
	res, _ := fmApply(t, e, "c-merged", take)
	merged, linear := fmSnapshot(t, e, "c-merged"), fmSnapshot(t, e, "c-linear")

	t.Logf("merged: %d raw cells taken, %d conflicts; Z10 = %q (computed %q), A10 computed %q",
		len(take), len(conflicts),
		merged.dispRaw[ref("Z10")], merged.dispComp[ref("Z10")], merged.dispComp[ref("A10")])
	t.Logf("recalc re-derived %d cells, depth %d, in %v",
		res.Stats.Nodes, res.Stats.Depth, res.Stats.Elapsed)
	if got, want := merged.dispComp[ref("Z10")], "1500"; got != want {
		t.Errorf("Z10 computed = %q, want %q — the formula did not see the other fork's input", got, want)
	}
	if d := fmDisplayDiff(merged, linear); len(d) != 0 {
		t.Errorf("merged != linear in %d cells: %v", len(d), d[:min(10, len(d))])
	} else {
		t.Logf("merged is cell-for-cell identical to applying both edits in one order")
	}
	// The whole point of claim 2: neither side's COMPUTED value was consulted.
	t.Logf("computed values merged: 0 (fmMerge reads only .raw)")
}

// ─── d: a row insert on one side only ─────────────────────────────────────────

func fmScenarioD(t *testing.T) {
	e := fmSetup(t)
	base := e.base("d")
	fmWrite(t, e, base, w("Z100", "landmark"))
	b := fmSnapshot(t, e, base)
	e.fork(base, "d-ours")
	e.fork(base, "d-theirs")
	e.fork(base, "d-naive")

	fmInsertRows(t, e, "d-ours", 3, 1)
	fmWrite(t, e, "d-ours", w("A4", "inserted"))
	fmWrite(t, e, "d-theirs", w("A100", "777"))

	ours, theirs := fmSnapshot(t, e, "d-ours"), fmSnapshot(t, e, "d-theirs")

	// (1) What does the naive key-join merge do? Nothing about it knows a row
	//     was inserted; it just sees keys whose content changed.
	take, conflicts := fmMerge(b, ours, theirs)
	t.Logf("ours inserted one row at rank 3 and typed A4; theirs typed A100.")
	t.Logf("  naive key-join three-way diff: %d cells to take, %d conflicts "+
		"(the honest answer is 2 cells and 0 conflicts)", len(take), len(conflicts))
	t.Logf("  ours' diff against base alone is %d cells; theirs' is %d",
		len(fmDiff(b, ours)), len(fmDiff(b, theirs)))

	_, orphans := fmApply(t, e, "d-naive", take)
	naive := fmSnapshot(t, e, "d-naive")
	t.Logf("  applying it: %d of those keys named no row on the target and were dropped", orphans)
	t.Logf("  naive merge result: Z100 landmark is at %s (base %s, ours %s)",
		fmFind(naive, 25, "landmark"), fmFind(b, 25, "landmark"), fmFind(ours, 25, "landmark"))

	// (2) The disciplined answer: replay the structural op, then merge values in
	//     the post-replay coordinate space.
	e.fork(base, "d-merged")
	str := fmStructuralSince(t, e, "d-ours", b.lastSeq)
	t.Logf("  structural events on ours since the fork point: %d %v", len(str), fmEventText(str))
	fmInsertRows(t, e, "d-merged", 3, 1)
	replayed := fmSnapshot(t, e, "d-merged")

	// Ours and the replayed base now have the same geometry, so ours' diff is
	// honest. Theirs' diff is still in base coordinates and has to be carried
	// across the insert one entry at a time.
	dOurs := fmDiff(replayed, ours)
	dTheirs, _ := fmTransposeDiff(b, replayed, fmDiff(b, theirs), 3, 1, false)
	t.Logf("  after replaying the insert: ours' value diff is %d cells, "+
		"theirs' transposed value diff is %d cells", len(dOurs), len(dTheirs))
	take2 := map[fmKey]string{}
	for k, v := range dOurs {
		take2[k] = v
	}
	conf2 := 0
	for k, v := range dTheirs {
		if ov, both := take2[k]; both && ov != v {
			conf2++
			continue
		}
		take2[k] = v
	}
	_, orph2 := fmApply(t, e, "d-merged", take2)
	merged := fmSnapshot(t, e, "d-merged")
	t.Logf("  replay-then-merge: %d cells, %d conflicts, %d orphan keys", len(take2), conf2, orph2)

	// The linear reference: ours' insert, then theirs' edit at the row it moved
	// to. A101 in post-insert coordinates is base row 100.
	e.fork(base, "d-linear")
	fmInsertRows(t, e, "d-linear", 3, 1)
	fmWrite(t, e, "d-linear", w("A4", "inserted"), w("A101", "777"))
	linear := fmSnapshot(t, e, "d-linear")
	if d := fmDisplayDiff(merged, linear); len(d) != 0 {
		t.Logf("  replay-then-merge != linear in %d cells: %v", len(d), d[:min(10, len(d))])
	} else {
		t.Logf("  replay-then-merge IS cell-for-cell identical to insert-then-edit")
	}
	nd := fmDisplayDiff(naive, linear)
	t.Logf("  naive key-join merge differs from linear in %d cells", len(nd))
	if len(nd) > 0 {
		for _, s := range nd[:min(6, len(nd))] {
			t.Logf("    %s", s)
		}
	}
}

// fmTransposeDiff carries one side's value diff across a row insert the other
// side made: base rank r becomes r+n at or below `at`, and the entry is re-keyed
// with the target's key for that rank. This is the operational transform, done
// by hand, and it is the work the "join on the storage key" was supposed to make
// unnecessary.
//
// It transposes the DIFF and not the snapshot on purpose. Transposing a whole
// snapshot and diffing it against the target reports every formula in the moved
// range as changed, because Cell.Raw is materialized in display coordinates: a
// row that moved from rank 99 to rank 100 still says "=A100*2" in the side's raw
// text while the target says "=A101*2". Measured at 508 spurious cells on this
// fixture.
func fmTransposeDiff(base, target *fmSnap, d map[fmKey]string, at, n int, del bool) (map[fmKey]string, []CellRef) {
	out := make(map[fmKey]string, len(d))
	var stranded []CellRef
	for k, v := range d {
		r, ok := base.rowOf[k.k]
		if !ok {
			continue
		}
		nr, alive := fmTransformRank(r, at, n, del)
		if !alive {
			stranded = append(stranded, CellRef{r, k.col})
			continue
		}
		if nr >= len(target.keyOf) {
			continue
		}
		out[fmKey{target.keyOf[nr], k.col}] = v
	}
	sort.Slice(stranded, func(i, j int) bool {
		if stranded[i].Row != stranded[j].Row {
			return stranded[i].Row < stranded[j].Row
		}
		return stranded[i].Col < stranded[j].Col
	})
	return out, stranded
}

// fmTransformRank maps a base display rank through one structural op. `alive` is
// false when the row the rank named no longer exists, which is the one case a
// transform cannot answer and a person has to.
func fmTransformRank(r, at, n int, del bool) (int, bool) {
	if !del {
		if r >= at {
			return r + n, true
		}
		return r, true
	}
	switch {
	case r < at:
		return r, true
	case r < at+n:
		return 0, false
	default:
		return r - n, true
	}
}

// fmFind is the display address of the one cell in `col` holding `text`, so a
// landmark can be followed through a mutation deterministically.
func fmFind(s *fmSnap, col int, text string) string {
	for r := 0; r < s.used; r++ {
		if s.dispRaw[CellRef{r, col}] == text {
			return CellRef{r, col}.String()
		}
	}
	return "(not found)"
}

func fmEventText(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Ref+" "+ev.Raw)
	}
	return out
}

// ─── e: row inserts on BOTH sides — the case that should break ────────────────

func fmScenarioE(t *testing.T) {
	e := fmSetup(t)
	base := e.base("e")
	// A landmark row below both insertion points, and a formula in row 1 that
	// reads it. If claim 3 is right — references are display coordinates — the
	// formula should end up pointing somewhere else.
	fmWrite(t, e, base, w("Z1", "=A20*1"), w("A20", "777777"))
	b := fmSnapshot(t, e, base)
	t.Logf("base: Z1 = %q -> %q; the landmark 777777 is at %s",
		b.dispRaw[ref("Z1")], b.dispComp[ref("Z1")], fmFind(b, 0, "777777"))

	e.fork(base, "e-ours")
	e.fork(base, "e-theirs")
	fmInsertRows(t, e, "e-ours", 3, 1)
	fmWrite(t, e, "e-ours", w("A4", "ours"))
	fmInsertRows(t, e, "e-theirs", 7, 1)
	fmWrite(t, e, "e-theirs", w("A8", "theirs"))

	ours, theirs := fmSnapshot(t, e, "e-ours"), fmSnapshot(t, e, "e-theirs")
	t.Logf("ours:   insert at rank 3, A4=ours;   Z1 = %q -> %q, landmark at %s",
		ours.dispRaw[ref("Z1")], ours.dispComp[ref("Z1")], fmFind(ours, 0, "777777"))
	t.Logf("theirs: insert at rank 7, A8=theirs; Z1 = %q -> %q, landmark at %s",
		theirs.dispRaw[ref("Z1")], theirs.dispComp[ref("Z1")], fmFind(theirs, 0, "777777"))

	// The detector the rule proposes.
	so := fmStructuralSince(t, e, "e-ours", b.lastSeq)
	st := fmStructuralSince(t, e, "e-theirs", b.lastSeq)
	t.Logf("structural events since the fork point — ours %v, theirs %v", fmEventText(so), fmEventText(st))
	if len(so) > 0 && len(st) > 0 {
		t.Logf("RULE FIRES: both sides mutated structure; refuse the auto-merge")
	}

	// Now measure what refusing buys, by not refusing. Two replay orders, each
	// with the operations' arguments left exactly as the fork recorded them.
	var untransformed []*fmSnap
	for _, order := range []struct {
		name        string
		first, then [2]int
		firstVal    [2]string
		thenVal     [2]string
	}{
		{"ours-then-theirs", [2]int{3, 1}, [2]int{7, 1}, [2]string{"A4", "ours"}, [2]string{"A8", "theirs"}},
		{"theirs-then-ours", [2]int{7, 1}, [2]int{3, 1}, [2]string{"A8", "theirs"}, [2]string{"A4", "ours"}},
	} {
		id := "e-" + order.name
		e.fork(base, id)
		fmInsertRows(t, e, id, order.first[0], order.first[1])
		fmWrite(t, e, id, w(order.firstVal[0], order.firstVal[1]))
		fmInsertRows(t, e, id, order.then[0], order.then[1])
		fmWrite(t, e, id, w(order.thenVal[0], order.thenVal[1]))
		s := fmSnapshot(t, e, id)
		untransformed = append(untransformed, s)
		t.Logf("untransformed replay, %s: \"ours\" at %s, \"theirs\" at %s, blank rows at %v; "+
			"Z1 = %q -> %q, landmark at %s",
			order.name, fmFind(s, 0, "ours"), fmFind(s, 0, "theirs"), fmBlankRows(s, 0, 12),
			s.dispRaw[ref("Z1")], s.dispComp[ref("Z1")], fmFind(s, 0, "777777"))
	}
	if d := fmDisplayDiff(untransformed[0], untransformed[1]); len(d) == 0 {
		t.Logf("the two replay orders AGREE — the operations commuted")
	} else {
		t.Logf("the two replay orders DISAGREE in %d cells — the operations do not commute:", len(d))
		for _, s := range d[:min(6, len(d))] {
			t.Logf("    %s", s)
		}
	}

	// And the transformed replay: theirs' rank 7 becomes rank 8 because ours
	// inserted a row above it. This is the ordinary operational transform, done
	// by hand, and it is entirely about the OPERATION'S ARGUMENTS.
	e.fork(base, "e-xform")
	fmInsertRows(t, e, "e-xform", 3, 1)
	fmWrite(t, e, "e-xform", w("A4", "ours"))
	fmInsertRows(t, e, "e-xform", 8, 1)
	fmWrite(t, e, "e-xform", w("A9", "theirs"))
	x := fmSnapshot(t, e, "e-xform")
	t.Logf("transformed replay: \"ours\" at %s, \"theirs\" at %s, blank rows at %v; "+
		"Z1 = %q -> %q, landmark at %s",
		fmFind(x, 0, "ours"), fmFind(x, 0, "theirs"), fmBlankRows(x, 0, 12),
		x.dispRaw[ref("Z1")], x.dispComp[ref("Z1")], fmFind(x, 0, "777777"))
	if x.dispComp[ref("Z1")] != b.dispComp[ref("Z1")] {
		t.Errorf("the formula lost its referent across the merge: %q -> %q",
			b.dispComp[ref("Z1")], x.dispComp[ref("Z1")])
	}
	t.Logf("CLAIM 3's MECHANISM: Z1 still evaluates to %q in every replay above, "+
		"transformed or not. The formula reference is stored as a KEY (cells.ref0_k) "+
		"and shiftKeyRange moves it with its row, so it never repointed.",
		x.dispComp[ref("Z1")])
	for i, name := range []string{"ours-then-theirs", "theirs-then-ours"} {
		d := fmDisplayDiff(untransformed[i], x)
		t.Logf("untransformed %s vs the transformed replay: %d differing cells%s",
			name, len(d), map[bool]string{true: "  <- accidentally correct", false: ""}[len(d) == 0])
	}

	// Does the key join even survive? Compare ours' and theirs' key spaces.
	sameKeyDifferentText := 0
	for k, v := range ours.raw {
		if tv, ok := theirs.raw[k]; ok && tv != v {
			sameKeyDifferentText++
		}
	}
	t.Logf("cells where ours and theirs hold DIFFERENT text at the SAME storage key: %d "+
		"(2 cells were actually edited)", sameKeyDifferentText)
}

// fmBlankRows lists the display rows in `col` that hold nothing, up to `upTo` —
// the inserted rows, which is where a mis-replayed structural op shows itself.
func fmBlankRows(s *fmSnap, col, upTo int) []int {
	var out []int
	for r := 0; r < min(upTo, s.used); r++ {
		if s.dispRaw[CellRef{r, col}] == "" {
			out = append(out, r+1)
		}
	}
	return out
}

// ─── f: delete on one side of a row edited on the other ───────────────────────

func fmScenarioF(t *testing.T) {
	e := fmSetup(t)
	base := e.base("f")
	b := fmSnapshot(t, e, base)
	e.fork(base, "f-ours")
	e.fork(base, "f-theirs")

	fmDeleteRows(t, e, "f-ours", 5, 1)
	fmWrite(t, e, "f-theirs", w("A6", "42"), w("B6", "43"))

	ours, theirs := fmSnapshot(t, e, "f-ours"), fmSnapshot(t, e, "f-theirs")
	delKey := b.keyOf[5]
	t.Logf("ours deleted display row 6 (rank 5, k=%d); theirs edited A6 and B6 on that row", delKey)
	t.Logf("  ours now holds %q at k=%d col A (base held %q)",
		ours.raw[fmKey{delKey, 0}], delKey, b.raw[fmKey{delKey, 0}])

	take, conflicts := fmMerge(b, ours, theirs)
	t.Logf("  key-join merge: %d takes, %d conflicts", len(take), len(conflicts))
	if _, taken := take[fmKey{delKey, 0}]; taken {
		t.Logf("  the deleted row's edit was silently taken at k=%d and the delete was not seen at all",
			delKey)
	}
	so := fmStructuralSince(t, e, "f-ours", b.lastSeq)
	t.Logf("  structural events on ours: %v — this is the only signal that a row is gone",
		fmEventText(so))
	t.Logf("  ours extent=%d theirs extent=%d (a delete refills the tail, so the extent "+
		"does not report it either)", ours.rows, theirs.rows)

	// The disciplined path: replay the delete, then transform theirs' diff. The
	// transform is total for an insert and PARTIAL for a delete — the rows that
	// are gone have no destination rank, and that is not a defect of the
	// transform, it is the ambiguity itself.
	e.fork(base, "f-merged")
	fmDeleteRows(t, e, "f-merged", 5, 1)
	replayed := fmSnapshot(t, e, "f-merged")
	dOurs := fmDiff(replayed, ours)
	dTheirs, stranded := fmTransposeDiff(b, replayed, fmDiff(b, theirs), 5, 1, true)
	t.Logf("  replay-then-transform: ours' value diff %d cells, theirs' transposed %d cells, "+
		"%d cells STRANDED on the deleted row: %v",
		len(dOurs), len(dTheirs), len(stranded), stranded)
	t.Logf("  there is no linearization to check against: \"delete the row\" and \"put 42 in " +
		"the row\" have no order that satisfies both, so this case is a question for a " +
		"person and not a merge rule")
}
