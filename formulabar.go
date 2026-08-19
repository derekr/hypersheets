package main

// formulabar.go — the strip under the toolbar that shows the active cell's raw
// text and lets you edit it.
//
// A formula is routinely wider than the cell it lives in, and the cell displays
// the answer anyway: `=SUM(A1:A200)*B7/12` in a 96px column is unreadable, and
// the in-cell editor opens at that same 96px. The bar is the one place on the
// page where the sheet's source is legible.
//
// It is a second input on the same signals, not a second source of truth. `#ed`
// — the floating in-cell editor — carries `data-bind:raw`, and so does `#fx`:
// Datastar's text-input bind is a getter, a setter and an `input` listener over
// the signal, so N inputs on one signal are N mirrors of one value, whichever
// has focus writing it. All the bar adds is an answer to "what is `$raw` when
// nobody is typing" (fbSyncExpr). A separate server-patched signal would be two
// things holding one cell's text and able to disagree, and every commit path in
// keys.go reads `$raw`.
//
// Focus arbitration: focus is exclusive — a document has one activeElement — and
// a transfer of focus commits. The rules, stated rather than left emergent:
//
//	Focusing the bar opens edit mode: `$editing=true`, the same flag F2 and a
//	  double-click raise. Three existing pieces of the interaction model key off
//	  it — `vpKeyExpr` refuses the grid keymap while `$editing`,
//	  `vpPointerDownExpr` commits on a pointerdown in the grid while `$editing`,
//	  and fbSyncExpr stands down — so without it, clicking a cell after typing in
//	  the bar would silently discard the typing.
//	The cell editor appears but does not take focus. `#ed`'s `data-show` is
//	  `$editing`, so the cell mirrors what the bar holds; only `T.edit`/`T.focus`
//	  call focus() on it, and neither runs from here.
//	Enter commits and moves down, Tab commits and moves right, shift reversing
//	  each — the four directions `editorKeyExpr` sends, through the same two
//	  events.
//	Escape abandons, and the revert needs no code: lowering `$editing` wakes
//	  fbSyncExpr, which puts the cell's own raw back into `$raw`. Escape in the
//	  cell editor reverts the bar by the identical path.
//	Any other blur commits, which is keys.go's rule for `#ed`: losing typed data
//	  is never the right default.
//
// Coarser than Sheets in one case: clicking from a live in-cell edit into the
// bar blurs `#ed` and `editorBlurExpr` commits, where Sheets would treat it as
// one continuous edit. A `relatedTarget` test in `editorHTML` would fix it, but
// `#ed` renders inside `#g`, so those bytes would ride every full window render
// forever. The intermediate commit writes what was typed and `$raw` is
// untouched, so the edit continues in the bar: one extra round trip, nothing
// lost.
//
// The bar issues no request. `/cell` has exactly one issuer, `#ed`, because
// Datastar keys request cancellation on the element and because the local echo,
// the pending marker and the failure revert all live in `cellPost` and key off
// `#ed` being the issuing element (`d.el.id==='ed'` in echoScript's
// `datastar-fetch` listener). A second `@post` here would be a commit path with
// none of that, and would silently halve `/cell`'s
// `requestCancellation:'disabled'`, which is what stops two fast commits
// aborting each other. So the bar dispatches `commitEvent` on the window and
// `#ed` answers it, as the Delete key and the pointerdown-commit already do.
//
// `#fb` lives in the page shell as a sibling of `#vp`, so no push, no scroll
// patch and no structural re-render can reach it — no `data-ignore-morph` to
// re-emit per render the way `#ed` needs inside `#g`, and no per-cell cost. It
// is also why the bar may hold focus across an unrelated viewer's edit landing
// in the same band.
//
// Because `$raw` is a signal rather than a DOM read, the bar stays right when
// the active cell scrolls off-screen: every door that moves the active cell ends
// with it inside the rendered buffer (`applyMoveExpr` calls `T.reveal`, and the
// `?at=` anchor is the buffer), and scrolling afterwards does not move `$ref`.
//
// Known limit: if another viewer changes the cell you have selected, the bar
// keeps showing the raw it last read, because an edit push patches elements and
// nothing the bar subscribes to. Fixing it needs a signal on the edit path — a
// wire cost paid by every viewer on every edit, for a case only one is in.
//
// The bar shows raw, always. A currency cell renders `$1,234.50` and carries
// `data-r="1234.5"`; `T.rawOf` prefers `data-r` over textContent, the same rule
// F2 and the double-click follow.

