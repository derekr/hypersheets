package main

// limits_test.go — the buckets, the caps, and the middleware.
//
// TIME IS DRIVEN, NEVER SLEPT. Every rate test moves a fake clock, so the suite
// asserts the refill arithmetic exactly rather than approximately and stays
// fast enough to run on every build.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── Token buckets ────────────────────────────────────────────────────────────

func TestTokenBucketBurstThenRefill(t *testing.T) {
	b := newTokenBuckets(10, 5) // 10/s, burst 5
	var now time.Duration

	for i := range 5 {
		if ok, _ := b.allow("a", now); !ok {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	ok, wait := b.allow("a", now)
	if ok {
		t.Fatal("the 6th request in a burst of 5 was allowed")
	}
	// One token at 10/s is 100ms, but Retry-After never advertises less than a
	// second — a client told "retry in 0" retries immediately.
	if wait != time.Second {
		t.Fatalf("wait = %v, want 1s floor", wait)
	}

	// 100 ms buys exactly one token.
	now += 100 * time.Millisecond
	if ok, _ := b.allow("a", now); !ok {
		t.Fatal("refused after 100ms at 10/s, which is one full token")
	}
	if ok, _ := b.allow("a", now); ok {
		t.Fatal("allowed twice on one refilled token")
	}

	// A long idle refills to the burst and no further.
	now += time.Hour
	for i := range 5 {
		if ok, _ := b.allow("a", now); !ok {
			t.Fatalf("request %d after an hour idle was refused", i+1)
		}
	}
	if ok, _ := b.allow("a", now); ok {
		t.Fatal("an hour idle refilled past the burst")
	}
}

func TestTokenBucketKeysAreIndependent(t *testing.T) {
	b := newTokenBuckets(1, 2)
	var now time.Duration
	for range 2 {
		if ok, _ := b.allow("a", now); !ok {
			t.Fatal("a was refused inside its own burst")
		}
	}
	if ok, _ := b.allow("a", now); ok {
		t.Fatal("a was allowed past its burst")
	}
	if ok, _ := b.allow("b", now); !ok {
		t.Fatal("b was refused because a had spent its tokens")
	}
}

func TestTokenBucketDisabledIsNil(t *testing.T) {
	for _, tc := range []struct{ rate, burst float64 }{{0, 10}, {-1, 10}, {10, 0}} {
		if b := newTokenBuckets(tc.rate, tc.burst); b != nil {
			t.Fatalf("rate=%v burst=%v produced a live bucket", tc.rate, tc.burst)
		}
	}
	var b *tokenBuckets
	if ok, _ := b.allow("a", 0); !ok {
		t.Fatal("a nil (disabled) bucket refused a request")
	}
}

func TestTokenBucketSweepsFullKeys(t *testing.T) {
	b := newTokenBuckets(10, 5)
	var now time.Duration
	for i := range 100 {
		b.allow("ip-"+strconv.Itoa(i), now)
	}
	if len(b.at) != 100 {
		t.Fatalf("map holds %d keys, want 100", len(b.at))
	}
	// Every one of those is one token down and refills in 100ms; well past the
	// sweep interval they are all full, so the next new key drops them all.
	now += bucketSweepEvery + time.Second
	b.allow("fresh", now)
	if len(b.at) != 1 {
		t.Fatalf("after a sweep the map holds %d keys, want just the fresh one", len(b.at))
	}
}

func TestTokenBucketConcurrent(t *testing.T) {
	// The buckets are shared by every request goroutine; -race must be clean and
	// exactly `burst` requests must get through.
	b := newTokenBuckets(0.0001, 50)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := b.allow("one", 0); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 50 {
		t.Fatalf("%d of 200 concurrent requests allowed, want exactly the burst of 50", allowed)
	}
}

// ─── Concurrency counters ─────────────────────────────────────────────────────

func TestLiveCountsPerKeyAndTotal(t *testing.T) {
	var c liveCounts
	for i := range 3 {
		if !c.acquire("a", 3, 5) {
			t.Fatalf("acquire %d for a was refused under a per-key cap of 3", i+1)
		}
	}
	if c.acquire("a", 3, 5) {
		t.Fatal("a acquired a 4th stream under a per-key cap of 3")
	}
	if !c.acquire("b", 3, 5) {
		t.Fatal("b was refused although only a was at its per-key cap")
	}
	if !c.acquire("b", 3, 5) {
		t.Fatal("b was refused at 2 of 3")
	}
	if c.acquire("b", 3, 5) {
		t.Fatal("the total cap of 5 was exceeded")
	}
	c.release("a")
	if !c.acquire("b", 3, 5) {
		t.Fatal("releasing one of a's streams did not free the total")
	}
	// Releasing to zero must delete the key rather than leave a 0 entry behind.
	c.release("a")
	c.release("a")
	c.release("b")
	c.release("b")
	c.release("b")
	if n, total := c.snapshot(); n != 0 || total != 0 {
		t.Fatalf("after releasing everything: %d keys, %d total; want 0, 0", n, total)
	}
}

// ─── Classification ───────────────────────────────────────────────────────────

func TestClassifyCoversEveryRoute(t *testing.T) {
	tests := []struct {
		method, path string
		want         reqClass
		wantID       string
	}{
		{"GET", "/", classIndex, ""},
		{"POST", "/sheets", classCreate, ""},
		{"GET", "/s/demo", classPage, "demo"},
		{"GET", "/s/demo/live", classStream, "demo"},
		{"POST", "/s/demo/cell", classWrite, "demo"},
		{"POST", "/s/demo/clear", classWrite, "demo"},
		{"POST", "/s/demo/fill", classWrite, "demo"},
		{"POST", "/s/demo/paste", classWrite, "demo"},
		{"POST", "/s/demo/style", classWrite, "demo"},
		{"POST", "/s/demo/colwidth", classWrite, "demo"},
		{"POST", "/s/demo/rows", classWrite, "demo"},
		{"POST", "/s/demo/cols", classWrite, "demo"},
		{"POST", "/s/demo/viewport", classNav, "demo"},
		{"POST", "/s/demo/sel", classNav, "demo"},
		// Shapes that must not be mistaken for a route.
		{"GET", "/s/demo/cell", classOther, ""},
		{"POST", "/s/demo/live", classOther, ""},
		{"POST", "/s//cell", classOther, ""},
		{"POST", "/s/demo/cell/extra", classOther, ""},
		{"POST", "/s/demo", classOther, ""},
		{"GET", "/s/", classOther, ""},
		{"GET", "/s/demo/", classOther, ""},
		{"GET", "/favicon.ico", classOther, ""},
		{"GET", "/sheets", classOther, ""},
	}
	for _, tc := range tests {
		got, id := classify(tc.method, tc.path)
		if got != tc.want || id != tc.wantID {
			t.Errorf("classify(%s %s) = %v/%q, want %v/%q",
				tc.method, tc.path, got, id, tc.want, tc.wantID)
		}
	}
}

// TestClassifyKnowsEveryRegisteredRoute is the guard against a new route being
// added to Routes() and silently landing in classOther, unlimited. It reads the
// pattern list out of http.go's source rather than trusting this file's memory.
func TestClassifyKnowsEveryRegisteredRoute(t *testing.T) {
	src, err := os.ReadFile("http.go")
	if err != nil {
		t.Fatal(err)
	}
	// Routes that are deliberately unclassified, with the reason. Empty today:
	// every route in Routes() draws on one of the buckets.
	exempt := map[string]string{}
	found := 0
	for _, line := range strings.Split(string(src), "\n") {
		i := strings.Index(line, `mux.HandleFunc("`)
		if i < 0 {
			continue
		}
		rest := line[i+len(`mux.HandleFunc("`):]
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			continue
		}
		pattern := rest[:j]
		found++
		if _, ok := exempt[pattern]; ok {
			continue
		}
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Errorf("route %q has no method", pattern)
			continue
		}
		path = strings.ReplaceAll(path, "{sheetID}", "demo")
		path = strings.ReplaceAll(path, "{$}", "")
		if class, _ := classify(method, path); class == classOther {
			t.Errorf("route %q (%s %s) is not classified — it would be unlimited. "+
				"Add it to classify() in limits.go or to the exempt list here.",
				pattern, method, path)
		}
	}
	if found < 12 {
		t.Fatalf("only found %d routes in http.go; the scrape is broken", found)
	}
}

