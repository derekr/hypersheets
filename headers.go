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

// contentSecurityPolicy is as strict as Datastar allows, which is less strict
// than it first appears.
//
// script-src needs all three of 'self', 'unsafe-inline' and 'unsafe-eval', and
// the reason for each is a mechanism the page is built on rather than a
// convenience:
//
//   - 'unsafe-eval'  Datastar compiles the expressions in data-* attributes into
//     functions at runtime.
//   - 'unsafe-inline' the runtime creates script elements with textContent — for
//     re-running scripts inside patched elements, and for
//     text/javascript responses. A script built that way is an
//     inline script as far as CSP is concerned, whatever the page
//     source contains. Omitting this served a page that worked in
//     Chrome and broke in Firefox, which is the worst kind of
//     wrong.
//
// style-src needs 'unsafe-inline' because the stylesheet is inlined in the
// document (deliberately: it is render-blocking and small enough that a second
// request costs more than it saves) and data-style writes inline styles.
//
// So this policy does not stop injected script from executing, and pretending
// otherwise would be worse than not having it. What it does do is worth keeping:
// no script may be LOADED from another origin, connect-src keeps the SSE stream
// and every command on this origin, object-src closes plugin embedding,
// base-uri closes base-tag hijacking, form-action keeps form posts on this
// origin (it must be 'self', not 'none' — creating a sheet is a real form post),
// and frame-ancestors closes clickjacking. The application's
// actual defence against injection is that it escapes every piece of user text
// it renders and validates colours against an allow-list before they reach the
// stylesheet.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' 'unsafe-eval'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"