// formulaBarPx is the strip's height. It is a `--fb` custom property so that
// `#vp` can be `calc(100vh - var(--tb) - var(--fb))` and give up exactly the
// space the bar takes. Every other piece of viewport arithmetic reads
// `vp.clientHeight` — scrollExpr's row window, T.pageRows, T.reveal, T.seek — so
// subtracting it once here is enough; duplicating the number into any of those
// would leave a stale offset that shows up, silently, as the selection sitting
// one row off.
const formulaBarPx = 28

const (
	// formulaBarID is the strip. It carries the sync effect and issues nothing.
	formulaBarID = "fb"
	// formulaInputID is the second input on `$raw`.
	formulaInputID = "fx"
)

// formulaBarCSS is spliced into buildGridCSS so the page keeps one stylesheet.
//
// The `fx` glyph is a label, not a control: it is what makes a full-width text
// field read as a formula bar rather than a stray search box. The cell reference
// is deliberately not repeated here — `$ref` is already bound in the header one
// row above (`header .r`, always visible, never scrolled away), and a second
// element on the same signal is a second thing that can be wrong about a fact
// already on screen.
//
// `position:relative` and the z-index are the latency chip's, not the bar's: the
// chip's hover panel is an absolutely positioned child that has to hang out of
// the strip and over the sheet, and `#vp` is `position:relative` with an auto
// z-index, so without a stacking context here the panel would paint under the
// first row of cells. 7 sits above `#vp` and below `#mn` (9), the drag guide (9)
// and the toast (10), all of which must still win. See latency.go.
const formulaBarCSS = `#` + formulaBarID + `{display:flex;position:relative;z-index:7;flex:0 0 auto;gap:8px;align-items:center;height:var(--fb);padding:0 12px;border-bottom:1px solid var(--ln);background:#fff}
#` + formulaBarID + `>i{flex:0 0 auto;width:1.25rem;color:#5f6368;font:italic 600 13px/1 Georgia,'Times New Roman',serif;text-align:center;user-select:none}
#` + formulaInputID + `{flex:1 1 auto;min-width:0;height:calc(var(--fb) - 6px);margin:0;padding:0 4px;border:0;border-radius:2px;background:none;color:#202124;font:13px/1 ui-monospace,SFMono-Regular,monospace;outline:none}
#` + formulaInputID + `:focus{background:#f8f9fa;box-shadow:inset 0 0 0 1px var(--bl)}
`

// fbSyncExpr keeps `$raw` holding the active cell's text whenever nobody is
// typing. It is one subscription rather than a call on every door: twelve
// separate things move the active cell (the list applyMoveExpr enumerates), and
// a resync on eleven of them is a stale formula bar on the twelfth. Subscribing
// to the signals they all end in is one place instead of twelve.
//
// Every signal is read before the guard, on purpose: Datastar re-tracks an
// effect's dependencies on each run, so an early `return` that skipped a read
// would drop that signal from the dependency set and the effect would stop
// waking for it. Reading all six unconditionally pins the subscription.
//
//	$editing  stand down entirely while a human owns the buffer, whichever input
//	          they are typing into. This is also what makes Escape a revert with
//	          no revert code: lowering the flag wakes this.
//	$ref      the active cell moved.
//	$row      the guard's left-hand side, and the reason the guard can be cheap.
//	$blo/$bhi the server's buffer bounds, patched after the elements they
//	          describe (patchBounds runs after the elements patch in http.go), so
//	          waking on them means waking on DOM that has already landed. This is
//	          what re-syncs a jump whose rows arrived after the selection did.
//	$raw      compared, so a re-run that changes nothing writes nothing and the
//	          effect settles in one extra pass instead of looping.
//
// The `$row` in-buffer test is the off-screen case: `T.rawOf` answers the empty
// string for a cell with no element, and outside the buffer "no element" means
// "not rendered", not "empty". Declining to look is the only honest answer, and
// the signal already holds the right one from when the cell was in view.
const fbSyncExpr = `const e=$editing,r=$ref,q=$row,lo=$blo,hi=$bhi,v=$raw;` +
	`if(e||!window.__ss||r===''||q<lo||q>hi)return;` +
	`const s=window.__ss.rawOf(r);if(s!==v)$raw=s`

