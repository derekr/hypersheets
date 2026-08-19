package main

import (
	"fmt"
	"testing"
	"time"
)

func mustRegister(t *testing.T, r *Registry, id, sheetID string, lo, hi int) *Conn {
	t.Helper()
	c := &Conn{ID: id, SheetID: sheetID, LoBand: lo, HiBand: hi}
	if err := r.Register(c); err != nil {
		t.Fatalf("Register(%s): %v", id, err)
	}
	return c
}

// drain empties a wake channel without blocking and reports how many signals
// were pending.
func drain(c *Conn) int {
	n := 0
	for {
		select {
		case _, ok := <-c.Wakes():
			if !ok {
				return n
			}
			n++
		default:
			return n
		}
	}
}

func awaitWake(t *testing.T, c *Conn, d time.Duration) bool {
	t.Helper()
	select {
	case <-c.Wakes():
		return true
	case <-time.After(d):
		return false
	}
}

func TestRegisterSubscribesEveryBufferBand(t *testing.T) {
	r := NewRegistry(newTestBus(t), 0)
	mustRegister(t, r, "c1", "s1", 2, 6)

	s := r.Stats()
	if s.Connections != 1 {
		t.Errorf("connections = %d, want 1", s.Connections)
	}
	if s.Subscriptions != 5 {
		t.Errorf("subscriptions = %d, want 5 for bands 2..6", s.Subscriptions)
	}
}

func TestRegisterRejectsBadInput(t *testing.T) {
	r := NewRegistry(newTestBus(t), 0)
	tests := []struct {
		name string
		conn *Conn
	}{
		{"no id", &Conn{SheetID: "s1", LoBand: 0, HiBand: 1}},
		{"no sheet", &Conn{ID: "c", LoBand: 0, HiBand: 1}},
		{"inverted window", &Conn{ID: "c", SheetID: "s1", LoBand: 5, HiBand: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Register(tc.conn); err == nil {
				t.Error("expected an error")
			}
		})
	}
	mustRegister(t, r, "dup", "s1", 0, 1)
	if err := r.Register(&Conn{ID: "dup", SheetID: "s1", LoBand: 0, HiBand: 1}); err == nil {
		t.Error("duplicate registration should fail")
	}
}

// The whole point of the registry: scrolling one band must cost one Subscribe
// and one Unsubscribe, not a rebuild of the window.
func TestSetBufferDiffs(t *testing.T) {
	tests := []struct {
		name           string
		from           [2]int
		to             [2]int
		wantAdd, wantD int
		wantSubs       int
	}{
		{"scroll down one band", [2]int{0, 4}, [2]int{1, 5}, 1, 1, 5},
		{"scroll up one band", [2]int{4, 8}, [2]int{3, 7}, 1, 1, 5},
		{"no movement", [2]int{0, 4}, [2]int{0, 4}, 0, 0, 5},
		{"grow the buffer", [2]int{2, 6}, [2]int{1, 7}, 2, 0, 7},
		{"shrink the buffer", [2]int{1, 7}, [2]int{2, 6}, 0, 2, 5},
		{"jump far away (disjoint)", [2]int{0, 4}, [2]int{100, 104}, 5, 5, 5},
		{"overlapping jump", [2]int{0, 4}, [2]int{3, 7}, 3, 3, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry(newTestBus(t), 0)
			mustRegister(t, r, "c1", "s1", tc.from[0], tc.from[1])

			added, dropped, err := r.SetBuffer("c1", tc.to[0], tc.to[1])
			if err != nil {
				t.Fatalf("SetBuffer: %v", err)
			}
			if added != tc.wantAdd || dropped != tc.wantD {
				t.Errorf("added=%d dropped=%d, want added=%d dropped=%d",
					added, dropped, tc.wantAdd, tc.wantD)
			}
			if got := r.Stats().Subscriptions; got != tc.wantSubs {
				t.Errorf("subscriptions = %d, want %d", got, tc.wantSubs)
			}
			c, _ := r.Conn("c1")
			if c.LoBand != tc.to[0] || c.HiBand != tc.to[1] {
				t.Errorf("window = [%d,%d], want [%d,%d]", c.LoBand, c.HiBand, tc.to[0], tc.to[1])
			}
		})
	}
}

