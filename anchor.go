package main

// anchor.go — a linkable region.
//
// `GET /s/{id}?at=D500` and `GET /s/{id}?at=A1:D20` make a place in the sheet
// addressable. The design turns on `at` being a query parameter rather than a
// fragment: a fragment never reaches the server, so `#D500` would render row 0,
// ship 250 rows the reader did not ask for, and then jump — a visibly wrong
// first paint. `?at=` is in the request line, so the buffer that comes back is
// already the right buffer and the browser only has to position itself in it.
//
// Parsing is deliberately a thin adapter over grid.go: ParseRef and
// ParseRangeBounds already reject out-of-range rows, multi-letter columns and
// oversized rectangles, and a second parser would be a second set of bugs. All
// this file adds is the anchor/selection distinction and the rule that a
// malformed `at` is not an error — it is the absence of an anchor. A shared
// link that has been mangled by a chat client must open the sheet, not a 500.

import "strings"

// selRange is the linked rectangle, in the same 0-indexed space as CellRef. It
// is comparable on purpose: "has the selection changed since the client's DOM
// was built" is a `!=` on this struct, and that question decides whether a push
// can stay incremental.
type selRange struct {
	Lo, Hi CellRef
	On     bool
}

// contains reports whether a cell is inside the linked rectangle. Called once
// per rendered cell, so it is bounds arithmetic and nothing else — no map, no
// allocation, and the common case (no selection at all) is one boolean test.
func (s selRange) contains(row, col int) bool {
	return s.On &&
		row >= s.Lo.Row && row <= s.Hi.Row &&
		col >= s.Lo.Col && col <= s.Hi.Col
}

// rows returns the inclusive row span of the selection.
func (s selRange) rows() (lo, hi int, ok bool) {
	if !s.On {
		return 0, 0, false
	}
	return s.Lo.Row, s.Hi.Row, true
}

// anchor is a parsed `at` value.
//
// Ref is the cell the viewport is positioned on; for a range it is the top-left
// corner, because anchoring on the bottom-right of a tall range would scroll the
// thing being linked to off the top of the screen.
type anchor struct {
	Ref CellRef
	Sel selRange
	// Raw is the normalized round-trip form — "D500" or "A1:D20" — echoed back
	// into the `at` signal so every later command carries the same selection
	// without the client having to re-send the original spelling.
	Raw string
}

// zeroAnchor is what an absent or malformed `at` means: the top of the sheet,
// nothing selected. It is the same result as no `at` at all, which is why a bad
// ref can be treated as no ref rather than as a failure.
func zeroAnchor() anchor { return anchor{} }

// parseAt parses an `at` query value. ok is false for empty or malformed
// input; the returned anchor is still usable (it is the top of the sheet), so
// callers may ignore ok entirely and only consult it in order to log.
//
// A range collapses to a single cell when both endpoints are equal — "D500" and
// "D500:D500" then differ only in that the latter marks one cell as selected,
// which is a distinction worth keeping.
func parseAt(s string) (anchor, bool) {
	v := strings.TrimSpace(s)
	if v == "" {
		return zeroAnchor(), false
	}
	if strings.Contains(v, ":") {
		lo, hi, err := ParseRangeBounds(v)
		if err != nil {
			return zeroAnchor(), false
		}
		return anchor{
			Ref: lo,
			Sel: selRange{Lo: lo, Hi: hi, On: true},
			Raw: lo.String() + ":" + hi.String(),
		}, true
	}
	ref, err := ParseRef(v)
	if err != nil {
		return zeroAnchor(), false
	}
	return anchor{Ref: ref, Raw: ref.String()}, true
}

// selSeed is the anchor/focus pair the client starts with: the linked range's
// two corners, or the anchored cell twice when the link named a single cell.
//
// The collapsed case is the one that matters. Seeding the anchor at A1 for a
// `?at=D500` load looks identical (a collapsed range draws nothing) until the
// first Shift+Arrow, which would extend from a corner the reader has never seen.
// The selection has to start on the cell the URL named, not at the origin.
func selSeed(a anchor) (lo, hi CellRef) {
	if a.Sel.On {
		return a.Sel.Lo, a.Sel.Hi
	}
	return a.Ref, a.Ref
}

