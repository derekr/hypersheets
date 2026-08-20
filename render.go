package main

// render.go — the windowed grid.
//
// The server renders a buffer: the rows on screen widened by BUFFER_BANDS on
// each side. The client scrolls natively inside it and only talks to the network
// when it approaches an edge. Two pieces of structure make that work. `#g` is
// `calc(var(--rows) * var(--rh))` tall and `--rows` inherits from `#vp`, so the
// container is the sheet's full height from the first paint and scrollTop is
// never corrected. And every rendered element carries `--r`, its 0-indexed sheet
// row, placed at `top: calc(var(--r) * var(--rh))` inside that box — a sparse
// scattering of elements at their true positions, not a strip that floats. DOM
// order under a translated `#b` would be worse three ways: a scroll patch can
// desync the rows from the offset (they arrive in separate SSE frames), rows
// revealed above and below would need a prepend and an append rather than one
// `append`, and a cell could not be inserted anywhere in the child list.
//
// An empty cell renders nothing at all. Dense rendering would emit 11,700
// `<td>`s for a 450-row buffer whether the sheet held anything or not; brotli
// crushes the redundant empties to almost nothing, so dense-empty is cheap on
// the wire and expensive in the DOM, and the DOM is where it hurts. Per screen:
// one `<b>` per non-empty cell, one `<b>` per buffer row, 26 `<i>` column rules,
// and no element at all for the horizontal grid lines.
//
// O(config) data goes in the stylesheet, O(cells) data in the markup. Column
// widths are 26 `--w-N` custom properties on `#vp`; column position is 26
// generated rules, since a cell id is A1 notation and so the column letter is a
// prefix. A cell therefore carries only `style="--r:250"` — 6-7 bytes less,
// 10-20% of its payload, ~10 points of fill on the sparse/dense break-even. Safe
// only because MaxCols is 26: `[id^=A]` also matches `AA1`, so a wider grid needs
// `[id^=AA]` rules ordered ahead of the single-letter ones, or an explicit `--c`.
//
// The buffer bounds are the server-owned signals `blo`/`bhi`, not attributes on
// `#g`, because a scroll ships row patches that never rewrite `#g` and stale
// attributes would silently stop the edge test firing. They are deliberately not
// `lo`/`hi`, which are client-owned — the browser writes the viewport it wants
// into them immediately before posting — because one pair for both directions
// would make the scroll handler read back its own request as the server's answer.
//
// Every render is one line with no newlines in it: the Datastar SDK splits an
// elements payload on "\n" and prefixes each line with "data: ".

import (
	"crypto/sha256"
	"encoding/hex"
	"html"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Layout geometry. These numbers appear in exactly two places — the stylesheet
// and the editor's positioning expression — and both read them from here.
const (
	rowHeightPx = 22 // one row
	// colWidthPx is the width of a column nobody has resized. It is
	// DefaultColWidth (mutate.go) rather than a number of its own, because an
	// absent row in the `cols` table means "the default" and the stylesheet is
	// the only place that can render that absence.
	colWidthPx = DefaultColWidth
	rowHeadPx  = 56 // the row-number gutter
	colHeadPx  = 24 // the frozen A/B/C… header strip
	toolbarPx  = 44 // the toolbar above the grid
	// The formula bar's strip height belongs to this list — it is the second
	// number `#vp`'s height subtracts — but it is declared in formulabar.go as
	// `formulaBarPx`, so the strip, its height, its stylesheet and its four
	// expressions are one thing to read and one thing to delete.
)

// Element ids. `g` is the patch target (the SSE loop always sends the whole of
// it); `b` holds the cells; `rn` holds the row-number gutter; `ed` is the editor
// — see the morph hole below.
//
// `b` and `rn` are the two targets of an incremental scroll patch, and `append`
// inserts inside the selected element, so both need a name of their own. There
// is no `prepend` target: every element carries its own absolute position, so
// rows revealed above and below the buffer are both an `append`.
const (
	gridID   = "g"
	bufferID = "b"
	gutterID = "rn"
	editorID = "ed"
	// selID is the linked-region overlay: one positioned box rather than a class on
	// every cell it covers, since most of the cells in a range do not exist.
	//
	// It is first paint only. `?at=A1:D20` has to be correct in the bytes that come
	// back from the request line, before the bundle has been fetched. Once the
	// client boots it takes the region over (`#sb`, keys.go) and removes this one;
	// the server never renders it again, because a server copy of interactive
	// selection would re-assert a stale range on every full morph.
	selID = "sl"
	// selBoxID is the interactive selection: the client's box, in the page shell.
	// It is `#ac` one rectangle larger — same coordinate space, same column
	// tracks, same zero bytes per cell — and living in the shell means no push
	// can remove it and no render pays for it.
	selBoxID = "sb"
	// copyBoxID is the copy marquee: `#sb` again, one rectangle over, drawn from
	// the clipboard signal instead of from the selection's four. It exists so a
	// copy is visible: a paste that lands somewhere unexpected is a data-loss
	// bug, and the only defence a spreadsheet offers is showing you what is on
	// the clipboard before you press Ctrl+V.
	copyBoxID = "cb"
	// cellOverlayID and boxOverlayID are the two local overlay grids, so every
	// positioned overlay derives its rectangle from `grid-template-columns`. `#oc`
	// is inside `#g` and holds the editor and the active-cell outline; `#ov` is in
	// the page shell and holds the selection box and the copy marquee; `#pc`
	// (presenceui.go) is the third. Pixels from `$x`/`$w` snapshotted at click time
	// would not survive a resize; as grid children they need no snapshot at all.
	//
	// They are `position:absolute` because that makes them the containing block for
	// their own children: an absolutely positioned child of a positioned grid
	// container with a definite `grid-column` is laid out against that grid area.
	cellOverlayID = "oc"
	boxOverlayID  = "ov"
	// extentID is the row-extent watcher: a hidden, request-free element in the
	// page shell whose only job is to copy the `_rows` signal into the client's
	// mutable row count when the sheet grows or shrinks. See pageShellWidths.
	extentID = "ex"
	// styleSheetID is the sheet's own stylesheet — one `<style>` element in the
	// page shell holding one rule per distinct cell appearance. It is O(config) in
	// CSS again, after the column widths and `--rows`; those are custom properties
	// a signal can carry, and a style is a rule no custom property can express, so
	// this is an element and is patched as one. It lives outside every patch region
	// so no push re-sends it, and a cell sharing a look carries four bytes.
	styleSheetID = "sy"
)

// cellRendered decides whether a cell exists in the DOM. It is one function
// because three places have to agree: the renderers, the server's model of what
// the browser holds (maskOf), and the edit patch that inserts and removes elements
// (renderCellPatch). A disagreement is an orphaned element that never disappears
// or a value that never arrives.
//
// "Has content or has style", because styling a cell makes it live and storage
// agrees. The inverse is the half that bites — clearing the style of a valueless
// cell kills it again (ClearStyle deletes rows left holding nothing), so this must
// go false on that edge too, or the client keeps an element the server has
// forgotten and no later patch can name it.
func cellRendered(c Cell) bool { return c.Kind != KindEmpty || c.Style != 0 }

// extentEffectExpr keeps T.rows in step with the server's extent. One
// subscription to one signal; no request, no DOM, no per-render cost.
const extentEffectExpr = `if(window.__ss){window.__ss.setRows($_rows);window.__ss.setHeights($_rhs)}`

// ─── The morph hole ────────────────────────────────────────────────────────────
//
// The actively-edited cell is client-owned territory: morphing an element that
// holds focus, a selection or a live IME composition destroys the caret and kills
// the composition mid-word. Every push re-renders the whole buffer and pushes
// arrive while the user is typing, so this is the normal case.
//
// The mechanism is `data-ignore-morph`, which the Datastar bundle honours only
// when it is on both the live node and the incoming node. The server therefore
// emits it, and unconditionally, because it cannot know which cell has focus at
// the instant an unrelated viewer's edit triggers a push. That forces the
// editable surface to be one element rather than 6,500: cells are inert and morph
// freely, and a single `<input id="ed" data-ignore-morph>` floats over the active
// cell, positioned by the client from `$row`/`$col`.
//
// Being present in every render also disposes of the related hazard — removing an
// element aborts its in-flight request, and this is the element that issues
// `@post(.../cell)`. See keys.go for why the commit and the move that follows it
// come from two elements.
// A textarea and not an input, for one reason: a cell can hold a line break.
// Wrapping made that visible — `pre-wrap` renders a newline as a newline — and
// Shift+Enter is how one is typed. An <input> cannot carry the character at all,
// so it would have silently dropped what the user pressed.
func editorHTML(sheetID string) string {
	return `<textarea id="` + editorID + `" data-ignore-morph data-bind:raw data-show="$editing"` +
		` data-style="` + cellBoxStyle + `"` +
		` rows="1" wrap="off" autocomplete="off" spellcheck="false" aria-label="cell editor"` +
		` data-on:keydown="` + editorKeyExpr(sheetID) + `"` +
		// Blur commits. Clicking another cell, clicking the toolbar or losing the
		// window must not throw away what the user has typed.
		` data-on:blur="` + editorBlurExpr(sheetID) + `"` +
		// The commit anyone else can ask for. Delete-to-clear and the pointerdown
		// that precedes a click on another cell both dispatch this rather than
		// posting themselves, so `/cell` has exactly one issuer.
		` data-on:` + commitEvent + `__window="` + cellPost(sheetID) + `"></textarea>`
}

// It writes pixels rather than `--r`, and that is not a style preference. Rows are
// not all the same height, so `--r` alone would send every overlay through the
// uniform fallback and place the editor and the active-cell outline in the gap
// between two rows on any sheet somebody has resized. Only the elements the
// SERVER renders can be positioned by row index, because only those get a
// per-row rule; anything the client places has to do the arithmetic.
//
// cellBoxStyle places a box flush over the active cell, shared by the editor and
// the active-cell outline so the two can never disagree.
//
// It carries no pixels. Arithmetic over `$x`/`$w` — snapshots of the clicked
// column's offsetLeft/offsetWidth — is correct only until the thing it copied
// moves, and a column width moves. `--r` plus a `grid-column` is the cell's
// address instead, resolved against the overlay grid's tracks (cellOverlayID), so
// a resize moves the outline in the same layout that moves the cell under it.
var cellBoxStyle = "{'--t':window.__ss.topOf($row,$" + heightSignal + ")+'px'," +
	"'--hr':window.__ss.hOf($row,$" + heightSignal + ")+'px'," +
	"gridColumn:($col+2)+'/'+($col+3)}"

// activeCellHTML is the outline on the selected cell: a sibling of the editor,
// in the same coordinate space, and what makes a cell look selected while it is
// not being edited — the editor only appears on `$editing`.
//
// It is `pointer-events:none` in CSS because it sits on top of the cell it
// outlines, and a selection box that swallowed the next click would make the
// grid feel broken in a way a real spreadsheet never does.
const activeCellID = "ac"

func activeCellHTML() string {
	return `<div id="` + activeCellID + `" data-show="$ref!==''" data-style="` + cellBoxStyle + `"></div>`
}

// cellOverlayHTML is the in-window overlay grid: the editor and the active-cell
// outline, and nothing else. One wrapper buys the two of them the same coordinate
// space the cells have, so they need an address rather than pixels.
//
// It stays inside `#g` rather than joining `#ov` in the shell, because `#ed` has
// to be re-emitted by every full render for `data-ignore-morph` to hold on both
// the live node and the incoming one.
func cellOverlayHTML(sheetID string) string {
	return `<div id="` + cellOverlayID + `">` + editorHTML(sheetID) + activeCellHTML() + `</div>`
}

// editorBytes is the fixed cost of the morph hole in every render.
var editorBytes = len(cellOverlayHTML("x"))

// renderWindow renders the dense rectangle Window returned for the inclusive row
// range [loRow, hiRow] into the `#g` patch region.
//
// cells must be exactly (hiRow-loRow+1)*MaxCols long and row-major, which is what
// Sheet.Window guarantees and why there is no gap handling here: an unwritten cell
// arrives as KindEmpty, not as a missing entry. A short slice renders fewer rows.
//
// sel is per-screen state — two people can look at the same rows with different
// regions linked — so it is threaded through rather than looked up.
func renderWindow(cells []Cell, loRow, hiRow int, sheetID string, sel selRange) string {
	loRow, nRows := clampWindow(cells, loRow, hiRow)

	var b strings.Builder
	// A sparse buffer is dominated by the row gutter when it is empty and by the
	// cells when it is full, so budget for both.
	b.Grow(nRows*36 + len(cells)/2*30 + editorBytes + len(sheetID) + 1024)

	// `--rows` is deliberately not written here, and that is how a grown sheet
	// reaches an already-open screen. The row extent is O(config), it changes
	// without any of these rows changing, and `#g` is the element every push
	// morphs — so a number living on `#g` could only ever be updated by
	// re-rendering the grid to change one integer. It lives on `#vp` instead, as
	// an inherited custom property patched by one signal, exactly like the 26
	// column widths. See pageShellWidths and patchExtent.
	b.WriteString(`<div id="` + gridID + `"><div id="` + gutterID + `">`)
	writeRowNums(&b, loRow, nRows)
	b.WriteString(`</div><div id="` + bufferID + `">`)
	writeColRules(&b)
	writeRowStrips(&b, loRow, nRows)
	b.WriteString(selHTML(sel))
	writeCells(&b, cells, loRow, nRows)
	b.WriteString(`</div>`)
	b.WriteString(cellOverlayHTML(html.EscapeString(sheetID)))
	b.WriteString(`</div>`)
	return b.String()
}

// renderWindowParts is renderWindow with the selection left out, returned as the
// two halves that surround it.
//
// The selection is the only part of a window that differs between two people
// looking at the same rows — everything else, the gutter, the column rules, every
// cell, is a pure function of the sheet and the range. Splitting there is what
// lets one render serve every viewer on a window: the halves are cached, and each
// screen pays only its own `head + selHTML(mine) + tail`.
//
// It is deliberately the same code as renderWindow rather than a parallel
// implementation, because two renderers that agree today and drift tomorrow is
// how a shared cache starts serving one viewer another viewer's grid.
// TestRenderWindowPartsRejoinExactly pins them together.
func renderWindowParts(cells []Cell, loRow, hiRow int, sheetID string) (head, tail string) {
	loRow, nRows := clampWindow(cells, loRow, hiRow)

	var h strings.Builder
	h.Grow(nRows*36 + 512)
	h.WriteString(`<div id="` + gridID + `"><div id="` + gutterID + `">`)
	writeRowNums(&h, loRow, nRows)
	h.WriteString(`</div><div id="` + bufferID + `">`)
	writeColRules(&h)
	writeRowStrips(&h, loRow, nRows)

	var t strings.Builder
	t.Grow(len(cells)/2*30 + editorBytes + len(sheetID) + 512)
	writeCells(&t, cells, loRow, nRows)
	t.WriteString(`</div>`)
	t.WriteString(cellOverlayHTML(html.EscapeString(sheetID)))
	t.WriteString(`</div>`)

	return h.String(), t.String()
}

// clampWindow puts [loRow,hiRow] inside the grid and shortens it to whatever the
// caller's cell rectangle actually covers.
//
// The sheet's own extent is not clamped here, because this function has no sheet
// and a compile-time row count would be wrong the moment a sheet grows. Two places
// that do hold a sheet clamp for it: Server.bufferRows before anything is read,
// and Sheet.Window, which returns fewer cells. RowCeiling is the sanity bound.
func clampWindow(cells []Cell, loRow, hiRow int) (lo, nRows int) {
	if hiRow < loRow {
		loRow, hiRow = hiRow, loRow
	}
	if loRow < 0 {
		loRow = 0
	}
	if hiRow > RowCeiling-1 {
		hiRow = RowCeiling - 1
	}
	nRows = hiRow - loRow + 1
	if have := len(cells) / MaxCols; have < nRows {
		nRows = have
	}
	if nRows < 0 {
		nRows = 0
	}
	return loRow, nRows
}

// writeColRules emits the vertical grid lines: one element per column, not one
// per cell. They are in-flow grid items, so `grid-template-columns` places and
// sizes them with no per-element markup — which also makes them the client's
// column-geometry oracle, since hit-testing over empty space has to turn an x
// coordinate into a column. See gridKeysScript's T.strip/T.colAt.
//
// A gradient cannot do this job the way it does the horizontal rules, because
// columns are individually resizable and the pitch is not constant. The first one
// covers the row-number gutter track, emitted rather than special-cased so
// auto-placement lands every following rule on its own column.
func writeColRules(b *strings.Builder) {
	for c := 0; c <= MaxCols; c++ {
		b.WriteString(`<i></i>`)
	}
}

// writeRowNums emits the row-number gutter for nRows rows starting at loRow.
//
// This is the one thing a blank sheet pays per row, and it is irreducible: the
// number is data. Everything else about an empty row — its height, its rule,
// its background — is CSS.
func writeRowNums(b *strings.Builder, loRow, nRows int) {
	for r := 0; r < nRows; r++ {
		row := loRow + r
		b.WriteString(`<b id="n`)
		b.WriteString(strconv.Itoa(row + 1))
		b.WriteString(`" style="--r:`)
		b.WriteString(strconv.Itoa(row))
		b.WriteString(`">`)
		b.WriteString(strconv.Itoa(row + 1))
		b.WriteString(`</b>`)
	}
}

// renderRowNums is the gutter half of an incremental scroll patch: `append`
// against `#rn`.
func renderRowNums(loRow, hiRow int) string {
	if hiRow < loRow {
		return ""
	}
	var b strings.Builder
	b.Grow((hiRow - loRow + 1) * 36)
	writeRowNums(&b, loRow, hiRow-loRow+1)
	return b.String()
}

// renderCellRows is the cell half of an incremental scroll patch: `append`
// against `#b`. cells is the dense row-major rectangle Window returns; only the
// non-empty entries produce an element.
func renderCellRows(cells []Cell, loRow, hiRow int) string {
	_, nRows := clampWindow(cells, loRow, hiRow)
	if nRows <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(cells) / 2 * 30)
	writeCells(&b, cells, loRow, nRows)
	return b.String()
}

