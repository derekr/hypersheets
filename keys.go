package main

// keys.go — the interaction model.
//
// The grid owns focus, a cell is selected, and editing is a mode you enter.
// That is Google Sheets' model. The obvious alternative — an `<input>` that
// floats over the cell you clicked — is a form rather than a spreadsheet, and
// it fails three ways: a blur discards the edit instead of committing it, so
// clicking another cell loses data; after any blur `document.activeElement` is
// `<body>`, so a selected cell cannot be typed into; and there is nowhere for
// keyboard navigation to live.
//
// The rules implemented here, all of them Sheets':
//
//	SELECTED (not editing)
//	  arrows            move one cell            Ctrl/Cmd+arrow  edge of the data block
//	  Tab / Shift+Tab   right / left             Enter/Shift+Enter  down / up
//	  Home              column A of the row      Ctrl/Cmd+Home   A1
//	  End               last data column         PageUp/PageDown one viewport
//	  a printable key   start editing, replacing the cell with that character
//	  F2, double-click  start editing, preserving the cell's content
//	  Delete/Backspace  clear the cell (a command carrying an empty value)
//
//	EDITING
//	  Enter / Shift+Enter   commit, then move down / up
//	  Tab / Shift+Tab       commit, then move right / left
//	  Escape                abandon; the cell keeps its old value
//	  click another cell    commit, then select the clicked cell
//	  any other blur        commit; losing typed data is never the right default
//
// ─── Where the handlers live ──────────────────────────────────────────────────
//
// Everything is delegated onto two elements no patch ever touches. The byte
// budget rests on a cell being `<b id="D7" style="--r:6">490</b>` and nothing
// more; 13,000 `data-on:keydown` attributes would dominate every full render.
// So the keyboard is a single `data-on:keydown__window` on `#vp` — the scroll
// container, page-shell markup, never inside a patch region.
//
// `__window` rather than a focused grid, because focus is not dependable: after
// any blur the keyboard belongs to `<body>`, and a `tabindex` on `#vp` only
// moves the problem — a morph, a scroll patch, or a click on a cell can each
// take focus away again. A text-entry guard leaves the go-to box and the cell
// editor their own keystrokes.
//
// ─── Why the posts come from different elements ───────────────────────────────
//
// Datastar keys request cancellation on the element (`Vt.get(el)?.abort()` in
// the vendored bundle). Two consequences shape this file:
//
//   - `/viewport` is posted from `#vp` and `/cell` from `#ed`, never both from
//     one element. "Enter commits and moves down" is a cell command followed
//     immediately by a possible viewport command; issued from the same element
//     the second would abort the first, losing the edit.
//   - The cell command passes `requestCancellation:'disabled'`. Typing into a
//     cell, pressing Enter, typing into the next one and pressing Enter again
//     is ordinary spreadsheet speed, and under the default `'auto'` the second
//     commit aborts the first one's request. The bundle only registers the
//     abort controller for `'auto'` and `'cleanup'`, so any other value opts
//     out. Two commits are two commands; neither supersedes the other.
//
// That split is why keyboard commit is a two-step: `#ed` posts the cell, then
// dispatches a window event that `#vp` turns into the move. `#vp` is also where
// the Delete key's clear is turned back into a cell command — it dispatches the
// same commit event, so `#ed` remains the only element that ever posts `/cell`.
//
// The model costs nothing per cell, so the single-cell patch and the scroll
// patch are whatever sparse rendering makes them (RESULTS.md). The only
// per-render cost is `#ed`, re-emitted by the full path: a few hundred bytes
// once per full window against ~294 KB. See editorBytes.

import "strconv"

// commitEvent is the custom DOM event that means "post the current edit".
//
// It exists so `#ed` can be the only element that ever issues `/cell`. Anything
// needing a commit — the keyboard's Delete, a pointerdown on another cell —
// dispatches this on `window` instead of posting itself, and `#ed` answers it.
// One element, one in-flight cell command, no cross-element aborts. One
// lowercase word because the HTML parser lowercases attribute names and
// Datastar takes the event name straight from the attribute key.
const commitEvent = "sscmt"

// navEvent carries "commit is away, now move" from `#ed` to `#vp`. Its
// `detail` is one of u/d/l/r.
const navEvent = "ssnav"

// clearEvent carries "empty this range" from the keyboard to the one element
// that posts `/clear`. Its `detail` is the range in A1 notation.
//
// It exists for the same reason commitEvent does — per-element cancellation, as
// the header explains. A range clear is the slowest command here (a loop over
// cells; see handleClear) and the most expensive to lose, so it gets an element
// nothing else posts from.
const clearEvent = "ssclr"

// fillEvent and pasteEvent carry the other two selection-shaped operations from
// the keyboard to the one element that issues each of them.
//
// Four events and four elements, all for per-element cancellation: a fill and a
// paste are slow writes that must not be lost, and the selection commit is the
// opposite case needing the opposite behaviour, which it can only have by being
// the only thing its own element ever issues. See selPost.
const (
	fillEvent  = "ssfil"
	pasteEvent = "sspst"
)

// aggDebounceMs is how long the selection must stop moving before it is
// committed. A drag crosses a cell every few frames and each crossing writes two
// signals, so without it a 12-cell drag would issue twelve commands and
// supersede eleven. 180ms is longer than the gap between two cells of an
// ordinary drag and shorter than the pause before a reader looks at the
// toolbar.
const aggDebounceMs = 180

// cellPost is the one cell command in the page. See the header for why it opts
// out of Datastar's default request cancellation.
//
// It paints the committed value into the cell before it posts. Enter lowers
// `$editing`, which hides the editor and reveals the cell underneath — still
// holding the previous value, because the only thing that ever writes a cell is
// the server's push, a full round trip later. At 90 ms a leg that is ~11 frames
// of stale data on screen.
//
// The echo goes here, in the one expression every commit funnels through, for
// the same reason `/cell` has one issuer: Enter, Tab, blur, a pointerdown on
// another cell and Delete-to-clear are five doors into one command, and an echo
// on four of them is a bug on the fifth. It runs before `@post` and therefore
// before anything that moves the selection — `#ed` dispatches the nav event
// after this returns — so the paint lands on the committed cell, not on the one
// Enter moved to. See T.echo for what it may paint: a literal, never a computed
// value.
func cellPost(sheetID string) string {
	return echoCall + `@post('/s/` + sheetID + `/cell',{requestCancellation:'disabled'})`
}

// echoCall is the local echo, on the signals as they stand at commit time:
// `$ref` is the cell being left and `$raw` is what is being committed into it.
//
// `$raw` holds the selected cell's raw text at every moment, not just during an
// edit, because the formula bar reads it continuously and a write-only buffer
// cannot be read. fbSyncExpr is its one writer; see formulabar.go for why that
// is a single effect rather than a call on each of the dozen gestures that move
// the selection. The three commits carrying an explicit value — a printable key
// (`$raw=a.type`), Delete (`$raw=”`), F2 or a double-click
// (`$raw=rawOf($ref)`) — set it themselves.
const echoCall = `window.__ss&&window.__ss.echo($ref,$raw);`

