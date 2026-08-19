package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func csp(t *testing.T) map[string]string {
	t.Helper()
	rec := httptest.NewRecorder()
	securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	out := map[string]string{}
	for _, d := range strings.Split(rec.Header().Get("Content-Security-Policy"), ";") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		name, val, _ := strings.Cut(d, " ")
		out[name] = val
	}
	if len(out) == 0 {
		t.Fatal("no Content-Security-Policy was set")
	}
	return out
}

// TestCSPAllowsWhatThePagesActuallyDo is the test that should have existed before
// the policy shipped.
//
// A CSP is asserted about the page rather than derived from it, so it fails in
// exactly one direction: the page stops working, in a browser, for someone else.
// Two directives were wrong on the first deploy — script-src omitted
// 'unsafe-inline', which broke Firefox and not Chrome, and form-action was 'none'
// while three separate pages post a form to create a sheet, which broke the
// primary action of the demo everywhere.
//
// So each directive below is tied to the mechanism that needs it, and the ones
// with a page-visible consequence are checked against rendered output.
func TestCSPAllowsWhatThePagesActuallyDo(t *testing.T) {
	d := csp(t)

	// Creating a sheet is a real <form method="post" action="/sheets">, emitted
	// by the sheet shell and by both index pages. 'none' blocks all three.
	forms := regexp.MustCompile(`<form[^>]*action="([^"]*)"`)
	pages := map[string]string{
		"sheet shell": pageShell("demo", 0, 249, "", zeroAnchor()),
		"index":       indexCreateOnlyHTML(0),
	}
	for name, page := range pages {
		for _, m := range forms.FindAllStringSubmatch(page, -1) {
			if !strings.HasPrefix(m[1], "/") {
				continue // a cross-origin action would need more than 'self'
			}
			if got := d["form-action"]; got != "'self'" {
				t.Errorf("%s posts a form to %s but form-action is %q — the form is blocked",
					name, m[1], got)
			}
		}
	}

	// Datastar compiles data-* expressions into functions, and creates script
	// elements with textContent to re-run scripts inside patched elements. Both
	// are inline as far as CSP is concerned.
	for _, need := range []string{"'unsafe-eval'", "'unsafe-inline'", "'self'"} {
		if !strings.Contains(d["script-src"], need) {
			t.Errorf("script-src is %q, missing %s — the runtime needs it", d["script-src"], need)
		}
	}

	// The stylesheet is inlined in the document and data-style writes inline
	// styles.
	if !strings.Contains(d["style-src"], "'unsafe-inline'") {
		t.Errorf("style-src is %q; the inlined stylesheet would be blocked", d["style-src"])
	}

	// The favicon is a data: URI so no page ever requests /favicon.ico.
	if !strings.Contains(d["img-src"], "data:") {
		t.Errorf("img-src is %q; the inline favicon would be blocked", d["img-src"])
	}

	// The SSE stream and every command are same-origin.
	if !strings.Contains(d["connect-src"], "'self'") {
		t.Errorf("connect-src is %q; the live stream would be blocked", d["connect-src"])
	}

	// The directives that cost nothing and should stay shut.
	for name, want := range map[string]string{
		"object-src":      "'none'",
		"base-uri":        "'none'",
		"frame-ancestors": "'none'",
	} {
		if d[name] != want {
			t.Errorf("%s is %q, want %s", name, d[name], want)
		}
	}
}

func TestNoPageAsksForAFaviconFile(t *testing.T) {
	for name, page := range map[string]string{
		"sheet shell": pageShell("demo", 0, 249, "", zeroAnchor()),
		"index":       indexCreateOnlyHTML(0),
	} {
		if !strings.Contains(page, `rel="icon"`) {
			t.Errorf("%s declares no icon, so the browser requests /favicon.ico and gets a 404", name)
		}
		if strings.Contains(page, `href="/favicon`) {
			t.Errorf("%s points at a favicon file that is not served", name)
		}
	}
}

// The URL is the capability, so a referrer header would leak it.
func TestReferrerPolicyIsNoReferrer(t *testing.T) {
	rec := httptest.NewRecorder()
	securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer: a sheet URL is its capability", got)
	}
}
