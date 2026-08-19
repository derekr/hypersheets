package main

// otel_test.go — the instrumentation has to be trustworthy before its output
// is worth reading, so what is pinned here is exactly the set of things that
// would silently produce plausible-but-wrong numbers if they broke:
//
//   - spans reach the file, as one parseable JSON object per line, with real
//     durations and their attributes intact;
//   - a child span started after its parent ended still lands in the parent's
//     trace, because the whole viewport→push correlation depends on that;
//   - the cause handoff between the command goroutine and the render goroutine
//     is take-once;
//   - the debounce arithmetic;
//   - the client harness is actually shipped, hooks the events it claims to
//     hook, and reports on BOTH branches of the scroll decision. A harness that
//     only reported when it posted would make the debounce look free.

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

func TestStartTracingWritesJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	shutdown, err := StartTracing(path)
	if err != nil {
		t.Fatalf("StartTracing: %v", err)
	}

	ctx, parent := tracer.Start(context.Background(), "viewport.command")
	_, child := tracer.Start(ctx, "registry.set_buffer")
	time.Sleep(2 * time.Millisecond)
	child.End()
	parent.End()

	// The push happens on another goroutine and therefore AFTER the command
	// span has ended. If this stops sharing a trace id, a scroll stops being
	// one readable trace.
	_, late := tracer.Start(ctx, "conn.wake")
	late.End()

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	spans := readSpans(t, path)
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3", len(spans))
	}

	byName := map[string]spanLine{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	for _, want := range []string{"viewport.command", "registry.set_buffer", "conn.wake"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("missing span %q (have %v)", want, keysOf(byName))
		}
	}

	traceID := byName["viewport.command"].SpanContext.TraceID
	if traceID == "" {
		t.Fatal("no trace id on the root span")
	}
	for name, s := range byName {
		if s.SpanContext.TraceID != traceID {
			t.Errorf("%s: trace id %s, want %s — the trace is fragmented", name, s.SpanContext.TraceID, traceID)
		}
	}
	if got := byName["conn.wake"].Parent.SpanID; got != byName["viewport.command"].SpanContext.SpanID {
		t.Errorf("conn.wake parent = %q, want the command span %q; a span started after its parent ended must still be parented",
			got, byName["viewport.command"].SpanContext.SpanID)
	}
	if d := byName["registry.set_buffer"].EndTime.Sub(byName["registry.set_buffer"].StartTime); d < time.Millisecond {
		t.Errorf("duration %v — spans are not being timed", d)
	}
}

