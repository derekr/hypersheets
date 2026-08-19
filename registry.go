package main

// registry.go — per-connection subscriptions, the thing that makes scrolling
// cheap.
//
// One Conn is one held-open SSE connection, i.e. one screen. It subscribes to
// exactly the bands its buffer covers — the viewport widened by BUFFER_BANDS on
// each side — and when the client crosses a buffer edge, SetBuffer diffs the band
// sets, so scrolling by one band costs one Subscribe and one Unsubscribe rather
// than a teardown and rebuild of the whole window.
//
// Three mechanisms keep an idle viewer at zero cost. Subscription scope is the one
// that matters: a viewer on bands 0..4 is not subscribed to band 180, so a write
// there never reaches this process's callback. Digest suppression and coalescing
// are backstops — a re-render producing bytes identical to the last ones sent is
// dropped, and the one-slot wake channel is latest-state-wins, so a slow consumer
// cannot queue a backlog.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
)

// Conn is one SSE connection's subscription state.
type Conn struct {
	ID      string
	SheetID string
	LoBand  int // buffer start (NOT viewport — the buffer is wider)
	HiBand  int // buffer end, inclusive

	// subs is guarded by Registry.mu, as are LoBand/HiBand after Register.
	subs map[int]*nats.Subscription

	// wake holds at most one pending signal: latest-state-wins backpressure.
	wake chan struct{}

	mu          sync.Mutex
	dirty       bool
	closed      bool
	lastRelease time.Time
	timer       *time.Timer
	digest      [sha256.Size]byte
	hasDigest   bool

	// Presence has its own lock, channel and closed flag, and Wake, Deliver and
	// markDirty touch none of them. Presence ticks whether or not anything is being
	// written, so it must not contend with the render path for c.mu; and a wake
	// means "re-render your buffer", costing a window read and thousands of cells,
	// where somebody else's cursor moving must cost a few hundred bytes of overlay
	// and no database access. One channel would make every selection commit
	// re-render every other viewer's grid.
	//
	// sel is this screen's selection, held the way LoBand/HiBand above are: the
	// client manipulates it locally and commits on settle, and the server keeps the
	// copy that the render path reads to mark the owner's cells and the presence
	// read model reads to place a collaborator's cursor. It is in memory, not
	// persisted — a selection is correctly empty after a restart, so it dies with
	// the *Conn.
	pmu     sync.Mutex
	pclosed bool
	pwake   chan struct{}
	sel     Selection

	// hue/color are this screen's presence colour, assigned once by Register
	// (presence.go, pickHueLocked) and immutable afterwards, like the ID.
	//
	// It is assigned rather than derived because the only useful statement about it
	// is relative — "not like anyone else's here" — which a hash of the connection
	// id cannot make. It lives on the connection so that the delete in unregister
	// releases it without anything having to remember to free it.
	hue   int
	color string
}

// PresenceWakes is the connection's presence trigger, separate from Wakes.
// Receiving from it means "who is here, where they are looking, or what they just
// changed is no longer what you rendered"; it never means the grid is stale. Like
// Wakes it is closed on Unregister.
func (c *Conn) PresenceWakes() <-chan struct{} { return c.pwake }

// Selection reads this connection's held selection. The render path wants it to
// mark the owner's own cells; the presence read model wants it to place a
// collaborator's cursor. Both read the same copy.
func (c *Conn) Selection() Selection {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	return c.sel
}

// setSelection stores a selection and reports whether it differs from the last
// one. A repeated commit is not a change and must not cost a publish, so At is
// deliberately not part of the comparison.
func (c *Conn) setSelection(s Selection) bool {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	if c.sel.Cell == s.Cell && c.sel.On == s.On && c.sel.Range == s.Range {
		return false
	}
	c.sel = s
	return true
}

// Wakes is the connection's render trigger. Receiving from it means "something
// in your buffer changed; re-render the whole buffer and call Deliver". The
// channel is closed on Unregister, which is how the render loop exits.
func (c *Conn) Wakes() <-chan struct{} { return c.wake }

