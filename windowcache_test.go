package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestRenderWindowPartsRejoinExactly is the guard the shared cache rests on.
//
// renderWindowParts exists so one render can serve many viewers, and it is only
// safe while rejoining its halves around a selection is byte-identical to
// rendering the window whole. If the two ever drift, the cache serves subtly
// wrong grids to everyone except the first viewer — which is the hardest possible
// bug to see, because the page still looks like a spreadsheet.
func TestRenderWindowPartsRejoinExactly(t *testing.T) {
	cells := []Cell{
		{Ref: CellRef{Row: 0, Col: 0}, Computed: "a", Kind: KindText},
		{Ref: CellRef{Row: 2, Col: 3}, Computed: "42", Kind: KindNumber},
		{Ref: CellRef{Row: 4, Col: 25}, Computed: "z", Kind: KindText},
	}
	for _, sel := range []selRange{
		{},
		{On: true, Lo: CellRef{Row: 1, Col: 1}, Hi: CellRef{Row: 3, Col: 4}},
		{On: true, Lo: CellRef{Row: 0, Col: 0}, Hi: CellRef{Row: 0, Col: 0}},
	} {
		whole := renderWindow(cells, 0, 5, "demo", sel)
		head, tail := renderWindowParts(cells, 0, 5, "demo")
		if got := head + selHTML(sel) + tail; got != whole {
			t.Fatalf("rejoined render differs from whole render\n got %q\nwant %q", got, whole)
		}
	}
}

func TestRenderWindowPartsRejoinOnAnEmptyWindow(t *testing.T) {
	whole := renderWindow(nil, 10, 19, "demo", selRange{})
	head, tail := renderWindowParts(nil, 10, 19, "demo")
	if got := head + tail; got != whole {
		t.Fatalf("empty window differs\n got %q\nwant %q", got, whole)
	}
}

// ─── the cache itself ─────────────────────────────────────────────────────────

func halves(body string) windowHalves {
	return windowHalves{head: "<head:" + body + ">", tail: "<tail:" + body + ">", mask: []uint32{1, 2}}
}

func counting(n *int, body string) func(context.Context) (windowHalves, error) {
	return func(context.Context) (windowHalves, error) { *n++; return halves(body), nil }
}

func TestManyViewersOnOneWindowRenderOnce(t *testing.T) {
	c := newFlightCache[windowKey, windowHalves]()
	sh := &Sheet{ID: "demo"}
	key := windowKey{"demo", 0, 249}
	n := 0
	for i := 0; i < 50; i++ {
		got, err := c.get(context.Background(), key, sh, 7, counting(&n, "x"))
		if err != nil {
			t.Fatal(err)
		}
		if got.head != "<head:x>" || got.tail != "<tail:x>" {
			t.Fatalf("viewer %d got %q/%q", i, got.head, got.tail)
		}
	}
	if n != 1 {
		t.Fatalf("rendered %d times for 50 viewers on one window, want 1", n)
	}
	if hits, misses, _ := c.stats(); hits != 49 || misses != 1 {
		t.Errorf("hits=%d misses=%d, want 49/1", hits, misses)
	}
}

func TestAWriteInvalidatesTheWindow(t *testing.T) {
	c := newFlightCache[windowKey, windowHalves]()
	sh := &Sheet{ID: "demo"}
	key := windowKey{"demo", 0, 249}
	n := 0
	_, _ = c.get(context.Background(), key, sh, 1, counting(&n, "before"))
	got, _ := c.get(context.Background(), key, sh, 2, counting(&n, "after"))
	if got.head != "<head:after>" {
		t.Fatalf("served %q after the version moved, want the fresh render", got.head)
	}
	if n != 2 {
		t.Fatalf("rendered %d times across a version change, want 2", n)
	}
}

func TestDifferentWindowsDoNotShare(t *testing.T) {
	c := newFlightCache[windowKey, windowHalves]()
	sh := &Sheet{ID: "demo"}
	n := 0
	a, _ := c.get(context.Background(), windowKey{"demo", 0, 249}, sh, 1, counting(&n, "top"))
	b, _ := c.get(context.Background(), windowKey{"demo", 250, 499}, sh, 1, counting(&n, "mid"))
	if a.head == b.head {
		t.Fatal("two different windows returned the same bytes")
	}
	if n != 2 {
		t.Fatalf("rendered %d times for two windows, want 2", n)
	}
}

