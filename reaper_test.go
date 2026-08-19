package main

// reaper_test.go — retention.
//
// TIME IS SET ON THE FILES, NEVER SLEPT. A sheet's age is its `.db` mtime, so
// aging one is os.Chtimes and the whole suite runs in milliseconds. The two
// facts that are NOT about arithmetic — that a read does not age a sheet, and
// that a reaped URL still serves a working sheet — are exercised against the
// real store and the real HTTP routes respectively, because both are claims
// about behaviour the arithmetic cannot see.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── Helpers ──────────────────────────────────────────────────────────────────

// fakeViewers answers the one question the reaper asks the registry.
type fakeViewers map[string]bool

func (f fakeViewers) sheetsWithViewers(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = f[id]
	}
	return out
}

// makeSheet creates a sheet and backdates it by age.
func makeSheet(t *testing.T, c *SheetCache, id string, age time.Duration) {
	t.Helper()
	if err := c.Create(id); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	ageSheet(t, c, id, age)
}

// ageSheet backdates the database AND its WAL. Both, because the reaper reads
// both: a sheet abandoned a month ago has an old `-wal` too (or none at all,
// once the LRU has closed it and checkpointed), and moving only the `.db` would
// model a sheet that is still being written — which is a different test, and
// there is one of those below.
func ageSheet(t *testing.T, c *SheetCache, id string, age time.Duration) {
	t.Helper()
	when := time.Now().Add(-age)
	base := filepath.Join(c.dir, id+".db")
	if err := os.Chtimes(base, when, when); err != nil {
		t.Fatalf("age %s: %v", id, err)
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(base + side); err == nil {
			if err := os.Chtimes(base+side, when, when); err != nil {
				t.Fatalf("age %s%s: %v", id, side, err)
			}
		}
	}
}

func sheetIDs(t *testing.T, c *SheetCache) []string {
	t.Helper()
	list, err := c.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.ID
	}
	return out
}

func testReaper(t *testing.T, c *SheetCache, p ReaperPolicy, v viewerSource, q *sheetQuota) *Reaper {
	t.Helper()
	if p.TTL == 0 {
		p.TTL = 24 * time.Hour
	}
	p.Enabled = p.Enabled || p.TTL > 0
	return NewReaper(p, v, q).useCache(c)
}

// ─── Age selection ────────────────────────────────────────────────────────────

// TestReaperReapsOnlyWhatIsOverTheTTL is the arithmetic, and it is the whole
// policy: a sheet one minute inside the window survives and a sheet one minute
// past it does not.
func TestReaperReapsOnlyWhatIsOverTheTTL(t *testing.T) {
	const ttl = 24 * time.Hour
	cases := []struct {
		id   string
		age  time.Duration
		reap bool
	}{
		{"fresh", time.Minute, false},
		{"yesterdayish", ttl - time.Minute, false},
		{"exactly", ttl, true},
		{"stale", ttl + time.Minute, true},
		{"ancient", 90 * 24 * time.Hour, true},
	}

	c := newTestCache(t, 8, time.Minute)
	for _, tc := range cases {
		makeSheet(t, c, tc.id, tc.age)
	}

	r := testReaper(t, c, ReaperPolicy{TTL: ttl}, fakeViewers{}, nil)
	res := r.Sweep()

	want := 0
	for _, tc := range cases {
		if tc.reap {
			want++
		}
	}
	if res.Reaped != want {
		t.Fatalf("sweep reaped %d, want %d (%+v)", res.Reaped, want, res)
	}
	if res.Scanned != len(cases) {
		t.Errorf("scanned %d, want %d", res.Scanned, len(cases))
	}
	if res.Bytes <= 0 {
		t.Errorf("freed %d bytes; a reap of %d sheets must account for the space", res.Bytes, want)
	}

	left := map[string]bool{}
	for _, id := range sheetIDs(t, c) {
		left[id] = true
	}
	for _, tc := range cases {
		if tc.reap && left[tc.id] {
			t.Errorf("%s (age %v) survived a %v TTL", tc.id, tc.age, ttl)
		}
		if !tc.reap && !left[tc.id] {
			t.Errorf("%s (age %v) was reaped under a %v TTL", tc.id, tc.age, ttl)
		}
	}
}

