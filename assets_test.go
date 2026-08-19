package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestAppJSIsDeterministic is the test that protects the whole feature.
//
// The bundle is served `immutable` for a year under a content-addressed URL, so
// a value that varies per request does not produce a visible error — it produces
// a page quietly running a bundle built at process start, with whatever row
// count or p50 happened to be true then. Three such values existed before this
// change (see assets.go), and every one of them was invisible.
func TestAppJSIsDeterministic(t *testing.T) {
	js1, h1 := buildAppJS()
	js2, h2 := buildAppJS()
	if h1 != h2 || js1 != js2 {
		t.Fatalf("bundle is not deterministic: %s vs %s", h1, h2)
	}
	if h1 != appJSHash {
		t.Fatalf("package hash %s does not match a fresh build %s", appJSHash, h1)
	}
}

// TestAppJSCarriesNoPerRequestNumbers names the three that used to be baked in,
// so a future edit that reintroduces one fails here rather than in production.
func TestAppJSCarriesNoPerRequestNumbers(t *testing.T) {
	for _, bad := range []string{
		"T.rows=1000;", "T.rows=10000;", // the extent — now reads --rows
		"T.anchorRow=1", "T.anchorRow=2", // the ?at= landing row — now reads data-ar
		"T.ltS=0.", "T.ltS=1", "T.ltS=2", "T.ltS=3", "T.ltS=4", // the p50 — now reads data-s
	} {
		if strings.Contains(appJS, bad) {
			t.Errorf("bundle contains a per-request literal %q; it must read that value from the DOM", bad)
		}
	}
	for _, want := range []string{
		"getPropertyValue('--rows')",
		"dataset.ar",
		"dataset.s",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("bundle does not read %q from the DOM", want)
		}
	}
}

// TestBundleCreatesTheToolbeltFirst pins the one ordering the page depends on.
// Every block but the first opens `var T=window.__ss;if(!T)return;`, so a block
// that ran ahead of the one that CREATES `__ss` would not error — it would
// return, silently, and the page would come up half wired.
func TestBundleCreatesTheToolbeltFirst(t *testing.T) {
	create := strings.Index(appJS, "window.__ss=")
	if create < 0 {
		t.Fatal("bundle never assigns window.__ss")
	}
	if first := strings.Index(appJS, "var T=window.__ss"); first >= 0 && first < create {
		t.Fatalf("a block reads window.__ss at %d, before it is created at %d", first, create)
	}
}

// TestPageLoadsTheBundleAndCarriesNoInlineScript is the document half.
func TestPageLoadsTheBundleAndCarriesNoInlineScript(t *testing.T) {
	page := pageShell("demo", 0, 249, "", zeroAnchor())

	if n := strings.Count(page, "<script>"); n != 0 {
		t.Errorf("page still carries %d inline <script> block(s); they belong in the cached bundle", n)
	}
	if !strings.Contains(page, `<script defer src="`+appJSPath()+`">`) {
		t.Errorf("page does not load the bundle at %s", appJSPath())
	}

	// `defer` before the module, or __ss is incomplete when Datastar evaluates
	// its first expression.
	bundle := strings.Index(page, appJSPath())
	module := strings.Index(page, `<script type="module"`)
	if bundle < 0 || module < 0 || bundle > module {
		t.Errorf("bundle at %d must come before the Datastar module at %d", bundle, module)
	}

	// The CSS deliberately did NOT move: it is render-blocking, and the
	// measurement in assets.go says the trade is bad for a first-time visitor.
	if !strings.Contains(page, "<style>") {
		t.Error("the stylesheet should still be inline — see assets.go for why")
	}
}

// TestAnchorTravelsAsDataNotCode covers the attribute that replaced seekCall.
func TestAnchorTravelsAsDataNotCode(t *testing.T) {
	at := zeroAnchor()
	if page := pageShell("demo", 0, 249, "", at); strings.Contains(page, "data-ar=") {
		t.Error("row 0 should not emit data-ar at all")
	}
}

func assetMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /a/{hash}/{name}", s.handleAsset)
	return mux
}