// ─── Client IP ────────────────────────────────────────────────────────────────

func TestClientIPIgnoresForwardedByDefault(t *testing.T) {
	l := NewLimiter(DefaultLimitPolicy())
	r := httptest.NewRequest("POST", "/s/demo/cell", nil)
	r.RemoteAddr = "10.0.0.7:51234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := l.clientIP(r); got != "10.0.0.7" {
		t.Fatalf("clientIP = %q, want the peer address 10.0.0.7 — a spoofed header must not be believed", got)
	}
}

func TestClientIPUsesRightmostForwardedWhenTrusted(t *testing.T) {
	p := DefaultLimitPolicy()
	p.TrustForwardedFor = true
	l := NewLimiter(p)
	r := httptest.NewRequest("POST", "/s/demo/cell", nil)
	r.RemoteAddr = "10.0.0.7:51234"
	// The leftmost entry is whatever the client claimed; the rightmost is what
	// the proxy in front actually observed.
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 1.2.3.4")
	if got := l.clientIP(r); got != "1.2.3.4" {
		t.Fatalf("clientIP = %q, want the rightmost entry 1.2.3.4", got)
	}
	r.Header.Del("X-Forwarded-For")
	if got := l.clientIP(r); got != "10.0.0.7" {
		t.Fatalf("clientIP = %q with no header, want the peer address", got)
	}
}

func TestHostOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1.2.3.4:5678", "1.2.3.4"},
		{"[::1]:5678", "::1"},
		{"[fe80::1%eth0]:80", "fe80::1%eth0"},
		{"1.2.3.4", "1.2.3.4"},
		{"", ""},
	} {
		if got := hostOf(tc.in); got != tc.want {
			t.Errorf("hostOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ─── Middleware ───────────────────────────────────────────────────────────────

// testLimiter builds a limiter over a driven clock and a handler that records
// how many requests actually reached it.
func testLimiter(t *testing.T, p LimitPolicy) (*Limiter, http.Handler, *int, *time.Duration) {
	t.Helper()
	l := NewLimiter(p)
	var now time.Duration
	l.now = func() time.Duration { return now }
	reached := 0
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	}))
	return l, h, &reached, &now
}

func post(h http.Handler, path, body, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.RemoteAddr = ip + ":40000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCreateFloodIsRefusedWithRetryAfter(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = "" // disk caps off; this is about the rate
	_, h, reached, _ := testLimiter(t, p)

	for i := range int(p.CreateBurst) {
		if w := post(h, "/sheets", "", "1.1.1.1"); w.Code != http.StatusNoContent {
			t.Fatalf("create %d of the burst got %d", i+1, w.Code)
		}
	}
	w := post(h, "/sheets", "", "1.1.1.1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("the flood got %d, want 429", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q, want a positive whole number of seconds", ra)
	}
	if body := w.Body.String(); !strings.Contains(body, "Too many new sheets") {
		t.Fatalf("refusal body is not a sentence a person can read: %q", body)
	}
	if *reached != int(p.CreateBurst) {
		t.Fatalf("%d requests reached the handler, want %d", *reached, int(p.CreateBurst))
	}
	// Another address is untouched by the first one's flood.
	if w := post(h, "/sheets", "", "2.2.2.2"); w.Code != http.StatusNoContent {
		t.Fatalf("a second address got %d during another's flood", w.Code)
	}
}