func TestReaperIsOffWithoutATTL(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	makeSheet(t, c, "ancient", 365*24*time.Hour)

	r := NewReaper(ReaperPolicy{Enabled: true, TTL: 0}, fakeViewers{}, nil).useCache(c)
	if res := r.Sweep(); res.Reaped != 0 || res.Scanned != 0 {
		t.Fatalf("a reaper with no TTL did something: %+v", res)
	}
	if !strings.Contains(r.Describe(), "OFF") {
		t.Errorf("Describe = %q, want it to say retention is off", r.Describe())
	}
	if len(sheetIDs(t, c)) != 1 {
		t.Fatal("the sheet is gone")
	}
}

func TestReaperNeverTouchesAKeptSheet(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	makeSheet(t, c, "demo", 400*24*time.Hour)
	makeSheet(t, c, "someone", 400*24*time.Hour)

	r := testReaper(t, c, ReaperPolicy{TTL: time.Hour, Keep: map[string]bool{"demo": true}}, fakeViewers{}, nil)
	res := r.Sweep()
	if res.Reaped != 1 || res.SkippedKept != 1 {
		t.Fatalf("sweep = %+v, want 1 reaped and 1 kept", res)
	}
	if got := sheetIDs(t, c); len(got) != 1 || got[0] != "demo" {
		t.Fatalf("sheets left = %v, want only demo", got)
	}
}

// ─── Live connections ─────────────────────────────────────────────────────────

// TestReaperSkipsSheetsWithLiveConnections is the rule that stops the confusing
// bug report: an ancient sheet somebody is CURRENTLY LOOKING AT is not deleted
// out from under them, however idle it is.
func TestReaperSkipsSheetsWithLiveConnections(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	for _, id := range []string{"watched", "abandoned"} {
		makeSheet(t, c, id, 30*24*time.Hour)
	}

	r := testReaper(t, c, ReaperPolicy{TTL: time.Hour}, fakeViewers{"watched": true}, nil)
	res := r.Sweep()
	if res.Reaped != 1 || res.SkippedLive != 1 {
		t.Fatalf("sweep = %+v, want 1 reaped and 1 skipped for a live viewer", res)
	}
	if got := sheetIDs(t, c); len(got) != 1 || got[0] != "watched" {
		t.Fatalf("sheets left = %v, want only the watched one", got)
	}
	if _, err := os.Stat(filepath.Join(c.dir, "watched.gone")); err == nil {
		t.Error("a sheet that was skipped for a live viewer got a tombstone")
	}

	// The viewer leaves; the next sweep takes it.
	r2 := testReaper(t, c, ReaperPolicy{TTL: time.Hour}, fakeViewers{}, nil)
	if res := r2.Sweep(); res.Reaped != 1 {
		t.Fatalf("second sweep = %+v, want the sheet reaped once nobody is on it", res)
	}
}

// TestReaperWithNoRegistryReapsNothing — refusing to delete is the correct
// failure when the thing that answers "is anybody looking at this" is missing.
func TestReaperWithNoRegistryReapsNothing(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	makeSheet(t, c, "old", 30*24*time.Hour)

	r := testReaper(t, c, ReaperPolicy{TTL: time.Hour}, nil, nil)
	if res := r.Sweep(); res.Reaped != 0 || res.SkippedLive != 1 {
		t.Fatalf("sweep = %+v, want nothing reaped without a registry", res)
	}
	if len(sheetIDs(t, c)) != 1 {
		t.Fatal("the sheet was reaped with no way to know whether anybody was on it")
	}
}

// TestRegistrySatisfiesTheReaper pins that the real Registry is the thing the
// reaper is talking to — a signature change there must not silently fall back
// to a stub.
func TestRegistrySatisfiesTheReaper(t *testing.T) {
	var _ viewerSource = (*Registry)(nil)
}

// ─── The mtime invariant ──────────────────────────────────────────────────────

