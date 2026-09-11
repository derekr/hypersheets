package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// adminEnv builds a limiter whose index is create-only, so the listing is
// reachable only through the admin path. The "listing" is a sentinel handler:
// reaching it means serveIndex delegated, which is the thing under test.
func adminEnv(t *testing.T, email string, trustProxy bool) http.Handler {
	t.Helper()
	p := DefaultLimitPolicy()
	p.Index = IndexCreate
	p.AdminEmail = email
	p.TrustForwardedFor = trustProxy
	l := NewLimiter(p)
	return l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("THE-LISTING"))
	}))
}

func getIndex(h http.Handler, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.9:1234"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func sawListing(rec *httptest.ResponseRecorder) bool {
	return strings.Contains(rec.Body.String(), "THE-LISTING")
}

func TestAnonymousVisitorNeverSeesTheListing(t *testing.T) {
	h := adminEnv(t, "owner@example.com", true)
	rec := getIndex(h, nil)
	if sawListing(rec) {
		t.Fatal("an unauthenticated visitor reached the sheet listing")
	}
	// And it must look like an ordinary visit, not like a locked door: nothing
	// in the response should hint that a listing exists to be found.
	body := strings.ToLower(rec.Body.String())
	for _, leak := range []string{"admin", "forbidden", "unauthor", "login", "listing"} {
		if strings.Contains(body, leak) {
			t.Errorf("the public page mentions %q — it should be indistinguishable from a deployment with no admin", leak)
		}
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: a 401/403 would advertise that something is there", rec.Code)
	}
}

func TestTheAdminSeesTheListing(t *testing.T) {
	h := adminEnv(t, "owner@example.com", true)
	rec := getIndex(h, map[string]string{adminEmailHeader: "owner@example.com"})
	if !sawListing(rec) {
		t.Fatalf("the configured admin did not reach the listing (status %d)", rec.Code)
	}
}

// TestAForgedHeaderIsUselessWithoutTheProxyDeclaration is the interlock. The
// header is only trustworthy because a proxy strips client-supplied copies;
// with no proxy declared, it is attacker-controlled and must be ignored.
func TestAForgedHeaderIsUselessWithoutTheProxyDeclaration(t *testing.T) {
	h := adminEnv(t, "owner@example.com", false) // no trusted proxy declared
	rec := getIndex(h, map[string]string{adminEmailHeader: "owner@example.com"})
	if sawListing(rec) {
		t.Fatal("the identity header was believed without a trusted proxy — any visitor could set it")
	}
}

func TestAnotherAuthenticatedUserIsNotTheAdmin(t *testing.T) {
	h := adminEnv(t, "owner@example.com", true)
	for _, other := range []string{
		"someone@example.com",
		"owner@example.com.evil.test", // suffix games
		"xowner@example.com",          // prefix games
		"",
	} {
		rec := getIndex(h, map[string]string{adminEmailHeader: other})
		if sawListing(rec) {
			t.Errorf("%q reached the listing", other)
		}
	}
}

// Addresses are case-insensitive; a proxy that normalises differently than the
// operator typed the flag must not lock the operator out.
func TestAdminMatchIsCaseInsensitiveAndTrimmed(t *testing.T) {
	h := adminEnv(t, "Owner@Example.COM", true)
	for _, v := range []string{"owner@example.com", "OWNER@EXAMPLE.COM", "  owner@example.com  "} {
		if rec := getIndex(h, map[string]string{adminEmailHeader: v}); !sawListing(rec) {
			t.Errorf("%q did not match the configured admin", v)
		}
	}
}

// With no admin configured the header is inert, so a clone of this repo cannot
// be talked into exposing a listing by any request at all.
func TestWithNoAdminConfiguredTheHeaderIsInert(t *testing.T) {
	h := adminEnv(t, "", true)
	for _, v := range []string{"anyone@example.com", "owner@example.com"} {
		if rec := getIndex(h, map[string]string{adminEmailHeader: v}); sawListing(rec) {
			t.Errorf("%q reached the listing with no admin configured", v)
		}
	}
}