// clearPost is the range clear. It carries the range and the connection and
// nothing else: `payload` replaces Datastar's usual "every non-underscore
// signal" body (`let A=d!==void 0?d:$({include,exclude})` in the vendored
// bundle), which keeps the selection out of the steady-state signal set. It
// disables request cancellation because a second slow write issued while the
// first is still running is two commands, not a correction of one.
//
// `$p` is the pending chip the structural commands use — a clear of a thousand
// cells is the same kind of wait — and the server clears the flag on the push
// that carries the emptied cells rather than on the command's response, which
// returns before anything is on screen.
func clearPost(sheetID string) string {
	return `if(!evt.detail)return;` + pendingRaise(`Clearing…`) +
		`@post('/s/` + sheetID + `/clear',{payload:{conn:$conn,rng:evt.detail},` +
		`requestCancellation:'disabled'})`
}

// fillPost and pastePost are the two large-range writes, each on an element of
// its own and each opting out of request cancellation for the reason clearPost
// gives. Both carry an explicit `payload` — the range, the direction or the
// destination, and the connection — so the selection is named on the requests
// that need it and on no other request the page will ever make.
func fillPost(sheetID string) string {
	return `if(!evt.detail)return;` + pendingRaise(`Filling…`) +
		`@post('/s/` + sheetID + `/fill',{payload:{conn:$conn,rng:evt.detail.rng,op:evt.detail.op},` +
		`requestCancellation:'disabled'})`
}

func pastePost(sheetID string) string {
	return `if(!evt.detail)return;` + pendingRaise(`Pasting…`) +
		`@post('/s/` + sheetID + `/paste',{payload:{conn:$conn,src:evt.detail.src,dst:evt.detail.dst},` +
		`requestCancellation:'disabled'})`
}

// selPost commits the settled selection: the same "manipulate locally, commit
// discretely" shape as the column resize and the buffer-edge crossing, where
// the gesture writes four local signals and one command follows it.
//
// It is the one write that keeps Datastar's default `'auto'` cancellation.
// `#cl`/`#fl`/`#pv`/`#st` opt out because a superseded command there is a lost
// intention — bold then italic is two intentions, a fill aborted halfway is
// cells that were never written. A selection commit is a last-writer-wins write
// of one value, and presence.go agrees from the other end: intermediate
// rectangles have no value, only the latest does. So aborting an older commit
// loses nothing and buys what this command wants — the answer that arrives is
// the answer to the newest question. It is also safer than `'disabled'`: two
// selection commands racing could land out of order and leave the server
// holding a rectangle the reader has left.
//
// The rectangle and the cell travel as an explicit `payload`. `$conn` is
// checked rather than assumed: on a cold load the effect below arms before
// `/live` has issued a connection id.
func selPost(sheetID string) string {
	return `if(!evt.detail||!$conn)return;` +
		`@post('/s/` + sheetID + `/sel',{payload:{conn:$conn,cell:evt.detail.cell,rng:evt.detail.rng}})`
}

// selEffectExpr is the trigger, and it is a `data-effect` rather than a handler
// on the dozen interactions that can move a selection.
//
// Drag, shift+click, Shift+Arrow, Ctrl/Cmd+Shift+Arrow, Ctrl/Cmd+A, a header
// click, Escape, a commit-and-move and every ordinary arrow all end in the same
// signals, so subscribing to the signals rather than to the gestures is one
// subscription instead of a dozen chances to forget one — the same argument as
// putting the local echo in cellPost.
//
// `$conn` is named for its subscription, not for its value. A cold load arms
// this effect before the stream has handed down a connection id, and the post
// above declines without one; naming the signal means the id's arrival re-runs
// the effect, so the anchored cell is committed as soon as there is a
// connection to commit it against rather than at whatever the reader touches
// first.
//
// It issues nothing itself: the effect re-arms a timer (T.selq) and the command
// comes from `#se` when that fires, so a 12-cell drag runs this twelve times
// and touches the network once, after the pointer stops.
const selEffectExpr = `if(window.__ss)window.__ss.selq($conn,$ref,` +
	`window.__ss.selRng($_sar,$_sac,$_sfr,$_sfc))`

// editorKeyExpr is the editing-mode keymap, on the editor itself because that is
// where the caret is.
//
// `evt.isComposing` is the IME guard and it comes first: during a multi-keystroke
// composition, the Enter that accepts a candidate must not commit the cell.
//
// `$editing=false` before `el.blur()` is load-bearing. Hiding a focused element
// blurs it, and the blur handler commits — so without the flag already down,
// every Enter would post twice.
func editorKeyExpr(sheetID string) string {
	return `if(evt.isComposing)return;const k=evt.key;` +
		`if(k==='Enter'||k==='Tab'){evt.preventDefault();$editing=false;el.blur();` +
		cellPost(sheetID) + `;` +
		`window.dispatchEvent(new CustomEvent('` + navEvent + `',{detail:` +
		`k==='Tab'?(evt.shiftKey?'l':'r'):(evt.shiftKey?'u':'d')}))}` +
		`else if(k==='Escape'){evt.preventDefault();$editing=false;el.blur()}`
}

// editorBlurExpr commits on any blur: clicking another cell, clicking the
// toolbar and alt-tabbing away all end the edit, and none of them should
// discard what was typed.
//
// The `$editing` test keeps that from double-posting: Enter, Escape and the
// pointerdown commit all lower the flag before the blur they cause arrives, so
// this fires only for a blur nobody has already accounted for.
func editorBlurExpr(sheetID string) string {
	return `if(!$editing)return;$editing=false;` + cellPost(sheetID)
}

// ─── Range selection ──────────────────────────────────────────────────────────
//
// A range is four local signals and one positioned box. It is per-viewer,
// transient client state: the server is never told, nothing is published, and
// no other viewer can see it.
//
//	$_sar/$_sac   the anchor: where the gesture started
//	$_sfr/$_sfc   the focus: where it is now
//
// A collapsed selection is anchor == focus, which is also "no range": `#sb`'s
// `data-show` is exactly that comparison, so a plain click leaves one element
// hidden rather than a zero-size range drawn on top of the active cell.
//
// The underscore prefix is what makes them local in Datastar: the bundle's
// fetch actions filter the signal payload with `exclude:/(^|\.)_/`, so these
// four ride on no request — zero wire cost by construction rather than by
// discipline. The commands that need a range carry it as an explicit `payload`.
//
// Anchor/focus rather than lo/hi, and rather than deriving the anchor from the
// active cell: Shift+click and Shift+Arrow both extend from a corner that has
// to survive the extension, and Ctrl/Cmd+A selects everything without moving
// the active cell, which a model anchored on `$row`/`$col` cannot express. The
// lo/hi rectangle is min/max of the two corners, computed where it is needed
// (T.selBox, T.selRng) and stored nowhere.
//
// The interactive box is `#sb`, not `#sl`. `#sl` (render.go) is the server's
// box: it exists so `?at=A1:D20` is correct on the first paint, before Datastar
// has been fetched, and can only ever say what the URL said. `#sb` lives in the
// page shell — outside every patch region, like `#vp` itself — placed by
// grid-column against the sheet's own column tracks in a shell grid (`#ov`)
// rather than in `#b`. So it costs nothing per render, survives a full morph
// and every scroll patch because none of them touch the shell, and is correct
// over empty cells (most of them, under sparse rendering) because it is a box
// over a coordinate space rather than a mark on elements that do not exist.
//
// The two boxes never coexist: `T.seed` removes the server's the moment
// Datastar boots (vpInitExpr), and the server renders it only on first paint.
// The signals are seeded from the same anchor in `data-signals`, so the linked
// range is already the client's before the reader can touch anything.