// TestReadingASheetDoesNotAgeIt is the load-bearing assumption of the whole
// file: idleness is LAST WRITE, so opening a sheet, reading a window out of it
// and letting the LRU close it must leave the mtime exactly where the last
// write's checkpoint put it. Under WAL that holds because a read never writes
// to the `.db` and a close with an empty WAL checkpoints nothing.
func TestReadingASheetDoesNotAgeIt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.db")

	write := NewSheetCache(dir, 4, time.Minute)
	sh, err := write.Open("s")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := ParseRef("A1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sh.WriteCell(ref, "42",
		[]ComputedCell{{Ref: ref, Computed: "42", Kind: KindNumber}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Closing the cache checkpoints the WAL into the database, which is the
	// last thing that legitimately moves the mtime.
	_ = write.Close()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	afterWrite := fi.ModTime()

	// Two full read cycles: open (which creates a WAL), read a window, and
	// close (which checkpoints and removes it again).
	for i := range 2 {
		read := NewSheetCache(dir, 4, time.Minute)
		rsh, err := read.Open("s")
		if err != nil {
			t.Fatal(err)
		}
		cells, err := rsh.Window(0, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(cells) == 0 {
			t.Fatal("read nothing back")
		}
		_ = read.Close()

		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(afterWrite) {
			t.Fatalf("read cycle %d moved the mtime %v -> %v; idleness would then mean LAST READ, "+
				"and a sheet nobody edits would never be reaped",
				i+1, afterWrite, fi.ModTime())
		}
	}

	// And a write does move it, or the reaper would never fire at all.
	again := NewSheetCache(dir, 4, time.Minute)
	wsh, err := again.Open("s")
	if err != nil {
		t.Fatal(err)
	}
	ref2, _ := ParseRef("B2")
	if err := wsh.WriteCell(ref2, "7",
		[]ComputedCell{{Ref: ref2, Computed: "7", Kind: KindNumber}}); err != nil {
		t.Fatal(err)
	}
	_ = again.Close()
	fi, err = os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().After(afterWrite) {
		t.Fatalf("a write left the mtime at %v; nothing would ever age", fi.ModTime())
	}
}

// ─── Deletion safety against the cache ────────────────────────────────────────

// TestDeleteEvictsTheHandleAndTheNextOpenIsFresh is the LRU half of the
// problem. The reaper deletes files that the cache may be holding open; a
// retained handle must stop working rather than read a deleted file, and the
// next Open must build a new database rather than hand back the corpse.
func TestDeleteEvictsTheHandleAndTheNextOpenIsFresh(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	sh := mustOpen(t, c, "s")
	mustSet(t, sh, "A1", "keep me")
	wantCell(t, sh, "A1", "keep me", "keep me")

	if !c.IsOpen("s") {
		t.Fatal("the sheet is not in the LRU; the test is not testing what it says")
	}

	if err := c.Delete("s"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if c.IsOpen("s") {
		t.Error("the LRU still holds a handle to a deleted sheet")
	}
	// The retained pointer is closed, not merely orphaned.
	if _, err := sh.Window(0, 10); !errors.Is(err, ErrSheetClosed) {
		t.Errorf("a retained handle to a deleted sheet returned %v, want ErrSheetClosed", err)
	}
	for _, suffix := range []string{".db", ".db-wal", ".db-shm"} {
		if _, err := os.Stat(filepath.Join(c.dir, "s"+suffix)); err == nil {
			t.Errorf("%s survived the delete", suffix)
		}
	}

	// Opening again is a NEW, EMPTY sheet — not a resurrection of the old one.
	fresh := mustOpen(t, c, "s")
	if fresh == sh {
		t.Error("Open handed back the closed handle")
	}
	if n := countRows(t, fresh, "cells"); n != 0 {
		t.Errorf("the reopened sheet has %d cells, want 0 — the old data came back", n)
	}
	wantCell(t, fresh, "A1", "", "")
	// And it is a WORKING sheet, not just a file.
	mustSet(t, fresh, "A1", "=2+2")
	wantCell(t, fresh, "A1", "=2+2", "4")
}

func TestDeleteOfAnUnopenedSheetIsFine(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	makeSheet(t, c, "s", time.Hour)
	// Force it out of the LRU so the delete path takes the not-held branch.
	c2 := NewSheetCache(c.dir, 8, time.Minute)
	t.Cleanup(func() { _ = c2.Close() })
	if err := c2.Delete("s"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !isTombstoned(c2.dir, "s") {
		t.Error("no tombstone")
	}
}

func TestDeleteRejectsABadID(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	if err := c.Delete("../../etc/passwd"); !errors.Is(err, ErrBadSheetID) {
		t.Fatalf("Delete of a traversal id = %v, want ErrBadSheetID", err)
	}
}

// ─── The URL survives ─────────────────────────────────────────────────────────

func TestReapedSheetStillExistsAsAURL(t *testing.T) {
	dir := t.TempDir()
	ConfigureStore(dir, 8, time.Minute)
	t.Cleanup(func() { _ = CloseStore() })

	if err := CreateSheet("keeper"); err != nil {
		t.Fatal(err)
	}
	if !SheetExists("keeper") {
		t.Fatal("a created sheet does not exist")
	}
	if err := DeleteSheet("keeper"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keeper.db")); err == nil {
		t.Fatal("the database is still there")
	}
	if !SheetExists("keeper") {
		t.Fatal("a reaped id stopped existing — its URL would 404 at somebody's bookmark")
	}
	// An id that never existed is still unknown: the tombstone is not a licence
	// to conjure sheets out of any well-formed string.
	if SheetExists("neverwasasheet") {
		t.Fatal("an id that was never created reports as existing")
	}

	// Opening it revives it, and the tombstone goes.
	sh, err := OpenSheet("keeper")
	if err != nil {
		t.Fatalf("open a reaped sheet: %v", err)
	}
	if n := countRows(t, sh, "cells"); n != 0 {
		t.Errorf("the revived sheet has %d cells, want 0", n)
	}
	if isTombstoned(dir, "keeper") {
		t.Error("the tombstone survived the resurrection; the sheet is live again and must count as one")
	}
	if list, _ := ListSheets(); len(list) != 1 || list[0].ID != "keeper" {
		t.Errorf("ListSheets = %+v, want the revived sheet", list)
	}
}

// TestReapedURLServesAWorkingEmptySheet drives the REAL route. The unit tests
// above prove the store does the right thing; this proves `GET /s/{id}` does,
// because that is where the 404 would have been.
func TestReapedURLServesAWorkingEmptySheet(t *testing.T) {
	dir := t.TempDir()
	ConfigureStore(dir, 8, time.Minute)
	t.Cleanup(func() { _ = CloseStore() })

	bus, err := StartBus(BusOptions{})
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(bus.Close)
	reg := NewRegistry(bus, 0)
	t.Cleanup(reg.Close)
	srv := NewServer(ServerOptions{Registry: reg, Bus: bus, Recalc: Recalc, BufferBands: 4})
	h := srv.Routes()

	if err := CreateSheet("abcdefghijkmno"); err != nil {
		t.Fatal(err)
	}
	sh := mustOpenGlobal(t, "abcdefghijkmno")
	mustSet(t, sh, "A1", "before the sweep")

	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "1.2.3.4:5000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := get("/s/abcdefghijkmno")
	if w.Code != http.StatusOK {
		t.Fatalf("the live sheet answered %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "before the sweep") {
		t.Fatal("the live sheet did not render its own content")
	}

	if err := DeleteSheet("abcdefghijkmno"); err != nil {
		t.Fatal(err)
	}

	w = get("/s/abcdefghijkmno")
	if w.Code != http.StatusOK {
		t.Fatalf("a reaped sheet's URL answered %d, want 200 — the promise is that the link keeps working",
			w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "before the sweep") {
		t.Fatal("the reaped sheet still has its old content")
	}
	if strings.Contains(body, "no such sheet") {
		t.Fatal("the page is an error, not a sheet")
	}
	// It is a real, usable grid: the shell, the scroll container and the SSE
	// endpoint are all there.
	for _, want := range []string{`id="vp"`, `/s/abcdefghijkmno/live`, `id="g"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the revived page is missing %q; it is not a working sheet", want)
		}
	}
	// And a second load is the ordinary path, not another resurrection.
	if w := get("/s/abcdefghijkmno"); w.Code != http.StatusOK {
		t.Fatalf("the second load answered %d", w.Code)
	}
	if isTombstoned(dir, "abcdefghijkmno") {
		t.Error("the tombstone is still there after the sheet came back")
	}
}

func mustOpenGlobal(t *testing.T, id string) *Sheet {
	t.Helper()
	sh, err := OpenSheet(id)
	if err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
	return sh
}

// ─── Tombstones ───────────────────────────────────────────────────────────────

func TestTombstoneIsNotASheet(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	makeSheet(t, c, "gone", 30*24*time.Hour)
	makeSheet(t, c, "here", time.Minute)

	r := testReaper(t, c, ReaperPolicy{TTL: time.Hour}, fakeViewers{}, nil)
	if res := r.Sweep(); res.Reaped != 1 {
		t.Fatalf("sweep = %+v", res)
	}

	// It does not appear in the listing...
	if got := sheetIDs(t, c); len(got) != 1 || got[0] != "here" {
		t.Fatalf("List = %v, want only the live sheet", got)
	}
	// ...and it does not count against the global sheet cap or anybody's quota.
	count, _, err := sheetDirUsage(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("sheetDirUsage counts %d sheets, want 1 — a tombstone is occupying a slot", count)
	}
	marks, err := c.Tombstones()
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 1 || marks[0].ID != "gone" {
		t.Fatalf("Tombstones = %+v, want one for `gone`", marks)
	}
}

func TestTombstonesExpireOnlyWhenAsked(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	makeSheet(t, c, "old", 30*24*time.Hour)
	if err := c.Delete("old"); err != nil {
		t.Fatal(err)
	}
	// Backdate the marker.
	when := time.Now().Add(-100 * 24 * time.Hour)
	if err := os.Chtimes(tombPath(c.dir, "old"), when, when); err != nil {
		t.Fatal(err)
	}

	// Default policy keeps tombstones forever, so the URL keeps working.
	keep := testReaper(t, c, ReaperPolicy{TTL: time.Hour}, fakeViewers{}, nil)
	if res := keep.Sweep(); res.Tombstones != 0 {
		t.Fatalf("tombstones expired with TombstoneTTL unset: %+v", res)
	}
	if !isTombstoned(c.dir, "old") {
		t.Fatal("the marker went without being asked to")
	}

	expire := testReaper(t, c, ReaperPolicy{TTL: time.Hour, TombstoneTTL: 30 * 24 * time.Hour}, fakeViewers{}, nil)
	if res := expire.Sweep(); res.Tombstones != 1 {
		t.Fatalf("sweep = %+v, want 1 tombstone expired", res)
	}
	if isTombstoned(c.dir, "old") {
		t.Fatal("the marker survived its own TTL")
	}
}

// ─── Quota is returned ────────────────────────────────────────────────────────

// TestReapingReturnsQuota is the reason the per-IP cap counts LIVE sheets. A
// visitor who made their limit a month ago and never came back must not be
// locked out forever.
func TestReapingReturnsQuota(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	q := newSheetQuota()

	for _, id := range []string{"one", "two", "three"} {
		makeSheet(t, c, id, 30*24*time.Hour)
		q.add("9.9.9.9", id)
	}
	makeSheet(t, c, "recent", time.Minute)
	q.add("9.9.9.9", "recent")

	if got := q.held("9.9.9.9"); got != 4 {
		t.Fatalf("ledger says %d, want 4", got)
	}

	r := testReaper(t, c, ReaperPolicy{TTL: time.Hour}, fakeViewers{}, q)
	if res := r.Sweep(); res.Reaped != 3 {
		t.Fatalf("sweep = %+v, want 3 reaped", res)
	}
	if got := q.held("9.9.9.9"); got != 1 {
		t.Fatalf("after reaping 3 of 4 sheets the ledger says %d, want 1", got)
	}

	// A sheet the reaper skipped for a live viewer keeps its slot.
	ageSheet(t, c, "recent", 30*24*time.Hour)
	live := testReaper(t, c, ReaperPolicy{TTL: time.Hour}, fakeViewers{"recent": true}, q)
	if res := live.Sweep(); res.Reaped != 0 || res.SkippedLive != 1 {
		t.Fatalf("sweep = %+v", res)
	}
	if got := q.held("9.9.9.9"); got != 1 {
		t.Fatalf("a skipped sheet released its quota anyway: held = %d", got)
	}
}

// ─── Lifecycle ────────────────────────────────────────────────────────────────

func TestReaperStartSweepsImmediatelyAndStops(t *testing.T) {
	c := newTestCache(t, 8, time.Minute)
	makeSheet(t, c, "old", 30*24*time.Hour)

	r := testReaper(t, c, ReaperPolicy{TTL: time.Hour, Every: time.Hour}, fakeViewers{}, nil)
	r.Start()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := r.Stats(); n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.Close()

	if n, bytes := r.Stats(); n != 1 || bytes == 0 {
		t.Fatalf("Stats = (%d, %d), want one sheet and a non-zero size", n, bytes)
	}
	if len(sheetIDs(t, c)) != 0 {
		t.Fatal("the sheet survived the boot sweep")
	}
	r.Close() // idempotent
}

func TestReaperDescribeStatesThePolicy(t *testing.T) {
	r := NewReaper(ReaperPolicy{Enabled: true, TTL: DefaultSheetTTL, Every: time.Hour,
		Keep: map[string]bool{"demo": true}}, fakeViewers{}, nil)
	got := r.Describe()
	for _, want := range []string{"14 days", "1 hour", "URLs keep working", "demo"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe = %q, missing %q", got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{14 * 24 * time.Hour, "14 days"},
		{7 * 24 * time.Hour, "7 days"},
		{24 * time.Hour, "1 day"},
		{6 * time.Hour, "6 hours"},
		{time.Hour, "1 hour"},
		{5 * time.Minute, "5 minutes"},
		{30 * time.Second, "30s"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// ─── The `.db` mtime is the last CHECKPOINT, not the last write ───────────────

// TestASheetWithUncheckpointedWritesIsNotReaped. Under WAL a write lands in the
// `-wal` sidecar and the database file's mtime does not move until a
// checkpoint, so a sheet that is being written continuously — and therefore
// never goes idle long enough for the LRU to close it — can carry an mtime from
// days ago. Reaping on that alone would delete a sheet somebody is using.
func TestASheetWithUncheckpointedWritesIsNotReaped(t *testing.T) {
	c := newTestCache(t, 8, time.Hour)
	sh := mustOpen(t, c, "busy")
	mustSet(t, sh, "A1", "still typing")
	// The handle stays open, so nothing has been checkpointed: backdate ONLY the
	// database file, exactly as a long-lived handle would leave it, and leave
	// the WAL stamped now.
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(c.dir, "busy.db"), old, old); err != nil {
		t.Fatal(err)
	}

	list, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || time.Since(list[0].ModTime) < 29*24*time.Hour {
		t.Fatalf("the setup did not produce a stale mtime: %+v", list)
	}

	r := testReaper(t, c, ReaperPolicy{TTL: time.Hour}, fakeViewers{}, nil)
	res := r.Sweep()
	if res.Reaped != 0 || res.SkippedWriting != 1 {
		t.Fatalf("sweep = %+v, want the sheet kept for its pending writes", res)
	}
	wantCell(t, sh, "A1", "still typing", "still typing")

	// Once the writes are checkpointed and the file really is old, it goes.
	if err := c.Close(); err != nil { // checkpoints and removes the WAL
		t.Fatal(err)
	}
	c2 := NewSheetCache(c.dir, 8, time.Hour)
	t.Cleanup(func() { _ = c2.Close() })
	ageSheet(t, c2, "busy", 30*24*time.Hour)
	r2 := testReaper(t, c2, ReaperPolicy{TTL: time.Hour}, fakeViewers{}, nil)
	if res := r2.Sweep(); res.Reaped != 1 {
		t.Fatalf("sweep = %+v, want the checkpointed sheet reaped", res)
	}
}

// TestReadingDoesNotCreateAWriteMarker is the other half: a read-only session
// leaves a ZERO-BYTE WAL stamped with the moment it opened, and trusting that
// mtime would turn "idle" into "unread" — the exact inversion this policy is
// built to avoid.
func TestReadingDoesNotCreateAWriteMarker(t *testing.T) {
	dir := t.TempDir()

	w := NewSheetCache(dir, 4, time.Hour)
	sh := mustOpen(t, w, "s")
	mustSet(t, sh, "A1", "written once")
	_ = w.Close() // checkpoint, remove the WAL

	when := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "s.db"), when, when); err != nil {
		t.Fatal(err)
	}

	// A reader opens it, which creates an empty WAL with a mtime of NOW.
	rc := NewSheetCache(dir, 4, time.Hour)
	t.Cleanup(func() { _ = rc.Close() })
	rsh := mustOpen(t, rc, "s")
	if _, err := rsh.Window(0, 50); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "s.db-wal"))
	if err != nil {
		t.Skip("no WAL sidecar on this platform; nothing to disprove")
	}
	if fi.Size() != 0 {
		t.Fatalf("a read left %d bytes in the WAL; the size test is not a valid discriminator", fi.Size())
	}

	got := sheetLastWrite(dir, "s", when)
	if !got.Equal(when) {
		t.Fatalf("sheetLastWrite = %v after a read-only open, want the write's own time %v — "+
			"reading a sheet would keep it alive forever", got, when)
	}

	r := testReaper(t, rc, ReaperPolicy{TTL: time.Hour}, fakeViewers{}, nil)
	if res := r.Sweep(); res.Reaped != 1 {
		t.Fatalf("sweep = %+v, want the read-but-unwritten sheet reaped", res)
	}
}
