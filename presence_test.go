package main

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// newPresenceRegistry builds a registry over a real bus and stops the decay
// interval when the test ends, so a test never leaves a ticker behind.
func newPresenceRegistry(t *testing.T, throttleMs int) *Registry {
	t.Helper()
	r := NewRegistry(newTestBus(t), throttleMs)
	t.Cleanup(r.Close)
	return r
}

// drainPresence empties a presence wake channel without blocking.
func drainPresence(c *Conn) int {
	n := 0
	for {
		select {
		case _, ok := <-c.PresenceWakes():
			if !ok {
				return n
			}
			n++
		default:
			return n
		}
	}
}

func awaitPresence(t *testing.T, c *Conn, d time.Duration) bool {
	t.Helper()
	select {
	case <-c.PresenceWakes():
		return true
	case <-time.After(d):
		return false
	}
}

// ─── Identity ─────────────────────────────────────────────────────────────────

// The NAME stays a pure function of the connection id: that is what makes it
// need no storage and stay fixed for a session. (The COLOUR deliberately is not
// — see the assignment tests below — which is why this no longer asserts
// anything about who gets which hue.)
func TestNameIsDeterministic(t *testing.T) {
	for _, id := range []string{"a1b2c3d4", "", "0000000000000000", "zzz"} {
		first := nameFor(id)
		for range 5 {
			if got := nameFor(id); got != first {
				t.Errorf("nameFor(%q) = %q then %q; must be stable", id, first, got)
			}
		}
		if first == "" {
			t.Errorf("nameFor(%q) produced an empty name", id)
		}
	}
}

// identityFor is the FALLBACK identity — the one used for an edit whose author
// is no longer connected, or was never a connection at all. It has to be stable
// and well formed; it does NOT have to be unique, which is the whole reason
// assignment exists for everybody who IS in the room.
func TestFallbackIdentityIsDeterministic(t *testing.T) {
	for _, id := range []string{"a1b2c3d4", "", "0000000000000000", "zzz"} {
		first := identityFor(id)
		for range 5 {
			if got := identityFor(id); got != first {
				t.Errorf("identityFor(%q) = %+v then %+v; must be stable", id, first, got)
			}
		}
		if first.Name == "" || first.Color == "" {
			t.Errorf("identityFor(%q) produced an empty identity: %+v", id, first)
		}
	}
}

func TestFallbackIdentityIsDistributedAndWellFormed(t *testing.T) {
	names := map[string]int{}
	hues := map[int]int{}
	for i := range 2000 {
		id := identityFor(fmt.Sprintf("conn-%04d", i))
		names[id.Name]++
		hues[id.Hue]++

		var known bool
		for _, h := range presenceHues {
			if h == id.Hue {
				known = true
			}
		}
		if !known {
			t.Fatalf("hue %d is not on the wheel", id.Hue)
		}
		if want := fmt.Sprintf("hsl(%d 68%% 45%%)", id.Hue); id.Color != want {
			t.Fatalf("Color = %q, want %q", id.Color, want)
		}
	}
	// 32x32 names: 2,000 draws from 1,024 slots reaches ~877 of them if the
	// distribution is flat, so the threshold is the birthday expectation and not
	// the slot count.
	if len(names) < 800 {
		t.Errorf("distinct names over 2000 ids = %d, want ~877 of 1024", len(names))
	}
	if len(hues) != len(presenceHues) {
		t.Errorf("distinct hues = %d, want %d", len(hues), len(presenceHues))
	}
}

// THE WHEEL MUST NOT CONTAIN THE UI'S BLUE. #1a73e8 is hue 214 and it is what
// "this cell is selected" looks like; a collaborator wearing it would draw what
// reads as the viewer's own cursor on somebody else's row, which is worse than
// two viewers sharing a colour.
func TestTheWheelAvoidsTheSelectionBlue(t *testing.T) {
	for _, h := range presenceHues {
		if d := hueDistance(h, 214); d < 18 {
			t.Errorf("hue %d is %d degrees from the active-cell blue", h, d)
		}
	}
}

