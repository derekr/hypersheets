package main

// styleui.go — cell styling and number formats, from the toolbar to the store.
//
//	POST /s/{id}/style   payload: {conn, rng, set:{…}} or {conn, rng, clear:1}
//
// The store side is style.go, which holds the argument for why a style is an
// index into a per-sheet table rather than something a cell carries inline.
// This file is the half that reaches it.
//
// Four rules it inherits from the other command surfaces:
//
//  1. It posts from an element nothing else posts from (`#st`), because
//     Datastar keys request cancellation on the element. `#cl`, `#fl`, `#pv`
//     and `#ag` each have their own for the same reason.
//  2. It opts out of cancellation, joining `#cl`/`#fl`/`#pv` rather than `#ag`.
//     A style command is a write, and bold-then-italic-then-a-colour is three
//     commands at toolbar speed: under Datastar's default `'auto'` the second
//     would abort the first mid-transaction, which loses an intent rather than
//     correcting it. `#ag` keeps `auto` because it is the opposite case, a read
//     whose older answer is worthless.
//  3. The range travels as an explicit `payload`, so the selection stays out of
//     the steady-state signal set — the same bargain the clear, the fill, the
//     paste and the aggregate struck.
//  4. The per-cell path shares maxWriteCells with the clear, the fill and the
//     paste, because four caps that have to be kept in step is three too many.
//     A level command is not capped at all; see below.

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"
	"go.opentelemetry.io/otel/attribute"
)

// styleEvent carries "apply this to that range" from a toolbar control or a
// keystroke to the one element that issues `/style`. See rule 1 above.
const styleEvent = "sssty"

// styleIssuerID is that element.
const styleIssuerID = "st"

// ─── Which level of the cascade a selection means ─────────────────────────────
//
// The toolbar sends a rectangle, and a rectangle that happens to be a whole
// column is a level command: one record instead of 10,000 cell writes past
// every large-range cap. These few lines decide which it is.
//
// The rule is "does the selection cover the whole axis", which is what the
// client already produces: clicking a column header selects rows 0..rows-1 of
// that column, and clicking a row header selects columns A..Z of that row
// (T.selDown, keys.go). Ctrl/Cmd+A is both, and resolves to columns — 26
// records beat one per row, and a column style covers rows the sheet does not
// have yet, which is what "select all" means on a sheet somebody is still
// filling in.

type styleLevel int

const (
	styleLevelCell styleLevel = iota
	styleLevelCol
	styleLevelRow
)

func (l styleLevel) String() string {
	switch l {
	case styleLevelCol:
		return "col"
	case styleLevelRow:
		return "row"
	default:
		return "cell"
	}
}

// styleLevelOf decides which level a rectangle means, and returns the columns
// or rows to apply it to.
//
// The extent it compares against is read outside the actor, deliberately: it is
// resident in the band index, and it is what the client was looking at when it
// built the selection. Re-reading it inside the write turn could only change
// the answer to one the reader did not ask for — a sheet that grew in the last
// millisecond would turn "I selected the whole column" into 26,000 cell writes.
// A sheet that cannot be opened at all answers "cell".
func styleLevelOf(sheetID string, lo, hi CellRef) (styleLevel, []int) {
	sh, err := OpenSheet(sheetID)
	if err != nil {
		return styleLevelCell, nil
	}
	rows := sh.Rows()
	// `>=` and not `==`: the client clamps its selection to the extent it last
	// heard about, so a sheet that has since shrunk must still read as covered.
	if lo.Row <= 0 && hi.Row >= rows-1 && rows > 0 {
		cols := make([]int, 0, hi.Col-lo.Col+1)
		for c := lo.Col; c <= hi.Col; c++ {
			cols = append(cols, c)
		}
		return styleLevelCol, cols
	}
	if lo.Col <= 0 && hi.Col >= MaxCols-1 {
		out := make([]int, 0, hi.Row-lo.Row+1)
		for r := lo.Row; r <= hi.Row; r++ {
			out = append(out, r)
		}
		return styleLevelRow, out
	}
	return styleLevelCell, nil
}

