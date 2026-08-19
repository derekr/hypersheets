package main

// presence.go — who else is here, where they are looking, and what they just
// changed.
//
// Presence is a read model over the registry's connection map — the same map a
// push uses to find its audience. Nothing here stores a viewer, so its
// invalidation has no write behind it: the topic goes stale when a stream opens
// or closes, when a viewer commits a selection, and — for the change flash — on a
// timer. All four publish a subject and let each subscriber re-resolve its own
// window, exactly as a write does.
//
// None of it touches SQLite. The correct value after a restart is "nobody", so
// state lives on the *Conn and dies with it, which makes teardown a consequence
// of Unregister rather than a second thing to remember.
//
// The one ghost this cannot prevent: Chrome's back/forward cache keeps an
// abandoned page's SSE stream open, so Unregister never runs and the viewer stays
// frozen in the avatar list. No server-side signal separates "reading quietly"
// from "in the bfcache", so the fix belongs on the page (bfScript, presenceui.go).
//
// Three mechanisms share one subject: identity (a derived name and an assigned
// hue), selection (per-connection state on the *Conn, committed on settle), and
// attribution (a bounded per-sheet ring of cell -> who/when that decays).

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// ─── Tuning ───────────────────────────────────────────────────────────────────

const (
	// presenceMaxHz caps presence publishes for one sheet. A safety cap, not a
	// frame rate: the client commits on settle, so the arrival rate is human-scale
	// and this never binds in normal operation. The failure it prevents is
	// quadratic — a client publishing per pointermove would put 50 viewers at
	// 10 Hz into 500 publishes a second, each waking 50 subscribers. Capping per
	// connection would not help, so the cap is on the sheet, because the sheet is
	// what the fan-out is over.
	presenceMaxHz = 10

	// presencePublishEvery is presenceMaxHz as a gap.
	presencePublishEvery = time.Second / presenceMaxHz

	// attributionTTL is how long a cell keeps saying who changed it: long enough
	// to be seen if you were looking elsewhere on the same screen, short enough to
	// be a flash rather than a permanent annotation.
	attributionTTL = 3 * time.Second

	// attributionMax is the per-sheet bound. Attribution must not grow with the
	// sheet: a paste writes up to maxWriteCells cells in one command and a sheet
	// holds a quarter of a million. The ring keeps the most recent cells and
	// forgets the rest, so a bulk operation flashes its tail — honest anyway, since
	// 10,000 simultaneously flashing cells is a strobe rather than a hint.
	attributionMax = 256

	// presenceSweepEvery is the decay interval. An edit's flash appears on the
	// band wake the edit already causes, but nothing happens when it expires, so
	// without a clock the read model would keep reporting it until the next
	// unrelated event.
	presenceSweepEvery = 500 * time.Millisecond
)

// ─── Identity ─────────────────────────────────────────────────────────────────

// presenceAdjectives x presenceAnimals is 1,024 names. Collisions within one
// sheet are still possible (~50/50 at 38 simultaneous viewers) and are resolved
// in the read model rather than avoided here — see disambiguate.
var presenceAdjectives = [32]string{
	"Amber", "Azure", "Brisk", "Bright", "Calm", "Cobalt", "Coral", "Crimson",
	"Dapper", "Dusky", "Eager", "Ember", "Fleet", "Gentle", "Golden", "Hazel",
	"Indigo", "Jade", "Keen", "Lively", "Lunar", "Merry", "Mellow", "Noble",
	"Olive", "Plucky", "Quiet", "Rapid", "Rusty", "Sable", "Teal", "Violet",
}

var presenceAnimals = [32]string{
	"Otter", "Heron", "Marten", "Badger", "Falcon", "Lynx", "Ibis", "Tapir",
	"Gecko", "Puffin", "Wombat", "Osprey", "Kestrel", "Meerkat", "Panda", "Raven",
	"Salmon", "Turtle", "Urchin", "Viper", "Walrus", "Yak", "Zebra", "Bison",
	"Cricket", "Dingo", "Egret", "Ferret", "Gibbon", "Hare", "Jackal", "Koala",
}

