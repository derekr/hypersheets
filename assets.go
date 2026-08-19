package main

// assets.go — the page's JavaScript, served once and cached forever.
//
// The document is `Cache-Control: no-store` — it must be, a sheet's contents
// change — so anything inline in it is re-sent on every navigation, and this
// demo is navigated constantly (index -> sheet -> back -> another sheet). The JS
// is about 44% of the compressed page, so it moves out.
//
// The CSS does not, and the asymmetry is the point. The script is deferred, so
// fetching it from a second URL costs nothing before first paint; a stylesheet
// is render-blocking, so moving it out would trade a guaranteed extra round trip
// for bytes that take about a millisecond to send, and most visitors arrive once
// from a shared link with a cold cache. The rule is "externalise what does not
// block the first paint", not "externalise what is big".
//
// The precondition is that the bundle be byte-identical for every sheet and
// every request, so no per-request number may be baked into the source. The
// three that would be are read from the document instead:
//
//	T.rows      the sheet's row extent      -> reads `--rows` off `#vp`
//	T.anchorRow the `?at=` landing row      -> reads `data-ar` off `#vp`
//	T.ltS       the server's p50 at render  -> reads `data-s` off the chip
//
// A regression here fails silently — a stale bundle serving yesterday's row
// count — so TestAppJSIsDeterministic guards it.
//
// The URL is `/a/{hash}/a.js`, where hash is the content's own SHA-256. Content
// addressing is what lets the response be `immutable` with a year of freshness
// and still be impossible to serve stale: a deploy that changes a byte changes
// the URL, and the document naming it is never cached. A mismatched hash is a
// 404 rather than a redirect, because the only way to hold a stale URL is to
// hold a stale document, which this application never issues.

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
)

// Datastar is vendored rather than fetched from a CDN: a second origin puts a
// DNS lookup, a TCP connection and a TLS handshake on the critical path of a
// demo whose whole subject is latency, and tells the CDN who is reading.
//
// The file is named for the exact build, so swapping it means editing the embed
// line by hand. That friction is deliberate: this is a custom bundle containing
// only the plugins this application uses, and a missing plugin does not throw —
// the attribute is simply inert, so the page boots, accepts keystrokes, posts
// commands, and silently ignores every SSE frame. A drop-in filename would make
// such a swap invisible; a named one makes it a decision. See vendorjs/README.md
// and TestBundleManifestMatchesWhatThePageUses, which checks the vendor's own
// manifest against the rendered page.
//
// No source map: the custom builder does not emit a sourceMappingURL, so there
// is nothing to serve and nothing for a relative reference to resolve against.

//go:embed vendorjs/datastar-1-0-2-4efd7a457ef99e73.js
var datastarJS []byte

// appJS is the page's scripts concatenated in document order. The order is
// load-bearing: clientTimingScript creates `window.__ss` and every other block
// opens with `var T=window.__ss; if(!T)return;`, so a block running before it
// would silently do nothing.
//
// It is one `<script defer>` in the head rather than several inline blocks in
// the body. Deferred classic scripts run after parsing, in document order, and
// before module scripts, so `__ss` is fully built before Datastar evaluates its
// first expression — the one ordering guarantee the page needs. Running after
// the parse also means each block can reach for `#vp` without being positioned
// below it by hand.
var appJS, appJSHash = buildAppJS()

func buildAppJS() (string, string) {
	var b strings.Builder
	for _, s := range []string{
		clientTimingScript,
		anchorScript(),
		gridKeysScript(),
		latencyScript(),
		bfScript(),
	} {
		b.WriteString(unwrapScript(s))
		// A newline and a semicolon between blocks. Each is already an IIFE, so
		// neither is strictly needed; both are here because the cost is two bytes
		// once and the failure they prevent — ASI joining the last statement of
		// one block to the first of the next — is the kind that produces a page
		// that half works.
		b.WriteString("\n;\n")
	}
	js := b.String()
	sum := sha256.Sum256([]byte(js))
	return js, hex.EncodeToString(sum[:])[:16]
}

