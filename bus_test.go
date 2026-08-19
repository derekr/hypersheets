package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func newTestBus(t *testing.T) *Bus {
	t.Helper()
	b, err := StartBus(BusOptions{})
	if err != nil {
		t.Fatalf("StartBus: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

func TestSubjectConstruction(t *testing.T) {
	tests := []struct {
		name    string
		sheetID string
		band    int
		want    string
	}{
		{"simple", "s1", 0, "sheet.s1.band.0"},
		{"high band", "s1", 199, "sheet.s1.band.199"},
		{"hyphen and underscore survive", "my-sheet_2", 7, "sheet.my-sheet_2.band.7"},
		{"dot would split the token", "a.b", 1, "sheet.a_b.band.1"},
		{"wildcards are neutralised", "a>b*c", 1, "sheet.a_b_c.band.1"},
		{"space is illegal in a subject", "a b", 1, "sheet.a_b.band.1"},
		{"empty id", "", 3, "sheet._.band.3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BandSubject(tc.sheetID, tc.band); got != tc.want {
				t.Errorf("BandSubject(%q, %d) = %q, want %q", tc.sheetID, tc.band, got, tc.want)
			}
		})
	}

	if got, want := SheetSubject("s1"), "sheet.s1.>"; got != want {
		t.Errorf("SheetSubject = %q, want %q", got, want)
	}
}

func TestParseBandSubject(t *testing.T) {
	tests := []struct {
		subject string
		sheetID string
		band    int
		ok      bool
	}{
		{"sheet.s1.band.0", "s1", 0, true},
		{"sheet.s1.band.180", "s1", 180, true},
		{"sheet.s1.band", "", 0, false},
		{"sheet.s1.band.x", "", 0, false},
		{"sheet.s1", "", 0, false},
		{"other.s1.band.1", "", 0, false},
		{"sheet.s1.row.1", "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.subject, func(t *testing.T) {
			id, band, ok := ParseBandSubject(tc.subject)
			if ok != tc.ok || id != tc.sheetID || band != tc.band {
				t.Errorf("ParseBandSubject(%q) = (%q, %d, %v), want (%q, %d, %v)",
					tc.subject, id, band, ok, tc.sheetID, tc.band, tc.ok)
			}
		})
	}

	t.Run("round trip", func(t *testing.T) {
		id, band, ok := ParseBandSubject(BandSubject("sheet-7", 42))
		if !ok || id != "sheet-7" || band != 42 {
			t.Fatalf("round trip = (%q, %d, %v)", id, band, ok)
		}
	})
}

