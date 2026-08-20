package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The interaction model is mostly a browser fact and is verified in a real
// browser. These tests pin the parts of it that are decidable from the HTML,
// because every one of them is a rule that was broken once and would break
// silently: the morph hole, the byte budget, and which element issues which
// command.

func sampleCells(n int) []Cell {
	cells := make([]Cell, n*MaxCols)
	for r := 0; r < n; r++ {
		for c := 0; c < MaxCols; c++ {
			cells[r*MaxCols+c] = Cell{
				Ref:      CellRef{Row: r, Col: c},
				Kind:     KindNumber,
				Computed: "490", Display: "490",
			}
		}
	}
	return cells
}

// A CELL IS AN ID, A ROW AND A VALUE. Every measured number in RESULTS.md rests
// on a cell carrying nothing else — no per-cell handler, no per-cell attribute.
// Keyboard handling is delegated onto the two page-shell elements precisely so
// that this stays true.
//
// SPARSE RENDERING ADDED EXACTLY ONE THING: `style="--r:6"`, the absolute row.
// The COLUMN is deliberately not here — it comes from the id via 26 generated
// `#b [id^=D]{grid-column:5}` rules, which is 6-7 bytes per cell that stay in
// the stylesheet instead of being repeated in every window forever.
func TestCellMarkupUnchangedByInteractionModel(t *testing.T) {
	got := renderCells([]Cell{{Ref: CellRef{Row: 6, Col: 3}, Kind: KindNumber, Computed: "490", Display: "490"}})
	if want := `<b id="D7" style="--r:6">490</b>`; got != want {
		t.Fatalf("cell markup grew:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "--c") {
		t.Error("the cell encodes its column; that belongs in colPlacementCSS")
	}
	if n := len(got); n != 32 {
		t.Fatalf("cell is %d bytes, want 32", n)
	}
}

// A ROW MUST CARRY NO HANDLERS EITHER. renderCellRows is what an incremental
// scroll patch ships; one `data-on` per row would be 10,000 copies.
func TestRowMarkupCarriesNoHandlers(t *testing.T) {
	rows := renderCellRows(sampleCells(3), 0, 2) + renderRowNums(0, 2)
	for _, bad := range []string{"data-on", "tabindex", "onclick", "contenteditable"} {
		if strings.Contains(rows, bad) {
			t.Errorf("row markup contains %q — the keyboard must be delegated, not per-row", bad)
		}
	}
}

// THE MORPH HOLE. `#ed` must carry `data-ignore-morph` in EVERY full render:
// the Datastar bundle only skips a node when the attribute is on both the live
// node and the incoming one, and the server cannot know which cell has focus
// when someone else's edit triggers a push.
func TestEditorAlwaysCarriesTheMorphHole(t *testing.T) {
	win := renderWindow(sampleCells(4), 0, 3, "demo", selRange{})
	if !strings.Contains(win, `id="`+editorID+`" data-ignore-morph`) {
		t.Fatalf("the editor lost data-ignore-morph:\n%s", win)
	}
	// ...and the incremental paths must never re-send it, or a cell patch could
	// replace the element the caret lives in.
	if strings.Contains(renderCellRows(sampleCells(3), 0, 2), editorID) {
		t.Error("a scroll patch carries the editor")
	}
	if strings.Contains(renderCells(sampleCells(1)), editorID) {
		t.Error("a cell patch carries the editor")
	}
}

// BUFFER BOUNDS ARE SIGNALS. `data-lo`/`data-hi` on `#g` were only ever correct
// because every push re-morphed the wrapper; once a scroll ships row patches
// they freeze and the edge test dies silently. keyboard `reveal()` reads the
// same `$blo`/`$bhi`, so this rule now has a second caller depending on it.
func TestGridWrapperCarriesNoBufferAttributes(t *testing.T) {
	win := renderWindow(sampleCells(4), 0, 3, "demo", selRange{})
	for _, bad := range []string{"data-lo", "data-hi"} {
		if strings.Contains(win, bad) {
			t.Errorf("#g carries %q again", bad)
		}
	}
}

// ONE ELEMENT PER COMMAND. Datastar keys request cancellation on the element,
// so `/cell` and `/viewport` must never share one — "Enter commits and moves
// down" would otherwise abort the commit with the move.
func TestCommandsComeFromTheirOwnElements(t *testing.T) {
	shell := pageShell("demo", 0, 3, renderWindow(sampleCells(4), 0, 3, "demo", selRange{}), zeroAnchor())

	ed := editorHTML("demo")
	if !strings.Contains(ed, "/s/demo/cell") {
		t.Error("the editor does not post the cell command")
	}
	if strings.Contains(ed, "/viewport") {
		t.Error("the editor posts a viewport command — it would abort its own cell command")
	}
	// The cell command must opt out of request cancellation, or two commits in
	// quick succession lose the first.
	if !strings.Contains(ed, `requestCancellation:'disabled'`) {
		t.Error("the cell command still cancels the previous one")
	}
	for _, expr := range []string{vpKeyExpr("demo"), vpNavExpr("demo")} {
		if strings.Contains(expr, "/cell") {
			t.Error("#vp posts a cell command directly; it must dispatch " + commitEvent)
		}
	}
	// `#live` must issue nothing but the stream. A fetch from it aborts the
	// stream, which is how a viewer goes permanently deaf.
	live := shell[strings.Index(shell, `<div id="live"`):]
	live = live[:strings.Index(live, "</div>")]
	if strings.Count(live, "@") != 1 || !strings.Contains(live, "@get('/s/demo/live?") {
		t.Errorf("#live issues something other than the stream: %s", live)
	}
	if !strings.Contains(live, "openWhenHidden:true") {
		t.Error("the stream lost openWhenHidden — it dies when the tab is hidden and never returns")
	}
	// THE STREAM MUST BE ABORTABLE BY REMOVING ITS ELEMENT, which is what the
	// bfcache bracket does on pagehide. Datastar's default `'auto'` stores the
	// controller against the element for the NEXT fetch to supersede and
	// registers no cleanup, so removal left the socket open — measured, as a
	// viewer who navigated away and stayed in the avatar list.
	if !strings.Contains(live, `requestCancellation:'cleanup'`) {
		t.Error("removing #live would not close the stream; pagehide could not produce a real leave")
	}
	// The handover the document hands its own stream: which window it holds and
	// the digests of the two payloads it already carries. Without it the stream
	// re-renders the whole buffer over a DOM that is already correct.
	for _, want := range []string{"bl=", "bh=", "d=", "sy="} {
		if !strings.Contains(live, want) {
			t.Errorf("#live's URL is missing the %q handover", want)
		}
	}
}

// THE LOCAL ECHO IS ON THE COMMAND, NOT ON A KEY. Enter, Tab, blur, the
// pointerdown that precedes a click on another cell and Delete-to-clear are five
// doors into one command; an echo wired to four of them is a stale value on the
// fifth. Pinning it to cellPost is what makes "every commit paints" a property
// of the code rather than of a checklist.
func TestEveryCommitEchoesLocally(t *testing.T) {
	for name, expr := range map[string]string{
		"Enter/Tab": editorKeyExpr("demo"),
		"blur":      editorBlurExpr("demo"),
		"editor":    editorHTML("demo"),
	} {
		post := strings.Count(expr, "/s/demo/cell")
		echo := strings.Count(expr, ".echo($ref,$raw)")
		if post != echo {
			t.Errorf("%s: %d cell commands but %d echoes — a commit that paints nothing "+
				"leaves the OLD value on screen for a round trip", name, post, echo)
		}
	}
	// It must come BEFORE the post, and therefore before the nav event the
	// keydown dispatches: the paint belongs on the cell being committed, not on
	// the one the selection is about to move to.
	k := editorKeyExpr("demo")
	if i, j := strings.Index(k, ".echo("), strings.Index(k, "@post("); i < 0 || j < 0 || i > j {
		t.Error("the echo does not run before the command that carries the value away")
	}
	if i, j := strings.Index(k, ".echo("), strings.Index(k, navEvent); i < 0 || j < 0 || i > j {
		t.Error("the echo runs after the move — it would paint the wrong cell")
	}
}

// FORMULAS ARE ALWAYS ROUND TRIP. The client has no formula engine and must not
// grow one by accident: the only thing it may paint for a formula is the text
// the user typed, marked as pending.
func TestTheEchoNeverComputes(t *testing.T) {
	js := echoScript()
	for _, bad := range []string{"SUM", "eval(", "parseFormula", "*2"} {
		if strings.Contains(js, bad) {
			t.Errorf("the echo looks like it evaluates something (%q)", bad)
		}
	}
	if !strings.Contains(js, `'f `+pendClass+`'`) {
		t.Error("a formula echo is not marked pending, so it cannot be told from a value")
	}
	// And the pending mark must be styled, or "working" looks exactly like
	// "wrong".
	if !strings.Contains(gridCSS, "#"+bufferID+" b."+pendClass+"{") {
		t.Error("the pending class has no rule; a formula would show as a plain value")
	}
	// The server never emits it, which is what makes the confirming morph remove
	// it — the bundle deletes attributes the incoming element does not carry.
	cell := renderCells([]Cell{{Ref: CellRef{Row: 0, Col: 0}, Kind: KindFormula, Raw: "=B1", Computed: "7", Display: "7"}})
	if strings.Contains(cell, `"f `+pendClass+`"`) || strings.Contains(cell, `"`+pendClass+`"`) {
		t.Errorf("the server renders the pending class, so it could never clear: %s", cell)
	}
}

// THREE SCRIPTS SHARE ONE FLAT `window.__ss`, AND THE LAST ONE WINS.
//
// There is no module boundary and no declaration: the anchor script, the timing
// harness and the keyboard all assign helpers onto the same object, in page
// order. A name used twice is not an error, it is a silent replacement — which
// is how a pending-marker helper called `settle` deleted the anchor script's
// scroll-settle guard and made a go-to jump answer its own synthetic scroll
// (`?at=D500` became `?at=A500`, and a viewport command went out that nobody
// asked for). Nothing about that is visible in either file.
//
// mkc/rmc are the exception on purpose: they are the two RENDERING SHAPES of the
// same helper and only one is ever emitted.
func TestClientNamespaceIsUnique(t *testing.T) {
	shapes := map[string]bool{"T.mkc=": true, "T.rmc=": true}
	// SCANNED IN THE BUNDLE, NOT THE PAGE. The scripts moved out of the document
	// into one cached, concatenated file (assets.go), which is now the exact
	// place a collision happens — the five blocks are joined in page order into
	// a single scope, so this is a stricter reading of the same hazard than
	// scanning the rendered document ever was.
	page := appJS
	seen := map[string]int{}
	for i := 0; ; {
		j := strings.Index(page[i:], "T.")
		if j < 0 {
			break
		}
		i += j
		k := i + 2
		for k < len(page) && (page[k] == '_' ||
			(page[k] >= 'a' && page[k] <= 'z') || (page[k] >= 'A' && page[k] <= 'Z') ||
			(page[k] >= '0' && page[k] <= '9')) {
			k++
		}
		// Only DEFINITIONS. `T.postedAt=...` is a counter the timing harness
		// assigns from several places on purpose; a second `=function(` under an
		// existing name is the hazard.
		if strings.HasPrefix(page[k:], "=function(") {
			seen[page[i:k+1]]++
		}
		i = k
	}
	if len(seen) == 0 {
		t.Fatal("no client helpers found; the scan is broken, not the page")
	}
	for name, n := range seen {
		if n > 1 && !shapes[name] {
			t.Errorf("%s is assigned %d times in one page — the later one silently "+
				"replaces the earlier, and neither file mentions the other", name, n)
		}
	}
}

// ONE PENDING STATE, BOTH KINDS OF ECHO. A literal echo without a marker is an
// optimistic update pretending to be a settled one: "the typed text is the
// value" is a claim the server can still refuse, and the reader would have been
// shown it with no signal. The formula case reads honestly only because it looks
// unfinished, so the marker is added to the literal rather than removed from the
// formula.
func TestBothEchoesAreVisiblyUnconfirmed(t *testing.T) {
	js := echoScript()
	// The marker is raised from the ONE branch both kinds of paint go through —
	// not from the formula arm, which is how the literal came to be unmarked.
	if strings.Count(js, "T.pnd(rec)") != 1 {
		t.Error("the pending marker is not raised once for every painted cell")
	}
	if !strings.Contains(js, `classList.add('`+flightClass+`')`) {
		t.Error("nothing ever raises the pending marker")
	}
	// Confirmed and refused are the same mechanism, not a second path.
	if !strings.Contains(js, `classList.remove('`+flightClass+`')`) {
		t.Error("nothing ever clears the pending marker")
	}
	if !strings.Contains(js, `classList.add('`+failClass+`')`) {
		t.Error("a refused commit has no error treatment")
	}
	// 'finished' is dispatched from a `finally` in the v1.0.1 bundle, so it is
	// the only clear that is guaranteed to run. A marker that can outlive its own
	// request is worse than no marker.
	if !strings.Contains(js, `d.type==='finished'`) || !strings.Contains(js, "T.clr") {
		t.Error("the pending marker has no guaranteed clear")
	}
	// The refusal holds the error treatment on screen with a timer, and 'finished'
	// fires during that hold — so the records must be OUT of T.pe before it does
	// or the revert loses them and the lie stays up.
	if !strings.Contains(js, "T.fail(T.pe.splice(0))") {
		t.Error("the refusal shares its record list with the clear; the revert can be stranded")
	}
	// The server emits neither marker, which is what lets the confirming morph
	// remove them (the bundle deletes attributes the incoming element lacks).
	cell := renderCells([]Cell{
		{Ref: CellRef{Row: 0, Col: 0}, Kind: KindFormula, Raw: "=B1", Computed: "7", Display: "7"},
		{Ref: CellRef{Row: 0, Col: 1}, Kind: KindNumber, Computed: "7", Display: "7"},
	})
	for _, c := range []string{flightClass, failClass} {
		if strings.Contains(cell, `"`+c+`"`) || strings.Contains(cell, ` `+c+`"`) {
			t.Errorf("the server renders the %q marker, so it could never clear: %s", c, cell)
		}
	}
}

// THE MARKER MUST NOT MOVE A PIXEL OF TEXT. It appears on the one interaction
// that happens on every keystroke, so a treatment that changes the cell's
// metrics is a reflow of the edited cell on every commit. box-shadow is paint;
// border, padding, width and font-size are not.
func TestThePendingMarkerHasNoMetrics(t *testing.T) {
	for _, c := range []string{flightClass, failClass} {
		i := strings.Index(gridCSS, "#"+bufferID+" b."+c+"{")
		if i < 0 {
			t.Fatalf("the %q marker has no rule; the state would be invisible", c)
		}
		rule := gridCSS[i:]
		rule = rule[:strings.Index(rule, "}")]
		if !strings.Contains(rule, "box-shadow:inset") {
			t.Errorf("%q is not painted with an inset shadow: %s", c, rule)
		}
		for _, bad := range []string{"border", "padding", "margin", "width", "height", "font-size", "left:", "top:"} {
			if strings.Contains(rule, bad) {
				t.Errorf("%q changes the cell's metrics (%s): %s", c, bad, rule)
			}
		}
	}
}

// The delay-threshold variant is a render-time flag, not a build: both readings
// have to be comparable on one binary.
func TestPendingDelayIsCarriedIntoTheScript(t *testing.T) {
	if got := pendingDelay(); got != 0 {
		t.Fatalf("the default is %d; always-on is the default reading", got)
	}
	if !strings.Contains(echoScript(), "T.PD=0;") {
		t.Error("the always-on reading does not reach the client")
	}
	old := *pendingDelayMs
	*pendingDelayMs = 150
	defer func() { *pendingDelayMs = old }()
	js := echoScript()
	if !strings.Contains(js, "T.PD=150;") {
		t.Error("-pending-delay-ms does not reach the client")
	}
	// Scheduled, not conditional on the paint: the formula's provisional TEXT
	// must still appear immediately or the delay reintroduces the flicker.
	if !strings.Contains(js, "if(T.PD>0)rec.t=setTimeout") {
		t.Error("the threshold is not a timer on the marker alone")
	}
	if !strings.Contains(js, `'f `+pendClass+`'`) {
		t.Error("the delay variant dropped the formula's provisional text")
	}
}

// The echo inserts and removes elements, which is a change to the DOM the
// SERVER models (screen.heldMask). The command has to carry the connection or
// the next push appends a second element with the same id.
func TestCellCommandCarriesItsConnection(t *testing.T) {
	var sig cellSignals
	if err := json.Unmarshal([]byte(`{"ref":"A1","raw":"7","conn":"abc"}`), &sig); err != nil {
		t.Fatal(err)
	}
	if sig.Conn != "abc" {
		t.Fatalf("the cell command dropped conn: %+v", sig)
	}
	// Presence is the store's rule, not a second copy of it.
	for _, tc := range []struct {
		raw  string
		want bool
	}{{"", false}, {"7", true}, {" ", true}, {"=A1", true}} {
		if got := InferKind(tc.raw) != KindEmpty; got != tc.want {
			t.Errorf("presence of %q = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// The keymap is one dispatch table in one place; this is the checklist that it
// still answers every key the model promises.
func TestKeymapCoversTheModel(t *testing.T) {
	js := gridKeysScript()
	for _, k := range []string{
		"ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight",
		"Tab", "Enter", "Home", "End", "PageUp", "PageDown",
		"F2", "Delete", "Backspace", "Escape",
	} {
		if !strings.Contains(js, "'"+k+"'") {
			t.Errorf("the keymap no longer handles %s", k)
		}
	}
	if !strings.Contains(js, "e.ctrlKey||e.metaKey") {
		t.Error("Ctrl/Cmd is not consulted, so the edge jumps and Ctrl+Home are gone")
	}
	if !strings.Contains(js, "k.length===1") {
		t.Error("type-to-edit is gone")
	}
}

// The window-level keydown is what makes the grid deaf to nothing; the guard is
// what keeps the go-to box and the editor usable.
func TestWindowKeydownGuardsTextEntry(t *testing.T) {
	shell := pageShell("demo", 0, 3, "", zeroAnchor())
	if !strings.Contains(shell, "data-on:keydown__window=") {
		t.Fatal("the grid's keyboard is not on the window")
	}
	expr := vpKeyExpr("demo")
	if !strings.Contains(expr, "t.tagName==='INPUT'") || !strings.Contains(expr, "t.isContentEditable") {
		t.Error("the window keydown does not step aside for text entry")
	}
	if !strings.Contains(expr, "if($editing)return") {
		t.Error("the window keydown does not step aside while the editor is open")
	}
}

// A click SELECTS. Opening the editor on click is what destroyed an in-flight
// edit's buffer on the way past.
func TestClickSelectsAndDoesNotEdit(t *testing.T) {
	if strings.Contains(vpClickExpr, "$editing") {
		t.Error("a click still enters edit mode")
	}
	if strings.Contains(vpClickExpr, "$raw") {
		t.Error("a click still overwrites the edit buffer")
	}
	if !strings.Contains(vpDblClickExpr("demo"), "$editing=true") {
		t.Error("a double-click no longer edits")
	}
}

// ─── Range selection ──────────────────────────────────────────────────────────
//
// The selection itself is a browser fact and is verified in a real browser
// (/tmp/ssobs/sel.mjs, 53 checks). What is decidable from the HTML is the
// contract that makes it free — where the state lives, where the box lives, and
// what the wire therefore does not carry — and each of those is a rule that
// would break silently.

// THE SELECTION IS LOCAL DURING THE GESTURE AND COMMITTED ONCE IT SETTLES, and
// that narrower claim is what is actually true now — presence made the settled
// rectangle server state, because a collaborator's cursor has to be drawn by a
// server that knows where it is.
//
// The test this replaced asserted that selection signals NEVER leave the
// browser, which is now deliberately false. What survives, and what the
// measured zero-request drag rests on, is the same shape the column resize and
// the buffer-edge crossing have: the four corner signals stay
// underscore-prefixed, so they ride on NO request ever — including the commit,
// which names the rectangle in an explicit payload instead — and the commit is
// armed by a settle timer that a live pointer drag does not arm at all.
func TestSelectionSignalsAreLocal(t *testing.T) {
	shell := pageShell("demo", 0, 3, "", zeroAnchor())
	for _, sig := range []string{"_sar:", "_sac:", "_sfr:", "_sfc:"} {
		if !strings.Contains(shell, sig) {
			t.Errorf("the shell does not declare %q", sig)
		}
	}
	// The un-prefixed spellings would be sent with every request forever.
	for _, bad := range []string{",sar:", ",sac:", ",sfr:", ",sfc:"} {
		if strings.Contains(shell, bad) {
			t.Errorf("%q is an ordinary signal; selection would ride every request", bad)
		}
	}
	// THE COMMIT CARRIES THE RECTANGLE ITSELF, so the four signals stay out of
	// the steady-state signal set even on the one request that needs them.
	post := selPost("demo")
	if !strings.Contains(post, "payload:{") {
		t.Error("the selection commit has no explicit payload; the rectangle would join every request")
	}
	for _, sig := range []string{"_sar", "_sac", "_sfr", "_sfc"} {
		if strings.Contains(post, sig) {
			t.Errorf("the selection commit names %q; it must send the settled range, not the signals", sig)
		}
	}
	// AND ONLY THE SETTLED ONE IS SENT. The effect arms a timer; a live pointer
	// drag arms nothing at all (T.dg), which is what makes a 12-step drag cost
	// zero requests by construction rather than by choosing a long enough
	// debounce.
	if strings.Contains(selEffectExpr, "@post") || strings.Contains(selEffectExpr, "@get") {
		t.Error("the selection effect issues a request itself; a drag would post per cell crossed")
	}
	if !strings.Contains(selectScript(), "if(T.dg||!c||!ref){T.aq=0;return;}") {
		t.Error("T.selq arms its timer during a live drag; the gesture would no longer be free")
	}
	if !strings.Contains(selectScript(), "T.selq(T.sc,T.sr,T.ag)") {
		t.Error("the pointer-up does not arm the commit; a drag would never be committed at all")
	}
}

// The four corners are seeded from the anchor, and the COLLAPSED case is the one
// that matters: `?at=D500` has to start the selection ON D500, or the first
// Shift+Arrow extends from a corner the reader has never seen.
func TestSelectionSeedsFromTheAnchor(t *testing.T) {
	tests := []struct {
		at             string
		wantLo, wantHi CellRef
	}{
		{"", CellRef{}, CellRef{}},
		{"D500", CellRef{Row: 499, Col: 3}, CellRef{Row: 499, Col: 3}},
		{"B4:D9", CellRef{Row: 3, Col: 1}, CellRef{Row: 8, Col: 3}},
		{"D9:B4", CellRef{Row: 3, Col: 1}, CellRef{Row: 8, Col: 3}},
	}
	for _, tc := range tests {
		a, _ := parseAt(tc.at)
		lo, hi := selSeed(a)
		if lo != tc.wantLo || hi != tc.wantHi {
			t.Errorf("selSeed(%q) = %v..%v, want %v..%v", tc.at, lo, hi, tc.wantLo, tc.wantHi)
		}
	}
	shell := pageShell("demo", 0, 3, "", mustAt(t, "B4:D9"))
	if !strings.Contains(shell, "_sar:3,_sac:1,_sfr:8,_sfc:3") {
		t.Error("the shell does not seed the linked range into the client's signals")
	}
}

func mustAt(t *testing.T, s string) anchor {
	t.Helper()
	a, ok := parseAt(s)
	if !ok {
		t.Fatalf("parseAt(%q) failed", s)
	}
	return a
}

// THE INTERACTIVE BOX COSTS NOTHING PER RENDER. It is page-shell markup, like
// `#vp` itself: written once per load, re-sent by no push, removable by no
// morph. A box inside `#g` would be re-sent by every full render and would have
// to be re-created by every structural change.
func TestSelectionBoxIsShellNotGrid(t *testing.T) {
	win := renderWindow(sampleCells(4), 0, 3, "demo", selRange{})
	if strings.Contains(win, `id="`+selBoxID+`"`) {
		t.Fatal("the interactive selection box is inside the render; it must be shell markup")
	}
	shell := pageShell("demo", 0, 3, "", zeroAnchor())
	if !strings.Contains(shell, `<div id="`+selBoxID+`"`) {
		t.Fatal("the shell has no selection box")
	}
	// It must be a sibling of `#g` inside `#vp`, which is what the CH offset in
	// T.selBox pays for.
	if i, j := strings.Index(shell, `id="vp"`), strings.Index(shell, `id="`+selBoxID+`"`); i < 0 || j < i {
		t.Error("the selection box is not inside the scroll container")
	}
	// And the grid is byte-identical whether or not a range is selected: a
	// selected range must not change one cell of the payload.
	sel := renderWindow(sampleCells(4), 0, 3, "demo", selRange{})
	if sel != win {
		t.Error("the render depends on the selection")
	}
}

// A range clear is the slowest command in the prototype, so it must not share an
// element with the fastest. Datastar keys request cancellation on the element:
// `/viewport` from `#vp` and `/clear` from `#vp` would abort each other.
func TestClearPostsFromItsOwnElement(t *testing.T) {
	shell := pageShell("demo", 0, 3, "", zeroAnchor())
	if !strings.Contains(shell, `<div id="cl" hidden data-on:`+clearEvent+`__window=`) {
		t.Fatal("the range clear has no element of its own")
	}
	post := clearPost("demo")
	if !strings.Contains(post, "requestCancellation:'disabled'") {
		t.Error("two clears in flight would abort each other")
	}
	// The range travels as an explicit payload, so it is not a signal that rides
	// every other request in the page.
	if !strings.Contains(post, "payload:{conn:$conn,rng:evt.detail}") {
		t.Error("the clear does not carry its range as a payload")
	}
	if strings.Contains(shell, `id="live" hidden data-on:`+clearEvent) {
		t.Error("the clear must not post from the element holding the SSE stream")
	}
}

// Every movement collapses the range, and the collapse lives in the ONE tail
// every movement shares — not in each arm of the keymap, where it would be
// right seven times and forgotten the eighth.
func TestNavigationCollapsesTheRange(t *testing.T) {
	move := applyMoveExpr("demo")
	for _, want := range []string{"$_sar=a.r", "$_sac=a.c", "$_sfr=a.r", "$_sfc=a.c"} {
		if !strings.Contains(move, want) {
			t.Errorf("the shared move tail does not collapse the range (%q)", want)
		}
	}
	// The extension tail is its opposite: focus only, anchor and active cell
	// untouched.
	ext := applyExtendExpr("demo")
	if strings.Contains(ext, "$_sar=") || strings.Contains(ext, "$row=") {
		t.Error("shift+arrow moves the anchor or the active cell")
	}
	if !strings.Contains(ext, "$_sfr=a.ext.r") {
		t.Error("shift+arrow does not move the focus")
	}
}

// The header selection must not swallow the two affordances the headers already
// own — the resize grip and the menu caret — nor a right-click.
func TestHeaderSelectionYieldsToResizeAndMenu(t *testing.T) {
	js := selectScript()
	if !strings.Contains(js, "t.tagName==='I'") || !strings.Contains(js, "contains('cr')") {
		t.Error("the column-header hit test does not refuse the resize grip and the caret")
	}
	if !strings.Contains(js, "if(e.button!==0)return null") {
		t.Error("a right-click would start a selection instead of opening the menu")
	}
}
