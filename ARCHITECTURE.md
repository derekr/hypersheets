# How this works

A multiplayer spreadsheet where the browser runs no application code. The server
owns every cell, every selection and every pixel of layout; the browser holds a
DOM and an SSE connection. Typing in a cell posts a command; the server writes
it, works out who is looking at that region, and pushes each of them the part of
their screen that changed.

That one sentence is the whole design, and most of what follows is a consequence
of it.

## Read in this order

Eight stops, about 4,000 lines — a sixth of the code — and you will be able to
place the rest.

| # | file | what it teaches |
| --- | --- | --- |
| 1 | `grid.go` | what a cell is, and how `A1` becomes coordinates |
| 2 | `bandkey.go` | why a row's **position is not its identity** — the idea the storage turns on |
| 3 | `store.go` § *Storage model*, § *Reads* | the schema, and why a window read is one contiguous scan |
| 4 | `actor.go` | one goroutine per sheet: the single place a write can happen |
| 5 | `recalc.go` | the dirty set is discovered *during* the write, not after it |
| 6 | `render.go` § *Page shell* | sparse rendering — why an empty cell costs nothing |
| 7 | `screen.go` | one held-open connection's view of a sheet |
| 8 | `push.go` | a woken screen becomes bytes |

Then read `growrows.go` (161 lines). It is one complete feature — storage,
policy, HTML, client behaviour, HTTP handler — and it is the shape almost every
other feature in here takes.

## The path of one edit

Follow a keystroke end to end and the file list stops being a list.

```
  browser        POST /s/{id}/cell            keys.go        what the keyboard means
     |                                        http.go        handleCell
     v
  limits.go      classify, rate-limit, refuse loudly if refusing
     |
     v
  actor.go       Actors.exec — THE write chokepoint. One goroutine per sheet,
     |           so a write never races another write on the same sheet.
     v
  store.go       WriteCell in one transaction
  recalc.go        ...which discovers the dirty set as it goes
  editlog.go       ...and records which cells changed, by sequence number
     |
     v
  bus.go         publish "sheet X, rows lo..hi are dirty"
     |
     v
  registry.go    which connections are looking at rows lo..hi?
     |           (nobody looking = nothing rendered, and that is the point)
     v
  screen.go      per connection: what does THIS viewer have on screen now?
     |
  windowcache.go one read serves every viewer woken by the same edit
     |           (32 viewers used to mean 32 database reads; now it means 1)
     v
  render.go      cells -> HTML for the changed region only
  push.go        diff against what this screen last had, brotli, write to SSE
     |
     v
  browser        Datastar morphs the fragment in. No client state to reconcile,
                 because the client never had any.
```

## The domains

Measured, not asserted — `spike/layout` type-checks the package and reports the
real reference graph. Arrows point the way dependencies actually run, and the
number is how many distinct symbols cross.

```
              ┌──────────────────────────── boot ─────────┐   main.go: flags, start order
              v            v              v            v
   ┌──────► web ──93──► live ──36──► view ──41──► sheet     the way dependencies run
   │         │ 125         │ 18         │
   │         └─────────────┴────────────┘
   │                                          ...and the four edges that run back:
   └── view ──23──► web        live ──15──► web
       sheet ──4──► view       obs ──4──► live
```

| domain | files | what it is |
| --- | ---: | --- |
| **sheet** | 10 | the model. Cells, band keys, formulas, styles, and the actor that writes them. Knows nothing about HTTP. |
| **view** | 6 | cells in, HTML and client script out. No request, no connection, no I/O. |
| **live** | 6 | who is watching, what woke them, what goes on the wire. |
| **web** | 13 | routes, commands, policy. Everything that starts from a request. |
| **obs** | 3 | traces, logs, and the tool that reads them back. |

The forward edges are a clean descent: requests reach connections reach HTML
reaches the model. The four backward edges are the whole story of why this is one
package, and they are small enough to name:

| backward edge | what it is |
| --- | --- |
| `view → web` (23) | the page shell asks each feature slice for its fragment |
| `live → web` (15) | `push.go` reaches the server for latency and patch helpers |
| `sheet → view` (4) | `rowHeightPx` and the window range types, filed on the wrong side |
| `obs → live` (4) | `screenObs` knows what a `screen` is |

