package main

// presenceui.go — the UI half of presence: the selection commit that makes a
// cursor exist, the avatar chips, the collaborator overlay and the change
// flash.
//
// presence.go is the read model; none of it is visible without this file.
//
// Presence is page-shell UI: not one byte of it may reach a cell — the same
// O(config)-in-the-shell rule the column widths and `--rows` follow, and here not
// merely an economy but the only shape that works. A cursor sits on an empty cell
// most of the time, and under sparse rendering an empty cell has no element to
// carry a class; a cursor living in `#g` would be re-sent by every full morph,
// removed by every scroll patch that dropped its row, and would need a grid render
// to fade a flash that decays on a timer with no write behind it.
//
// So there are exactly three presence elements, all in the page shell, all patched
// by id and by nothing else:
//
//	#pr   the toolbar's avatar chips
//	#pc   the collaborator overlay — cursors, ranges and flashes
//	#se   the element that commits this screen's selection
//
// `#pc` borrows `#b`'s mechanism: a grid with the same `grid-template-columns`,
// sized by the same 26 `--w-N` custom properties inherited from `#vp`. A cursor is
// therefore `style="--r:12;grid-column:5/6"` and nothing more, resolved against
// whatever the columns currently are — including a width a viewer changed a second
// ago — with no client-side geometry read and no second copy of the width cascade.
//
// The flash fades in CSS, not on the wire. Re-sending an opacity would need a frame
// per step to look like a fade while the sweep publishes at most twice a second, so
// the server sends when the edit happened, once, as a negative animation delay, and
// one `@keyframes` rule does the rest.

import (
	"context"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"
	"go.opentelemetry.io/otel/attribute"
)

// ─── Element ids and events ───────────────────────────────────────────────────

const (
	// chipsID is the toolbar's avatar strip: who else is here.
	chipsID = "pr"

	// overlayID is the collaborator overlay — every other viewer's cursor and
	// range, plus the change flash. One element, patched whole.
	//
	// One element and not three, because all three go stale at the same moment: a
	// presence wake means "who is where and what just changed is no longer what
	// you rendered". Splitting them would be three comparisons and three frames to
	// express one fact, over a payload of a few hundred bytes.
	overlayID = "pc"

	// selIssuerID is the element that commits this screen's settled selection. It
	// gets its own element for the reason `#cl`/`#fl`/`#pv`/`#st`/`#ag` each do —
	// see selPost.
	selIssuerID = "se"

	// bfID watches pagehide/pageshow. It issues no request of its own; it
	// removes and recreates `#live`. See bfScript.
	bfID = "bf"
)

// selEvent carries "my selection has settled" from the reactive effect that
// notices it to the one element that posts it.
const selEvent = "sssel"

// selDebounceMs is how long the selection must stop moving before it is committed:
// longer than the gap between two cells of an ordinary drag, shorter than the pause
// before a reader looks at the toolbar. It is the aggregate's number, for the same
// reason. A drag arms no timer at all (see T.selq): `T.dg` says a gesture is live,
// so "settled" is known rather than guessed, and a multi-step drag costing zero
// requests is a property of the mechanism rather than of the number.
const selDebounceMs = aggDebounceMs

// ─── POST /s/{sheetID}/sel ────────────────────────────────────────────────────

// selSignals is a settled selection: the active cell, and the rectangle if there
// is one. It arrives as an explicit Datastar `payload`, as the clear, fill, paste
// and style commands do, so the selection signals stay underscore-prefixed and out
// of every other request.
type selSignals struct {
	Conn string `json:"conn"`
	Cell string `json:"cell"` // "D7"
	Rng  string `json:"rng"`  // "B2:D5", or "" for a bare active cell
}

