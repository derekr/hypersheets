package main

// windowcache.go — read a window once, however many people are looking at it.
//
// Every connection used to read for itself. An edit dirties ten cells and each of
// N viewers independently reads those cells out of SQLite and builds its own
// patch. Measured, that is not CPU-bound: it is N copies of one read queued
// behind a per-sheet connection pool of eight, which is why the same render took
// 26x longer at 256 viewers than at 1 while the machine was 29% of one core busy.
// Enlarging the pool was measured and does not help — the queue simply moves into
// the Go scheduler and CPU per edit rises.
//
// So the cached artefact is the READ, not the markup. That is where the
// contention is, and it is the one thing every render path shares:
//
//	renderCellPatch   the edit path, and the hot one
//	renderScreen      first paint, disjoint jumps, structural changes
//	renderDelta       rows appearing at the edge of a scroll
//
// The markup cannot simply be shared, and that is not an oversight: whether a
// cell is a morph or an insert depends on whether THAT browser already holds it
// (screen.heldCell), so the patch is genuinely per viewer. Only the data it is
// built from is common. renderScreen is the exception — its window markup is
// viewer-independent apart from the selection — so it caches its rendered halves
// as well, above the same read.

import (
	"context"
	"sync"
	"sync/atomic"
)

// windowKey identifies a range of rows of a sheet. The sheet's mutation counter
// is deliberately NOT part of it: entries are replaced in place when the version
// moves, so a hot window occupies one slot for the life of the sheet rather than
// one slot per edit.
type windowKey struct {
	sheetID string
	lo, hi  int
}

// windowHalves is a rendered window with the selection left out — the two pieces
// that surround it. See renderWindowParts.
type windowHalves struct {
	head, tail string
	mask       []uint32
}

// flight is one cached value and its own single-flight latch.
//
// The entry is published to the map BEFORE it is populated, with `ready` still
// open. That is the whole concurrency design: the second caller to want a value
// finds the entry, waits on the channel, and gets the first caller's result.
// Without it, N viewers woken by the same edit all miss together and the cache
// changes nothing — under exactly the load it exists for.
type flight[V any] struct {
	ready chan struct{}

	// sh identifies the HANDLE, not just the sheet id. A reaped id can be revived
	// as a fresh, empty sheet whose version counter restarts at zero, which would
	// collide with an entry cached at version zero and serve the deleted sheet's
	// rows under the new sheet's name. Comparing the handle pointer closes that,
	// and a revived sheet is necessarily a different *Sheet.
	sh  *Sheet
	ver uint64

	val V
	err error
}

// flightCache is bounded, per process, and needs no LRU.
//
// Viewers cluster hard onto a handful of distinct windows — buffers are
// band-aligned and people scroll to the same places — so the working set is tiny
// and eviction only has to stop abandoned windows accumulating without bound.
// When the map fills it is dropped wholesale, costing one round of misses on a
// boundary that is crossed rarely.
type flightCache[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]*flight[V]

	hits, misses, waits atomic.Uint64
}

// maxCachedEntries is generous against the working set and small against memory.
const maxCachedEntries = 256

func newFlightCache[K comparable, V any]() *flightCache[K, V] {
	return &flightCache[K, V]{m: make(map[K]*flight[V], 64)}
}

// get returns the value for key at (sh, ver), producing it at most once across
// every concurrent caller that wants the same thing.
//
// make runs WITHOUT the cache lock held, so a slow read cannot block callers
// wanting a different key.
func (c *flightCache[K, V]) get(ctx context.Context, key K, sh *Sheet, ver uint64,
	make func(context.Context) (V, error),
) (V, error) {
	var zero V
	if c == nil {
		return make(ctx)
	}

	c.mu.Lock()
	if e, ok := c.m[key]; ok && e.sh == sh && e.ver == ver {
		c.mu.Unlock()
		select {
		case <-e.ready:
		case <-ctx.Done():
			// Do not inherit the leader's work if this caller is going away.
			return zero, ctx.Err()
		}
		if e.err != nil {
			c.misses.Add(1)
			return zero, e.err
		}
		c.hits.Add(1)
		c.waits.Add(1)
		return e.val, nil
	}
	if len(c.m) >= maxCachedEntries {
		c.m = map[K]*flight[V]{}
	}
	e := &flight[V]{ready: make2(), sh: sh, ver: ver}
	c.m[key] = e
	c.mu.Unlock()

	c.misses.Add(1)
	e.val, e.err = make(ctx)
	close(e.ready)

	if e.err != nil {
		// A failed entry must not be served to the next caller, which would turn
		// one bad read into a sheet that cannot render until something writes to
		// it.
		c.mu.Lock()
		if cur, ok := c.m[key]; ok && cur == e {
			delete(c.m, key)
		}
		c.mu.Unlock()
	}
	return e.val, e.err
}

// make2 exists only because `make` is shadowed by the parameter above.
func make2() chan struct{} { return make(chan struct{}) }

// stats reports hits, misses, and how many hits were served by waiting on work
// already in flight. The last is the number that says the single-flight is
// working: it is the fan-out that used to be N separate reads.
func (c *flightCache[K, V]) stats() (hits, misses, waits uint64) {
	if c == nil {
		return 0, 0, 0
	}
	return c.hits.Load(), c.misses.Load(), c.waits.Load()
}

// readWindow is the one read every render path goes through.
//
// The returned slice is SHARED by every caller and must be treated as read-only.
// Every consumer today only reads it (renderWindowParts, maskOf, and the
// classification in renderCellPatch, which copies into its own map).
func (s *Server) readWindow(ctx context.Context, sh *Sheet, lo, hi int) ([]Cell, error) {
	return s.cells.get(ctx, windowKey{sh.ID, lo, hi}, sh, sh.Version(),
		func(ctx context.Context) ([]Cell, error) { return sh.WindowCtx(ctx, lo, hi) })
}
