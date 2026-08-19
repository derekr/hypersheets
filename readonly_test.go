package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withReadOnly installs a set for the duration of one test and puts the old one
// back. The variable is process configuration, so a test that changed it and
// did not restore it would silently make read-only sheets of everybody else's
// fixtures.
func withReadOnly(t *testing.T, ids ...string) {
	t.Helper()
	prev := readOnlySheets
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	readOnlySheets = m
	t.Cleanup(func() { readOnlySheets = prev })
}

func TestNoSheetIsReadOnlyByDefault(t *testing.T) {
	withReadOnly(t)
	if isReadOnly("anything") {
		t.Fatal("the empty configuration must protect nothing")
	}
	prev := readOnlySheets
	readOnlySheets = nil
	if isReadOnly("anything") {
		t.Fatal("a NIL set must answer false rather than panic — that is the zero value main.go would leave if the flag were never parsed")
	}
	readOnlySheets = prev
}

// okHandler stands in for the application. Reaching it means the middleware
// ALLOWED the request, which is the thing every case below is really asserting.
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusNoContent)
	})
}

func TestReadOnlySheetRefusesEveryWriteVerb(t *testing.T) {
	withReadOnly(t, "locked")
	l := NewLimiter(DefaultLimitPolicy())

	// Every write route the mux carries. A verb added later that is classified
	// classWrite is covered by the same check without touching this list — but
	// one that is NOT classified classWrite would slip through, which is why
	// this test names them explicitly rather than trusting the classifier.
	for _, path := range []string{
		"/s/locked/cell", "/s/locked/clear", "/s/locked/fill", "/s/locked/paste",
		"/s/locked/style", "/s/locked/colwidth", "/s/locked/rows", "/s/locked/cols",
	} {
		reached := false
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, strings.NewReader("{}"))
		r.RemoteAddr = "10.0.0.1:1234"
		l.Middleware(okHandler(&reached)).ServeHTTP(rec, r)

		if reached {
			t.Errorf("%s reached the handler; a read-only sheet must be refused in the middleware", path)
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403 — the sheet is fixed, which is not a rate limit and must not be a 429", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "read-only") {
			t.Errorf("%s body = %q, want the sentence that says why", path, rec.Body.String())
		}
	}
}

func TestReadOnlyRefusalCarriesNoRetryAfter(t *testing.T) {
	withReadOnly(t, "locked")
	l := NewLimiter(DefaultLimitPolicy())
	reached := false
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/s/locked/cell", strings.NewReader("{}"))
	r.RemoteAddr = "10.0.0.1:1234"
	l.Middleware(okHandler(&reached)).ServeHTTP(rec, r)

	// Retry-After would promise that waiting helps. It never will.
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want none: this refusal is permanent, and telling a client to retry would make every well-behaved one busy-loop", got)
	}
}

func TestReadOnlySheetStillReads(t *testing.T) {
	withReadOnly(t, "locked")
	l := NewLimiter(DefaultLimitPolicy())

	// A sheet nobody can open, scroll or select in is not read-only, it is
	// broken. The page, the viewport command and the selection are all reads.
	for _, tc := range []struct{ method, path string }{
		{"GET", "/s/locked"},
		{"POST", "/s/locked/viewport"},
		{"POST", "/s/locked/sel"},
	} {
		reached := false
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		r.RemoteAddr = "10.0.0.2:1234"
		l.Middleware(okHandler(&reached)).ServeHTTP(rec, r)
		if !reached {
			t.Errorf("%s %s was refused with %d; reads must still work on a read-only sheet", tc.method, tc.path, rec.Code)
		}
	}
}

func TestWritesToOtherSheetsAreUnaffected(t *testing.T) {
	withReadOnly(t, "locked")
	l := NewLimiter(DefaultLimitPolicy())
	reached := false
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/s/notlocked/cell", strings.NewReader("{}"))
	r.RemoteAddr = "10.0.0.3:1234"
	l.Middleware(okHandler(&reached)).ServeHTTP(rec, r)
	if !reached {
		t.Fatalf("a write to an unlisted sheet was refused with %d; the feature must be per-sheet, not global", rec.Code)
	}
}

func TestReadOnlyRefusalIsCounted(t *testing.T) {
	withReadOnly(t, "locked")
	l := NewLimiter(DefaultLimitPolicy())
	reached := false
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/s/locked/cell", strings.NewReader("{}"))
	r.RemoteAddr = "10.0.0.4:1234"
	l.Middleware(okHandler(&reached)).ServeHTTP(rec, r)

	st := l.Stats()
	if st.ReadOnly != 1 {
		t.Errorf("Stats().ReadOnly = %d, want 1", st.ReadOnly)
	}
	// It must NOT be counted as a rate-limit refusal: the two have different
	// causes and an operator reading "writes refused: 400" should not be
	// hunting a limiter that is behaving perfectly.
	if st.Write != 0 {
		t.Errorf("Stats().Write = %d, want 0 — a read-only refusal is not a rate-limit refusal", st.Write)
	}
}

func TestReadOnlyIsCheckedBeforeTheRateLimit(t *testing.T) {
	withReadOnly(t, "locked")
	p := DefaultLimitPolicy()
	p.WritePerSec = 0
	p.WriteBurst = 1 // exactly one token, ever
	l := NewLimiter(p)

	for i := 0; i < 5; i++ {
		reached := false
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/s/locked/cell", strings.NewReader("{}"))
		r.RemoteAddr = "10.0.0.5:1234"
		l.Middleware(okHandler(&reached)).ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("write %d = %d, want 403 every time", i, rec.Code)
		}
	}
	// The bucket must be untouched: a refused write should not have spent a
	// token, or one reader poking a fixed sheet would exhaust the limit that
	// protects the sheets people are actually editing.
	reached := false
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/s/open/cell", strings.NewReader("{}"))
	r.RemoteAddr = "10.0.0.5:1234"
	l.Middleware(okHandler(&reached)).ServeHTTP(rec, r)
	if !reached {
		t.Fatalf("the one write token was spent by refusals; got %d", rec.Code)
	}
}

// ─── The chrome ───────────────────────────────────────────────────────────────

func TestBadgeAppearsOnlyOnAReadOnlySheet(t *testing.T) {
	withReadOnly(t, "locked")
	if got := readOnlyBadgeHTML("locked"); !strings.Contains(got, "read-only") {
		t.Errorf("badge for a locked sheet = %q, want the words", got)
	}
	if got := readOnlyBadgeHTML("open"); got != "" {
		t.Errorf("badge for an ordinary sheet = %q, want nothing at all", got)
	}
}

func TestRetentionChipIsSilentOnAReadOnlySheet(t *testing.T) {
	withReadOnly(t, "locked")
	prev := sheetTTLFlag
	sheetTTLFlag = 14 * 24 * time.Hour
	t.Cleanup(func() { sheetTTLFlag = prev })

	if got := retentionChipHTML("open"); !strings.Contains(got, "14 days") {
		t.Errorf("retention chip on an ordinary sheet = %q, want the period", got)
	}
	// main.go puts every read-only id in the reaper's Keep set, so this sheet
	// is never cleared. Promising it will be would be a false statement in the
	// chrome of the one sheet that disproves it.
	if got := retentionChipHTML("locked"); got != "" {
		t.Errorf("retention chip on a read-only sheet = %q, want nothing: it is never reaped", got)
	}
	sheetTTLFlag = 0
	if got := retentionChipHTML("open"); got != "" {
		t.Errorf("retention chip with retention disabled = %q, want nothing", got)
	}
}