// handleSel records this screen's settled selection. It is the counterpart of
// /viewport: the client manipulates the rectangle locally at pointer rate and
// commits it when the gesture settles, exactly as it scrolls locally and posts
// lo/hi on a buffer-edge crossing.
//
// It has to wake its own screen explicitly. SetSelection invalidates the sheet's
// presence topic, which tells everyone else, but deliberately does not markDirty
// the owner's band channel, because a selection changes no cell. The owner's
// aggregate is a read model over the same rectangle, so it needs a frame too, and
// asking for it here keeps the registry from knowing that a toolbar shows a sum.
func (s *Server) handleSel(w http.ResponseWriter, r *http.Request) {
	sheetID := r.PathValue("sheetID")
	if err := validSheetID(sheetID); err != nil {
		http.Error(w, "bad sheet id", http.StatusBadRequest)
		return
	}
	var sig selSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx, span := tracer.Start(r.Context(), "sel.command")
	defer span.End()
	started := time.Now()
	span.SetAttributes(
		attribute.String("sheet.id", sheetID),
		attribute.String("conn.id", sig.Conn),
		attribute.String("sel.cell", sig.Cell),
		attribute.String("sel.range", sig.Rng),
	)

	scr := s.screen(sig.Conn)
	if scr == nil || scr.sheetID != sheetID {
		// The stream this command names is gone (reload, navigation, a reaped
		// proxy, or a commit that beat `/live`'s first signal patch). There is no
		// connection to record a selection on, and the next stream commits its own.
		span.SetAttributes(attribute.Bool("conn.missing", true))
		s.respondCommand(w)
		return
	}

	// A rectangle too big to address is not a bad request: Ctrl/Cmd+A is 260,000
	// cells and ParseRangeBounds refuses anything over 100,000. Refusing the whole
	// command would leave the server holding the previous selection, so this
	// screen's cursor would lie to every other viewer until the reader moved it.
	// Keeping the active cell and dropping the range is the truthful degradation.
	big := false
	err := s.reg.SetSelectionRefs(sig.Conn, sig.Cell, sig.Rng)
	if err != nil && sig.Rng != "" {
		if err2 := s.reg.SetSelectionRefs(sig.Conn, sig.Cell, ""); err2 == nil {
			big, err = true, nil
		}
	}
	if err != nil {
		span.RecordError(err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	scr.setAggBig(big)
	span.SetAttributes(attribute.Bool("sel.oversize", big))

	// The cause is set for the trace and for push's signals-only early return.
	scr.obs.setCause(ctx, "sel", clientTiming{})
	s.reg.markDirty(scr.conn)

	obsLog.InfoContext(ctx, "sel",
		"sheet", sheetID, "conn", sig.Conn, "cell", sig.Cell, "range", sig.Rng,
		"oversize", big, "command_ms", msf(time.Since(started)))
	s.respondCommand(w)
}

// ─── The aggregate, as part of a screen's read model ──────────────────────────

// patchAgg recomputes the toolbar's SUM/AVG/COUNT/MIN/MAX for this screen's
// selection when — and only when — it can have changed. Because the selection is
// held on the connection, the aggregate is a read model over server state: it
// follows the data rather than only the rectangle.
//
// Exactly two causes make it stale, which is what keeps it cheap: the rectangle
// moved, or a cell inside it is in the dirty set this push is about to service —
// already in hand, since the edit log is what the cell patch is built from. A range
// read is a windowed scan on the hot path, so recomputing unconditionally would
// scan up to 100,000 cells on every frame of every viewer.
func (s *Server) patchAgg(ctx context.Context, sse *datastar.ServerSentEventGenerator,
	scr *screen, rng selRange, dirty []CellRef, dirtyOK bool,
) error {
	if !scr.aggStale(rng, dirty, dirtyOK) {
		return nil
	}
	text := ""
	switch {
	case scr.aggOversize():
		text = aggTooBig
	case rng.On:
		sh, err := OpenSheet(scr.sheetID)
		if err != nil {
			return err
		}
		cells, err := sh.WindowCtx(ctx, rng.Lo.Row, rng.Hi.Row)
		if err != nil {
			return err
		}
		text = summarize(cells, rng.Lo, rng.Hi).String()
	}
	v, changed := scr.takeAgg(rng, text)
	if !changed {
		return nil
	}
	return sse.MarshalAndPatchSignals(aggSignal{v})
}

// ─── The presence frame ───────────────────────────────────────────────────────

// patchPresence brings this screen's chips and overlay up to date.
//
// It is called from both loops on purpose. A presence wake means the answer changed
// for a presence reason; a grid push means it changed for a data reason — an edit's
// flash has to appear on the frame that carries the edit, or it would arrive up to
// three seconds later on the next presence tick.
//
// Calling it twice is free because it is a comparison, not a send: both halves are
// remembered as sent text and re-stated only when they differ. Screens connect at
// different times and miss different frames, so a per-screen compare is right for
// all of them where a broadcast delta is right only for those listening.
func (s *Server) patchPresence(sse *datastar.ServerSentEventGenerator, scr *screen) error {
	users := s.reg.PresenceFor(scr.sheetID, scr.id)
	flashes := s.reg.FlashSet(scr.sheetID, scr.id)
	lo, hi := scr.window()

	if v, changed := scr.takeChips(presenceChipsHTML(users)); changed {
		if err := patch(sse, v); err != nil {
			scr.forgetChips()
			return err
		}
	}
	if v, changed := scr.takeOverlay(presenceOverlayHTML(users, flashes, scr.flashDelays(flashes), lo, hi)); changed {
		if err := patch(sse, v); err != nil {
			scr.forgetOverlay()
			return err
		}
	}
	return nil
}

// ─── Rendering ────────────────────────────────────────────────────────────────

// presenceChipsHTML is the avatar strip: one `<i>` per viewer carrying a hue and a
// title, with the circle, the colour and the self ring all in CSS.
//
// The whole strip is re-stated rather than diffed — it is O(viewers) at a dozen
// bytes each and changes only on a join or leave, so a diff would be more code
// than payload.
func presenceChipsHTML(users []PresenceUser) string {
	var b strings.Builder
	b.Grow(len(users)*72 + 48)
	b.WriteString(`<span id="` + chipsID + `" class="m pl">`)
	for _, u := range users {
		b.WriteString(`<i class="pc`)
		if u.Self {
			b.WriteString(` me`)
		}
		b.WriteString(`" style="--h:`)
		b.WriteString(strconv.Itoa(u.Hue))
		b.WriteString(`" title="`)
		b.WriteString(html.EscapeString(u.Name))
		if u.Self {
			b.WriteString(` (you)`)
		}
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(initialsOf(u.Name)))
		b.WriteString(`</i>`)
	}
	b.WriteString(`</span>`)
	return b.String()
}

// initialsOf is the two or three characters that fit in a 22px circle. The
// disambiguating suffix survives ("Amber Otter 2" -> "AO2") because two chips that
// look identical are worse than a third character.
func initialsOf(name string) string {
	var b strings.Builder
	for _, f := range strings.Fields(name) {
		if b.Len() >= 3 {
			break
		}
		b.WriteString(string([]rune(f)[0]))
	}
	if b.Len() == 0 {
		return "?"
	}
	return b.String()
}

// presenceOverlayHTML draws every other viewer's position and the change flash,
// as absolutely positioned children of a grid that mirrors `#b`.
//
// Everything is clipped to this screen's buffer, which saves the bytes of drawing a
// cursor 4,000 rows off screen; a scroll re-runs this, so a collaborator scrolled
// into view appears on the frame that reveals their rows.
//
// The three shapes are ordered range, cursor, flash — the painting order they want,
// and a stable one, so a morph matches element to element. Every element carries an
// id for the same reason: an animation that restarted because a sibling was
// inserted above it would make an unrelated cell blink.
func presenceOverlayHTML(users []PresenceUser, flashes map[CellRef]Flash, delays map[CellRef]int, lo, hi int) string {
	var b strings.Builder
	b.Grow(len(users)*160 + len(flashes)*72 + 32)
	b.WriteString(`<div id="` + overlayID + `">`)

	for _, u := range users {
		if u.Self || !u.Sel.On {
			continue
		}
		if r := u.Sel.Range; r.On && r.Hi.Row >= lo && r.Lo.Row <= hi {
			b.WriteString(`<div id="g`)
			b.WriteString(u.ConnID)
			b.WriteString(`" class="pb" style="--r:`)
			b.WriteString(strconv.Itoa(r.Lo.Row))
			b.WriteString(`;--n:`)
			b.WriteString(strconv.Itoa(r.Hi.Row - r.Lo.Row + 1))
			b.WriteString(`;grid-column:`)
			b.WriteString(strconv.Itoa(r.Lo.Col + 2))
			b.WriteByte('/')
			b.WriteString(strconv.Itoa(r.Hi.Col + 3))
			b.WriteString(`;--h:`)
			b.WriteString(strconv.Itoa(u.Hue))
			b.WriteString(`"></div>`)
		}
		c := u.Sel.Cell
		if c.Row < lo || c.Row > hi {
			continue
		}
		b.WriteString(`<div id="u`)
		b.WriteString(u.ConnID)
		b.WriteString(`" class="pu" style="--r:`)
		b.WriteString(strconv.Itoa(c.Row))
		b.WriteString(`;grid-column:`)
		b.WriteString(strconv.Itoa(c.Col + 2))
		b.WriteByte('/')
		b.WriteString(strconv.Itoa(c.Col + 3))
		b.WriteString(`;--h:`)
		b.WriteString(strconv.Itoa(u.Hue))
		b.WriteString(`"><b>`)
		b.WriteString(html.EscapeString(u.Name))
		b.WriteString(`</b></div>`)
	}

	for _, f := range sortedFlashes(flashes) {
		if f.Ref.Row < lo || f.Ref.Row > hi {
			continue
		}
		b.WriteString(`<div id="k`)
		b.WriteString(f.Ref.String())
		b.WriteString(`" class="pf" style="--r:`)
		b.WriteString(strconv.Itoa(f.Ref.Row))
		b.WriteString(`;grid-column:`)
		b.WriteString(strconv.Itoa(f.Ref.Col + 2))
		b.WriteByte('/')
		b.WriteString(strconv.Itoa(f.Ref.Col + 3))
		b.WriteString(`;--h:`)
		b.WriteString(strconv.Itoa(f.Hue))
		// The age as a negative animation delay, so a viewer who scrolls the flash
		// into view or joins late gets it already the right amount of faded and
		// finishing on time.
		//
		// It comes from screen.flashDelays, not f.Age: a negative delay is relative
		// to the animation's own start, so re-stating a larger one on an element
		// already running makes the fade jump forward.
		b.WriteString(`;--d:-`)
		b.WriteString(strconv.Itoa(delays[f.Ref]))
		b.WriteString(`ms"></div>`)
	}

	b.WriteString(`</div>`)
	return b.String()
}

// sortedFlashes orders attribution by cell so two renders of the same set produce
// the same bytes. Flashes orders by age instead, which changes between renders
// and would reshuffle the overlay for no reason.
func sortedFlashes(set map[CellRef]Flash) []Flash {
	out := make([]Flash, 0, len(set))
	for _, f := range set {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ref.Row != out[j].Ref.Row {
			return out[i].Ref.Row < out[j].Ref.Row
		}
		return out[i].Ref.Col < out[j].Ref.Col
	})
	return out
}

