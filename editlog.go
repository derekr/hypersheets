package main

// editlog.go — which cells changed, keyed by sequence number.
//
// The bus carries no payload (bus.go): a subject means "go look", which is what
// makes it idempotent, unordered and loss-tolerant. That is right for a wake and
// wrong for a patch, since a woken connection knows only that it must re-render.
// So every committed edit appends its dirty set here under a monotonic sequence
// number and every screen remembers the sequence it has shown; a wake then asks
// "what changed since N" and patches exactly those cells.
//
// A log rather than a single "last edit" per sheet, because wakes coalesce
// (registry.go: the wake channel holds one slot, latest-state-wins) and two
// edits landing between one connection's renders produce one wake. Keeping only
// the newest dirty set would leave the first edit's cells stale until something
// unrelated touched them.
//
// The ring is bounded, so history can be lost: a connection that fell far enough
// behind gets ok=false and its caller falls back to a full morph. Losing history
// must degrade to "send everything", never to "send nothing".

import (
	"context"
	"sort"
	"sync"
	"time"
)

// editLogDepth is how many edits of history one sheet keeps. A screen that has
// missed more than this many is re-rendered whole, which costs one fat morph and
// is self-correcting. At one edit per second it is four minutes of slack for a
// connection that is not being serviced at all — far longer than a live stream
// can plausibly be behind.
const editLogDepth = 256

// editEntry is one committed edit's effect.
type editEntry struct {
	seq   uint64
	ctx   context.Context
	at    time.Time
	cells []CellRef
}

// editLog is one sheet's bounded dirty-set history.
type editLog struct {
	mu   sync.Mutex
	seq  uint64
	ring []editEntry
}

// Seq is the sequence number of the most recent edit. A screen that renders from
// the database should record the value read before the read, so an edit that
// commits during the read is replayed rather than assumed included.
func (l *editLog) Seq() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// Last is the newest edit's trace context, so a fan-out push can be parented
// onto the command that caused it. A sheet with no edits yet returns a
// background context rather than nil — a wake with no recorded cause is normal
// on a stream that has only ever seen its own first paint.
func (l *editLog) Last() (context.Context, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ring) == 0 {
		return context.Background(), time.Time{}
	}
	e := l.ring[len(l.ring)-1]
	return e.ctx, e.at
}

// Append records an edit's dirty set and returns its sequence number.
func (l *editLog) Append(ctx context.Context, cells []CellRef) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	// The slice comes from the recalc pass and the caller keeps using it; copy
	// so a later append cannot rewrite history under a reader.
	cp := make([]CellRef, len(cells))
	copy(cp, cells)
	l.ring = append(l.ring, editEntry{seq: l.seq, ctx: ctx, at: time.Now(), cells: cp})
	if len(l.ring) > editLogDepth {
		l.ring = append(l.ring[:0], l.ring[len(l.ring)-editLogDepth:]...)
	}
	return l.seq
}

// Since returns every cell dirtied after seq, deduplicated, together with the
// newest edit's trace context and timestamp so the push can be parented onto
// the command that caused it.
//
// ok is false when the answer cannot be trusted: the caller has fallen off the
// back of the ring and there are edits it will never learn about. It is the
// caller's job to turn that into a full re-render.
func (l *editLog) Since(seq uint64) (cells []CellRef, ctx context.Context, at time.Time, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq >= l.seq {
		// Nothing new. Not an error: a wake can arrive for an edit this screen
		// already picked up on a previous pass.
		return nil, nil, time.Time{}, true
	}
	if len(l.ring) == 0 || l.ring[0].seq > seq+1 {
		return nil, nil, time.Time{}, false
	}
	seen := make(map[CellRef]struct{}, 16)
	for _, e := range l.ring {
		if e.seq <= seq {
			continue
		}
		for _, c := range e.cells {
			if _, dup := seen[c]; dup {
				continue
			}
			seen[c] = struct{}{}
			cells = append(cells, c)
		}
		ctx, at = e.ctx, e.at
	}
	return cells, ctx, at, true
}

// cellRunGap / cellRunMax mirror recalc.go's prefetch tuning, for the same
// reason: cells are clustered on (row, col), so reading across a small hole is
// cheaper than issuing a second query.
const (
	cellRunGap = 4
	cellRunMax = 64
)

// rowRuns coalesces a set of row numbers into the fewest contiguous ranges
// worth reading. rows may repeat and need not be sorted; the result is
// ascending and disjoint.
//
// A dirty set is scattered by nature — `C1=SUM(A1:A10)` and `Y201` are 200 rows
// apart — so reading one range from the lowest to the highest dirty row would
// pull back most of the buffer and undo the whole point.
func rowRuns(rows []int, gap, max int) []rowRange {
	if len(rows) == 0 {
		return nil
	}
	want := make([]int, 0, len(rows))
	seen := make(map[int]struct{}, len(rows))
	for _, r := range rows {
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		want = append(want, r)
	}
	sort.Ints(want)

	out := make([]rowRange, 0, len(want))
	cur := rowRange{Lo: want[0], Hi: want[0]}
	for _, r := range want[1:] {
		if r-cur.Hi <= gap && r-cur.Lo < max {
			cur.Hi = r
			continue
		}
		out = append(out, cur)
		cur = rowRange{Lo: r, Hi: r}
	}
	return append(out, cur)
}