func writeCells(b *strings.Builder, cells []Cell, loRow, nRows int) {
	for r := 0; r < nRows; r++ {
		row := loRow + r
		human := ""
		r0 := ""
		open := false
		for c := 0; c < MaxCols; c++ {
			cell := cells[r*MaxCols+c]
			if !cellRendered(cell) {
				continue
			}
			// Both spellings of the row are needed — 1-based for the A1 id,
			// 0-based for `--r` — and a row with no data pays for neither.
			if human == "" {
				human = strconv.Itoa(row + 1)
				r0 = strconv.Itoa(row)
			}
			if rowGroupMode && !open {
				writeGroupOpen(b, human, r0)
				open = true
			}
			writeCell(b, cell, byte('A'+c), human, r0)
		}
		if open {
			b.WriteString(`</div>`)
		}
	}
}

// ─── Row groups ────────────────────────────────────────────────────────────────
//
// The same idea one level up: a row that holds data gets a wrapper carrying `--r`
// once, and its cells carry only their id. It is a real alternative rather than a
// variant, because it moves three numbers in different directions:
//
//	bytes   a cell loses `style="--r:N"` (13-16 B) and a row gains a wrapper
//	        (~34 B), so groups win above ~2.5 non-empty cells per row.
//	drop    a dropped band is `#r1751,…` — O(rows) — instead of naming every
//	        cell it contains: 32 KB flat against 1.4 KB grouped, dense sheet.
//	layout  500 nested grid containers instead of one, the cost that does not
//	        show up in a byte count.
//
// MEASURED, AND IT LOSES. On a 10,000-row seeded sheet with 9,918 cells in the
// buffer, wrapping rows costs 49.4ms per morph against 6.3ms flat — 7.8x, with
// tight non-overlapping samples — because it is 450 nested grid containers
// rather than one. The byte win is real (-29% raw) and nowhere near worth that.
//
// The row strip (writeRowStrips) is what replaced it: an element per row that
// PAINTS but does not contain, which costs nothing measurable. Kept only so the
// comparison can be re-run.
//
// SS_ROW_GROUPS=1 selects it.
var rowGroupMode = os.Getenv("SS_ROW_GROUPS") == "1"

// ─── Row strips ───────────────────────────────────────────────────────────────
//
// One element per buffered row, carrying the horizontal rule and, later, the
// row's height and background. It is deliberately NOT the cells' parent: making
// the row a container means one nested grid per row, which measured 7.8x slower
// to morph on a dense sheet (6.3ms against 49.4ms, 450 wrappers, tight samples).
// A strip that only paints costs 0.3ms and about 400 compressed bytes.
//
// It is emitted for EVERY buffered row, not only rows holding data, because a
// rule and a background are properties of the row rather than of its contents —
// and painting a row background was impossible before this: sparse rendering
// emits nothing where a row is empty, so an axis-level fill had nothing to land
// on and tinted only the cells that happened to hold a value.
//
// Strips come before the cells in document order, so cells paint over them.
const stripClass = "rs"

// measureClass releases a cell's height for the length of one fit-to-contents
// measurement. See T.fitR.
const measureClass = "mz"

func writeRowStrips(b *strings.Builder, loRow, nRows int) {
	for i := 0; i < nRows; i++ {
		b.WriteString(`<i class="` + stripClass + `" style="--r:`)
		b.WriteString(strconv.Itoa(loRow + i))
		b.WriteString(`"></i>`)
	}
}

// groupClass is on the wrapper rather than being matched by an id prefix. `#sl`
// is also a `<div>` child of `#b`, and `[id^=r]` would be an attribute match
// against a value whose case-sensitivity is a rule nobody should have to
// remember — column R exists.
const groupClass = "w"

func writeGroupOpen(b *strings.Builder, human, r0 string) {
	b.WriteString(`<div class="` + groupClass + `" id="r`)
	b.WriteString(human)
	b.WriteString(`" style="--r:`)
	b.WriteString(r0)
	b.WriteString(`">`)
}

// renderCellGroup renders one row's cells inside a new wrapper. It is the
// insert half of the edit path in row-group mode: a value arriving in a row
// that had none needs the wrapper created with it.
func renderCellGroup(row int, cells []Cell) string {
	if len(cells) == 0 {
		return ""
	}
	human, r0 := strconv.Itoa(row+1), strconv.Itoa(row)
	var b strings.Builder
	b.Grow(len(cells)*28 + 40)
	writeGroupOpen(&b, human, r0)
	for _, cell := range cells {
		if !cell.Ref.Valid() || !cellRendered(cell) {
			continue
		}
		writeCell(&b, cell, byte('A'+cell.Ref.Col), human, r0)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// groupID is the wrapper's id, which is also the append target for a cell
// joining a row that already has one.
func groupID(row int) string { return "r" + strconv.Itoa(row+1) }

// writeCell emits one cell. It is also a patch on its own: an edit ships the cells
// its dirty set names, and those have to be byte-identical to what a full render
// would produce or the next full morph would flicker.
//
// The element carries its row and not its column: `--r` becomes
// `top: calc(var(--r) * var(--rh))`, and the column comes from the id via one of
// 26 generated rules. col, human and r0 are passed in rather than derived from
// cell.Ref so a row does not re-do the same conversions 26 times.
func writeCell(b *strings.Builder, cell Cell, col byte, human, r0 string) {
	// Stable per-cell id in A1 notation. Ids are what make morph correct —
	// without them Idiomorph matches by position and a single inserted node
	// shifts every value one cell over — they are what lets Datastar resolve a
	// bare cell patch with no selector at all (the default `outer` mode looks
	// the target up by the incoming element's own id), they are what the remove
	// selector names, AND they are now the column placement.
	b.WriteString(`<b id="`)
	b.WriteByte(col)
	b.WriteString(human)
	// One class attribute, carrying the kind and the style. An unstyled cell
	// emits `class="t"`, or no attribute at all for the common right-aligned
	// number, so per-cell markup does not grow for the overwhelming majority of a
	// sheet. A styled one appends four more bytes.
	if cls := cellClass(cell); cls != "" {
		b.WriteString(`" class="`)
		b.WriteString(cls)
	}
	// In row-group mode the wrapper states the row and the cell does not. This
	// is the only difference between the two shapes at the cell level.
	switch {
	case rowGroupMode && cellColMode:
		b.WriteString(`" style="--c:`)
		b.WriteString(strconv.Itoa(int(col-'A') + 2))
	case rowGroupMode:
	case cellColMode:
		b.WriteString(`" style="`)
		b.WriteString(rowVarDecl(r0))
		b.WriteString(`;--c:`)
		b.WriteString(strconv.Itoa(int(col-'A') + 2))
	default:
		// The row level of the cascade matches on this attribute, which is how a
		// row style costs no per-cell markup: `rowLevelSelector` reads `--r` out of
		// the style attribute. Change the spelling here and the row level stops
		// matching, which is why both sides call rowVarDecl.
		b.WriteString(`" style="`)
		b.WriteString(rowVarDecl(r0))
	}
	// `data-r` is what the user typed, so F2 on `=A1*2` opens the formula rather
	// than the value, and an editor opened on a currency cell commits 1234.5 rather
	// than the string `$1,234.50`. The test is `Display != Computed` rather than
	// "has a format", because a format that does not alter this particular value
	// leaves the text equal to the raw and needs no attribute.
	if cell.Kind == KindFormula || cell.Kind == KindError || cell.Display != cell.Computed {
		b.WriteString(`" data-r="`)
		b.WriteString(html.EscapeString(cell.Raw))
	}
	b.WriteString(`">`)
	// Display, not Computed, and the split is load-bearing: Computed is the
	// machine value (what recalc reads and the event log records) and Display is
	// that value with this cell's number format applied. They are the same string
	// for every unstyled cell in the sheet.
	b.WriteString(html.EscapeString(cell.Display))
	b.WriteString(`</b>`)
}

// cellClass is the kind class and the style class, joined. Neither can be
// folded into the other: the kind decides the default presentation (a number is
// right-aligned, a formula blue, an error red on pink) and the style is what
// someone asked for on top of it. So a bold red text cell is `class="t s3"` and
// `#b b.s3` wins by source order over `#b b.t` at equal specificity.
//
// It allocates only for a styled cell; the unstyled cases return constants.
func cellClass(cell Cell) string {
	kind := ""
	switch cell.Kind {
	case KindText:
		kind = "t"
	case KindFormula:
		kind = "f"
	case KindError:
		kind = "e"
	}
	sc := StyleClass(cell.Style)
	switch {
	case sc == "":
		return kind
	case kind == "":
		return sc
	default:
		return kind + " " + sc
	}
}

// renderCells renders a scattered set of cells as a run of bare elements, in the
// order given. Datastar patches each one over the element that already carries its
// id, so the run needs no wrapper and no selector — which is what makes a 10-cell
// cascade a few hundred bytes instead of a buffer.
//
// Cells with nothing to draw are skipped: an edit that clears a cell is a remove,
// not a patch, and pushCells sorts the dirty set into the two piles first.
func renderCells(cells []Cell) string {
	if len(cells) == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(cells) * 34)
	for _, cell := range cells {
		if !cell.Ref.Valid() || !cellRendered(cell) {
			continue
		}
		writeCell(&b, cell, byte('A'+cell.Ref.Col),
			strconv.Itoa(cell.Ref.Row+1), strconv.Itoa(cell.Ref.Row))
	}
	return b.String()
}

// ─── The sheet's own stylesheet ──────────────────────────────────────────────
//
// One rule per distinct cell appearance, generated from the store's style table
// and emitted once into the page shell — O(config) in CSS, O(cells) in markup,
// which is why a bold red cell costs the four bytes `s3` rather than
// `style="font-weight:700;color:#cc0000"` on every render of every band.
//
// The selector is `#b b.s3` and not `.s3`, and the extra seven bytes are
// load-bearing: the grid's own rules are (1,1,1), a bare `.s3` is (0,1,0) and
// would lose to all of them, so a red formula would stay blue.
//
// A style whose CSS body is empty produces no rule — the number-format case,
// where the transform happens to the text in the store, as Cell.Display. The cell
// still carries the class, which keeps "does this cell have a style" answerable
// from the DOM alone.

// ─── The three-level cascade ─────────────────────────────────────────────────
//
// A cell's look resolves `cell ?: row ?: col ?: default`, expressed in CSS rather
// than per cell. That is the whole reason the levels exist: "make column D
// currency" is one record in the store and one rule here, against 10,000 cell
// writes and 10,000 four-byte classes — which also materializes 10,000 rows that
// did not exist.
//
// Precedence is source order, which fixes the order below: all three selectors
// are (1,1,1), so CSS breaks the tie by which rule was written last.
//
//	column   #b b[id^=D]                  the column letter IS the id prefix
//	row      #b b[style="--r:12"]         the row number IS the position var
//	cell     #b b.s3                      the class the cell already carries
//
// The renderer emits `StyleClass(cell.Style)`, the cell's own record, and not
// `Cell.Effective`: the resolved id would apply every level twice. The bodies are
// `CSSFull()`, not `CSS()`, because a cell must be able to override a level back
// to plain and an empty body is not an override — see style.go.

// sheetBG is the page's own background, stated so that a cell's own record can
// hide a level underneath it. `CSSFull` says `background:transparent` for a style
// with no fill, which is wrong when a column backdrop is behind the cell (see
// colBackdropCSS): record-level resolution means a cell with its own style ignores
// its column entirely, so the rule for that record has to paint.
//
// It is safe to be opaque: a cell is `--rh - 1` tall and stops 1px short on the
// right, so it covers neither the horizontal row rule nor the vertical one.
const sheetBG = "#fff"

// ruleBody is one style's declarations. `over` says there is a level underneath
// this rule that it has to be able to hide.
//
// The column level is the bottom and keeps `transparent`; the row and cell
// levels sit over it and paint. Opaque where it matters, or a cell that
// overrode a yellow column back to plain would still be yellow, because the
// backdrop is a different element. Transparent where it does not, or the 26
// rules a select-all produces would paint every cell in the sheet opaque white
// and take the error cell's pink (`#b b.e`) with them.
func ruleBody(st Style, over bool) string {
	css := st.CSSFull()
	if over && st.BG == "" {
		// The one token CSSFull emits for "no fill", swapped for one that paints.
		// TestRuleBodiesNeverLeaveAHoleForALevel pins that nothing gets past this.
		css = strings.ReplaceAll(css, "background:transparent", "background:"+sheetBG)
	}
	return css
}

// sheetStyleCSS is the whole stylesheet for one window: the column level, the row
// level and the per-cell rules, in that order.
//
// It takes a window because the row level is unbounded while the column level is
// 26 integers and the cell level is O(distinct looks). It is re-derived per screen
// on every push for the same reason, and `screen.sentStyles` compares what it last
// sent, so a scroll revealing no styled row patches nothing.
func sheetStyleCSS(sh *Sheet, loRow, hiRow int) string {
	if sh == nil {
		return ""
	}
	rules := sh.StyleRules()
	cols := sh.ColStyles()
	// A read failure here is not worth failing a render for: the row level goes
	// missing for one frame and the next push restates it, where returning an
	// error would cost the reader the whole grid.
	rows, err := sh.RowStyles(loRow, hiRow)
	if err != nil {
		rows = nil
	}
	css := cascadeCSS(rules, cols, rows)

	// Geometry rides the same per-window stylesheet as the cascade: both are
	// derived per screen on every push and compared against what that screen
	// last held, so a window with no resized row patches nothing. It is appended
	// rather than folded into cascadeCSS because a sheet with no styles at all
	// still has geometry, and cascadeCSS returns early on an empty style table.
	heights, herr := sh.RowHeights(loRow, hiRow)
	if herr != nil {
		return css
	}
	base, berr := sh.RowOffset(loRow)
	if berr != nil {
		return css
	}
	total, terr := sh.TotalHeight()
	if terr != nil {
		return css
	}
	return css + rowGeometryCSS(loRow, hiRow, base, total, sh.Rows(), heights)
}

// rowGeometryCSS sets `--t` and `--hr` for the rows in a window whose geometry
// is not the uniform default.
//
// One rule per row, and it covers all three server-rendered element kinds at
// once: the gutter's number, the row strip, and the cells. Everything else in
// the stylesheet reads those two properties through a fallback, so a sheet with
// no resized row emits nothing here and lays out by multiplication exactly as
// before.
//
// It is O(rows in the buffer), not O(cells): a dense window has ten thousand
// cells and two hundred and fifty rows. The cell rules cannot be avoided by
// putting the numbers in the cells' style attributes, which would be the same
// two numbers repeated across every cell of the row.
//
// A resize shifts every row below it, so the rules start at the first row whose
// top has moved and run to the end of the window. baseTop is the window's own
// top, which absorbs every resize ABOVE the window in one number.
func rowGeometryCSS(loRow, hiRow, baseTop, totalH, rows int, heights map[int]int) string {
	if hiRow < loRow {
		return ""
	}
	var b strings.Builder
	// The scroll container's height, when the sheet is no longer rows x pitch.
	// It rides this stylesheet rather than a signal of its own because it moves
	// exactly when the per-row rules below do, and `#sy` is emitted after the
	// build's stylesheet so an equal-specificity rule here wins.
	if totalH > 0 && totalH != rows*rowHeightPx {
		px := strconv.Itoa(totalH) + "px"
		b.WriteString(`#` + gridID + `{height:` + px + `}`)
		b.WriteString(`#` + boxOverlayID + `,#pc{height:` + px + `}`)
	}
	top := baseTop
	for r := loRow; r <= hiRow; r++ {
		h := rowHeightPx
		if v, ok := heights[r]; ok && v > 0 {
			h = v
		}
		if top != r*rowHeightPx || h != rowHeightPx {
			d := rowVarDecl(strconv.Itoa(r))
			// Every element the SERVER positions by row index. Anything the
			// client places does the arithmetic itself (see cellBoxStyle):
			// a rule cannot reach it, because setProperty spells the same
			// declaration with a space and would not match these selectors.
			sel := `#` + gutterID + `>b[style="` + d + `"],` +
				`#` + bufferID + `>i.` + stripClass + `[style="` + d + `"],` +
				`#` + bufferID + ` b[style="` + d + `"],` +
				`#` + bufferID + ` b[style^="` + d + `;"],` +
				`#pc>div[style="` + d + `"],` +
				`#pc>div[style^="` + d + `;"]`
			b.WriteString(sel)
			b.WriteString(`{--t:`)
			b.WriteString(strconv.Itoa(top))
			b.WriteString(`px;--hr:`)
			b.WriteString(strconv.Itoa(h))
			b.WriteString(`px}`)
		}
		top += h
	}
	return b.String()
}

// heightSignal carries the resized rows to the client.
// sortedKeys is the row order both the seed and the push encode in. A Go map is
// not ordered and the client binary-searches the result.
func sortedKeys(m map[int]int) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// heightSignal is `_rhs`, not `_rh`: the row drag commits through `rh`, and two
// signals a character apart in the same expressions is a reading hazard for no
// gain.
const heightSignal = "_rhs"

// rowHeightPairs encodes a sparse height map as a flat, row-sorted array.
//
// Flat pairs rather than an object because the client binary-searches it, and an
// array of two-element arrays would be one allocation per resized row for the
// same information. Sorted because the search depends on it and a Go map is not.
func rowHeightPairs(heights map[int]int) string {
	if len(heights) == 0 {
		return "[]"
	}
	rows := sortedKeys(heights)
	var b strings.Builder
	b.Grow(len(rows) * 12)
	b.WriteByte('[')
	for i, r := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(r))
		b.WriteByte(',')
		b.WriteString(strconv.Itoa(heights[r]))
	}
	b.WriteByte(']')
	return b.String()
}