func TestHueDistanceGoesTheShortWayRound(t *testing.T) {
	for _, tc := range []struct{ a, b, want int }{
		{0, 0, 0}, {0, 15, 15}, {15, 0, 15}, {0, 180, 180}, {0, 181, 179},
		{350, 10, 20}, {10, 350, 20}, {345, 0, 15}, {90, 270, 180},
	} {
		if got := hueDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("hueDistance(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// ─── Assignment: the colour comes from the room ───────────────────────────────

// minSeparation is the worst pair in a set of hues — the number a person is
// actually judging when they glance at two cursors. An average would hide
// exactly the failure being tested.
func minSeparation(hues []int) int {
	worst := 360
	for i := range hues {
		for j := i + 1; j < len(hues); j++ {
			if d := hueDistance(hues[i], hues[j]); d < worst {
				worst = d
			}
		}
	}
	return worst
}

// joinRoom plays n joins through the assignment and returns the hues in the
// room, in join order. It is pickHue's own contract — the registry supplies the
// `used` map from its live connections and nothing else.
func joinRoom(prefix string, n int) []int {
	used := map[int]int{}
	hues := make([]int, 0, n)
	for i := range n {
		h := pickHue(used, fmt.Sprintf("%s-%03d", prefix, i))
		used[h]++
		hues = append(hues, h)
	}
	return hues
}

// huesOn is every live viewer's hue on a sheet, by connection id.
func huesOn(r *Registry, sheetID string) map[string]int {
	out := map[string]int{}
	for _, u := range r.Presence(sheetID) {
		out[u.ConnID] = u.Hue
	}
	return out
}

func huesOnly(r *Registry, sheetID string) []int {
	var out []int
	for _, u := range r.Presence(sheetID) {
		out = append(out, u.Hue)
	}
	return out
}

// THE BUG, AS A TEST. Two sessions of one sheet used to be able to land on the
// same hue (birthday odds over a twelve-entry palette) or — far more often — on
// neighbouring ones, which looks the same. The second viewer now takes the far
// side of the wheel: half a turn away, or as close to it as the wheel allows
// when the exact antipode is the arc reserved around the UI's blue.
func TestTwoViewersLandOnOppositeSidesOfTheWheel(t *testing.T) {
	for i := range 200 {
		hues := joinRoom(fmt.Sprintf("room%03d", i), 2)
		if got := minSeparation(hues); got < 150 {
			t.Fatalf("two viewers landed %d degrees apart (%v), want ~180", got, hues)
		}
	}

	// And through the registry, which is where the `used` map actually comes
	// from: two connections, one sheet.
	r := newPresenceRegistry(t, 0)
	for i := range 25 {
		sheet := fmt.Sprintf("s%02d", i)
		mustRegister(t, r, fmt.Sprintf("a%02d", i), sheet, 0, 4)
		mustRegister(t, r, fmt.Sprintf("b%02d", i), sheet, 0, 4)
		if got := minSeparation(huesOnly(r, sheet)); got < 150 {
			t.Fatalf("two viewers of %s are %d degrees apart (%v), want ~180",
				sheet, got, huesOn(r, sheet))
		}
	}
}

// THE GUARANTEE IS FOR THE WORST PAIR IN THE ROOM, AND IT IS HALF OF EVEN.
// Two viewers land 180 apart, three and four land 90 apart, five through eight
// 45 or 30, and everyone up to a full wheel still gets a hue of their own.
//
// The halving is not a defect in the search, it is the price of the property
// that matters more: A VIEWER'S COLOUR NEVER CHANGES WHILE THEY ARE LOOKING.
// Perfectly even spacing for three would mean moving the second viewer from 180
// to 120 when the third arrives, i.e. recolouring somebody mid-session to
// optimise for a person who has just walked in. Placing each joiner in the
// largest remaining gap and never touching anyone else is farthest-point
// insertion, whose bound is exactly half the even spacing — so that, floored at
// the wheel's own 15-degree step, is what this asserts.
func TestHuesSpreadAcrossTheWholeRoom(t *testing.T) {
	for _, n := range []int{2, 3, 4, 5, 6, 8, 12, 16, 22} {
		want := max(15, 180/n)
		for i := range 20 {
			hues := joinRoom(fmt.Sprintf("n%02d-%02d", n, i), n)
			if got := minSeparation(hues); got < want {
				t.Errorf("%d viewers: worst pair %d degrees apart, want >= %d (even would be %d): %v",
					n, got, want, 360/n, hues)
			}
		}
	}

	// Everyone up to a full wheel gets a distinct hue, through the registry.
	r := newPresenceRegistry(t, 0)
	for i := range len(presenceHues) {
		mustRegister(t, r, fmt.Sprintf("c%03d", i), "s1", 0, 4)
	}
	distinct := map[int]bool{}
	for _, h := range huesOnly(r, "s1") {
		distinct[h] = true
	}
	if len(distinct) != len(presenceHues) {
		t.Errorf("%d viewers hold %d distinct hues, want %d",
			len(presenceHues), len(distinct), len(presenceHues))
	}
}

// DETERMINISTIC, NOT MAP ORDER. `used` is a map, and Go randomises its
// iteration, so an assignment that depended on that order would be
// unreproducible and a bug in it unreportable.
func TestHueAssignmentIsReproducible(t *testing.T) {
	used := map[int]int{0: 1, 90: 1, 180: 2, 345: 1}
	first := pickHue(used, "joiner")
	for range 200 {
		if got := pickHue(used, "joiner"); got != first {
			t.Fatalf("pickHue over the same room gave %d then %d", first, got)
		}
	}
	// Same room, different joiner: still a pure function of the two.
	if a, b := pickHue(used, "someone-else"), pickHue(used, "someone-else"); a != b {
		t.Errorf("pickHue is not stable for a given id: %d then %d", a, b)
	}

	// The whole room replays identically, and through the registry too.
	want := joinRoom("replay", 9)
	for range 5 {
		if got := joinRoom("replay", 9); !slices.Equal(got, want) {
			t.Fatalf("replaying nine joins gave %v then %v", want, got)
		}
	}
	r := newPresenceRegistry(t, 0)
	for i := range 9 {
		mustRegister(t, r, fmt.Sprintf("replay-%03d", i), "s1", 0, 4)
	}
	for id, hue := range huesOn(r, "s1") {
		i := 0
		if _, err := fmt.Sscanf(id, "replay-%03d", &i); err != nil {
			t.Fatal(err)
		}
		if hue != want[i] {
			t.Errorf("%s got hue %d through the registry, %d through pickHue", id, hue, want[i])
		}
	}
}

// The tie-break is the CONNECTION ID and not the wheel's order, so the first
// viewer of a sheet is not always handed the same red — an empty room is a
// twenty-two-way tie and the id breaks it.
func TestTheFirstViewerIsNotAlwaysTheSameColour(t *testing.T) {
	seen := map[int]bool{}
	for i := range 60 {
		seen[pickHue(map[int]int{}, fmt.Sprintf("first-%03d", i))] = true
	}
	if len(seen) < 10 {
		t.Errorf("first-viewer hues over 60 sheets = %d distinct, want them spread over the wheel", len(seen))
	}
}

// A LEAVER RETURNS ITS HUE AND DISTURBS NOBODY. Colour is stable for the life of
// a connection, so the freed hue is only available to the NEXT joiner; nobody
// still in the room is recoloured to take advantage of it.
func TestLeavingReleasesItsHueWithoutMovingAnyone(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	for _, id := range []string{"c1", "c2", "c3"} {
		mustRegister(t, r, id, "s1", 0, 4)
	}
	before := huesOn(r, "s1")

	r.Unregister("c2")
	after := huesOn(r, "s1")
	if len(after) != 2 {
		t.Fatalf("presence after a leave = %d viewers, want 2", len(after))
	}
	for _, id := range []string{"c1", "c3"} {
		if after[id] != before[id] {
			t.Errorf("%s moved from hue %d to %d when someone else left", id, before[id], after[id])
		}
	}

	// The freed hue is back in the pool: the next joiner is placed against the
	// two viewers who are here, not against the ghost of the one who left.
	mustRegister(t, r, "c4", "s1", 0, 4)
	hues := huesOn(r, "s1")
	for _, id := range []string{"c1", "c3"} {
		if hues[id] != before[id] {
			t.Errorf("%s moved from hue %d to %d when someone joined", id, before[id], hues[id])
		}
	}
	if got := minSeparation(huesOnly(r, "s1")); got < 60 {
		t.Errorf("after a leave and a join the worst pair is %d degrees apart: %v", got, hues)
	}
	if hues["c4"] != before["c2"] {
		// Not required, but it is the honest expectation: three viewers, one
		// gap, and the gap is where the leaver was.
		t.Logf("c4 took hue %d; c2 had freed %d", hues["c4"], before["c2"])
	}
}

// MORE VIEWERS THAN HUES DEGRADES, IT DOES NOT CLUSTER. Every hue is spent
// before any is reused, and reuse stays within one of even — the room becomes
// "two people share a colour", never "four people share one while eight hues go
// unused".
func TestMoreViewersThanHuesReuseEvenly(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	for i := range 60 {
		mustRegister(t, r, fmt.Sprintf("c%02d", i), "s1", 0, 4)
	}
	counts := map[int]int{}
	for _, hue := range huesOn(r, "s1") {
		counts[hue]++
	}
	if len(counts) != len(presenceHues) {
		t.Errorf("60 viewers used %d of %d hues; every hue should be spent before any is reused",
			len(counts), len(presenceHues))
	}
	lo, hi := 60, 0
	for _, h := range presenceHues {
		n := counts[h]
		lo, hi = min(lo, n), max(hi, n)
	}
	if hi-lo > 1 {
		t.Errorf("hue use ranges from %d to %d viewers, want it within one: %v", lo, hi, counts)
	}
}

// PER SHEET. Two viewers who can never appear on the same screen must not spend
// each other's wheel: a second sheet starts from an empty room.
func TestSheetsDoNotConstrainEachOther(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	for i := range len(presenceHues) {
		mustRegister(t, r, fmt.Sprintf("a%03d", i), "s1", 0, 4)
	}
	// s1 has now spent the whole wheel. s2 must still place its first two
	// viewers as far apart as an empty room allows.
	mustRegister(t, r, "b1", "s2", 0, 4)
	mustRegister(t, r, "b2", "s2", 0, 4)
	if got := minSeparation(huesOnly(r, "s2")); got < 150 {
		t.Errorf("two viewers of s2 are %d degrees apart while s1 is full, want ~180: %v",
			got, huesOn(r, "s2"))
	}
	if n := len(huesOn(r, "s1")); n != len(presenceHues) {
		t.Errorf("s1 has %d viewers, want %d", n, len(presenceHues))
	}
}

// The colour a viewer is given must not change under them. Joins, leaves and
// selection commits by other people all invalidate presence; none of them may
// re-assign.
func TestAColourIsStableForTheLifeOfAConnection(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	c := mustRegister(t, r, "c1", "s1", 0, 4)
	mine := huesOn(r, "s1")["c1"]

	for i := range 10 {
		mustRegister(t, r, fmt.Sprintf("other%02d", i), "s1", 0, 4)
	}
	if err := r.SetSelection("c1", CellRef{Row: 1, Col: 1}, selRange{}); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		r.Unregister(fmt.Sprintf("other%02d", i))
	}
	if got := huesOn(r, "s1")["c1"]; got != mine {
		t.Errorf("c1's hue changed from %d to %d while it was connected", mine, got)
	}
	if u, ok := r.PresenceOf("c1"); !ok || u.Hue != mine || u.Color != hueColor(mine) {
		t.Errorf("PresenceOf disagrees with Presence: %+v, want hue %d", u, mine)
	}
	if c.hue != mine || c.color != hueColor(mine) {
		t.Errorf("the conn holds hue %d / %q, the read model reports %d", c.hue, c.color, mine)
	}
}

// Two viewers with the same derived name in one sheet must degrade, not
// collide and not crash.
func TestDisambiguateHandlesCollisions(t *testing.T) {
	users := []PresenceUser{
		{ConnID: "a", Name: "Amber Otter"},
		{ConnID: "b", Name: "Amber Otter"},
		{ConnID: "c", Name: "Amber Otter"},
		{ConnID: "d", Name: "Teal Yak"},
	}
	disambiguate(users)
	want := []string{"Amber Otter", "Amber Otter 2", "Amber Otter 3", "Teal Yak"}
	for i, u := range users {
		if u.Name != want[i] {
			t.Errorf("users[%d].Name = %q, want %q", i, u.Name, want[i])
		}
	}

	// And the real read model must not produce two identical names either.
	r := newPresenceRegistry(t, 0)
	for i := range 60 {
		mustRegister(t, r, fmt.Sprintf("c%02d", i), "s1", 0, 4)
	}
	seen := map[string]string{}
	for _, u := range r.Presence("s1") {
		if prev, dup := seen[u.Name]; dup {
			t.Errorf("name %q used by both %s and %s", u.Name, prev, u.ConnID)
		}
		seen[u.Name] = u.ConnID
	}
}

// ─── The read model ───────────────────────────────────────────────────────────

func TestPresenceReportsViewersPerSheet(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	mustRegister(t, r, "c1", "s1", 0, 4)
	mustRegister(t, r, "c2", "s1", 100, 104) // different buffer, same room
	mustRegister(t, r, "c3", "s2", 0, 4)

	got := r.Presence("s1")
	if len(got) != 2 {
		t.Fatalf("presence on s1 = %d viewers, want 2", len(got))
	}
	if got[0].ConnID != "c1" || got[1].ConnID != "c2" {
		t.Errorf("presence order = %s,%s, want c1,c2 (stable by conn id)", got[0].ConnID, got[1].ConnID)
	}
	if r.PresenceCount("s2") != 1 {
		t.Errorf("presence count on s2 = %d, want 1", r.PresenceCount("s2"))
	}
	if r.PresenceCount("nobody") != 0 {
		t.Error("an unknown sheet must have no viewers")
	}

	for _, u := range r.PresenceFor("s1", "c2") {
		if want := u.ConnID == "c2"; u.Self != want {
			t.Errorf("%s Self = %v, want %v", u.ConnID, u.Self, want)
		}
	}

	if _, ok := r.PresenceOf("c1"); !ok {
		t.Error("PresenceOf(c1) should resolve")
	}
	if _, ok := r.PresenceOf("gone"); ok {
		t.Error("PresenceOf of an unknown connection must not resolve")
	}
}

// A join and a leave must invalidate the presence topic. This is the whole
// claim: invalidation from connection lifecycle, with no write anywhere.
func TestJoinAndLeaveInvalidatePresence(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	held := mustRegister(t, r, "held", "s1", 0, 4)
	if !awaitPresence(t, held, 2*time.Second) {
		t.Fatal("a connection's own join did not invalidate presence")
	}
	drainPresence(held)

	// Someone else joins.
	other := mustRegister(t, r, "other", "s1", 0, 4)
	if !awaitPresence(t, held, 2*time.Second) {
		t.Fatal("a join did not invalidate presence for the existing viewer")
	}
	if n := len(r.Presence("s1")); n != 2 {
		t.Fatalf("presence after join = %d, want 2", n)
	}
	drainPresence(held)
	_ = other

	// ...and leaves.
	r.Unregister("other")
	if !awaitPresence(t, held, 2*time.Second) {
		t.Fatal("a leave did not invalidate presence for the remaining viewer")
	}
	if n := len(r.Presence("s1")); n != 1 {
		t.Fatalf("presence after leave = %d, want 1", n)
	}
}

// A leaked presence entry is a ghost user in the avatar list. The read model IS
// the connection map, so teardown cannot leave one — this pins that.
func TestTeardownLeavesNoGhosts(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	ids := make([]string, 0, 20)
	for i := range 20 {
		id := fmt.Sprintf("c%02d", i)
		ids = append(ids, id)
		c := mustRegister(t, r, id, "s1", 0, 4)
		if err := r.SetSelection(id, CellRef{Row: i, Col: 1}, selRange{}); err != nil {
			t.Fatalf("SetSelection: %v", err)
		}
		_ = c
	}
	if n := len(r.Presence("s1")); n != 20 {
		t.Fatalf("presence = %d, want 20", n)
	}

	for _, id := range ids {
		r.Unregister(id)
		r.Unregister(id) // idempotent
	}

	if n := len(r.Presence("s1")); n != 0 {
		t.Errorf("presence after teardown = %d, want 0 (ghosts in the avatar list)", n)
	}
	if n := r.PresenceCount("s1"); n != 0 {
		t.Errorf("presence count after teardown = %d, want 0", n)
	}
	if s := r.Stats(); s.Connections != 0 || s.Subscriptions != 0 {
		t.Errorf("registry stats after teardown = %d/%d, want 0/0", s.Connections, s.Subscriptions)
	}
	// A selection commit for a dead connection is an error, not a resurrection.
	if err := r.SetSelection("c00", CellRef{}, selRange{}); err == nil {
		t.Error("SetSelection on a torn-down connection should fail")
	}
	// And the presence channel is closed, so a render loop's select terminates.
	c, _ := r.Conn("c00")
	if c != nil {
		t.Fatal("connection should be gone from the registry")
	}
}

func TestUnregisterClosesPresenceChannel(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	c := mustRegister(t, r, "c1", "s1", 0, 4)
	r.Unregister("c1")
	select {
	case _, open := <-c.PresenceWakes():
		if open {
			t.Error("presence channel should be closed after Unregister")
		}
	case <-time.After(time.Second):
		t.Error("presence channel was not closed by Unregister")
	}
	// A late presence publish must not panic on the closed channel.
	r.presence.invalidate("s1")
	time.Sleep(200 * time.Millisecond)
}

// ─── The two topics must not cross ────────────────────────────────────────────

// A presence change must not wake a band subscriber, and an edit must not wake a
// presence subscriber. Two subjects, no prefix relationship, and this is the
// test that says so out loud — the sibling-scope failure bus.go documents was
// silent when it happened.
func TestPresenceAndBandsDoNotWakeEachOther(t *testing.T) {
	bus := newTestBus(t)
	r := NewRegistry(bus, 0)
	t.Cleanup(r.Close)

	c := mustRegister(t, r, "c1", "s1", 0, 4)
	// Settle the join's own presence invalidation.
	if !awaitPresence(t, c, 2*time.Second) {
		t.Fatal("join did not invalidate presence")
	}
	drain(c)
	drainPresence(c)
	wakesBefore := r.Stats().Wakes

	// A presence change: a viewer commits a selection.
	if err := r.SetSelection("c1", CellRef{Row: 7, Col: 3}, selRange{}); err != nil {
		t.Fatalf("SetSelection: %v", err)
	}
	if !awaitPresence(t, c, 2*time.Second) {
		t.Fatal("a selection commit did not invalidate presence")
	}
	if awaitWake(t, c, 300*time.Millisecond) {
		t.Error("a presence change woke a BAND subscriber — the subjects are crossing")
	}
	if got := r.Stats().Wakes; got != wakesBefore {
		t.Errorf("band wakes = %d, want %d (a presence change must not count as one)", got, wakesBefore)
	}

	// A band change: an edit.
	drainPresence(c)
	if err := bus.Publish("s1", []int{2}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !awaitWake(t, c, 2*time.Second) {
		t.Fatal("a band publish did not wake the band subscriber")
	}
	if awaitPresence(t, c, 300*time.Millisecond) {
		t.Error("an edit woke a PRESENCE subscriber — the subjects are crossing")
	}
}

// NoteEdit records attribution but must not publish presence: the edit's own
// band subjects already wake exactly the viewers who can see the cell.
func TestNoteEditDoesNotInvalidatePresence(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	c := mustRegister(t, r, "c1", "s1", 0, 4)
	if !awaitPresence(t, c, 2*time.Second) {
		t.Fatal("join did not invalidate presence")
	}
	drainPresence(c)
	before := r.Stats().PresencePublishes

	r.NoteEdit("s1", "someone-else", []CellRef{{Row: 1, Col: 1}, {Row: 2, Col: 2}})

	if awaitPresence(t, c, 400*time.Millisecond) {
		t.Error("NoteEdit invalidated presence; the band publish is what wakes viewers")
	}
	if got := r.Stats().PresencePublishes; got != before {
		t.Errorf("presence publishes = %d, want %d", got, before)
	}
	if len(r.FlashSet("s1", "c1")) != 2 {
		t.Error("the attribution should still be readable")
	}
}

func TestPresenceSubjectIsNotABandSubject(t *testing.T) {
	if _, _, ok := ParseBandSubject(PresenceSubject("s1")); ok {
		t.Error("the presence subject must not parse as a band subject")
	}
	if got, want := PresenceSubject("s1"), "sheet.s1.presence"; got != want {
		t.Errorf("PresenceSubject = %q, want %q", got, want)
	}
	if PresenceSubject("s1") == BandSubject("s1", 0) {
		t.Error("presence and band 0 must be different subjects")
	}
}

// ─── Selection ───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────

func TestSetSelectionStoresCellAndRange(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	mustRegister(t, r, "c1", "s1", 0, 4)

	sel := selRange{Lo: CellRef{Row: 9, Col: 3}, Hi: CellRef{Row: 2, Col: 1}, On: true}
	if err := r.SetSelection("c1", CellRef{Row: 2, Col: 1}, sel); err != nil {
		t.Fatalf("SetSelection: %v", err)
	}
	got := r.Presence("s1")[0].Sel
	if !got.On || got.Cell != (CellRef{Row: 2, Col: 1}) {
		t.Errorf("active cell = %+v, want B3", got)
	}
	// The corners are normalized, whichever way the drag went.
	if got.Range.Lo != (CellRef{Row: 2, Col: 1}) || got.Range.Hi != (CellRef{Row: 9, Col: 3}) {
		t.Errorf("range = %+v, want B3:D10 normalized", got.Range)
	}
	if got.At.IsZero() {
		t.Error("a selection should carry a timestamp")
	}

	if err := r.SetSelection("nobody", CellRef{}, selRange{}); err == nil {
		t.Error("SetSelection on an unknown connection should fail")
	}
	if err := r.SetSelection("c1", CellRef{Row: -1}, selRange{}); err == nil {
		t.Error("an unaddressable cell should be refused")
	}

	r.ClearSelection("c1")
	if r.Presence("s1")[0].Sel.On {
		t.Error("ClearSelection should drop the held selection")
	}
}

func TestSetSelectionRefsIsTheHTTPSeam(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	mustRegister(t, r, "c1", "s1", 0, 4)

	tests := []struct {
		name     string
		cell     string
		sel      string
		wantErr  bool
		wantCell CellRef
		wantSel  selRange
	}{
		{name: "bare cell", cell: "D7", wantCell: CellRef{Row: 6, Col: 3}},
		{
			name: "range", cell: "A1", sel: "A1:D20",
			wantCell: CellRef{},
			wantSel:  selRange{Lo: CellRef{}, Hi: CellRef{Row: 19, Col: 3}, On: true},
		},
		{
			name: "single ref as a one-cell range", cell: "B2", sel: "B2",
			wantCell: CellRef{Row: 1, Col: 1},
			wantSel:  selRange{Lo: CellRef{Row: 1, Col: 1}, Hi: CellRef{Row: 1, Col: 1}, On: true},
		},
		{name: "bad cell", cell: "A0", wantErr: true},
		{name: "bad range", cell: "A1", sel: "A1:", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := r.SetSelectionRefs("c1", tc.cell, tc.sel)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("SetSelectionRefs: %v", err)
			}
			pos := r.Presence("s1")[0].Sel
			if pos.Cell != tc.wantCell || pos.Range != tc.wantSel {
				t.Errorf("pos = %+v/%+v, want %+v/%+v", pos.Cell, pos.Range, tc.wantCell, tc.wantSel)
			}
		})
	}
}

// The render path reads a screen's own selection back out of the registry —
// that is the whole reason selection is server state rather than four client
// signals, so it has its own test.
func TestSelectionIsReadableBackByTheRenderPath(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	mustRegister(t, r, "c1", "s1", 0, 4)

	if sel, ok := r.Selection("c1"); !ok || sel.On {
		t.Errorf("a fresh connection should hold an empty selection, got %+v ok=%v", sel, ok)
	}
	if _, ok := r.Selection("nobody"); ok {
		t.Error("an unknown connection must not resolve")
	}

	rng := selRange{Lo: CellRef{Row: 2, Col: 1}, Hi: CellRef{Row: 9, Col: 3}, On: true}
	if err := r.SetSelection("c1", CellRef{Row: 2, Col: 1}, rng); err != nil {
		t.Fatalf("SetSelection: %v", err)
	}

	sel, ok := r.Selection("c1")
	if !ok || !sel.On || sel.Range != rng {
		t.Fatalf("read back %+v ok=%v, want the committed range", sel, ok)
	}
	// The renderer marks cells with it, using the same contains() the linked
	// region already uses.
	if !sel.Range.contains(5, 2) || sel.Range.contains(5, 9) {
		t.Error("the read-back range does not answer contains() correctly")
	}
	// And it is the SAME copy presence reports, not a second one.
	if got := r.Presence("s1")[0].Sel; got != sel {
		t.Errorf("presence reports %+v, render path reads %+v; there must be one copy", got, sel)
	}
	// It dies with the connection.
	r.Unregister("c1")
	if _, ok := r.Selection("c1"); ok {
		t.Error("a selection must not outlive its connection")
	}
}

// THE SAFETY CAP, EXERCISED. Real clients commit on settle, so this rate never
// happens; the point of the test is that if one ever regressed to committing per
// pointermove it would still be bounded, and bounded per SHEET — which is what
// keeps the fan-out linear rather than quadratic in viewers.
func TestSelectionPublishesAreRateCapped(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	const conns = 10
	for i := range conns {
		mustRegister(t, r, fmt.Sprintf("c%02d", i), "s1", 0, 4)
	}
	time.Sleep(2 * presencePublishEvery)
	before := r.Stats().PresencePublishes

	// 10 connections x 100 reports each, as fast as the machine will do it.
	const burst = 100
	start := time.Now()
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("c%02d", i)
			for j := range burst {
				_ = r.SetSelection(id, CellRef{Row: j % 500, Col: i % 26}, selRange{})
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	// Let the deferred publish for the tail of the burst land.
	time.Sleep(3 * presencePublishEvery)
	got := r.Stats().PresencePublishes - before

	// The bound: one publish per throttle window, plus one for the immediate
	// first and one for the deferred tail. 1,000 reports must not be anywhere
	// near 1,000 publishes.
	ceiling := uint64(elapsed/presencePublishEvery) + 4
	if got > ceiling {
		t.Errorf("publishes = %d for %d reports over %v, want <= %d",
			got, conns*burst, elapsed, ceiling)
	}
	if got == 0 {
		t.Error("the burst produced no publish at all; it must not be dropped")
	}
	t.Logf("%d selection commits from %d connections over %v -> %d presence publishes",
		conns*burst, conns, elapsed.Round(time.Millisecond), got)

	// COALESCED, NOT DROPPED: the last selection of the burst is what the read
	// model reports.
	for i := range conns {
		id := fmt.Sprintf("c%02d", i)
		var pos Selection
		for _, u := range r.Presence("s1") {
			if u.ConnID == id {
				pos = u.Sel
			}
		}
		want := CellRef{Row: (burst - 1) % 500, Col: i % 26}
		if pos.Cell != want {
			t.Errorf("%s final selection = %+v, want %+v (the last report must survive)", id, pos.Cell, want)
		}
	}
}

// A repeated identical selection is not a change and must cost nothing.
func TestRepeatedSelectionCostsNoPublish(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	mustRegister(t, r, "c1", "s1", 0, 4)
	time.Sleep(2 * presencePublishEvery)

	if err := r.SetSelection("c1", CellRef{Row: 3, Col: 3}, selRange{}); err != nil {
		t.Fatalf("SetSelection: %v", err)
	}
	time.Sleep(2 * presencePublishEvery)
	before := r.Stats().PresencePublishes

	for range 50 {
		if err := r.SetSelection("c1", CellRef{Row: 3, Col: 3}, selRange{}); err != nil {
			t.Fatalf("SetSelection: %v", err)
		}
	}
	time.Sleep(2 * presencePublishEvery)
	if got := r.Stats().PresencePublishes; got != before {
		t.Errorf("publishes = %d, want %d (a repeated selection is not a change)", got, before)
	}
}

// The deferred publish must actually happen: a throttled invalidation is
// deferred, never dropped, exactly like a throttled wake.
func TestThrottledPresenceIsDeferredNotDropped(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	c := mustRegister(t, r, "c1", "s1", 0, 4)
	if !awaitPresence(t, c, 2*time.Second) {
		t.Fatal("join did not invalidate presence")
	}
	drainPresence(c)

	// Immediately after the join, inside the window.
	if err := r.SetSelection("c1", CellRef{Row: 1, Col: 1}, selRange{}); err != nil {
		t.Fatalf("SetSelection: %v", err)
	}
	if !awaitPresence(t, c, 2*time.Second) {
		t.Fatal("a throttled presence invalidation was dropped instead of deferred")
	}
	if r.Presence("s1")[0].Sel.Cell != (CellRef{Row: 1, Col: 1}) {
		t.Error("the deferred publish must carry the latest selection")
	}
}

// ─── Attribution ──────────────────────────────────────────────────────────────

func TestFlashesExcludeTheViewersOwnEdits(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	mustRegister(t, r, "mine", "s1", 0, 4)
	mustRegister(t, r, "theirs", "s1", 0, 4)

	r.NoteEdit("s1", "mine", []CellRef{{Row: 0, Col: 0}})
	r.NoteEdit("s1", "theirs", []CellRef{{Row: 1, Col: 1}, {Row: 2, Col: 2}})

	mine := r.FlashSet("s1", "mine")
	if len(mine) != 2 {
		t.Fatalf("flashes for the author of A1 = %d, want 2 (only the other's)", len(mine))
	}
	if _, self := mine[CellRef{Row: 0, Col: 0}]; self {
		t.Error("a viewer must not be shown a flash for their own edit")
	}

	theirs := r.FlashSet("s1", "theirs")
	if len(theirs) != 1 {
		t.Fatalf("flashes for the author of B2/C3 = %d, want 1", len(theirs))
	}

	// The colour is the author's ASSIGNED colour — the one their cursor is
	// wearing on this sheet right now — and not a hash of their id.
	author, ok := r.PresenceOf("mine")
	if !ok {
		t.Fatal("mine should be present")
	}
	f := theirs[CellRef{Row: 0, Col: 0}]
	if f.ConnID != "mine" || f.Hue != author.Hue || f.Color != author.Color {
		t.Errorf("flash = %+v, want it attributed to mine in %s", f, author.Color)
	}

	// AN ATTRIBUTION OUTLIVES ITS AUTHOR'S CONNECTION. It used to do so because
	// the colour was a pure function of the id; now it does so because NoteEdit
	// SNAPSHOTTED who they were, which is the more honest model — the flash is a
	// statement about a past event and carries the colour that event appeared
	// in, even though the connection it belonged to is gone.
	r.Unregister("mine")
	got := r.FlashSet("s1", "theirs")
	if len(got) != 1 {
		t.Fatalf("flashes after the author left = %d, want 1", len(got))
	}
	gone := got[CellRef{Row: 0, Col: 0}]
	if gone.Name != author.Name || gone.Hue != author.Hue || gone.Color != author.Color {
		t.Errorf("flash after the author left = %+v, want the identity it was noted with (%+v)", gone, author)
	}

	// Flashes is the same set, newest first.
	if n := len(r.Flashes("s1", "mine")); n != 2 {
		t.Errorf("Flashes = %d, want 2", n)
	}
	if r.Flashes("s1", "nobody-here") == nil {
		t.Error("a viewer who wrote nothing should see every flash")
	}
	if got := r.FlashSet("other-sheet", "mine"); got != nil {
		t.Error("attribution must be per sheet")
	}
}

// The decay: the read model goes stale on a timer with no write behind it, and
// the interval publishes the topic so a viewer re-resolves and drops the flash.
func TestAttributionDecays(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	c := mustRegister(t, r, "c1", "s1", 0, 4)
	if !awaitPresence(t, c, 2*time.Second) {
		t.Fatal("join did not invalidate presence")
	}

	r.NoteEdit("s1", "author", []CellRef{{Row: 0, Col: 0}})
	if len(r.FlashSet("s1", "c1")) != 1 {
		t.Fatal("the flash should be live immediately")
	}

	// The read model stops reporting it once the TTL passes...
	drainPresence(c)
	deadline := time.Now().Add(attributionTTL + 3*presenceSweepEvery + time.Second)
	var gone, invalidated bool
	for time.Now().Before(deadline) {
		if !gone && len(r.FlashSet("s1", "c1")) == 0 {
			gone = true
		}
		select {
		case <-c.PresenceWakes():
			// ...and the decay INVALIDATES, with no write behind it.
			if gone {
				invalidated = true
			}
		case <-time.After(20 * time.Millisecond):
		}
		if gone && invalidated {
			break
		}
	}
	if !gone {
		t.Fatalf("a flash was still reported %v after the edit", attributionTTL)
	}
	if !invalidated {
		t.Error("the decay did not publish the presence topic; the flash would linger on screen")
	}
}

// Bounded: attribution must not grow with the sheet.
func TestAttributionIsBounded(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	mustRegister(t, r, "viewer", "s1", 0, 4)

	// Far more cells than the ring holds, and more than any one command may
	// write (maxClearCells is 2,000).
	cells := make([]CellRef, 0, 5000)
	for i := range 5000 {
		cells = append(cells, CellRef{Row: i, Col: i % 26})
	}
	r.NoteEdit("s1", "bulk", cells)

	got := r.FlashSet("s1", "viewer")
	if len(got) > attributionMax {
		t.Errorf("attribution held %d cells, want <= %d", len(got), attributionMax)
	}
	if len(got) != attributionMax {
		t.Errorf("attribution held %d cells, want exactly %d (the tail of the write)", len(got), attributionMax)
	}
	// It is the TAIL that survives, which is the most recent thing to have
	// happened and the only defensible choice.
	if _, ok := got[cells[len(cells)-1]]; !ok {
		t.Error("the most recent cell should be attributed")
	}
	if _, ok := got[cells[0]]; ok {
		t.Error("the oldest cell should have been evicted")
	}

	// Re-editing one cell repeatedly must not evict the others: it updates in
	// place rather than consuming a slot.
	r.presence.mu.Lock()
	sp := r.presence.sheets["s1"]
	r.presence.mu.Unlock()
	before := sp.attr.len()
	for range 1000 {
		r.NoteEdit("s1", "bulk", []CellRef{cells[len(cells)-1]})
	}
	if after := sp.attr.len(); after != before {
		t.Errorf("ring size = %d after 1000 repeats of one cell, want %d", after, before)
	}
}

func TestAttrRingReuseAndSweep(t *testing.T) {
	var ring attrRing
	now := time.Now()
	ring.note(CellRef{Row: 1}, "a", identityFor("a"), now)
	ring.note(CellRef{Row: 2}, "a", identityFor("a"), now)
	ring.note(CellRef{Row: 1}, "b", identityFor("b"), now) // overwrite in place
	if ring.len() != 2 {
		t.Fatalf("len = %d, want 2 (an overwrite must not consume a slot)", ring.len())
	}
	// The overwrite replaces the author too: the flash says who changed it LAST.
	if i, ok := ring.idx[CellRef{Row: 1}]; !ok || ring.entries[i].who != identityFor("b") {
		t.Error("an overwritten cell must carry the new author's identity")
	}
	if !ring.sweep(now.Add(attributionTTL)) {
		t.Error("sweep should report that it dropped something")
	}
	if ring.len() != 0 {
		t.Errorf("len after sweep = %d, want 0", ring.len())
	}
	if ring.sweep(now.Add(2 * attributionTTL)) {
		t.Error("sweeping an empty ring should report nothing dropped")
	}
}

// The sweeper reaps sheets nobody is looking at, so the hub does not grow one
// entry per sheet ever edited.
func TestSweeperReapsIdleSheets(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	mustRegister(t, r, "c1", "s1", 0, 4)
	r.NoteEdit("s1", "c1", []CellRef{{Row: 0, Col: 0}})
	r.Unregister("c1")

	deadline := time.Now().Add(attributionTTL + 6*presenceSweepEvery + 2*time.Second)
	for time.Now().Before(deadline) {
		r.presence.mu.Lock()
		n := len(r.presence.sheets)
		r.presence.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the presence hub still holds a sheet with no viewers and no attribution")
}

// ─── Concurrency ──────────────────────────────────────────────────────────────

// Presence must be safe under the same race the registry already survives: a
// stream tearing down while its selection is being committed and read.
func TestPresenceRacesTeardownSafely(t *testing.T) {
	r := newPresenceRegistry(t, 0)
	var wg sync.WaitGroup
	for i := range 30 {
		id := fmt.Sprintf("c%02d", i)
		mustRegister(t, r, id, "s1", 0, 4)
		wg.Add(4)
		go func() { defer wg.Done(); _ = r.SetSelection(id, CellRef{Row: i, Col: 1}, selRange{}) }()
		go func() { defer wg.Done(); r.Presence("s1") }()
		go func() { defer wg.Done(); r.NoteEdit("s1", id, []CellRef{{Row: i, Col: 2}}) }()
		go func() { defer wg.Done(); r.Unregister(id) }()
	}
	wg.Wait()
	for i := range 30 {
		r.Unregister(fmt.Sprintf("c%02d", i))
	}
	if n := len(r.Presence("s1")); n != 0 {
		t.Errorf("presence after teardown = %d, want 0", n)
	}
	if s := r.Stats(); s.Connections != 0 || s.Subscriptions != 0 {
		t.Errorf("registry stats = %d/%d, want 0/0", s.Connections, s.Subscriptions)
	}
}

// A registry with no bus still delivers presence, the way subscribeLocked
// already accommodates one for tests and single-process dispatch.
func TestPresenceWorksWithoutABus(t *testing.T) {
	r := NewRegistry(nil, 0)
	t.Cleanup(r.Close)
	c := mustRegister(t, r, "c1", "s1", 0, 4)
	if !awaitPresence(t, c, 2*time.Second) {
		t.Error("a bus-less registry should still deliver presence wakes")
	}
}