// Dirty reports whether a wake is owed to this connection but has not been
// released yet — only possible while the throttle window is open.
func (c *Conn) Dirty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirty
}

// RegistryStats is a snapshot of registry counters. Connections and
// Subscriptions are gauges (they go back to zero on teardown); the rest are
// cumulative.
type RegistryStats struct {
	Connections     int
	Subscriptions   int
	Wakes           uint64
	Renders         uint64
	Pushes          uint64
	Suppressed      uint64
	SuppressionRate float64
	Coalesced       uint64

	// Presence counters are separate, and none of the counters above count a
	// presence event: Wakes has to stay the number of times a connection was told
	// its grid is stale.
	PresencePublishes uint64 // per sheet, capped at presenceMaxHz
	PresenceWakes     uint64 // per connection
	PresenceCoalesced uint64
}

// Registry maps sheets and bands to the connections that care about them.
type Registry struct {
	bus      *Bus
	throttle time.Duration

	mu    sync.RWMutex
	conns map[string]*Conn

	wakes      atomic.Uint64
	renders    atomic.Uint64
	pushes     atomic.Uint64
	suppressed atomic.Uint64
	coalesced  atomic.Uint64

	// presence is the read model's bookkeeping (presence.go). It holds no viewers —
	// conns above is the list of viewers — only the per-sheet publish throttle, the
	// per-sheet attribution ring and the decay interval.
	presence          *presenceHub
	presencePublishes atomic.Uint64
	presenceWakes     atomic.Uint64
	presenceCoalesced atomic.Uint64
}

// NewRegistry builds a registry over a bus. throttleMs is the floor on the gap
// between two renders of a single connection; 0 (the default) disables it.
func NewRegistry(bus *Bus, throttleMs int) *Registry {
	r := &Registry{
		bus:      bus,
		throttle: time.Duration(throttleMs) * time.Millisecond,
		conns:    make(map[string]*Conn),
	}
	r.presence = newPresenceHub(r, bus)
	return r
}

// Close stops the presence decay interval, for tests and orderly shutdown. It does
// not tear down connections, which own their own lifetimes.
func (r *Registry) Close() {
	if r.presence != nil {
		r.presence.close()
	}
}

// Register subscribes a connection to every band in its buffer, and announces it to
// the sheet's presence topic. The presence invalidation is deliberately outside the
// registry lock: it ends in a publish whose callback wants to read the connection
// map, so under r.mu the join would wait on itself in the single-process case, and
// it is the wrong lock order in every case.
func (r *Registry) Register(c *Conn) error {
	if err := r.register(c); err != nil {
		return err
	}
	r.presence.join(c)
	return nil
}