// presenceHues is the wheel a joining viewer's colour is chosen from — a set of
// positions, not a list of identities: nothing maps a viewer to an entry by index.
//
// Even spacing is the opposite of what a hash-indexed palette wants, where the
// palette must do the separating itself. Here the assignment does it (pickHue) and
// the palette only has to be a fine enough ruler.
//
// The gap at 210-225 is the UI's blue: the active-cell outline is hue 214, and a
// collaborator's cursor is the same 2px inset box-shadow as your own active cell,
// so a viewer wearing that hue would put what looks like your cursor on somebody
// else's row.
var presenceHues = [22]int{
	0, 15, 30, 45, 60, 75, 90, 105, 120, 135, 150,
	165, 180, 195, 240, 255, 270, 285, 300, 315, 330, 345,
}

// presenceIdentity is what a viewer looks like: a derived name and an assigned
// colour. Hue and Color are two spellings of one fact — the renderer passes `--h`
// to CSS that owns the saturation and lightness, a snapshot wants the string.
type presenceIdentity struct {
	Name  string // "Amber Otter"
	Hue   int    // 0..359, a position on presenceHues
	Color string // "hsl(210 68% 45%)" — the same hue, ready for an attribute
}

// nameFor derives a name from a connection id.
//
// The name is a pure function while the colour is not: a duplicate name is fixable
// where it is observed (disambiguate appends " 2"), whereas there is no such thing
// as "the same colour, but with a 2 after it". So the hue is the one thing worth
// coordinating over.
func nameFor(connID string) string {
	sum := identityHash(connID)
	// Two different slices of the hash, so an adjective collision does not drag
	// an animal collision along with it.
	return presenceAdjectives[(sum>>0)&31] + " " + presenceAnimals[(sum>>16)&31]
}

// hueColor is the one place the presence palette's saturation and lightness are
// spelled in Go; they agree with the CSS in render.go, which builds the same
// colour from `--h`. The string form exists for snapshots and for callers that
// want a colour rather than a number.
func hueColor(hue int) string {
	return "hsl(" + strconv.Itoa(hue) + " 68% 45%)"
}

// identityHash is the mixed hash of a connection id: the source of the name and
// of every tie-break that wants to be deterministic without being ordered.
func identityHash(connID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(connID))
	return avalanche(h.Sum64())
}

// identityFor is the fallback identity: a name and a hue derived from a connection
// id alone, consulting nothing about who else is present, so it can and does
// collide. A live viewer gets their colour from Register (pickHue) instead; this
// is for authors with no connection to ask — an edit attributed to a connection
// that has already gone, or to none at all — where "some colour, derived, possibly
// shared" is the right answer for somebody who is by definition not in the room.
func identityFor(connID string) presenceIdentity {
	hue := presenceHues[(identityHash(connID)>>32)%uint64(len(presenceHues))]
	return presenceIdentity{Name: nameFor(connID), Hue: hue, Color: hueColor(hue)}
}

// ─── Assignment: the colour is chosen from the room, not from a hash ──────────

// pickHue chooses a joining viewer's hue: the palette entry whose minimum
// circular distance to the hues already in use is largest.
//
// Maximin rather than "the first free one", because the property that matters is
// not "two viewers got the same hue" but "two viewers were indistinguishable", and
// adjacent palette entries are indistinguishable too. It places the joiner and
// moves nobody, which caps the spread at half the even spacing — the price of a
// colour never changing under the person wearing it.
//
// Once every entry is taken every candidate scores zero on distance, so the tie
// goes to fewest live users, which keeps multiplicities within one of each other.
// The final tie-break is the connection id, not palette order (every sheet's first
// viewer would get the same red) and not map iteration (unreproducible, untestable).
//
// `used` maps hue -> how many live viewers of the sheet hold it.
func pickHue(used map[int]int, connID string) int {
	bestDist, bestUsers := -1, 0
	tied := make([]int, 0, len(presenceHues))

	for _, h := range presenceHues {
		// In an empty room every candidate scores 360, which is what makes the
		// first viewer's hue fall through to the id hash below.
		dist := 360
		for u := range used {
			if d := hueDistance(h, u); d < dist {
				dist = d
			}
		}
		users := used[h]

		switch {
		case dist > bestDist || (dist == bestDist && users < bestUsers):
			bestDist, bestUsers = dist, users
			tied = append(tied[:0], h)
		case dist == bestDist && users == bestUsers:
			tied = append(tied, h)
		}
	}
	return tied[identityHash(connID)%uint64(len(tied))]
}

// hueDistance is the distance between two hues the short way round the wheel,
// which is the only sense in which 350 and 10 are close.
func hueDistance(a, b int) int {
	d := (a - b) % 360
	if d < 0 {
		d = -d
	}
	if d > 180 {
		d = 360 - d
	}
	return d
}

