package main

// index.go — the two routes that are about sheets rather than about a sheet.
//
//	GET  /         the index: every sheet on disk, newest first
//	POST /sheets   create a blank sheet and go to it
//
// Neither is Datastar. Both are navigations — a link and a form — so they work
// with the bundle blocked, they put a real URL in the address bar, and the
// browser's back button behaves without anything being wired up. Datastar earns
// its place on the grid, where a patch is cheaper than a page; here a page is
// the answer.
//
// POST-then-redirect, not POST-then-render: reload would otherwise create a
// second sheet, and 303 removes that.

import (
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// indexCSS is the index page's stylesheet. It shares the palette with the grid
// (buildGridCSS) but not the sheet itself: pulling in 26 column-width rules and
// a sticky table for a list of links would be silly.
const indexCSS = `*{box-sizing:border-box}
body{margin:0;background:#f8f9fa;color:#202124;font:14px/1.5 Arial,Helvetica,sans-serif}
main{max-width:44rem;margin:0 auto;padding:48px 24px}
h1{margin:0 0 4px;font-size:22px;font-weight:500}
p.sub{margin:0 0 24px;color:#5f6368;font-size:13px}
.bar{display:flex;align-items:center;gap:12px;margin-bottom:16px}
.bar .sp{flex:1}
.btn{display:inline-flex;align-items:center;height:36px;padding:0 16px;border:1px solid #dadce0;border-radius:4px;background:#fff;color:#1a73e8;font:500 14px/1 Arial,Helvetica,sans-serif;text-decoration:none;cursor:pointer}
.btn:hover{background:#f6fafe;border-color:#d2e3fc}
.btn.p{background:#1a73e8;border-color:#1a73e8;color:#fff}
.btn.p:hover{background:#1b66c9}
ul{margin:0;padding:0;list-style:none;border:1px solid #e1e3e1;border-radius:8px;background:#fff;overflow:hidden}
li{display:flex;align-items:center;gap:12px;padding:12px 16px;border-top:1px solid #f1f3f4}
li:first-child{border-top:0}
li:hover{background:#f8f9fa}
li a{flex:1;color:#202124;font-weight:500;text-decoration:none}
li a:hover{color:#1a73e8}
li .m{color:#5f6368;font-family:ui-monospace,SFMono-Regular,monospace;font-size:12px}
li .tag{padding:2px 8px;border-radius:10px;background:#e8f0fe;color:#1967d2;font-size:11px}
.empty{padding:32px 16px;color:#5f6368;text-align:center}
.src{margin:28px 0 0;color:#5f6368;font-size:12px;text-align:center}
.src a{color:#1a73e8;text-decoration:none}
`

// handleRoot lists the sheets. Once sheets can be created, "which sheets are
// there" is a question the front door has to answer, so this is a list rather
// than a redirect to the default sheet.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	sheets, err := ListSheets()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var b strings.Builder
	b.Grow(2048 + len(sheets)*160)
	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>hypersheets</title><link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Crect width='16' height='16' fill='%23fff'/%3E%3Cpath d='M0 5h16M0 10h16M5 0v16M10 0v16' stroke='%23cdd5e2'/%3E%3Crect x='5' y='5' width='5' height='5' fill='%233b5e8b'/%3E%3C/svg%3E"><style>`)
	b.WriteString(indexCSS)
	b.WriteString(`</style></head><body><main>`)
	b.WriteString(`<h1>hypersheets</h1><p class="sub">`)
	b.WriteString(strconv.Itoa(len(sheets)))
	b.WriteString(` sheet`)
	if len(sheets) != 1 {
		b.WriteByte('s')
	}
	b.WriteString(` · `)
	// DefaultRows, not a maximum: rows grow on demand, so this is where a sheet
	// starts rather than where it stops. Naming a per-sheet extent here would
	// mean opening every sheet in the list to draw a subtitle.
	b.WriteString(strconv.Itoa(DefaultRows))
	b.WriteString(`+ rows × `)
	b.WriteString(strconv.Itoa(MaxCols))
	b.WriteString(` columns each</p>`)

	b.WriteString(`<div class="bar"><span class="sp"></span>`)
	b.WriteString(`<form method="post" action="/sheets" style="margin:0">`)
	b.WriteString(`<button class="btn p" type="submit">New sheet</button></form></div>`)

	if len(sheets) == 0 {
		b.WriteString(`<div class="empty">No sheets yet.</div>`)
	} else {
		b.WriteString(`<ul>`)
		for _, sh := range sheets {
			esc := html.EscapeString(sh.ID)
			b.WriteString(`<li><a href="/s/`)
			b.WriteString(esc)
			b.WriteString(`">`)
			b.WriteString(esc)
			b.WriteString(`</a>`)
			if sh.ID == s.defaultSheet {
				b.WriteString(`<span class="tag">seeded</span>`)
			}
			b.WriteString(`<span class="m">`)
			b.WriteString(humanSize(sh.Size))
			b.WriteString(`</span><span class="m">`)
			b.WriteString(humanAge(time.Since(sh.ModTime)))
			b.WriteString(`</span></li>`)
		}
		b.WriteString(`</ul>`)
	}
	b.WriteString(`<p class="src"><a href="https://github.com/derekr/hypersheets">Source on GitHub</a> — a prototype to read, not a library to reuse.</p>`)
	b.WriteString(`</main></body></html>`)

	// no-store, not merely no-cache: this document is a live render whose shell
	// changes on every deploy and which embeds per-connection seeded signals
	// (buffer bounds, the latency p50, the anchor). Without a directive browsers
	// apply heuristic caching, so a deploy serves the previous shell — and a
	// shared cache could hand one reader another reader's seed.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.outboundLeg()
	_, _ = w.Write([]byte(b.String()))
}

// handleNewSheet creates a blank sheet and sends the browser to it.
//
// The id is server-generated (NewSheetID) rather than user-supplied, which is
// what keeps a sheet id a single legal NATS subject token: `a.b` and `a b` must
// never both be able to become the same bus subject.
func (s *Server) handleNewSheet(w http.ResponseWriter, r *http.Request) {
	id := NewSheetID()
	if err := CreateSheet(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	obsLog.InfoContext(r.Context(), "sheet.create", "sheet", id)
	s.outboundLeg()
	// 303, not 302: the browser must follow it with GET whatever it sent here.
	http.Redirect(w, r, "/s/"+id, http.StatusSeeOther)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatInt(n/(1<<10), 10) + " KB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}
