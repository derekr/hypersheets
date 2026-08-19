package main

import "net/http"

// securityHeaders sets response headers that limit the damage a mistake
// elsewhere could do. They are defence in depth: the page escapes every piece of
// user text it renders and validates colours against an allow-list before they
// reach the stylesheet, so nothing here is load bearing on its own.
//
// Referrer-Policy is the one that is specific to this application rather than
// generic hardening. A sheet's URL is its capability — anyone holding the link
// may read and write it — so a referrer header is a capability leak, not a
// privacy nuisance. no-referrer means the address never travels anywhere the
// reader did not intend to send it.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}

// contentSecurityPolicy is as strict as Datastar allows.
//
// 'unsafe-eval' is required and cannot be removed: Datastar compiles the
// expressions in data-* attributes into functions at runtime, which is the
// mechanism the whole page is built on. 'unsafe-inline' for styles is required
// for the same kind of reason — the stylesheet is inlined in the document
// (deliberately: it is render-blocking and small enough that a second request
// costs more than it saves) and data-style writes inline styles.
//
// What remains is still worth having. Scripts may only come from this origin, so
// the vendored Datastar and the page's own bundle are the only code that can
// run; no third party can be injected. frame-ancestors and base-uri close
// clickjacking and base-tag hijacking, form-action closes form-based
// exfiltration, and connect-src keeps the SSE stream and every command on this
// origin.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-eval'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"