// TestFastTypingIsNeverLimited is the requirement that matters most: the demo's
// feel. Ten commits a second for a solid minute — far faster than anyone types —
// must not produce a single refusal.
func TestFastTypingIsNeverLimited(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	_, h, _, now := testLimiter(t, p)

	for i := range 600 { // 60 s at 10 commits/s
		*now = time.Duration(i) * 100 * time.Millisecond
		w := post(h, "/s/demo/cell", `{"ref":"A1","raw":"1","conn":"c"}`, "1.1.1.1")
		if w.Code != http.StatusNoContent {
			t.Fatalf("edit %d at 10/s was refused with %d — a normal user must never be limited", i+1, w.Code)
		}
	}
	// And a held Delete key, ~30/s for ten seconds, is still inside the policy.
	base := 60 * time.Second
	for i := range 300 {
		*now = base + time.Duration(i)*33*time.Millisecond
		if w := post(h, "/s/demo/clear", `{"rng":"A1:B2","conn":"c"}`, "1.1.1.1"); w.Code != http.StatusNoContent {
			t.Fatalf("auto-repeat clear %d at 30/s was refused with %d", i+1, w.Code)
		}
	}
}

func TestWriteFloodIsRefused(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	_, h, _, _ := testLimiter(t, p)
	// The clock never moves, so the burst is all there is.
	for i := range int(p.WriteBurst) {
		if w := post(h, "/s/demo/cell", "{}", "1.1.1.1"); w.Code != http.StatusNoContent {
			t.Fatalf("write %d of the burst got %d", i+1, w.Code)
		}
	}
	w := post(h, "/s/demo/cell", "{}", "1.1.1.1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("the write flood got %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("a rate refusal carried no Retry-After")
	}
}

// TestWriteRefusalReachesTheScreen pins the "never a silent drop" rule: the
// range commands raise a pending chip on the click and need the server to lower
// it, so a refusal has to travel to the screen as well as to the socket.
func TestWriteRefusalNotifiesTheScreen(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.WriteBurst = 1
	l, h, _, _ := testLimiter(t, p)

	var gotConn, gotMsg string
	l.Notify(func(conn, msg string) { gotConn, gotMsg = conn, msg })

	post(h, "/s/demo/fill", `{"conn":"abc123","rng":"A1:A9","op":"d"}`, "1.1.1.1")
	w := post(h, "/s/demo/fill", `{"conn":"abc123","rng":"A1:A9","op":"d"}`, "1.1.1.1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second fill got %d, want 429", w.Code)
	}
	if gotConn != "abc123" {
		t.Fatalf("notified conn %q, want abc123 — a refused range command would hang its chip", gotConn)
	}
	if gotMsg == "" {
		t.Fatal("the screen was notified with an empty message")
	}
}

func TestOversizeBodyIs413(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.MaxBodyBytes = 1024
	_, h, reached, _ := testLimiter(t, p)

	big := `{"raw":"` + strings.Repeat("x", 4096) + `"}`
	w := post(h, "/s/demo/cell", big, "1.1.1.1")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body got %d, want 413", w.Code)
	}
	if *reached != 0 {
		t.Fatal("an oversize body reached the handler")
	}
	if !strings.Contains(w.Body.String(), "too large") {
		t.Fatalf("413 body is not readable: %q", w.Body.String())
	}
	// A body inside the cap is untouched.
	if w := post(h, "/s/demo/cell", `{"raw":"ok"}`, "1.1.1.1"); w.Code != http.StatusNoContent {
		t.Fatalf("a small body got %d", w.Code)
	}
}

// TestOversizeChunkedBodyIsCaught covers the request that declares no length:
// ContentLength is -1, so the ceiling has to be the MaxBytesReader backstop.
func TestOversizeChunkedBodyIsCaught(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.MaxBodyBytes = 64
	l := NewLimiter(p)
	var readErr error
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		for {
			_, err := r.Body.Read(buf)
			if err != nil {
				if err.Error() != "EOF" {
					readErr = err
				}
				return
			}
		}
	}))
	r := httptest.NewRequest("POST", "/s/demo/cell", strings.NewReader(strings.Repeat("x", 4096)))
	r.RemoteAddr = "1.1.1.1:40000"
	r.ContentLength = -1 // chunked: nothing declared
	h.ServeHTTP(httptest.NewRecorder(), r)
	if readErr == nil {
		t.Fatal("a chunked oversize body was read to the end; MaxBytesReader is not wired")
	}
}

// TestStreamSurvivesWriteRefusal is the "do not break SSE" rule. A stream is a
// long-lived read and must not be touched by a write bucket that has run dry.
func TestStreamSurvivesWriteRefusal(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.WriteBurst = 1
	_, h, _, _ := testLimiter(t, p)

	post(h, "/s/demo/cell", "{}", "1.1.1.1")
	if w := post(h, "/s/demo/cell", "{}", "1.1.1.1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("write was not refused (%d); the rest of this test proves nothing", w.Code)
	}
	r := httptest.NewRequest("GET", "/s/demo/live", nil)
	r.RemoteAddr = "1.1.1.1:40001"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("the stream got %d after a write refusal — a viewer must not be disconnected for typing fast", w.Code)
	}
	// And a nav command is on its own bucket too.
	if w := post(h, "/s/demo/viewport", "{}", "1.1.1.1"); w.Code != http.StatusNoContent {
		t.Fatalf("a viewport command got %d after a write refusal", w.Code)
	}
}