func TestStartTracingDisabled(t *testing.T) {
	shutdown, err := StartTracing("")
	if err != nil {
		t.Fatalf("StartTracing(\"\"): %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestSpanAttributesSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	shutdown, err := StartTracing(path)
	if err != nil {
		t.Fatalf("StartTracing: %v", err)
	}
	scr := &screen{id: "abc123", sheetID: "demo", loRow: 100, hiRow: 399, obs: newScreenObs()}
	_, span := tracer.Start(context.Background(), "sse.patch")
	span.SetAttributes(connAttrs(scr)...)
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	spans := readSpans(t, path)
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	attrs := map[string]any{}
	for _, a := range spans[0].Attributes {
		attrs[a.Key] = a.Value.Value
	}
	for key, want := range map[string]any{
		"conn.id":       "abc123",
		"sheet.id":      "demo",
		"buffer.lo_row": float64(100),
		"buffer.hi_row": float64(399),
	} {
		if got, ok := attrs[key]; !ok || got != want {
			t.Errorf("attribute %s = %v (present=%v), want %v", key, got, ok, want)
		}
	}
}

func TestScreenObsCauseIsTakeOnce(t *testing.T) {
	o := newScreenObs()

	ctx, kind, at, _ := o.takeCause()
	if kind != "unknown" || ctx == nil || !at.IsZero() {
		t.Errorf("empty obs: kind=%q at=%v, want unknown and a zero time", kind, at)
	}

	want := clientTiming{DebounceMs: 2071.7}
	o.setCause(context.Background(), "viewport", want)

	_, kind, at, got := o.takeCause()
	if kind != "viewport" {
		t.Errorf("kind = %q, want viewport", kind)
	}
	if at.IsZero() {
		t.Error("cause time not recorded")
	}
	if got.DebounceMs != want.DebounceMs {
		t.Errorf("client debounce = %v, want %v", got.DebounceMs, want.DebounceMs)
	}

	// A second wake with no new command must NOT re-claim the viewport as its
	// cause, or one command would appear to have produced two renders.
	_, kind, _, sticky := o.takeCause()
	if kind != "unknown" {
		t.Errorf("second take: kind = %q, want unknown", kind)
	}
	// The client report is deliberately sticky: it is the latest thing the
	// browser told us, and an edit-driven push should still report it.
	if sticky.DebounceMs != want.DebounceMs {
		t.Errorf("client report should persist across takes, got %v", sticky.DebounceMs)
	}
}

func TestViewportSignalsTiming(t *testing.T) {
	cases := []struct {
		name         string
		sig          viewportSignals
		wantDebounce float64
		wantAny      bool
	}{
		{
			name:         "a continuous drag: two seconds of scrolling before the handler runs",
			sig:          viewportSignals{TScroll: 1000, TPost: 3071.7, TQuiet: 122.5},
			wantDebounce: 2071.7,
			wantAny:      true,
		},
		{
			name:         "a short flick: mostly the debounce timer itself",
			sig:          viewportSignals{TScroll: 5000, TPost: 5221.3, TQuiet: 121.4},
			wantDebounce: 221.3,
			wantAny:      true,
		},
		{
			name:    "a client with no harness (curl, a test script) reports nothing",
			sig:     viewportSignals{Conn: "x", Lo: 0, Hi: 40},
			wantAny: false,
		},
		{
			name: "a post stamp before the scroll stamp is nonsense and is not reported",
			sig:  viewportSignals{TScroll: 900, TPost: 100},
			// ScrollAt is still set, so the report is non-empty; the derived
			// duration is what must not be a negative.
			wantAny: true,
		},
		{
			name:    "morph reported with no scroll stamps still counts as a report",
			sig:     viewportSignals{TMorph: 42.8, TRender: 115.6},
			wantAny: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.sig.timing()
			if math.Abs(got.DebounceMs-tc.wantDebounce) > 1e-9 {
				t.Errorf("DebounceMs = %v, want %v", got.DebounceMs, tc.wantDebounce)
			}
			if got.DebounceMs < 0 {
				t.Errorf("DebounceMs = %v, must never be negative", got.DebounceMs)
			}
			if got.any() != tc.wantAny {
				t.Errorf("any() = %v, want %v", got.any(), tc.wantAny)
			}
			if n := len(got.attrs()); n != 8 {
				t.Errorf("attrs() = %d attributes, want 8", n)
			}
		})
	}
}

func TestClientHarnessIsShipped(t *testing.T) {
	shell := pageShell("demo", 0, 149, `<div id="g"></div>`, zeroAnchor())

	// The harness itself. IT LIVES IN THE CACHED BUNDLE NOW, not inline in the
	// document — see assets.go — so this asserts against appJS. The property
	// being checked is unchanged: the clock is shipped to every page.
	for _, want := range []string{
		"window.__scrollTiming", // the documented inspection point
		"window.__ss",           // the short name the scroll expression calls
		"datastar-fetch",        // Datastar v1.0.1's per-SSE-frame event
		"datastar-patch-elements",
		"MutationObserver",      // morph completion; Datastar has no event for it
		"requestAnimationFrame", // layout/paint, one frame past the morph
		"{passive:true}",        // the raw scroll listener must not block scrolling
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("bundle is missing %q — the client-side clock is not installed", want)
		}
	}

	// THE ORDERING REQUIREMENT IS THE SAME AND IS NOW ENFORCED MORE STRONGLY.
	// What matters is that the harness's `datastar-fetch` listener is registered
	// before Datastar handles the first SSE frame. This used to be arranged by
	// hand — put the inline block after `#vp` and before `#live` — and is now a
	// guarantee of the platform: a deferred classic script runs after parsing
	// and BEFORE any module script, so the whole harness is installed before
	// Datastar has executed a single line, let alone opened the stream. Both
	// elements are therefore already parsed too, which the old arrangement could
	// only manage for `#vp`.
	iBundle := strings.Index(shell, appJSPath())
	iModule := strings.Index(shell, `<script type="module"`)
	iLive := strings.Index(shell, `id="live"`)
	if iBundle < 0 || iModule < 0 || iLive < 0 {
		t.Fatalf("page is missing the bundle (%d), the module (%d) or #live (%d)", iBundle, iModule, iLive)
	}
	if iBundle > iModule {
		t.Errorf("bundle at %d must be requested before the Datastar module at %d, or the "+
			"datastar-fetch listener is not registered when the first frame arrives", iBundle, iModule)
	}

	// The signals that carry the measurements have to exist, or Datastar will
	// not ship them with the command.
	for _, sig := range []string{"tScroll:0", "tPost:0", "tQuiet:0", "tWait:0", "tMorph:0", "tRender:0", "tBytes:0", "tScrolls:0", "tSkips:0"} {
		if !strings.Contains(shell, sig) {
			t.Errorf("signal %q not declared on <body>; it would never reach the server", sig)
		}
	}
}