// pickHueLocked assigns a joining connection's hue from the hues its sheet is
// already using. It requires r.mu held for writing so that it runs inside the
// same critical section that inserts the connection: two simultaneous joins must
// not both survey a room that neither of them is in yet and pick the same colour.
//
// It is the whole allocator: the pool is the live connections at the moment of the
// join, so a departing viewer releases its hue by the same delete that removes it
// from the read model. Per sheet, because that is the set a colour has to be
// distinguishable within.
func (r *Registry) pickHueLocked(sheetID, connID string) int {
	used := make(map[int]int, len(presenceHues))
	for _, c := range r.conns {
		if c.SheetID == sheetID {
			used[c.hue]++
		}
	}
	return pickHue(used, connID)
}

// avalanche is splitmix64's finalizer. FNV-1a is a fine hash for a map key and a
// poor one here, because three independent facts are cut out of three bit ranges
// of one 64-bit word: without a mixing step a slice such as `(sum>>32) % n`
// inherits FNV's structure and never reaches some entries.
func avalanche(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// disambiguate makes names unique within one sheet: the second "Amber Otter"
// becomes "Amber Otter 2". It runs where duplication is observed rather than where
// identities are minted, because "unique" is only meaningful relative to a set —
// the same connection can be the only Amber Otter on one sheet and the second on
// another. Callers pass the list already ordered by connection id, which makes the
// suffix deterministic.
func disambiguate(users []PresenceUser) {
	counts := make(map[string]int, len(users))
	for i := range users {
		counts[users[i].Name]++
	}
	seen := make(map[string]int, len(counts))
	for i := range users {
		base := users[i].Name
		if counts[base] < 2 {
			continue
		}
		seen[base]++
		if n := seen[base]; n > 1 {
			users[i].Name = base + " " + strconv.Itoa(n)
		}
	}
}

// ─── The read model ───────────────────────────────────────────────────────────

// Selection is one connection's selection: an active cell, plus range bounds when
// a range is selected. It is ephemeral — in memory on the *Conn, never written
// anywhere — and held the way the buffer window is: the client manipulates locally
// and commits on settle, and the render path and the presence read model both read
// the server's copy rather than each maintaining a version.
type Selection struct {
	// Cell is the active cell. On is false until the connection has committed
	// anything, which is permanent for a viewer who only ever scrolls.
	Cell CellRef
	On   bool

	// Range is the rectangle when a range is selected; Range.On is false for a bare
	// active cell. Reusing selRange keeps it comparable and hands the renderer the
	// same contains() it already uses for the linked region.
	Range selRange

	// At is when the selection last changed, so a renderer can fade a cursor that
	// has been parked. Zero when On is false.
	At time.Time
}

// PresenceUser is one viewer of one sheet: everything the renderer needs for an
// avatar and a collaborator cursor.
type PresenceUser struct {
	ConnID string
	Name   string // disambiguated within the sheet
	Hue    int
	Color  string
	Self   bool // set by PresenceFor: this is the viewer being rendered for
	Sel    Selection
}

// Flash is one recently-changed cell, attributed. Age is how long ago, so a
// renderer can pick an opacity without needing a second clock.
type Flash struct {
	Ref    CellRef
	ConnID string
	Name   string // the base name, not disambiguated — see Flashes
	Hue    int
	Color  string
	Age    time.Duration
}

// Presence returns everyone currently viewing a sheet, ordered by connection id so
// the avatar list does not reshuffle itself between renders.
//
// It owns no state: it walks the registry's connection map — the same map that
// decides who a push goes to. The hue lives on the *Conn because Register chose it
// there (pickHueLocked), so there is no second copy anywhere to fall out of step.
func (r *Registry) Presence(sheetID string) []PresenceUser {
	r.mu.RLock()
	out := make([]PresenceUser, 0, len(r.conns))
	for _, c := range r.conns {
		if c.SheetID != sheetID {
			continue
		}
		out = append(out, PresenceUser{
			ConnID: c.ID,
			Name:   nameFor(c.ID),
			Hue:    c.hue,
			Color:  c.color,
			Sel:    c.Selection(),
		})
	}
	r.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].ConnID < out[j].ConnID })
	disambiguate(out)
	return out
}