// vpPointerDownExpr is where selection begins, and it does three things in
// order: commit an edit in flight, then select, then start the drag.
//
// Selection happens on pointerdown rather than on click. A drag is
// pointerdown → pointermove* → pointerup, and Chrome fires a `click` at the
// common ancestor afterwards, so a click handler that selected the cell under
// the pointer would collapse every drag the instant it ended. It is also what a
// spreadsheet does: the rectangle follows the pointer from the first pixel of
// the gesture, not from its end.
//
// The commit must come first because it reads `$ref` and `$raw` as they stand,
// and the selection below is about to rewrite `$ref`.
//
// It ignores a pointerdown on the editor itself, which bubbles here because the
// editor is rendered inside `#g` inside `#vp`.
const vpPointerDownExpr = `const t=evt.target;` +
	`if($editing&&!(t&&t.id==='` + editorID + `')){$editing=false;` +
	`window.dispatchEvent(new Event('` + commitEvent + `'))}` +
	`if(!window.__ss)return;const d=window.__ss.selDown(evt);if(!d)return;` +
	// Shift extends: the anchor is whatever it already was, and only the focus
	// moves. Everything else is a new selection, collapsed onto the cell — or
	// onto the whole row or column, which is the same gesture with a different
	// rectangle.
	`if(d.ext){$_sfr=d.fr;$_sfc=d.fc;return}` +
	`$row=d.r;$col=d.c;$ref=d.ref;` +
	`$_sar=d.r;$_sac=d.c;$_sfr=d.fr;$_sfc=d.fc`

// vpPointerMoveExpr extends the drag and issues nothing: the whole gesture is
// two signal writes per cell crossed, and the box follows them. That is the
// rule the column resize follows (see rzMoveExpr) and the rule scrolling
// follows — a pointer-rate command is never the answer.
//
// It runs on every pointermove over the grid, including the ones that are just
// the mouse passing through, so the first thing it does is a null test on a
// property. T.selMove returns null unless a drag is live and the pointer has
// reached a different cell, so a drag across one cell writes signals once.
const vpPointerMoveExpr = `const m=window.__ss&&window.__ss.selMove(evt);` +
	`if(m){$_sfr=m.r;$_sfc=m.c}`

// vpPointerUpExpr ends the drag. It is on the window, not on `#vp`: a drag that
// leaves the grid and is released over the toolbar must still end, and a drag
// state that outlives its own gesture would turn the next idle mouse movement
// into a selection.
const vpPointerUpExpr = `if(window.__ss)window.__ss.selUp()`

// ─── Selection and navigation, on `#vp` ───────────────────────────────────────

// applyMoveExpr is the tail every movement shares: put the selection on
// `a.r`/`a.c`, follow it with the viewport, and ask for a new buffer if the move
// walked near the edge of the one we have.
//
// Keeping the selection visible is the one place keyboard nav touches the
// buffer design. An arrow key can walk off the rendered window — the buffer is
// 4 bands each side, not the whole sheet — so `reveal()` runs the same edge
// test the scroll handler runs (`$blo`/`$bhi`, the server's bounds) and returns
// the viewport to ask for when the move came within a band of an edge. The post
// is from `#vp`, the element the scroll handler also posts from, so a keyboard
// viewport request and a scroll viewport request supersede each other rather
// than racing.
//
// It also collapses the range, which is the whole of "any navigation key
// returns you to one cell". That lives here rather than in each arm of the
// keymap for the same reason the local echo lives in cellPost: arrows, Tab,
// Enter, Home, End, PageUp/Down and the move that follows a commit are eight
// doors into one movement, and a collapse on seven of them is a bug on the
// eighth.
func applyMoveExpr(sheetID string) string {
	return `const b=window.__ss.box(a.r,a.c);$row=a.r;$col=a.c;$ref=b.ref;` +
		`$_sar=a.r;$_sac=a.c;$_sfr=a.r;$_sfc=a.c;` +
		`const v=window.__ss.reveal(a.r,b.x,b.w,$blo,$bhi);` +
		`if(v){$lo=v.lo;$hi=v.hi;@post('/s/` + sheetID + `/viewport')}`
}

// applyExtendExpr is applyMoveExpr's other half: Shift+Arrow moves the focus
// and leaves the anchor and the active cell where they are. It still reveals,
// on the same terms — an extension that walks off the bottom of the screen has
// to bring the screen with it, and has to ask for buffer rows at the edge.
func applyExtendExpr(sheetID string) string {
	return `$_sfr=a.ext.r;$_sfc=a.ext.c;const b=window.__ss.box(a.ext.r,a.ext.c);` +
		`const v=window.__ss.reveal(a.ext.r,b.x,b.w,$blo,$bhi);` +
		`if(v){$lo=v.lo;$hi=v.hi;@post('/s/` + sheetID + `/viewport')}return`
}

// vpKeyExpr is the whole selected-mode keymap: one attribute on one element,
// with the decision of what a key means in `window.__ss.key` so this stays a
// dispatch rather than a program.
//
// The text-entry guard is what makes `__window` safe: the cell editor and the
// go-to box get their own keystrokes, everything else belongs to the grid.
func vpKeyExpr(sheetID string) string {
	return `if($editing)return;const t=evt.target;` +
		`if(t&&(t.tagName==='INPUT'||t.tagName==='TEXTAREA'||t.tagName==='SELECT'||t.isContentEditable))return;` +
		`if($ref===''||!window.__ss)return;` +
		`const a=window.__ss.key(evt,$row,$col,$blo,$bhi,$_sfr,$_sfc);if(!a)return;evt.preventDefault();` +
		// A read-only sheet still navigates, so the guard sits after the action
		// is resolved and before the three that mutate rather than at the top
		// of the handler: arrows, page keys, tab and the selection are reads
		// and must keep working. See readonly.go.
		`if($_ro&&(a.type!==undefined||a.edit||a.clear)){$note='` + readOnlyChipNote + `';return}` +
		// Typing replaces. `$raw` is the signal the command carries; __ss.edit
		// writes the same string straight into the input, because data-bind's
		// effect does not flush until this expression's batch ends and the caret
		// has to be placed against a value that is already there.
		`if(a.type!==undefined){$raw=a.type;$editing=true;window.__ss.edit(a.type);return}` +
		// F2 preserves. The raw text of a formula cell is its `data-r`, which is
		// what the user typed, not what it computed.
		`if(a.edit){const s=window.__ss.rawOf($ref);$raw=s;$editing=true;window.__ss.edit(s);return}` +
		// Delete on one cell is an ordinary cell command carrying an empty
		// value, routed through `#ed` so that element stays the only `/cell`
		// issuer. Delete on a range is a different command with a different
		// worst case (see handleClear), so it goes to its own element for the
		// same reason.
		`if(a.clear){const g=window.__ss.selRng($_sar,$_sac,$_sfr,$_sfc);` +
		`if(g){window.dispatchEvent(new CustomEvent('` + clearEvent + `',{detail:g}));return}` +
		`$raw='';window.dispatchEvent(new Event('` + commitEvent + `'));return}` +
		// Ctrl/Cmd+A selects the whole grid without moving the active cell,
		// which is why the anchor is its own pair of signals rather than
		// `$row`/`$col`. The far corner is the sheet's last row read from the
		// client's live extent rather than from a constant baked in at render
		// time: the page may have been open since before the sheet grew, and a
		// select-all stopping at row 999 of a 15,000-row sheet would be quietly
		// wrong. See T.rows in anchorScript. `window.__ss` is non-null here.
		`if(a.all){$_sar=0;$_sac=0;$_sfr=window.__ss.rows-1` +
		`;$_sfc=` + strconv.Itoa(MaxCols-1) + `;return}` +
		// Ctrl/Cmd+D and Ctrl/Cmd+R fill the range from its top row or its left
		// column. A collapsed selection has nothing to fill into, so it is a
		// no-op rather than a command — which is also what Sheets does.
		`if(a.fill){const g=window.__ss.selRng($_sar,$_sac,$_sfr,$_sfc);` +
		`if(g)window.dispatchEvent(new CustomEvent('` + fillEvent +
		`',{detail:{op:a.fill,rng:g}}));return}` +
		// Ctrl/Cmd+B and Ctrl/Cmd+I are the toolbar's two toggles reached from
		// the keyboard, and they go through the same event and the same element,
		// so the keystroke and the button cannot drift apart. The flip is
		// decided in the client because the store's wire format has no "toggle"
		// — see stylePost.
		//
		// Copy is purely local: one signal, no request, nothing published. It is
		// underscore-prefixed like the selection itself, so the clipboard rides
		// on no request until the paste that uses it. A collapsed selection
		// copies the active cell as a one-cell range, so the paste path has one
		// shape rather than two.
		`if(a.sty){window.__ss.sy({tog:a.sty});return}` +
		`if(a.copy){$_cp=window.__ss.selRng($_sar,$_sac,$_sfr,$_sfc)||($ref+':'+$ref);return}` +
		// Paste is one command carrying (sourceRange, destAnchor). The anchor is
		// the selection's top-left corner rather than `$ref`: a drag from the
		// bottom-right leaves the active cell at the far corner, and Sheets
		// pastes from the corner the rectangle starts at.
		`if(a.paste){if(!$_cp)return;` +
		`window.dispatchEvent(new CustomEvent('` + pasteEvent + `',{detail:{src:$_cp,` +
		`dst:window.__ss.a1(Math.min($_sar,$_sfr),Math.min($_sac,$_sfc))}}));return}` +
		// Escape collapses. It is the one key that does nothing else at all when
		// there is no range, which is exactly right: it means "never mind" — and
		// it drops the copy marquee too, which is the other thing "never mind"
		// has to mean once there is one.
		`if(a.esc){$_cp='';$_sar=$row;$_sac=$col;$_sfr=$row;$_sfc=$col;return}` +
		`if(a.ext){` + applyExtendExpr(sheetID) + `}` +
		`if(a.r===undefined)return;` +
		applyMoveExpr(sheetID)
}