// cascadeCSS is sheetStyleCSS's body with the store taken out, so the three
// levels' interaction is testable without a sheet.
func cascadeCSS(rules []StyleRule, cols, rows map[int]int) string {
	if len(rules) == 0 {
		return ""
	}
	by := make(map[int]Style, len(rules))
	for _, r := range rules {
		by[r.ID] = r.Style
	}
	var b strings.Builder
	b.Grow(len(rules)*72 + (len(cols)+len(rows))*96)
	var done [MaxCols]bool

	// ── The column level, first, so everything else beats it ────────────────
	//
	// Columns sharing a look share a rule. A select-all resolves to 26 columns
	// carrying one style id (the store memoizes the merge per source style), so
	// grouping the selectors turns 26 copies of the same declarations into one,
	// and the declarations are the long half. The backdrops stay individual
	// because each one names its own grid track.
	for c := 0; c < MaxCols; c++ {
		id := cols[c]
		if id <= 0 || done[c] {
			continue
		}
		first := true
		for d := c; d < MaxCols; d++ {
			if cols[d] != id {
				continue
			}
			done[d] = true
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(`#` + bufferID + ` b[id^=`)
			b.WriteByte(byte('A' + d))
			b.WriteByte(']')
		}
		b.WriteByte('{')
		b.WriteString(ruleBody(by[id], false))
		b.WriteByte('}')
		for d := c; d < MaxCols; d++ {
			if cols[d] == id {
				colBackdropCSS(&b, d, by[id])
			}
		}
	}

	// ── The row level ───────────────────────────────────────────────────────
	//
	// Sorted, because a map is not, and two renders of an unchanged window must
	// produce identical bytes or the screen's stylesheet compare would patch on
	// every push forever.
	if len(rows) > 0 {
		keys := make([]int, 0, len(rows))
		for r := range rows {
			keys = append(keys, r)
		}
		sort.Ints(keys)
		for _, r := range keys {
			id := rows[r]
			if id <= 0 {
				continue
			}
			b.WriteString(rowLevelSelector(r))
			b.WriteByte('{')
			b.WriteString(ruleBody(by[id], true))
			b.WriteByte('}')
			rowBackdropCSS(&b, r, by[id])
		}
	}

	// ── The cell level, last, so a cell's own record wins ───────────────────
	for _, r := range rules {
		cls := StyleClass(r.ID)
		if cls == "" {
			continue
		}
		b.WriteString(`#` + bufferID + ` b.`)
		b.WriteString(cls)
		b.WriteByte('{')
		b.WriteString(ruleBody(r.Style, true))
		b.WriteByte('}')
	}
	return b.String()
}

// rowLevelSelector matches every cell of one display row, reading the row out of
// the `style` attribute writeCell already emits. It is the `[id^=D]` trick on the
// other axis: a cell's row is not in its id, but it is in `--r`, which every cell
// already carries because that is what positions it — so the row level costs no
// class, no attribute, not one byte per cell.
//
// Two spellings, because writeCell has two: `style="--r:12"` and, under
// SS_CELL_COL=1, `style="--r:12;--c:5"`. The semicolon in the prefix match is what
// stops `--r:1` matching row 12. It is generated from rowVarDecl, the function
// writeCell writes with, so selector and markup cannot drift apart.
func rowLevelSelector(row int) string {
	d := rowVarDecl(strconv.Itoa(row))
	return `#` + bufferID + ` b[style="` + d + `"],#` + bufferID + ` b[style^="` + d + `;"]`
}

// rowVarDecl is the `--r:N` declaration writeCell puts in a cell's style
// attribute. One function, so rowLevelSelector cannot fall out of step with it.
func rowVarDecl(r0 string) string { return `--r:` + r0 }

// colBackdropCSS paints a styled column's fill behind the whole column, including
// the rows that hold nothing. Without it, "make this column yellow" paints only
// the cells that exist: a column style deliberately materializes no cell — that is
// the entire saving — so the headline case would colour six rows and leave the
// rest white. It costs no markup, because `writeColRules` already emits one
// full-height `<i>` per column.
//
// The row level gets no equivalent: there is no per-row element inside `#b`, and
// inventing one would be markup per styled row on the scroll path. A row fill
// therefore paints the cells of that row and not the blanks between them.
func colBackdropCSS(b *strings.Builder, c int, st Style) {
	if st.BG == "" {
		return
	}
	// nth-of-type, not nth-child: the `<i>` runs are the first children of `#b`
	// today, and counting only among themselves keeps that from being a thing
	// anyone has to preserve. The first one covers the row-number gutter track,
	// so column c is the (c+2)th.
	b.WriteString(`#` + bufferID + `>i:nth-of-type(`)
	b.WriteString(strconv.Itoa(c + 2))
	b.WriteString(`){background:`)
	b.WriteString(st.BG)
	b.WriteByte('}')
}

// rowBackdropCSS paints a styled row's fill across the whole row, including the
// columns that hold nothing. It is colBackdropCSS one axis over, and it needed
// the row strip to exist: before that a row style could only be expressed as a
// selector over cells, so "make this row yellow" coloured the two cells that
// happened to hold a value and left the rest white.
//
// The strip is a later child of `#b` than the column backdrops, so a row fill
// paints over a column fill — which is the precedence the cascade already
// resolves for cells (cell beats row beats column).
func rowBackdropCSS(b *strings.Builder, row int, st Style) {
	if st.BG == "" {
		return
	}
	d := rowVarDecl(strconv.Itoa(row))
	sel := `#` + bufferID + `>i.` + stripClass
	b.WriteString(sel + `[style="` + d + `"],` + sel + `[style^="` + d + `;"]`)
	b.WriteString(`{background:`)
	b.WriteString(st.BG)
	b.WriteByte('}')
}

// styleSheetHTML wraps the rules in the element the shell carries and a push
// patches. It is emitted even when it is empty, because a patch needs something
// to morph over: a blank sheet's `<style id="sy"></style>` is 22 bytes written
// once per page load, and the alternative is an append-or-morph decision on the
// one path where getting it wrong means two stylesheets.
func styleSheetHTML(css string) string {
	return `<style id="` + styleSheetID + `">` + css + `</style>`
}

// selHTML is the linked region: one positioned box. A `class="s"` on every cell in
// the range is impossible rather than merely expensive under sparse rendering, and
// a box is correct over empty space, which is what a selection has to be. The
// vertical extent is rows (`--r`, `--n`) because a row is a constant height; the
// horizontal extent is a grid span, because a column is not.
func selHTML(sel selRange) string {
	if !sel.On {
		return ""
	}
	return `<div id="` + selID + `" style="--r:` + strconv.Itoa(sel.Lo.Row) +
		`;--n:` + strconv.Itoa(sel.Hi.Row-sel.Lo.Row+1) +
		`;grid-column:` + strconv.Itoa(sel.Lo.Col+2) + `/` + strconv.Itoa(sel.Hi.Col+3) + `"></div>`
}

// ─── Remove selectors ────────────────────────────────────────────────────────
//
// Datastar's remove mode runs `document.querySelectorAll(selector)` and removes
// every match, so one frame drops a whole band. Sparse rendering has no row
// element, so cells have to be named individually — which means the server has to
// know which ones exist. That is screen.held (http.go): a 26-bit column mask per
// buffer row, ~2 KB per screen. Without it the choice would be to name all 26
// columns of every dropped row, or to re-read the rows being thrown away, which
// is exactly the read the incremental path exists to avoid.

// rowNumSelector names the gutter numbers for the inclusive row range.
func rowNumSelector(loRow, hiRow int) string {
	if hiRow < loRow {
		return ""
	}
	var b strings.Builder
	b.Grow((hiRow - loRow + 1) * 8)
	for r := loRow; r <= hiRow; r++ {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString("#n")
		b.WriteString(strconv.Itoa(r + 1))
	}
	return b.String()
}

// appendSelector joins two selector fragments, either of which may be empty.
func appendSelector(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "," + b
	}
}

// cellIDs appends the A1 ids of the set bits of one row's column mask.
func cellIDs(b *strings.Builder, row int, mask uint32) {
	if mask == 0 {
		return
	}
	human := strconv.Itoa(row + 1)
	for c := 0; c < MaxCols; c++ {
		if mask&(1<<uint(c)) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('#')
		b.WriteByte(byte('A' + c))
		b.WriteString(human)
	}
}

// ─── Page shell ───────────────────────────────────────────────────────────────

// datastarBuild names the vendored artifact. It is provenance, not a URL —
// nothing fetches it, the page serves Datastar from this origin (assets.go).
// It is a custom build carrying only the plugins this application uses; the
// manifest beside it lists them and a test reads that manifest rather than
// trusting this comment. See vendorjs/README.md.
const datastarBuild = `datastar-1-0-2-4efd7a457ef99e73`

// Scroll anchoring is off on `#vp`. Chrome adjusts scrollTop to keep a chosen
// anchor node visually still when content above it changes, and a scroll patch
// changes content above the viewport by design. Every correction is drift, because
// the client's row arithmetic is `scrollTop / rowHeightPx` and nothing re-derives
// it — 40 PageDowns walked the selection 1,280 rows while the viewport followed
// ~1,047. Wheel scrolling hides it; keyboard navigation does not.

// gridCSS is the whole stylesheet: one sheet, classes only, with one exception.
// A cell carries `style="--r:250"`, which is data (where it is) and not
// presentation; a `style` attribute expressing appearance would be re-sent to
// every viewer on every full render of every band that cell lives in.
//
// Frozen headers are `position:sticky` rather than extra markup, so they cost
// zero bytes per row and nothing is duplicated anywhere.
//
// Grid lines are paint, not elements. The horizontal rules are one tiled
// gradient at the row pitch, exact by construction because the period is the row
// height. The vertical rules cannot be a gradient — columns are individually
// resizable, so the pitch is not constant — and are 26 elements placed by
// `grid-template-columns`, O(columns) rather than O(cells). Those 26 double as
// the client's column-geometry oracle, which is what makes hit-testing over
// empty space possible. See gridKeysScript.
//
// The sticky gutter's right-hand rule is a `box-shadow`, not a `border`: under
// `border-collapse:collapse` the borders belong to the table, so a sticky cell's
// border would stay behind while the cell moves. Collapse is kept because a row
// must stay exactly rowHeightPx tall — scrollTop/rowHeightPx is the client's row
// arithmetic, and a 23px row puts the buffer edge test silently out of step.
//
// The pending markers `b.q` (in flight) and `b.x` (refused) are inset shadows
// for the same reason: paint inside the existing padding, changing no box and no
// metric. A border would eat the content width under `box-sizing:border-box`, so
// every committed cell would re-wrap its text at the moment of commit.
var gridCSS = buildGridCSS()