func TestAssetsServeImmutableAndRefuseAStaleHash(t *testing.T) {
	mux := assetMux(&Server{})

	for _, p := range []string{appJSPath(), datastarPath()} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 {
			t.Fatalf("%s = %d, want 200", p, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s Cache-Control = %q, want immutable — without it a reload still costs a revalidation round trip", p, cc)
		}
		// Two byte streams share this URL. A shared cache without Vary would
		// hand brotli to a client that cannot read it.
		if v := rec.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
			t.Errorf("%s Vary = %q, want Accept-Encoding", p, v)
		}
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/a/0123456789abcdef/a.js", nil))
	if rec.Code != 404 {
		t.Errorf("stale hash = %d, want 404: serving it would hand a year of immutability to the wrong bytes", rec.Code)
	}
}

func TestAssetsArePreCompressed(t *testing.T) {
	mux := assetMux(&Server{})
	for _, p := range []string{appJSPath(), datastarPath()} {
		r := httptest.NewRequest("GET", p, nil)
		r.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if got := rec.Header().Get("Content-Encoding"); got != "br" {
			t.Errorf("%s Content-Encoding = %q, want br from the startup-compressed copy", p, got)
		}

		// And identity when it is not offered — the bytes must still be readable
		// by a client that asked for none.
		r2 := httptest.NewRequest("GET", p, nil)
		r2.Header.Set("Accept-Encoding", "gzip")
		rec2 := httptest.NewRecorder()
		mux.ServeHTTP(rec2, r2)
		if got := rec2.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("%s served %q to a client that did not accept br", p, got)
		}
		if rec2.Body.Len() <= rec.Body.Len() {
			t.Errorf("%s: identity (%d) should be larger than brotli (%d)", p, rec2.Body.Len(), rec.Body.Len())
		}
	}
}

// datastarBuildSHA256 is the checksum of the vendored artifact. It is asserted
// rather than merely written down, so a careless re-copy of the wrong build — of
// which there has already been one — fails here instead of shipping.
const datastarBuildSHA256 = "ee67e070d3a168cdca0920bd25adad953074f4420a4196892f854bc01bfa405b"

func TestVendoredDatastarIsTheBuildWeThinkItIs(t *testing.T) {
	sum := sha256.Sum256(datastarJS)
	if got := hex.EncodeToString(sum[:]); got != datastarBuildSHA256 {
		t.Fatalf("vendored bundle sha256 = %s, want %s (%s)", got, datastarBuildSHA256, datastarBuild)
	}
	// A custom build emits no source map. If one appears, the map has to be
	// vendored and served from this script's own hash directory, because the
	// reference is relative — otherwise it is a 404 in the devtools of exactly
	// the audience this demo is for.
	if strings.Contains(string(datastarJS), "sourceMappingURL") {
		t.Error("the bundle now names a source map; vendor and serve it, or the reference dangles")
	}
}

// ─── The manifest is the authority, not this file ─────────────────────────────