// anchorViewport is the row window the anchor asks the server to render around:
// the anchored row at the top of the screen, then a screen's worth below it.
//
// It is not centred on the anchor. Centring would put half the buffer above the
// thing being linked to, and it would cost the client its trivial positioning:
// scrollTop = row * rowHeight, with no half-viewport correction that has to
// agree with the server's idea of the screen height.
//
// A tall selection extends the viewport so a modest linked range is entirely
// inside the first buffer — but only up to anchorViewportMaxRows. `A1:Z3846` is
// a legal range (ParseRangeBounds allows 100,000 cells) and honouring it
// literally would make one shared link render 100,000 cells into the first
// response. The rest of a taller range is marked as the reader scrolls into it,
// which costs nothing extra: renderRows already applies the selection to rows
// revealed by an incremental patch.
//
// `rows` must be the sheet's own allocated extent — (*Sheet).Rows(), not a
// constant. Clamping to a compile-time size would make `?at=C15000` unreachable
// on a sheet that has grown past it.
func anchorViewport(a anchor, rows int) (viewLo, viewHi int) {
	if rows < 1 {
		rows = 1
	}
	viewLo = a.Ref.Row
	if viewLo < 0 {
		viewLo = 0
	}
	if viewLo > rows-1 {
		viewLo = rows - 1
	}
	viewHi = viewLo + defaultViewportRows - 1
	if _, hi, ok := a.Sel.rows(); ok && hi > viewHi {
		viewHi = min(hi, viewLo+anchorViewportMaxRows-1)
	}
	if viewHi > rows-1 {
		viewHi = rows - 1
	}
	return viewLo, viewHi
}

// anchorViewportMaxRows bounds how far a tall selection may stretch the first
// paint. Four bands: enough that an ordinary highlighted region arrives whole,
// small enough that a pathological one cannot turn a link into a megabyte.
const anchorViewportMaxRows = 4 * BandHeight

// clampTo confines an anchor to a sheet that exists, and is the one place that
// does it.
//
// anchorViewport clamps the row window, but the `at` signal, the two selection
// seeds (`_sar`/`_sfr`), the active-cell box and the toolbar readout all come
// from the parsed anchor rather than from the window. Without this, `?at=C115000`
// on a 10,000-row sheet renders a correct container and then tells the client to
// select row 114,999 in it. The row extent is dynamic, so a link that named a
// real row can outlive it.
//
// Clamping here — where the extent becomes known, before anything is rendered
// from it — covers every consumer at once; clamping in each of them would be
// several clamps that have to agree, and the next consumer would not have one.
//
// A range clamps both corners. Clamping is monotonic, so lo <= hi survives it
// and `A9000:D115000` becomes `A9000:D9999` rather than an off-grid rectangle.
func (a anchor) clampTo(rows int) anchor {
	if a.Raw == "" {
		return a // no anchor at all; there is nothing to confine
	}
	if rows < 1 {
		rows = 1
	}
	last := rows - 1
	if a.Sel.On {
		lo, hi := clampRefTo(a.Sel.Lo, last), clampRefTo(a.Sel.Hi, last)
		return anchor{
			Ref: lo,
			Sel: selRange{Lo: lo, Hi: hi, On: true},
			Raw: lo.String() + ":" + hi.String(),
		}
	}
	ref := clampRefTo(a.Ref, last)
	return anchor{Ref: ref, Raw: ref.String()}
}

// clampRefTo pulls one reference inside the grid. The column bound is the fixed
// 26; only the row bound is a fact about this particular sheet.
func clampRefTo(r CellRef, lastRow int) CellRef {
	if r.Row < 0 {
		r.Row = 0
	}
	if r.Row > lastRow {
		r.Row = lastRow
	}
	if r.Col < 0 {
		r.Col = 0
	}
	if r.Col >= MaxCols {
		r.Col = MaxCols - 1
	}
	return r
}