func TestScrollExprMeasuresBothDecisions(t *testing.T) {
	expr := scrollExpr("demo")

	// mark() must be called unconditionally, BEFORE the post decision is acted
	// on. If it were inside the `if`, the runs where the buffer absorbed the
	// scroll would be invisible and the skip count would always read zero.
	iMark := strings.Index(expr, "__ss.mark(ok)")
	iPost := strings.Index(expr, "@post(")
	if iMark < 0 {
		t.Fatalf("scroll expression never calls mark(): %s", expr)
	}
	if iPost < 0 || iMark > iPost {
		t.Errorf("mark() at %d must precede @post at %d so non-posting runs are measured", iMark, iPost)
	}

	// THE PIN. This expression is the only thing standing between a working
	// prototype and a grid that stops loading rows without saying anything, so
	// it is asserted whole rather than sampled.
	//
	// It was updated ONCE, deliberately, when scrolling stopped re-morphing the
	// `#g` wrapper: the bounds moved from `g.dataset.lo/hi` (which only stayed
	// current because every push rewrote the wrapper) to the server-owned
	// `$blo`/`$bhi` signals. If you are here because this failed, check that
	// the bounds you are reading are still ones the server patches.
	//
	// Updated a SECOND time for linkable regions, which bracket the unchanged
	// middle with two additions and change nothing between them:
	//   - the settle guard, FIRST, so the synthetic scroll event fired by an
	//     anchored load's own scrollTop assignment does not post a viewport
	//     command for the window the server just rendered;
	//   - track(r), OUTSIDE the `if`, so the address bar follows the scroll
	//     (replaceState, zero history entries) on every run of the handler and
	//     not only on the ones that reach the network.
	const want = `if(window.__ss&&window.__ss.settle(el.scrollTop))return;` +
		`const lo=$blo,hi=$bhi,r=Math.floor(el.scrollTop/22),n=Math.ceil(el.clientHeight/22);` +
		`const ok=(r-lo<50||hi-(r+n)<50);` +
		`const m=window.__ss?window.__ss.mark(ok):null;` +
		`if(m){$tScroll=m.scroll;$tPost=m.post;$tQuiet=m.quiet;$tWait=m.wait;` +
		`$tMorph=m.morph;$tRender=m.render;$tBytes=m.bytes;$tScrolls=m.scrolls;$tSkips=m.skips}` +
		`if(window.__ss)window.__ss.track(r);` +
		`if(ok){$lo=r;$hi=r+n;@post('/s/demo/viewport')}`
	if expr != want {
		t.Errorf("scroll expression changed.\n got: %s\nwant: %s", expr, want)
	}

	// The pin above is exact, so these would be redundant if the constants
	// never moved. They are not: they say WHICH parts of it are load-bearing,
	// and they fail with a useful message when a geometry constant changes.
	for _, w := range []string{
		"r-lo<" + strconv.Itoa(edgeGuardRows),
		"hi-(r+n)<" + strconv.Itoa(edgeGuardRows),
		"$lo=r;$hi=r+n;",
		"@post('/s/demo/viewport')",
		// The client writes $lo/$hi and READS $blo/$bhi. Conflating them makes
		// the handler read back its own request as the server's answer.
		"lo=$blo,hi=$bhi",
	} {
		if !strings.Contains(expr, w) {
			t.Errorf("scroll expression lost %q", w)
		}
	}
	if n := strings.Count(expr, "@post("); n != 1 {
		t.Errorf("%d posts in the scroll expression, want exactly 1", n)
	}
	// The dataset read is what went stale. It must not come back.
	if strings.Contains(expr, "dataset") {
		t.Error("scroll expression reads a data attribute again; incremental patches never rewrite #g, so it would freeze")
	}
	// Attribute-safe: the expression is embedded in a double-quoted HTML
	// attribute.
	if strings.Contains(expr, `"`) {
		t.Error("scroll expression contains a double quote and would break out of its attribute")
	}
}

