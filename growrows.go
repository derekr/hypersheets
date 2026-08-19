package main

// growrows.go — "Add more rows at bottom": the door out of a fixed-size sheet.
//
// DefaultRows is a starting size, not a ceiling, and this control is what makes
// that true from the reader's side: without it a viewport command past the last
// row just clamps, so the sheet grows on write but not on scroll.
//
// Not auto-grow on scroll. A viewport command is a read, and SPEC.md's division
// is that commands write and views read. A read that mutates shared sheet state
// would let an idle reader enlarge a document for everybody else, permanently,
// with nothing in the log naming who did it — and it would be unbounded in
// exactly the direction a scrollbar drag is cheapest. An explicit control is a
// command: attributable (`structure` logs conn and author like every other
// write), rate limited (`/rows` is already classWrite in limits.go), refusable
// at the ceiling with a sentence, and it is the gesture spreadsheet users know.
//
// Where it lives: one absolutely positioned bar inside `#vp`, a sibling of `#g`,
// seated at
//
//	top: calc(var(--ch) + var(--rows) * var(--rh))
//
// — the first pixel past the last row. That one declaration means it is always
// in the DOM (no scroll handler, no threshold, no signal deciding whether to
// show it), only ever visible at the foot of the sheet, and it follows the
// extent for free: `--rows` is already a server-patched custom property on `#vp`
// (patchExtent, colWidthStyleExpr), so when any viewer adds rows every other
// viewer's bar walks down in the same signal patch that makes their scroll
// container taller. Nothing has to be kept in step.
//
// It is shell markup on the same terms as `#sb`, `#cb`, `#ov` and `#pc`: no push
// re-sends it, no morph can remove it, and it costs nothing per rendered cell.
//
// It sits below the last row rather than pinned to the bottom of the window: a
// floating bar would cover cells at every scroll position for the sake of an
// action nobody takes often.
//
// The command is `POST /s/{id}/rows` with `mop:'ap'`, which structure.go turns
// into `sh.InsertRows(sh.Rows(), n)` — the append position (`at == extent`,
// DATA-MODEL.md change 4). No new store API, no new route, and the rate limiter
// already knows the verb.
//
// The count travels as an explicit `payload` rather than as a signal, the same
// reason `#cl`/`#fl`/`#pv`/`#st` carry payloads: Datastar sends every ordinary
// signal with every request, so a count declared as one would ride on every
// scroll command the page issues for the sake of one click. `_gn` is therefore
// local (underscore-prefixed, never sent) and named explicitly in the one
// request that needs it. The payload replaces the signal set in the vendored
// bundle (`let A = d !== void 0 ? d : $({include,exclude})`), so `conn` has to
// be named too.

import "strconv"

// footID is the bar; growCountID is the editable count inside it.
const (
	footID      = "fo"
	growCountID = "gc"
)

// growSignal is the count, as a local signal. See the payload note above.
const growSignal = "_gn"

// growRowsOp is the `mop` value that means "append n rows at the bottom". It is
// a third operation on the existing row endpoint rather than a route of its own:
// `ia`/`ib`/`d` and this are all "reshape the row axis", they share the refusal
// path, the pending chip and the extent broadcast, and a new verb would silently
// fall out of limits.go's classWrite list.
const growRowsOp = "ap"

// growRowsDefault is what the box says before anybody touches it: the amount
// Google Sheets offers, and the amount that reads as "some more room" rather
// than as a decision.
//
// It is not DefaultRows, and must not be tied to it. DefaultRows is how big a
// sheet starts; this is how much you extend it by. Deriving one from the other
// makes the box offer a different amount whenever the starting size moves.
const growRowsDefault = 1000

// growRowsMax is the largest count the control will send in one command. It is
// not a limit on how tall a sheet may be — that is RowCeiling, enforced in the
// store — but on how much work one click may ask for: an append re-parses every
// formula in the sheet exactly as any other structural mutation does, so a
// fat-fingered 900,000 in the box should be refused by the input rather than
// held by the actor for a minute.
const growRowsMax = 100000

// growRowsSignalSeed is the `,_gn:1000` fragment of `data-signals`.
func growRowsSignalSeed() string {
	return `,` + growSignal + `:` + strconv.Itoa(growRowsDefault)
}

