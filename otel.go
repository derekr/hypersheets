package main

// otel.go — tracing, and the client-side clock.
//
// The server side of this prototype is fast enough to be uninteresting: a
// viewport command returns in well under a millisecond and the resulting SSE
// patch lands single-digit milliseconds later. The latency a human notices while
// scrolling is therefore spent where the server cannot see it — in the debounce
// window before a request is even issued, and in the browser's morph and layout
// of a ~3,900-cell table afterwards. So this file does two things:
//
//  1. Wires an OpenTelemetry tracer that writes JSON-lines to a file. No
//     collector, no network, nothing to stand up: `stdouttrace` with a *os.File
//     writer produces one span object per line, which analyze.go reads back.
//  2. Carries client-measured durations into those spans and logs, so one log
//     line shows the whole chain. The browser stamps performance.now() when a
//     scroll fires, when the debounced handler decides, and when a patch is
//     received and applied; those numbers ride the next viewport command as
//     signals.
//
// Parenting across goroutines. A viewport POST and the SSE push it causes run on
// different goroutines connected by a wake channel that carries no payload
// (registry.go), so the command stashes its context on the screen (setCause) and
// the live loop pops it (takeCause) as the parent of the push. Edits fan out to
// many connections, so their context is stashed per-sheet instead and every
// woken connection parents off it. A child span whose parent has already ended
// is legal, and is exactly what an asynchronous fan-out looks like.

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// tracer is the instrumentation handle every span in this program comes from. It
// starts as a no-op, so calls made before StartTracing — and every call when
// tracing is off with `-trace-file=` — cost nothing and cannot panic.
//
// It is assigned directly rather than read through otel.Tracer(): the global
// delegating provider can only be swapped once per process, so a second
// SetTracerProvider leaves already-handed-out tracers pointing at the first
// provider and silently writes the second run's spans into the first run's file.
// Owning the handle here makes StartTracing idempotent.
var tracer trace.Tracer = noop.NewTracerProvider().Tracer("sheetstream")

// StartTracing points the global tracer provider at a JSON-lines file. An empty
// path disables tracing and returns a no-op shutdown, so `-trace-file=` is a
// supported way to run without any of this.
//
// The returned func must be called on shutdown: the batch processor holds spans
// in memory for up to its batch timeout, so a process killed without it loses
// the tail of the file, or truncates a line mid-object.
func StartTracing(path string) (shutdown func(context.Context) error, err error) {
	if path == "" {
		return func(context.Context) error { return nil }, nil
	}
	// Append, not Create: os.Create truncates, and a deploy is exactly when
	// the lines from just before it matter most. Rotation is logrotate's job,
	// not ours.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("trace file: %w", err)
	}
	exp, err := stdouttrace.New(stdouttrace.WithWriter(f))
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithMaxQueueSize(16384),
			sdktrace.WithMaxExportBatchSize(2048),
			sdktrace.WithBatchTimeout(500*time.Millisecond),
		),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", "sheetstream"),
		)),
	)
	tracer = tp.Tracer("sheetstream")
	otel.SetTracerProvider(tp)
	return func(ctx context.Context) error {
		// tracer is deliberately NOT reassigned here. It is written once at
		// startup, before anything serves, and read without synchronisation by
		// every request goroutine; writing it again during shutdown — while
		// held-open streams are still running — would be a data race. Shutdown
		// already makes the provider stop recording, so spans started after this
		// point are dropped before they can reach the file.
		err := tp.Shutdown(ctx)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}, nil
}

// ─── Client-reported timings ──────────────────────────────────────────────────

// clientTiming is what the browser measured, in the browser's own clock. Every
// field is milliseconds from performance.now() except the two raw stamps, which
// are absolute performance.now() values only ever subtracted from each other.
//
// DebounceMs is the number this file exists to produce: the gap between the
// first scroll event of a burst and the moment the debounced handler ran.
// `data-on:scroll__debounce.120ms` cannot fire until scrolling stops, so during
// a continuous drag this is unbounded — it is the length of the drag, not 120ms.
type clientTiming struct {
	ScrollAt   float64 // performance.now() at the first scroll event of this burst
	HandlerAt  float64 // performance.now() when the debounced handler ran
	DebounceMs float64 // HandlerAt - ScrollAt
	QuietMs    float64 // HandlerAt - last scroll event: how much of the wait was the debounce timer
	// The previous patch, reported one command late because it had not
	// happened yet when the command that caused it was sent.
	WaitMs   float64 // post issued -> patch frame received
	MorphMs  float64 // patch received -> last DOM mutation (Datastar's morph)
	RenderMs float64 // patch received -> the frame after the browser laid it out
	Bytes    float64 // size of the patched elements payload the browser saw
	Scrolls  float64 // scroll events seen so far this session
	Skips    float64 // handler runs that decided not to post (scrolled inside the buffer)
}