func TestConcurrentStreamCap(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.StreamsPerIP = 2
	p.StreamsTotal = 3
	l := NewLimiter(p)

	// A handler that blocks until told, so the streams really are concurrent.
	release := make(chan struct{})
	var open, refused int
	var mu sync.Mutex
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		open++
		mu.Unlock()
		<-release
	}))

	get := func(ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/s/demo/live", nil)
		r.RemoteAddr = ip + ":40000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	var wg sync.WaitGroup
	start := func(ip string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if get(ip).Code == http.StatusTooManyRequests {
				mu.Lock()
				refused++
				mu.Unlock()
			}
		}()
	}
	start("1.1.1.1")
	start("1.1.1.1")
	// Wait for both to be inside the handler before asking for a third.
	for {
		mu.Lock()
		n := open
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if w := get("1.1.1.1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a third concurrent stream from one IP got %d, want 429", w.Code)
	}
	if _, total := l.live.snapshot(); total != 2 {
		t.Fatalf("live total = %d, want 2", total)
	}
	close(release)
	wg.Wait()
	_ = refused
	// Every stream has ended, so the count is back to zero and the IP may open
	// streams again.
	if _, total := l.live.snapshot(); total != 0 {
		t.Fatalf("live total = %d after every stream closed, want 0", total)
	}
}

// TestPageBucketIsSeparate pins the split: a page load is an expensive read and
// gets its own allowance, so a page flood cannot starve an edit and an edit
// flood cannot stop somebody reloading.
func TestPageBucketIsSeparate(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.PageBurst = 2
	p.WriteBurst = 2
	_, h, _, _ := testLimiter(t, p)

	get := func(path string) int {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "1.1.1.1:40000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := range 2 {
		if c := get("/s/demo"); c != http.StatusNoContent {
			t.Fatalf("page %d of the burst got %d", i+1, c)
		}
	}
	if c := get("/s/demo"); c != http.StatusTooManyRequests {
		t.Fatalf("a page flood got %d, want 429", c)
	}
	// The write bucket is untouched by the page flood.
	if w := post(h, "/s/demo/cell", "{}", "1.1.1.1"); w.Code != http.StatusNoContent {
		t.Fatalf("a write got %d after a page flood — the buckets are not separate", w.Code)
	}
	// And the index draws on the same page bucket, which is already dry.
	if c := get("/"); c != http.StatusTooManyRequests {
		t.Fatalf("the index got %d after the page bucket ran dry, want 429", c)
	}
}

// ─── Disk caps ────────────────────────────────────────────────────────────────

func writeFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSheetDirUsage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "aaa.db"), 100)
	writeFile(t, filepath.Join(dir, "aaa.db-wal"), 50)
	writeFile(t, filepath.Join(dir, "bbb.db"), 200)
	writeFile(t, filepath.Join(dir, "notes.txt"), 7)

	count, bytes, err := sheetDirUsage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2 (.db files only)", count)
	}
	if bytes != 357 {
		t.Fatalf("bytes = %d, want 357 (every file, sidecars included)", bytes)
	}

	// A directory that does not exist yet is an empty store, not an error.
	if c, b, err := sheetDirUsage(filepath.Join(dir, "nope")); err != nil || c != 0 || b != 0 {
		t.Fatalf("missing dir gave %d/%d/%v, want 0/0/nil", c, b, err)
	}
}

func TestSheetFileBytes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "demo.db"), 1000)
	writeFile(t, filepath.Join(dir, "demo.db-wal"), 234)
	if got := sheetFileBytes(dir, "demo"); got != 1234 {
		t.Fatalf("sheetFileBytes = %d, want 1234 (db + WAL)", got)
	}
	if got := sheetFileBytes(dir, "missing"); got != 0 {
		t.Fatalf("a sheet that does not exist measured %d, want 0", got)
	}
	// A traversal attempt must measure nothing rather than stat somewhere else.
	if got := sheetFileBytes(dir, "../../etc/passwd"); got != 0 {
		t.Fatalf("a path-traversal id measured %d, want 0", got)
	}
}

func TestGlobalSheetCountCap(t *testing.T) {
	dir := t.TempDir()
	for i := range 3 {
		writeFile(t, filepath.Join(dir, "sheet"+strconv.Itoa(i)+".db"), 10)
	}
	p := DefaultLimitPolicy()
	p.DataDir = dir
	p.MaxSheets = 3
	_, h, reached, _ := testLimiter(t, p)

	w := post(h, "/sheets", "", "1.1.1.1")
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("create at the sheet cap got %d, want 507", w.Code)
	}
	if *reached != 0 {
		t.Fatal("a refused create reached the handler")
	}
	if !strings.Contains(w.Body.String(), "maximum") {
		t.Fatalf("refusal body: %q", w.Body.String())
	}

	p.MaxSheets = 4
	_, h2, reached2, _ := testLimiter(t, p)
	if w := post(h2, "/sheets", "", "1.1.1.1"); w.Code != http.StatusNoContent {
		t.Fatalf("create under the cap got %d", w.Code)
	}
	if *reached2 != 1 {
		t.Fatal("an allowed create did not reach the handler")
	}
}

func TestGlobalDiskByteCap(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "big.db"), 4096)
	p := DefaultLimitPolicy()
	p.DataDir = dir
	p.MaxSheets = 0 // count cap off; this is the byte cap
	p.MaxTotalBytes = 4096
	_, h, _, _ := testLimiter(t, p)
	if w := post(h, "/sheets", "", "1.1.1.1"); w.Code != http.StatusInsufficientStorage {
		t.Fatalf("create at the byte cap got %d, want 507", w.Code)
	}
}