**These are not Go packages, and that is deliberate.** See below.

## Why one flat package

The obvious next step — cut those five domains into `internal/` packages and let
the compiler enforce them — does not work here, and the reason is worth
understanding because it is the organising principle of the codebase.

**Most files are feature slices.** `growrows.go` holds the CSS, the HTML, the
client script, the signals, the policy and the HTTP handler for "Add more rows
at bottom". So does `latency.go` for the latency chip, `styleui.go` for the
toolbar, `presenceui.go` for other people's cursors, `anchor.go` for linkable
regions. Each is one feature you can read start to finish and then close.

That is exactly what makes them unpackageable. The page shell in `render.go`
calls into each slice for its fragment, and each slice calls back into the
server for its handler — so `view → web` and `web → view` are both real, by
design. Splitting them into layers would mean cutting every feature into three
pieces filed in three directories, which trades the property that makes them
readable for a directory tree.

So: **slices are the unit, and the layering above is a reading aid rather than a
boundary.** If you want the boundary enforced, `spike/layout` is what enforces it
— run it and it will tell you what moved.

## The five ideas everything else follows from

**A row's position is not its identity.** A cell is stored under a *band key*
(`bandkey.go`), not a row number, so inserting a row renumbers nothing. Display
rank is computed; the key is permanent. Nearly every hard problem in the store —
inserts, deletes, formula references surviving both — dissolves into this one
idea, and the schema (`WITHOUT ROWID`, clustered on `(k, col)`) exists so a
window read is one contiguous b-tree scan.

**One writer per sheet.** `Actors.exec` is the only place a sheet is mutated.
Not a lock — a goroutine — so ordering is a property of the queue rather than of
discipline. Readers do not go through it: WAL readers run concurrently with the
writer, which is why a push never waits on an edit.

**Empty cells cost nothing.** The grid is 26 columns by up to a million rows and
almost all of it is empty. Nothing empty is ever rendered. Layout that would be
per-cell — column widths, row heights, styles, backgrounds — is CSS custom
properties and per-row rules instead, so the markup is O(cells that exist) while
the geometry is O(things somebody changed).

**The client owns exactly one thing: the caret.** Every push re-renders the
buffer, and morphing an element that holds focus would destroy an in-progress
edit. So one `<textarea>` floats over the active cell wearing `data-ignore-morph`
and every other cell is inert. That is also why removing an element is dangerous
here — it aborts that element's in-flight request — and why commands are issued
from page-shell markup no push replaces.

**Refusals are loud.** A demo on the open internet has to say no a lot: rate
limits, disk caps, oversize ranges, read-only sheets. Every refusal has words a
person can read, and every command that fails surfaces something. A silent
failure in this system is a bug in its own right — `pxnum.go` exists because of
one.

## Where to find things

| looking for | start at |
| --- | --- |
| what a cell is, `A1` parsing | `grid.go` |
| the schema, migrations | `store.go` |
| why inserting a row is cheap | `bandkey.go`, `mutate.go` |
| formulas and recalculation | `formula.go`, `recalc.go` |
| styling, number formats, the cascade | `style.go` (model), `styleui.go` (toolbar) |
| row heights, wrapping, fit-to-contents | `rowheight.go`, `render.go` |
| the keyboard | `keys.go` |
| selection, copy, paste, fill | `rangeops.go` |
| other people's cursors | `presence.go`, `presenceui.go` |
| the SSE stream | `http.go` § *GET /s/{id}/live*, `screen.go` |
| rate limits and abuse policy | `limits.go` |
| what gets measured | `otel.go`, `analyze.go`, `RESULTS.md` |

## The other documents

- `SPEC.md` — what it is supposed to do.
- `DATA-MODEL.md` — the storage design and the changes it has been through.
- `RESULTS.md` — measurements. Several of them contradict the reasoning that
  preceded them, which is why they are written down.
- `BACKLOG.md` — decisions already reasoned through and not built, each with the
  constraint that makes it non-obvious.
- `SPIKE-*.md` — investigations, including the ones whose answer was no.