// any reports whether the browser told us anything at all. A client with no
// harness — curl, a load-test script — sends zeros for every field, and
// recording those as measurements would drag every percentile toward zero.
func (t clientTiming) any() bool {
	return t.ScrollAt != 0 || t.HandlerAt != 0 || t.DebounceMs != 0 || t.QuietMs != 0 ||
		t.WaitMs != 0 || t.MorphMs != 0 || t.RenderMs != 0 ||
		t.Bytes != 0 || t.Scrolls != 0 || t.Skips != 0
}

func (t clientTiming) attrs() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.Float64("client.debounce_ms", t.DebounceMs),
		attribute.Float64("client.quiet_ms", t.QuietMs),
		attribute.Float64("client.prev_wait_ms", t.WaitMs),
		attribute.Float64("client.prev_morph_ms", t.MorphMs),
		attribute.Float64("client.prev_render_ms", t.RenderMs),
		attribute.Float64("client.prev_bytes", t.Bytes),
		attribute.Float64("client.scroll_events", t.Scrolls),
		attribute.Float64("client.skipped_posts", t.Skips),
	}
}

// ─── Per-screen observability state ───────────────────────────────────────────

// screenObs is everything the tracing layer needs to hang off a screen without
// changing the shape of screen itself: the cause of the next wake, the last
// client report, and this connection's own post-compression byte counter.
type screenObs struct {
	mu        sync.Mutex
	cause     context.Context
	causeKind string
	causeAt   time.Time
	client    clientTiming

	// wire counts bytes after compression for this connection only. The global
	// wireBytes counter in http.go is shared by every stream, so it cannot
	// answer "how big was that patch on the wire".
	wire atomic.Uint64
}

func newScreenObs() *screenObs { return &screenObs{} }

// setCause records why the next wake is about to happen, and under what trace.
func (o *screenObs) setCause(ctx context.Context, kind string, client clientTiming) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.cause, o.causeKind, o.causeAt = ctx, kind, time.Now()
	if client.any() {
		o.client = client
	}
	o.mu.Unlock()
}

