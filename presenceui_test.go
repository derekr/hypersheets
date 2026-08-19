package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ─── The rule presence had to obey ────────────────────────────────────────────

// PRESENCE COSTS NOTHING PER CELL, AND THAT IS THE WHOLE BUDGET. Every byte
// figure in RESULTS.md — `#g` at 9,950 B blank and 217,727 B seeded, the 55-byte
// one-cell edit patch — is a claim about a cell's markup. A cursor, a range and
// a change flash are all page-shell overlays for exactly that reason, and this
// is what fails the build if one of them ever leaks into the grid.
func TestPresenceAddsNothingToTheGrid(t *testing.T) {
	win := renderWindow(sampleCells(4), 0, 3, "demo", selRange{})
	for _, id := range []string{chipsID, overlayID, selIssuerID, bfID} {
		if strings.Contains(win, `id="`+id+`"`) {
			t.Errorf("#%s is rendered inside #g; every push would re-send it", id)
		}
	}
	for _, cls := range []string{`class="pu"`, `class="pb"`, `class="pf"`, `class="pc`} {
		if strings.Contains(win, cls) {
			t.Errorf("the grid carries %s; presence must be an overlay", cls)
		}
	}
	// The cell itself, byte for byte.
	if !strings.Contains(win, `<b id="D1" style="--r:0">490</b>`) {
		t.Errorf("a cell's markup changed:\n%s", win[:min(600, len(win))])
	}
	// And the four elements ARE in the shell, where no push re-sends them.
	shell := pageShell("demo", 0, 3, "", zeroAnchor())
	for _, id := range []string{chipsID, overlayID, selIssuerID, bfID} {
		if !strings.Contains(shell, `id="`+id+`"`) {
			t.Errorf("#%s is not in the page shell", id)
		}
	}
}

// ─── The overlay ──────────────────────────────────────────────────────────────

func testUsers() []PresenceUser {
	return []PresenceUser{
		{ConnID: "aaaa", Name: "Amber Otter", Hue: 210, Self: true,
			Sel: Selection{Cell: CellRef{Row: 2, Col: 1}, On: true}},
		{ConnID: "bbbb", Name: "Teal Heron", Hue: 45,
			Sel: Selection{Cell: CellRef{Row: 5, Col: 3}, On: true,
				Range: selRange{Lo: CellRef{Row: 5, Col: 3}, Hi: CellRef{Row: 8, Col: 6}, On: true}}},
	}
}

// SELF IS NEVER A COLLABORATOR. Your own cursor is `#ac`, drawn from your own
// signals at pointer rate; drawing the server's copy of it as well would put a
// second box a round trip behind the first one, on top of it.
func TestTheOverlayNeverDrawsYourself(t *testing.T) {
	html := presenceOverlayHTML(testUsers(), nil, nil, 0, 100)
	if strings.Contains(html, "aaaa") {
		t.Errorf("the viewer's own cursor is in their overlay:\n%s", html)
	}
	if !strings.Contains(html, `id="ubbbb" class="pu"`) {
		t.Errorf("the collaborator's cursor is missing:\n%s", html)
	}
	if !strings.Contains(html, `id="gbbbb" class="pb"`) {
		t.Errorf("the collaborator's range is missing:\n%s", html)
	}
	// Colour is a hue and geometry is a grid placement: nothing here needs a
	// client-side measurement, which is what lets a resized column move all of
	// it for free.
	if !strings.Contains(html, `--r:5;--n:4;grid-column:5/9;--h:45`) {
		t.Errorf("the range box is not placed by grid-column:\n%s", html)
	}
	if !strings.Contains(html, "<b>Teal Heron</b>") {
		t.Errorf("the cursor carries no name tag:\n%s", html)
	}
}