// vpNavExpr is the second half of "commit and move": `#ed` has posted, and this
// moves the selection one cell in the direction it asked for.
func vpNavExpr(sheetID string) string {
	return `if(!window.__ss)return;const d=window.__ss.dirs[evt.detail];if(!d)return;` +
		`const a=window.__ss.mv($row+d[0],$col+d[1]);` +
		applyMoveExpr(sheetID)
}

// vpClickExpr selects; it does not open the editor. In Sheets a click selects
// and a double-click edits, and a click that entered edit mode would overwrite
// `$raw` with the clicked cell's text, destroying an uncommitted buffer.
//
// It does not read the clicked element, because under sparse rendering there
// usually isn't one: most of the grid is empty space, and empty space has to be
// clickable — a spreadsheet where an empty cell cannot be selected is not a
// spreadsheet. So hit-testing is arithmetic: the row is `scrollTop/rowHeight`
// and the column is a search over the 26 column rules' offsetLeft/offsetWidth,
// which are the only elements that have to exist for it to work. See T.hit.
//
// It is a fallback. The pointerdown handler above owns the gesture and
// `T.selTook()` says so, so this returns immediately for any click that had
// one. What is left is the click nobody pressed a pointer for —
// `element.click()`, an accessibility action, a test dispatching a bare
// MouseEvent — which still has to select a cell and collapse the range.
const vpClickExpr = `if(window.__ss&&window.__ss.selTook())return;` +
	`const h=window.__ss&&window.__ss.hit(evt);if(!h)return;` +
	`$ref=h.ref;$col=h.c;$row=h.r;` +
	`$_sar=h.r;$_sac=h.c;$_sfr=h.r;$_sfc=h.c`

// vpDblClickExpr enters edit mode preserving the content, the same door F2
// opens. The click that preceded it has already selected the cell; an empty
// cell opens an empty editor.
const vpDblClickExpr = `const h=window.__ss&&window.__ss.hit(evt);if(!h)return;evt.preventDefault();` +
	`const s=window.__ss.rawOf(h.ref);$raw=s;$editing=true;window.__ss.edit(s)`

// ─── The client half ──────────────────────────────────────────────────────────

