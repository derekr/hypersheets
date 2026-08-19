package main

// latency.go — the latency transparency chip.
//
// This demo runs on one virtual machine in one region, and the server's own
// share of an edit is well under a millisecond. Almost everything a distant
// reader experiences is distance, so somebody across an ocean sees 200 ms and
// concludes, entirely reasonably, that the app is slow. The chip publishes the
// split rather than a single number —
//
//	you ↔ server 180 ms · server 0.4 ms
//
// — because a blended figure invites exactly that misreading, while two figures
// side by side let the reader do the subtraction.
//
// The round trip is measured, not probed. A browser cannot ICMP, and a synthetic
// ping to a health endpoint would be a different request on a different path
// answering a question nobody asked. The number that matters is keystroke →
// confirmed value, which the page is already in a position to time: Datastar
// dispatches `datastar-fetch` on `document` with `type:'started'` when a fetch
// action goes out and `type:'finished'` when it comes back, and the SSE frames
// carrying the result arrive on the same event as
// `datastar-patch-elements`/`-signals`. This is otel.go's `client_prev_wait_ms`,
// widened from the scroll path to every command the page sends.
//
// Whichever answer arrives first closes the stamp: the 204 that ends the POST
// and the SSE frame that carries the result are both one network round trip and
// the reader cannot tell them apart. `#live` is excluded by id — it is a
// held-open GET that never finishes, so a `finished`/`error` on it is a
// disconnect rather than a round trip.
//
// The figure is the median of this viewer's last five commands. One sample is
// jitter — a GC pause, a scheduler hiccup, a retransmit — and it is not an
// average across viewers, because the reader's own distance is the explanation
// and someone else's is not evidence about theirs.
//
// One stamp is open at a time, and it is the oldest unanswered command. Commands
// overlap and the answers all arrive on one stream, so a frame cannot be
// attributed to a particular request; letting a later `started` overwrite the
// stamp produces samples closed by an earlier command's frame. Declining to
// overwrite means the first answer to arrive always belongs to the open stamp.
//
// The server's own number is deliberately stale. There is no interval, no
// heartbeat and no per-viewer tick anywhere in this file, because an idle viewer
// costing exactly zero is worth more than a fresh number nobody is staring at:
// the p50 rides pushes that were happening anyway (patchLatency, from push()),
// and is restated only on a material move — ≥20% relative and ≥0.05 ms absolute,
// at most once per 10 seconds per screen. The relative test is the honest one at
// a scale where the reader holds 0.4 ms against 180 ms; the absolute floor stops
// a p50 near zero repatching on noise; the gap bounds a genuinely thrashing
// server at ~2 bytes per second per viewer.
//
// It publishes the region, the server p50 and the reader's own RTT, and nothing
// else. No IP, no sheet counts, no disk path, uptime or memory: the service has
// no authentication and infrastructure detail is an invitation to probe it. Load
// average is excluded on its own merits, a weak signal that invites "is it
// struggling?" for no diagnostic gain. Region is a flag defaulting to empty, in
// which case the tooltip says "one server, in one region" rather than printing a
// placeholder.

import (
	"html"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// latencyChipID is the chip. It carries the effect that receives the server's
	// p50 and issues no request of its own, so the one-element-per-writer rule
	// that `#cl`/`#fl`/`#pv`/`#st` follow does not reach it, any more than it
	// reaches `#ex` or `#bf`: Datastar keys request cancellation on the element,
	// and an element that never fetches cannot cancel anything.
	latencyChipID = "lt"
	// latencyValueID is the one line of text. It is written by JS rather than
	// by a `data-text` binding because the round trip is a client measurement
	// that never becomes a signal — see latencyScript.
	latencyValueID = "ltv"
	// latencyTipID is the hover panel. The tooltip is the point of the feature,
	// not decoration on it, so it is a real element rather than a `title`
	// attribute: a native tooltip on macOS arrives after a second, truncates,
	// and cannot be styled to read as three short paragraphs.
	latencyTipID = "ltp"
	// The two live figures inside the panel: slots in prose the server wrote, so
	// the copy lives in one place and the client never composes a sentence.
	latencyTipRTTID = "ltpr"
	latencyTipSrvID = "ltps"
)

// ─── The rolling server-side p50 ──────────────────────────────────────────────