func TestPublishSubscribeRoundTrip(t *testing.T) {
	bus := newTestBus(t)

	got := make(chan int, 8)
	sub, err := bus.SubscribeBand("s1", 3, func() { got <- 3 })
	if err != nil {
		t.Fatalf("SubscribeBand: %v", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck

	if err := bus.Publish("s1", []int{3}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case band := <-got:
		if band != 3 {
			t.Fatalf("woken for band %d, want 3", band)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no message delivered within 2s")
	}
}

// The broker does the lineage matching: one `>` subscription covers every band
// of the sheet, including bands that did not exist when it subscribed.
func TestWildcardCoversWholeSheet(t *testing.T) {
	bus := newTestBus(t)

	seen := make(chan int, 16)
	sub, err := bus.SubscribeSheet("s1", func(band int) { seen <- band })
	if err != nil {
		t.Fatalf("SubscribeSheet: %v", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck

	want := []int{0, 4, 180}
	if err := bus.Publish("s1", want); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// A different sheet must not reach this subscriber.
	if err := bus.Publish("s2", []int{4}); err != nil {
		t.Fatalf("Publish s2: %v", err)
	}
	if err := bus.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	for _, w := range want {
		select {
		case band := <-seen:
			if band != w {
				t.Fatalf("got band %d, want %d (order should follow publish order)", band, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for band %d", w)
		}
	}
	select {
	case band := <-seen:
		t.Fatalf("wildcard on s1 received an extra band %d (cross-sheet leak)", band)
	case <-time.After(100 * time.Millisecond):
	}
}

// Regression test for the prefix-chain bug described at the top of bus.go: if
// publishes expanded to `sheet.{id}` and subscribers also walked their chain,
// sibling bands would meet at the shared parent and every write would wake
// every subscriber. Publish leaves only; sibling scopes must stay disjoint.
func TestSiblingBandsDoNotMeetAtTheParent(t *testing.T) {
	bus := newTestBus(t)

	var near, far atomic.Int64
	subNear, err := bus.SubscribeBand("s1", 0, func() { near.Add(1) })
	if err != nil {
		t.Fatalf("SubscribeBand 0: %v", err)
	}
	defer subNear.Unsubscribe() //nolint:errcheck
	subFar, err := bus.SubscribeBand("s1", 180, func() { far.Add(1) })
	if err != nil {
		t.Fatalf("SubscribeBand 180: %v", err)
	}
	defer subFar.Unsubscribe() //nolint:errcheck

	// And nothing at all may be published on the bare parent subject.
	var parent atomic.Int64
	subParent, err := bus.Conn().Subscribe("sheet.s1", func(*nats.Msg) { parent.Add(1) })
	if err != nil {
		t.Fatalf("subscribe parent: %v", err)
	}
	defer subParent.Unsubscribe() //nolint:errcheck

	if err := bus.Publish("s1", []int{0}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := bus.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if n := near.Load(); n != 1 {
		t.Errorf("band 0 subscriber woken %d times, want 1", n)
	}
	if n := far.Load(); n != 0 {
		t.Errorf("band 180 subscriber woken %d times for a band 0 write, want 0", n)
	}
	if n := parent.Load(); n != 0 {
		t.Errorf("bare parent subject received %d messages, want 0 (publish leaves only)", n)
	}
}

func TestPublishManyBandsIsOneMessageEach(t *testing.T) {
	bus := newTestBus(t)

	var hits atomic.Int64
	sub, err := bus.SubscribeSheet("s1", func(int) { hits.Add(1) })
	if err != nil {
		t.Fatalf("SubscribeSheet: %v", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck

	// A SUM cascade dirtying three bands.
	if err := bus.Publish("s1", []int{1, 2, 3}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := bus.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if n := hits.Load(); n != 3 {
		t.Errorf("got %d messages for 3 dirty bands, want 3 (no prefix chain)", n)
	}
}

// Core pub/sub only. A stream here would turn a loss-tolerant hint into a
// durable log that has to be acked, trimmed and replayed — SQLite already owns
// the log.
func TestBusHasNoJetStream(t *testing.T) {
	bus := newTestBus(t)
	if bus.ns.JetStreamEnabled() {
		t.Error("embedded NATS must be core only; JetStream is enabled")
	}
	if bus.ns.ClientURL() == "" {
		t.Error("expected an in-process client URL")
	}
}

func TestBusCloseIsClean(t *testing.T) {
	bus, err := StartBus(BusOptions{})
	if err != nil {
		t.Fatalf("StartBus: %v", err)
	}
	if _, err := bus.SubscribeBand("s1", 0, func() {}); err != nil {
		t.Fatalf("SubscribeBand: %v", err)
	}
	bus.Close()
	if bus.Publish("s1", []int{0}) == nil {
		t.Error("publish after Close should fail")
	}
	bus.Close() // idempotent
}

func TestPublishDirtyMapsCellsToBands(t *testing.T) {
	bus := newTestBus(t)

	seen := make(chan int, 16)
	sub, err := bus.SubscribeSheet("s1", func(band int) { seen <- band })
	if err != nil {
		t.Fatalf("SubscribeSheet: %v", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck

	// Two cells in band 0 and one in band 2: three dirty cells, two subjects.
	cells := []CellRef{{Row: 0, Col: 0}, {Row: 10, Col: 1}, {Row: 100, Col: 2}}
	if err := bus.PublishDirty("s1", cells); err != nil {
		t.Fatalf("PublishDirty: %v", err)
	}
	if err := bus.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	var got []int
	for {
		select {
		case b := <-seen:
			got = append(got, b)
			continue
		default:
		}
		break
	}
	want := BandsFor(cells)
	if len(got) != len(want) {
		t.Fatalf("published %v for cells in bands %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("published %v, want %v", got, want)
		}
	}
}