// rangeCorners is ParseRangeBounds without its two size caps — the addresses
// only. Those caps belong to the per-cell path, which materializes a dense
// window for the rectangle, so reading the corners first is what lets "the
// whole of column D" on a 40,000-row sheet be one record instead of a refusal.
// Every other caller keeps ParseRangeBounds. Both endpoints still go through
// ParseRef, so every address is on the grid.
func rangeCorners(s string) (lo, hi CellRef, err error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return CellRef{}, CellRef{}, fmt.Errorf("%w: %q is not a range (want A1:A10)", ErrBadRef, s)
	}
	start, err := ParseRef(a)
	if err != nil {
		return CellRef{}, CellRef{}, fmt.Errorf("range %q start: %w", s, err)
	}
	end, err := ParseRef(b)
	if err != nil {
		return CellRef{}, CellRef{}, fmt.Errorf("range %q end: %w", s, err)
	}
	return CellRef{Row: min(start.Row, end.Row), Col: min(start.Col, end.Col)},
		CellRef{Row: max(start.Row, end.Row), Col: max(start.Col, end.Col)}, nil
}

// ─── The command ──────────────────────────────────────────────────────────────

// styleSignals is what a toolbar click sends, as an explicit Datastar payload
// rather than the usual sweep of every non-underscore signal.
//
// `Set` is a map and not a struct, which is the wire spelling of StylePatch's
// pointers: an absent key means "leave this alone" and an empty value means
// "reset to the default". ParseStylePatch decodes the distinction.
type styleSignals struct {
	Conn  string            `json:"conn"`
	Rng   string            `json:"rng"`
	Set   map[string]string `json:"set"`
	Clear bool              `json:"clear"`
}