func TestSetBufferUnknownConn(t *testing.T) {
	r := NewRegistry(newTestBus(t), 0)
	if _, _, err := r.SetBuffer("nope", 0, 4); err == nil {
		t.Error("expected an error for an unknown connection")
	}
}

// After a one-band scroll the *live* subscriptions must match the new window:
// the dropped band must stop waking the connection and the added one must start.
func TestSetBufferRewiresLiveSubscriptions(t *testing.T) {
	bus := newTestBus(t)
	r := NewRegistry(bus, 0)
	c := mustRegister(t, r, "c1", "s1", 0, 4)

	if _, _, err := r.SetBuffer("c1", 1, 5); err != nil {
		t.Fatalf("SetBuffer: %v", err)
	}
	drain(c)

	// Band 0 was dropped.
	if err := bus.Publish("s1", []int{0}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := bus.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if awaitWake(t, c, 200*time.Millisecond) {
		t.Error("band 0 still wakes the connection after it left the buffer")
	}

	// Band 5 was added.
	if err := bus.Publish("s1", []int{5}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !awaitWake(t, c, 2*time.Second) {
		t.Error("band 5 did not wake the connection after it entered the buffer")
	}
}

// The measured cost of a one-band scroll, from the broker's point of view.
func TestOneBandScrollCostsOneSubscribeOneUnsubscribe(t *testing.T) {
	bus := newTestBus(t)
	r := NewRegistry(bus, 0)
	mustRegister(t, r, "c1", "s1", 10, 14) // a 5-band buffer

	before := bus.Conn().NumSubscriptions()
	if before != 5 {
		t.Fatalf("client subscriptions after register = %d, want 5", before)
	}

	added, dropped, err := r.SetBuffer("c1", 11, 15)
	if err != nil {
		t.Fatalf("SetBuffer: %v", err)
	}
	if added != 1 || dropped != 1 {
		t.Errorf("one-band scroll cost added=%d dropped=%d, want 1/1", added, dropped)
	}
	if after := bus.Conn().NumSubscriptions(); after != before {
		t.Errorf("client subscriptions after scroll = %d, want %d (steady state)", after, before)
	}
	t.Logf("one-band scroll: %d subscribe, %d unsubscribe, %d live subscriptions (buffer width 5)",
		added, dropped, bus.Conn().NumSubscriptions())
}

func TestWakeTargeting(t *testing.T) {
	r := NewRegistry(newTestBus(t), 0)
	mustRegister(t, r, "c1", "s1", 2, 6)
	mustRegister(t, r, "c2", "s1", 100, 104)
	mustRegister(t, r, "c3", "s2", 2, 6) // same bands, different sheet

	tests := []struct {
		name    string
		sheetID string
		band    int
		want    []string
	}{
		{"inside the buffer", "s1", 4, []string{"c1"}},
		{"lower edge is inclusive", "s1", 2, []string{"c1"}},
		{"upper edge is inclusive", "s1", 6, []string{"c1"}},
		{"one past the edge", "s1", 7, nil},
		{"far away", "s1", 9, nil},
		{"other conn's window", "s1", 102, []string{"c2"}},
		{"same band, other sheet", "s2", 4, []string{"c3"}},
		{"unknown sheet", "s9", 4, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Wake(tc.sheetID, tc.band)
			ids := make([]string, 0, len(got))
			for _, c := range got {
				ids = append(ids, c.ID)
			}
			if fmt.Sprint(ids) != fmt.Sprint(append([]string{}, tc.want...)) {
				t.Errorf("Wake(%s, %d) = %v, want %v", tc.sheetID, tc.band, ids, tc.want)
			}
		})
	}
}

// A viewer on band 0 while band 180 changes must cost zero: the subscription
// scope means the wake never even reaches the connection.
func TestDistantBandCostsZeroBytes(t *testing.T) {
	bus := newTestBus(t)
	r := NewRegistry(bus, 0)
	near := mustRegister(t, r, "near", "s1", 0, 4)
	far := mustRegister(t, r, "far", "s1", 178, 182)

	if err := bus.Publish("s1", []int{180}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := bus.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if !awaitWake(t, far, 2*time.Second) {
		t.Fatal("the connection buffered over band 180 was not woken")
	}
	if awaitWake(t, near, 200*time.Millisecond) {
		t.Error("a band 180 write woke a viewer buffered on bands 0..4")
	}
	if s := r.Stats(); s.Wakes != 1 {
		t.Errorf("wakes = %d, want 1 (only the interested connection)", s.Wakes)
	}
	if s := r.Stats(); s.Pushes != 0 {
		t.Errorf("pushes = %d, want 0 (nothing rendered yet)", s.Pushes)
	}
}

func TestDigestSuppression(t *testing.T) {
	r := NewRegistry(newTestBus(t), 0)
	mustRegister(t, r, "c1", "s1", 0, 4)

	payload := []byte(`<div id="grid"><span id="A1">1</span></div>`)

	send, err := r.Deliver("c1", payload)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !send {
		t.Fatal("first payload must be sent")
	}

	// Re-render triggered by an unrelated change in the same band: identical
	// bytes, so it must never reach the wire.
	for i := range 3 {
		send, err := r.Deliver("c1", payload)
		if err != nil {
			t.Fatalf("Deliver %d: %v", i, err)
		}
		if send {
			t.Fatalf("identical payload %d was not suppressed", i)
		}
	}

	// A real change gets through again.
	if send, _ := r.Deliver("c1", []byte(`<div id="grid"><span id="A1">2</span></div>`)); !send {
		t.Error("changed payload must be sent")
	}

	s := r.Stats()
	if s.Renders != 5 {
		t.Errorf("renders = %d, want 5", s.Renders)
	}
	if s.Pushes != 2 {
		t.Errorf("pushes = %d, want 2", s.Pushes)
	}
	if s.Suppressed != 3 {
		t.Errorf("suppressed = %d, want 3", s.Suppressed)
	}
	if want := 3.0 / 5.0; s.SuppressionRate != want {
		t.Errorf("suppression rate = %v, want %v", s.SuppressionRate, want)
	}
}

func TestDigestIsPerConnection(t *testing.T) {
	r := NewRegistry(newTestBus(t), 0)
	mustRegister(t, r, "c1", "s1", 0, 4)
	mustRegister(t, r, "c2", "s1", 0, 4)

	payload := []byte("same bytes")
	if send, _ := r.Deliver("c1", payload); !send {
		t.Fatal("c1 first send")
	}
	if send, _ := r.Deliver("c2", payload); !send {
		t.Error("c2 must still receive its first copy; digests are per connection")
	}
	if _, err := r.Deliver("gone", payload); err == nil {
		t.Error("Deliver to an unknown connection should fail")
	}
}

// A wake inside the throttle window leaves the connection DIRTY and releases it
// when the window closes. It is never dropped: the throttle bounds CPU, not
// correctness. A dropped wake would strand a client on stale HTML forever.
func TestThrottleLeavesConnectionDirtyRatherThanDropping(t *testing.T) {
	const throttleMs = 120
	r := NewRegistry(newTestBus(t), throttleMs)
	c := mustRegister(t, r, "c1", "s1", 0, 4)

	// First wake passes straight through.
	r.Wake("s1", 1)
	if !awaitWake(t, c, time.Second) {
		t.Fatal("first wake should not be throttled")
	}

	// Second wake lands inside the window.
	r.Wake("s1", 2)
	if c.Dirty() != true {
		t.Fatal("a throttled wake must leave the connection dirty")
	}
	if drain(c) != 0 {
		t.Fatal("a throttled wake must not be released immediately")
	}

	// ...and is released once the window closes.
	if !awaitWake(t, c, 2*time.Second) {
		t.Fatal("the throttled wake was dropped instead of deferred")
	}
	if c.Dirty() {
		t.Error("connection should be clean after the deferred release")
	}
	if s := r.Stats(); s.Wakes != 2 {
		t.Errorf("wakes = %d, want 2", s.Wakes)
	}
}

func TestThrottleCollapsesABurstIntoOneRelease(t *testing.T) {
	r := NewRegistry(newTestBus(t), 100)
	c := mustRegister(t, r, "c1", "s1", 0, 4)

	for range 20 {
		r.Wake("s1", 3)
	}
	// One immediate release plus one deferred release for the whole burst.
	if n := drain(c); n != 1 {
		t.Fatalf("immediate releases = %d, want 1", n)
	}
	if !awaitWake(t, c, 2*time.Second) {
		t.Fatal("the burst was dropped instead of deferred")
	}
	if n := drain(c); n != 0 {
		t.Errorf("extra releases = %d, want 0 (the burst must collapse)", n)
	}
	if s := r.Stats(); s.Wakes != 20 {
		t.Errorf("wakes = %d, want 20 (every wake is counted, even when collapsed)", s.Wakes)
	}
}

// Backpressure: a consumer that never reads must not grow a queue. Wakes carry
// no payload, so one pending signal covers any number of changes.
func TestSlowConsumerCoalesces(t *testing.T) {
	r := NewRegistry(newTestBus(t), 0)
	c := mustRegister(t, r, "c1", "s1", 0, 4)

	for range 50 {
		r.Wake("s1", 1)
	}
	if n := drain(c); n != 1 {
		t.Errorf("pending wakes = %d, want 1 (latest-state-wins)", n)
	}
	s := r.Stats()
	if s.Wakes != 50 {
		t.Errorf("wakes = %d, want 50", s.Wakes)
	}
	if s.Coalesced != 49 {
		t.Errorf("coalesced = %d, want 49", s.Coalesced)
	}
}

func TestUnregisterTearsEverythingDown(t *testing.T) {
	bus := newTestBus(t)
	r := NewRegistry(bus, 0)
	c1 := mustRegister(t, r, "c1", "s1", 0, 4)
	mustRegister(t, r, "c2", "s1", 0, 4)

	if s := r.Stats(); s.Connections != 2 || s.Subscriptions != 10 {
		t.Fatalf("before teardown: connections=%d subscriptions=%d, want 2/10",
			s.Connections, s.Subscriptions)
	}

	r.Unregister("c1")
	r.Unregister("c1") // idempotent
	r.Unregister("c2")
	r.Unregister("nope")

	s := r.Stats()
	if s.Connections != 0 {
		t.Errorf("connections = %d, want 0", s.Connections)
	}
	if s.Subscriptions != 0 {
		t.Errorf("subscriptions = %d, want 0 (a leaked subscriber renders for a dead browser)", s.Subscriptions)
	}

	// The wake channel is closed, which is how a render loop learns to exit.
	select {
	case _, open := <-c1.Wakes():
		if open {
			t.Error("wake channel should be closed after Unregister")
		}
	case <-time.After(time.Second):
		t.Error("wake channel was not closed by Unregister")
	}

	// And a publish to a torn-down band must not panic on the closed channel.
	if err := bus.Publish("s1", []int{2}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := bus.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := r.Wake("s1", 2); len(got) != 0 {
		t.Errorf("Wake after teardown returned %d connections, want 0", len(got))
	}
}

// A wake racing an Unregister must not send on a closed channel.
func TestUnregisterRacesWakeSafely(t *testing.T) {
	r := NewRegistry(newTestBus(t), 0)
	for i := range 25 {
		id := fmt.Sprintf("c%d", i)
		mustRegister(t, r, id, "s1", 0, 4)
		go r.Wake("s1", 2)
		go r.Unregister(id)
	}
	time.Sleep(200 * time.Millisecond)
	for i := range 25 {
		r.Unregister(fmt.Sprintf("c%d", i))
	}
	if s := r.Stats(); s.Connections != 0 || s.Subscriptions != 0 {
		t.Errorf("after teardown: connections=%d subscriptions=%d, want 0/0",
			s.Connections, s.Subscriptions)
	}
}