// PresenceFor is Presence with Self marked: a viewer's own entry is drawn
// differently, and their own cursor must never be drawn as a collaborator's.
func (r *Registry) PresenceFor(sheetID, viewer string) []PresenceUser {
	users := r.Presence(sheetID)
	for i := range users {
		if users[i].ConnID == viewer {
			users[i].Self = true
		}
	}
	return users
}

// PresenceOf returns one connection's identity and position. ok is false once the
// connection is gone, which is the only "does this user still exist" test there is.
//
// The name is not disambiguated: uniqueness is a property of a sheet's live set and
// this call is about one connection. Use PresenceFor when the answer goes in a list
// next to other people's.
func (r *Registry) PresenceOf(connID string) (PresenceUser, bool) {
	r.mu.RLock()
	c, ok := r.conns[connID]
	var hue int
	var color string
	if ok {
		hue, color = c.hue, c.color
	}
	r.mu.RUnlock()
	if !ok {
		return PresenceUser{}, false
	}
	return PresenceUser{
		ConnID: connID,
		Name:   nameFor(connID),
		Hue:    hue,
		Color:  color,
		Self:   true,
		Sel:    c.Selection(),
	}, true
}

// PresenceCount is how many viewers a sheet has, without building the list.
func (r *Registry) PresenceCount(sheetID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, c := range r.conns {
		if c.SheetID == sheetID {
			n++
		}
	}
	return n
}

// ─── Selection: the seam for an HTTP handler ──────────────────────────────────

// Selection reads back one connection's held selection. ok is false once the
// connection is gone.
//
// The read half is what makes holding the selection server-side worth it: the
// render path marks the owner's own cells from here, so rows revealed by a later
// scroll patch arrive already marked, without the client re-asserting a rectangle
// over every row the server just sent it.
func (r *Registry) Selection(connID string) (Selection, bool) {
	r.mu.RLock()
	c, ok := r.conns[connID]
	r.mu.RUnlock()
	if !ok {
		return Selection{}, false
	}
	return c.Selection(), true
}

// SetSelection records a connection's settled selection and invalidates presence.
// It is the counterpart of SetBuffer: the client manipulates locally, commits
// discretely, and the server holds one copy per connection.
//
// The publish is rate-capped; the state is not. The selection is stored
// immediately, so a render happening for any other reason already sees it; only
// the invalidation is capped, the way markDirty caps a wake. Coalescing a selection
// is free in a way coalescing an edit is not: intermediate rectangles have no
// value, only the latest one does.
func (r *Registry) SetSelection(connID string, cell CellRef, rng selRange) error {
	if !cell.Valid() {
		return fmt.Errorf("set selection %s: %s is not addressable", connID, cell.String())
	}
	if rng.On {
		if !rng.Lo.Valid() || !rng.Hi.Valid() {
			return fmt.Errorf("set selection %s: range is not addressable", connID)
		}
		// Normalize rather than reject: a drag upward commits its corners in
		// the order the pointer visited them.
		lo := CellRef{Row: min(rng.Lo.Row, rng.Hi.Row), Col: min(rng.Lo.Col, rng.Hi.Col)}
		hi := CellRef{Row: max(rng.Lo.Row, rng.Hi.Row), Col: max(rng.Lo.Col, rng.Hi.Col)}
		rng.Lo, rng.Hi = lo, hi
	}

	r.mu.RLock()
	c, ok := r.conns[connID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("set selection: unknown connection %s", connID)
	}

	if !c.setSelection(Selection{Cell: cell, On: true, Range: rng, At: time.Now()}) {
		// Identical to what this connection last committed. A repeated selection
		// is not a change, so it must not cost a publish — the same argument as
		// the no-op edit that costs zero on the wire.
		return nil
	}
	r.presence.invalidate(c.SheetID)
	return nil
}

// SetSelectionRefs is SetSelection in the vocabulary an HTTP handler has: A1
// strings straight off a signal. `rng` may be empty (a bare active cell), a range
// ("A1:D20"), or a single ref (a one-cell range). A malformed ref is an error
// rather than a silent no-op, because the caller can answer 400 with it and a
// selection that silently stops updating is very hard to debug.
func (r *Registry) SetSelectionRefs(connID, cell, rng string) error {
	ref, err := ParseRef(cell)
	if err != nil {
		return fmt.Errorf("set selection %s: %w", connID, err)
	}
	var sel selRange
	if rng != "" {
		lo, hi, err := ParseRangeBounds(rng)
		if err != nil {
			// A single ref is a legal one-cell range; try that before failing.
			one, oneErr := ParseRef(rng)
			if oneErr != nil {
				return fmt.Errorf("set selection %s: %w", connID, err)
			}
			lo, hi = one, one
		}
		sel = selRange{Lo: lo, Hi: hi, On: true}
	}
	return r.SetSelection(connID, ref, sel)
}