func buildGridCSS() string {
	var b strings.Builder
	b.Grow(6144)
	b.WriteString(`:root{--rh:` + strconv.Itoa(rowHeightPx) + `px;--cw:` + strconv.Itoa(colWidthPx) +
		`px;--hw:` + strconv.Itoa(rowHeadPx) + `px;--ch:` + strconv.Itoa(colHeadPx) +
		`px;--tb:` + strconv.Itoa(toolbarPx) + `px;--fb:` + strconv.Itoa(formulaBarPx) +
		`px;--fh:` + strconv.Itoa(footHeightPx) +
		`px;--ln:#e1e3e1;--ln2:#c4c7c5;--hd:#f8f9fa;--bl:#1a73e8;` +
		// The row extent, as a default here and as the sheet's real value on `#vp`
		// (see vpInlineStyle). A sheet grows and shrinks, so this number is a
		// starting size and never a bound — but declaring it means a page whose
		// inline style was stripped still has a scroll container of some sensible
		// height rather than one of zero.
		`--rows:` + strconv.Itoa(DefaultRows) + `;`)
	// The per-column widths. They are custom properties so that a resize is one
	// number changing in one place, rather than an attribute on 13,000 cells.
	for c := 0; c < MaxCols; c++ {
		b.WriteString(colVar(c))
		b.WriteString(`:var(--cw);`)
	}
	b.WriteString(`}
*{box-sizing:border-box}
body{margin:0;background:#fff;color:#202124;font:13px/1 Arial,Helvetica,sans-serif;-webkit-font-smoothing:antialiased}
header{display:flex;gap:8px;align-items:center;height:var(--tb);padding:0 12px;border-bottom:1px solid var(--ln);background:#fff}
header b{font-size:15px;font-weight:500;letter-spacing:.1px}
header a.h{color:inherit;text-decoration:none}
header .m{color:#5f6368;font-family:ui-monospace,SFMono-Regular,monospace;font-size:12px}
/* Every variable-width readout may shrink; the controls may not. The header is
   one row and the aggregate line can be 300px of text, so without this the
   toolbar would be pushed off the end of a narrow window by a summary.
   IT COMES BEFORE the header .r rule, WHICH IS NOT A STYLE PREFERENCE: the two
   selectors have identical specificity, so the later one wins, and putting this
   after would set min-width:0 on the active-cell readout and collapse "B2" to
   "B". Measured in a screenshot before it was believed. */
header .m{min-width:0;overflow:hidden;white-space:nowrap;text-overflow:ellipsis}
header .r{color:#202124;font-weight:500;min-width:4.5rem}
header .sp{flex:1}
header .tb{display:flex;flex:0 0 auto;gap:2px;align-items:center;padding:0 6px;border-left:1px solid var(--ln);border-right:1px solid var(--ln)}
header .tb .tg{width:24px;height:24px;padding:0;border:0;border-radius:4px;background:none;color:#202124;font:13px/1 Arial,Helvetica,sans-serif;cursor:pointer}
header .tb .tg:hover{background:#f1f3f4}
/* The glyphs are real b/i elements so the button LOOKS like what it does. They
   have to opt out of the header b rule, which is the 15px semibold treatment
   the page title wears. */
header .tb .tg b{font-size:13px;font-weight:700;letter-spacing:0}
header .tb .tg i{font-size:13px}
/* The colour control is a native picker with a glyph in front of it. The input
   itself is squashed to a 20x14 well: browsers give type=color a chunky default
   box, and the affordance here is the swatch, not the widget. */
header .tb .cg{display:inline-flex;flex-direction:column;align-items:center;gap:1px;padding:0 2px;color:#444746;font-size:10px;line-height:1;cursor:pointer}
header .tb .cp{width:22px;height:12px;padding:0;border:1px solid #dadce0;border-radius:2px;background:none;cursor:pointer}
header .tb .sw{width:14px;height:14px;padding:0;border:1px solid rgba(0,0,0,.2);border-radius:2px;cursor:pointer}
/* "No fill" is a crossed-out box. An invisible button is not a button. */
header .tb .sw.n{background-image:linear-gradient(to top right,transparent 45%,#d93025 45%,#d93025 55%,transparent 55%)}
header .tb .fs{height:24px;max-width:7.5rem;border:1px solid #dadce0;border-radius:4px;background:#fff;color:#202124;font:12px/1 Arial,Helvetica,sans-serif}
.btn{display:inline-flex;align-items:center;white-space:nowrap;height:28px;padding:0 12px;border:1px solid #dadce0;border-radius:4px;background:#fff;color:var(--bl);font:500 13px/1 Arial,Helvetica,sans-serif;text-decoration:none;cursor:pointer}
.btn:hover{background:#f6fafe;border-color:#d2e3fc}
.btn.p{background:var(--bl);border-color:var(--bl);color:#fff}
.btn.p:hover{background:#1b66c9}
` + latencyChipCSS + formulaBarCSS + `#go{width:10rem;height:28px;padding:0 8px;border:1px solid #dadce0;border-radius:4px;background:#fff;color:#202124;font:12px/1 ui-monospace,SFMono-Regular,monospace}
#go:focus{outline:none;border-color:var(--bl);box-shadow:0 0 0 1px var(--bl)}
#vp{position:relative;overflow:auto;height:calc(100vh - var(--tb) - var(--fb));background:#fff;overscroll-behavior:none;overflow-anchor:none;--tw:calc(var(--hw)` + colWidthSum() + `)}
#cols{display:flex;position:sticky;top:0;z-index:6;width:var(--tw);height:var(--ch);background:var(--hd);border-bottom:1px solid var(--ln2);user-select:none}
#cols span{position:relative;flex:0 0 auto;width:var(--cw);height:var(--ch);line-height:var(--ch);text-align:center;color:#444746;font-weight:500;font-size:11px;box-shadow:inset -1px 0 0 var(--ln)}
#cols span:first-child{position:sticky;left:0;z-index:7;width:var(--hw);background:var(--hd)}
#cols span.a{background:#d3e3fd;color:#0b57d0}
#cols i{position:absolute;top:0;right:-2px;width:5px;height:100%;z-index:8;cursor:col-resize}
#cols i:hover,#cols i.d{background:var(--bl)}
#cols b{position:absolute;top:0;right:6px;height:100%;opacity:0;color:#444746;font-weight:400;cursor:pointer}
#cols span:hover b{opacity:.7}
#cols b:hover{opacity:1}
#` + gridID + `{position:relative;width:var(--tw);height:var(--th,calc(var(--rows) * var(--rh)))}
#` + gutterID + `{position:sticky;left:0;z-index:1;display:block;width:var(--hw);height:100%;background-color:var(--hd);box-shadow:inset -1px 0 0 var(--ln2);user-select:none}
#` + gutterID + `>b{position:absolute;left:0;top:var(--t,calc(var(--r) * var(--rh)));width:var(--hw);height:var(--hr,var(--rh));line-height:calc(var(--rh) - 1px);color:#444746;font-weight:400;font-size:11px;text-align:center;border-bottom:1px solid var(--ln)}
#` + gutterID + `>b.a{background:#d3e3fd;color:#0b57d0}
#` + gutterID + `>b::after,#` + gutterID + `>b::before{content:"";position:absolute;left:0;right:0;height:` + strconv.Itoa(rowGripPx) + `px;cursor:row-resize}
#` + gutterID + `>b::after{bottom:0}
#` + gutterID + `>b::before{top:0}
#` + bufferID + `{position:absolute;top:0;left:0;width:var(--tw);height:100%;display:grid;grid-template-columns:` + gridTracks() + `;grid-template-rows:100%;user-select:none}
#` + bufferID + `>i.` + stripClass + `{position:absolute;left:0;right:0;top:var(--t,calc(var(--r) * var(--rh)));height:var(--hr,var(--rh));border-bottom:1px solid var(--ln);pointer-events:none}
#` + bufferID + `>i{grid-row:1;box-shadow:inset -1px 0 0 var(--ln);pointer-events:none}
#` + bufferID + ` b{position:absolute;left:0;right:1px;top:var(--t,calc(var(--r) * var(--rh)));height:calc(var(--hr,var(--rh)) - 1px);padding:0 4px;overflow:hidden;white-space:nowrap;font-weight:400;line-height:calc(var(--rh) - 1px);text-align:right;font-variant-numeric:tabular-nums}
#` + bufferID + ` b.t{text-align:left}
#` + bufferID + ` b.f{color:#0b57d0}
#` + bufferID + ` b.e{color:#c5221f;background:#fce8e6;text-align:left}
#` + bufferID + ` b.` + measureClass + `{height:auto}
#` + bufferID + ` b.` + pendClass + `{color:#5f6368;font-style:italic;text-align:left}
#` + bufferID + ` b.` + flightClass + `{box-shadow:inset 2px 0 0 #f9ab00}
#` + bufferID + ` b.` + failClass + `{box-shadow:inset 2px 0 0 #d93025}
#` + bufferID + `>.` + groupClass + `{position:absolute;left:0;right:0;top:calc(var(--r) * var(--rh));height:calc(var(--rh) - 1px);display:grid;grid-template-columns:` + gridTracks() + `}
#` + bufferID + `>.` + groupClass + `>b{position:relative;left:auto;right:auto;top:auto;height:100%;margin-right:1px}
#` + selID + `{position:absolute;z-index:2;left:0;right:1px;top:var(--t,calc(var(--r) * var(--rh)));height:calc(var(--sh,calc(var(--n) * var(--rh))) - 1px);background:rgba(26,115,232,.14);box-shadow:inset 0 0 0 1px rgba(26,115,232,.45);pointer-events:none}
/* THE THREE OVERLAY GRIDS. One rule, one copy of the column tracks, and every
   positioned overlay on the page is a child of one of them — the editor and the
   active-cell outline (#oc, inside #g), the selection and the copy marquee (#ov,
   in the shell) and the collaborators' cursors (#pc, in the shell). See
   overlayGridCSS's note for why they are a grid rather than pixels. */
#` + cellOverlayID + `,#` + boxOverlayID + `,#pc{position:absolute;left:0;width:var(--tw);display:grid;grid-template-columns:` + gridTracks() + `;grid-template-rows:100%;pointer-events:none}
#` + cellOverlayID + `{top:0;height:100%}
#` + boxOverlayID + `,#pc{top:var(--ch);z-index:2;height:var(--th,calc(var(--rows) * var(--rh)))}
#` + cellOverlayID + `>*,#` + boxOverlayID + `>*,#pc>div{position:absolute;left:0;right:1px;top:var(--t,calc(var(--r) * var(--rh)))}
#` + selBoxID + `{height:calc(var(--sh,calc(var(--n) * var(--rh))) - 1px);background:rgba(26,115,232,.14);box-shadow:inset 0 0 0 1px rgba(26,115,232,.45)}
#` + copyBoxID + `{height:calc(var(--sh,calc(var(--n) * var(--rh))) - 1px);outline:2px dashed var(--bl);outline-offset:-2px}
#` + editorID + `{left:-1px;right:auto;width:calc(100% + 2px);top:calc(var(--t,calc(var(--r) * var(--rh))) - 1px);z-index:4;field-sizing:content;height:auto;min-height:calc(var(--hr,var(--rh)) + 2px);max-height:` + strconv.Itoa(MaxRowHeight) + `px;margin:0;padding:0 3px;border:2px solid var(--bl);border-radius:0;background:#fff;color:#202124;font:13px/` + strconv.Itoa(rowHeightPx-1) + `px Arial,Helvetica,sans-serif;text-align:right;outline:none;box-shadow:0 1px 3px rgba(60,64,67,.3);pointer-events:auto;user-select:text;resize:none;overflow:auto;white-space:pre}
#` + activeCellID + `{left:-1px;right:auto;width:calc(100% + 2px);top:calc(var(--t,calc(var(--r) * var(--rh))) - 1px);z-index:3;height:calc(var(--hr,var(--rh)) + 2px);border:2px solid var(--bl)}
#mn{position:fixed;z-index:9;min-width:11rem;padding:6px 0;border:1px solid #dadce0;border-radius:4px;background:#fff;box-shadow:0 2px 6px 2px rgba(60,64,67,.15);font-size:13px}
#mn button{display:block;width:100%;padding:7px 14px;border:0;background:none;color:#202124;font:13px/1 Arial,Helvetica,sans-serif;text-align:left;cursor:pointer}
#mn button:hover{background:#f1f3f4}
#mn hr{margin:6px 0;border:0;border-top:1px solid var(--ln)}
#` + guideID + `{position:fixed;z-index:9;display:none;width:2px;background:var(--bl);pointer-events:none;will-change:transform}
#rt{flex:0 0 auto;margin-right:14px;color:#80868b;font-size:11px;white-space:nowrap;cursor:help}
header .ro{flex:0 0 auto;overflow:visible;color:#b06000;border:1px solid #e0c088;border-radius:3px;padding:1px 6px;font-size:11px;letter-spacing:.04em;text-transform:uppercase;cursor:help}
#bz{position:fixed;left:50%;bottom:24px;transform:translateX(-50%);z-index:10;padding:10px 16px;border-radius:4px;background:#3c4043;color:#fff;font-size:13px;box-shadow:0 1px 3px rgba(60,64,67,.4);opacity:0;visibility:hidden;animation:bzin 1ms linear 200ms forwards}
@keyframes bzin{to{opacity:1;visibility:visible}}
/* Presence. Every rule is O(config): colour from one custom property the
   server sets per element (--h, a hue), geometry from --r/--n and a
   grid-column. A cell carries none of it. See presenceui.go. */
header .pl{display:flex;flex:0 0 auto;gap:3px;align-items:center}
header .pl i{display:inline-flex;align-items:center;justify-content:center;width:22px;height:22px;border-radius:50%;background:hsl(var(--h) 68% 45%);color:#fff;font:700 10px/1 Arial,Helvetica,sans-serif;font-style:normal;letter-spacing:.2px;cursor:default}
header .pl i.me{box-shadow:0 0 0 2px #fff,0 0 0 3px hsl(var(--h) 68% 45%)}
#pc>.pu{height:calc(var(--hr,var(--rh)) - 1px);box-shadow:inset 0 0 0 2px hsl(var(--h) 68% 45%)}
#pc>.pu>b{position:absolute;left:-2px;bottom:100%;margin-bottom:1px;padding:1px 5px;border-radius:3px;background:hsl(var(--h) 68% 45%);color:#fff;font:500 10px/13px Arial,Helvetica,sans-serif;white-space:nowrap}
#pc>.pb{height:calc(var(--sh,calc(var(--n) * var(--rh))) - 1px);background:hsl(var(--h) 68% 45% / .12);box-shadow:inset 0 0 0 1px hsl(var(--h) 68% 45% / .45)}
#pc>.pf{height:calc(var(--hr,var(--rh)) - 1px);background:hsl(var(--h) 68% 45% / .38);box-shadow:inset 0 0 0 1px hsl(var(--h) 68% 45% / .6);animation:ssfl ` + strconv.Itoa(int(attributionTTL/time.Millisecond)) + `ms linear var(--d) forwards}
@keyframes ssfl{from{opacity:1}to{opacity:0}}
` + growRowsCSS)
	// The frozen header strip is a flexbox, so it still needs one width rule per
	// column. The cells do not: `grid-template-columns` on `#b` sizes every
	// column once, and the rules below place a cell in one of those tracks from
	// its own id.
	for c := 0; c < MaxCols; c++ {
		n := strconv.Itoa(c + 2)
		b.WriteString(`#cols span:nth-child(` + n + `){width:var(` + colVar(c) + `)}`)
		b.WriteByte('\n')
	}
	b.WriteString(colPlacementCSS())
	return b.String()
}