// ─── The page-shell markup ────────────────────────────────────────────────────

// presenceChipsSeed is the empty strip the first paint carries, so the first
// presence frame can be an ordinary `outer` morph by id rather than an
// append-or-morph decision — the same argument the empty `<style id="sy">` makes.
func presenceChipsSeed() string {
	return `<span id="` + chipsID + `" class="m pl"></span>`
}

// presenceOverlaySeed is the same for the overlay. It is a sibling of `#g` inside
// `#vp`, so no push re-sends it and no morph removes it.
func presenceOverlaySeed() string {
	return `<div id="` + overlayID + `"></div>`
}

// selIssuerHTML is the element that commits the selection. The `data-effect`
// subscribes to the connection id, the active cell and the four selection signals
// — one subscription instead of a handler on each of the dozen gestures that can
// move a selection. The effect issues nothing itself: it re-arms a timer, and the
// timer dispatches the event this element turns into one request.
func selIssuerHTML(sheetID string) string {
	return `<div id="` + selIssuerID + `" hidden data-effect="` + selEffectExpr +
		`" data-on:` + selEvent + `__window="` + selPost(sheetID) + `"></div>`
}

// bfHTML watches the two events that bracket a bfcached page. It issues no request
// of its own, so it is exempt from the one-element-per-command rule for the same
// reason `#ex` is: an element that never fetches cannot cancel anything.
func bfHTML() string {
	return `<div id="` + bfID + `" hidden data-on:pagehide__window="` +
		`if(window.__ss)window.__ss.bfOff(evt.persisted)" data-on:pageshow__window="` +
		`if(window.__ss)window.__ss.bfOn()"></div>`
}