// ClearSelection drops a connection's held selection — the client has no active
// cell any more (blur, escape). The connection stays in the room.
func (r *Registry) ClearSelection(connID string) {
	r.mu.RLock()
	c, ok := r.conns[connID]
	r.mu.RUnlock()
	if !ok {
		return
	}
	if c.setSelection(Selection{}) {
		r.presence.invalidate(c.SheetID)
	}
}

// ─── Attribution: who just changed this cell ──────────────────────────────────

// NoteEdit records that connID changed these cells, for the change flash.
//
// Attribution is a read model rather than a message because the bus carries a
// subject and nothing else (bus.go), which is what makes a wake idempotent,
// unordered and loss-tolerant; "who changed this" has nowhere in the fan-out to
// travel, so the woken connection resolves it for itself.
//
// It deliberately does not invalidate presence: the edit is about to publish its
// own band subjects, waking exactly the audience whose buffers can see the flash.
// Waking presence too would wake those people twice and wake people who cannot see
// the cell at all.
//
// The identity is snapshotted rather than resolved at read time, because the
// assigned hue lives on a *Conn that Unregister deletes and the author's stream may
// be gone before the flash is read. connID may be empty (a server-side or scripted
// write): those cells flash for everyone.
func (r *Registry) NoteEdit(sheetID, connID string, cells []CellRef) {
	if len(cells) == 0 {
		return
	}
	r.presence.note(sheetID, connID, r.identityOf(connID), cells, time.Now())
}

// identityOf resolves a connection's identity for a snapshot: the assigned colour
// while the connection is live, the derived fallback once it is not. It takes and
// releases r.mu itself so that the caller can hand the result to the hub without
// the registry lock and the hub lock ever nesting.
func (r *Registry) identityOf(connID string) presenceIdentity {
	r.mu.RLock()
	c, ok := r.conns[connID]
	var hue int
	var color string
	if ok {
		hue, color = c.hue, c.color
	}
	r.mu.RUnlock()
	if !ok {
		return identityFor(connID)
	}
	return presenceIdentity{Name: nameFor(connID), Hue: hue, Color: color}
}

// FlashSet returns the cells changed within attributionTTL by someone other than
// viewer, as a map for a render pass to consult per cell.
//
// One map taken once, because the alternative is a lock acquisition per rendered
// cell and a buffer is thousands of cells; a flash that expires mid-render is shown
// for one extra frame, which is a fine thing to be wrong about. "Other than the
// viewer" is why this takes a connection id: your own edit already painted itself
// locally, so flashing it again would be a second, contradictory animation.
func (r *Registry) FlashSet(sheetID, viewer string) map[CellRef]Flash {
	return r.presence.flashes(sheetID, viewer, time.Now())
}

// Flashes is FlashSet as a slice, newest first — for a renderer that wants to
// emit a run of patches rather than test cell by cell.
//
// Name is the base identity, not disambiguated against the sheet's viewers: an
// attribution outlives its author's connection, and at that point there is no live
// set to be unique within.
func (r *Registry) Flashes(sheetID, viewer string) []Flash {
	set := r.FlashSet(sheetID, viewer)
	out := make([]Flash, 0, len(set))
	for _, f := range set {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Age != out[j].Age {
			return out[i].Age < out[j].Age
		}
		if out[i].Ref.Row != out[j].Ref.Row {
			return out[i].Ref.Row < out[j].Ref.Row
		}
		return out[i].Ref.Col < out[j].Ref.Col
	})
	return out
}

// ─── The hub ──────────────────────────────────────────────────────────────────

// presenceHub owns the per-sheet publish throttle, the per-sheet attribution
// ring, and the one subscription each live sheet has on its presence subject.
// It holds no viewers: those are the registry's connections.
type presenceHub struct {
	reg  *Registry
	bus  *Bus
	stop chan struct{}
	done chan struct{}

	mu     sync.Mutex
	sheets map[string]*sheetPresence
}