// gridTracks is `grid-template-columns` for `#b`: the row-number gutter, then
// the 26 column-width custom properties. One declaration, 26 numbers, and every
// cell in the buffer is sized and positioned by it.
func gridTracks() string {
	var b strings.Builder
	b.Grow(MaxCols*16 + 16)
	b.WriteString("var(--hw)")
	for c := 0; c < MaxCols; c++ {
		b.WriteString(" var(")
		b.WriteString(colVar(c))
		b.WriteByte(')')
	}
	return b.String()
}

// colPlacementCSS keeps a cell's column out of its markup. A cell id is A1
// notation, so the column letter is a prefix and an attribute selector reads it:
// `#b [id^=D]{grid-column:5}`. 26 rules generated once, against a `--c` custom
// property that would otherwise be repeated on every cell of every window. It is
// a descendant selector rather than a child selector so that row-group rendering
// places its cells the same way. Nothing else on the page has an id starting with
// an upper-case letter, so the rules cannot reach anything but a cell.

// cellColMode is the control for the attribute-selector trick: with it set, every
// cell carries `--c` and the 26 rules collapse to one. Set SS_CELL_COL=1.
var cellColMode = os.Getenv("SS_CELL_COL") == "1"

func colPlacementCSS() string {
	if cellColMode {
		// Both lines, for the same reason the generated rules give both: an
		// abspos grid child with `grid-column-end:auto` stretches to the
		// container's padding edge.
		return `#` + bufferID + ` b{grid-column:var(--c)/calc(var(--c) + 1)}`
	}
	var b strings.Builder
	b.Grow(MaxCols * 26)
	for c := 0; c < MaxCols; c++ {
		b.WriteString(`#` + bufferID + ` [id^=`)
		b.WriteByte(byte('A' + c))
		// `grid-column:N`, without the end line, sets grid-column-end to `auto` —
		// and for an absolutely positioned grid child `auto` means the grid
		// container's padding edge, not the next line, so every cell would start at
		// the right x and then run to the end of the sheet. Both lines, always.
		b.WriteString(`]{grid-column:`)
		b.WriteString(strconv.Itoa(c + 2))
		b.WriteByte('/')
		b.WriteString(strconv.Itoa(c + 3))
		b.WriteString(`}`)
	}
	return b.String()
}

// colVar is the custom property that carries column c's width. `--w-0`, not
// `--w0`: Datastar's data-style kebab-cases its object keys
// (`.replace(/([a-z])([0-9]+)/gi,"$1-$2")`), so `--w0` written in an expression
// would reach the DOM as `--w-0` anyway. Naming them that way in the first place
// means the stylesheet and the binding agree instead of agreeing by accident.
func colVar(c int) string { return "--w-" + strconv.Itoa(c) }

// colWidthSum is the `+ var(--w-0) + var(--w-1) …` tail of the grid's total
// width, shared by the table and the column header strip so the two cannot
// drift apart when a column is resized.
//
// It is declared on `#vp` and not on `:root`, because a custom property is
// inherited as its computed value: `--tw` on `:root` would already have
// substituted `:root`'s `--w-*` and would freeze at 26 default columns while the
// overrides on `#vp` moved underneath it, putting the header's letters over the
// wrong columns at full horizontal scroll.
func colWidthSum() string {
	var b strings.Builder
	b.Grow(MaxCols * 16)
	for c := 0; c < MaxCols; c++ {
		b.WriteString(" + var(" + colVar(c) + ")")
	}
	return b.String()
}