// fbFocusExpr enters edit mode. See the arbitration rules above for why this is
// the mode flag rather than a bar-local one: three existing pieces of the
// interaction model already key off `$editing`, and a bar edit that they could
// not see would be a bar edit a click on the grid throws away.
const fbFocusExpr = `if($ref===''||$editing)return;$editing=true`

// fbKeyExpr is editorKeyExpr's shape with the post replaced by a dispatch.
//
// `evt.isComposing` is tested first: during a multi-keystroke IME composition
// the Enter that accepts a candidate must not commit the cell.
//
// `$editing=false` before `el.blur()` is load-bearing here exactly as it is on
// `#ed`: blurring a focused element fires the blur handler, and the blur handler
// commits, so without the flag already down every Enter would commit twice.
//
// The window keydown handler on `#vp` sees this keystroke too and declines twice
// over: `$editing` is still true for a key this does not handle, and its
// text-entry guard catches `evt.target.tagName==='INPUT'` for the ones it does.
func fbKeyExpr() string {
	return `if(evt.isComposing)return;const k=evt.key;` +
		`if(k==='Enter'||k==='Tab'){evt.preventDefault();$editing=false;el.blur();` +
		`window.dispatchEvent(new Event('` + commitEvent + `'));` +
		`window.dispatchEvent(new CustomEvent('` + navEvent + `',{detail:` +
		`k==='Tab'?(evt.shiftKey?'l':'r'):(evt.shiftKey?'u':'d')}))}` +
		`else if(k==='Escape'){evt.preventDefault();$editing=false;el.blur()}`
}

// fbBlurExpr is editorBlurExpr with the post replaced by a dispatch. Any blur
// commits; the `$editing` test is what stops Enter, Tab, Escape and the
// pointerdown-commit — all of which lower the flag before the blur they cause
// arrives — from committing a second time.
const fbBlurExpr = `if(!$editing)return;$editing=false;` +
	`window.dispatchEvent(new Event('` + commitEvent + `'))`

// formulaBarHTML is the whole of it: one strip, one glyph, one input, four
// expressions and no request — plus two lodgers at the right-hand end, the
// retention chip and the latency chip. sheetID is only theirs.
//
// They ride here because the toolbar and the header have no room: at 1280px the
// header already ellipsises the sheet id and the row readout, and a latency chip
// truncated to `you ↔ server 365 m…` loses the second half of the split, which
// is the entire point of showing a split rather than one blended number. This
// strip has ~1,000px of nothing. `#fx` is `flex:1 1 auto` so it yields the
// width, the chips are `flex:0 0 auto` so they never shrink, and the viewport
// arithmetic does not move: the bar's height is `var(--fb)` whatever is inside
// it.
//
// They are lodgers rather than a second concern of this file — no signal in
// common with the bar, nothing issued, and their effects sit on their own
// elements. See latency.go.
func formulaBarHTML(sheetID string) string {
	return `<div id="` + formulaBarID + `" data-effect="` + fbSyncExpr + `">` +
		`<i aria-hidden="true">fx</i>` +
		`<input id="` + formulaInputID + `" data-bind:raw` +
		` autocomplete="off" spellcheck="false" aria-label="formula bar"` +
		` data-on:focus="` + fbFocusExpr + `"` +
		` data-on:keydown="` + fbKeyExpr() + `"` +
		` data-on:blur="` + fbBlurExpr + `">` +
		retentionChipHTML(sheetID) + latencyChipHTML() + `</div>`
}