// gridKeysScript hangs the keyboard's helpers off the same `window.__ss` object
// the timing harness and the anchor script use, so there is one place to look
// for "what does the client know". It must run after both of those.
//
// Everything here is geometry and DOM reading — no signals. The Datastar
// expressions above own the signals; these functions take the current row/col
// as arguments and hand back a target. That split is what stops the keymap from
// becoming a second, invisible store of client state, which SPEC.md forbids.
//
// The comments live here rather than in the script because everything below the
// backtick ships verbatim to every viewer inside an uncompressed 181 KB page
// shell: an explanation written there is paid for on every page load. What each
// helper is for:
//
//	strip(c)         the column-rule element for column c — one of the 26 `<i>`s
//	                 `#b` holds, and the column geometry oracle. Sparse rendering
//	                 guarantees no cell an element, but the rules always exist
//	                 and are sized by the same `grid-template-columns` the cells
//	                 are placed in, so offsetLeft/offsetWidth on one of them is
//	                 the truth about a column however its width was arrived at.
//	colAt(x)         the inverse: which column contains this content x. A linear
//	                 scan over 26 boxes, cheaper than duplicating the width
//	                 cascade in JS. Only what cannot be expressed as an address
//	                 needs pixels — which column is under this pointer, and how
//	                 far to scroll to bring one into view; the overlays are
//	                 placed by `grid-column` (render.go) instead.
//	hit(evt)         which cell is under the pointer, empty space included. It
//	                 refuses the two sticky chrome strips explicitly: the
//	                 row-number gutter covers the left HW px and the column
//	                 header the top CH px, so a click there is on the chrome even
//	                 though the arithmetic would happily name a cell under it.
//	mv(r,c)          clamp to the grid rectangle, so a move off an edge is a
//	                 no-op exactly as it is in a spreadsheet.
//	box(r,c)         a cell's ref plus its column's pixel left edge and width,
//	                 for `reveal`'s horizontal scrolling, which is the one
//	                 question with no answer in grid coordinates.
//	rawOf(ref)       what F2 opens on: a formula cell's `data-r` is what the user
//	                 typed, not what the cell displays. An empty cell has no
//	                 element and opens an empty editor.
//	edge(r,c,dr,dc)  Ctrl/Cmd+arrow, Sheets' rule: from inside a block of data
//	                 run to its last cell; from an empty cell, or from a block's
//	                 last cell, skip the empties to the next cell with something
//	                 in it. "Has something in it" is "has an element", which is
//	                 what sparse rendering means. The vertical walk is bounded by
//	                 $blo/$bhi rather than by the DOM, because a missing element
//	                 means "empty", not "outside the rendered window"; reaching
//	                 the edge stops the walk rather than claiming to know what
//	                 lies beyond, and the move reveals the next buffer so a
//	                 second press continues. The alternative is a round trip per
//	                 keystroke.
//	pageRows()       a viewport, minus the frozen header, minus one row of overlap.
//	key(evt,r,c)     what a keystroke means: {r,c} to move, {type} to start
//	                 editing on a character (replacing), {edit} to start editing
//	                 on the cell's own content (preserving), {clear} to empty it,
//	                 or null to let the browser have the key. Enter moves down
//	                 rather than opening the editor — that is F2 or a
//	                 double-click, and the two the wrong way round is the most
//	                 noticeable way a grid stops feeling like a spreadsheet.
//	edit(v)          open the editor on a value, deterministically. The value is
//	                 written straight onto the input rather than waited for:
//	                 setting `$raw` is a signal write inside data-on's batch and
//	                 data-bind's effect does not flush until that batch ends, so
//	                 the caret would land against the previous value's length.
//	                 `display` is removed by hand for a related reason — focus()
//	                 on a display:none element is a silent no-op.
//	echo(ref,raw)    paint the committed value into the cell now, so the round
//	                 trip is never a window in which the old value is on screen.
//	                 Formulas are always round trip, so the client must never
//	                 predict a computed value and has no engine to predict one
//	                 with. Hence three cases: a literal writes exactly the bytes
//	                 the server is about to send (the store keeps computed == raw
//	                 for a literal), so the confirming push morphs identical over
//	                 identical; a formula shows its own text dimmed and italic
//	                 (`class="f p"`), which is what the user typed and is visibly
//	                 unfinished rather than visibly wrong; empty removes the
//	                 element, because that is what an empty cell is under sparse
//	                 rendering — unless the cell carries a style, since a yellow
//	                 blank cell is a live element with no text. The style class
//	                 is read back off the DOM (T.scls) rather than copied,
//	                 because the confirming morph owns it: get that wrong and
//	                 committing into a bold red cell flashes it plain for the
//	                 round trip.
//	                 Both paints are claims the server can still refuse, so
//	                 flightClass rides on both until the push confirms them.
//	                 The echo must match the shape of the server's patch and not
//	                 just its text — a value in a previously-empty cell is an
//	                 insert, a clear is a remove — and doing that locally moves
//	                 the client's DOM out from under screen.heldMask, which is
//	                 why the command carries `conn` and handleCell updates the
//	                 mask; without it the server appends a second element with
//	                 the same id.
//	                 A commit that changes nothing is skipped
//	                 (`rawOf(ref)===raw`), which is also what makes digest
//	                 suppression safe: suppression needs bytes identical to the
//	                 previous patch, so it needs the same raw twice — exactly the
//	                 case that paints nothing.
//	mkc/rmc(...)     create and destroy a cell element the way this rendering
//	                 shape does: flat children of `#b` carrying `--r`, or a
//	                 wrapper per row under SS_ROW_GROUPS. Generated by Go so the
//	                 build ships one shape's worth of bytes, not both.
//	pnd(rec)         raise the pending marker on the cell just painted.
//	                 Deliberately a thin accent: the common commit settles in
//	                 about five frames, so anything heavier would strobe on every
//	                 keystroke. It is an inset box-shadow on the leading edge —
//	                 paint only, where a border or padding change would relayout
//	                 the committed cell's text. With `-pending-delay-ms` it is
//	                 scheduled instead, so only a slow commit is announced; the
//	                 timer lives on the record rather than in a global list,
//	                 because two cells can be in flight and the first's
//	                 confirmation must not cancel the second's marker.
//	clr(rec)         drop the marker. Nearly free — the server never emits the
//	                 class, so the confirming morph already removes it. This is
//	                 the backstop, run on 'finished', which the bundle dispatches
//	                 from a `finally` and therefore always: a marker must not
//	                 outlive its own request whatever the push does.
//	un(rec)          revert one echo, so a paint the server refused is not left
//	                 on screen as a lie. Datastar dispatches `datastar-fetch`
//	                 with `type:'error'` and `el` set to the issuing element on
//	                 any response >= 400, so the editor's own failures are
//	                 identifiable and the saved outerHTML goes back. Records are
//	                 dropped on 'finished', which always follows, so a later
//	                 failure cannot revert an edit that succeeded.
//	                 It is per-request, not per-cell, and cannot be otherwise:
//	                 `el` is always `#ed`, so two commits in flight — which
//	                 `requestCancellation:'disabled'` allows — are
//	                 indistinguishable here and the first 'finished' clears the
//	                 second cell's marker early. That is the safe direction of
//	                 the error: a marker that clears early understates, one that
//	                 strands lies.
//	fail(q)          the third state on the same mechanism. A refusal turns the
//	                 marker red for failHoldMs and only then reverts, so a value
//	                 changing back is an answer rather than a mystery. The
//	                 records are spliced out of T.pe first: the hold is a timer,
//	                 'finished' fires during it, and the revert must own its own
//	                 list or the clear would strand the lie on screen.
//	reveal(...)      keep the selection on screen, and say whether the buffer has
//	                 to move. The vertical arithmetic is against the frozen
//	                 column header — CH of flow at the top of the scroll content,
//	                 so row r is clear of the sticky strip when scrollTop <=
//	                 r*RH — and the horizontal against the HW-wide sticky
//	                 row-number gutter. The buffer test is deliberately the one
//	                 scrollExpr runs, over the same server-owned $blo/$bhi: an
//	                 arrow key walking toward the edge has to ask for rows
//	                 exactly as a wheel tick does. Assigning scrollTop fires a
//	                 synthetic scroll event, so the settle guard is armed and
//	                 this move issues one request.
func gridKeysScript() string {
	return `<script>(function(){var T=window.__ss;if(!T)return;
var vp=document.getElementById('vp'),RH=` + strconv.Itoa(rowHeightPx) +
		`,CH=` + strconv.Itoa(colHeadPx) + `,HW=` + strconv.Itoa(rowHeadPx) +
		`,COLS=` + strconv.Itoa(MaxCols) +
		`,GUARD=` + strconv.Itoa(edgeGuardRows) + `;
// The clamps below read T.rows rather than a constant, because a sheet grows
// when someone writes past its bottom and shrinks when rows are deleted. It is
// written only by T.setRows (anchorScript) from the _rows signal. COLS is a
// constant because the column axis really is fixed at 26 (grid.go).
T.dirs={u:[-1,0],d:[1,0],l:[0,-1],r:[0,1]};
T.a1=function(r,c){return String.fromCharCode(65+c)+(r+1);};
T.td=function(r,c){return document.getElementById(T.a1(r,c));};
T.mv=function(r,c){if(r<0)r=0;if(r>=T.rows)r=T.rows-1;if(c<0)c=0;if(c>=COLS)c=COLS-1;
 return {r:r,c:c};};
T.strip=function(c){var b=document.getElementById('` + bufferID + `');
 return b?b.children[c+1]:null;};
T.colAt=function(x){for(var c=0;c<COLS;c++){var e=T.strip(c);if(!e)return -1;
 var l=e.offsetLeft;if(x>=l&&x<l+e.offsetWidth)return c;}
 return -1;};
T.hit=function(e){if(!vp)return null;
 var t=e.target;if(t&&(t.id==='` + editorID + `'||(t.closest&&t.closest('#cols,#mn,#bz'))))return null;
 var rc=vp.getBoundingClientRect(),dx=e.clientX-rc.left,dy=e.clientY-rc.top;
 if(dx<HW||dy<CH)return null;
 var r=T.rowAtY(dy+vp.scrollTop-CH);if(r<0||r>=T.rows)return null;
 var c=T.colAt(dx+vp.scrollLeft);if(c<0)return null;
 return {r:r,c:c,ref:T.a1(r,c)};};
T.box=function(r,c){var e=T.strip(c);
 return {ref:T.a1(r,c),x:e?e.offsetLeft:0,w:e?e.offsetWidth:0};};
T.rawOf=function(ref){var e=document.getElementById(ref);if(!e)return '';
 return e.dataset.r!==undefined?e.dataset.r:e.textContent;};
T.filled=function(r,c){var e=T.td(r,c);return !!e&&e.textContent!=='';};
T.edge=function(r,c,dr,dc,blo,bhi){
 var ok=function(rr,cc){return rr>=0&&rr<T.rows&&cc>=0&&cc<COLS&&(!dr||(rr>=blo&&rr<=bhi));};
 var pr=r,pc=c,nr=r+dr,nc=c+dc;
 if(!ok(nr,nc))return T.mv(r,c);
 var run=T.filled(r,c)&&T.filled(nr,nc);
 while(ok(nr,nc)){var f=T.filled(nr,nc);
  if(run){if(!f)break;pr=nr;pc=nc;}else{pr=nr;pc=nc;if(f)break;}
  nr+=dr;nc+=dc;}
 return T.mv(pr,pc);};
T.pageRows=function(){var h=vp?vp.clientHeight:600;
 return Math.max(1,Math.floor((h-CH)/RH)-1);};
T.key=function(e,r,c,blo,bhi,fr,fc){var k=e.key,m=e.ctrlKey||e.metaKey;
 if(e.altKey)return null;
 var P=T.pageRows();
 if(m&&(k==='a'||k==='A'))return {all:1};
 // FILL IS ON CONTROL, NEVER ON COMMAND. Cmd+R is reload, and reload is the
 // gesture someone reaches for when a page is misbehaving — taking it in a demo
 // means the recovery key silently writes to their sheet instead. Sheets does
 // bind Cmd+R on a Mac and gets away with it because people know Sheets. The
 // Ctrl spelling is Excel's and stays. On Windows, where Ctrl IS the command
 // key, Ctrl+R is reload and this collides exactly as it does in Excel Online;
 // that one is a convention worth matching rather than a bug worth inventing a
 // second binding for.
 if(e.ctrlKey&&!e.metaKey&&!e.shiftKey)switch(k){
 case 'd': case 'D': return {fill:'d'};
 case 'r': case 'R': return {fill:'r'};}
 if(m&&!e.shiftKey)switch(k){
 case 'c': case 'C': return {copy:1};
 case 'v': case 'V': return {paste:1};
 case 'b': case 'B': return {sty:'bold'};
 case 'i': case 'I': return {sty:'italic'};}
 if(e.shiftKey)switch(k){
 case 'ArrowUp':    return {ext:m?T.edge(fr,fc,-1,0,blo,bhi):T.mv(fr-1,fc)};
 case 'ArrowDown':  return {ext:m?T.edge(fr,fc,1,0,blo,bhi):T.mv(fr+1,fc)};
 case 'ArrowLeft':  return {ext:m?T.edge(fr,fc,0,-1,blo,bhi):T.mv(fr,fc-1)};
 case 'ArrowRight': return {ext:m?T.edge(fr,fc,0,1,blo,bhi):T.mv(fr,fc+1)};
 case 'PageUp':     return {ext:T.mv(fr-P,fc)};
 case 'PageDown':   return {ext:T.mv(fr+P,fc)};
 case 'Home':       return {ext:T.mv(m?0:fr,0)};}
 switch(k){
 case 'ArrowUp':    return m?T.edge(r,c,-1,0,blo,bhi):T.mv(r-1,c);
 case 'ArrowDown':  return m?T.edge(r,c,1,0,blo,bhi):T.mv(r+1,c);
 case 'ArrowLeft':  return m?T.edge(r,c,0,-1,blo,bhi):T.mv(r,c-1);
 case 'ArrowRight': return m?T.edge(r,c,0,1,blo,bhi):T.mv(r,c+1);
 case 'Tab':        return T.mv(r,c+(e.shiftKey?-1:1));
 case 'Enter':      return T.mv(r+(e.shiftKey?-1:1),c);
 case 'Home':       return m?T.mv(0,0):T.mv(r,0);
 case 'End':        return m?null:T.edge(r,c,0,1,blo,bhi);
 case 'PageUp':     return T.mv(r-P,c);
 case 'PageDown':   return T.mv(r+P,c);
 case 'F2':         return {edit:1};
 case 'Delete': case 'Backspace': return {clear:1};
 case 'Escape':     return {esc:1};}
 if(m)return null;
 if(k.length===1)return {type:k};
 return null;};
T.edit=function(v){var e=document.getElementById('` + editorID + `');if(!e)return;
 var put=function(){e.style.removeProperty('display');
  if(e.value!==v)e.value=v;
  e.focus();
  try{e.setSelectionRange(e.value.length,e.value.length);}catch(x){}};
 put();
 if(document.activeElement!==e)requestAnimationFrame(put);};
` + echoScript() + `
T.reveal=function(r,x,w,blo,bhi){
 if(!vp)return null;
 var st=vp.scrollTop,top=T.topOf(r),bot=CH+T.topOf(r)+T.hOf(r)-vp.clientHeight;
 if(st>top)st=top;else if(st<bot)st=bot;
 if(st<0)st=0;
 var sl=vp.scrollLeft;
 if(x-HW<sl)sl=x-HW;else if(x+w>sl+vp.clientWidth)sl=x+w-vp.clientWidth;
 if(sl<0)sl=0;
 var moved=false;
 if(Math.abs(st-vp.scrollTop)>=1){vp.scrollTop=st;moved=true;}
 if(Math.abs(sl-vp.scrollLeft)>=1){vp.scrollLeft=sl;moved=true;}
 if(moved&&T.arm)T.arm(vp.scrollTop);
 var fr=T.rowAtY(vp.scrollTop),n=Math.ceil(vp.clientHeight/RH);
 if(moved&&T.track)T.track(fr);
 if(fr-blo<GUARD||bhi-(fr+n)<GUARD)return {lo:fr,hi:fr+n};
 return null;};
` + selectScript() + styleScript() + chipFailScript() + `
})();</script>`
}