// sheetPresence is one sheet's presence bookkeeping.
type sheetPresence struct {
	sub *nats.Subscription

	// last/timer/pending are the publish throttle, with markDirty's semantics: a
	// pending invalidation is deferred, never dropped. It bounds fan-out cost, not
	// correctness — dropping one would leave a cursor frozen or a departed viewer
	// in the avatar list forever.
	last    time.Time
	timer   *time.Timer
	pending bool

	attr attrRing
}

func newPresenceHub(r *Registry, bus *Bus) *presenceHub {
	h := &presenceHub{
		reg:    r,
		bus:    bus,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		sheets: make(map[string]*sheetPresence),
	}
	go h.run()
	return h
}

// close stops the decay interval; a Registry normally lives as long as the
// process, so this is for tests and orderly shutdown.
func (h *presenceHub) close() {
	select {
	case <-h.stop:
		return
	default:
	}
	close(h.stop)
	<-h.done

	h.mu.Lock()
	for id, sp := range h.sheets {
		if sp.timer != nil {
			sp.timer.Stop()
			sp.timer = nil
		}
		if sp.sub != nil {
			_ = sp.sub.Unsubscribe()
			sp.sub = nil
		}
		delete(h.sheets, id)
	}
	h.mu.Unlock()
}

// entryLocked requires h.mu.
func (h *presenceHub) entryLocked(sheetID string) *sheetPresence {
	sp, ok := h.sheets[sheetID]
	if !ok {
		sp = &sheetPresence{}
		h.sheets[sheetID] = sp
	}
	return sp
}

// join wires a sheet up the first time somebody looks at it, then invalidates. It
// must be called after the registry lock is released, so a callback wanting the
// connection map never waits on the register that caused it.
func (h *presenceHub) join(c *Conn) {
	h.mu.Lock()
	sp := h.entryLocked(c.SheetID)
	if sp.sub == nil && h.bus != nil {
		sheetID := c.SheetID
		sub, err := h.bus.SubscribePresence(sheetID, func() { h.reg.wakePresence(sheetID) })
		if err == nil {
			sp.sub = sub
		}
		// A failed subscribe is not fatal: presence degrades to "you see
		// yourself", the grid is unaffected, and the next join retries.
	}
	h.mu.Unlock()
	h.invalidate(c.SheetID)
}

// leave invalidates after a teardown. There is nothing to remove: the connection is
// already out of the registry's map, and there is no second place a viewer is
// written down that could disagree. The sheet's subscription and throttle record
// are reaped by the sweeper rather than here, so a leave racing a join cannot tear
// down a subscription the join is about to need.
func (h *presenceHub) leave(c *Conn) {
	h.invalidate(c.SheetID)
}

// invalidate publishes a sheet's presence subject, at most presenceMaxHz times a
// second, never dropping the last one.
func (h *presenceHub) invalidate(sheetID string) {
	h.mu.Lock()
	sp := h.entryLocked(sheetID)
	sp.pending = true

	elapsed := time.Since(sp.last)
	if elapsed >= presencePublishEvery {
		h.publishLocked(sheetID, sp)
		h.mu.Unlock()
		return
	}
	if sp.timer == nil {
		sp.timer = time.AfterFunc(presencePublishEvery-elapsed, func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			cur, ok := h.sheets[sheetID]
			if !ok {
				return
			}
			cur.timer = nil
			if !cur.pending {
				return
			}
			h.publishLocked(sheetID, cur)
		})
	}
	h.mu.Unlock()
}

// publishLocked requires h.mu.
func (h *presenceHub) publishLocked(sheetID string, sp *sheetPresence) {
	sp.pending = false
	sp.last = time.Now()
	h.reg.presencePublishes.Add(1)

	if h.bus == nil {
		// A bus-less registry is the test and single-process-dispatch case
		// subscribeLocked already accommodates: the topic exists, delivered by
		// hand.
		go h.reg.wakePresence(sheetID)
		return
	}
	_ = h.bus.PublishPresence(sheetID)
}

// note records attribution for a set of cells, with the author's identity as it
// was at the moment of the edit (see NoteEdit).
func (h *presenceHub) note(sheetID, connID string, who presenceIdentity, cells []CellRef, now time.Time) {
	h.mu.Lock()
	sp := h.entryLocked(sheetID)
	for _, ref := range cells {
		sp.attr.note(ref, connID, who, now)
	}
	h.mu.Unlock()
}