// The nastiest case: a sheet is reaped and its id revived as a fresh, empty sheet
// whose version restarts at zero. Keyed on version alone that collides with an
// entry cached at version zero, and the new sheet is served the dead one's rows.
func TestARevivedSheetIsNotServedTheDeadOnesRows(t *testing.T) {
	c := newFlightCache[windowKey, windowHalves]()
	key := windowKey{"demo", 0, 249}
	dead, alive := &Sheet{ID: "demo"}, &Sheet{ID: "demo"}
	n := 0
	oldv, _ := c.get(context.Background(), key, dead, 0, counting(&n, "dead"))
	fresh, _ := c.get(context.Background(), key, alive, 0, counting(&n, "alive"))
	if oldv.head == fresh.head {
		t.Fatal("a revived sheet was served the reaped sheet's window")
	}
	if fresh.head != "<head:alive>" {
		t.Fatalf("revived sheet got %q", fresh.head)
	}
}

// Single-flight is the whole point: N viewers woken by one edit must produce one
// read, not N. Without it the cache changes nothing under the load it exists for.
func TestConcurrentMissesRunOnce(t *testing.T) {
	c := newFlightCache[windowKey, windowHalves]()
	sh := &Sheet{ID: "demo"}
	key := windowKey{"demo", 0, 249}

	var mu sync.Mutex
	runs := 0
	release := make(chan struct{})
	slow := func(context.Context) (windowHalves, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		<-release
		return halves("shared"), nil
	}

	const N = 64
	var wg sync.WaitGroup
	got := make([]string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if h, err := c.get(context.Background(), key, sh, 3, slow); err == nil {
				got[i] = h.head
			}
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	n := runs
	mu.Unlock()
	if n != 1 {
		t.Fatalf("%d concurrent viewers caused %d renders, want 1", N, n)
	}
	for i, h := range got {
		if h != "<head:shared>" {
			t.Fatalf("viewer %d got %q", i, h)
		}
	}
}

func TestAFailedRenderIsNotCached(t *testing.T) {
	c := newFlightCache[windowKey, windowHalves]()
	sh := &Sheet{ID: "demo"}
	key := windowKey{"demo", 0, 249}
	boom := errors.New("read failed")

	_, err := c.get(context.Background(), key, sh, 1,
		func(context.Context) (windowHalves, error) { return windowHalves{}, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the render's error", err)
	}
	n := 0
	got, err := c.get(context.Background(), key, sh, 1, counting(&n, "ok"))
	if err != nil || got.head != "<head:ok>" || n != 1 {
		t.Fatalf("retry after failure: %q %v n=%d", got.head, err, n)
	}
}

func TestTheCacheIsBounded(t *testing.T) {
	c := newFlightCache[windowKey, windowHalves]()
	sh := &Sheet{ID: "demo"}
	n := 0
	for i := 0; i < maxCachedEntries*3; i++ {
		_, _ = c.get(context.Background(), windowKey{"demo", i * 250, i*250 + 249}, sh, 1,
			counting(&n, fmt.Sprint(i)))
	}
	c.mu.Lock()
	size := len(c.m)
	c.mu.Unlock()
	if size > maxCachedEntries {
		t.Fatalf("cache holds %d entries, want at most %d", size, maxCachedEntries)
	}
}

func TestANilCacheStillRuns(t *testing.T) {
	var c *flightCache[windowKey, windowHalves]
	n := 0
	got, err := c.get(context.Background(), windowKey{"demo", 0, 9}, nil, 0, counting(&n, "direct"))
	if err != nil || got.head != "<head:direct>" || n != 1 {
		t.Fatalf("nil cache: %q %v n=%d", got.head, err, n)
	}
}

// ─── the read every render path shares ───────────────────────────────────────

// TestOneEditReadsTheRowsOnceForEveryViewer is the property the whole change is
// for. The markup stays per viewer — whether a cell is a morph or an insert
// depends on what that browser holds — but the database read behind it must
// happen once, or N viewers queue N reads behind eight connections.
func TestOneEditReadsTheRowsOnceForEveryViewer(t *testing.T) {
	ConfigureStore(t.TempDir(), 8, time.Minute)
	t.Cleanup(func() { _ = CloseStore() })

	const id = "shared"
	if err := WriteSheet(id, func(s *Sheet) error {
		r, _ := ParseRef("A1")
		return s.WriteCell(r, "v", nil)
	}); err != nil {
		t.Fatal(err)
	}
	sh, err := OpenSheet(id)
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{cells: newFlightCache[windowKey, []Cell]()}

	const N = 64
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, rerr := srv.readWindow(context.Background(), sh, 0, 49); rerr != nil {
				t.Error(rerr)
			}
		}()
	}
	wg.Wait()

	hits, misses, _ := srv.cells.stats()
	if misses != 1 {
		t.Fatalf("%d viewers caused %d reads of the same rows, want 1", N, misses)
	}
	if hits != N-1 {
		t.Errorf("hits = %d, want %d", hits, N-1)
	}

	// And a write must push everyone back to the database exactly once more.
	if err := WriteSheet(id, func(s *Sheet) error {
		r, _ := ParseRef("B2")
		return s.WriteCell(r, "w", nil)
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < N; i++ {
		if _, rerr := srv.readWindow(context.Background(), sh, 0, 49); rerr != nil {
			t.Fatal(rerr)
		}
	}
	if _, misses, _ := srv.cells.stats(); misses != 2 {
		t.Fatalf("after one write the read count is %d, want 2", misses)
	}
}

// ─── the invalidation contract ────────────────────────────────────────────────

// TestEveryWriteVerbMovesTheVersion is what the shared cache actually rests on.
//
// The cache asks "has this sheet changed since I rendered it?" and answers with
// Sheet.Version, which the actor bumps. That is only complete while every
// mutation in the application goes through the actor — and the failure mode if
// one does not is not a slow page, it is every viewer being served a window that
// silently predates the edit.
//
// So this drives each write verb the way a handler does, through WriteSheet, and
// insists the version moved. A verb added later that writes directly to a *Sheet
// fails here rather than in production.
func TestEveryWriteVerbMovesTheVersion(t *testing.T) {
	ConfigureStore(t.TempDir(), 8, time.Minute)
	t.Cleanup(func() { _ = CloseStore() })

	const id = "verbs"
	sh, err := OpenSheet(id)
	if err != nil {
		t.Fatal(err)
	}

	ref := func(a string) CellRef {
		r, perr := ParseRef(a)
		if perr != nil {
			t.Fatalf("ParseRef(%q): %v", a, perr)
		}
		return r
	}

	for _, tc := range []struct {
		name string
		fn   func(*Sheet) error
	}{
		{"write a cell", func(s *Sheet) error { return s.WriteCell(ref("A1"), "1", nil) }},
		{"write cells", func(s *Sheet) error {
			return s.WriteCells([]BatchWrite{{Ref: ref("B1"), Raw: "2"}}, nil)
		}},
		{"column width", func(s *Sheet) error { return s.SetColWidth(3, 120) }},
		{"cell style", func(s *Sheet) error {
			bold := true
			_, e := s.SetStyle([]CellRef{ref("A1")}, StylePatch{Bold: &bold})
			return e
		}},
		{"column style", func(s *Sheet) error {
			bold := true
			_, e := s.SetColStyle([]int{2}, StylePatch{Bold: &bold})
			return e
		}},
		{"clear style", func(s *Sheet) error { _, e := s.ClearStyle([]CellRef{ref("A1")}); return e }},
		{"insert rows", func(s *Sheet) error { _, e := s.InsertRows(5, 1); return e }},
		{"delete rows", func(s *Sheet) error { _, e := s.DeleteRows(5, 1); return e }},
		{"insert cols", func(s *Sheet) error { _, e := s.InsertCols(3, 1); return e }},
		{"delete cols", func(s *Sheet) error { _, e := s.DeleteCols(3, 1); return e }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := sh.Version()
			if werr := WriteSheet(id, tc.fn); werr != nil {
				t.Fatalf("%s: %v", tc.name, werr)
			}
			if after := sh.Version(); after == before {
				t.Fatalf("%s did not move the version (still %d) — every viewer would "+
					"keep being served the window from before this write", tc.name, before)
			}
		})
	}
}

// A read must not move the version, or the cache never hits.
func TestReadingDoesNotMoveTheVersion(t *testing.T) {
	ConfigureStore(t.TempDir(), 8, time.Minute)
	t.Cleanup(func() { _ = CloseStore() })

	sh, err := OpenSheet("reads")
	if err != nil {
		t.Fatal(err)
	}
	if werr := WriteSheet("reads", func(s *Sheet) error {
		r, _ := ParseRef("A1")
		return s.WriteCell(r, "seed", nil)
	}); werr != nil {
		t.Fatal(werr)
	}

	before := sh.Version()
	for i := 0; i < 5; i++ {
		if _, rerr := sh.Window(0, 49); rerr != nil {
			t.Fatal(rerr)
		}
		_ = sh.Rows()
		_, _ = sh.UsedRows()
	}
	if after := sh.Version(); after != before {
		t.Fatalf("reads moved the version %d -> %d; the cache would never hit", before, after)
	}
}
