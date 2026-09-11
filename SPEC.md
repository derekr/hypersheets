# hypersheets — a realtime spreadsheet on hypermedia + light client state

A prototype to test one hypothesis:

> A Google-Sheets-like collaborative experience is achievable with server-rendered
> hypermedia (Datastar fat morph over SSE) plus only *light* client state — active
> cell, edit buffer, scroll offset, selection — with no client-side formula engine,
> no OT/CRDT, and no client replica of the data.

This is a **risk-first prototype**. It builds the two things identified as hard and
stubs everything else. It is not a product.

## The two hard parts (this is the entire point)

**1. Scrolling fights "a subscription's parameters are fixed for its lifetime."**
Scroll changes which rows you're subscribed to, but it can't be a navigation (60Hz, no
history entries) and it mustn't be a command per scroll event (round trip on the one
interaction where lag is most felt). The answer under test: **over-fetch a buffer**
(2-3 screens), scroll natively inside it with zero network, and issue a viewport
command only when approaching the buffer edge.

**2. Dependency-driven invalidation breaks static `invalidates`.**
Editing `A1` cascades to `B1=A1*2` to `C1=SUM(A1:A10)`. The affected set is discovered
*during* the write by walking the dependency graph, so a command cannot declare its
topics up front. The answer under test: **the handler returns a dirty set**, and the
dispatcher converts dirty cells → bands → subjects.

## Explicitly OUT of scope

Auth, users, sheet CRUD, undo UI, charts, cross-sheet formulas (`=Other!A1` — disallowed
on purpose; they turn one read into a distributed read), rich formatting, column resize,
insert/delete row-or-column (the large-range worst case — noted, not built).

## Architecture decisions (already made — do not relitigate)

- **Go**, `package main`, flat file layout in the repo root. Mirrors
  `../nats-datastar-agents`, which is the reference for house style.
- **Embedded NATS, core pub/sub only.** No JetStream. The bus carries a subject and
  nothing else — no payload, so it is idempotent, unordered and loss-tolerant. NATS is
  the *fan-out*, not the log.
  - NATS subject wildcards do the lineage matching, which is why there is no
    prefix-chain publishing and no two-index registry: publish the leaf
    `sheet.{id}.band.{n}`, subscribe `sheet.{id}.>` or a specific band.
- **SQLite per sheet is the system of record**, one file per sheet. The event log lives
  *inside* the same database so a mutation and its event commit in one transaction.
  - Driver: `modernc.org/sqlite` (pure Go, no cgo, easy cross-compile). Swappable.
  - WAL mode, `synchronous=NORMAL`.
- **One goroutine per sheet (an actor) owns writes.** This serializes edits, which is
  what buys a total order without OT, and removes lock contention between sheets.
- **An LRU of open sheet handles with idle eviction** — thousands of sheets must not
  mean thousands of open file handles.
- **Datastar** for the client. The only client state permitted is: active cell, edit
  buffer, scroll offset, selection anchor. Anything else is a bug against the thesis.

## Grid model

- Sheet: 26 columns (A..Z) and a row extent that **grows on demand** — a sheet starts
  10,000 rows tall and extends when an insert or a write needs rows it does not have,
  shrinking again on delete down to that floor. A hard ceiling of 1,000,000 rows exists
  as a sanity bound, not a design limit. There are two extents and they are different
  numbers: **allocated** (`(*Sheet).Rows()`, what sizes the scroll container) and **used**
  (`(*Sheet).UsedRows()`, the last row holding data). Seeded with a mix of literals and
  formulas.
- **Band** = 50 rows. `band(row) = row / 50`. Bands are the invalidation and
  subscription granularity.
- **Viewport** = the rows actually on screen (~40).
- **Buffer** = viewport expanded by `BUFFER_BANDS` (default 2) on each side. The server
  renders the buffer; the client scrolls inside it without touching the network.

## Formulas (deliberately minimal — just enough to exercise the dependency graph)

Supported cell contents:
- a literal number, or a literal string
- `=A1*2`, `=A1+B2` — single binary op on two refs/literals
- `=SUM(A1:A10)` — one range

That is enough for multi-hop cascades (`C1=SUM(A1:A10)`, `D1=C1*2`) which is all the
prototype needs. **Do not build a real expression parser.** Cycles must be detected and
reported as `#CYCLE!` rather than hanging.

## Contracts

```go
// A dirty set is what a write returns. This is hard part #2.
type Dirty struct {
    Cells []CellRef // cells whose computed value changed
    Bands []int     // derived from Cells; the subjects to publish
}

// Subjects
//   sheet.{sheetID}.band.{n}     published on any change within that band
// A connection subscribes to exactly the bands its BUFFER covers, and resubscribes
// when the buffer moves.
```

### HTTP surface

| route | purpose |
| --- | --- |
| `GET /s/{sheetID}` | the page: shell + initial buffer render + SSE connect. `?at=D500` / `?at=A1:D20` anchors the buffer on a linkable region (a query parameter, not a fragment, so the first paint is already correct). |
| `GET /s/{sheetID}/live` | held-open Datastar SSE; one connection per screen |
| `POST /s/{sheetID}/cell` | commit a cell edit (signals: `ref`, `raw`) |
| `POST /s/{sheetID}/viewport` | client reports a new buffer window (debounced, edge-triggered only) |

### The morph hole — non-negotiable

The **actively-edited cell must be excluded from the morph.** Morphing a cell that has
focus or an active IME composition destroys the caret, the selection, or the
composition. The cell being edited is client-owned territory until commit. Use stable
per-cell ids and `data-preserve`-style exclusion. Test with a real keyboard, and test
with a multi-keystroke IME composition if you can.

Related known hazard: removing an element aborts its in-flight request. A morph must not
yank a cell that has a command in flight.

## What must be measured (the deliverable)

1. **Bytes on the wire per committed edit**, per viewer — brotli'd. Compare a 1-cell
   literal edit against a `SUM` cascade touching 3 bands.
2. **Renders per edit** across N viewers, and the **digest suppression rate** — a viewer
   on band 0 when band 180 changes must cost zero bytes.
3. **Scroll**: does scrolling inside the buffer issue zero requests? What is the p95
   time to fill when you cross a buffer edge? At `LATENCY_MS` 0/40/90/180.
4. **Fan-out cost**: 1, 10, 50 viewers on one hot sheet, one edit per second. CPU and
   bytes. Where does it fall over?
5. **Caret survival**: type into a cell while another viewer edits a neighbouring cell in
   the same band. The caret must not move and the composition must not break.

Report real numbers. Never fabricate. If something did not run, say so.

## House style

- Follow `../nats-datastar-agents`: flat `package main`, an `newSSE` helper wrapping
  `datastar.NewSSE(... WithCompression())`, `datastar.ReadSignals(r, &signals)`.
- `go vet ./...` and `gofmt` clean. Table-driven tests with the stdlib `testing` package.
- No dependency beyond: `nats-server/v2`, `nats.go`, `datastar-go`, `modernc.org/sqlite`.