func TestPerSheetSizeCapRefusesWrites(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fat.db"), 2048)
	writeFile(t, filepath.Join(dir, "thin.db"), 10)
	p := DefaultLimitPolicy()
	p.DataDir = dir
	p.MaxSheetBytes = 2048
	l, h, _, _ := testLimiter(t, p)

	var msg string
	l.Notify(func(_, m string) { msg = m })

	w := post(h, "/s/fat/cell", `{"conn":"c1"}`, "1.1.1.1")
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("a write to an over-size sheet got %d, want 507", w.Code)
	}
	if !strings.Contains(msg, "size limit") {
		t.Fatalf("the screen was told %q; a refused write must say why", msg)
	}
	// A different sheet on the same box is unaffected — the cap is per sheet.
	if w := post(h, "/s/thin/cell", `{"conn":"c1"}`, "1.1.1.1"); w.Code != http.StatusNoContent {
		t.Fatalf("a write to a small sheet got %d", w.Code)
	}
	// Reads are never refused: a full sheet becomes read-only, not unreachable.
	r := httptest.NewRequest("GET", "/s/fat/live", nil)
	r.RemoteAddr = "1.1.1.1:40000"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != http.StatusNoContent {
		t.Fatalf("the stream for a full sheet got %d; a full sheet must still be readable", rw.Code)
	}
}

// ─── Index exposure ───────────────────────────────────────────────────────────

func TestIndexModes(t *testing.T) {
	for _, tc := range []struct {
		mode     IndexMode
		wantCode int
		reaches  bool
		wantBody string
	}{
		{IndexList, http.StatusNoContent, true, ""},
		{IndexCreate, http.StatusOK, false, "New sheet"},
		{IndexOff, http.StatusNotFound, false, ""},
	} {
		p := DefaultLimitPolicy()
		p.DataDir = ""
		p.Index = tc.mode
		_, h, reached, _ := testLimiter(t, p)
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "1.1.1.1:40000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.wantCode {
			t.Errorf("index=%s got %d, want %d", tc.mode, w.Code, tc.wantCode)
		}
		if (*reached > 0) != tc.reaches {
			t.Errorf("index=%s reached the real handler = %v, want %v", tc.mode, *reached > 0, tc.reaches)
		}
		if tc.wantBody != "" && !strings.Contains(w.Body.String(), tc.wantBody) {
			t.Errorf("index=%s body does not contain %q", tc.mode, tc.wantBody)
		}
	}
	// The default is the behaviour that already existed. Changing what a
	// deployment exposes must be an explicit act.
	if DefaultLimitPolicy().Index != IndexList {
		t.Fatal("the default index mode is not `list`; that silently changes what an existing deployment exposes")
	}
	// The create-only page must still work with the rate limiting switched off,
	// because it is an exposure decision and not a rate one.
	p := DefaultLimitPolicy()
	p.Enabled = false
	p.Index = IndexOff
	l := NewLimiter(p)
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("-limits=false -index=off served the index anyway (%d)", w.Code)
	}
}

func TestValidIndexMode(t *testing.T) {
	for _, m := range []IndexMode{IndexList, IndexCreate, IndexOff} {
		if !validIndexMode(m) {
			t.Errorf("%q rejected", m)
		}
	}
	for _, m := range []IndexMode{"", "LIST", "none", "yes"} {
		if validIndexMode(m) {
			t.Errorf("%q accepted", m)
		}
	}
}

// ─── The off switch ───────────────────────────────────────────────────────────

func TestLimitsDisabledPassesEverything(t *testing.T) {
	p := DefaultLimitPolicy()
	p.Enabled = false
	l := NewLimiter(p)
	reached := 0
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	}))
	for range 100 {
		if w := post(h, "/sheets", "", "1.1.1.1"); w.Code != http.StatusNoContent {
			t.Fatalf("-limits=false refused a request with %d", w.Code)
		}
	}
	// Including a body far past the cap: with limits off there is no cap.
	if w := post(h, "/s/demo/cell", strings.Repeat("x", 1<<20), "1.1.1.1"); w.Code != http.StatusNoContent {
		t.Fatalf("-limits=false refused a 1 MB body with %d", w.Code)
	}
	if reached != 101 {
		t.Fatalf("%d requests reached the handler, want 101", reached)
	}
}

// TestZeroDisablesOneLimit pins the "0 disables" contract each flag documents.
func TestZeroDisablesOneLimit(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.CreatePerMin = 0 // this one off
	_, h, _, _ := testLimiter(t, p)
	for i := range 50 {
		if w := post(h, "/sheets", "", "1.1.1.1"); w.Code != http.StatusNoContent {
			t.Fatalf("create %d was refused with -limit-create-per-min=0: %d", i+1, w.Code)
		}
	}
	// While the write bucket, untouched, still holds.
	p2 := DefaultLimitPolicy()
	p2.DataDir = ""
	p2.MaxBodyBytes = 0
	_, h2, _, _ := testLimiter(t, p2)
	if w := post(h2, "/s/demo/cell", strings.Repeat("x", 1<<20), "1.1.1.1"); w.Code != http.StatusNoContent {
		t.Fatalf("a 1 MB body was refused with -limit-body-kb=0: %d", w.Code)
	}
}

// ─── Cost ─────────────────────────────────────────────────────────────────────