// latencySamples is how many recent commands the p50 is taken over. 512 is a few
// seconds of a 64-connection load test and an entire session of ordinary use, so
// the figure means "recently" under both.
const latencySamples = 512

// latencyRing is a fixed ring of command durations in milliseconds.
//
// It is process-wide rather than per-server, like obsLog and tracer: it is
// instrumentation about the process, and the page shell is rendered by a
// package-level function that has to seed the chip with a real number at first
// paint rather than waiting for a push to correct a zero.
type latencyRing struct {
	mu  sync.Mutex
	buf [latencySamples]float64
	n   int // how many slots are filled (saturates at len(buf))
	i   int // next slot to write
}

var commandLatency latencyRing

func (r *latencyRing) add(ms float64) {
	if ms < 0 {
		return
	}
	r.mu.Lock()
	r.buf[r.i] = ms
	r.i = (r.i + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
	r.mu.Unlock()
}

// p50 is the median of the samples held, or 0 for "nothing measured yet". Zero
// is a legal sentinel here precisely because a command that took literally no
// time did not happen.
func (r *latencyRing) p50() float64 {
	r.mu.Lock()
	if r.n == 0 {
		r.mu.Unlock()
		return 0
	}
	s := make([]float64, r.n)
	copy(s, r.buf[:r.n])
	r.mu.Unlock()
	sort.Float64s(s)
	return s[len(s)/2]
}

// noteCommand records how long a command handler took. It is called from the
// handlers, next to the `command_ms` line each already logs, so the number on
// the chip is the number in the log file rather than a second measurement of a
// slightly different thing.
//
// It cannot be a middleware: respondCommand applies the outbound half of the
// artificial delay (`-latency-ms`) inside the handler, so a wrapper around the
// mux would fold the simulated network into the very figure that exists to
// separate the two.
//
// Only commands that change a sheet are counted — cell, clear, fill, paste,
// style, colwidth, rows, cols. `/sel` and `/viewport` touch no data and run far
// faster, so mixing them in drags the median to a figure that reads as a boast
// and answers a question nobody asked. The tooltip promises the server's share
// of your change, so the figure has to be over changes.
func noteCommand(d time.Duration) { commandLatency.add(msf(d)) }

// noteCommandNow records a command that ends here and hands back its duration,
// so the call can be spliced into the `command_ms` argument the handler was
// already writing. Measuring twice would put a different number on the chip from
// the one in the log, and checking one against the other is most of what makes
// the chip believable.
func noteCommandNow(started time.Time) time.Duration {
	d := time.Since(started)
	commandLatency.add(msf(d))
	return d
}

// ─── Region ───────────────────────────────────────────────────────────────────

// regionFlag is set by main.go and read at render time rather than captured at
// init, for the same reason pendingDelay is: it keeps the flag's ownership in
// one place and keeps page-shell tests working without a flag parse.
var regionFlag *string

func serverRegion() string {
	if regionFlag == nil {
		return ""
	}
	return strings.TrimSpace(*regionFlag)
}

// ─── The chip, as page-shell furniture ────────────────────────────────────────

// latencyChipCSS is spliced into buildGridCSS so the page keeps one stylesheet.
//
// `flex:0 0 auto` is the whole layout decision. The line must never be
// ellipsised: half a split — `you ↔ server 365 m…` — is precisely the single
// blended number this feature exists to replace, shown under the pretence of a
// split. It cannot be `overflow:hidden` either, because the panel is an
// absolutely positioned child and clipping the chip would clip the explanation.
const latencyChipCSS = `#` + latencyChipID + `{position:relative;flex:0 0 auto;cursor:default}
#` + latencyValueID + `{display:block;white-space:nowrap;border-bottom:1px dotted #dadce0;color:#5f6368;font:11px/1.6 ui-monospace,SFMono-Regular,monospace}
#` + latencyTipID + `{display:none;position:absolute;top:calc(100% + 8px);right:0;z-index:10;width:23rem;max-width:80vw;padding:10px 12px;border:1px solid #dadce0;border-radius:4px;background:#fff;color:#3c4043;font:12px/1.5 Arial,Helvetica,sans-serif;white-space:pre-line;box-shadow:0 2px 6px 2px rgba(60,64,67,.15)}
#` + latencyTipID + ` b{font:inherit;font-weight:500}
#` + latencyChipID + `:hover #` + latencyTipID + `,#` + latencyChipID + `:focus-within #` + latencyTipID + `{display:block}
`

// latencySignal is the server's p50, as seeded into the page shell and as
// patchLatency restates it. Underscore-prefixed means local: the server patches
// it down and the client never ships it back, which keeps a fact the server
// already knows off every request the page makes.
const latencySignal = "_lp"

// latencySignalSeed is the `,_lp:0.412` fragment of `data-signals`. Seeding at
// render costs one number in an attribute that already exists, once per page
// load; without it every session would open on `server —` and stay there until
// something woke the connection, which on a quiet sheet could be a long time. A
// server that has genuinely handled no writes yet says so, and fills in on the
// first one.
func latencySignalSeed() string {
	return "," + latencySignal + ":" + latencyNum(commandLatency.p50())
}

// latencyNum renders a millisecond figure for JSON. Three decimals because the
// numbers reported here are sub-millisecond, and rounding at the transport
// would be the transport deciding what the reader may see.
func latencyNum(v float64) string {
	return strconv.FormatFloat(v, 'f', 3, 64)
}

// latencyChipHTML is the chip: one span, one line of text, one hover panel and
// one effect. No request, no per-cell byte, and nothing a push sends can reach
// it — it lives at the right-hand end of `#fb`, page-shell markup on the same
// terms as `#sb`, `#cb` and the aggregate line. See formulaBarHTML for why that
// strip and not the toolbar.
//
// Every word is rendered here and none of it is in the script: the two live
// figures sit in slots of their own (`#ltpr`, `#ltps`) and the client rewrites
// those text nodes and nothing else, so the copy exists exactly once and cannot
// drift. `tabindex` makes the panel reachable without a mouse and
// `:focus-within` does the rest; the element is inert otherwise.
func latencyChipHTML() string {
	p50 := commandLatency.p50()
	// The seed is an attribute rather than an interpolation into the script.
	// The script is a hashed, immutable, shared asset (assets.go), so a
	// per-request p50 baked into it would change it on every request. Numbers
	// that vary per request belong in the document; the code that reads them
	// does not.
	return `<span id="` + latencyChipID + `" tabindex="0" data-s="` + latencyNum(p50) + `"` +
		` data-effect="if(window.__ss)window.__ss.ltSrv($` + latencySignal + `)">` +
		`<span id="` + latencyValueID + `">` + html.EscapeString(latencyLine(0, p50)) + `</span>` +
		latencyTipHTML(0, p50, serverRegion()) +
		`</span>`
}

// latencyFmt is the one number format on the chip, and it exists in Go and in
// JavaScript because the first paint is server-rendered and every paint after it
// is not. One decimal below 10 ms so 0.35 reads as 0.4 rather than 0; whole
// milliseconds above it, because nobody needs 180.3.
func latencyFmt(v float64) string {
	if v <= 0 {
		return "—"
	}
	if v < 10 {
		return strconv.FormatFloat(v, 'f', 1, 64) + " ms"
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " ms"
}

// latencyLine is the chip's one line. rtt of 0 means "not measured yet", which
// on one line of a toolbar is an em dash.
func latencyLine(rtt, p50 float64) string {
	return "you ↔ server " + latencyFmt(rtt) + " · server " + latencyFmt(p50)
}

// latencyWords is a figure as the tooltip says it, where there is room for a
// sentence and an em dash would read as a gap in the prose. Both halves get the
// same treatment: after a restart the round trip and the median are equally
// unknown, and saying so twice in one voice beats one dash and one sentence.
func latencyWords(v float64) string {
	if v <= 0 {
		return "not yet measured"
	}
	return latencyFmt(v)
}

// latencyTipHTML is the tooltip, and it is the point of the feature. Three
// things it has to get right, in this order:
//
//  1. It names the cause before the excuse. "One server, in this region" is the
//     fact and everything else follows from it; starting with an apology would
//     bury the reason under the regret.
//  2. It quantifies both halves, so the reader can check the claim rather than
//     take it — somebody 200 ms away can see for themselves that 199 ms of it is
//     not the application.
//  3. It says what would change, which is the difference between an explanation
//     and a disclaimer. The fix is topological — more origins, or one nearer the
//     reader — and so a deployment decision, not an architectural one.
//
// Measured and unmeasured share one sentence shape ("…is not yet measured" reads
// where "…is 180 ms" does), so the client only swaps a number into it and never
// has to compose a paragraph. The blank line is a real newline under
// `white-space:pre-line`: the cheapest paragraph break there is.
func latencyTipHTML(rtt, p50 float64, region string) string {
	where, from := "One server, in one region.", ""
	if region != "" {
		esc := html.EscapeString(region)
		where, from = "One server, in "+esc+".", " between you and "+esc
	}
	return `<span id="` + latencyTipID + `">` + where +
		" That is the whole explanation.\n\n" +
		"Your round trip — keystroke to confirmed value — is " +
		`<b id="` + latencyTipRTTID + `">` + html.EscapeString(latencyWords(rtt)) + `</b>` +
		". The server’s own share of it, its running median for commands that " +
		"change a sheet, is " +
		`<b id="` + latencyTipSrvID + `">` + html.EscapeString(latencyWords(p50)) + `</b>` +
		". The rest is network distance" + from + ".\n\n" +
		"Closing that gap is a deployment decision — more origins, or one nearer you — " +
		"not an architectural one. Nothing in the application is doing that work." +
		`</span>`
}

// latencyScript is the client half. It hangs off `window.__ss` like every other
// helper on this page, so it must run after clientTimingScript has created the
// object.
//
// The round trip is not a signal. Datastar exposes no API for writing a
// signal from plain JavaScript, and the measurement happens inside a
// `document`-level listener rather than a `data-on` expression, so there is no
// element whose attribute could hold it. Writing the text directly is also
// cheaper: a client-owned fact that never travels would otherwise ride every
// request the page makes for the rest of the session.
//
// The server's half comes the other way, through `$_lp` and one `data-effect` —
// the shape `extentEffectExpr` uses to hand `--rows` to T.setRows. One writer of
// the text, two sources for it, and no copy in the script: every word the reader
// sees was written by latencyTipHTML, and this only swaps numbers into slots.
func latencyScript() string {
	return `<script>(function(){var T=window.__ss;if(!T)return;
T.ltS=+((document.getElementById('` + latencyChipID + `')||{dataset:{}}).dataset.s||0);T.ltR=0;T.ltQ=[];T.ltT=0;
T.ltFmt=function(v){return v>0?(v<10?v.toFixed(1):v.toFixed(0))+' ms':'—';};
T.ltMed=function(a){var b=a.slice(0).sort(function(x,y){return x-y;});return b[b.length>>1];};
T.ltSet=function(id,s){var e=document.getElementById(id);if(e)e.textContent=s;};
T.ltPaint=function(){var s=T.ltFmt(T.ltS),r=T.ltFmt(T.ltR);
 T.ltSet('` + latencyValueID + `','you ↔ server '+r+' · server '+s);
 T.ltSet('` + latencyTipRTTID + `',T.ltR>0?r:'not yet measured');
 T.ltSet('` + latencyTipSrvID + `',T.ltS>0?s:'not yet measured');};
T.ltSrv=function(v){if(typeof v!=='number'||!(v>=0))return;T.ltS=v;T.ltPaint();};
T.ltDone=function(t){if(!T.ltT)return;var d=t-T.ltT;T.ltT=0;
 if(!(d>0)||d>60000)return;T.ltQ.push(d);if(T.ltQ.length>5)T.ltQ.shift();
 T.ltR=T.ltMed(T.ltQ);T.ltPaint();};
document.addEventListener('datastar-fetch',function(e){var d=e.detail;if(!d)return;
 var t=performance.now(),el=d.el&&d.el.id;
 // #live is the held-open stream. Its 'started' is the page connecting and its
 // 'finished'/'error' is a DISCONNECT — neither is a round trip, and timing the
 // second against the first would report the length of the session.
 if(d.type==='started'){if(el&&el!=='live'&&!T.ltT)T.ltT=t;return;}
 if(d.type==='finished'||d.type==='error'){if(el&&el!=='live')T.ltDone(t);return;}
 // The confirming SSE frame. It is dispatched with d.el set to the element that
 // opened the stream (#live), which is why the frame types are tested here and
 // not inside the guard above: the answer to a command arrives on somebody
 // else's element, and that is the whole shape of this architecture.
 if(d.type==='datastar-patch-elements'||d.type==='datastar-patch-signals')T.ltDone(t);});
T.ltPaint();})();</script>`
}