// handleStyle applies a partial style patch — or a full reset — to a rectangle.
//
// It is not handleClear's loop: SetStyle is a batched upsert over the whole
// selection in one statement group, because a style is six integers wide and a
// round trip per cell would be a visible pause. There is no recalc — a style
// changes no value — so the dirty set is exactly the cells whose style id
// moved, the store returns `Dirty{Structural:false}`, and this goes down the
// ordinary pushCells path.
func (s *Server) handleStyle(w http.ResponseWriter, r *http.Request) {
	sheetID := r.PathValue("sheetID")
	if err := validSheetID(sheetID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var sig styleSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		http.Error(w, "read signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	// The endpoints are parsed before the caps are applied. ParseRangeBounds
	// refuses a rectangle wider than 100,000 cells or taller than 10,000 rows
	// because a per-cell command materializes a dense window for it, but a level
	// command materializes nothing — so the addresses are read first, the level
	// is decided, and the per-cell caps guard only the per-cell path.
	lo, hi, err := rangeCorners(sig.Rng)
	if err != nil {
		// A range this cannot read is a malformed reference, not a large one.
		s.notify(sig.Conn, styleRefusal)
		obsLog.WarnContext(r.Context(), "style.bad_range",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"range", sig.Rng, "err", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The patch is validated before the actor is taken, so a bad colour is one
	// error against the request rather than N errors discovered per cell inside
	// a transaction.
	var patch StylePatch
	if !sig.Clear {
		patch, err = ParseStylePatch(sig.Set)
		if err != nil {
			s.notify(sig.Conn, "Can’t apply that formatting: "+err.Error())
			obsLog.WarnContext(r.Context(), "style.bad_patch",
				"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
				"range", sig.Rng, "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if patch.Empty() {
			// A control that changed nothing. Answer, do not write.
			s.notify(sig.Conn, "")
			s.respondCommand(w)
			return
		}
	}

	ctx, span := tracer.Start(r.Context(), "style.command")
	defer span.End()
	started := time.Now()
	cells := (hi.Row - lo.Row + 1) * (hi.Col - lo.Col + 1)
	// Which level of the cascade this selection means. See styleLevelOf.
	level, targets := styleLevelOf(sheetID, lo, hi)
	span.SetAttributes(
		attribute.String("sheet.id", sheetID),
		attribute.String("range", lo.String()+":"+hi.String()),
		attribute.Int("range.cells", cells),
		attribute.String("style.level", level.String()),
		attribute.Bool("style.clear", sig.Clear),
		attribute.String("style.patch", patch.String()),
	)
	// The cap guards the per-cell path only: a level command writes one record
	// per column or per row.
	if level == styleLevelCell && cells > maxWriteCells {
		span.SetAttributes(attribute.Bool("refused", true))
		// A refusal is attributed as carefully as a success: without the conn,
		// the name and the selection there is no telling who hit the wall.
		obsLog.WarnContext(ctx, "style.too_big",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"range", lo.String()+":"+hi.String(), "cells", cells,
			"clear", sig.Clear, "patch", patch.String())
		s.notify(sig.Conn, styleRefusal)
		http.Error(w, "range too large", http.StatusBadRequest)
		return
	}

	var refs []CellRef
	if level == styleLevelCell {
		refs = make([]CellRef, 0, cells)
		for row := lo.Row; row <= hi.Row; row++ {
			for col := lo.Col; col <= hi.Col; col++ {
				refs = append(refs, CellRef{Row: row, Col: col})
			}
		}
	}

	var dirty Dirty
	var writeDur time.Duration
	// Styling a cell below the bottom grows the sheet, exactly as writing one
	// does, so the extent is measured across the write like every other
	// mutation here. A row-level style grows it too; a column one cannot,
	// because it names no extent at all.
	grew, err := writeSheetRows(sheetID, func(sh *Sheet) error {
		t0 := time.Now()
		defer func() { writeDur = time.Since(t0) }()
		var serr error
		switch {
		case level == styleLevelCol && sig.Clear:
			dirty, serr = sh.ClearColStyle(targets)
		case level == styleLevelCol:
			dirty, serr = sh.SetColStyle(targets, patch)
		case level == styleLevelRow && sig.Clear:
			dirty, serr = sh.ClearRowStyle(targets)
		case level == styleLevelRow:
			dirty, serr = sh.SetRowStyle(targets, patch)
		case sig.Clear:
			dirty, serr = sh.ClearStyle(refs)
		default:
			dirty, serr = sh.SetStyle(refs, patch)
		}
		return serr
	})
	if err != nil {
		span.RecordError(err)
		status := commandStatus(err)
		if errors.Is(err, ErrBadStyle) {
			status = http.StatusBadRequest
		}
		s.notify(sig.Conn, humanCommandError("Formatting failed", err))
		obsLog.WarnContext(ctx, "style.failed",
			"sheet", sheetID, "conn", sig.Conn, "name", s.authorName(sig.Conn),
			"range", lo.String()+":"+hi.String(), "err", err.Error())
		http.Error(w, err.Error(), status)
		return
	}

	// The fan-out has two shapes because the store returns two.
	//
	// A cell style returns `Dirty{Cells, Bands}` — the ordinary per-cell patch
	// an edit produces — so it goes down the ordinary path: one edit-log entry,
	// one publish, and every viewer patches only the cells it holds. The
	// stylesheet rides out on the same push (patchStyles) rather than being
	// published separately, which makes creating a look ~50 bytes plus the
	// cells.
	//
	// A level style returns `Dirty{Bands, Config:true}` with no dirty cells,
	// because no cell changed — that is the entire saving. There is nothing for
	// pushCells to patch, so it takes the shape a column width has: mark every
	// screen on the sheet, let the per-screen stylesheet compare restate the
	// rules, and re-render. The re-render is not decoration — a level can carry
	// a number format, and a currency column changes the text of every cell in
	// it, which no CSS can do.
	var editSeq uint64
	var woke int
	if level == styleLevelCell {
		editSeq = s.publishRange(ctx, sheetID, sig.Conn, dirty.Cells, grew)
	} else {
		woke = s.markSheetFull(ctx, sheetID)
		s.extentChanged(ctx, sheetID, grew)
		if s.bus != nil && len(dirty.Bands) > 0 {
			if perr := s.bus.Publish(sheetID, dirty.Bands); perr != nil {
				span.RecordError(perr)
			}
		}
		if woke == 0 {
			// Nobody to wake, so nothing will take this screen's chip down.
			s.notify(sig.Conn, "")
		}
	}
	span.SetAttributes(
		attribute.Int("dirty_cells", len(dirty.Cells)),
		attribute.Int("dirty_bands", len(dirty.Bands)),
		attribute.Bool("dirty.config", dirty.Config),
		attribute.Int("level.targets", len(targets)),
		attribute.Int("screens", woke),
		attribute.Int64("edit.seq", int64(editSeq)),
		attribute.Float64("duration_ms", msf(time.Since(started))),
	)
	obsLog.InfoContext(ctx, "style",
		"sheet", sheetID,
		"conn", sig.Conn,
		"name", s.authorName(sig.Conn),
		"range", lo.String()+":"+hi.String(),
		// `style_level` and not `level`: slog's own severity key is "level", and
		// two attributes of that name make one JSON object with a duplicate key,
		// which every reader resolves differently.
		"style_level", level.String(),
		"targets", len(targets),
		"screens", woke,
		"cells", cells,
		"clear", sig.Clear,
		"patch", patch.String(),
		"dirty_cells", len(dirty.Cells),
		"dirty_bands", len(dirty.Bands),
		"write_ms", msf(writeDur),
		"command_ms", msf(noteCommandNow(started)))

	s.respondCommand(w)
}

// styleRefusal names the limit and the way out. A whole column or a whole row
// is one record at the level above the cell and is not capped, so this refusal
// is only ever about a rectangle in the middle of a sheet.
var styleRefusal = "Can’t format more than " + strconv.Itoa(maxWriteCells) +
	" cells at once — select the whole column or row (click its header) to " +
	"format all of it in one go."

// ─── The client half ──────────────────────────────────────────────────────────

// stylePost is the one style command in the page, where a toolbar click becomes
// a request.
//
// The range and the toggle are resolved here rather than in the buttons, for
// bytes. Only a Datastar expression can read signals (there is no global
// for them), so every control needing `$_sar…$_sfc` and `$ref` would carry the
// whole range expression in its own attribute — ~180 bytes x 18 controls of
// identical page-shell markup. Resolved here, each control says only what it
// means (`{set:{align:'left'}}`), and there is one copy of the rule "the
// toolbar applies to the selection, or to the active cell when there is none".
func stylePost(sheetID string) string {
	return `if(!evt.detail||!window.__ss||$ref==='')return;` +
		`const d=evt.detail,g=` + styleRangeExpr + `;let st=d.set||{};` +
		// The flip is decided against the active cell, which is Sheets' rule:
		// bolding a mixed range makes all of it bold rather than inverting each
		// cell. See T.tog for why it reads getComputedStyle.
		`if(d.tog){st={};st[d.tog]=window.__ss.tog(d.tog,$ref)?'1':'0'}` +
		pendingRaise(`Formatting…`) +
		`@post('/s/` + sheetID + `/style',{payload:{conn:$conn,rng:g,set:st,clear:!!d.clear},` +
		`requestCancellation:'disabled'})`
}

// styleRangeExpr is what a toolbar control applies to: the selected rectangle,
// or the active cell as a one-cell range when nothing is selected. The `||`
// fallback is the one Ctrl+C uses, and for the same reason — it gives the
// command one shape instead of two, so the server never has to know whether the
// user had a range or a cell.
const styleRangeExpr = `(window.__ss.selRng($_sar,$_sac,$_sfr,$_sfc)||($ref+':'+$ref))`

// styleDispatch is a control's whole handler. `detail` is one of three shapes
// and `#st` turns each into the same command:
//
//	{set:{…}}    these fields, absent ones left alone (StylePatch's pointers)
//	{tog:'bold'} flip whatever the active cell has
//	{clear:1}    ClearStyle, a different verb: it also deletes rows it leaves
//	             holding nothing, giving an over-styled blank region its rows
//	             back
func styleDispatch(detail string) string {
	return `window.__ss&&window.__ss.sy(` + detail + `)`
}

// styleToolbarHTML is the whole toolbar: three toggles, two colour pickers with
// quick swatches, three alignments, seven number formats and a reset.
//
// All of it is page shell, written once per load and re-sent by nothing — the
// same terms `#sb`, `#cb` and the aggregate line are on. That is the only
// reason a feature with this many controls is affordable here.
func styleToolbarHTML() string {
	var b strings.Builder
	b.Grow(4096)
	b.WriteString(`<span class="tb">`)
	b.WriteString(`<button class="tg" title="Bold (Ctrl/Cmd+B)" aria-label="bold" data-on:click="` +
		styleDispatch(`{tog:'bold'}`) + `"><b>B</b></button>`)
	b.WriteString(`<button class="tg" title="Italic (Ctrl/Cmd+I)" aria-label="italic" data-on:click="` +
		styleDispatch(`{tog:'italic'}`) + `"><i>I</i></button>`)

	// The colour control is `<input type="color">` and not a palette: the store
	// accepts any `#rgb`/`#rrggbb` and normalizes it, so a fixed set would be
	// the interface refusing something the model supports. The native picker
	// brings its own presets, recents and eyedropper for zero bytes and returns
	// exactly the `#rrggbb` the store wants. The swatches beside it are the fast
	// path, not a substitute.
	b.WriteString(`<span class="cg" title="Text colour">A<input class="cp" type="color" value="#000000"` +
		` aria-label="text colour" data-on:change="` + styleDispatch(`{set:{fg:el.value}}`) + `"></span>`)
	writeSwatches(&b, "fg", []string{"#000000", "#cc0000", "#188038", "#1a73e8"})
	b.WriteString(`<span class="cg" title="Fill colour">▨<input class="cp" type="color" value="#fff2cc"` +
		` aria-label="fill colour" data-on:change="` + styleDispatch(`{set:{bg:el.value}}`) + `"></span>`)
	// The first fill swatch is "no fill" — an empty value, the wire spelling of
	// "reset this field to the default" and the only way back to a transparent
	// cell once one has a background.
	writeSwatches(&b, "bg", []string{"", "#fff2cc", "#d9ead3", "#d0e2f3"})

	for _, a := range []struct{ op, glyph, label string }{
		{"left", "⇤", "align left"},
		{"center", "↔", "align centre"},
		{"right", "⇥", "align right"},
	} {
		b.WriteString(`<button class="tg" title="Align ` + a.label[6:] + `" aria-label="` + a.label +
			`" data-on:click="` + styleDispatch(`{set:{align:'`+a.op+`'}}`) + `">` + a.glyph + `</button>`)
	}

	// Wrap sits with the alignments because that is what it is — where the text
	// breaks — and because it is the only formatting control whose effect is a
	// row too short to show its own contents. Fit-to-contents on the gutter is
	// the other half of the gesture.
	b.WriteString(`<button class="tg" title="Wrap text" aria-label="wrap text" data-on:click="` +
		styleDispatch(`{tog:'wrap'}`) + `">⤶</button>`)

	// A `<select>` because the format set is closed and seven long. It is the one
	// control that takes keyboard focus, which is why the grid's `__window`
	// keymap has a SELECT case in its text-entry guard — without it an arrow key
	// would move the selection and change the format at once.
	b.WriteString(`<select class="fs" aria-label="number format" data-on:change="` +
		styleDispatch(`{set:{fmt:el.value}}`) + `">`)
	for _, f := range []struct{ v, label string }{
		{"plain", "123 Plain"},
		{"integer", "1,235"},
		{"2dp", "1,234.50"},
		{"currency", "$1,234.50"},
		{"percent", "12.34%"},
		{"date", "Date"},
		{"datetime", "Date time"},
	} {
		b.WriteString(`<option value="` + f.v + `">` + f.label + `</option>`)
	}
	b.WriteString(`</select>`)

	b.WriteString(`<button class="tg" title="Clear formatting" aria-label="clear formatting"` +
		` data-on:click="` + styleDispatch(`{clear:1}`) + `">⌫</button>`)
	b.WriteString(`</span>`)
	return b.String()
}

// writeSwatches emits the quick-access colours for one field. A swatch is a
// button whose own background is its value, so it needs no label and no
// generated CSS rule — the one inline style in this file, and it carries data
// (which colour) rather than presentation.
func writeSwatches(b *strings.Builder, field string, colors []string) {
	for _, c := range colors {
		style, title := `background:`+c, c
		if c == "" {
			// "No fill", drawn as a crossed-out box rather than as nothing at
			// all, because an invisible button is not a button.
			style, title = `background:#fff`, "none"
		}
		b.WriteString(`<button class="sw`)
		if c == "" {
			b.WriteString(` n`)
		}
		b.WriteString(`" style="` + style + `" title="` + title + `" aria-label="` + field + ` ` + title +
			`" data-on:click="` + styleDispatch(`{set:{`+field+`:'`+c+`'}}`) + `"></button>`)
	}
}

// styleIssuerHTML is the element that owns the request. See rules 1 and 2.
func styleIssuerHTML(sheetID string) string {
	return `<div id="` + styleIssuerID + `" hidden data-on:` + styleEvent + `__window="` +
		// `pw` is the pending chip's backstop marker — see chipFailScript.
		stylePost(sheetID) + `" class="` + pendingWriteCl + `"></div>`
}

// styleScript is the client's half, hung off the same `window.__ss` object as
// everything else. gridKeysScript calls it, so it cannot run before the object
// exists. What the three helpers are for:
//
//	scls(el)      the style class on an element, or ''. The local echo rewrites
//	              className to say what kind of cell it just painted, and without
//	              this that rewrite would strip the style — committing into a
//	              bold red cell would flash it plain for the round trip, which is
//	              the flicker the echo exists to remove. It scans for `s<digits>`
//	              rather than keeping a saved copy because the confirming morph
//	              owns the class, and reading it back is how the echo stays a
//	              follower.
//	sy(detail)    dispatch a style command, so eighteen toolbar controls and two
//	              keystrokes name one event instead of repeating the dispatch.
//	tog(k,ref)    the value bold/italic/wrap should take at the active cell: true when
//	              it is not currently on. It reads getComputedStyle — the
//	              browser's own resolution of the rules the server generated —
//	              rather than parsing the class and modelling the cascade a
//	              second time. A cell with no element at all (empty and unstyled,
//	              which is most of a sheet) reads as off, so the first press
//	              turns it on.
func styleScript() string {
	return `T.scls=function(e){if(!e||!e.className)return '';
 var c=e.className.split(' ');
 for(var i=0;i<c.length;i++)if(/^s[0-9]+$/.test(c[i]))return c[i];
 return '';};
T.tog=function(k,ref){var e=document.getElementById(ref);if(!e)return true;
 var s=getComputedStyle(e);
 if(k==='bold')return !(+s.fontWeight>=600);
 if(k==='wrap')return s.whiteSpace!=='pre-wrap';
 return s.fontStyle!=='italic';};
T.sy=function(o){window.dispatchEvent(new CustomEvent('` + styleEvent + `',{detail:o}));};
`
}