// TestBundleManifestMatchesWhatThePageUses is the test that would have caught the
// broken build before it was ever loaded.
//
// A custom Datastar bundle omits plugins, and an omitted plugin is INERT rather
// than loud: no throw, no warning, no failed parse. The first custom build tried
// here left out PATCHELEMENTS and PATCHSIGNALS — the two plugins with no
// `data-*` spelling, so nothing in the markup hints at them — and produced a page
// that booted, took keystrokes, posted commands, received 204s, and silently
// discarded every SSE frame the server sent. It was indistinguishable from
// working until a second browser failed to see an edit.
//
// So the vendor's OWN MANIFEST is read and compared against the rendered page.
// Not a hand-kept list: the thing the builder actually produced, against the
// thing the application actually emits.
func TestBundleManifestMatchesWhatThePageUses(t *testing.T) {
	raw, err := os.ReadFile("vendorjs/" + datastarBuild + ".json")
	if err != nil {
		t.Fatalf("the build manifest must be committed beside the bundle: %v", err)
	}
	var doc struct {
		SourceSize int `json:"sourceSize"`
		Manifest   struct {
			Version string `json:"version"`
			Plugins []struct {
				Label string `json:"label"`
			} `json:"plugins"`
		} `json:"manifest"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("manifest: %v", err)
	}

	// The .js and the .json must be the same build, or everything below is
	// checking one artifact and shipping another.
	if doc.SourceSize != len(datastarJS) {
		t.Errorf("manifest describes a %d-byte bundle but the embedded one is %d bytes — "+
			"the .js and .json are from different builds", doc.SourceSize, len(datastarJS))
	}

	inBundle := map[string]bool{}
	for _, p := range doc.Manifest.Plugins {
		inBundle[strings.ToLower(p.Label)] = true
	}

	for _, p := range datastarCatalog {
		name := strings.ToLower(p.name)
		switch {
		case p.used && !inBundle[name]:
			t.Errorf("the page uses %s but the bundle does not contain it — the attribute "+
				"or frame type will be silently ignored at runtime", p.name)
		case !p.used && inBundle[name]:
			t.Errorf("the bundle contains %s but nothing uses it; rebuild without it", p.name)
		}
	}
}

func TestPageFetchesNothingFromAThirdParty(t *testing.T) {
	page := pageShell("demo", 0, 249, "", zeroAnchor())
	if strings.Contains(page, "jsdelivr") || strings.Contains(page, "//cdn.") {
		t.Error("the page still references a CDN; Datastar is vendored (see vendorjs/README.md)")
	}
	if !strings.Contains(page, `<script type="module" src="`+datastarPath()+`">`) {
		t.Errorf("page does not load the vendored Datastar at %s", datastarPath())
	}
}

// ─── What a custom Datastar build must contain ────────────────────────────────
//
// Datastar's site builds a bundle from a chosen plugin set. This is the choice,
// kept next to the thing that verifies it.
//
// WHY THIS IS A TEST AND NOT A NOTE IN A README. A plugin that is absent from the
// bundle does not throw, warn, or fail to parse — the attribute is simply INERT.
// `data-show` without the show plugin is an element that never hides;
// `data-bind` without bind is an input that silently stops feeding its signal.
// The whole class of failure is invisible, which is the class this codebase
// spends most of its comments on. The moment the bundle stops being "everything",
// the set of attributes the page may use has to be enforced rather than
// remembered.
//
// ADDING A PLUGIN HERE IS THE POINT: it is the reminder that the custom bundle
// must be rebuilt before that attribute does anything at all.

// datastarCatalog is every plugin Datastar publishes, as its own docs group them,
// mapped to the token that would appear in a rendered page. An empty token means
// the plugin has no page-visible spelling (the watchers are server-driven).
//
// The full catalog is listed — not just the ones in use — because the value of
// this test is catching an attribute that was never considered, and a list that
// only contains what we already do cannot do that.
var datastarCatalog = []struct {
	name  string // as the bundle builder spells it
	token string // how it appears in the document
	pro   bool
	used  bool
}{
	// actions
	{"FETCH", "@get(", false, true}, // also @post; see TestOnlyTheFetchActionIsUsed
	{"PEEK", "@peek(", false, false},
	{"SETALL", "@setAll(", false, false},
	{"TOGGLEALL", "@toggleAll(", false, false},

	// attributes
	{"ATTR", "data-attr", false, false},
	{"BIND", "data-bind", false, true},
	{"CLASS", "data-class", false, false},
	{"COMPUTED", "data-computed", false, false},
	{"EFFECT", "data-effect", false, true},
	{"INDICATOR", "data-indicator", false, false},
	{"INIT", "data-init", false, true},
	{"JSONSIGNALS", "data-json-signals", false, false},
	{"ON", "data-on", false, true},
	{"ONINTERSECT", "data-on-intersect", false, false},
	{"ONINTERVAL", "data-on-interval", false, false},
	{"ONSIGNALPATCH", "data-on-signal-patch", false, false},
	{"REF", "data-ref", false, false},
	{"SHOW", "data-show", false, true},
	{"SIGNALS", "data-signals", false, true},
	{"STYLE", "data-style", false, true},
	{"TEXT", "data-text", false, true},

	// watchers — no page token; the SERVER drives these, and both are load
	// bearing here. PATCHELEMENTS is the fat morph the whole design rests on;
	// PATCHSIGNALS carries every suppressed-digest update and the `_hb`
	// keepalive. Neither can be dropped.
	{"PATCHELEMENTS", "", false, true},
	{"PATCHSIGNALS", "", false, true},

	// pro — none used. Listed so that reaching for one is a decision, not a
	// surprise at bundle time.
	{"CLIPBOARD", "@clipboard(", true, false},
	{"FIT", "@fit(", true, false},
	{"INTL", "@intl(", true, false},
	{"ANIMATE", "data-animate", true, false},
	{"CUSTOMVALIDITY", "data-custom-validity", true, false},
	{"MATCHMEDIA", "data-match-media", true, false},
	{"ONRAF", "data-on-raf", true, false},
	{"ONRESIZE", "data-on-resize", true, false},
	{"PERSIST", "data-persist", true, false},
	{"QUERYSTRING", "data-query-string", true, false},
	{"REPLACEURL", "data-replace-url", true, false},
	{"SCROLLINTOVIEW", "data-scroll-into-view", true, false},
	{"VIEWTRANSITION", "data-view-transition", true, false},
}

// present reports whether the page uses a plugin, being careful that `data-on`
// is a PREFIX of `data-on-intersect`: an attribute name always ends at `="` or
// at `:` (the key separator), never at another name character.
func present(page, token string) bool {
	if token == "" {
		return false
	}
	if strings.HasPrefix(token, "@") {
		return strings.Contains(page, token)
	}
	return strings.Contains(page, token+`="`) || strings.Contains(page, token+":")
}

func TestPageUsesOnlyTheDatastarPluginsWeIntendToBundle(t *testing.T) {
	page := pageShell("demo", 0, 249, "", zeroAnchor())
	for _, p := range datastarCatalog {
		got := present(page, p.token)
		switch {
		case got && p.pro:
			t.Errorf("page uses %s (%s), a PRO plugin — that is a licensing and bundle "+
				"decision, not a stylistic one", p.name, p.token)
		case got && !p.used:
			t.Errorf("page uses %s (%s), which the custom bundle omits — the attribute "+
				"would be silently inert. Mark it used here AND rebuild the bundle.", p.name, p.token)
		case !got && p.used && p.token != "":
			t.Errorf("%s (%s) is marked used but appears nowhere; drop it from the bundle", p.name, p.token)
		}
	}
}

// TestOnlyTheFetchActionIsUsed pins the action half. FETCH covers every verb, so
// its presence is checked through the two this application actually issues.
func TestOnlyTheFetchActionIsUsed(t *testing.T) {
	page := pageShell("demo", 0, 249, "", zeroAnchor())
	for _, a := range []string{"@get(", "@post("} {
		if !strings.Contains(page, a) {
			t.Errorf("page no longer uses %s", a)
		}
	}
	for _, a := range []string{"@put(", "@patch(", "@delete("} {
		if strings.Contains(page, a) {
			t.Errorf("page uses %s; still FETCH, but worth noticing", a)
		}
	}
}

// TestBothWatchersAreActuallyDriven proves the two plugins with no page token are
// not cargo cult: the server really does send both frame types.
func TestBothWatchersAreActuallyDriven(t *testing.T) {
	for _, tc := range []struct{ plugin, fn string }{
		{"PATCHELEMENTS", "PatchElements"},
		{"PATCHSIGNALS", "MarshalAndPatchSignals"},
	} {
		found := false
		for _, f := range []string{"http.go", "render.go", "presence.go", "styleui.go", "latency.go"} {
			b, err := os.ReadFile(f)
			if err == nil && strings.Contains(string(b), tc.fn) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is marked used but nothing calls %s; drop it from the bundle", tc.plugin, tc.fn)
		}
	}
}