// colHeaderHTML is the frozen A/B/C… strip. It is emitted once, in the page
// shell, and never patched: the letters do not change and the widths reach it
// through the same custom properties the cells use.
//
// The first span is the corner over the row-number gutter. It is sticky in both
// axes so it covers the row numbers as they scroll under it.
func colHeaderHTML(sheetID string) string {
	var b strings.Builder
	b.Grow(MaxCols*48 + 512)
	b.WriteString(`<div id="cols" data-on:pointerdown="` + rzDownExpr +
		`" data-on:pointermove="` + rzMoveExpr +
		`" data-on:pointerup="` + rzUpExpr(sheetID) +
		`" data-on:pointercancel="` + rzCancelExpr +
		`" data-on:click="` + menuCaretExpr + `"><span></span>`)
	for c := 0; c < MaxCols; c++ {
		b.WriteString(`<span data-c="`)
		b.WriteString(strconv.Itoa(c))
		b.WriteString(`">`)
		b.WriteByte(byte('A' + c))
		// The caret (insert/delete menu) and the drag handle (resize). One of
		// each per column, in the shell, never patched — 52 elements total
		// against the 13,000 a per-cell affordance would cost.
		b.WriteString(`<b class="cr">▾</b><i></i></span>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// ─── Column resize ───────────────────────────────────────────────────────────
//
// The drag issues no requests and no layout. No layout is why it writes no width
// at all: `$rw` would reach `--w-N`, a track of `grid-template-columns` on `#b`,
// and changing a grid track forces full layout of every grid item in the buffer,
// ~13,000 of them, per frame. So the drag moves a `position:fixed` guide line by
// `transform`, which the compositor handles without layout or paint, and applies
// the real width once on release.
//
// `$rc` stays -1 for the whole drag, and that is the mechanism: colWidthStyleExpr
// reads `$rc===k ? $rw : $_wk`, so at -1 every column keeps the server's width
// and the data-style effect does not run at all. The pair is set for the first
// time on pointerup, immediately before the POST. The server clears it again —
// its push patches `{_wk: px, rc: -1}` in one frame, so the override and the
// authoritative value swap atomically and the column never flashes back.
//
// T.gdShow takes an axis because a variable row height has the identical problem
// one axis over: it feeds `grid-template-rows`/`top`.
const rzDownExpr = `const t=evt.target;if(t.tagName!=='I')return;evt.preventDefault();` +
	`const s=t.parentElement;` +
	`if(window.__ss)window.__ss.rzStart(+s.dataset.c,evt.clientX,s.offsetWidth);` +
	`el.setPointerCapture(evt.pointerId)`

// THE ROW GESTURE LIVES ON `#vp`, WHICH IS PAGE SHELL. It began on the gutter,
// which reads better and is wrong twice over: `#rn` is inside `#g`, and `#g` is
// what every push re-renders. So the element holding a live drag is morphed
// underneath it — and, the sharper half, the element issuing the commit is one a
// push can replace, which aborts its in-flight request. A resize that lands
// while anyone else is typing is dropped, with nothing in the log to say so.
// That is this codebase's oldest Datastar hazard, the one the editor is shaped
// around, and this walked straight into it.
//
// On `#vp` there is no second element to race and nothing to stop propagating:
// one handler decides between resizing and selecting, in that order, and the
// release is already on the window so a drag that ends anywhere still ends.
//
// It writes no optimistic height, unlike the column drag. A row's geometry lives
// in the stylesheet the server derives per window, so painting the new height
// before the server answers would mean the client generating that stylesheet
// too — two sources for one set of rules. The guide line is the feedback during
// the gesture; the row moves when the push lands.
const rowResizeDownExpr = `if(window.__ss&&window.__ss.rzDownR(evt))return;`

func rowResizeUpExpr(sheetID string) string {
	return `if(window.__ss){const v=window.__ss.rzEndR(evt.clientY);if(v){` +
		`$rr=v.r;$rh=v.h;` +
		`@post('/s/` + sheetID + `/rowheight',{requestCancellation:'disabled'})}}`
}

// Fit to contents, on the same band the drag uses and by the convention every
// spreadsheet has: double-click the edge. It commits through the resize endpoint
// because a fitted height is a height — the store has no notion of "automatic",
// so a fitted row stays where it was put until something fits it again.
func rowFitExpr(sheetID string) string {
	return `if(window.__ss){const rw=window.__ss.gripAt(evt);if(rw>=0){evt.preventDefault();` +
		`$rr=rw;$rh=window.__ss.fitR(rw);` +
		`@post('/s/` + sheetID + `/rowheight',{requestCancellation:'disabled'});return}}`
}

// rowGripPx is the reach of the resize band on EACH side of a row's bottom edge.
// Straddling it is the difference between a target you aim at and one you hit:
// the edge is what the eye sees, and a band reaching only upwards leaves half the
// pixels around that line belonging to the row below — where the same press
// means "select this row" instead.
const rowGripPx = 4

// rzMoveExpr writes no signal. It moves one element's transform, which is what
// keeps the drag cheap on a dense buffer.
const rzMoveExpr = `if(window.__ss)window.__ss.rzMove(evt.clientX)`

// rzUpExpr commits, and it is the only moment in the gesture that touches a
// signal or the network.
//
// IT WRITES THE COLUMN'S OWN WIDTH SIGNAL, and the switch that does it is the
// point rather than clumsiness. The optimistic width used to live in `$rc`/`$rw`
// — one slot shared by all 26 columns — and `--w-N` read it only while `$rc`
// still pointed at N. So a resized column held its new width exactly until the
// next column was dragged, and then fell back to whatever the server had last
// said. Any commit whose confirming push was lost, refused, or simply slower
// than the next drag left an earlier column snapping to a stale width, with
// nothing on screen connecting the two events. Writing `$_wN` directly makes the
// optimistic value per column and durable; the server's push then confirms the
// same number instead of being the only thing holding it up.
//
// `$rc`/`$rw` remain, as the payload this POST carries and nothing else. It posts from `#cols`, which lives in the page shell
// and is never inside a patch region — the same rule `#nav` follows: a fetch
// action's request cancellation is keyed on the element, so issuing it from
// anything the stream can remove or replace is how you abort your own SSE.
//
// IT OPTS OUT OF CANCELLATION, for the reason the clear, fill, paste and style
// commands do. Every resize posts from this one element, so under the default
// `auto` a second resize aborts the first — and resizing two columns is two
// operations, not a correction of one. The abort is invisible at the moment it
// happens and shows up later: the abandoned column keeps its optimistic width
// only while `$rc` still points at it, so it snaps back to its stale stored
// width as soon as another column is dragged. Reported as "I resize the sixth
// one and one of the earlier ones changes size".
func rzUpExpr(sheetID string) string {
	var set strings.Builder
	set.WriteString(`switch(v.c){`)
	for c := 0; c < MaxCols; c++ {
		i := strconv.Itoa(c)
		set.WriteString(`case ` + i + `:$_w` + i + `=v.w;break;`)
	}
	set.WriteByte('}')
	return `if(!window.__ss)return;const v=window.__ss.rzEnd(evt.clientX);if(!v)return;` +
		set.String() + `$rc=v.c;$rw=v.w;` +
		`@post('/s/` + sheetID + `/colwidth',{requestCancellation:'disabled'})`
}

const rzCancelExpr = `if(window.__ss)window.__ss.rzCancel();$rc=-1`

// ─── The pending chip's backstop ─────────────────────────────────────────────
//
// A slow write that is refused must not leave a spinner up for twenty seconds.
//
// `$p` goes true on the click and the server lowers it, on the push that carries
// the result or on the notice a refusal sends (structure.go's notify). The
// response is the wrong thing to trust: it returns before anything is on screen.
//
// The hole is every refusal that happens before the handler knows which screen
// asked — an unreadable body, an invalid sheet id, an index out of range, a rate
// limit, a 500 from anywhere. Those answer 4xx/5xx and send no notice, because
// there is no connection to send one to.
//
// So the client closes it from `datastar-fetch`, which Datastar dispatches on
// `document` for any response >= 400 and for a transport failure, with `el` set
// to the issuing element. The test is a class rather than a list of ids, so the
// backstop can only fire for an element that actually raised the chip. `$note` is
// written only when still empty, so a refusal that did reach a handler keeps the
// specific words.
const (
	failEvent      = "ssfail"
	pendingWriteCl = "pw"
)

// pendingRaise is the four signal writes every slow command makes before it
// posts: raise the flag, name the work, clear any stale refusal, arm the safety
// net.
//
// The label is a parameter because the chip has to name the work: one hardcoded
// sentence would be worn by every write that raises `$p`, so a column alignment
// would announce itself as "Reshaping the sheet…" and teach the reader that
// formatting is expensive. The 200ms reveal delay that makes this bearable is in
// CSS (`#bz`), because it is a property of the chip rather than of any command.
func pendingRaise(label string) string {
	return `$p=true;$_pt='` + label + `';$note='';setTimeout(()=>{$p=false},20000);`
}

func chipFailScript() string {
	return `document.addEventListener('datastar-fetch',function(v){var d=v.detail;
 if(!d||d.type!=='error'||!d.el||!d.el.classList)return;
 if(!d.el.classList.contains('` + pendingWriteCl + `'))return;
 window.dispatchEvent(new Event('` + failEvent + `'));});
`
}

// chipFailHTML is the element that turns that event into the two signal writes.
// It issues no request, so it may have an element of its own without joining the
// `#cl`/`#fl`/`#pv`/`#st` cancellation argument — the same exemption `#ex` and
// `#bf` have.
func chipFailHTML() string {
	return `<div id="er" hidden data-on:` + failEvent + `__window="` +
		`$p=false;if($note==='')$note=$_ro?'This sheet is read-only — make your own from the front page.':'That command was refused.'"></div>`
}

// guideID is the drag guide: one 2px line, `position:fixed`, moved by transform.
// It is page-shell markup like the other overlays, so no push re-sends it and no
// morph removes it. Fixed rather than absolute inside `#vp` so it needs no scroll
// correction: a fixed element is the one thing a compositor can move without
// consulting the document at all.
const guideID = "gl"

// colWidthStyleExpr binds all 26 column widths to `#vp` in one data-style
// attribute. `#vp` is page-shell markup: written once per page load, never re-sent
// by a push, and custom properties inherit into every cell for free. A width
// attribute per cell would put 13,000 copies of a 26-number fact into every full
// render.
//
// It also carries `--rows`, so a sheet that grew from 1,000 to 15,000 rows is one
// signal patch away from a correctly sized scroll container. See patchExtent.
func colWidthStyleExpr() string {
	var b strings.Builder
	b.Grow(MaxCols*40 + 24)
	b.WriteByte('{')
	for c := 0; c < MaxCols; c++ {
		if c > 0 {
			b.WriteByte(',')
		}
		i := strconv.Itoa(c)
		b.WriteString(`'` + colVar(c) + `':$_w` + i + `+'px'`)
	}
	// Stringified because setProperty takes a string and a bare number would
	// reach CSS as a value the `calc()` above cannot use.
	b.WriteString(`,'--rows':''+$_rows`)
	b.WriteByte('}')
	return b.String()
}

// colWidthSignals declares the server-owned width signals. They are
// underscore-prefixed, which in Datastar means local: the server can patch them
// down, and the client never ships them back up. That matters because Datastar
// sends every ordinary signal with every request, and 26 numbers riding on
// every scroll command would be a real cost for a fact the server already knows.
func colWidthSignals(widths map[int]int) string {
	var b strings.Builder
	b.Grow(MaxCols * 10)
	for c := 0; c < MaxCols; c++ {
		b.WriteString(`,_w`)
		b.WriteString(strconv.Itoa(c))
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(colWidthOf(widths, c)))
	}
	return b.String()
}

// vpInlineStyle is the pre-Datastar first paint: the sheet's row extent and any
// non-default column widths, inline on `#vp`, so the server-rendered HTML is
// already correct before the bundle has been fetched.
//
// `--rows` is always emitted, unlike the widths, because the anchor script seeks
// by assigning scrollTop synchronously in the same parse — and scrollTop only goes
// where the container is tall enough to allow.
func vpInlineStyle(widths map[int]int, rows int) string {
	var b strings.Builder
	b.Grow(MaxCols*12 + 24)
	b.WriteString("--rows:")
	b.WriteString(strconv.Itoa(rows))
	b.WriteByte(';')
	for c := 0; c < MaxCols; c++ {
		w, ok := widths[c]
		if !ok || w == colWidthPx {
			continue
		}
		b.WriteString(colVar(c))
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(ClampColWidth(w)))
		b.WriteString("px;")
	}
	return ` style="` + b.String() + `"`
}

func colWidthOf(widths map[int]int, c int) int {
	if w, ok := widths[c]; ok {
		return ClampColWidth(w)
	}
	return colWidthPx
}

// pageShell is the full HTML document: the stylesheet, the static column header,
// the scroll container with the initial buffer already rendered, and a separate
// anchor that opens the SSE stream.
//
// The SSE anchor is outside the patch region on purpose. Removing an element
// aborts its in-flight request, and the live stream is the longest in-flight
// request on the page; hanging `data-init` off `#g` would put the connection one
// bad morph away from being torn down by its own payload.
//
// `conn` is a server-issued handle for this screen, so a viewport command can
// name the connection it moves. `at` (anchor.go) is an ordinary signal rather
// than baked into the markup, because a jump to a new region has to reach the
// server and the signals riding the next command are the only channel there is.

// pageShell renders the shell with default column widths and a default-sized
// sheet, for callers that do not care about shared sheet geometry.
func pageShell(sheetID string, loRow, hiRow int, grid string, at anchor) string {
	return pageShellWidths(sheetID, loRow, hiRow, grid, at, nil, DefaultRows, "", nil)
}

// styleCSS is the sheet's own stylesheet, from sh.StyleRules(). It rides in the
// shell for the same reason the column widths do: it is O(config), it changes
// only when a style is created or collected, and a push then patches ~50 bytes
// instead of re-rendering a grid to change a colour.
func pageShellWidths(sheetID string, loRow, hiRow int, grid string, at anchor, widths map[int]int, rows int, styleCSS string, heights map[int]int) string {
	esc := html.EscapeString(sheetID)
	var b strings.Builder
	b.Grow(len(grid) + len(gridCSS) + 3072)
	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>sheetstream · `)
	b.WriteString(esc)
	// An inline data: icon, so no browser ever asks for /favicon.ico and gets a
	// 404 in the console of a page whose whole point is being inspected.
	b.WriteString(`</title><link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Crect width='16' height='16' fill='%23fff'/%3E%3Cpath d='M0 5h16M0 10h16M5 0v16M10 0v16' stroke='%23cdd5e2'/%3E%3Crect x='5' y='5' width='5' height='5' fill='%233b5e8b'/%3E%3C/svg%3E"><style>`)
	b.WriteString(gridCSS)
	b.WriteString(`</style>`)
	// The sheet's stylesheet is a second element, after the first, and both facts
	// matter. Second, because CSS breaks specificity ties by source order and
	// these rules have to beat `#b b.t`/`#b b.f` at equal specificity. An element
	// of its own, because it is the only part of the stylesheet that is per-sheet
	// rather than per-build, so it is the only part a push ever has to replace —
	// see patchStyles.
	b.WriteString(styleSheetHTML(styleCSS))
	// The page's own JavaScript, as one hashed immutable file rather than inline
	// blocks in the body: 7,553 brotli bytes, 44% of the page, that do not ride every
	// navigation. `defer` so it does not block the parse, and before the Datastar
	// module so `window.__ss` is complete before the first expression is evaluated —
	// deferred classic scripts run after parsing and ahead of module scripts.
	b.WriteString(`<script defer src="`)
	b.WriteString(appJSPath())
	b.WriteString(`"></script>`)
	b.WriteString(`<script type="module" src="`)
	b.WriteString(datastarPath())
	// `row`/`col` are seeded from the anchor; `ref` deliberately is not. It is
	// resolved by `#vp`'s data-init from the cell's own geometry, which may have
	// been resized, and the server would have to duplicate that arithmetic to get
	// it right. A sheet opens with a cell selected, the way a spreadsheet does —
	// otherwise the keyboard is inert until the reader happens to click something.
	b.WriteString(`"></script></head><body data-signals="{conn:'',ref:'',raw:'',row:` +
		strconv.Itoa(at.Ref.Row) + `,col:` + strconv.Itoa(at.Ref.Col) +
		`,editing:false,lo:`)
	b.WriteString(strconv.Itoa(loRow))
	b.WriteString(`,hi:`)
	b.WriteString(strconv.Itoa(hiRow))
	// blo/bhi are the server's buffer, patched on every window change; lo/hi
	// above are the client's requested viewport. They start equal because the
	// first paint is the first buffer, and they diverge from the first scroll.
	b.WriteString(`,blo:`)
	b.WriteString(strconv.Itoa(loRow))
	b.WriteString(`,bhi:`)
	b.WriteString(strconv.Itoa(hiRow))
	// The `t*` signals are instrumentation, not application state: they carry the
	// browser's own performance.now() measurements to the server on the next
	// viewport command. They are ordinary (non-underscore) signals precisely
	// because Datastar ships those with every request — that is the transport,
	// and it costs no extra round trip. See otel.go.
	b.WriteString(`,tScroll:0,tPost:0,tQuiet:0,tWait:0,tMorph:0,tRender:0,tBytes:0,tScrolls:0,tSkips:0`)
	// The linked region, as the URL asked for it. A1 notation only, so it needs
	// no escaping beyond the shell's own — but it goes through EscapeString
	// anyway, because "the parser rejected everything dangerous" is a property
	// of today's ParseRef and not a promise.
	b.WriteString(`,at:'`)
	b.WriteString(html.EscapeString(at.Raw))
	b.WriteByte('\'')
	// The range selection, as four local signals. Underscore-prefixed means
	// Datastar never sends them to the server, which is what "selection is client
	// state" costs on the wire: nothing, on every request, forever. They are
	// seeded from the same anchor the server rendered `#sl` with, so a link to a
	// range is already the client's range before the reader can touch anything.
	// See keys.go.
	selLo, selHi := selSeed(at)
	b.WriteString(`,_sar:` + strconv.Itoa(selLo.Row) + `,_sac:` + strconv.Itoa(selLo.Col) +
		`,_sfr:` + strconv.Itoa(selHi.Row) + `,_sfc:` + strconv.Itoa(selHi.Col))
	// `_ag` is the aggregate line the server computed for the current selection: a
	// server answer, patched down and never sent back up. `_cp` is the clipboard —
	// the source range a Ctrl+C recorded, in A1 notation — which is client state
	// that reaches the server exactly once, as an explicit payload on the paste
	// that uses it.
	b.WriteString(`,_ag:'',_cp:''`)
	// The column-resize pair. `rc` is the column being dragged (-1 when none)
	// and `rw` its live width; they are ordinary signals because the commit POST
	// has to carry them to the server. The 26 `_w` signals it also writes are
	// local (see colWidthSignals).
	b.WriteString(`,rc:-1,rw:0,rr:-1,rh:0`)
	// The sheet's allocated row extent — how tall the scroll container is, not
	// how many rows hold data (see (*Sheet).UsedRows for the other one). It is
	// underscore-prefixed for the same reason the widths are: it is shared sheet
	// state the server owns and patches down, and shipping it back up on every
	// scroll command would be paying for a fact the server already knows.
	b.WriteString(`,_rows:`)
	b.WriteString(strconv.Itoa(rows))
	// The resized rows, as flat [row,height,…] pairs. Local for the reason
	// `_rows` is: the server owns it. It is the whole sheet rather than the
	// window because the client needs an offset for rows it cannot see — a
	// scroll has to know which row it is landing on before that row is fetched.
	b.WriteString(`,` + heightSignal + `:`)
	b.WriteString(rowHeightPairs(heights))
	// The header menu: open, kind ('r'/'c'), index, position, chosen operation,
	// and `p` — the pending flag the server clears when the reshaped grid
	// actually arrives.
	b.WriteString(`,mo:false,mk:'',mi:0,mx:0,my:0,mop:'',p:false,note:'',_pt:''`)
	// `_ro` is this sheet's read-only bit. Local: the server decided it and
	// re-reads it from configuration on every request, so shipping it back
	// up would be the client telling the server its own policy — and a
	// client that could assert it could also clear it. The enforcement is in
	// the middleware (readonly.go); this only stops the page offering an
	// edit it knows will be refused.
	b.WriteString(`,_ro:`)
	b.WriteString(strconv.FormatBool(isReadOnly(sheetID)))
	// The server's own p50 command time, seeded so the latency chip is right at
	// first paint instead of reading `server —` until the first push corrects
	// it. Local (underscore-prefixed) for the reason the 26 widths and `_rows`
	// are: the server owns it, patches it down, and must not be told it back on
	// every request the page makes. See latency.go.
	b.WriteString(latencySignalSeed())
	// How many rows the foot control will add. Local, so it never rides a
	// request; it reaches the server as an explicit payload on the one command
	// that needs it. See growrows.go.
	b.WriteString(growRowsSignalSeed())
	b.WriteString(colWidthSignals(widths))
	b.WriteString(`}">`)

	b.WriteString(`<header><b><a class="h" href="/">sheetstream</a></b><span class="m">`)
	b.WriteString(esc)
	b.WriteString(`</span>`)
	// Says the sheet is fixed before anything is refused. See readonly.go.
	b.WriteString(readOnlyBadgeHTML(sheetID))
	b.WriteString(`<span class="m r" data-text="$ref||'—'"></span>`)
	// The styling toolbar goes here — immediately after the active-cell readout
	// and ahead of everything variable-width. The aggregate line can be 300px of
	// text and the header is one row, so putting the controls before it means the
	// thing squeezed when a wide selection is summarised is the summary, not the
	// buttons.
	b.WriteString(styleToolbarHTML())
	// The selection size, Sheets' "4R × 3C". It is one span bound to the four
	// local signals, so it costs one element in the shell and nothing per render
	// — and it is empty (and therefore invisible) whenever the selection is a
	// single cell, which is most of the time.
	b.WriteString(`<span class="m s" data-text="window.__ss?window.__ss.selSize($_sar,$_sac,$_sfr,$_sfc):''"></span>`)
	// The aggregate line — Sum / Avg / Count / Min / Max for the selected range.
	// One span in the shell bound to one local signal, so it costs nothing per
	// render and nothing per cell; the numbers are computed by the server from the
	// same Window() every push already reads, and arrive as ~60 bytes of signal
	// patch. It is empty (and therefore invisible) for a single cell.
	b.WriteString(`<span class="m ag" data-text="$_ag"></span>`)
	// Who else is here. One element in the shell, patched by id on a presence
	// wake and on nothing else — no push re-sends it, no morph removes it, and no
	// cell grows by a byte. It is emitted empty so the first presence frame can be
	// an ordinary `outer` morph rather than an append-or-morph decision, the same
	// argument the empty `<style id="sy">` makes.
	b.WriteString(presenceChipsSeed())
	b.WriteString(`<span class="m" data-text="'rows '+$lo+'–'+$hi"></span>`)
	// The go-to box. It is the affordance that makes a linkable region usable
	// (type D500, land on D500) and it is also the only interaction in the page
	// that is a navigation rather than a scroll — see gotoExpr for why that
	// distinction is the whole of the feature.
	b.WriteString(`<input id="go" placeholder="D500 or A1:D20" aria-label="go to cell or range"`)
	b.WriteString(` autocomplete="off" spellcheck="false" data-on:keydown="`)
	b.WriteString(gotoExpr(esc))
	b.WriteString(`"><span class="sp"></span>`)
	// Sheet CRUD lives in the toolbar as ordinary hypermedia: a link and a form.
	// Neither needs Datastar, neither needs a signal, and both work with the
	// bundle blocked — which is the right default for the two actions that
	// navigate rather than patch.
	b.WriteString(`<a class="btn" href="/">All sheets</a>`)
	b.WriteString(`<form method="post" action="/sheets" style="margin:0"><button class="btn p" type="submit">New sheet</button></form>`)
	b.WriteString(`</header>`)
	// The formula bar: page-shell furniture between the toolbar and the scroll
	// container. A second input on `$ref`/`$raw`, showing the active cell's raw text
	// rather than its computed value and committing through the same `#ed`. Nothing a
	// push sends can reach it. See formulabar.go.
	b.WriteString(formulaBarHTML(sheetID))

	// Both interaction handlers live on the never-patched scroll container and
	// use event delegation. Nothing per-cell: 6,500 `data-on:click` attributes
	// would dominate the payload, and every one of them would be re-sent on
	// every push.
	b.WriteString(`<div id="vp"`)
	b.WriteString(vpInlineStyle(widths, rows))
	// The anchor row, for anchorScript's seek. An attribute rather than an
	// interpolation for the reason given on anchorScript, and omitted at row 0
	// so the overwhelmingly common case costs nothing.
	if at.Ref.Row > 0 {
		b.WriteString(` data-ar="`)
		b.WriteString(strconv.Itoa(at.Ref.Row))
		b.WriteString(`"`)
	}
	// The column widths and the row extent live here, on one element, as 27
	// custom properties. They inherit into every row and every cell without a
	// single per-cell byte, and because `#vp` is page-shell markup this attribute
	// is never re-sent by a push — which is what lets a running sheet change
	// height without re-rendering the grid.
	b.WriteString(` data-style="`)
	b.WriteString(colWidthStyleExpr())
	b.WriteString(`" data-on:contextmenu="`)
	b.WriteString(menuOpenExpr)
	// Pointerdown commits, and it is listed first because it happens first: the
	// gesture that moves the selection begins here, strictly before the click
	// handler rewrites `$ref` out from under an edit still sitting in the editor.
	b.WriteString(`" data-on:pointerdown="`)
	b.WriteString(vpPointerDownExpr)
	// The drag: two more delegated handlers on the same never-patched element.
	// Between them they issue no requests at all — a rectangle is two signals, and
	// the box that draws it is CSS. The release is on the window because a gesture
	// may well end outside the grid it started in.
	b.WriteString(`" data-on:pointermove="`)
	b.WriteString(vpPointerMoveExpr)
	b.WriteString(`" data-on:pointerup__window="`)
	b.WriteString(vpPointerUpExpr(sheetID))
	b.WriteString(`" data-on:click="`)
	b.WriteString(vpClickExpr)
	// A double-click edits, preserving the content — the mouse's F2.
	b.WriteString(`" data-on:dblclick="`)
	b.WriteString(vpDblClickExpr(sheetID))
	// The keyboard is one attribute on one element, and it listens on the window
	// rather than on a focused grid: after any blur the keyboard belongs to
	// `<body>`, so a grid-scoped listener would discard every keystroke after a
	// cell is merely selected. See keys.go.
	b.WriteString(`" data-on:keydown__window="`)
	b.WriteString(vpKeyExpr(esc))
	// The second half of "commit and move": `#ed` has posted the cell command and
	// says which way to go. It cannot do the move itself — a viewport command
	// issued from `#ed` would abort the cell command it just sent, because
	// Datastar keys request cancellation on the element.
	b.WriteString(`" data-on:` + navEvent + `__window="`)
	b.WriteString(vpNavExpr(esc))
	// The anchored cell becomes the selection as soon as Datastar boots. It runs
	// once, only if nothing is selected yet, and it reads the cell's real box —
	// see the data-signals note above.
	b.WriteString(`" data-init="`)
	b.WriteString(vpInitExpr)
	// Throttle, not debounce. A continuous drag emits a scroll event every frame, so
	// under a debounce the handler never runs until the drag stops and the delay
	// tracks drag length rather than the setting — against a 65-125ms budget for
	// everything downstream. Both edges are wanted: leading so the first scroll
	// event posts immediately, trailing so the drag's final position is requested
	// rather than landing between ticks and stranding a stale buffer.
	b.WriteString(`" data-on:scroll__throttle.100ms.trailing="`)
	b.WriteString(scrollExpr(esc))
	b.WriteString(`">`)
	// The column header lives inside the scroll container. A sibling above it is
	// fine until the grid is wider than the window: then the header stays put while
	// the columns scroll under it and every letter is over the wrong column.
	//
	// It costs the scroll arithmetic nothing. The strip is `colHeadPx` of flow at
	// the top of the scroll content, so `floor(scrollTop/rowHeightPx)` is still
	// exactly the first row below the frozen header — which is where `T.seek(r)`
	// puts it.
	b.WriteString(colHeaderHTML(esc))
	b.WriteString(grid)
	// The selection box is shell markup, not grid markup, and that is most of what
	// makes interactive selection free: written once per page load, unremovable by
	// anything a push sends, and — as a box over a coordinate space — exactly as
	// correct over the 99% of cells with no element. The price is that as a sibling
	// of `#g` it sits below the frozen header strip, which `#ov` carries once as
	// `top:var(--ch)`.
	b.WriteString(`<div id="` + boxOverlayID + `">`)
	b.WriteString(`<div id="` + selBoxID + `" data-show="$_sar!==$_sfr||$_sac!==$_sfc"`)
	b.WriteString(` data-style="window.__ss?window.__ss.selBox($_sar,$_sac,$_sfr,$_sfc,$` + heightSignal + `):{}"></div>`)
	// The copy marquee, on exactly the same terms as `#sb`: shell markup, one box
	// over a coordinate space, zero bytes per render and per cell, and correct
	// over the empty cells that under sparse rendering are most of them. It is
	// drawn from the clipboard signal rather than a second pair of corner signals,
	// because the string is what the paste command has to send anyway.
	b.WriteString(`<div id="` + copyBoxID + `" data-show="$_cp!==''"`)
	b.WriteString(` data-style="window.__ss?window.__ss.rngBox($_cp):{}"></div>`)
	b.WriteString(`</div>`)
	// The collaborator overlay, on the same terms as `#sb` and `#cb`: shell markup,
	// a sibling of `#g`, zero bytes per render and per cell. Same mechanism as `#ov`
	// and `#oc` — a grid with `#b`'s column tracks whose children carry `--r` and a
	// `grid-column` — differing only in the source of the coordinates, which here is
	// what the server pushed. See presenceui.go.
	b.WriteString(presenceOverlaySeed())
	// The way out of a fixed-size sheet, and it is the last child of `#vp` for a
	// positional reason: it is seated one pixel past the last row, so it is always in
	// the DOM and only visible at the foot of the sheet, and `--rows` walks it down
	// when anybody grows the sheet. See growrows.go.
	b.WriteString(growRowsHTML(esc))
	b.WriteString(`</div>`)

	// The range clear, on an element of its own. Datastar keys request cancellation
	// on the element, so the slowest command must not share one with the fastest —
	// `#vp` posts a viewport command on every buffer edge, and one landing mid-clear
	// would abort a command part-way through writing cells. The range travels as an
	// explicit `payload` rather than a signal, which would ride every request the
	// page ever makes for the sake of one keystroke.
	b.WriteString(`<div id="cl" hidden data-on:` + clearEvent + `__window="`)
	b.WriteString(clearPost(esc))
	// The `pw` class is the pending chip's backstop marker — see chipFailScript.
	b.WriteString(`" class="` + pendingWriteCl + `"></div>`)

	// The other three selection-shaped operations, each on an element nothing else
	// posts from — the `#cl` rule again, and the reason it is a rule. Datastar
	// keys request cancellation on the element, so a fill sharing an issuer with
	// the viewport command would be aborted by the next buffer edge the reader
	// scrolls past, half-way through writing cells.
	b.WriteString(`<div id="fl" hidden data-on:` + fillEvent + `__window="`)
	b.WriteString(fillPost(esc))
	b.WriteString(`" class="` + pendingWriteCl + `"></div>`)
	b.WriteString(`<div id="pv" hidden data-on:` + pasteEvent + `__window="`)
	b.WriteString(pastePost(esc))
	b.WriteString(`" class="` + pendingWriteCl + `"></div>`)
	// The fourth slow write, on the fourth element. Styling joins `#cl`, `#fl` and
	// `#pv` rather than sharing an issuer, because a newer style command must not
	// abort an older one: bold-then-italic is two intentions, not a correction of
	// one. See styleui.go.
	b.WriteString(styleIssuerHTML(esc))
	// The selection commit. The `data-effect` subscribes to the connection id, the
	// active cell and the four selection signals — one subscription instead of a
	// handler on each of the dozen gestures that can move a range — and only
	// re-arms a timer, so a drag issues nothing until it settles. The aggregate
	// comes back on the ordinary push, because the server holds the rectangle. See
	// selPost and T.selq.
	b.WriteString(selIssuerHTML(esc))

	// The bfcache bracket: `pagehide` closes the stream and `pageshow` reopens it.
	// Chrome keeps an abandoned page's SSE stream open in the back/forward cache,
	// which costs twice — the server still counts that reader as present, and the
	// socket still counts against the six-per-origin connection budget, so after
	// ~6 same-origin navigations every subsequent request stalls for tens of
	// seconds. See bfScript.
	b.WriteString(bfHTML())

	// The row extent's second consumer. `--rows` reaches CSS through `#vp`'s
	// data-style, but the client's own arithmetic needs the number too: the go-to box
	// rejects a row past the end of the sheet, Ctrl+A selects to the last row, and
	// every arrow key clamps against it.
	//
	// It issues no request, so it may be its own element without joining the
	// cancellation argument the writers make, and it is an effect rather than a line
	// in some other expression so that there is exactly one writer of T.rows.
	b.WriteString(`<div id="` + extentID + `" hidden data-effect="` + extentEffectExpr + `"></div>`)
	b.WriteString(chipFailHTML())
	// The drag guide. Shell markup, no signal, no data-* attribute of any kind: it
	// is moved by T.gdMove with a transform, which is the only way to preview a
	// resize without asking the browser to lay out the whole buffer. See the
	// column-resize note above.
	b.WriteString(`<div id="` + guideID + `"></div>`)

	b.WriteString(menuHTML(esc))

	// Back and forward move the view: set the viewport signals, position the scroll
	// container, issue the viewport command. It gets an element of its own because
	// Datastar's fetch actions abort the element's previous in-flight request;
	// hanging this off `#live` would make the first back press abort the SSE stream,
	// and nothing reopens it.
	b.WriteString(`<div id="nav" hidden data-on:popstate__window="`)
	b.WriteString(popExpr(esc))
	b.WriteString(`"></div>`)

	// The stream must opt in to staying open when the tab is hidden. Datastar
	// registers GET with `openWhenHidden` defaulting to false, which is sensible for
	// a one-shot GET and fatal for the only channel this page learns anything on:
	// the tab backgrounds for a moment, the stream is aborted, and the grid silently
	// stops updating while every command it sends still succeeds.
	//
	// `requestCancellation:'cleanup'` is the bfcache bracket's other half. The
	// default `'auto'` registers no element cleanup, so removing `#live` would leave
	// the stream open and the departed viewer in the avatar list. Nothing else ever
	// posts from `#live`, so this weakens nothing.
	//
	// The query string is the render-and-subscribe handover: this page arrived with
	// the whole buffer already in it, so without it the stream's first act is to
	// render the same window again and morph it over itself. `bl`/`bh` are the
	// window this document holds, without which the stream cannot agree with the
	// page about which rows it means — `lo`/`hi` are seeded with the page's buffer
	// and handleLive widens them by BufferBands again. `d` and `sy` are the SHA-256
	// of the grid and stylesheet already in the document, priming the comparison
	// Registry.Deliver and patchStyles already do. It is an optimisation and never a
	// shortcut: a client that connects with no query string gets the full window.
	b.WriteString(`<div id="live" hidden data-init="@get('/s/`)
	b.WriteString(esc)
	b.WriteString(`/live?`)
	// Escaped because it is an attribute value: a bare `&` in `&bh=` is a parse
	// error the browsers all tolerate, and tolerated is not the same as correct.
	b.WriteString(html.EscapeString(liveHandoverQuery(loRow, hiRow, grid, styleCSS)))
	b.WriteString(`',{openWhenHidden:true,requestCancellation:'cleanup'})"></div>`)
	b.WriteString(`</body></html>`)
	return b.String()
}

// ─── Insert / delete rows and columns ────────────────────────────────────────
//
// The affordance is a context menu on the headers, plus a caret on the column
// headers for people who do not think to right-click. Both are delegated handlers
// on page-shell elements, so the feature costs zero bytes per row and per cell.
//
// Rows get no caret: the row-number gutter is rendered per row and inside the
// patch region, so a caret there would be re-sent on every scroll patch and every
// full morph. Right-click works on both.
const menuID = "mn"

// menuOpenExpr is the contextmenu handler on `#vp`. It figures out whether the
// pointer is over a row number or a column letter, records which one, and
// positions the menu in viewport coordinates (the menu is position:fixed, so
// clientX/clientY are exactly right and no scroll offset is involved).
const menuOpenExpr = `const t=evt.target;` +
	// The row-number gutter. The parent test is load-bearing: a cell is the same
	// element type as a gutter number, and only its parent tells them apart.
	`if(t.parentElement&&t.parentElement.id==='` + gutterID + `'){$mk='r';$mi=+t.id.slice(1)-1}` +
	`else if(t.dataset&&t.dataset.c!==undefined){$mk='c';$mi=+t.dataset.c}` +
	`else if(t.tagName==='I'&&t.parentElement.dataset.c!==undefined){$mk='c';$mi=+t.parentElement.dataset.c}` +
	`else return;` +
	`evt.preventDefault();$mx=Math.min(evt.clientX,innerWidth-200);$my=evt.clientY;$mo=true`

// menuCaretExpr opens the same menu from the ▾ in a column header. It stops
// propagation because the menu closes on any window click — including the very
// click that opened it, which would make the caret appear to do nothing.
const menuCaretExpr = `const t=evt.target;if(!t.classList.contains('cr'))return;` +
	`evt.stopPropagation();const s=t.parentElement;$mk='c';$mi=+s.dataset.c;` +
	`const r=s.getBoundingClientRect();$mx=Math.min(r.left,innerWidth-200);$my=r.bottom;$mo=true`

// menuHTML is the menu itself: six buttons, in the page shell, never patched. Each
// posts from its own element, because Datastar keys request cancellation on the
// issuing element and a command posted from anything the stream can replace can
// abort the stream.
//
// `$p=true` before the post is the honest part: a row insert rewrites every cell
// below the insertion point and takes seconds on a 10,000-row sheet. The server
// clears `$p` on the push that carries the new grid, not on the command's response,
// because the response returns before the data is on screen.
func menuHTML(sheetID string) string {
	item := func(op, path, label string) string {
		// The timeout is a safety net, not the mechanism: the server clears `$p` on
		// the push that carries the reshaped grid, and this only fires if that push
		// never comes (the stream was reaped, the command failed, no screen was
		// registered). A pending chip that can get stuck forever is worse than no
		// chip at all, because it makes a working page look broken.
		return `<button class="` + pendingWriteCl + `" data-on:click="$mop='` + op + `';` +
			pendingRaise(`Reshaping the sheet…`) +
			`@post('/s/` + sheetID + `/` + path + `',{requestCancellation:'disabled'})">` + label + `</button>`
	}
	return `<div id="` + menuID + `" data-show="$mo" data-style="{top:` + "`${$my}px`" + `,left:` + "`${$mx}px`" + `}"` +
		` data-on:click__window="$mo=false">` +
		`<div data-show="$mk==='r'">` +
		item("ia", "rows", "Insert 1 row above") +
		item("ib", "rows", "Insert 1 row below") +
		`<hr>` + item("d", "rows", "Delete row") +
		`</div><div data-show="$mk==='c'">` +
		item("ia", "cols", "Insert 1 column left") +
		item("ib", "cols", "Insert 1 column right") +
		`<hr>` + item("d", "cols", "Delete column") +
		`</div></div>` +
		// The status chip says one of two things: that slow structural work is in
		// flight, or why the last one was refused. A refusal is not hypothetical —
		// inserting a row into a sheet whose last row has content would push that
		// content off the grid, and the store rejects it (ErrWouldTruncate). The
		// command returns 400, so no push follows and the chip would otherwise just
		// vanish. Click to dismiss.
		`<div id="bz" data-show="$p||$note!==''" data-on:click="$note=''"` +
		` data-text="$note||$_pt"></div>`
}

// scrollExpr keeps scrolling inside the buffer at zero requests. It reads the
// buffer bounds the server last told it about, works out which rows are on
// screen, and posts a viewport command only when the visible rows come within
// edgeGuardRows of an edge. The bounds are `$blo`/`$bhi` and not `$lo`/`$hi`:
// the latter are this handler's own output, the viewport it is asking for.
//
// mark() runs on every invocation, including the ones that decide not to post,
// because "how often did the buffer absorb a scroll" and "how long did the
// handler wait" are the two numbers that say whether a scroll stall is throttle
// or morph.
//
// The URL tracks the scroll with replaceState, never pushState: a scroll is not
// a navigation, and a history entry per wheel tick would fill the back stack.
//
// The settle guard comes first. Setting `scrollTop` fires a synthetic `scroll`
// event, so an anchored load would otherwise post a viewport command asking for
// the window it is already looking at. `settle` swallows exactly that event:
// matched on position, consumed once, expiring.
func scrollExpr(sheetID string) string {
	return `if(window.__ss&&window.__ss.settle(el.scrollTop))return;` +
		`const lo=$blo,hi=$bhi,` +
		`r=Math.floor(el.scrollTop/` + strconv.Itoa(rowHeightPx) + `),` +
		`n=Math.ceil(el.clientHeight/` + strconv.Itoa(rowHeightPx) + `);` +
		`const ok=(r-lo<` + strconv.Itoa(edgeGuardRows) + `||hi-(r+n)<` + strconv.Itoa(edgeGuardRows) + `);` +
		`const m=window.__ss?window.__ss.mark(ok):null;` +
		`if(m){$tScroll=m.scroll;$tPost=m.post;$tQuiet=m.quiet;$tWait=m.wait;` +
		`$tMorph=m.morph;$tRender=m.render;$tBytes=m.bytes;$tScrolls=m.scrolls;$tSkips=m.skips}` +
		`if(window.__ss)window.__ss.track(r);` +
		`if(ok){$lo=r;$hi=r+n;@post('/s/` + sheetID + `/viewport')}`
}

// gotoExpr is the go-to box: type `D500` or `A1:D20`, press Enter, land there.
//
// It is a pushState, and that is the distinction the feature turns on: an explicit
// jump is a navigation and back must return the reader, where a scroll is not and
// uses replaceState (scrollExpr). The row arithmetic is client-side because
// scrollTop is a client fact the server cannot set, and anything it rejects is
// ignored — a typo should do nothing rather than navigate somewhere arbitrary.
func gotoExpr(sheetID string) string {
	return `if(evt.key!=='Enter'||!window.__ss)return;` +
		`const v=el.value.trim(),r=window.__ss.row(v);if(r<0)return;` +
		`$at=v;` +
		`history.pushState({at:v},'',location.pathname+'?at='+encodeURIComponent(v));` +
		`const n=window.__ss.seek(r);$lo=r;$hi=r+n;` +
		`@post('/s/` + sheetID + `/viewport');el.blur()`
}

// popExpr services back/forward. It reads `at` back out of the URL the browser
// has just restored and does exactly what a jump does, minus the pushState —
// the entry already exists, which is why we are here.
func popExpr(sheetID string) string {
	return `if(!window.__ss)return;` +
		`const v=window.__ss.at(),r=window.__ss.row(v);if(r<0)return;` +
		`$at=v;const n=window.__ss.seek(r);$lo=r;$hi=r+n;` +
		`@post('/s/` + sheetID + `/viewport')`
}

// anchorScript is the client half of a linkable region: it positions the scroll
// container on the anchored row and owns the helpers the Datastar expressions above
// call. They hang off the same `window.__ss` object as the measurement harness, so
// it must run after clientTimingScript, which creates it.
//
// The initial seek is synchronous and inline, because from `data-init` or a load
// handler the browser would paint row 0 first and then jump. It emits nothing for
// row 0: seeking to where the page already is would arm the settle guard for an
// event that never fires, leaving it to swallow the first real scroll.
//
// `history.scrollRestoration = 'manual'` is not decoration: back/forward between two
// same-document entries makes Chrome restore the scroll position it remembers, which
// races the popstate handler's own seek.
//
// The row and the extent are read from the DOM rather than interpolated, because a
// bundle whose bytes depend on which sheet you opened is not shareable. See assets.go.
func anchorScript() string {
	rh := strconv.Itoa(rowHeightPx)
	return `<script>(function(){var T=window.__ss;if(!T)return;
var vp=document.getElementById('vp'),RH=` + rh + `,MINH=` + strconv.Itoa(MinRowHeight) + `,MAXH=` + strconv.Itoa(MaxRowHeight) + `,GRIP=` + strconv.Itoa(rowGripPx) + `;
// T.rows is the sheet's allocated row extent, and it is a variable: a sheet
// grows when someone writes past its bottom and shrinks when rows are deleted.
// Seeded from --rows and re-seeded by T.setRows whenever the _rows signal
// changes (see #` + extentID + `),
// so every client-side row clamp — the go-to box below, select-all, arrow keys,
// hit testing — follows a sheet that grows or shrinks under an open screen.
T.rows=(+getComputedStyle(vp).getPropertyValue('--rows'))||0;
// setRows(): the one writer. The zero guard matters because a missing value
// would turn every clamp into "row -1" and make the grid inert.
//
// Shrinking is the direction that needs care. When the sheet gets shorter than
// the reader's scroll position the browser clamps scrollTop itself and fires a
// scroll event, and that event has to reach the throttled viewport handler so
// the server learns where this screen ended up. The settle guard is deliberately
// not armed: swallowing that event would strand a viewport on rows the sheet no
// longer has. It fires once per clamp and the response never moves scrollTop, so
// it cannot loop.
T.setRows=function(n){n=+n;if(!(n>0))return;T.rows=n;};
// ROW GEOMETRY. Rows are not all the same height, so a row's top is a prefix
// sum rather than a multiplication. The _rh signal carries the resized rows as
// flat [row,height,...] pairs, sorted, and the running totals sit beside them so
// an offset costs a binary search instead of a walk over the sheet.
//
// Every clamp, hit test and scroll below goes through topOf/rowAtY. Leaving any
// one of them multiplying by RH is not a rendering glitch — the CSS still places
// the row correctly, so the grid looks right and answers a click with the wrong
// cell.
T.hrow=[];T.hdel=[];T.hpre=[0];T.hsrc='';
T.setHeights=function(a){var s=(a&&a.length)?String(a):'';if(s===T.hsrc)return;
 var rw=[],dl=[],pre=[0],d=0;
 if(a&&a.length)for(var i=0;i+1<a.length;i+=2){rw.push(+a[i]);
  var x=(+a[i+1])-RH;dl.push(x);d+=x;pre.push(d);}
 T.hrow=rw;T.hdel=dl;T.hpre=pre;T.hsrc=s;};
// hlo: the first resized row at or after r, which is also how many resized rows
// lie strictly above it — so hpre[hlo(r)] is exactly the shift r has inherited.
T.hlo=function(r){var a=T.hrow,lo=0,hi=a.length;
 while(lo<hi){var m=(lo+hi)>>1;if(a[m]<r)lo=m+1;else hi=m;}return lo;};
// The second argument is the height signal itself, and it is both the
// subscription and the data. A Datastar expression that positions an overlay has
// to name the signal to re-run when a row is resized, because the tables above
// are plain state; naming it without reading it is not enough, because two
// effects on one signal have no defined order and the overlay's would otherwise
// be free to compute its top from tables the other effect had not filled in yet.
// Rebuilding from the argument makes the answer independent of that order.
// TestNoOverlayComputesItsOwnPixels states the rule for both axes.
T.topOf=function(r,dep){if(dep!==undefined)T.setHeights(dep);return r*RH+T.hpre[T.hlo(r)];};
T.hOf=function(r,dep){if(dep!==undefined)T.setHeights(dep);
 var i=T.hlo(r);return (i<T.hrow.length&&T.hrow[i]===r)?RH+T.hdel[i]:RH;};
// rowAtY inverts topOf. Monotonic, so a binary search over the extent; the
// uniform case skips it entirely, which is every sheet nobody has resized.
T.rowAtY=function(y){if(y<0)return 0;
 if(!T.hrow.length)return Math.floor(y/RH);
 var lo=0,hi=(T.rows||1)-1;
 while(lo<hi){var m=(lo+hi+1)>>1;if(T.topOf(m)<=y)lo=m;else hi=m-1;}
 return lo;};
if('scrollRestoration' in history)history.scrollRestoration='manual';
// row(): the client's half of ParseRef. Returns the 0-indexed TOP-LEFT row of
// "D500" or "A1:D20", or -1 for anything it does not recognise.
T.row=function(v){var m=/^\s*([A-Za-z])([0-9]+)(?::([A-Za-z])([0-9]+))?\s*$/.exec(v||'');
 if(!m)return -1;var a=+m[2],b=m[4]?+m[4]:a,r=Math.min(a,b)-1;
 return (r<0||r>=T.rows)?-1:r;};
T.at=function(){try{return new URLSearchParams(location.search).get('at')||'A1'}catch(e){return 'A1'}};
// focus(): put the caret in the editor, in the same task as the click.
//
// Setting the editing signal un-hides the editor through data-show, which is a
// reactive effect scheduled on the browser's terms. Until it runs the input is
// still display:none, and focus() on a display:none element is a silent no-op —
// the cell looks selected, keystrokes vanish, and Enter does nothing.
//
// So the un-hiding happens here rather than waiting to be given it: removing the
// inline display:none is what data-show is about to do anyway, and doing it first
// makes focus deterministic. The rAF remains as a fallback in case something else
// re-hid the element in between.
T.focus=function(){var e=document.getElementById('` + editorID + `');if(!e)return;
 e.style.removeProperty('display');
 e.focus();if(e.select)e.select();
 if(document.activeElement!==e)requestAnimationFrame(function(){
  e.style.removeProperty('display');e.focus();if(e.select)e.select();});};
// seek(): put row r at the top of the viewport and arm the settle guard for the
// synthetic scroll event that assignment is about to fire. Returns how many rows
// are actually visible, so the caller can report a real viewport instead of the
// server's assumed one.
var pendTop=null,pendAt=0;
T.seek=function(r){var top=T.topOf(r);pendTop=top;pendAt=performance.now();
 if(vp)vp.scrollTop=top;
 return vp?Math.ceil(vp.clientHeight/RH):40;};
// arm(): the same guard, for a caller that has already moved the container
// itself. Keyboard navigation scrolls to follow the selection (keys.go), and
// that assignment fires a synthetic scroll event which the scroll handler would
// answer with a viewport command of its own — a second request for a buffer the
// keyboard has already asked for.
T.arm=function(top){pendTop=top;pendAt=performance.now();};
// settle(): true exactly once, and only for the scroll event caused by our own
// seek. Position-matched so it cannot swallow a real scroll, consumed on first
// call so it cannot swallow a second one, and expired after 3s so a page that
// never fires the event does not leave a guard armed forever.
T.settle=function(top){if(pendTop===null)return false;
 var p=pendTop;pendTop=null;
 if(performance.now()-pendAt>3000)return false;
 return Math.abs(top-p)<=1;};
// track(): the address bar follows the scroll, at most once every 300ms, and
// ALWAYS with replaceState — zero history entries, however long the scroll.
var urlAt=0,urlTimer=0,urlRow=-1;
T.flushURL=function(){urlTimer=0;urlAt=performance.now();
 if(urlRow<0)return;
 try{history.replaceState(history.state,'',location.pathname+'?at=A'+(urlRow+1))}catch(e){}};
T.track=function(r){urlRow=r;
 var now=performance.now();
 if(now-urlAt>=300){T.flushURL();return;}
 if(!urlTimer)urlTimer=setTimeout(T.flushURL,300);};
// The drag guide, on either axis. Column resize uses 'x'; row resize will use
// 'y' and needs no new mechanism, only the other two style properties.
var gdEl=null,gdAxis='x';
T.gdShow=function(axis,pos){gdEl=gdEl||document.getElementById('` + guideID + `');
 if(!gdEl||!vp)return;var r=vp.getBoundingClientRect();gdAxis=axis;
 if(axis==='x'){gdEl.style.top=r.top+'px';gdEl.style.height=r.height+'px';
  gdEl.style.left='0px';gdEl.style.width='2px';}
 else{gdEl.style.left=r.left+'px';gdEl.style.width=r.width+'px';
  gdEl.style.top='0px';gdEl.style.height='2px';}
 gdEl.style.display='block';T.gdMove(pos);};
T.gdMove=function(pos){if(!gdEl)return;
 gdEl.style.transform=gdAxis==='x'?'translateX('+pos+'px)':'translateY('+pos+'px)';};
T.gdHide=function(){if(gdEl)gdEl.style.display='none';};
// rzStart/rzMove/rzEnd: the column drag. The move writes no signal and touches
// no width — it moves the guide's transform and nothing else — so a dense buffer
// lays out once, on release, instead of once per frame.
var rzX=0,rzW=0,rzC=-1;
T.rzStart=function(c,x,w){rzC=c;rzX=x;rzW=w;T.gdShow('x',x);};
T.rzAt=function(x){var v=rzW+(x-rzX);
 return v<` + strconv.Itoa(MinColWidth) + `?` + strconv.Itoa(MinColWidth) +
		`:(v>` + strconv.Itoa(MaxColWidth) + `?` + strconv.Itoa(MaxColWidth) + `:v);};
// The guide follows the CLAMPED width, not the raw pointer, so dragging past the
// minimum stops the line where the column would actually stop.
T.rzMove=function(x){if(rzC<0)return;T.gdMove(rzX+(T.rzAt(x)-rzW));};
T.rzEnd=function(x){if(rzC<0)return null;var c=rzC,w=T.rzAt(x);
 rzC=-1;T.gdHide();return {c:c,w:w};};
T.rzCancel=function(){rzC=-1;T.gdHide();};
// The same gesture one axis over. The move writes nothing either: a row resize
// reflows every row below it, so doing that per frame on a dense buffer is the
// one thing the guide exists to avoid.
// THE GUTTER SERVES THREE GESTURES and they overlap on the same pixels: click a
// row number to select the row, drag its bottom edge to resize, double-click
// that edge to fit. gripAt is the one test that separates them, and both sides
// consult it — the resize acts on it, the selection declines it (T.rowAt). Two
// independent guesses at where the grip is would be two chances to disagree, and
// the way they disagree is that a resize drag also drags a row selection behind
// it.
//
// The column axis draws the same line with an element: its grips are <i> and
// T.hdrAt ignores them. The row axis has no element to test, because a handle
// per buffered row would double the gutter's markup for a target the pointer can
// only be on one of at a time.
T.gripAt=function(e){var t=e.target;
 if(!t||t.tagName!=='B'||!t.parentElement||t.parentElement.id!=='` + gutterID + `')return -1;
 var q=t.getBoundingClientRect(),r=+t.style.getPropertyValue('--r');
 if(e.clientY>=q.bottom-GRIP)return r;
 // The other half of the same edge. Pressing just BELOW a boundary is aiming at
 // that boundary, and the boundary belongs to the row above it.
 if(e.clientY<=q.top+GRIP&&r>0)return r-1;
 return -1;};
// These report whether they took the gesture, because they share one handler
// with the selection rather than sitting on an element of their own. Saying "not
// mine" is how the selection gets its turn.
//
// The starting height comes from the geometry the client already has, not from
// the element under the pointer: the grip may belong to the row ABOVE the one
// that was pressed, and measuring the wrong box is a resize that jumps on the
// first pixel of movement.
var rzYy=0,rzHh=0,rzR=-1;
T.rzDownR=function(e){if(e.button!==0)return false;
 var r=T.gripAt(e);if(r<0)return false;
 e.preventDefault();rzR=r;rzYy=e.clientY;rzHh=T.hOf(r);T.gdShow('y',e.clientY);
 return true;};
T.rzAtR=function(y){var v=rzHh+(y-rzYy);return v<MINH?MINH:(v>MAXH?MAXH:v);};
T.rzMoveR=function(y){if(rzR<0)return false;T.gdMove(rzYy+(T.rzAtR(y)-rzHh));return true;};
T.rzEndR=function(y){if(rzR<0)return null;var r=rzR,h=T.rzAtR(y);
 rzR=-1;T.gdHide();return h===rzHh?null:{r:r,h:h};};
// The one layout fact the client originates. The server has no font metrics, so
// the height a row needs is measurable only where the text is laid out — and
// then it travels as an ordinary resize command, which is why fitting and
// dragging arrive at the store through the same door.
//
// Only the buffered rows can be measured, and that is exactly the set this can
// be asked about: the gesture is a double-click on a row's own number.
//
// The measuring class rather than scrollHeight, and it is the difference
// between fitting and only ever growing: scrollHeight never reports less than
// the box it is measured in, so a row someone has made tall would stay tall no
// matter what it held. Releasing the height first is what lets a fit shrink one.
// A class rather than an inline height, because the row level of the cascade
// matches on the literal style attribute (rowVarDecl) and writing to it here
// would drop the row's own styling for the length of the measurement.
T.fitR=function(r){var d='--r:'+r,h=0,i,
 q=document.querySelectorAll('#` + bufferID + ` b[style="'+d+'"],#` + bufferID + ` b[style^="'+d+';"]');
 for(i=0;i<q.length;i++)q[i].classList.add('` + measureClass + `');
 for(i=0;i<q.length;i++)if(q[i].offsetHeight>h)h=q[i].offsetHeight;
 for(i=0;i<q.length;i++)q[i].classList.remove('` + measureClass + `');
 h=h?h+1:RH;
 return h<MINH?MINH:(h>MAXH?MAXH:h);};
T.rzLive=function(){return rzC;};
T.anchorRow=+((vp&&vp.dataset.ar)||0);
if(T.anchorRow>0)T.seek(T.anchorRow);
})();</script>`
}

// edgeGuardRows is how close the visible rows may get to a buffer edge before
// the client asks for a new buffer. One band: cross it and the next band is
// already being fetched while there is still a band of slack to scroll through.
const edgeGuardRows = BandHeight

// liveHandoverQuery is what the document tells its own stream about itself: the
// buffer it holds and the digests of the two payloads it already carries.
//
// Full SHA-256, not a prefix. A collision here is a suppressed push, which is a
// stale grid with no later correction — the one failure this must not have.
func liveHandoverQuery(loRow, hiRow int, grid, styleCSS string) string {
	return "bl=" + strconv.Itoa(loRow) +
		"&bh=" + strconv.Itoa(hiRow) +
		"&d=" + digestHex(grid) +
		"&sy=" + digestHex(styleCSS)
}

// digestHex is the hash both sides compare on. It is sha256 because that is
// what Registry.Deliver already uses, so the primed value and the value the
// suppression path computes are the same function of the same bytes.
func digestHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