// TestMiddlewareIsAllocationFree guards the hot path. The middleware runs on
// every request including the push path, so anything it allocates per request
// is a cost the whole system pays.
func TestMiddlewareIsAllocationFree(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = "" // the disk caps are syscalls by nature and are not on this path
	l := NewLimiter(p)
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	r := httptest.NewRequest("POST", "/s/demo/viewport", nil)
	r.RemoteAddr = "1.1.1.1:40000"
	w := httptest.NewRecorder()
	// Warm the bucket key so the one legitimate allocation (the clone on
	// insert) is not counted.
	h.ServeHTTP(w, r)

	got := testing.AllocsPerRun(200, func() {
		h.ServeHTTP(w, r)
	})
	// ZERO. The classification slices the path, hostOf slices RemoteAddr, and
	// the body reader is only wrapped for a body that declared no length — so
	// an ordinary request allocates nothing at all on the way through.
	if got != 0 {
		t.Fatalf("the middleware allocates %.1f objects per request; it is on every push", got)
	}
}

func BenchmarkMiddlewareAllowed(b *testing.B) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	l := NewLimiter(p)
	l.write = newTokenBuckets(1e9, 1e9) // never refuse; measure the cost, not the refusal
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	r := httptest.NewRequest("POST", "/s/demo/cell", nil)
	r.RemoteAddr = "1.1.1.1:40000"
	w := httptest.NewRecorder()
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(w, r)
	}
}

// ─── Refusal shape ────────────────────────────────────────────────────────────

func TestRefuseShape(t *testing.T) {
	w := httptest.NewRecorder()
	refuse(w, http.StatusTooManyRequests, 2500*time.Millisecond, "slow down")
	if w.Code != 429 {
		t.Fatalf("code = %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q, want 3 (rounded whole seconds)", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q; a refusal is never markup", ct)
	}
	if w.Body.String() != "slow down\n" {
		t.Fatalf("body = %q", w.Body.String())
	}
	// No Retry-After on a refusal that waiting will not fix.
	w2 := httptest.NewRecorder()
	refuse(w2, http.StatusInsufficientStorage, 0, "full")
	if w2.Header().Get("Retry-After") != "" {
		t.Fatal("a quota refusal advertised a Retry-After; waiting does not fix it")
	}
}

func TestTellScreenIgnoresJunk(t *testing.T) {
	l := NewLimiter(DefaultLimitPolicy())
	called := false
	l.Notify(func(string, string) { called = true })
	for _, body := range []string{"", "not json", "[]", `{"conn":""}`, `{"other":1}`} {
		called = false
		r := httptest.NewRequest("POST", "/s/demo/cell", strings.NewReader(body))
		l.tellScreen(r, "msg")
		if called {
			t.Errorf("body %q produced a notify call", body)
		}
	}
	r := httptest.NewRequest("POST", "/s/demo/cell", strings.NewReader(`{"conn":"xyz","ref":"A1"}`))
	var conn string
	l.Notify(func(c, _ string) { conn = c })
	l.tellScreen(r, "msg")
	if conn != "xyz" {
		t.Fatalf("conn = %q, want xyz", conn)
	}
}

// TestDescribeStatesThePolicy — the startup line has to be enough to know what
// a running server is enforcing without reading the source.
func TestDescribeStatesThePolicy(t *testing.T) {
	d := NewLimiter(DefaultLimitPolicy()).Describe()
	for _, want := range []string{"create", "writes", "streams", "sheets", "index=list"} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe() does not mention %q: %s", want, d)
		}
	}
	off := NewLimiter(LimitPolicy{}).Describe()
	if !strings.Contains(off, "DISABLED") {
		t.Errorf("a disabled limiter describes itself as %q", off)
	}
}

// TestSignalPayloadsCarryConn is a contract check against the client: every
// command that raises the pending chip sends `conn` at the top level of its
// body, which is what tellScreen reads. If a future payload nests it, the
// refusal path goes quiet.
func TestSignalPayloadsCarryConn(t *testing.T) {
	for _, body := range []string{
		`{"conn":"c","ref":"A1","raw":"5"}`,
		`{"conn":"c","rng":"A1:B2"}`,
		`{"conn":"c","rng":"A1:B2","op":"d"}`,
		`{"conn":"c","src":"A1:B2","dst":"C3"}`,
		`{"conn":"c","rng":"A1:B2","set":{"b":"1"}}`,
		`{"conn":"c","mop":"ia","mi":3}`,
	} {
		var sig struct {
			Conn string `json:"conn"`
		}
		if err := json.Unmarshal([]byte(body), &sig); err != nil || sig.Conn != "c" {
			t.Errorf("%s did not yield conn=c (%v)", body, err)
		}
	}
}

// ─── Per-IP sheet quota ───────────────────────────────────────────────────────
//
// THE GLOBAL 500-SHEET CAP WITHOUT THIS IS A DENIAL OF SERVICE BY ACCUMULATION:
// one address creating at the allowed 6/min fills every slot in ~83 minutes and
// creation then fails for everybody. These tests are that story.

// creatingLimiter is testLimiter with a handler that behaves like index.go's:
// it answers 303 with `Location: /s/{id}`, which is how the middleware learns
// which sheet the address just made.
func creatingLimiter(t *testing.T, p LimitPolicy) (*Limiter, http.Handler, *[]string) {
	t.Helper()
	l := NewLimiter(p)
	var now time.Duration
	l.now = func() time.Duration { return now }
	made := []string{}
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := "sheet" + strconv.Itoa(len(made))
		made = append(made, id)
		http.Redirect(w, r, "/s/"+id, http.StatusSeeOther)
	}))
	return l, h, &made
}