func (r *Registry) register(c *Conn) error {
	if c == nil || c.ID == "" {
		return fmt.Errorf("register: connection needs an ID")
	}
	if c.SheetID == "" {
		return fmt.Errorf("register %s: connection needs a sheet ID", c.ID)
	}
	if c.LoBand > c.HiBand {
		return fmt.Errorf("register %s: bad buffer [%d,%d]", c.ID, c.LoBand, c.HiBand)
	}
	if c.LoBand < 0 {
		c.LoBand = 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.conns[c.ID]; dup {
		return fmt.Errorf("register %s: already registered", c.ID)
	}

	c.subs = make(map[int]*nats.Subscription, c.HiBand-c.LoBand+1)
	c.wake = make(chan struct{}, 1)
	c.pwake = make(chan struct{}, 1)

	for band := c.LoBand; band <= c.HiBand; band++ {
		if err := r.subscribeLocked(c, band); err != nil {
			r.dropSubsLocked(c)
			return err
		}
	}
	// The hue is chosen inside the same critical section that inserts the
	// connection: two simultaneous joins must not each survey a room that neither
	// of them is in yet and pick the same colour. The cost is paid once per stream,
	// and nothing in Wake, Deliver or markDirty learns that this exists.
	c.hue = r.pickHueLocked(c.SheetID, c.ID)
	c.color = hueColor(c.hue)

	r.conns[c.ID] = c
	return nil
}

// Unregister tears a connection down: subscriptions first (so no further callbacks
// can arrive), then the timer, then both wake channels. A leaked subscriber renders
// for a browser that is gone, so this has to be reliable — it is safe to call twice
// and safe to call concurrently with a wake.
//
// It is also all that presence teardown needs: the read model is r.conns and
// nothing else, so there is no second list that could disagree and leave a ghost.
// What remains is telling the others, which is the invalidate.
func (r *Registry) Unregister(connID string) {
	c := r.unregister(connID)
	if c != nil {
		r.presence.leave(c)
	}
}

// unregister returns the connection it tore down, or nil if there was nothing to
// do. It fires exactly once per connection, which is what makes the presence
// invalidation above idempotent too.
func (r *Registry) unregister(connID string) *Conn {
	r.mu.Lock()
	c, ok := r.conns[connID]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	delete(r.conns, connID)
	r.dropSubsLocked(c)
	r.mu.Unlock()

	c.pmu.Lock()
	if !c.pclosed {
		c.pclosed = true
		c.sel = Selection{}
		close(c.pwake)
	}
	c.pmu.Unlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return c
	}
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	close(c.wake)
	c.mu.Unlock()
	return c
}