// A CURSOR 4,000 ROWS AWAY IS BYTES NOBODY CAN SEE. The overlay is clipped to
// the screen's own buffer, and a scroll re-runs it (patchPresence is called from
// push), so a collaborator scrolled into view appears on the frame that reveals
// their rows.
func TestTheOverlayIsClippedToTheBuffer(t *testing.T) {
	users := testUsers()
	if got := presenceOverlayHTML(users, nil, nil, 200, 400); got != `<div id="`+overlayID+`"></div>` {
		t.Errorf("a cursor outside the buffer was drawn: %s", got)
	}
	// A range that only OVERLAPS the buffer is drawn whole — it is one element,
	// and clipping it would be arithmetic in exchange for nothing.
	if got := presenceOverlayHTML(users, nil, nil, 7, 400); !strings.Contains(got, `class="pb"`) {
		t.Errorf("a range overlapping the buffer was dropped: %s", got)
	}
}

// THE FLASH FADES IN CSS AND THE SERVER ONLY SAYS WHEN. `--d` is a negative
// animation delay, so a flash the reader scrolls into arrives already the right
// amount of faded and finishes on time — with no repaint traffic at all. It is
// rounded so that a re-render caused by something else does not rewrite every
// flash's style attribute and restart the animations it is describing.
func TestFlashCarriesItsAgeAsANegativeDelay(t *testing.T) {
	ref := CellRef{Row: 3, Col: 2}
	set := map[CellRef]Flash{ref: {Ref: ref, Hue: 45, Age: 1234 * time.Millisecond}}
	scr := &screen{}
	html := presenceOverlayHTML(nil, set, scr.flashDelays(set), 0, 100)
	if !strings.Contains(html, `id="kC4" class="pf" style="--r:3;grid-column:4/5;--h:45;--d:-1200ms"`) {
		t.Errorf("the flash is not placed or not aged:\n%s", html)
	}
	// A LATER RENDER OF THE SAME FLASH MUST BE BYTE-IDENTICAL. A negative delay
	// is relative to the animation's own start, so re-stating a larger one on a
	// running element makes the fade jump forward — measured as opacity 0.30 for
	// a flash the server called 900ms old. The screen re-states what it gave.
	for _, age := range []time.Duration{1290, 1600, 2500} {
		set[ref] = Flash{Ref: ref, Hue: 45, Age: age * time.Millisecond}
		if again := presenceOverlayHTML(nil, set, scr.flashDelays(set), 0, 100); again != html {
			t.Fatalf("an age of %v rewrote the flash:\n%s\n%s", age, html, again)
		}
	}
	// A SECOND EDIT TO THE SAME CELL DOES restate it — the ring refreshes the
	// timestamp in place, so the age goes DOWN, and the fade has to restart.
	set[ref] = Flash{Ref: ref, Hue: 45, Age: 50 * time.Millisecond}
	restarted := presenceOverlayHTML(nil, set, scr.flashDelays(set), 0, 100)
	if !strings.Contains(restarted, `--d:-0ms`) {
		t.Errorf("a re-edit did not restart the fade:\n%s", restarted)
	}
	// And a cell that stops flashing is forgotten, so the next flash there
	// starts from its own age rather than from a remembered one.
	scr.flashDelays(nil)
	if scr.flashAge != nil {
		t.Error("the delay memory outlived the flashes it describes")
	}
}

// The overlay is patched as one element, so its child ORDER has to be a function
// of the state and not of a map iteration.
func TestTheOverlayIsDeterministic(t *testing.T) {
	set := map[CellRef]Flash{
		{Row: 9, Col: 1}: {Ref: CellRef{Row: 9, Col: 1}, Hue: 0},
		{Row: 2, Col: 4}: {Ref: CellRef{Row: 2, Col: 4}, Hue: 0},
		{Row: 2, Col: 1}: {Ref: CellRef{Row: 2, Col: 1}, Hue: 0},
	}
	scr := &screen{}
	want := presenceOverlayHTML(testUsers(), set, scr.flashDelays(set), 0, 100)
	for i := 0; i < 20; i++ {
		if got := presenceOverlayHTML(testUsers(), set, scr.flashDelays(set), 0, 100); got != want {
			t.Fatalf("the overlay reordered itself:\n%s\n%s", want, got)
		}
	}
	if strings.Index(want, "kB3") > strings.Index(want, "kE3") {
		t.Error("flashes are not ordered by cell")
	}
}