// flashes snapshots one sheet's live attribution, excluding the viewer's own.
func (h *presenceHub) flashes(sheetID, viewer string, now time.Time) map[CellRef]Flash {
	h.mu.Lock()
	sp, ok := h.sheets[sheetID]
	if !ok || sp.attr.len() == 0 {
		h.mu.Unlock()
		return nil
	}
	out := make(map[CellRef]Flash, sp.attr.len())
	for _, e := range sp.attr.entries {
		if e.at.IsZero() || e.connID == viewer {
			continue
		}
		age := now.Sub(e.at)
		if age >= attributionTTL {
			continue
		}
		out[e.ref] = Flash{
			Ref:    e.ref,
			ConnID: e.connID,
			Name:   e.who.Name,
			Hue:    e.who.Hue,
			Color:  e.who.Color,
			Age:    age,
		}
	}
	h.mu.Unlock()
	if len(out) == 0 {
		return nil
	}
	return out
}

// run is the decay interval — one goroutine for the whole process.
func (h *presenceHub) run() {
	defer close(h.done)
	t := time.NewTicker(presenceSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case now := <-t.C:
			h.sweep(now)
		}
	}
}

// sweep expires attribution and reaps sheets nobody is looking at. The
// invalidation it emits has no write behind it — a timer crossed attributionTTL —
// but goes out on the same subject and through the same throttle as a join, so no
// subscriber can tell the difference.
func (h *presenceHub) sweep(now time.Time) {
	h.mu.Lock()
	if len(h.sheets) == 0 {
		h.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(h.sheets))
	for id := range h.sheets {
		ids = append(ids, id)
	}
	h.mu.Unlock()

	live := h.reg.sheetsWithViewers(ids)

	var stale []string
	h.mu.Lock()
	for _, id := range ids {
		sp, ok := h.sheets[id]
		if !ok {
			continue
		}
		if sp.attr.sweep(now) {
			stale = append(stale, id)
			continue
		}
		if !live[id] && sp.attr.len() == 0 && sp.timer == nil && !sp.pending {
			if sp.sub != nil {
				_ = sp.sub.Unsubscribe()
				sp.sub = nil
			}
			delete(h.sheets, id)
		}
	}
	h.mu.Unlock()

	for _, id := range stale {
		h.invalidate(id)
	}
}

// ─── The attribution ring ─────────────────────────────────────────────────────

// attrEntry is one cell's attribution; a zero `at` marks an unused slot. `who` is
// snapshotted by NoteEdit rather than looked up when the flash is read, because
// the author's connection — and with it their assigned hue — may be gone by then.
type attrEntry struct {
	ref    CellRef
	connID string
	who    presenceIdentity
	at     time.Time
}

// attrRing is a fixed-capacity, insertion-ordered map of cell -> who/when.
//
// It is bounded by construction: the backing array is allocated at attributionMax
// and never grows, however large the sheet or the last write. Overwriting a cell
// that is already attributed updates in place and keeps its slot, so repeatedly
// editing one cell can never evict the others.
type attrRing struct {
	entries []attrEntry
	idx     map[CellRef]int
	head    int
	n       int
}

func (a *attrRing) len() int { return a.n }

func (a *attrRing) note(ref CellRef, connID string, who presenceIdentity, at time.Time) {
	if a.idx == nil {
		a.entries = make([]attrEntry, attributionMax)
		a.idx = make(map[CellRef]int, attributionMax)
	}
	if i, ok := a.idx[ref]; ok {
		a.entries[i] = attrEntry{ref: ref, connID: connID, who: who, at: at}
		return
	}
	slot := a.head
	if old := a.entries[slot]; !old.at.IsZero() {
		delete(a.idx, old.ref)
		a.n--
	}
	a.entries[slot] = attrEntry{ref: ref, connID: connID, who: who, at: at}
	a.idx[ref] = slot
	a.n++
	a.head = (slot + 1) % len(a.entries)
}

// sweep drops everything past attributionTTL and reports whether it dropped
// anything — the same question as "does this sheet owe an invalidation".
func (a *attrRing) sweep(now time.Time) bool {
	if a.n == 0 {
		return false
	}
	dropped := false
	for i := range a.entries {
		e := a.entries[i]
		if e.at.IsZero() || now.Sub(e.at) < attributionTTL {
			continue
		}
		delete(a.idx, e.ref)
		a.entries[i] = attrEntry{}
		a.n--
		dropped = true
	}
	return dropped
}