// growRowsHTML is the bar. The button posts from its own element, the
// `#cl`/`#fl`/`#pv`/`#st` rule: Datastar keys request cancellation on the
// issuing element, so a command sharing an issuer with the viewport command
// would be aborted by the next buffer edge the reader scrolls past. Being shell
// markup that no patch reaches, nothing can remove it mid-flight either.
//
// `class="pw"` and nothing else: it is the pending chip's backstop marker
// (chipFailScript) and a test counts the exact string, so the bar's appearance
// comes from `#fo button` rules in the stylesheet rather than a second class.
//
// The timeout is the same safety net the menu items carry — the server takes the
// chip down on the frame that carries the new extent, and this only fires if
// that frame never comes.
func growRowsHTML(sheetID string) string {
	return `<div id="` + footID + `"><div class="fi">` +
		// The keyboard belongs to the focused control. `vpKeyExpr` listens on the
		// window and excuses itself for INPUT/TEXTAREA/SELECT but not for a
		// button, so Enter on a focused button would reach the grid handler,
		// which calls preventDefault to move the selection down — and a prevented
		// keydown never becomes a click. Stopping propagation here kills the
		// event at the element it was aimed at, leaving native Enter/Space
		// activation intact and the page-wide handler unchanged.
		`<button class="` + pendingWriteCl + `" data-on:keydown="evt.stopPropagation()" data-on:click="` +
		pendingRaise(`Adding rows…`) +
		`@post('/s/` + sheetID + `/rows',{payload:{conn:$conn,mop:'` + growRowsOp +
		// `+$` coerces: data-bind on a number input hands back a number while the
		// signal holds one, but an emptied box is `''`, and a string in an int
		// field would be a 400 from ReadSignals rather than the default.
		`',mn:+$` + growSignal + `}})">Add</button>` +
		`<input id="` + growCountID + `" type="number" min="1" max="` +
		strconv.Itoa(growRowsMax) + `" step="100" data-bind="` + growSignal +
		`" aria-label="how many rows to add">` +
		`<span>more rows at bottom</span>` +
		`</div></div>`
}

// growRowsCSS is spliced into buildGridCSS so the page keeps one stylesheet.
//
// The bar is full grid width and its contents are `position:sticky;left:0`: it
// has to be as wide as the grid or it would leave the reader's eye after a
// horizontal scroll, and the controls have to stay at the left edge for the same
// reason the row-number gutter does (`#rn` is the precedent — sticky inside a
// positioned child of the scroll container).
//
// It adds `--fh` of scrollable overflow below the last row, since an absolutely
// positioned box whose containing block is the scroll container contributes to
// that container's scrollable overflow. The bar has to be reachable, and this
// changes no row arithmetic: every row-from-scrollTop calculation on the page is
// `floor((y - CH) / RH)` measured from the top and nothing here is above a row.
// `T.pageRows()` reads clientHeight, which content below does not affect, and
// `--fb` — the height `#vp` gives up to the formula bar — is untouched. Anything
// computing the bottom of the scroll range must subtract this height.
const growRowsCSS = `#` + footID + `{position:absolute;left:0;top:calc(var(--ch) + var(--rows) * var(--rh));z-index:3;width:var(--tw);height:var(--fh)}
#` + footID + ` .fi{position:sticky;left:0;display:flex;gap:6px;align-items:center;width:max-content;max-width:100%;height:var(--fh);padding:0 8px 0 calc(var(--hw) + 8px);color:#5f6368;font-size:12px}
#` + footID + ` button{height:24px;padding:0 10px;border:1px solid #dadce0;border-radius:4px;background:#fff;color:var(--bl);font:500 12px/1 Arial,Helvetica,sans-serif;cursor:pointer}
#` + footID + ` button:hover{background:#f6fafe;border-color:#d2e3fc}
#` + growCountID + `{width:5rem;height:24px;padding:0 6px;border:1px solid #dadce0;border-radius:4px;background:#fff;color:#202124;font:12px/1 ui-monospace,SFMono-Regular,monospace}
#` + growCountID + `:focus{outline:none;border-color:var(--bl);box-shadow:0 0 0 1px var(--bl)}
`

// footHeightPx is how tall the bar is. It joins the geometry list in render.go
// rather than sitting as a literal in the CSS above because it is a layout fact
// others depend on: the scrollable overflow the sheet gains past its last row.
const footHeightPx = 34