// SetBuffer moves a connection's buffer window, diffing the old and new band sets
// so only the bands that changed are touched: scrolling by one band is one
// Subscribe and one Unsubscribe however wide the buffer is. Returns how many
// subscriptions were added and dropped.
func (r *Registry) SetBuffer(connID string, loBand, hiBand int) (added, dropped int, err error) {
	if loBand > hiBand {
		return 0, 0, fmt.Errorf("set buffer %s: bad window [%d,%d]", connID, loBand, hiBand)
	}
	if loBand < 0 {
		loBand = 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conns[connID]
	if !ok {
		return 0, 0, fmt.Errorf("set buffer: unknown connection %s", connID)
	}
	if c.LoBand == loBand && c.HiBand == hiBand {
		return 0, 0, nil
	}

	for band, sub := range c.subs {
		if band < loBand || band > hiBand {
			_ = sub.Unsubscribe()
			delete(c.subs, band)
			dropped++
		}
	}
	for band := loBand; band <= hiBand; band++ {
		if _, have := c.subs[band]; have {
			continue
		}
		if err := r.subscribeLocked(c, band); err != nil {
			return added, dropped, err
		}
		added++
	}
	c.LoBand, c.HiBand = loBand, hiBand
	return added, dropped, nil
}

// SetBufferCtx is SetBuffer with a span. The added/dropped counters are span
// attributes rather than something to infer from a log line, because they are what
// shows a one-band scroll costing exactly one subscribe and one unsubscribe
// however wide the buffer is.
func (r *Registry) SetBufferCtx(ctx context.Context, connID string, loBand, hiBand int) (added, dropped int, err error) {
	_, span := tracer.Start(ctx, "registry.set_buffer")
	defer span.End()
	added, dropped, err = r.SetBuffer(connID, loBand, hiBand)
	span.SetAttributes(
		attribute.String("conn.id", connID),
		attribute.Int("buffer.lo_band", loBand),
		attribute.Int("buffer.hi_band", hiBand),
		attribute.Int("bands.added", added),
		attribute.Int("bands.dropped", dropped),
	)
	if err != nil {
		span.RecordError(err)
	}
	return added, dropped, err
}

// DeliverCtx is Deliver with a span. `suppressed` is true exactly when the render
// produced bytes identical to the last ones sent — the difference between a
// full-window write and nothing on the wire.
func (r *Registry) DeliverCtx(ctx context.Context, connID string, payload []byte) (bool, error) {
	_, span := tracer.Start(ctx, "registry.deliver")
	defer span.End()
	send, err := r.Deliver(connID, payload)
	span.SetAttributes(
		attribute.String("conn.id", connID),
		attribute.Int("bytes", len(payload)),
		attribute.Bool("suppressed", err == nil && !send),
		attribute.Bool("digest_hit", err == nil && !send),
	)
	if err != nil {
		span.RecordError(err)
	}
	return send, err
}

// Wake marks every connection whose buffer covers band as needing a re-render and
// returns them.
//
// The hot path does not go through here: each connection holds its own NATS
// subscriptions and the broker's subject matching already decides who to wake,
// which is why the Registry has no band index. Wake is the direct, bus-free path —
// tests, single-process dispatch, and "who would this write reach?".
func (r *Registry) Wake(sheetID string, band int) []*Conn {
	r.mu.RLock()
	var hit []*Conn
	for _, c := range r.conns {
		if c.SheetID == sheetID && band >= c.LoBand && band <= c.HiBand {
			hit = append(hit, c)
		}
	}
	r.mu.RUnlock()

	for _, c := range hit {
		r.markDirty(c)
	}
	return hit
}

// Deliver records a freshly rendered payload and reports whether the caller should
// write it to the wire. If the bytes are identical to the last ones sent to this
// connection the morph would be a no-op, so it is dropped and counted. It is a
// backstop: with subscription scoping doing its job, a viewer on band 0 is never
// woken by a write to band 180 in the first place.
func (r *Registry) Deliver(connID string, payload []byte) (bool, error) {
	r.mu.RLock()
	c, ok := r.conns[connID]
	r.mu.RUnlock()
	if !ok {
		return false, fmt.Errorf("deliver: unknown connection %s", connID)
	}

	r.renders.Add(1)
	sum := sha256.Sum256(payload)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasDigest && c.digest == sum {
		r.suppressed.Add(1)
		return false, nil
	}
	c.digest = sum
	c.hasDigest = true
	r.pushes.Add(1)
	return true, nil
}

// ForgetDigest drops a connection's remembered payload so the next Deliver cannot
// suppress.
//
// It is what makes incremental patching safe. Deliver's contract — identical bytes
// mean the DOM is already in this state — holds only while the payload is the whole
// window. A scroll expressed as row patches describes a delta against a DOM the
// digest cannot see: two scrolls landing on the same bands emit byte-identical
// patches even though the client moved a long way in between, and suppressing the
// second would strand the user on rows they scrolled away from. So the scroll path
// calls this instead, which also invalidates the last full-window digest, since
// that describes a window the client no longer shows.
func (r *Registry) ForgetDigest(connID string) {
	r.mu.RLock()
	c, ok := r.conns[connID]
	r.mu.RUnlock()
	if !ok {
		return
	}
	c.mu.Lock()
	c.hasDigest = false
	c.digest = [sha256.Size]byte{}
	c.mu.Unlock()
}

// DeliverIncremental records a push whose payload is a delta rather than the
// whole window: it counts the render and the push so the Stats totals stay
// honest, and forgets the digest for the reason above.
func (r *Registry) DeliverIncremental(connID string) {
	r.renders.Add(1)
	r.pushes.Add(1)
	r.ForgetDigest(connID)
}

// wakePresence marks every connection on a sheet as owing a presence re-render. It
// runs on the presence subscription's callback goroutine, so like markDirty it must
// never block.
//
// One subscription, N marks — the opposite of the band scheme, where per-connection
// subscriptions are what make "who does this wake" free. A presence subject has
// exactly one audience, everyone on the sheet, so N subscriptions would only make
// the broker deliver N copies of the same empty message for this process to fan out
// anyway.
func (r *Registry) wakePresence(sheetID string) {
	r.mu.RLock()
	var hit []*Conn
	for _, c := range r.conns {
		if c.SheetID == sheetID {
			hit = append(hit, c)
		}
	}
	r.mu.RUnlock()

	for _, c := range hit {
		r.markPresenceDirty(c)
	}
}

// markPresenceDirty releases one presence wake, coalescing. There is no throttle:
// the publish side is already capped at presenceMaxHz per sheet, and the one-slot
// channel handles a consumer slower still — a presence wake carries no
// information, so a pending one already covers whatever just happened.
func (r *Registry) markPresenceDirty(c *Conn) {
	r.presenceWakes.Add(1)
	c.pmu.Lock()
	defer c.pmu.Unlock()
	if c.pclosed {
		return
	}
	select {
	case c.pwake <- struct{}{}:
	default:
		r.presenceCoalesced.Add(1)
	}
}

// sheetsWithViewers reports which of the given sheet ids still have at least one
// connection. One lock for the whole question, so the presence sweeper can reap
// without holding two locks at once.
func (r *Registry) sheetsWithViewers(ids []string) map[string]bool {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = false
	}
	r.mu.RLock()
	for _, c := range r.conns {
		if _, ok := want[c.SheetID]; ok {
			want[c.SheetID] = true
		}
	}
	r.mu.RUnlock()
	return want
}