// takeCause consumes the pending cause. A wake with no recorded cause (a
// heartbeat-adjacent race, or an edit whose per-sheet cause has already been
// taken) reports kind "unknown" rather than inventing a parent.
func (o *screenObs) takeCause() (ctx context.Context, kind string, at time.Time, client clientTiming) {
	if o == nil {
		return context.Background(), "unknown", time.Time{}, clientTiming{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	ctx, kind, at, client = o.cause, o.causeKind, o.causeAt, o.client
	o.cause, o.causeKind, o.causeAt = nil, "", time.Time{}
	if ctx == nil {
		ctx, kind = context.Background(), "unknown"
	}
	return ctx, kind, at, client
}

func (o *screenObs) lastClient() clientTiming {
	if o == nil {
		return clientTiming{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.client
}

// ─── Attribute helpers ────────────────────────────────────────────────────────

// connAttrs is the identity every span that touches a connection carries:
// which screen, which sheet, and the exact buffer that screen is showing.
func connAttrs(scr *screen) []attribute.KeyValue {
	if scr == nil {
		return nil
	}
	lo, hi := scr.window()
	return []attribute.KeyValue{
		attribute.String("conn.id", scr.id),
		attribute.String("sheet.id", scr.sheetID),
		attribute.Int("buffer.lo_row", lo),
		attribute.Int("buffer.hi_row", hi),
	}
}

// spanIDs pulls the ids out of a context for a log line. Empty strings when
// tracing is off, which keeps the log readable in that mode instead of full of
// all-zero ids.
func spanIDs(ctx context.Context) (traceID, spanID string) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}

// ─── The client-side clock, as shipped to the browser ─────────────────────────

// clientTimingScript is injected into the page shell. It is deliberately not a
// Datastar expression: two of the three things it measures happen at moments no
// `data-on` attribute can observe.
//
//   - The raw scroll event. `data-on:scroll__debounce.120ms` does not run until
//     scrolling has stopped for 120ms, so the debounced handler cannot say when
//     the burst started. A separate passive listener can.
//   - Patch arrival. Datastar dispatches a `datastar-fetch` CustomEvent
//     on `document` for every SSE frame, with `detail.type` set to the SSE event
//     name and `detail.argsRaw` holding the parsed data lines — verified in the
//     bundle, which is the only place this contract is written down. So
//     `detail.type === 'datastar-patch-elements'` is the exact instant the
//     browser has the HTML and has not yet morphed it. This is a classic inline
//     script in the body, so it registers before the deferred Datastar module
//     installs its own handler and the stamp is taken before the morph.
//   - Morph completion. Datastar exposes no "morph finished" event, so a
//     MutationObserver on the scroll container is the measurement. Its callback
//     is a microtask after the mutation batch, so it always lands after the
//     stamp above.
//
// RenderMs additionally waits two animation frames: the first lays out and paints
// the new table, the second starts once that frame is done. That is the closest
// a page can get to "the user can see it".
const clientTimingScript = `<script>(function(){
var T={scrolls:0,posts:0,skips:0,patches:0,
 firstScrollAt:null,lastScrollAt:null,handlerAt:null,postedAt:null,
 debounceMs:0,quietMs:0,waitMs:0,morphMs:0,renderMs:0,bytes:0,samples:[]};
window.__scrollTiming=T;window.__ss=T;
var vp=document.getElementById('vp');
if(vp){vp.addEventListener('scroll',function(){var t=performance.now();T.scrolls++;
 if(T.firstScrollAt===null)T.firstScrollAt=t;T.lastScrollAt=t;},{passive:true});}
// mark() is called by the debounced scroll expression, for BOTH decisions.
T.mark=function(post){var t=performance.now();
 var f=T.firstScrollAt===null?t:T.firstScrollAt,l=T.lastScrollAt===null?t:T.lastScrollAt;
 T.handlerAt=t;T.debounceMs=+(t-f).toFixed(2);T.quietMs=+(t-l).toFixed(2);
 T.firstScrollAt=null;
 if(post){T.posts++;T.postedAt=t;}else{T.skips++;}
 var s={post:!!post,debounceMs:T.debounceMs,quietMs:T.quietMs,
  waitMs:T.waitMs,morphMs:T.morphMs,renderMs:T.renderMs,bytes:T.bytes};
 T.samples.push(s);if(T.samples.length>200)T.samples.shift();
 return {scroll:+f.toFixed(2),post:+t.toFixed(2),debounce:T.debounceMs,quiet:T.quietMs,
  wait:T.waitMs,morph:T.morphMs,render:T.renderMs,bytes:T.bytes,
  scrolls:T.scrolls,skips:T.skips};};
var recvAt=null,burstAt=null;
document.addEventListener('datastar-fetch',function(e){var d=e.detail;
 if(!d||d.type!=='datastar-patch-elements')return;
 var t=performance.now();
 // A scroll is now 1-3 element frames (remove, prepend, append) instead of one
 // fat morph, so a per-FRAME stamp would report a third of the cost and a
 // third of the bytes. Frames of one push land back to back; anything after a
 // 100ms gap is a new push.
 if(recvAt===null||t-recvAt>100){burstAt=t;T.bytes=0;}
 recvAt=t;T.patches++;
 T.bytes+=(d.argsRaw&&d.argsRaw.elements)?d.argsRaw.elements.length:0;
 if(T.postedAt!==null){T.waitMs=+(t-T.postedAt).toFixed(2);T.postedAt=null;}
 var r0=burstAt;
 requestAnimationFrame(function(){requestAnimationFrame(function(){
  T.renderMs=+(performance.now()-r0).toFixed(2);});});});
if(vp){new MutationObserver(function(){if(burstAt!==null)
 T.morphMs=+(performance.now()-burstAt).toFixed(2);})
 .observe(vp,{childList:true,subtree:true,characterData:true,attributes:true});}
})();</script>`