// selectScript is the range-selection half of the client: pointer hit-testing
// for the two header strips, the drag state machine, and the geometry of the
// one box that draws the result. The explanation lives here and the browser
// gets the code, for the reason gridKeysScript gives.
//
//	yRow(e)        the row under a y coordinate, ignoring x. The gutter is only
//	               HW wide, and a row-header drag that strays a pixel into the
//	               grid must keep extending rather than stop dead, so the drag
//	               reads only the axis it is dragging along.
//	rowAt(e)       the row under a pointer actually on the gutter: the hit test
//	               for starting a row selection, where yRow continues one.
//	hdrAt(e)       the column under a pointer on the frozen letter strip, or -1.
//	               It refuses the two affordances the header already owns, the
//	               resize grip (`<i>`, rzDownExpr) and the menu caret (`<b
//	               class="cr">`); the right-click that opens the insert/delete
//	               menu is refused earlier, by the button test in selDown.
//	selDown(e)     what a pointerdown means: {r,c} is the new active cell,
//	               {fr,fc} the focus corner it selects to. A cell selects itself,
//	               a row number selects (r,0)..(r,COLS-1), a column letter
//	               selects (0,c)..(T.rows-1,c). `ext` is a shift-click, which
//	               moves only the focus.
//	selMove(e)     extend a live drag, returning null when nothing changed: this
//	               runs at pointer rate, and a signal written 60 times a second
//	               with the same value is 60 needless effect runs. It also ends a
//	               drag whose pointerup was lost — released outside the window,
//	               or swallowed by another element's capture — since `e.buttons`
//	               is the ground truth about whether a button is still down, and
//	               without it the next idle mouse movement would go on painting a
//	               rectangle nobody is dragging.
//	selUp()        end the drag and arm the aggregate it suppressed. The "was a
//	               drag actually live" test is load-bearing: this runs on every
//	               pointerup on the window, including ones over the toolbar, and
//	               re-arming unconditionally would re-issue the aggregate read
//	               for a selection nobody touched.
//	selTook()      "the pointerdown handled this gesture", consumed once. The
//	               click that follows a drag is Chrome's, not the user's; see
//	               vpClickExpr.
//	selRng(...)    the range in A1 notation, or '' when collapsed. The only place
//	               the selection is ever serialized, on one interaction (Delete)
//	               rather than on every request.
//	selBox(...)    where the box goes, as an address rather than as pixels: the
//	               top-left row, the row count, and a `grid-column` spanning the
//	               selected columns. Reading no DOM is what keeps it correct
//	               across a resize — pixels from the column rules would go stale,
//	               since a width change touches none of the four signals this is
//	               subscribed to, whereas a grid child of `#ov` is sized by the
//	               same `grid-template-columns` as the cells and moves in the
//	               same layout pass. The CH offset is `#ov`'s `top`.
//	selSize(...)   "4R × 3C", or '' when collapsed. Sheets shows it only while
//	               dragging; showing it whenever a range exists is a superset and
//	               one fewer piece of state.
//	rngBox(g)      the same geometry from an A1 range string — the copy marquee.
//	               The clipboard is one signal holding "B2:D5" because that is
//	               what the paste command sends, so drawing it needs the string
//	               parsed back: four characters of regex against a second pair of
//	               corner signals nobody would ever read.
//	selq(c,ref,g)  arm the selection commit, and issue nothing. It runs from a
//	               `data-effect` on the connection id, the active cell and the
//	               four selection signals, so it fires on every cell a drag
//	               crosses and every Shift+Arrow; all it does is re-arm a timer,
//	               and only the timer dispatches the event `#se` turns into one
//	               command. That is what keeps "the selection is server state"
//	               from becoming "a request per pointermove".
//	               The zero-request drag comes from `T.dg`, not from the timeout:
//	               a quiet timer alone is not enough because a slow drag outlasts
//	               it, whereas `T.dg` says a gesture is in progress and no timer
//	               is armed until selUp. It declines without a connection id —
//	               there is no screen to record a selection against until `/live`
//	               has issued one — and the effect names `$conn` so its arrival
//	               re-arms this.
//	seed()         take over from the server's first-paint box. `#sl` answered
//	               `?at=A1:D20` before Datastar existed; from here on the range
//	               is `#sb` and the four signals, seeded from the same anchor by
//	               the page shell. Removing it is not tidying: two translucent
//	               boxes over one range paint twice as dark.
func selectScript() string {
	return `T.dg=null;T.tk=false;T.ag='';T.aq=0;T.sc='';T.sr='';
T.yRow=function(e){if(!vp)return -1;
 var r=T.rowAtY(e.clientY-vp.getBoundingClientRect().top+vp.scrollTop-CH);
 return (r<0||r>=T.rows)?-1:r;};
// The grip band belongs to the resize, and this is the row axis's counterpart to
// hdrAt ignoring the column grips. Declining here is what keeps a resize drag
// from towing a row selection behind it; the resize also stops the event, so the
// two guards are belt and braces on purpose — see T.gripAt.
T.rowAt=function(e){if(!vp)return -1;var rc=vp.getBoundingClientRect();
 if(e.clientX-rc.left>=HW||e.clientY-rc.top<CH)return -1;
 if(T.gripAt(e)>=0)return -1;
 return T.yRow(e);};
T.hdrAt=function(e){var t=e.target;if(!t||!t.closest)return -1;
 if(t.tagName==='I'||(t.classList&&t.classList.contains('cr')))return -1;
 var s=t.closest('#cols span[data-c]');return s?+s.dataset.c:-1;};
T.selDown=function(e){if(e.button!==0)return null;
 var c=T.hdrAt(e);
 if(c>=0){T.dg='c';T.tk=true;
  return {r:0,c:c,fr:T.rows-1,fc:c,ref:T.a1(0,c)};}
 var r=T.rowAt(e);
 if(r>=0){T.dg='r';T.tk=true;
  return {r:r,c:0,fr:r,fc:COLS-1,ref:T.a1(r,0)};}
 var h=T.hit(e);if(!h)return null;
 T.dg='g';T.tk=true;
 return {r:h.r,c:h.c,fr:h.r,fc:h.c,ref:h.ref,ext:!!e.shiftKey};};
T.selMove=function(e){if(!T.dg||!vp)return null;
 if(!(e.buttons&1)){T.selUp();return null;}
 var n=null;
 if(T.dg==='c'){var c=T.colAt(e.clientX-vp.getBoundingClientRect().left+vp.scrollLeft);
  if(c>=0)n={r:T.rows-1,c:c};}
 else if(T.dg==='r'){var r=T.yRow(e);if(r>=0)n={r:r,c:COLS-1};}
 else{var h=T.hit(e);if(h)n={r:h.r,c:h.c};}
 if(!n||(T.lr===n.r&&T.lc===n.c))return null;
 T.lr=n.r;T.lc=n.c;return n;};
T.selUp=function(){var d=T.dg;T.dg=null;T.lr=-1;T.lc=-1;if(d)T.selq(T.sc,T.sr,T.ag);};
T.selTook=function(){var v=T.tk;T.tk=false;return v;};
T.selRng=function(ar,ac,fr,fc){if(ar===fr&&ac===fc)return '';
 return T.a1(Math.min(ar,fr),Math.min(ac,fc))+':'+T.a1(Math.max(ar,fr),Math.max(ac,fc));};
// The last argument is the height signal: both the subscription that makes this
// expression re-run when a row is resized and the data it resizes against. See
// T.topOf.
T.selBox=function(ar,ac,fr,fc,dep){if(dep!==undefined)T.setHeights(dep);
 var r0=Math.min(ar,fr),r1=Math.max(ar,fr),c0=Math.min(ac,fc),c1=Math.max(ac,fc);
 return {'--t':T.topOf(r0)+'px','--sh':(T.topOf(r1+1)-T.topOf(r0))+'px',
    gridColumn:(c0+2)+'/'+(c1+3)};};
T.selSize=function(ar,ac,fr,fc){
 var n=Math.abs(fr-ar)+1,m=Math.abs(fc-ac)+1;
 return (n>1||m>1)?(n+'R × '+m+'C'):'';};
T.rngBox=function(g){var m=/^([A-Z])(\d+):([A-Z])(\d+)$/.exec(g||'');if(!m)return {};
 return T.selBox(+m[2]-1,m[1].charCodeAt(0)-65,+m[4]-1,m[3].charCodeAt(0)-65);};
T.selq=function(c,ref,g){T.sc=c;T.sr=ref;T.ag=g;if(T.aq)clearTimeout(T.aq);
 if(T.dg||!c||!ref){T.aq=0;return;}
 T.aq=setTimeout(function(){T.aq=0;
  window.dispatchEvent(new CustomEvent('` + selEvent + `',{detail:{cell:T.sr,rng:T.ag}}));},` +
		strconv.Itoa(selDebounceMs) + `);};
T.seed=function(){var e=document.getElementById('` + selID + `');if(e)e.remove();};`
}

