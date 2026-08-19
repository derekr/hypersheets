# sheetstream

A realtime multiplayer spreadsheet with no client-side application state.

The server renders HTML and patches it over SSE. The browser holds only what is
genuinely local — which cell is active, and whether it is being edited. Everything
else, including what every other person is doing, arrives as markup.

**Live demo:** https://hypersheets.yagni.club

> Status: prototype. It exists to test a thesis, not to replace a spreadsheet.
> See [Where it falls short](#where-it-falls-short).

## The thesis

Collaborative editing is the canonical argument for a thick client: shared mutable
state, conflicting writes, latency you cannot hide. A spreadsheet is that argument
at its strongest — a dense grid where one edit can change a thousand cells.

So: can hypermedia do it? Not "can it be made to work", but is the result actually
good — does it feel direct, and does it stay cheap when several people are in the
same sheet?

Two things had to be true, and both were measured rather than assumed:

**Scrolling must be free.** A one-band scroll costs exactly one subscribe and one
unsubscribe, independent of buffer width, and scrolling inside the buffer touches
the network not at all.

**Invalidation must follow the data, not a static list.** A viewer looking at rows
100–349 is woken by an edit to `A1` when a four-hop formula chain connects them,
and is not woken by an unrelated edit elsewhere. The dirty set is discovered during
the write and published as subjects; nobody has to declare in advance what depends
on what.

## What it costs

Measured in production, one server, ~64 concurrent connections:

| | |
|---|---|
| rendered → on the wire | 26.80 MiB → **537.8 KiB** (51×) |
| median push | **29 bytes** |
| pushes suppressed as no-ops | **47%** |
| server render, p50 | **0.35 ms** |
| commit an edit, p50 | 0.96 ms |
| page weight, first visit | 27.9 KB, zero third-party requests |
| page weight, every navigation after | 9.8 KB |

The interesting number is the 51×, and it is not compression alone: most of it is
the server noticing that a render is byte-identical to what a viewer already has
and sending nothing at all.

Latency is dominated by geography, which no amount of server work fixes. The demo
says so out loud — the top bar shows your round trip beside the server's own time,
so you can see which half is which.

## How it works

A **sheet is a SQLite database**, reached through a single writer goroutine. That
makes a sheet its own unit of concurrency, its own file, and its own failure
domain: one sheet cannot slow another down, and there is no shared table to lock.

Cells are stored under a **band-keyed layout** — a sparse key space with room
between rows — which makes inserting a row a key rewrite rather than a table
rewrite, and makes "who cares about this change" a subject-wildcard match rather
than a scan.

**Fan-out is NATS in-process.** A write publishes the bands it dirtied; every
screen subscribed to those bands re-renders its own window and diffs it against
what that viewer already holds.

**What goes on the wire is the window as it now is**, not a description of what
changed. A viewer that missed a frame cannot drift, because there is no
accumulated state to drift from.

The costs that decide the design are **data movement and round trips, not
computation** — repeatedly, an optimisation that saved CPU saved no bytes and
therefore no time. So interaction is local until it commits: selection, column
resizing and cell editing are all manipulated in the browser and committed
discretely.

### Repo map

| | |
|---|---|
| **sheet domain** | `grid` `bandkey` `formula` `recalc` `style` `store` `mutate` `actor` |
| **fan-out** | `bus` `registry` `presence` `editlog` |
| **transport & view** | `http` `render` `keys` `anchor` `window` `index` |
| **feature slices** | `styleui` `rangeops` `structure` `formulabar` `growrows` `presenceui` `latency` |
| **operations** | `limits` `reaper` `readonly` `headers` `assets` |
| **observability** | `otel` `logging` `analyze` |

One flat `package main`, deliberately. The seams above are real and acyclic, but
extracting them would mean threading an injected store through every handler — a
few days of plumbing on a working prototype, for no reader's benefit.

## Running it

```
go build -o /tmp/sheetstream . && /tmp/sheetstream -addr :8090 -data /tmp/ss
```

Then open http://localhost:8090. `-seed-rows 10000` fills a sheet with 10,000 × 26
cells (220,404 cells, 40,513 dependency edges, 5.4 MB) in about 750 ms, which is
the interesting thing to scroll around in.

`-h` lists the rest. The ones worth knowing:

| flag | |
|---|---|
| `-limits` | rate limiting, on by default |
| `-index` | whether `/` lists sheets, offers only "new sheet", or 404s |
| `-sheet-ttl` | clear sheets idle this long; the URL keeps working |
| `-readonly-sheets` | ids that refuse every write |
| `-trace-file` | OpenTelemetry spans as JSONL, readable with `-analyze` |

**A sheet's URL is its capability.** Anyone with the link can read and write it,
and there is no other way back to it. That is deliberate for a demo and is the
first thing to change for anything else.

## Where it falls short

- **Each viewer still builds its own patch.** The database read behind it is
  shared — one edit costs one read whether two people or a hundred are watching —
  but whether a cell arrives as a morph or an insert depends on what that browser
  already holds, so the markup is genuinely per viewer. Whether that justifies
  raising the connection cap has not been measured.
- 26 columns, A–Z. Growing that axis is a render-layer change, not a storage one.
- Formulas cover arithmetic, ranges and the common aggregates. It is not Excel.
- No authentication, no sharing model, no export.
- One region, so latency is what it is. See the note in `BACKLOG.md`.

## Reading further

- `SPEC.md` — what was in scope and what was not
- `DATA-MODEL.md` — storage design, and why each on-disk change happened
- `RESULTS.md` — what was measured, including the times a measurement contradicted
  the obvious reasoning
- `BACKLOG.md` — what is deliberately not built, and the constraints that would
  bite whoever builds it

## Licence

Beerware. See `LICENSE`. The vendored runtime in `vendorjs/` is third-party and
carries its own.