func TestPerIPSheetQuotaRefusesTheOneOverTheLine(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.CreatePerMin, p.CreateBurst = 1e9, 1e9 // the rate is not what is under test
	p.MaxSheetsPerIP = 5
	l, h, made := creatingLimiter(t, p)

	for i := range p.MaxSheetsPerIP {
		if w := post(h, "/sheets", "", "1.1.1.1"); w.Code != http.StatusSeeOther {
			t.Fatalf("create %d got %d, want 303", i+1, w.Code)
		}
	}
	if len(*made) != p.MaxSheetsPerIP {
		t.Fatalf("%d sheets reached the handler, want %d", len(*made), p.MaxSheetsPerIP)
	}

	w := post(h, "/sheets", "", "1.1.1.1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("the sheet over the quota got %d, want 429", w.Code)
	}
	if len(*made) != p.MaxSheetsPerIP {
		t.Fatal("the refused create still reached the handler; a sheet was made anyway")
	}
	body := w.Body.String()
	// A SENTENCE, naming what happened and that the slot comes back.
	for _, want := range []string{"already have 5 sheets", "limit for one address", "cleared", "14 days"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal %q is missing %q", body, want)
		}
	}
	if l.Stats().Quota != 1 {
		t.Errorf("over-quota refusals = %d, want 1", l.Stats().Quota)
	}

	// ATTRIBUTION: another address is untouched by the first one's hoard.
	if w := post(h, "/sheets", "", "2.2.2.2"); w.Code != http.StatusSeeOther {
		t.Fatalf("a second address got %d while the first was over quota", w.Code)
	}
	if got := l.Quota().held("1.1.1.1"); got != 5 {
		t.Errorf("ledger for the hoarder = %d, want 5", got)
	}
	if got := l.Quota().held("2.2.2.2"); got != 1 {
		t.Errorf("ledger for the second address = %d, want 1", got)
	}
}

// TestReapingTheQuotaLetsCreationResume is the whole reason the cap counts LIVE
// sheets rather than lifetime creations.
func TestReapingTheQuotaLetsCreationResume(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.CreatePerMin, p.CreateBurst = 1e9, 1e9
	p.MaxSheetsPerIP = 3
	l, h, made := creatingLimiter(t, p)

	for range 3 {
		if w := post(h, "/sheets", "", "1.1.1.1"); w.Code != http.StatusSeeOther {
			t.Fatal("a create inside the quota was refused")
		}
	}
	if w := post(h, "/sheets", "", "1.1.1.1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("the fourth create got %d, want 429", w.Code)
	}

	// The reaper clears one of them.
	l.Quota().release((*made)[0])

	if w := post(h, "/sheets", "", "1.1.1.1"); w.Code != http.StatusSeeOther {
		t.Fatalf("creation did not resume after a reap: %d", w.Code)
	}
	if w := post(h, "/sheets", "", "1.1.1.1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("the quota did not close again after one slot was reused: %d", w.Code)
	}
	if got := l.Quota().held("1.1.1.1"); got != 3 {
		t.Errorf("ledger = %d, want 3", got)
	}
}

func TestQuotaLedgerArithmetic(t *testing.T) {
	q := newSheetQuota()
	if got := q.held("a"); got != 0 {
		t.Fatalf("an unseen address holds %d", got)
	}
	q.add("a", "one")
	q.add("a", "two")
	q.add("b", "three")
	// Idempotent: a retried create must not double-charge.
	q.add("a", "one")
	if got := q.held("a"); got != 2 {
		t.Errorf("held(a) = %d, want 2", got)
	}
	if addrs, sheets := q.snapshot(); addrs != 2 || sheets != 3 {
		t.Errorf("snapshot = (%d, %d), want (2, 3)", addrs, sheets)
	}
	// A release of a sheet nobody owns is a no-op, not a corruption: the
	// reaper calls it for every sheet it clears, including ones created before
	// this process started.
	q.release("never-seen")
	if got := q.held("a"); got != 2 {
		t.Errorf("held(a) after an unknown release = %d, want 2", got)
	}
	q.release("one")
	q.release("two")
	if got := q.held("a"); got != 0 {
		t.Errorf("held(a) after releasing both = %d, want 0", got)
	}
	if addrs, _ := q.snapshot(); addrs != 1 {
		t.Errorf("an address with nothing left is still in the ledger (%d addresses)", addrs)
	}
	// A nil ledger answers rather than panics — that is what a disabled quota is.
	var nilq *sheetQuota
	nilq.add("a", "b")
	nilq.release("b")
	if got := nilq.held("a"); got != 0 {
		t.Errorf("nil ledger held = %d", got)
	}
}

func TestZeroDisablesTheSheetQuota(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.CreatePerMin, p.CreateBurst = 1e9, 1e9
	p.MaxSheetsPerIP = 0
	_, h, made := creatingLimiter(t, p)

	for i := range 40 {
		if w := post(h, "/sheets", "", "1.1.1.1"); w.Code != http.StatusSeeOther {
			t.Fatalf("create %d got %d with the quota disabled", i+1, w.Code)
		}
	}
	if len(*made) != 40 {
		t.Fatalf("%d sheets reached the handler, want 40", len(*made))
	}
}