// ─── The chips ────────────────────────────────────────────────────────────────

func TestChipsMarkSelfAndCarryTheIdentity(t *testing.T) {
	html := presenceChipsHTML(testUsers())
	if !strings.Contains(html, `<i class="pc me" style="--h:210" title="Amber Otter (you)">AO</i>`) {
		t.Errorf("self is not marked:\n%s", html)
	}
	if !strings.Contains(html, `<i class="pc" style="--h:45" title="Teal Heron">TH</i>`) {
		t.Errorf("the other viewer's chip is wrong:\n%s", html)
	}
	// The rendered strip and the shell's empty seed must be the same element,
	// or the first presence frame is an append-or-morph decision rather than an
	// ordinary morph by id.
	if !strings.HasPrefix(html, strings.TrimSuffix(presenceChipsSeed(), "</span>")) {
		t.Errorf("the chips element does not match the seed the shell carries:\n%s\n%s",
			presenceChipsSeed(), html)
	}
}

// A disambiguated name has to stay distinguishable at 22px, which is the whole
// reason the suffix survives into the initials.
func TestInitials(t *testing.T) {
	for in, want := range map[string]string{
		"Amber Otter": "AO", "Amber Otter 2": "AO2", "Amber": "A", "": "?",
	} {
		if got := initialsOf(in); got != want {
			t.Errorf("initialsOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// ─── The selection commit ─────────────────────────────────────────────────────

// The commit is armed by ONE subscription rather than by a handler on each of
// the dozen gestures that can move a rectangle, and it names the connection id
// so that a cold load commits as soon as there is a connection to commit
// against.
func TestTheSelectionCommitIsArmedOnce(t *testing.T) {
	for _, want := range []string{"$conn", "$ref", "selRng($_sar,$_sac,$_sfr,$_sfc)"} {
		if !strings.Contains(selEffectExpr, want) {
			t.Errorf("the selection effect does not subscribe to %s", want)
		}
	}
	post := selPost("demo")
	if !strings.Contains(post, "/sel'") || !strings.Contains(post, "@post") {
		t.Errorf("the selection commit is not a POST to /sel: %s", post)
	}
	if !strings.Contains(post, "!$conn") {
		t.Error("the commit does not decline without a connection id")
	}
}

// ─── The aggregate's staleness rule ───────────────────────────────────────────

// TWO CAUSES AND ONLY TWO. The aggregate is a windowed scan and a push is the
// hot path; recomputing it unconditionally would put a scan of up to 100,000
// cells on every frame of every viewer.
func TestAggregateRecomputesOnlyWhenItCan(t *testing.T) {
	rng := selRange{Lo: CellRef{Row: 1, Col: 1}, Hi: CellRef{Row: 4, Col: 4}, On: true}
	scr := &screen{}
	if !scr.aggStale(rng, nil, true) {
		t.Error("a screen that has never computed an aggregate is not stale")
	}
	scr.takeAgg(rng, "Sum 3")
	if scr.aggStale(rng, nil, true) {
		t.Error("nothing changed and it wants to recompute")
	}
	if scr.aggStale(rng, []CellRef{{Row: 9, Col: 9}}, true) {
		t.Error("an edit outside the rectangle made it stale")
	}
	if !scr.aggStale(rng, []CellRef{{Row: 2, Col: 2}}, true) {
		t.Error("an edit INSIDE the rectangle did not make it stale — the whole point")
	}
	if !scr.aggStale(selRange{}, nil, true) {
		t.Error("the rectangle moved and it did not notice")
	}
	scr.takeAgg(rng, "Sum 3")
	if !scr.aggStale(rng, nil, false) {
		t.Error("a screen that fell off the edit log must recompute rather than guess")
	}
	// A rectangle that MOVED but summarises to the same text costs no frame and
	// is still recorded, or every later push would recompute it.
	scr.takeAgg(rng, "Sum 3")
	other := selRange{Lo: CellRef{Row: 20, Col: 1}, Hi: CellRef{Row: 24, Col: 4}, On: true}
	if _, changed := scr.takeAgg(other, "Sum 3"); changed {
		t.Error("an unchanged aggregate line was re-sent")
	}
	if scr.aggStale(other, nil, true) {
		t.Error("the new rectangle was not recorded")
	}
	// Ctrl/Cmd+A commits a bare cell and an oversize flag, so the flag itself
	// has to invalidate — the rectangle does not move.
	scr.takeAgg(selRange{}, "")
	scr.setAggBig(true)
	if !scr.aggStale(selRange{}, nil, true) {
		t.Error("an oversize selection never states its refusal")
	}
}

// ─── The render-and-subscribe handover ────────────────────────────────────────

// The digest both sides compare on has to be the same function of the same
// bytes as the one Registry.Deliver already keeps, or the whole thing is a
// no-op that looks like it works.
func TestHandoverDigestIsTheDeliverDigest(t *testing.T) {
	body := "<div id=\"g\">…</div>"
	sum := sha256.Sum256([]byte(body))
	if digestHex(body) != hex.EncodeToString(sum[:]) {
		t.Error("digestHex is not sha256 of the payload")
	}
	scr := &screen{}
	if scr.takePrimedMatch(body) {
		t.Error("an unprimed screen claimed a match")
	}
	scr.primeGrid(digestHex(body))
	if !scr.takePrimedMatch(body) {
		t.Error("a correct handover did not match")
	}
	if scr.takePrimedMatch(body) {
		t.Error("the handover is not one-shot; it would suppress a later push")
	}
	scr.primeGrid(digestHex(body))
	if scr.takePrimedMatch(body + " ") {
		t.Error("a changed payload was suppressed — this is an optimisation, not a shortcut")
	}
}

// The window the document says it holds is VALIDATED, not trusted: everything
// downstream — the subscription set, the held mask, every incremental patch —
// is indexed against it.
func TestHandoverWindowIsValidated(t *testing.T) {
	s := &Server{bufferBands: 4}
	q := func(kv string) url.Values { v, _ := url.ParseQuery(kv); return v }
	lo, hi := s.handoverWindow(q("bl=150&bh=649"), 10000, 0, 40)
	if lo != 150 || hi != 649 {
		t.Errorf("a valid handover window was rejected: %d..%d", lo, hi)
	}
	// The last buffer of a sheet that is not a whole number of bands.
	if lo, hi = s.handoverWindow(q("bl=10000&bh=10003"), 10004, 0, 40); lo != 10000 || hi != 10003 {
		t.Errorf("the last partial band was rejected: %d..%d", lo, hi)
	}
	for _, bad := range []string{
		"", "bl=150", "bh=649", "bl=649&bh=150", "bl=1&bh=649",
		"bl=150&bh=648", "bl=-50&bh=649", "bl=150&bh=99999", "bl=x&bh=y",
	} {
		lo, hi = s.handoverWindow(q(bad), 10000, 0, 40)
		if lo == 150 && hi == 649 {
			t.Errorf("%q was accepted as a window", bad)
		}
		if lo != 0 || hi != 249 {
			t.Errorf("%q did not fall back to the derived buffer: %d..%d", bad, lo, hi)
		}
	}
}

// The document's own stream is told what the document contains. Without the
// window the two cannot even agree which rows they are discussing — `lo`/`hi`
// are seeded with the page's BUFFER, and running a buffer back through
// bufferRows widens it by BufferBands a second time.
func TestTheShellHandsOverWhatItRendered(t *testing.T) {
	grid := renderWindow(sampleCells(4), 0, 3, "demo", selRange{})
	css := "#b b.s3{font-weight:700}"
	shell := pageShellWidths("demo", 0, 3, grid, zeroAnchor(), nil, DefaultRows, css)
	want := "bl=0&amp;bh=3&amp;d=" + digestHex(grid) + "&amp;sy=" + digestHex(css)
	if !strings.Contains(shell, want) {
		i := strings.Index(shell, `id="live"`)
		t.Errorf("the handover is wrong:\nwant %s\nin   %s", want, shell[i:min(i+400, len(shell))])
	}
}