// pendClass is the formula's provisional text: dimmed and italic, because the
// cell is showing the formula the user typed and not the value it will have. It
// goes only on a formula — a literal echo paints the exact bytes the server is
// about to send, so its text needs no reinterpretation.
//
// It is a content treatment, not the pending state; flightClass is that, and
// goes on both.
//
// The server's HTML never carries either, so the confirming morph removes them:
// the bundle's attribute sync deletes every attribute the incoming element does
// not have (`for(...of Array.from(r.attributes)) !s.hasAttribute(l) &&
// removeAttribute` in the morph), so "pending" cannot outlive its own
// patch.
const pendClass = "p"

// flightClass is the pending state: one marker, both kinds of echo, from the
// paint until the push confirms it.
//
// It goes on the literal echo as well as the formula one because a literal
// paint is still a claim the server can refuse; unmarked, it would be
// indistinguishable from a value the server had agreed to.
//
// Subtle on purpose. The common commit settles in about five frames, so
// anything with presence would strobe on every keystroke. The stylesheet paints
// a 2px inset shadow on the cell's leading edge — no border, no padding, no
// background — so the marker cannot move a single character of text, which a
// per-commit reflow of the edited cell would.
const flightClass = "q"

// failClass is the same marker, refused. The pending accent turns red for
// failHoldMs before the value reverts, so a value changing back under the
// reader is an answer rather than a mystery. Same geometry, same zero layout
// cost — only the colour differs.
const failClass = "x"