// Stats snapshots the counters.
func (r *Registry) Stats() RegistryStats {
	r.mu.RLock()
	s := RegistryStats{Connections: len(r.conns)}
	for _, c := range r.conns {
		s.Subscriptions += len(c.subs)
	}
	r.mu.RUnlock()

	s.Wakes = r.wakes.Load()
	s.Renders = r.renders.Load()
	s.Pushes = r.pushes.Load()
	s.Suppressed = r.suppressed.Load()
	s.Coalesced = r.coalesced.Load()
	s.PresencePublishes = r.presencePublishes.Load()
	s.PresenceWakes = r.presenceWakes.Load()
	s.PresenceCoalesced = r.presenceCoalesced.Load()
	if total := s.Pushes + s.Suppressed; total > 0 {
		s.SuppressionRate = float64(s.Suppressed) / float64(total)
	}
	return s
}

// Conn looks up a registered connection.
func (r *Registry) Conn(connID string) (*Conn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.conns[connID]
	return c, ok
}

// subscribeLocked requires r.mu held for writing.
func (r *Registry) subscribeLocked(c *Conn, band int) error {
	if r.bus == nil {
		// Bus-less registries are useful in tests and for a dispatcher that
		// drives Wake directly; buffer bookkeeping still has to be correct.
		c.subs[band] = nil
		return nil
	}
	sub, err := r.bus.SubscribeBand(c.SheetID, band, func() { r.markDirty(c) })
	if err != nil {
		return err
	}
	c.subs[band] = sub
	return nil
}

// dropSubsLocked requires r.mu held for writing.
func (r *Registry) dropSubsLocked(c *Conn) {
	for band, sub := range c.subs {
		if sub != nil {
			_ = sub.Unsubscribe()
		}
		delete(c.subs, band)
	}
}

// markDirty is the wake path. It runs on a NATS callback goroutine, so it must
// never block.
//
// A wake landing inside the throttle window leaves the connection dirty and
// schedules the release for when the window closes; it is never dropped. The
// throttle bounds CPU, not correctness — dropping a wake would let a client sit
// forever on stale HTML, a much worse bug than rendering a bit late.
func (r *Registry) markDirty(c *Conn) {
	r.wakes.Add(1)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.dirty = true

	if r.throttle <= 0 {
		r.releaseLocked(c)
		return
	}
	elapsed := time.Since(c.lastRelease)
	if elapsed >= r.throttle {
		r.releaseLocked(c)
		return
	}
	if c.timer == nil {
		c.timer = time.AfterFunc(r.throttle-elapsed, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.timer = nil
			if c.closed || !c.dirty {
				return
			}
			r.releaseLocked(c)
		})
	}
}

// releaseLocked requires c.mu held.
func (r *Registry) releaseLocked(c *Conn) {
	select {
	case c.wake <- struct{}{}:
		c.dirty = false
		c.lastRelease = time.Now()
	default:
		// A wake is already pending and the consumer has not picked it up.
		// Wakes carry no payload, so the pending one already covers this
		// change: latest-state-wins, and the queue never grows.
		c.dirty = false
		r.coalesced.Add(1)
	}
}