// unwrapScript takes `<script>…</script>` and returns the JavaScript inside it,
// since the sources spell themselves as complete elements. It returns the input
// unchanged when the shape is not what it expects, so a block written without
// tags still bundles correctly.
func unwrapScript(s string) string {
	i := strings.Index(s, ">")
	if !strings.HasPrefix(s, "<script") || i < 0 {
		return s
	}
	inner := s[i+1:]
	if j := strings.LastIndex(inner, "</script>"); j >= 0 {
		inner = inner[:j]
	}
	return inner
}

// asset is one immutable file: its bytes, its brotli, and the hash that names it.
type asset struct {
	name  string // the last path segment, e.g. "a.js"
	ctype string
	raw   []byte
	br    []byte // pre-compressed at q11; nil if compression lost
	hash  string
}

func (a *asset) path() string { return "/a/" + a.hash + "/" + a.name }

// assets is keyed by "hash/name". A wrong hash is a miss, which is a 404.
var assets = map[string]*asset{}

// newAsset hashes the bytes, compresses them once, and registers the result.
//
// q11 (the maximum) is only defensible because it happens at startup. For a
// per-response body the level is a real trade — the page uses q5 because q6
// costs far more CPU for a handful of bytes — but an immutable asset is
// compressed once per process and served from memory afterwards, so bytes are
// the only remaining term, and q11 is worth roughly 11% over q5 here.
func newAsset(name, ctype string, raw []byte) *asset {
	sum := sha256.Sum256(raw)
	a := &asset{name: name, ctype: ctype, raw: raw, hash: hex.EncodeToString(sum[:])[:16]}
	var buf bytes.Buffer
	w := brotli.NewWriterLevel(&buf, brotli.BestCompression)
	if _, err := w.Write(raw); err == nil && w.Close() == nil && buf.Len() < len(raw) {
		a.br = buf.Bytes()
	}
	assets[a.hash+"/"+a.name] = a
	return a
}

var (
	appJSAsset    = newAsset("a.js", "application/javascript; charset=utf-8", []byte(appJS))
	datastarAsset = newAsset("datastar.js", "application/javascript; charset=utf-8", datastarJS)
)

// appJSPath and datastarPath are what the document points at.
func appJSPath() string    { return appJSAsset.path() }
func datastarPath() string { return datastarAsset.path() }

// handleAsset serves any of them.
//
// `immutable` is the point of the whole exercise: without it a browser still
// revalidates on reload, which is a round trip to be told nothing changed. With
// it, and with the hash in the path, a reload is free and a deploy is instant.
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	a, ok := assets[r.PathValue("hash")+"/"+r.PathValue("name")]
	if !ok {
		// A URL from a previous build. The only way to be holding one is to be
		// holding a document from a previous build, which cannot happen — the
		// document is no-store — so this is a scanner or a hand-edited URL, and
		// the honest answer is that the resource does not exist.
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("X-Content-Type-Options", "nosniff")
	// Vary is required, not decorative: two different byte streams are served
	// from one URL, and a shared cache that missed this would hand brotli to a
	// client that cannot read it.
	h.Set("Vary", "Accept-Encoding")

	body := a.raw
	if a.br != nil && acceptsBrotli(r) {
		// Setting Content-Encoding here is also what makes the compression
		// middleware skip this response (see main.go), so nothing is compressed
		// twice and no request pays for work already done at startup.
		h.Set("Content-Encoding", "br")
		body = a.br
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	// A zero modtime omits Last-Modified: the hash is the version, so a date
	// would be a second, weaker answer to the same question. ServeContent is
	// still worth having for Range and HEAD.
	http.ServeContent(w, r, a.name, time.Time{}, bytes.NewReader(body))
}

// acceptsBrotli reads the header rather than trusting the middleware, because
// this response deliberately bypasses the middleware.
//
// It does not parse q-values. The only decision here is "may I send the bytes I
// already have", a client that lists br at q=0 is a client that does not exist,
// and being wrong in that direction costs one re-request rather than a broken
// page.
func acceptsBrotli(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if enc, _, _ := strings.Cut(strings.TrimSpace(part), ";"); enc == "br" {
			return true
		}
	}
	return false
}