// TestQuotaFollowsTheTrustedForwardedAddress — in production every request
// arrives from the proxy over loopback, so a quota keyed on RemoteAddr would be
// one shared bucket for the entire internet.
func TestQuotaFollowsTheTrustedForwardedAddress(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.CreatePerMin, p.CreateBurst = 1e9, 1e9
	p.MaxSheetsPerIP = 2
	p.TrustForwardedFor = true
	l, h, _ := creatingLimiter(t, p)

	fwd := func(xff string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/sheets", nil)
		r.RemoteAddr = "127.0.0.1:40000" // the proxy, for every request
		r.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for range 2 {
		if w := fwd("9.9.9.9"); w.Code != http.StatusSeeOther {
			t.Fatal("a create inside the quota was refused")
		}
	}
	if w := fwd("9.9.9.9"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("the third from one forwarded address got %d, want 429", w.Code)
	}
	// A DIFFERENT visitor behind the same proxy is unaffected.
	if w := fwd("8.8.8.8"); w.Code != http.StatusSeeOther {
		t.Fatalf("a second visitor behind the same proxy got %d", w.Code)
	}
	// And the spoofable leftmost entry is not what is counted.
	if w := fwd("1.2.3.4, 9.9.9.9"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("prepending a fake entry escaped the quota: %d", w.Code)
	}
	if got := l.Quota().held("9.9.9.9"); got != 2 {
		t.Errorf("ledger for the forwarded address = %d, want 2", got)
	}
	if got := l.Quota().held("127.0.0.1"); got != 0 {
		t.Errorf("the proxy itself was charged %d sheets", got)
	}
}

// TestQuotaOnlyChargesWhatWasActuallyCreated — a create the handler refused
// writes no Location, so nothing is charged for it.
func TestQuotaOnlyChargesWhatWasActuallyCreated(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.MaxSheetsPerIP = 5
	l := NewLimiter(p)
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	if w := post(h, "/sheets", "", "1.1.1.1"); w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d", w.Code)
	}
	if got := l.Quota().held("1.1.1.1"); got != 0 {
		t.Errorf("a failed create charged %d sheets", got)
	}
}

func TestRetentionIsStatedOnTheCreateOnlyIndex(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.Index = IndexCreate
	_, h, _, _ := testLimiter(t, p)

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "1.1.1.1:40000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	// The policy is STATED BEFORE IT BITES: the trigger, the period, and the
	// part a reader would not assume — that the link survives.
	for _, want := range []string{"14 days", "without an edit", "link keeps working", "empty sheet"} {
		if !strings.Contains(body, want) {
			t.Errorf("the index page does not say %q:\n%s", want, body)
		}
	}
	// With retention off, it promises nothing.
	p.SheetTTL = 0
	_, h2, _, _ := testLimiter(t, p)
	w2 := httptest.NewRecorder()
	h2.ServeHTTP(w2, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(w2.Body.String(), "cleared") {
		t.Error("the index promises a retention policy that is not running")
	}
}

// ─── The hot path is unchanged ────────────────────────────────────────────────

// TestHotPathIsStillAllocationFree is the regression guard for everything
// above. The quota is a map, the reaper is a goroutine, and neither may appear
// on a request that is not a create — the middleware runs on every push.
func TestHotPathIsStillAllocationFree(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = "" // the disk caps are syscalls by nature and are not on this path
	l := NewLimiter(p)
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for _, tc := range []struct{ method, path string }{
		{"POST", "/s/demo/viewport"}, // nav — the scroll path
		{"POST", "/s/demo/cell"},     // write — the keystroke path
		{"POST", "/s/demo/sel"},
		{"GET", "/s/demo"}, // page
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.RemoteAddr = "1.1.1.1:40000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r) // warm the bucket key so the insert clone is not counted

		got := testing.AllocsPerRun(200, func() { h.ServeHTTP(w, r) })
		if got != 0 {
			t.Errorf("%s %s allocates %.1f objects per request", tc.method, tc.path, got)
		}
	}
}

// ─── The two stream caps say different things ─────────────────────────────────

// TestStreamRefusalsNameTheRightCap. "Close a tab and reload" is correct advice
// when this address is holding too many streams and actively misleading when
// the process is full and the reader has one tab open — which, with the process
// cap now at 64, is the case a broadly-shared demo will actually produce.
func TestStreamRefusalsNameTheRightCap(t *testing.T) {
	p := DefaultLimitPolicy()
	p.DataDir = ""
	p.StreamsPerIP = 2
	p.StreamsTotal = 3
	l := NewLimiter(p)

	release := make(chan struct{})
	var open int
	var mu sync.Mutex
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		open++
		mu.Unlock()
		<-release
	}))
	get := func(ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/s/demo/live", nil)
		r.RemoteAddr = ip + ":40000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	hold := func(ip string) {
		go func() { get(ip) }()
	}
	waitOpen := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := open
			mu.Unlock()
			if got == n {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("only %d streams opened, want %d", open, n)
	}
	defer close(release)

	// Two from one address fills that address's cap but not the process's.
	hold("1.1.1.1")
	hold("1.1.1.1")
	waitOpen(2)
	w := get("1.1.1.1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("the third from one address got %d", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "from this address") ||
		!strings.Contains(body, "Close a tab") {
		t.Errorf("per-IP refusal reads %q; it must say it is about this address", body)
	}

	// A third address fills the process, and now the sentence changes.
	hold("2.2.2.2")
	waitOpen(3)
	w = get("3.3.3.3")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("a stream past the process cap got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "Close a tab") {
		t.Errorf("a reader with one tab open was told to close a tab: %q", body)
	}
	for _, want := range []string{"demo is full", "3 people", "Try again"} {
		if !strings.Contains(body, want) {
			t.Errorf("process-cap refusal %q is missing %q", body, want)
		}
	}
}

// TestStreamCapDefaultIsTheMeasuredOne guards the number itself: it was lowered
// from 256 on the strength of a measurement, and raising it back silently would
// undo that.
func TestStreamCapDefaultIsTheMeasuredOne(t *testing.T) {
	p := DefaultLimitPolicy()
	if p.StreamsTotal != 64 {
		t.Errorf("StreamsTotal = %d, want 64 — see the fan-out table in limits.go", p.StreamsTotal)
	}
	if p.StreamsPerIP > p.StreamsTotal {
		t.Errorf("per-IP cap %d exceeds the process cap %d", p.StreamsPerIP, p.StreamsTotal)
	}
}