// The query-string escape hatch this replaced was a master key that landed in
// browser history and access logs. Nothing should bring it back.
func TestNoQueryParameterRevealsTheListing(t *testing.T) {
	h := adminEnv(t, "owner@example.com", true)
	for _, q := range []string{
		"/?secret=1", "/?admin=1", "/?reveal=1", "/?index=1", "/?all=1", "/?list=1", "/?debug=1",
	} {
		r := httptest.NewRequest("GET", q, nil)
		r.RemoteAddr = "10.0.0.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if sawListing(rec) {
			t.Errorf("%s revealed the listing", q)
		}
	}
}

func TestLoginBounceOnlyExistsWhenAnAdminIsConfigured(t *testing.T) {
	withAdmin := getIndex(adminEnv(t, "owner@example.com", true), nil)
	_ = withAdmin
	r := httptest.NewRequest("GET", "/?login=1", nil)
	r.RemoteAddr = "10.0.0.9:1234"
	rec := httptest.NewRecorder()
	adminEnv(t, "owner@example.com", true).ServeHTTP(rec, r)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("?login=1 with an admin configured = %d, want 303", rec.Code)
	}

	rec = httptest.NewRecorder()
	adminEnv(t, "", true).ServeHTTP(rec, r)
	if rec.Code == http.StatusSeeOther {
		t.Error("?login=1 redirects on a deployment with no admin; it should be an ordinary page")
	}
}

// The query-string reveal mechanism this replaced is gone entirely: there is no
// parameter to guess, so no value to leak. Nothing should reintroduce it.
func TestTheRevealParameterMechanismIsGone(t *testing.T) {
	for _, f := range []string{"limits.go", "main.go"} {
		b := mustRead(t, f)
		if strings.Contains(b, "IndexReveal") {
			t.Errorf("%s still references IndexReveal", f)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// The "All sheets" link in the page shell must follow the listing: it is
// shown exactly when following it would show one. An unauthed visitor under
// create/off gets a create page or a 404, so the link would be a lie — and a
// hint that a listing exists to be found.
func TestAllSheetsLinkFollowsTheListing(t *testing.T) {
	mk := func(idx IndexMode) *Server {
		p := DefaultLimitPolicy()
		p.Index = idx
		p.AdminEmail = "owner@example.com"
		p.TrustForwardedFor = true
		lim := NewLimiter(p)
		srv := NewServer(ServerOptions{})
		srv.SetIndexPolicy(idx, lim.isAdmin)
		return srv
	}
	anon := httptest.NewRequest("GET", "/s/demo", nil)
	admin := httptest.NewRequest("GET", "/s/demo", nil)
	admin.Header.Set(adminEmailHeader, "owner@example.com")
	forged := httptest.NewRequest("GET", "/s/demo", nil)
	forged.Header.Set(adminEmailHeader, "someone@example.com")
	for _, c := range []struct {
		name  string
		index IndexMode
		r     *http.Request
		want  bool
	}{
		{"list shows everyone the link", IndexList, anon, true},
		{"create hides it from visitors", IndexCreate, anon, false},
		{"off hides it from visitors", IndexOff, anon, false},
		{"create shows it to the admin", IndexCreate, admin, true},
		{"off shows it to the admin", IndexOff, admin, true},
		{"a forged header buys nothing", IndexCreate, forged, false},
	} {
		if got := mk(c.index).showAllSheets(c.r); got != c.want {
			t.Errorf("%s: showAllSheets = %v, want %v", c.name, got, c.want)
		}
	}
	// And the shell obeys the flag: present when true, with no trace of it
	// when false — not a hidden element, not a comment, nothing to find.
	if got := pageShellWidths("demo", 0, 3, "", zeroAnchor(), nil, DefaultRows, "", nil, true); !strings.Contains(got, `href="/"`) {
		t.Error("showAllSheets=true left the link out of the shell")
	}
	if got := pageShellWidths("demo", 0, 3, "", zeroAnchor(), nil, DefaultRows, "", nil, false); strings.Contains(got, "All sheets") {
		t.Error("showAllSheets=false left a trace of the link in the shell")
	}
}