// bfScript is the bfcache bracket. Chrome keeps an abandoned page's SSE stream
// open: the socket stays healthy, handleLive never returns and Unregister never
// runs, so presence reports a ghost frozen at its last position, and the
// six-sockets-per-origin budget runs out after about six same-origin navigations,
// after which every request stalls for tens of seconds.
//
// One event fixes both, and it belongs with the page rather than the model.
// `pagehide` removes `#live`, which aborts that element's request — the "removing
// an element aborts its in-flight request" hazard, used on purpose here — and so
// produces a real leave. `pageshow` puts an identical element back, and Datastar's
// MutationObserver applies `data-init` to an inserted node, opening a new stream.
// A returning viewer therefore comes back under a new name and colour, because
// identity belongs to the connection.
//
// It acts only on `persisted`, true exactly when the page is going into the
// back/forward cache — the only case where the socket outlives the reader. A page
// being destroyed takes its socket with it, so aborting there would buy nothing and
// cost the console error Datastar logs for an aborted stream.
//
// It reads the URL off `#live` rather than being told the sheet id, so the stream's
// address has one spelling on the page, and it strips the query string: that query
// is the render-and-subscribe handover ("the document already holds this window
// with this digest"), and after a restore the DOM is the document plus every patch
// applied before the freeze, so reusing it could suppress a first push the browser
// genuinely needs.
func bfScript() string {
	return `<script>(function(){var T=window.__ss;if(!T)return;
var ID='live',e0=document.getElementById(ID);
T.bfURL=e0?(e0.getAttribute('data-init')||'').replace(/\/live\?[^']*'/,"/live'"):'';
T.bfOff=function(p){var e=document.getElementById(ID);if(!e)return;
 T.bfURL=(e.getAttribute('data-init')||T.bfURL).replace(/\/live\?[^']*'/,"/live'");
 if(p===false)return;
 e.remove();};
T.bfOn=function(){if(document.getElementById(ID)||!T.bfURL)return;
 var e=document.createElement('div');e.id=ID;e.hidden=true;
 e.setAttribute('data-init',T.bfURL);document.body.appendChild(e);};
})();</script>`
}