// failHoldMs is how long the refusal is shown before the old value comes back.
// Long enough to register at a glance, short enough that the cell is not lying
// about its contents for any longer than it has to.
const failHoldMs = 450

// pendingDelay is the threshold variant, in milliseconds: how long a commit may
// be in flight before it is marked. 0 marks it immediately.
//
// The flag is read at render time rather than at init, so one build serves
// either reading without a rebuild.
func pendingDelay() int {
	if pendingDelayMs == nil {
		return 0
	}
	return *pendingDelayMs
}

// echoScript is the local-echo half of the client. It is generated rather than
// literal because creating a cell is rendering, and rendering has two shapes:
// flat children of `#b` positioned by `--r` (the default) and a wrapper per row
// (SS_ROW_GROUPS). Whichever the server is using, the echo must produce the
// same thing or the next patch fights it — an insert into a row whose wrapper
// the client already created would give two elements the same id. Generating it
// also means the build ships one shape's worth of bytes, not both.
func echoScript() string {
	col := ""
	if cellColMode {
		// Under SS_CELL_COL the column is a custom property instead of one of the
		// 26 id-prefix rules, so an echoed cell has to carry it or it lands in
		// whatever track auto-placement picks. 'A' is 65 and column A is track 2.
		col = `e.style.setProperty('--c',''+(ref.charCodeAt(0)-63));`
	}
	mk := `T.mkc=function(ref){var b=document.getElementById('` + bufferID + `');if(!b)return null;
 var e=document.createElement('b');e.id=ref;var rw=+ref.slice(1)-1;
 // The echo places itself. A rule cannot: setProperty writes a space
 // after the colon, which the server's per-row selectors do not match.
 e.style.setProperty('--r',''+rw);
 e.style.setProperty('--t',T.topOf(rw)+'px');e.style.setProperty('--hr',T.hOf(rw)+'px');` + col + `
 b.appendChild(e);return e;};
T.rmc=function(e){e.remove();};`
	if rowGroupMode {
		mk = `T.mkc=function(ref){var b=document.getElementById('` + bufferID + `');if(!b)return null;
 var h=ref.slice(1),g=document.getElementById('r'+h);
 if(!g){g=document.createElement('div');g.className='` + groupClass + `';g.id='r'+h;
  g.style.setProperty('--r',''+(+h-1));b.appendChild(g);}
 var e=document.createElement('b');e.id=ref;` + col + `g.appendChild(e);return e;};
T.rmc=function(e){var g=e.parentElement;e.remove();
 if(g&&g.className==='` + groupClass + `'&&!g.firstElementChild)g.remove();};`
	}
	return mk + `
T.pe=[];T.PD=` + strconv.Itoa(pendingDelay()) + `;
T.pnd=function(rec){var f=function(){var e=document.getElementById(rec.r);
  if(e)e.classList.add('` + flightClass + `');};
 if(T.PD>0)rec.t=setTimeout(f,T.PD);else f();};
T.clr=function(rec){if(rec.t)clearTimeout(rec.t);
 var e=document.getElementById(rec.r);if(e)e.classList.remove('` + flightClass + `');};
T.echo=function(ref,raw){if(!ref||typeof raw!=='string'||T.rawOf(ref)===raw)return;
 var e=document.getElementById(ref),rec={r:ref,h:e?e.outerHTML:''},s=T.scls(e);
 if(raw===''&&!s){if(!e)return;T.rmc(e);}
 else{if(!e&&!(e=T.mkc(ref)))return;
  var f=raw.charAt(0)==='=',t=raw.trim();
  var k=raw===''?'':(f?'f ` + pendClass + `':(t!==''&&isFinite(+t)?'':'t'));
  if(s)k=k?k+' '+s:s;
  if(k)e.className=k;else e.removeAttribute('class');
  if(raw==='')e.removeAttribute('data-r');else e.setAttribute('data-r',raw);
  e.textContent=raw;T.pnd(rec);}
 T.pe.push(rec);if(T.pe.length>8)T.clr(T.pe.shift());};
T.un=function(rec){var e=document.getElementById(rec.r)||(rec.h?T.mkc(rec.r):null);
 if(!e)return;if(rec.h)e.outerHTML=rec.h;else T.rmc(e);};
T.fail=function(q){var n=0;
 for(var i=0;i<q.length;i++){var rec=q[i];if(rec.t)clearTimeout(rec.t);
  var e=document.getElementById(rec.r);
  if(e){e.classList.remove('` + flightClass + `');e.classList.add('` + failClass + `');n++;}}
 var back=function(){while(q.length)T.un(q.pop());};
 if(n)setTimeout(back,` + strconv.Itoa(failHoldMs) + `);else back();};
document.addEventListener('datastar-fetch',function(v){var d=v.detail;
 if(!d||!d.el||d.el.id!=='` + editorID + `')return;
 if(d.type==='error')T.fail(T.pe.splice(0));
 else if(d.type==='finished'){while(T.pe.length)T.clr(T.pe.pop());}});`
}

// vpInitExpr puts the selection on the anchored cell the moment Datastar boots,
// because a spreadsheet opens with a cell selected. Without it the keyboard is
// inert until the reader happens to click something.
//
// It resolves `$ref` from `$row`/`$col`, which do come from the server: they are
// the anchor, and only the request line knows it.
//
// It also hands the linked region over from the server's box to the client's —
// see T.seed. That runs unconditionally and before the guard, because the
// handover has to happen on every load that carried a range, including those
// where something has already selected a cell.
const vpInitExpr = `if(!window.__ss)return;window.__ss.seed();if($ref!=='')return;` +
	`$ref=window.__ss.a1($row,$col)`