func TestQuantile(t *testing.T) {
	cases := []struct {
		name   string
		sorted []float64
		q      float64
		want   float64
	}{
		{"empty", nil, 0.5, 0},
		{"single", []float64{7}, 0.95, 7},
		{"p50 of ten", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.50, 5},
		{"p95 of ten", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.95, 10},
		{"p0 clamps to the first", []float64{3, 4, 5}, 0, 3},
		{"p100 clamps to the last", []float64{3, 4, 5}, 1, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quantile(tc.sorted, tc.q); got != tc.want {
				t.Errorf("quantile(%v, %v) = %v, want %v", tc.sorted, tc.q, got, tc.want)
			}
		})
	}
}

func TestAnalyzeTrace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	shutdown, err := StartTracing(path)
	if err != nil {
		t.Fatalf("StartTracing: %v", err)
	}
	for i := 0; i < 5; i++ {
		ctx, cmd := tracer.Start(context.Background(), "viewport.command")
		cmd.SetAttributes(clientTiming{DebounceMs: float64(200 * (i + 1))}.attrs()...)
		_, p := tracer.Start(ctx, "sse.patch")
		p.SetAttributes(
			attribute.Int("bytes_raw", 198642),
			attribute.Int("bytes_compressed", 15276),
			attribute.Bool("suppressed", i == 0),
		)
		time.Sleep(time.Millisecond)
		p.End()
		cmd.End()
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	var out strings.Builder
	if err := AnalyzeTrace(path, &out); err != nil {
		t.Fatalf("AnalyzeTrace: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"SPAN DURATIONS (ms)",
		"CLIENT-REPORTED TIMINGS",
		"NUMERIC ATTRIBUTES",
		"FLAGS",
		"viewport.command",
		"sse.patch",
		"client.debounce_ms",
		"sse.patch · bytes_compressed",
		"sse.patch · suppressed",
		"10 spans",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("analyze output missing %q\n---\n%s", want, got)
		}
	}
}

func TestAnalyzeTraceTolerates(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		if err := AnalyzeTrace(filepath.Join(dir, "nope.jsonl"), &strings.Builder{}); err == nil {
			t.Error("want an error for a missing trace file")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		p := filepath.Join(dir, "empty.jsonl")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		if err := AnalyzeTrace(p, &out); err != nil {
			t.Fatalf("AnalyzeTrace: %v", err)
		}
		if !strings.Contains(out.String(), "0 spans") {
			t.Errorf("want a 0-span report, got %q", out.String())
		}
	})

	t.Run("a truncated tail does not lose the good lines", func(t *testing.T) {
		p := filepath.Join(dir, "torn.jsonl")
		good := `{"Name":"conn.wake","SpanContext":{"TraceID":"a","SpanID":"b"},"StartTime":"2026-08-16T15:48:44.774312-07:00","EndTime":"2026-08-16T15:48:44.780312-07:00","Attributes":[]}`
		if err := os.WriteFile(p, []byte(good+"\n{\"Name\":\"conn.wa"), 0o600); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		if err := AnalyzeTrace(p, &out); err != nil {
			t.Fatalf("AnalyzeTrace: %v", err)
		}
		s := out.String()
		if !strings.Contains(s, "1 spans") || !strings.Contains(s, "1 unparseable lines") {
			t.Errorf("want 1 span and 1 bad line reported, got:\n%s", s)
		}
		if !strings.Contains(s, "conn.wake") {
			t.Errorf("the intact span was dropped:\n%s", s)
		}
	})
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func readSpans(t *testing.T, path string) []spanLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer f.Close()
	var out []spanLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s spanLine
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("trace line is not JSON: %v\n%s", err, line)
		}
		out = append(out, s)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan trace: %v", err)
	}
	return out
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
