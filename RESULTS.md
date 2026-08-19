# sheetstream — measured results

Prototype status: **both hard parts work.** The thesis survives, with one concrete
change required before this scales past a handful of viewers.

Run: `go build -o /tmp/sheetstream . && /tmp/sheetstream -addr :8090 -data /tmp/ss`
Seeds 10,000 x 26 (220,404 cells, 40,513 dep edges, 5.4 MB) in ~750ms.

## Hard part 1 — scrolling is cheap. CONFIRMED.

A one-band scroll costs **exactly 1 subscribe + 1 unsubscribe, independent of buffer
width.** Live subscription count stays flat (5 with a 5-band buffer) across a scroll.
Verified for scroll up/down, buffer grow/shrink, overlapping jumps (3/3) and disjoint
jumps (5/5 — the correct floor, no overlap to preserve). Proven live, not bookkeeping:
after the move, a publish to the dropped band no longer wakes the connection and one to
the added band does.

Over-fetching a buffer is the right answer. Scrolling inside it touches nothing.

## Hard part 2 — dependency-driven invalidation. CONFIRMED.

A viewer buffered on **bands 2–6 (rows 100–349), not subscribed to band 0**, was woken
by an edit to `A1` — the 4-hop chain `A1 → Y1 → Y51 → Y101 → Y151 → Y201`. An unrelated
literal edit (`T9500`) produced **zero** additional frames for that viewer.

So a dirty set discovered *during* the write, converted to bands, published as subjects,
reaches exactly the right subscribers and nobody else. This is the thing a statically
declared invalidation list cannot express, and it works.

## Targeting and suppression

| case | near viewer (bands 0–4) | far viewer (band ~180) |
| --- | --- | --- |
| first paint | 223.1 KiB | 304.4 KiB |
| edit in far band | 0 B | (its own band) |
| edit `A1`, 5-band cascade | +223.1 KiB, 1 frame | **0 B, 0 frames** |
| edit `A1` again, same value | **0 B** | **0 B** |

A no-op edit costs zero on the wire — the changed-value-only rule in recalc means the
dirty set is empty, so nothing is even published. Subscription scope does the primary
work; digest suppression is the backstop.

## Patch granularity — the finding, and the fix, measured

The finding below stood: a fat morph shipped the whole buffer whatever changed.
Both paths now send only what changed. Measured in a real headless Chrome over
CDP (`/tmp/ssobs/`), same machine, same seeded sheet, same driving script,
before-binary against after-binary.

### Edits — the dirty set is now the patch

Identical edits, driven through the real UI (click cell, type, Enter), reading
the `datastar-patch-elements` payload the browser actually received:

| edit | before | after |
| --- | --- | --- |
| `A1=424242` — 11-cell, 5-band cascade | 294,500 B / 12,153 elements | **543 B / 11 elements** |
| `A1=5` — same cascade | 294,469 B | **512 B** |
| `D7=hello` — one literal | 294,457 B | **32 B** |
| `D7==A1*2` — literal becomes formula | 294,469 B | **44 B** |
| `T9500` — outside this viewer's buffer | 0 frames | **0 frames** |

542x on the cascade, 9,200x on the single-cell edit. Post-brotli, the cascade is
119 B and the literal 29 B on the wire.

One regression, honestly: **a repeated no-op edit went from 0 B to 23 B.** The
edited cell is unconditionally added to the dirty set (`withRef` in http.go)
because a formula cell's `data-r` can change while its computed value does not,
so it patches one `<td>` where the old full-morph digest suppressed everything.
Comparing raw as well as computed in recalc's changed-value test would restore
the zero; that is a one-line change to the recalc engine, deliberately not made
here.

### Scrolling — the buffer delta is now the patch

`SetBuffer` always returned `added`/`dropped` band counts and the render path
threw them away. It now emits `remove` for the rows that fell off and
`append`/`prepend` for the rows revealed, falling back to a full morph only when
the new window shares no rows with the old one (a scrollbar drag to the far end).

Per push, from the trace (`sse.patch · bytes_raw`):

| buffer move | before | after |
| --- | --- | --- |
| one band (50 rows) | 195,341 B | **32,985 B** |
| four bands (200 rows) | 330,738 B | **133,188 B** |
| shrink only, no new rows | 162,356 B | **1,199 B** (a selector, no HTML) |

`html.render · cells` p50 11,700 -> 5,200 over the same driving script; the
one-band case is 7,800 -> 1,300. Path mix over a full harness run: 100%
incremental after first paint, no disjoint fallbacks; a deliberate scrollbar
jump to row 4,500 takes the fallback and lands correctly.

**On the wire the scroll change is nearly free, and that is the interesting
part.** Compressed, a 200-row slide costs 12,450 B after versus 12,554 B before.
Brotli wraps the stream once, so its window already deduplicated the retained
rows — the full morph was mostly a near-copy of the previous one. The saving is
in server work (rows read, HTML built) and in DOM work, not in bytes. Client
`morphMs` p50 moved 31.8 -> 21.6 ms on a human-paced scroll and is unchanged on
a continuous 2-second drag, where each push legitimately moves 200 rows.

Conclusion: **for scrolling, over-fetching plus streaming compression was
already close to optimal on the wire; incremental patching buys CPU. For edits,
it is the difference between 294 KB and 543 bytes.**

### What it cost structurally

- The buffer bounds had to stop being `data-lo`/`data-hi` on `#g`. They were
  only ever current because every push re-morphed the wrapper; once a scroll
  stops touching it they freeze, and the edge test dies silently. They are now
  server-owned `blo`/`bhi` signals, distinct from the client-owned `lo`/`hi`
  the scroll handler writes as its request.
- Digest suppression had to be turned off for scroll pushes. "Identical bytes
  mean identical DOM" is true of a whole window and false of a delta: two
  scrolls landing on the same bands emit the same patch after the client has
  moved a long way in between.
- The wake had to grow a memory. Wakes coalesce, so two edits between one
  connection's renders arrive as one wake; a single "last dirty set" would drop
  the first edit's cells forever. `editlog.go` is a bounded per-sheet history
  and each screen tracks the sequence it has applied. Falling off the back of
  the ring degrades to a full morph, never to silence.

## Linkable regions — a place in the sheet is a URL

`GET /s/{id}?at=D500` and `?at=A1:D20`. A QUERY PARAMETER, not a fragment: a
fragment never reaches the server, so `#D500` would render row 0 and then jump,
which throws away the one thing this architecture is good at. `?at=` is in the
request line, so the first paint is already the right buffer.

Measured in a real headless Chrome over CDP (`/tmp/ssat/`), same seeded sheet:

| check | result |
| --- | --- |
| `?at=D500` server-rendered rows (raw HTML, pre-JS) | `<tr id="r251">`..`<tr id="r750">`, `--lo:250`, `<td id="D500">490</td>` |
| scroll position on load | `scrollTop` 10,978 (= 499 x 22) in **every** sample from the first rAF — no jump |
| `?at=A1:D20` | exactly 80 `<td class="s">`, A1..D20, +792 B over the unmarked page |
| viewport commands caused by the anchored load | **0 posts, 0 skips, 1 scroll event** — the settle guard swallowed the synthetic scroll |
| a real wheel tick immediately after that load | handled (skips 0 -> 1) — the guard did not swallow it |
| `?at=zzz` | 200, top of the sheet, no marks, no console errors |
| 400 wheel ticks of scrolling | `history.length` **10 -> 10**, URL tracked to `?at=A1619` then `?at=A1` |
| two go-to jumps | `history.length` 10 -> 11 -> 12, **+1 each** |
| back, back, forward, forward | scrollTop, buffer rows and the marks all follow the URL |
| an edit inside the region | 76 B frame, `B2` keeps `class="s"` |

**The URL follows the scroll with `replaceState` and a jump with `pushState`.**
That split is the whole feature: 400 wheel ticks add zero history entries, and
one go-to adds exactly one.

Existing behaviour, re-measured on the same build: one-band scroll patch
**32,985 B**, shrink-only **1,199 B**, full first paint **294,506 B** — all
unchanged from the numbers above. Path mix over a scroll harness run: 100%
incremental after first paint, no disjoint fallbacks. Edit patches: `D7=hello`
32 B, `D7==A1*2` 44 B, no-op repeat 23 B — unchanged. The `A1` cascade reads 546 B
against the 543 B above; it is the same 11 elements and the 3 bytes are the
cells' current values, not markup (an unselected `<td>` is asserted
byte-identical to the pre-anchor rendering by test).

### What it cost structurally

- **The selection is server state for the render.** A range is not marked by the
  client, because rows revealed by a later scroll patch have to arrive already
  marked — `renderRows` takes the range, so a 1,201-row region keeps marking
  itself as the reader scrolls into it. Directly observed: an `append` patch
  starting at `r3601` carried 600 marked cells.
- **It costs only the cells it covers.** A per-cell `data-sel` on all 13,000
  cells would spend ~150 KB to express ~900 bytes of fact; a class on the 80
  selected cells costs 792 B and leaves every other cell byte-identical, which
  is what keeps the digest and every number above valid.
- **A region change forces a full morph.** A jump can change the range without
  moving the buffer, and an incremental patch only ships rows the diff revealed
  — so the cells that must LOSE the class are in no range it knows about. This
  is the one place the linkable-region idea genuinely fights the incremental
  design, and the honest fix is to stop being incremental: `sel != heldSel`
  falls through to the window morph.
- **`@post` from `#live` kills the stream.** popstate first hung off the
  `#live` div, the obvious place — it is outside every patch region. Datastar's
  fetch actions default to `requestCancellation:"auto"`, which in the v1.0.1
  bundle is `Vt.get(el)?.abort()` keyed on the ELEMENT, so the first back button
  press aborted that element's in-flight request: the SSE stream. Measured
  before the fix: the POST went out, the server rendered and logged 330,664
  bytes, and the browser received nothing, ever again. Same family as "removing
  an element aborts its request", reached from the other direction. popstate
  now has its own `#nav` element.
- **A tall range cannot be honoured literally.** `A1:Z3846` is a legal range
  (100,000 cells) and anchoring the viewport on all of it would make one shared
  link render 100,000 cells into the first response. The anchored viewport is
  capped at four bands; the rest marks itself on scroll, for free.

## THE ORIGINAL FINDING — fat morph is the wrong granularity here

One full-buffer morph, 150 rows x 26 cols = 3,900 cells:

| | size | ratio |
| --- | --- | --- |
| uncompressed | 126.3 KiB | — |
| **brotli q2** (streaming-safe) | **20.9 KiB** | 6.0x |
| brotli q5 | 16.3 KiB | 7.7x |
| gzip | 23.4 KiB | 5.4x |

Sustained wire cost, one edit/second, everyone on the hot band (br q2):

| viewers | throughput |
| --- | --- |
| 1 | 20.9 KiB/s |
| 10 | 208.8 KiB/s |
| 50 | 1.02 MiB/s |
| 200 | 4.08 MiB/s |

**Changing one cell ships the entire buffer.** A single-cell literal edit and a 10-cell
5-band cascade cost the *same* 21 KiB, because both re-morph the whole window. That is
the bottleneck, and it is not a compression problem — it is a granularity problem.

### The fix (now implemented — see above for the measured result)

The dirty set is already computed exactly — the `A1` cascade is 10 cells. Emitting 10
targeted `<td>` patches instead of a 3,900-cell table is roughly **400 bytes raw**
against 126 KiB, order 100x better.

The rule that fell out — and the one correction the measurement made to it:

- **cells changed (edit)** → per-cell patches keyed by the stable cell ids already
  emitted. The dirty set *is* the patch list. Confirmed: 543 B for the cascade.
- **buffer changed (scroll)** → the prediction here was "full morph, you genuinely
  need the new window". Wrong twice over. You need the *newly revealed* rows, not
  the window, and the incremental version is worth having — but for CPU, not for
  bytes, because streaming brotli was already collapsing the redundant part.

The dirty set was used only to decide *who* to wake. It now also decides *what* to
send.

Fat morph remains right for a page. For a grid where you know precisely which cells
changed, it throws that knowledge away.

## Storage notes

- `cells` is `WITHOUT ROWID`, clustered on `(row, col)` — the table *is* the b-tree, so
  a window read is a contiguous PK range scan that emerges already sorted. A regression
  test asserts the query plan and fails if it degrades to `SCAN` or a `TEMP B-TREE`.
- `Window(250 rows = 6,500 cells)` averages **2.2ms**. At 50 viewers x 1 edit/s that is
  ~110ms of CPU per second just reading — the second thing to fix after patch
  granularity, via a per-band render cache.
- **Cross-sheet formulas are structurally impossible**, not merely disallowed: a dirty
  set spanning two SQLite files cannot be one transaction. Supporting `=Other!A1` later
  means changing storage, not the parser.
- Sheet ids must be a single legal NATS subject token — `.` splits it, and ` `, `*`, `>`
  are reserved. Currently non-conforming characters map to `_`, so `a.b` and `a b`
  collide. Hash or hex-encode if user-supplied names ever become ids.

## Sparse rendering — the DOM was the cost, and it still is

Storage was always sparse; rendering was not. `Window()` materialised
`(hi-lo+1) x 26` cells and emitted an element for every one, so a blank sheet
cost exactly what a full one did: ~13,000 nodes per buffer. Cells now render
only where there is data, grid lines are CSS, and a cell's column comes out of
the stylesheet rather than out of its markup.

Measured in a real headless Chrome over CDP (`/tmp/ssobs/measure.mjs`,
`fill.py`, `bytes.py`), same machine, same driving script, before-binary against
after-binary, on three sheets rebuilt from scratch for every run: **blank**,
**realistic** (`-seed-rows 500`) and **dense** (`-seed-rows 10000`). Every case
is a 500-row buffer reached by the same go-to jump, except `demo@A200` which is
a 450-row top-of-sheet buffer. A seeded row holds 22 of 26 columns, so a fully
seeded window is 84.8% full, not 100%.

### What is emitted now

| | before | after |
| --- | --- | --- |
| a cell with a value | `<td id="D7">490</td>` | `<b id="D7" style="--r:6">490</b>` |
| an empty cell | `<td id="D7"></td>` | *nothing* |
| a row | `<tr id="r7"><th scope="row">7</th>…` | `<b id="n7" style="--r:6">7</b>` |
| horizontal grid lines | a border per cell | one `repeating-linear-gradient` |
| vertical grid lines | a border per cell | 26 `<i>` placed by `grid-template-columns` |
| a linked region | `class="s"` on every covered cell | one positioned `#sl` box |
| a cell's column | `td:nth-child(N)` on a table column | `#b [id^=D]{grid-column:5/6}` |

### DOM, bytes, and time

`nodes` counts every element inside `#g`. `scroll` is one edge-crossing buffer
slide — 200 rows dropped and 200 revealed, which is the natural unit at
`-buffer-bands 4`. `morph`/`paint` are the client's own `performance.now()`
stamps around that slide. `edit` is a one-cell literal commit driven through the
real UI.

| sheet | nodes | first paint raw | br q2 | scroll raw | scroll wire | morph ms | paint ms | edit raw | edit wire |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| **blank** before | 14,005 | 283,545 | 12,059 | 114,399 | 3,508 | 29.3 | 64.1 | 35 | 40 |
| **blank** after | **531** | **20,650** | **1,901** | **9,199** | **696** | **2.7** | **31.2** | 50 | 45 |
| **realistic** (empty region) before | 14,005 | 283,542 | 12,059 | 114,399 | 3,397 | 27.1 | 64.5 | 35 | 23 |
| **realistic** (empty region) after | **531** | **20,650** | **1,901** | **9,199** | **733** | **3.3** | **22.8** | 50 | 33 |
| **realistic** (data region) before | 12,605 | 295,006 | 46,661 | 114,328 | 7,524 | 22.6 | 64.9 | 34 | 68 |
| **realistic** (data region) after | **10,403** | 394,981 | **46,176** | **67,959** | 8,184 | **1.2** | **21.1** | 48 | 77 |
| **dense** before | 14,005 | 346,814 | 50,635 | 139,701 | 12,618 | 30.1 | 80.1 | 35 | 41 |
| **dense** after | **11,551** | 469,622 | **50,513** | 219,637 | 15,939 | **4.0** | **47.1** | 50 | 38 |

**A blank screen went from 14,005 nodes to 531 — 26x — and from 12.1 KB
compressed to 1.9 KB.** Client morph time fell 8-19x on every sheet, and paint
roughly halved. That is the result the exercise was after and it is unambiguous.

**On a full sheet, sparse costs raw bytes and breaks even compressed.** A
fully-seeded window is 34% bigger raw and 0.2% *smaller* after brotli q2. This
is the same lesson the incremental-scroll work produced, arriving from the other
side: brotli was already collapsing the redundant empty cells, so the wire never
was the problem.

### The regressions, stated plainly

- **A single-cell edit went from 35 B to 50 B raw** (the brief's "must not
  regress" number). The 15 bytes are `style="--r:2202"`. Compressed it is a
  wash or better — 41 → 38 B on the dense sheet, 40 → 45 B on the blank one —
  because the framing dominates a payload this small. It is a real raw
  regression and the row-group shape below removes it entirely (33 B).
- **A dense-sheet scroll costs more: 139,701 → 219,637 raw, 12,618 → 15,939 on
  the wire (+26%).** Two causes, both structural. The revealed cells now carry
  `--r` (~13 B x 4,400 cells), and the drop selector has to name every cell it
  removes because there is no row element to take them with it: **32,255 bytes
  of selector against 1,399 before.** On a sparse sheet the same scroll is
  9,199 B against 114,399 — 12x better — so this is specifically the cost of
  being sparse on data that is not.
- **Paint on the blank sheet went 64 → 31 ms, not to zero.** 500 row-number
  elements is what a blank buffer still costs, and the number is data.

### THE FILL-RATIO BREAK-EVEN

The demo sheet holds data in rows 0..499 only, so a buffer straddling row 500
is a 500-row window at a known fill. Seven anchors give seven windows from 0% to
84.8% full, on one sheet, with no synthetic data anywhere (`/tmp/ssobs/fill.py`).

Grid bytes, sparse minus dense (negative = sparse cheaper):

| fill | raw | brotli q2 |
| --- | --- | --- |
| 0.0% | −262,892 | −10,158 |
| 8.5% | −214,025 | −9,600 |
| 25.4% | −141,293 | −7,126 |
| 42.4% | −68,561 | −5,716 |
| 59.4% | **+4,237** | −3,819 |
| 76.3% | +76,076 | −1,703 |
| 84.8% | +111,291 | −523 |

**Sparse starts costing more raw bytes than dense at 58.4% fill, and more
compressed bytes at 88.6%.** Below those it is cheaper on both, and the DOM
saving holds at every ratio.

The two numbers being 30 points apart is the finding. Raw bytes say "sparse
loses above three-fifths full"; the wire says "sparse essentially never loses",
because the thing dense rendering spends its bytes on — 13,000 near-identical
empty cells — is exactly what a compressor deletes for free.

### What the attribute-selector trick is worth

A cell id is A1 notation, so the column letter is a prefix of the id and 26
generated rules can place every cell in the sheet:

```css
#b [id^=A]{grid-column:2/3}  #b [id^=B]{grid-column:3/4}  …
```

The control is `SS_CELL_COL=1`, which puts `--c` back on every cell and
collapses those 26 rules to one. Same binary, same sheets, same script:

| shape | raw break-even | brotli q2 break-even |
| --- | --- | --- |
| flat, column from the id | **58.4%** | **88.6%** |
| flat, `--c` on every cell | 48.6% | 47.6% |
| row groups, column from the id | never (>100%) | never (>100%) |
| row groups, `--c` on every cell | 57.0% | 59.6% |

**The trick is worth ~10 points of fill raw and ~41 points compressed.** The
compressed delta is the larger one and that is not intuitive — but `;--c:12`
varies per cell where `--r:250` repeats down a row, so the column property is
the part brotli cannot fold away. It is 6.6 bytes per cell on average, and on
the dense first paint it is 43 KB raw and 8.9 KB on the wire.

The general rule it is an instance of: **O(config) data belongs in a
server-generated stylesheet, O(cells) data in the markup, and anything movable
from the second to the first should move.** Column widths already worked this
way (26 `--w-N` properties on `#vp`, patched when they change); column position
is the same fact one step further. Row heights, frozen panes and conditional
formatting are all the same shape.

Safe only because MaxCols is 26 — `[id^=A]` would also match `AA1`.

### Row groups: cheaper bytes, much more expensive layout

The other shape the brief asked to measure. A row that holds data gets a wrapper
carrying `--r` once, and its cells carry only their ids
(`<div class="w" id="r251" style="--r:250"><b id="A251">7</b>…`). `SS_ROW_GROUPS=1`.

| | flat | row groups | dense (before) |
| --- | --- | --- | --- |
| dense first paint, raw | 469,622 | **306,782** | 346,814 |
| dense first paint, br q2 | 50,513 | **47,997** | 50,635 |
| dense scroll, raw | 219,637 | **125,045** | 139,701 |
| dense scroll, wire | 15,939 | **13,082** | 12,618 |
| dense scroll, drop selector | 32,255 | **2,799** | 1,399 |
| single-cell edit, raw | 50 | **33** | 35 |
| insert into an empty row | **50** | 82 | 35 |
| dense nodes | **11,551** | 12,051 | 14,005 |
| **dense scroll morph ms** | **4.0** | 52.4 | 30.1 |
| **dense scroll paint ms** | **47.1** | 87.7 | 80.1 |
| realistic scroll morph ms | **1.2** | 30.7 | 22.6 |

**Row groups win every byte comparison — they beat even the dense table on raw
bytes at every fill ratio — and lose the one that motivated the work.** 500
nested grid containers cost 52 ms of morph against flat's 4 ms, which is worse
than the `<table>` we started from. The wrapper buys back the O(rows) drop
selector and the 32 B edit patch, and pays for it in layout.

So: **flat is the default**, because the brief's premise — cheap on the wire,
expensive in the DOM — is confirmed by these numbers rather than contradicted by
them. Row groups are the right answer if the wire ever becomes the constraint
(many viewers on one hot dense sheet), and both shapes are one environment
variable apart so the choice can be re-measured rather than re-argued.

### Hit-testing had to become arithmetic

This is the part sparse rendering makes genuinely harder. The click handler read
the clicked element's `offsetLeft`/`offsetWidth`; over empty space there is no
element, and a spreadsheet where you cannot select an empty cell is not a
spreadsheet.

The row half was already arithmetic (`scrollTop/22`). The column half is a scan
over the 26 vertical-rule elements — the same elements that draw the lines,
doing a second job as the column-geometry oracle. They exist unconditionally and
are sized by the same `grid-template-columns` the cells are placed in, so they
answer "which column is at x" and "where is column c" correctly however the
widths were arrived at: stylesheet default, first-paint inline style, live drag,
or a server patch.

Everything downstream then works over empty space without knowing it: keyboard
navigation, the editor overlay, the active-cell box, `?at=` linking, and the
selection box. `Ctrl+arrow` improved by accident — "has something in it" is now
literally "has an element", so the DOM became a direct index of the data. Its
vertical walk had to start taking `$blo`/`$bhi` as arguments, because a missing
element used to mean "outside the buffer" and now means "empty".

Verified with the existing harnesses driving real mouse and key events:
`/tmp/ssobs/keys.mjs` 37/37, `/tmp/ssobs/regress.mjs` 17/17, `repro2.mjs`
unchanged. Both harnesses now aim at coordinates rather than elements, which is
a strictly better test of the same behaviour.

### What it cost structurally

- **The server had to start remembering which cells the client has.**
  `screen.heldMask` is a 26-bit column mask per buffer row, ~2 KB per screen. A
  dense grid needed no such thing: every cell of the window existed, so a patch
  was always a morph and a drop could name 50 rows. Sparse rendering makes three
  questions real — is there an element to morph, is there one to remove, and
  which ids does this dropped band contain — and every alternative is worse.
  Guessing "always remove then append" doubles the frames on the most-measured
  path; re-reading the dropped rows reintroduces exactly the read the
  incremental scroll path exists to avoid; naming all 26 columns of every
  dropped row costs 26x the selector on a sheet that is mostly empty.
- **The buffer offset disappeared, and that is a simplification.** `#b` used to
  be translated by `$blo * 22` with rows positioned by DOM order. Every element
  now carries `--r`, so it sits at its true absolute position and the rows can
  no longer desync from the offset — which they could, because they arrived in
  separate SSE frames. `overflow-anchor:none` stays on `#vp` regardless; the
  keyboard-navigation failure it fixes is not the same one.
- **DOM order stopped carrying information, and three things collapsed.**
  `prepend` and `append` became one `append`, so a scroll is two element frames
  instead of three. A cell insert goes anywhere in the child list. And a scroll's
  frames can interleave in any order without putting the buffer out of sequence.
  The whole `TestRenderRowsIsAscending` / `TestPrependThenExistingIsInOrder`
  contract was deleted and replaced by one test asserting that every element
  names its own row.
- **The linked region stopped fighting the incremental design.** The one place
  RESULTS.md previously recorded a genuine conflict — "a region change forces a
  full morph", because the cells that must LOSE `class="s"` are in no range the
  diff knows about — is gone. One box is one patch: a jump that changes the
  region without moving the buffer went from ~300 KB to about 60 bytes, and a
  region taller than the buffer is drawn correctly for free rather than marking
  itself as the reader scrolls into it.
- **The row-number gutter is the irreducible floor.** A blank 500-row buffer is
  531 nodes, of which 500 are row numbers. CSS cannot generate them; the number
  is data.
- **Grid lines are pixel-exact at 1x and 2x** because the gradient period IS the
  row height, so line n lands at n x 22 whether n is 3 or 9,999 with no
  accumulated rounding. Verified by screenshot at row 9,500 at 2x
  (`/tmp/ssobs/shots/sp3-deep-2x.png`).

### A lever not pulled

Unquoted attribute values (`<b id=D7 class=t style=--r:6>`) are legal HTML5 and
save ~6 bytes per cell, which would move the raw break-even by roughly the same
amount as the attribute-selector trick. Not taken: it is orthogonal to sparse
rendering (the dense table could have done it too), and it would break every
regex in the harnesses for a saving the compressor mostly recovers anyway.

## The commit flicker — CQRS's one visible cost, and what it costs to remove

Reported: *"on the demo url i do see a flicker of the old value before its
persisted."* Confirmed, at frame level, before anything was changed. Enter
lowers `$editing`, which hides the editor and reveals the cell UNDERNEATH it —
still holding the previous value, because the command's response carries no
grid and the only thing that ever writes a cell is the push a full round trip
later.

Sampled on every animation frame between the keydown and the push landing
(`/tmp/ssflick/flick.mjs`, real Chrome over CDP, distinct values with frame
counts):

| case | before | after |
| --- | --- | --- |
| `B3` first → second (90ms/leg) | `"first"x11@6ms -> "second"@189ms` | `"second"x54@5ms` |
| `B3` first → second (180ms/leg) | `"first"x22@8ms -> "second"@374ms` | `"second"x54@6ms` |
| `C7` ∅ → fresh (an empty cell) | `"∅"x11 -> "fresh"@188ms` | `"fresh"x55@1ms` |
| `E3` ∅ → `=D3*3` | `"∅"x11 -> "21"@197ms` | `"=D3*3"x11@9ms -> "21"@192ms` |
| `B3` second → cleared | `"second"x11 -> "∅"@191ms` | `"∅"x55@1ms` |
| `F3` Enter commits and moves | `"∅"x21 -> "moved"@366ms` | `"moved"x54@11ms` |
| click-away / blur commit | (same window) | `"clicked"x55@2ms` / `"blurred"x70@3ms` |

**22 frames of stale data at 180ms/leg, and none after.** The formula row is the
interesting one and it is deliberately not zero.

### What the client is allowed to paint

The constraint that shapes the whole fix: **formulas are always round trip.**
There is no client-side formula engine and this must not grow one. That splits
the cases rather than complicating them:

- **A literal is not a prediction.** `computed == raw` for a literal in the
  store, so the text the user typed IS the value; the echo writes exactly the
  bytes the server is about to send, `class="t"`/no-class split included
  (`InferKind`'s number/text rule, three characters of JS). The confirming push
  then morphs identical content over identical content, so there is no second
  flicker either.
- **A formula shows the formula.** Dimmed, italic, left-aligned (`class="f p"`),
  replaced by the computed value when the push lands. It reads as *working*
  rather than as *wrong*, it is the truth the client actually has, and a cell
  that briefly shows `=D3*3` cannot be mistaken for a value the way a spinner or
  a stale number can. Nothing is invented and nothing is evaluated.
- **A clear removes the element**, because under sparse rendering that is what
  an empty cell is.

### The echo goes on the COMMAND, not on the key

Enter, Tab, blur, the pointerdown that precedes a click on another cell, and
Delete-to-clear are five doors into one command. An echo wired to four of them
is a stale value on the fifth, so it lives in `cellPost` — the one expression
they all funnel through — and runs BEFORE `@post`, therefore before the nav
event `#ed` dispatches. That ordering is what puts the paint on the cell being
committed rather than on the one Enter just moved to.

### What it cost structurally

- **The server had to be told.** Sparse rendering means a value arriving in an
  empty cell is an INSERT and a clear is a REMOVE, so the echo moves the client's
  DOM out from under `screen.heldMask` — the server's model of which cells the
  browser has. Left unsaid, the next push appends a SECOND element with the same
  id, because the mask still says "absent". The cell command therefore carries
  `conn` (free: Datastar ships every ordinary signal with every request) and
  `handleCell` updates that one screen's mask. This is the same lesson sparse
  rendering already taught, arriving from the client side: **the server has to
  remember what the browser has, and now the browser can change it.**
- **A patch changed shape, and got cheaper.** For the committing screen an
  insert becomes a morph and a pure clear becomes nothing at all — the element
  payloads are byte-identical (44/44/59/53/31 B before and after, same driving
  script) but a one-cell edit is now ONE SSE frame instead of two. Per-edit wire
  bytes fell 61→47, 25→20, 44→41, 33→16, 24→21. Other viewers are untouched: a
  second viewer of the same sheet still receives `append #b` for the insert,
  `outer` for the overwrite and `remove #C5` for the clear (verified with a raw
  SSE client alongside the browser, `/tmp/ssflick/viewer2.mjs`).
- **A refused command must un-paint.** `/cell` answers 400 on a ref it cannot
  parse (`bad cell reference: "A0" row out of range 1..10000`) and sends no
  patch, so nothing would ever correct an optimistic lie. Datastar dispatches
  `datastar-fetch` with `type:'error'` and `el` set to the issuing element for
  any response >= 400, so the editor's own failures are identifiable: the saved
  `outerHTML` goes back, and the records are dropped on `'finished'` so a later
  failure can never revert an edit that succeeded. Measured with CDP request
  interception (`/tmp/ssflick/reject.mjs`): `"lie" -> "keep"`,
  `"ghost" -> ∅`, `∅ -> "keep"`, one element per id afterwards, and the next
  commit still lands.
  - The server deliberately does NOT repair a rejected edit by forcing a full
    window morph at the committing screen. It would work, and it would also let
    anyone turn a malformed 40-byte POST into a 300 KB render.
- **A no-op commit paints nothing** (`rawOf(ref) === raw`). That stops a
  click-away from flashing a formula cell for no reason, and it is also what
  makes digest suppression safe: the only way the server can suppress an edit's
  patch is by producing bytes identical to the previous one, which needs the
  same raw committed twice in a row — exactly the case that now never paints a
  pending cell to strand.

### The bytes

| | before | after | delta |
| --- | --- | --- | --- |
| blank first paint, grid region raw | 20,680 | 20,803 | **+123** |
| blank first paint, grid region br q2 | 1,911 | 1,941 | +30 |
| page shell raw (never re-sent) | 26,542 | 27,704 | +1,162 |
| whole page br q5 | 9,905 | 10,335 | +430 |
| one-cell edit patch, raw | 44 | **44** | 0 |
| one-cell edit patch, wire | 61 / 25 | **47 / 20** | −14 / −5 |

The +123 in the grid is three copies of `window.__ss&&window.__ss.echo($ref,$raw);`
— the editor carries the cell command three times (keydown, blur, and the commit
event Delete and pointerdown dispatch), and it is re-sent only by the full path.
**Per cell: nothing, as before.** The echo itself is ~1.1 KB of page shell,
written once per page load, and it is generated per rendering shape so a build
ships the flat helpers or the row-group ones, never both.

Harnesses re-run green on the same build: `/tmp/ssobs/keys.mjs` 37/37,
`/tmp/ssobs/regress.mjs` 17/17 (on a fresh sheet — a second run against the same
server fails the region-box check because the earlier column resize persisted),
`blur.mjs` 2/2, `repro2.mjs` unchanged, `flick.mjs` 14/14 at both 90 and 180
ms/leg, `reject.mjs` 12/12, `viewer2.mjs` 6/6. No console errors.

## Range selection — the client's half of the region, and what it costs

`?at=A1:D20` could always *show* a range. Nothing could *make* one. This is the
interaction — drag, shift+click, Shift+Arrow, Ctrl/Cmd+Shift+Arrow,
Ctrl/Cmd+A, row and column headers, and Delete-to-clear — and the whole of it
is client state.

**The wire cost is zero, and it is zero by construction rather than by
discipline.** The four signals that hold the rectangle are underscore-prefixed,
which the v1.0.1 fetch actions filter out of every request body
(`exclude:/(^|\.)_/`), and the box that draws it is page-shell markup, which no
push re-sends and no morph can remove. Measured in a real browser:

| | measured |
| --- | --- |
| grid region (`#g`, re-sent by every full render) | **20,797 B raw / 1,945 B br2** — unchanged |
| one-cell edit patch, no selection | 55 B |
| one-cell edit patch, **inside** a selected range | **55 B** |
| a 12-step drag + six Shift+Arrows | **0 requests, 0 patches** |
| the viewport command's body | 351 B, **no selection signal in it** |
| page shell (once per load, never re-sent) | **+2,704 B raw** for the whole feature |

The +2,704 B is the two shell elements (`#sb`, `#cl` — 346 B), the toolbar's
size indicator (97 B), the four signal declarations (28 B), the two pointer
handlers (158 B) and `selectScript()` (2,075 B).

### The shape that made it free

The interactive box is **not** `#sl`. `#sl` is the server's box: it exists so a
shared link is correct in the first byte, before the bundle has been fetched.
The interactive one is `#sb`, a sibling of `#g` inside `#vp`, positioned in
pixels from the same column-rule oracle `#ac` uses — so it is correct over empty
cells (which under sparse rendering is most of them), it survives a scroll patch
that replaces every row, and it costs nothing per render. `T.seed` removes the
server's box the moment Datastar boots, and `handleLive` no longer seeds the
screen's selection, so the two can never coexist and the URL's range can never
be re-asserted over the reader's.

Selection state is **anchor + focus**, not lo/hi and not derived from the active
cell: Ctrl/Cmd+A has to select everything *without* moving the active cell, and
Shift+Arrow has to extend from a corner that survives the extension.

### Range clear — the first real large-range operation

`Delete` on a range loops the single-cell write path: one actor turn, one recalc
and one transaction per cell, cells that are already empty skipped. This is the
case SPEC.md deferred, and the numbers are the point.

Seeded 10,000-row sheet, literals in A..T with `=A*2`, `=U+B` and per-band
`=SUM()` cascades hanging off them (so every cleared cell dirties more than
itself). Three samples per size, server-side `command_ms`:

| range | cells | written | dirty | read | write | total | per cell |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `A301:A310` etc. | 10 | 10 | 33 | 0.3–1.6 ms | 9.9–11.3 ms | **10.3 / 11.0 / 12.9 ms** | ~1.1 ms |
| `A601:J610` etc. | 100 | 100 | 123 | 0.3–0.5 ms | 18.4–19.2 ms | **18.9 / 19.5 / 19.6 ms** | ~0.19 ms |
| `A901:T950` etc. | 1,000 | 1,000 | 1,103 | ~0.9 ms | 128–130 ms | **129.0 / 128.6 / 130.9 ms** | **~0.13 ms** |
| same range again | 1,000 | 0 | 0 | 0.6 ms | 0 ms | **0.66 ms** | — |

Per-cell cost *falls* with size — the 10-cell case is dominated by its
disproportionate cascade (33 dirty cells for 10 writes, because column A feeds a
band-wide `SUM`), not by the loop. The marginal cost is ~0.13 ms/cell, which is
one SQLite transaction each.

What that implies, and why the cap exists: 2,000 cells ≈ 260 ms, 10,000 ≈ 1.3 s,
and Ctrl/Cmd+A over the whole grid (260,000 cells) would be **~34 seconds** with
the sheet's single writer held for all of it. So `maxClearCells` is 2,000 and
anything larger is refused with a sentence rather than served slowly. That is the
honest baseline the brief asked for; it is not optimised here.

The fan-out cost, measured at a second screen holding the affected rows:

| clear | frames | remove selector | element patches |
| --- | --- | --- | --- |
| 10 cells | 2 | 69 B | 1,561 B |
| 100 cells | 2 | 699 B | 1,541 B |
| 1,000 cells | 2 | 6,999 B | 6,817 B |

Two frames whatever the size — one `remove` naming every cleared cell, one run of
recomputed formula cells — because the dirty set is published once, not per cell.
The selector is ~7 B per cell and irreducible under flat sparse rendering; this is
the same cost `maskSelector` already pays for a dropped band, and it is the case
row-group mode exists to make O(rows).

### Verified in a real browser

`/tmp/ssobs/sel.mjs`, 53/53, no console errors: drag over empty cells, the box in
the shell with the right geometry, shift+click, Shift+Arrow (active cell
unmoved), Ctrl/Cmd+Shift+Arrow to the block edge and again across the gap,
Ctrl/Cmd+A without moving the active cell, row-number and column-letter
selection, header drags, collapse on click / Escape / any arrow, survival across
a scroll that dropped and re-added the rows underneath it, the `?at=` handover,
the range clear, and the refusal. Screenshots in `/tmp/ssobs/shots/sel-*.png`.

Everything that already worked still works on the same build: `keys.mjs` 37/37,
`regress.mjs` 19/19 (17 + 2 new, fresh sheet and fresh profile), `flick.mjs`
14/14 at 90 and 180 ms/leg, `reject.mjs` 12/12, `viewer2.mjs` 6/6, `pending.mjs`
17/17. Column resize, the caret menu and the right-click menu are covered
explicitly, because the header selection is a new claim on elements that already
had owners.

## Selection as an ARGUMENT — the aggregate, fill, and copy/paste

Range selection could draw a rectangle. It could not do anything with one. These
are the three operations that make it useful, and the first of them is the
interesting one architecturally.

### The aggregate — a read model with arguments

SUM / AVG / COUNT / MIN / MAX for the selected range, in the toolbar.

**The selection stays client state and still parameterizes a server read.** The
range travels as an ARGUMENT on one request; the server computes from the same
`Window()` every push already reads, answers with five numbers, and stores
nothing — no per-screen selection field, no publish, no invalidation, no entry in
the edit log. That is a CQRS read model with args, and it is the whole point:
"the client owns the selection" and "the server does the arithmetic" are not in
tension.

It is the only GET on the page besides the stream itself, because it is the only
thing on the page that is a READ. Everything that follows falls out of that:

| | measured |
| --- | --- |
| the request | `GET /agg?datastar={"r":"B403:H414"}` — **43 B of query string** |
| the answer, 1,000-cell range | **112 B**, whole SSE frame — `{"_ag":"Sum 544000 · Avg 544 · Count 1000 · Min 0 · Max 997"}` |
| the answer, empty range | 105 B frame / **10 B** of signal payload |
| a 12-step drag, WHILE DRAGGING | **0 requests, 0 patches** |
| the same drag, after it settles | **exactly 1 request** — the aggregate, nothing else |
| six Shift+Arrows | **1 request**, not six |
| the viewport command's body | 353 B, **no `_sar`/`_sac`/`_sfr`/`_sfc`/`_cp`/`_ag` in it** |

**The zero-request property is intact where it was claimed and is now stated
precisely.** The gesture costs nothing; ONE debounced read follows the gesture.
Nothing was moved into the steady-state signal set to get there: the answer comes
back as `_ag`, underscore-prefixed like the 26 column widths, so it is patched
DOWN and never rides back UP.

### Fill down (Ctrl+D), fill right (Ctrl+R), and copy/paste (Ctrl+C/Ctrl+V)

Formulas translate their references, which is the entire difference between a
spreadsheet's fill and a block copy: filling `=A1*2` from B1 down to B2 yields
`=A2*2`, and pasting `B10:B12` (holding `=A10*2`…) at `D20` yields `=C20*2`…
Verified end to end in a real browser, reading the committed `data-r` back off a
fresh server render.

It needed **no store API change**. `Cell.Raw` already comes out of `Window()` as
rendered A1 text (the store keeps references structurally and re-spells them on
read), so a fill re-spells the references in that text and hands the result to
the ORDINARY single-cell write path, which re-parses and re-stores the slots
itself. `translateFormula` works off `ParseFormula`'s slot offsets, so it is a
splice at known byte spans rather than a second parser. A reference that lands
off the grid becomes `#REF!` and the formula then stores verbatim as text —
which is what a spreadsheet shows for the same mistake.

**Cost, on the seeded 10,000-row sheet, three samples each, server-side
`command_ms`.** The clear is re-measured on the same run and the same machine
state so the three are directly comparable; it reproduces its earlier numbers
(125.5 ms at 1,000 cells against 129.0/128.6/130.9 before).

| op | cells | written | dirty | read | write | total | per cell |
| --- | --- | --- | --- | --- | --- | --- | --- |
| clear | 10 | 10 | 33 | 0.2–0.3 ms | 9.8–12.4 ms | 12.8 / 11.3 / 10.0 ms | ~1.1 ms |
| **fill** | 11 | 10 | 33 | 0.18 ms | ~9.0 ms | **9.2 / 9.2 / 9.1 ms** | ~0.92 ms |
| **paste** | 10 | 10 | 20 | 0.29 ms | 1.0–1.8 ms | **1.6 / 1.3 / 2.0 ms** | ~0.16 ms |
| clear | 100 | 100 | 123 | 0.19 ms | 17.1–18.3 ms | 17.3 / 17.3 / 18.5 ms | ~0.18 ms |
| **fill** | 110 | 100 | 123 | 0.17 ms | 17.1–18.4 ms | **17.3 / 18.6 / 17.4 ms** | ~0.18 ms |
| **paste** | 100 | 100 | 110 | 0.30 ms | 8.7–9.0 ms | **9.4 / 9.0 / 9.0 ms** | ~0.09 ms |
| clear | 1,000 | 1,000 | 1,103 | 0.6–1.0 ms | ~124.7 ms | 125.5 / 125.6 / 125.5 ms | ~0.125 ms |
| **fill** | 1,020 | 1,000 | 1,105 | 0.6–0.7 ms | 128.8–130.3 ms | **129.6 / 131.0 / 129.5 ms** | ~0.130 ms |
| **paste** | 1,000 | 1,000 | 1,050 | 1.3–1.6 ms | 89.6–90.7 ms | **91.2 / 92.3 / 92.1 ms** | ~0.092 ms |

(A fill's range is one row or column taller than what it writes, because the
source line is read and never written — hence 11/110/1,020 cells for 10/100/1,000
writes. That also makes a fill idempotent: a second Ctrl+D over the same range
writes nothing at all.)

**Per-cell cost tracks the CASCADE, not the loop.** Paste is ~30% cheaper per
cell than clear or fill for the same 1,000 writes, and the difference is exactly
the dirty set: 1,050 cells against 1,103/1,105, because these fills and clears
land in column A which feeds a per-band `SUM` and this paste lands in B..U which
does not. The loop itself is one SQLite transaction per cell and is the same code
in all three.

**So the cap is the same number for all three: 2,000 cells.** Fill and paste are
`handleClear`'s loop over `handleCell`'s body, so a separate limit for each would
be three numbers to keep in step and two of them would drift. Ctrl+A then Ctrl+D
is 260,000 cells and is refused with a sentence naming the limit, as is a paste
of the same. A paste whose destination would run off the sheet is refused before
anything is written.

The fan-out, measured at a second screen holding the affected rows:

| op | frames | remove selector | element bytes |
| --- | --- | --- | --- |
| fill 10 | 1 | 0 B | 1,919 B |
| fill 100 | 1 | 0 B | 5,401 B |
| fill 1,000 | 1 | 0 B | 45,270 B |
| paste 10 | 1 | 0 B | 1,082 B |
| paste 100 | 1–2 | 0 B | 4,499 B |
| paste 1,000 | 1 | 0 B | 41,367 B |

**One frame, not two, and no remove selector at all** — where a clear of 1,000
cells cost 2 frames and 7 KB of `remove` selector naming every emptied cell, a
fill or a paste writes VALUES, so every dirty cell is a morph or an insert and
the patch is the cells themselves (~45 B each). That is the opposite side of the
same sparse-rendering coin: clearing is expensive in selector, filling is
expensive in content, and both are one publish.

### The bytes

| | before | after |
| --- | --- | --- |
| grid region (`#g`, re-sent by every full render) | 20,797 B raw | **20,797 B raw — identical** |
| a cell | `<b id="A1751" style="--r:1750">250</b>` | **unchanged** |
| a row number | `<b id="n1751" style="--r:1750">1751</b>` | **unchanged** |
| one-cell edit patch | 55 B | **55 B** |
| the same edit inside a selected range | 55 B | **55 B** |
| page shell (once per load, never re-sent) | 32,663 B | **34,633 B (+1,970)** |
| whole blank page, br q5 | 11,732 B | 12,216 B (+484) |

Measured against the previous binary on a blank sheet at the same anchor, both
servers running side by side. (`#g` brotlis to 1,946 before and 1,947 after; the
region contains the editor's POST url, so two sheets with different ids of the
same length give byte-identical raw and a byte or two of compressed difference.
The raw equality is the claim.)
**Per cell: nothing, again.** All three features are page-shell markup — one span
bound to `_ag`, one box bound to `_cp`, three hidden issuer elements, two client
helpers — so no push mentions any of it and no cell grew a byte.

### What it cost structurally

- **"Settled" had to be known, not guessed.** The aggregate is armed by a
  `data-effect` on the four selection signals — one subscription instead of a
  handler on each of the eight gestures that can move a range, the same argument
  that put the local echo in `cellPost`. A 180 ms quiet timer was the obvious
  debounce and it was wrong: a 12-step drag driven over CDP takes ~800 ms because
  every event is a round trip, so the timer fired FOUR times inside one gesture
  (measured, then fixed). `T.dg` already knows a pointer drag is live, so the
  timer is not armed at all until `selUp`. A human drag is fast enough that the
  bug would never have shown up in front of a person.
- **A fourth event and a fourth element, for the third time the same reason.**
  Datastar keys request cancellation on the ELEMENT. `#cl` (clear), `#fl` (fill)
  and `#pv` (paste) are slow writes that must not be aborted by the viewport
  command the next buffer edge issues; `#ag` is a fast read that SHOULD supersede
  itself, and it can only have Datastar's default `'auto'` cancellation by owning
  an element nothing else posts from. The two requirements are opposite and both
  are expressed by the same rule.
- **An overlapping paste has to snapshot its source.** Copy `A1:A5`, paste at
  `A3`, and a loop that reads as it writes copies the values it has just laid
  down. The source window is read once, up front, before any write — which is
  also what makes "this cell already says that" a free skip.
- **The aggregate does not follow the DATA, only the SELECTION.** Edit a cell
  inside a selected range and the toolbar keeps the number it last computed until
  the selection moves. Recomputing on every push would mean either re-reading per
  screen per edit, or making the selection server state — which is the thing this
  design exists to avoid. Stated as a limitation rather than fixed.
- **A one-shot SSE response is NOT retried, and that was worth checking rather
  than assuming.** Datastar's fetch actions default to `retry:"auto"` with
  `retryMaxCount:10`, and this handler answers with one signal frame and closes —
  which is exactly the shape that would reconnect forever if the bundle treated a
  clean end as a failure. Measured: one request, and still one after eight
  seconds of idling.
- **`pointerup` is on the WINDOW, so "the drag ended" is not the same event as
  "a pointer was released".** Re-arming the aggregate from every pointerup would
  re-issue the read whenever the reader clicked the toolbar with a range still
  selected. `selUp` now re-arms only if a drag was actually live — the same class
  of bug as `T.selMove` having to consult `e.buttons`.
- **Ctrl+A + Ctrl+D and Ctrl+A + aggregate hit two different limits.** The write
  refuses at 2,000 cells (a sentence in the chip); the READ refuses earlier, at
  `ParseRangeBounds`'s 100,000-cell ceiling, and says so in the aggregate line
  itself. Two limits, two places, both visible.

Verified in a real browser (`/tmp/ssobs/range.mjs`, 29/29, no console errors):
the aggregate over a mixed numeric/text range and updating as the selection
grows, a single cell showing nothing, the drag issuing nothing, fill down and
fill right with formula translation, copy/paste with translation on both axes,
the copy marquee, both refusals, and **a second real browser tab receiving every
one of the writes via push** while seeing none of the selection, marquee or
aggregate — because those are per-viewer client state. Screenshots in
`/tmp/ssobs/shots/rng-*.png`.

Everything that already worked still works on the same build: `sel.mjs` 53/53,
`keys.mjs` 37/37, `regress.mjs` 19/19 (fresh sheet, fresh Chrome profile),
`flick.mjs` 14/14 at 90 and 180 ms/leg, `reject.mjs` 12/12, `viewer2.mjs` 6/6,
`pending.mjs` 17/17. `go test ./...` green.

## Not verified

- **Caret and IME survival under morph.** Needs a real browser with a real keyboard.
  This is the highest-value remaining check. It is now structurally easier: an edit
  push is a run of bare `<td>`s, so `#ed` — the one focusable element — is not in
  the payload at all and cannot be morphed. It is still re-sent by the full path
  (first paint, disjoint scroll jump), which is where the `data-ignore-morph` hole
  still has to hold.
- `recalc_test.go` was never written (the agent was repeatedly killed by machine sleep).
  The recalc engine itself is exercised end-to-end by the cascade tests above, but the
  unit-level cases — cycle detection, topological ordering, the overlay contract — are
  unpinned.
- Latency profiles (`-latency-ms`) exist but were not swept.
- Large-range operations at 10k cells and beyond, and sorting a column — still the
  known worst case. Insert/delete row is built (`structure.go`); range CLEAR, FILL
  and PASTE are built and measured (see above): ~0.09–0.13 ms per cell depending on
  the cascade, all three capped at 2,000 cells, all three unoptimised on purpose.
  Nothing here has been tried at 10,000 cells because the cap refuses it.
- **Selected rows and columns are not highlighted in the headers.** Sheets tints the
  row number and the column letter of the selection. The gutter numbers are rendered
  per row and inside the patch region, so doing it with a class would put a per-row
  cost back into every scroll patch; the cheap version is a second overlay box over
  the gutter and the header strip, which is not built.
- **No auto-scroll while dragging past the edge of the viewport.** A drag stops
  extending at the last visible row; keyboard extension does reveal.

## Cell styling, number formats, and a resize that stops relayouting the grid

Four changes, landed in order, each verified before the next: the renderer
learned to draw a style, a toolbar learned to set one, a styled EMPTY cell
became a real element, and the column drag stopped touching the grid at all.

Plus one bug fixed first, because it was small and would have got tangled up:
**an `?at=` past the end of the sheet clamped the window and not the anchor.**
`?at=C115000` on a 10,000-row sheet rendered a correct 10,000-row container and
then seeded the selection on row 114,999 — the reader saw the last few rows
stranded at the top of a void with `C115000` in the toolbar. `anchorViewport`
had always clamped; the five things that read the ANCHOR rather than the window
(the `at` signal, `_sar`/`_sfr`, the active-cell box, the toolbar readout) had
not. One clamp at the point the extent becomes known (`anchor.clampTo`, called
once in `handlePage`) covers all of them. Verified live: `at:'C10000'`,
`_sar:9999`, `--rows:10000`, and the string `115000` appears nowhere in the page.
A range clamps both corners (`A900:D1200` on a 1,000-row sheet -> `A900:D1000`);
a range past every limit (`A9000:D115000`, 424,004 cells) is still refused by
`ParseRangeBounds` and opens the sheet at the top, which is the pre-existing
"a mangled link opens the sheet" rule.

### What it cost the wire: nothing per cell, and that is the claim

| | before | after |
| --- | --- | --- |
| `#g`, blank sheet | 9,950 B raw / 1,543 br2 | **9,950 B raw** / 1,545 br2 |
| `#g`, seeded 10,000-row sheet | 217,727 B raw / 26,418 br2 | **217,727 B raw / 26,418 br2** |
| an unstyled cell | `<b id="D7" style="--r:6">490</b>` | **byte-identical** |
| one-cell edit patch | 55 B | **55 B** |
| the same edit inside a selected range | 55 B | **55 B** |
| the viewport command's body | 351 B | **351 B** — no new signal |
| page shell (once per load, never re-sent) | 46,259 B raw / 12,365 br5 | 53,972 B / 14,608 br5 (**+7,713 / +2,243**) |

`#g` is byte-identical because a styled cell carries a CLASS and nothing else:
`<b id="D7" class="s3" style="--r:6">490</b>`, four bytes, against one
`#b b.s3{font-weight:700;color:#cc0000}` rule in the page shell. That is the
same O(config)-in-CSS / O(cells)-in-markup rule the column widths, the 26
column-placement rules and `--rows` already follow, and it is the third
instance of it.

The +7,713 B of shell is the whole feature, written once: the toolbar's
eighteen controls 2,914 B, the `#st` issuer 416 B, the client helpers 411 B,
the (empty) `<style id="sy">` 23 B, the drag guide 19 B, and ~1.3 KB of CSS.
**Per cell: nothing, again.**

The selector prefix is load-bearing and cost seven bytes a rule: the grid's own
`#b b.t{text-align:left}` and `#b b.f{color:#0b57d0}` are specificity (1,1,1),
so a bare `.s3` at (0,1,0) would lose every tie and a red formula would stay
blue. `#b b.s3` matches them and arrives later in the document, so source order
decides — the ordinary CSS answer rather than an `!important`.

### The stylesheet is patched, not re-rendered

`StyleRules()` is O(distinct styles) and changes only when a style is created or
collected, so `<style id="sy">` lives in the shell and a push replaces it whole.
Creating a new look costs **50 bytes of stylesheet** plus the cells that changed.

It is a per-screen COMPARISON rather than a flag anybody has to set — the same
self-healing shape `screen.sentRows` uses — so a viewer who scrolls into a
region styled while they were looking elsewhere finds the rules already there,
and nobody had to remember to wake them. A sheet nobody has styled compares ""
against "" forever and sends nothing.

### Styled empty cells: measured, and coalescing NOT taken

`renderCells` skipped `KindEmpty`, so a background on a blank cell was stored,
reported dirty, and never drawn. The rule is now **as soon as you style a cell it
becomes live** — one predicate, `cellRendered`, shared by the renderers, by
`maskOf` (the server's model of what the browser holds) and by `renderCellPatch`
(which decides insert/morph/remove). Storage already agreed: `style <> 0` counts
as content for `UsedRows` and the insert's tail probe.

Measured in a real browser on a 450-row buffer, styling a blank 40x25 region:

| | nodes | `#g` raw | `#g` br2 | client morph |
| --- | --- | --- | --- | --- |
| blank buffer | 481 | 9,950 | 1,544 | — |
| + 1,000 styled blank cells | **1,481** | 51,475 | 3,345 | 3.3 ms (paint 35.3 ms) |
| after clearing the style | **481** | 9,950 | 1,544 | 12.4 ms |

The push that carried it: one 50 B stylesheet patch and one 41,525 B append. The
clear came back as one `remove` frame, 4,774 B naming 1,000 ids, and every
element went away — `481 -> 1,481 -> 481`, no orphans, no duplicates. **A blank
sheet nobody has styled is still 481 nodes**, which is the point of the rule
rather than a caveat.

**Coalescing horizontal runs into `grid-column: 2 / 8` was measured against and
not taken.** The saving would be 960 elements and ~1.4 KB compressed; the cost
is that `heldMask`, `T.td`, `T.filled`, `T.rawOf`, the local echo and the remove
selector all rest on one element per cell, and a span-aware mask is a second
model of the DOM to keep in step. The numbers do not justify it: 1,000 elements
morph in **3.3 ms**, against the 4.0 ms a dense scroll already pays for 4,400
cells and the 52 ms row-group mode paid for 500 wrappers. Every one of those
elements corresponds to a cell somebody deliberately formatted, which is exactly
the case sparse rendering was never trying to optimise away — its win was
13,000 cells nobody had touched.

### Column resize: a guide line instead of live reflow

Reported: resizing lags and drops frames on a dense window. The cause was not
mysterious. The drag wrote `$rw`; `#vp`'s data-style turned it into `--w-N`;
`--w-N` is a track of `grid-template-columns` on `#b`; changing a grid track
forces full layout of every grid item in the buffer, on every pointermove frame.

It now does what Excel and Sheets do: **do not resize during the drag.** One
`position:fixed` 2px guide line follows the pointer by `transform`, which the
compositor moves without layout or paint, and the real width is applied once, on
release. `$rc` stays -1 for the whole gesture, so the data-style effect does not
run at all — the drag writes NO SIGNAL, so there is nothing for Datastar to
react to.

Measured in a real browser on the seeded 10,000-row sheet at `?at=A400` — a
buffer holding **18,738 cells / 19,619 nodes** — same drag, same driver, 60
pointermove events over ~1s, before-binary against after-binary on freshly
seeded data:

| | frames | **dropped** | frames >= 33ms | p50 | p95 | max |
| --- | --- | --- | --- | --- | --- | --- |
| before | 65 | **298** | 60 | 100 ms | 116.7 ms | 150 ms |
| after | 85 | **1** | **0** | **16.7 ms** | 17.6 ms | 33.3 ms |

p50 16.7 ms is exactly the 60 Hz budget. Both runs commit the identical width
(96 -> 316); mid-drag the column is 206 px before and unchanged at 96 px after,
with the guide sitting at the pointer.

This also makes resize consistent with every other gesture here, all of which
preview with a cheap proxy and commit discretely: the editor is a floating input
over the cell, the selection is one positioned box, scrolling moves inside a
pre-rendered buffer. Resize was the only one manipulating the real thing live.

**The guide takes an axis.** `T.gdShow('x'|'y', pos)` — the horizontal case is
the same element with its width and height exchanged and `translateY` instead of
`translateX` — because variable row heights are coming and will have the
identical problem: a row height feeds `top`/`grid-template-rows` and dragging one
live would relayout the same 19,000 elements.

### What styling fought, and what it did not

- **The local echo would have stripped the style.** `T.echo` rewrites
  `className` to say what kind of cell it just painted, so committing into a
  bold red cell flashed it plain for the round trip — the exact flicker the echo
  exists to remove, reintroduced by the echo. It now reads the style class back
  off the DOM (`T.scls`) rather than keeping a copy, because the confirming morph
  is what owns the class and the echo has to follow it.
- **Delete on a styled cell is not a remove.** The cell keeps its element and
  loses its text, so the echo's clear branch had to learn the same predicate the
  server uses — and `handleCell` had to stop inferring presence from
  `InferKind(raw)` and ASK the store, on the clear path only (~10 µs on the one
  commit in a hundred that empties a cell). Without it the mask says "absent",
  the next push appends a second element with the same id, and no later patch can
  resolve it.
- **`Ctrl+arrow` had to stop meaning "has an element".** Sparse rendering made
  "has something in it" literally "has an element"; a styled blank cell breaks
  that identity, so `T.filled` is now "has an element with text" and a yellow
  blank no longer stops a walk to the end of a data block.
- **A number format needed `data-r` on one more kind of cell.** F2 on a currency
  cell would otherwise open the editor on `$1,234.50` and commit a string. The
  attribute is emitted when `Display != Computed`, which is free to test for an
  unstyled cell (the store hands back the same string for both) and absent from
  every one of them.
- **Nothing fought the incremental patch paths.** `SetStyle` returns
  `Dirty{Structural:false}`, so styling goes down `pushCells` unchanged: insert,
  morph and remove were already the three shapes, and styling only changed which
  pile a cell falls into.

### Verified

All harnesses green on the final build: `sel.mjs` 53/53, `keys.mjs` 37/37,
`range.mjs` 29/29, `range-bytes.mjs` 7/7, `extent.mjs` 30/30, `regress.mjs`
19/19 (fresh sheet, fresh Chrome profile), `flick.mjs` 14/14 at 90 and 180
ms/leg, `reject.mjs` 12/12, `viewer2.mjs` 6/6, `pending.mjs` 17/17 (LAT=90). No
console errors. `go test ./...` green, `gofmt` and `go vet` clean.

Two of `regress.mjs`'s assertions were rewritten rather than satisfied, and
deliberately: they asserted that the drag resizes at pointer rate, which is the
behaviour this change removes. They now assert that the drag previews with a
guide line at the pointer while the column does not move, and that the release
commits.

New: `/tmp/ssobs/style.mjs` 33/33 (bold/italic/colour/fill/alignment applied to a
range and surviving a scroll that replaced every row, an arbitrary hex colour
through the native picker, seven number formats displaying while the editor
opens on the raw value, a styled empty cell rendering and un-rendering on clear,
a styled cell surviving Delete, and a second real browser tab receiving all of
it by push); `/tmp/ssobs/stylenodes.mjs` 3/3 (the node measurement above);
`/tmp/ssobs/stylechip.mjs` 2/2 (the pending chip comes down, and Ctrl+A then
Bold is refused with a sentence naming the 2,000-cell cap);
`/tmp/ssobs/resize.mjs` (the frame numbers above). Screenshots in
`/tmp/ssobs/shots/sty-*.png` and `rz-after-middrag.png`.

## The overlays stopped snapshotting geometry — one derivation, not two

Reported: *"I resized a column while a cell was active and the blue outline kept
its old width."* Confirmed, and it was worse than reported. The user's own
screenshot showed the box **offset out of its column entirely**: the active cell
was `D5`, column **C** was widened, and the outline stayed at the old x as well
as the old width — so it straddled the C/D rule instead of outlining `D5`.

The cause was two coordinate systems that agreed until something moved.

|  | how it was placed | survived a resize |
| --- | --- | --- |
| `#pc` collaborator cursors, ranges, flashes | `--r` + `grid-column`, on a grid with `#b`'s own `grid-template-columns` | **yes** |
| `#sl` the server's first-paint region box | the same, inside `#b` | **yes** |
| `#ac` the active-cell outline | `top:$row*22-1; left:$x-1; width:$w+2` | **no** |
| `#ed` the editor | the same expression, shared with `#ac` | **no** |
| `#sb` the selection box | `T.selBox`, pixels read from the column rules | **no** |
| `#cb` the copy marquee | `T.rngBox` → `T.selBox` | **no** |
| `#gl` the resize guide | `position:fixed`, one transform, drag-lifetime only | n/a |
| `#mn` the header menu | `position:fixed` at the pointer | n/a |

`$x`/`$w` were **snapshots**, written from the clicked column's
`offsetLeft`/`offsetWidth` at click/hit time (four sites in `keys.go`). `#sb` and
`#cb` did read the column-rule oracle — but from a `data-style` subscribed to the
four *selection* signals, so a width change re-ran nothing.

**The asymmetry is the finding.** The remote overlays were correct because the
server re-renders them and CSS resolves their geometry; the local ones were wrong
because they were the OPTIMIZED path — made client-fast by copying the answer.

### The fix is a deletion, not a recomputation

Every local overlay is now a child of a grid carrying the sheet's own column
tracks, exactly as `#pc` always was. Two wrappers were added:

- `#oc` inside `#g` — the editor and the active-cell outline. It stays inside
  `#g` because `data-ignore-morph` only opens a hole when BOTH the live node and
  the incoming one carry it, so the server has to keep emitting `#ed`.
- `#ov` in the page shell — the selection box and the copy marquee. Shell markup
  for the reasons it always was: no push re-sends it, no morph removes it.

One CSS rule now carries all three (`#oc,#ov,#pc{…grid-template-columns:…}`), so
there are exactly two declarations of the tracks in the stylesheet — the cells and
the overlays — plus the row-group wrapper the measurement mode uses.

What the overlays carry is an ADDRESS:

```
#ac / #ed   data-style="{'--r':$row,gridColumn:($col+2)+'/'+($col+3)}"
#sb / #cb   T.selBox → {'--r':r0,'--n':rows,gridColumn:(c0+2)+'/'+(c1+3)}
```

and the -1/+2 overlap that makes the outline read as a border ON the cell moved
into CSS with it (`left:-1px;width:calc(100% + 2px)` against the grid area, where
`100%` is the column's current width). **`$x` and `$w` are gone as signals.** They
were ordinary (non-underscore) signals, so every request the page made was
carrying two numbers only the client's stylesheet could ever have been right
about: the viewport command's body is **351 → 338 B**.

`T.selBox` now reads no DOM at all. The column-rule oracle survives for the two
jobs that genuinely need pixels and cannot be expressed as an address: `T.colAt`
(which column is under this pointer) and `T.box` (how far to scroll to reveal a
column).

**Nothing recomputes on pointermove, and nothing had to.** The guide-line design
is untouched: `--w-N` changes only on commit, and the overlay boxes relayout in
the same pass that relayouts the cells underneath them. The drag on a dense
window (11,022 cells / 11,554 nodes at `?at=A400` on the 10,000-row sheet, 60
pointermoves) drops **0** frames after against **1** before, p50 16.7 ms, max
17.7 ms against 33.5 ms — same binary pair, same driver. The 298-dropped-frame
regression the guide line exists to prevent stays prevented.

Deciding not to move the boxes DURING the drag was deliberate. The columns
themselves do not move during the drag, so an outline that followed the guide
would stop outlining the cell as drawn.

### Measured in a real browser, before-binary against after-binary

`/tmp/ssobs/geom.mjs`, same script, same driver, two real tabs:

| | before | after |
| --- | --- | --- |
| widen column **C**, active cell `D5` — outline x | 343 (col D is at 424) | **423** = col D − 1 |
| …outline width | 98 (col D is 186 wide) | **188** = col D + 2 |
| widen column **D** itself | unchanged at 98 | **follows** |
| editor over `D5`, a SECOND viewer widens C | stays at x 343 | **363** = col D − 1 |
| selection `B8:E12`, widen C | 548 px wide, stops inside E | **618** = B..E exactly |
| copy marquee, widen C | stale | **follows** |
| collaborator cursor + flash, widen C | already correct | **still correct** |

Screenshots: `/tmp/ssobs/shots/geo-before-*.png` and `geo-after-*.png`. The
before pair reproduces the report exactly — `geo-before-2-after-resize-left.png`
shows the outline straddling the C/D rule with `D5` in the toolbar, and
`geo-before-5-selection-after-resize.png` shows the selection stopping mid-E
while the collaborator's cursor sits correctly on F.

### The three "same class" cases

1. **Insert a row above the active cell, or a column to its left.** The
   selection is ADDRESS-STABLE: the content moves and `$row`/`$col`/`$ref` do
   not, so `D5` stays `D5` and the marker that was in it is now in `D6` (verified,
   `/tmp/ssobs/shift.mjs`). The outline is still geometrically exact — it is on
   column D wherever column D now is. This is Excel's rule rather than Sheets',
   and it is **self-consistent**: the server's held selection (`registry`,
   `presence`) is not shifted either, so the cursor every other viewer sees agrees
   with the local box. Making it content-stable is a whole-system change, not a
   geometry fix — the client half alone would desync the local box from the
   collaborator cursor — and `registry.go`/`presence.go` are outside this change.
   Stated as a limitation, not fixed.
2. **A resize while a range is selected.** Was broken (`#sb` kept its old
   rectangle); fixed by the same change, verified above.
3. **A resize by another viewer.** Was broken locally, correct remotely. The push
   patches `_wN` and every overlay is now downstream of those custom properties
   through CSS, so it follows a width change nobody here initiated — verified with
   two real tabs, above.

### The bytes

Same anchor, same sheets, before-binary against after-binary, side by side:

| | before | after |
| --- | --- | --- |
| `#g`, blank sheet (250-row buffer) | 10,278 raw / 1,295 br2 | **10,261 / 1,283** |
| `#g`, seeded 10,000-row sheet (500-row buffer) | 469,712 raw / 50,562 br2 | **469,695 / 50,545** |
| a cell | `<b id="A1751" style="--r:1750">250</b>` | **byte-identical** |
| all 11,020 cells of the dense window | 448,969 B | **448,969 B** |
| all 500 row numbers | 19,500 B | **19,500 B** |
| one-cell edit patch | 55 B | **55 B** |
| the same edit inside a selected range | 55 B | **55 B** |
| the viewport command's body | 351 B | **338 B** |
| page shell (once per load, never re-sent) | 47,000 B | 47,693 B (**+693**) |

**Per cell: nothing, again.** `#g` got 17 bytes SMALLER: the two overlay
expressions lost 13 bytes each and the `#oc` wrapper cost 19. The +693 of shell is
the `#ov` wrapper, the four overlay CSS rules, the pending-chip backstop and the
`pw` classes. `#g` is also unchanged in *shape* — nothing about the render became
viewer-dependent, and the four overlays that are per-viewer are still entirely
outside `#g`.

(These are not the 9,950 / 217,727 the styling section records: those were taken
at a different buffer width. The claim here is the before/after pair on one
configuration, measured on the same machine minutes apart.)

### A fan-out refusal is a resource limit, not bad input

`maxRecalcNodes` is now 5,000, and `ErrRecalcTooLarge` was reaching the client as
a **500** — "the server broke" about a perfectly well-formed edit. It is now its
own classification (`commandStatus` in http.go, used by `/cell`, `/clear`,
`/fill`, `/paste`, `/style` and the structural commands) and answers **400** with
a sentence of its own rather than inheriting `ErrBadRef`'s.

Measured against a sheet with 5,100 cells depending on `A1` (`-limits=false`):
editing `A1` returns **500 before, 400 after**, and in the browser the reader now
sees *"That change affects too many cells to recalculate in one go — try a
smaller range."* while the optimistic value reverts (`/tmp/ssobs/shots/cap-refused.png`).

**And the pending chip can no longer get stuck.** `#cl`/`#fl`/`#pv`/`#st` and the
six menu buttons raise `$p`, and the server lowers it — on the push that carries
the result, or on the notice a refusal sends. That has a hole: every refusal that
happens BEFORE the handler knows which screen asked (an unreadable body, a bad
sheet id, an index out of range, a rate limit, a 500 from anywhere) sends no
notice, and the chip sat there until the 20-second safety timer. The client now
closes it from `datastar-fetch` `type:'error'`, keyed on a `pw` CLASS rather than
a list of ids so the backstop can only fire for an element that actually raised
the chip — and it writes `$note` only when it is still empty, so the server's
specific sentence wins whichever arrives first. Verified by intercepting `/clear`
at the network layer and answering 400 with a body the server never produced: the
chip goes to *"That command was refused."* instead of spinning
(`/tmp/ssobs/chip.mjs`).

### Verified

All harnesses green on the final build: `regress.mjs` 19/19, `sel.mjs` 53/53,
`keys.mjs` 37/37, `range.mjs` 29/29, `range-bytes.mjs` 7/7, `extent.mjs` 30/30,
`style.mjs` 33/33, `stylenodes.mjs` 3/3, `reject.mjs` 12/12, `viewer2.mjs` 6/6,
`flick.mjs` 14/14 at 90 and 180 ms/leg, `pending.mjs` 17/17 (LAT=90),
`presence.mjs` 23/23, `bfcache.mjs` 9/9 (in a Chrome WITHOUT
`--disable-features=BackForwardCache` — `chrome.sh` sets that flag, and its
control case asserts a bfcache ghost, so it cannot pass there), `handover.mjs`
6/6, `resize.mjs` 4/4.
New: `geom.mjs` 15/15, `shift.mjs` 5/5, `chip.mjs` 3/3. No console errors.
`go test ./...` green, `gofmt` and `go vet` clean.

Two harness READERS were rewritten, and neither assertion was: `lib.mjs`,
`sel.mjs` and `range.mjs` read `#ac`/`#sb`/`#cb`'s inline `style.top`/`left`, which
no longer exists because the browser resolves the rectangle now. They measure the
RENDERED box against `#g`/`#vp` instead. Every expected number is unchanged —
`acLeft === '343px'`, `box left = column B`, `box width = 3 columns` — and it is a
strictly better test of the same behaviour, which is the same trade `regress.mjs`
made when the resize drag stopped reflowing the grid.

`regress.mjs` needs a `demo` sheet TALLER than 1,000 rows to score 19/19: its
`?at=A2000` clamps to the bottom of a default-sized sheet, where every scroll
event legitimately sits inside the buffer-edge guard and the "zero viewport
commands" assertion becomes a coin flip. Both binaries score 18/19 there and
19/19 on `-seed-rows 5000`. Not caused by this change; noted so the next run does
not chase it.

New Go regression tests (`overlay_test.go`) pin the property rather than the
pixels: no overlay may carry `$x`/`$w` or compute a `px`, the three overlay grids
must share one `grid-template-columns`, every element that raises `$p` must wear
the backstop class, and `ErrRecalcTooLarge` must classify as 400 with a sentence
that is not "bad cell reference".

## The formula bar — a second input on the same signals

A cell shows the ANSWER and a formula is routinely wider than its column, so
`=SUM(A1:A200)*B7/12` in a 96px cell is neither readable nor editable — and the
in-cell editor that opens on it is the same 96px wide. The bar is the one place
on the page where the sheet's SOURCE is legible.

**It cost one new signal writer and no new state.** `$ref` and `$raw` already
existed and `#ed` already carried `data-bind:raw`; `#fx` carries the same
attribute. Datastar's text-input bind adapter is `{get: el => el.value,
set: (el,v) => {el.value = v}}` plus an `input` listener and a plain effect on
the write side, so N inputs bound to one signal are N mirrors of one value —
whichever one holds focus writes it, all of them read it. Nothing had to be
added to make the cell and the bar agree.

### The one gap was that `$raw` was write-only

`$raw` was an EDIT BUFFER: loaded from the cell when an edit STARTED (F2,
double-click, a printable key) and stale garbage from the previous edit at every
other moment. That was invisible while its only reader was an editor hidden
behind `$editing`. A bar that is always on screen reads it all the time, so
`$raw` had to be promoted to "the active cell's text, which the user may be
part-way through changing".

That promotion is ONE `data-effect`, on the bar's own wrapper, and it is one
subscription rather than a call on each of the twelve gestures that move the
active cell (click, pointerdown, the drag's collapse, eight keyboard movements,
the move after a commit, and the boot anchor). It is the same argument that puts
the local echo in `cellPost` rather than on five keys: a resync on eleven of
twelve doors is a stale formula bar on the twelfth.

```
const e=$editing,r=$ref,q=$row,lo=$blo,hi=$bhi,v=$raw;
if(e||!window.__ss||r===''||q<lo||q>hi)return;
const s=window.__ss.rawOf(r);if(s!==v)$raw=s
```

Every signal is read BEFORE the guard on purpose: Datastar re-tracks an effect's
dependencies on each run, so an early `return` that skipped a read would drop
that signal from the dependency set and the effect would stop waking for it.

The alternative — a second signal the server patches with the active cell's raw,
next to `_ag` — was rejected: two signals holding one cell's text is two things
that can disagree, and every commit path in keys.go reads `$raw`.

### Focus arbitration: focus is exclusive, and a transfer of focus commits

Two inputs on one signal fight over exactly one thing, the caret, and only if
both can hold it — which they cannot, because a document has one
`activeElement`. The rules, stated rather than left emergent:

- **Focusing the bar opens edit mode** (`$editing=true`, the same flag F2 raises).
  That is Sheets, and here it is load-bearing in three places that already
  existed: `vpKeyExpr` refuses the grid keymap while `$editing`,
  `vpPointerDownExpr` COMMITS on any pointerdown in the grid while `$editing`,
  and the sync effect stands down. Without it, clicking a cell after typing in
  the bar would silently discard the typing.
- **The cell editor appears but does not take focus.** `#ed`'s `data-show` is
  `$editing`, so the cell shows an edit box mirroring the bar. Only `T.edit` and
  `T.focus` ever call `focus()` on it and neither runs from the bar.
- **Enter/Tab commit and move** (shift reversing each), through the same two
  events `editorKeyExpr` uses.
- **Escape abandons, and the revert is not code.** Lowering `$editing` wakes the
  sync effect, which puts the cell's own raw back into `$raw`, which both inputs
  then show. Escape in the CELL editor reverts the bar by the identical path.
- **Any other blur commits**, which is keys.go's rule for `#ed`.

One case is coarser than Sheets and is stated rather than hidden: clicking from a
live in-cell edit INTO the bar blurs `#ed` and `editorBlurExpr` commits. The fix
is a `relatedTarget` test inside `editorHTML`, and `#ed` is rendered inside `#g`
— so it would cost grid bytes for a keystroke nobody loses. The intermediate
commit writes the text the user had typed, `$raw` is untouched, and the edit
continues in the bar.

### It issues no request, and it has no sheet id

`formulaBarHTML()` takes no arguments. `/cell` still has exactly ONE issuer,
`#ed`: Datastar keys request cancellation on the element, and the local echo,
the pending marker and the failure revert all live in `cellPost` and all key off
`d.el.id==='ed'` in echoScript's `datastar-fetch` listener. A bar with its own
`@post` would have been a second commit path with none of that, and `/cell`'s
`requestCancellation:'disabled'` — the fix for two fast commits aborting each
other — would have been silently halved.

So the bar dispatches `commitEvent` on the window and `#ed` answers it, which is
precisely what Delete-to-clear and the pointerdown-commit already do. Four doors,
one command, one issuer.

### What it shows when the active cell is off-screen: the right thing

This falls out of `$raw` being a SIGNAL rather than a DOM read. Every way the
active cell can move ends with it inside the rendered buffer, because
`applyMoveExpr` calls `T.reveal` and the `?at=` anchor IS the buffer — so the
sync always runs against a cell that has an element (or has none because it is
genuinely empty, which is the same answer). Scrolling away afterwards does not
move `$ref`, so nothing re-reads and the signal keeps what it was given.

The `$row` within `$blo..$bhi` guard is exactly for that: the effect also
subscribes to the SERVER-owned buffer bounds so a window arriving after a jump
re-syncs against real DOM (`patchBounds` runs after the elements patch, so waking
on them is waking on DOM that has landed), and without the guard that same
subscription would fire while the active cell is scrolled out of the buffer and
overwrite a correct `$raw` with the empty string `T.rawOf` returns for a cell
with no element. Verified in a browser: select `D9` holding `=SUM(A1:A10)`,
scroll to row 380 until `document.getElementById('D9')` is null, and the bar
still reads `=SUM(A1:A10)` (`/tmp/ssobs/shots/fb-f-5-offscreen.png`).

**Known limit, stated rather than hidden:** if ANOTHER viewer changes the cell
you have selected while you are looking at it, the bar keeps showing the raw it
last read. The cell under it updates — it is an ordinary morph — but an edit push
patches elements and nothing the effect subscribes to. Fixing it needs a signal
on the edit path, which is a wire cost on every edit.

### Number formats

The bar shows RAW, always. A currency cell renders `$1,234.50` and carries
`data-r="1234.5"`; `T.rawOf` already prefers `data-r` over `textContent`, which
is the rule F2 and the double-click have always followed. The bar is a third
reader of one predicate, not a fourth spelling of it. Measured in a browser:
selecting the cell shows `1234.5` in the bar and `$1,234.50` in the grid, and
committing `1234.56` through the bar leaves `<b id="C3" class="s1" style="--r:2"
data-r="1234.56">$1,234.56</b>` (`/tmp/ssobs/shots/fb-f-2-currency.png`).

### The bytes

|  | before | after | delta |
| --- | --- | --- | --- |
| `#g` on a blank sheet, raw | 9,933 | **9,933** | **0** |
| one-cell edit patch, elements payload | 44 | **44** | **0** |
| page shell raw (once per load, never re-sent) | 57,684 | 59,060 | +1,376 |
| whole page br q5 | 15,552 | 15,724 | +172 |

**Per cell: nothing, and per render: nothing.** The bar is page-shell furniture
between `<header>` and `#vp` — the same terms `#sb`, `#cb`, `#ov` and the
aggregate line are on. No push re-sends it, no morph can remove it, and it needs
no `data-ignore-morph` because it is outside every patch region. That is also
why it can hold focus across an unrelated viewer's edit landing in the same band:
the morph rewrites cells inside `#b` and cannot see `#fb`.

The grid's viewport gives up exactly the bar's height — `#vp` is
`calc(100vh - var(--tb) - var(--fb))` — and that is the whole of the layout
change, because every other piece of viewport arithmetic on the page reads
`vp.clientHeight`: `scrollExpr`'s row window, `T.pageRows`, `T.reveal`,
`T.seek`. A number duplicated into any of those would be the stale-offset bug
(the selection one row off, silently). Measured in the browser: window 813,
toolbar 44, bar 28, `#vp.clientHeight` 741.

### The cell reference is deliberately NOT repeated

Sheets puts a Name Box at the left of its formula bar because Sheets has no other
readout. This page already binds `$ref` in the header one row above
(`header .r`, always visible, never scrolled away). A second element bound to the
same signal is a second thing that can be wrong, for a fact that is already on
screen 28 pixels away. The bar carries an `fx` glyph instead, which is what makes
a full-width text field read as a formula bar rather than as a stray search box.

### Verified, and the one number that moved

The bar's own harness (`/tmp/ssobs/fbar.mjs`, real Chrome over CDP) is **33/33**,
no console errors: the strip exists and `#vp.clientHeight === innerHeight - 44 -
28`; a formula cell reads `=A1*2` in the bar and `42` in the grid; a currency
cell reads `1234.5` in the bar and `$1,234.50` in the grid; an empty cell empties
the bar; focusing the bar opens `#ed` without taking the caret from `#fx`; typing
in the bar mirrors into `#ed`; Enter commits `=A1*3` (server reads `63`) and moves
to B4; Escape restores `=A1*3` in both inputs and commits nothing; F2 puts the
caret in `#ed` and typing there mirrors into the bar; `D9`'s `=SUM(A1:A10)`
survives scrolling to row 380 with `document.getElementById('D9')` null; a
neighbouring viewer's edit landing mid-type leaves `selectionStart/End` and
`activeElement` unchanged; clicking another cell commits the bar's edit and
moves the bar; Tab commits and moves right.


Every pinned harness re-run on the shipping binary: `regress.mjs` **19/19**,
`sel.mjs` **53/53**, `range.mjs` **29/29**, `range-bytes.mjs` **7/7**,
`extent.mjs` **30/30**, `style.mjs` **33/33**, `stylenodes.mjs` **3/3**,
`geom.mjs` **15/15**, `shift.mjs` **5/5**, `presence.mjs` **23/23**,
`reject.mjs` **12/12**, `viewer2.mjs` **6/6**, `handover.mjs` **6/6**,
`bfcache.mjs` **9/9**, `flick.mjs` **14/14** at 90 and 180 ms/leg,
`pending.mjs` **17/17** (LAT=90). No console errors anywhere.
`go test -count=1 ./...` green, `gofmt` and `go vet` clean.

`keys.mjs` scores **36/37 in a 900px-tall browser and 37/37 in a 928px-tall
one**, and the difference is the bar's own 28 pixels rather than anything the
bar does. `T.pageRows()` is `floor((clientHeight - CH) / RH) - 1`, so a viewport
28px shorter pages by 31 rows instead of 33; the harness's fixed 14 PageDowns
therefore travel 434 rows instead of 462, which lands 14 rows short of where the
buffer sheds its first band, and the assertion that the buffer's FIRST row must
have advanced sits inside that gap. A/B, same machine, minutes apart:

| binary | window | 14 PageDowns land on | buffer | score |
| --- | --- | --- | --- | --- |
| before | 1280x900 | A449 | r201..r700 | 37/37 |
| after | 1280x900 | A435 | r1..r500 | 36/37 |
| after | 1280x**928** | A449 | r201..r700 | **37/37** |

At 928 the after-binary reproduces the before-binary's trace exactly, including
the later `A1729 / r1451..r1950` after 54 PageDowns — which both binaries reach
at 900 as well, because 54 pages is far enough that 31 vs 33 stops mattering.
The buffer arithmetic agrees with the viewport; the harness's page count is what
is height-dependent. Recorded here rather than fixed, on the same terms as
`regress.mjs` needing `-seed-rows 5000`: the assertion is right and the driving
is what the geometry moved.


## The way out of a fixed-size sheet — a foot control, and 10,000 rows to start

Reported: *"seems like in prod i can't scroll past 1001"*. Measured against
production before anything was written, and the report was exactly right:

| | | |
| --- | --- | --- |
| blank sheet | | `--rows:1000` |
| viewport command past the end | 204 | **still 1000** — does NOT grow |
| write past the end (`A1500`) | 204 | now 1500 — grows correctly |

**The sheet grew on write and never on scroll**, so a new sheet was a 1,000-row
box with no door.

### Auto-growing on scroll was refused, and the reason is the architecture

A viewport command is a **read**. SPEC.md's division is that commands write and
views read, and a read that silently mutates persistent, SHARED state breaks it
in the most surprising way available: an idle reader flicking a scrollbar would
permanently enlarge a document for everybody else looking at it, with nothing in
the log naming who did it. It is also unbounded in the direction a scrollbar drag
is cheapest — one throw of the wheel is a million rows.

The affordance is a **command**: attributable in `structure` like every other
write, rate limited (the `/rows` verb is already `classWrite`), refusable at the
ceiling with a sentence, and the behaviour every spreadsheet user already knows.

### Where it is, and why that answers "always present or proximity-triggered"

One absolutely positioned bar inside `#vp`, a sibling of `#g`, at
`top: calc(var(--ch) + var(--rows) * var(--rh))` — the first pixel past the last
row. That one declaration makes it **always in the DOM** (no scroll handler, no
threshold, no signal, no effect) and **only ever visible at the foot** (because
that is where it is), and it **follows the extent for free**: `--rows` is already
a server-patched custom property on `#vp`, so the same patch that makes the
scroll container taller walks the bar down to the new bottom.

### It costs the grid and the cell nothing — A/B, three binaries, one machine

| | blank `#g` | one-cell edit patch | page shell |
| --- | --- | --- | --- |
| `DefaultRows` 1,000, no control | **9,906 B** | `<b id="B3" style="--r:2">42</b>` | 61,710 B |
| `DefaultRows` 10,000, no control | **9,906 B** | identical | 61,714 B |
| `DefaultRows` 10,000, **with** the control | **9,906 B** | identical | 62,859 B |

`#g` is byte-identical across all three; so is the element frame of a one-cell
edit. The whole feature is **1,145 bytes once, in the page shell**, and the
10,000-row default is **4 bytes** (the `--rows` literal). Zero per render, zero
per cell.

### The extent reaches every open screen for 62 bytes

A bystanding viewer who touched nothing, measured on the wire with
`accept-encoding: identity`:

```
event: datastar-patch-signals
data: signals {"_rows":11000}
```

62 bytes total. The server log for the click:

```
structure  op=ap  at=10000  n=1000  grew=1000  screens=0
extent     rows=11000  grew=1000  screens=2
```

`screens=0` on the structural line is the point: **an append is the one row
operation that moves nothing**, so unlike `ia`/`ib`/`d` it does NOT mark every
screen for a whole-window morph. Every rendered window is still correct; what
those screens are owed is one integer, and the two pushes that carry it have
their grid payload suppressed by the digest.

### In a real browser (CDP, two viewers, 20 checks, all PASS)

- blank sheet is 10,000 rows; `#g` is 220,000 px; scrolling to the end lands on
  the last row
- the bar is at `24 + 10000*22 = 220,024` px and visible there
- click Add: `--rows` 10,000 -> 11,000, `#g` 242,000 px, `T.rows` 11,000, the bar
  walks to 242,024 px
- **viewer two, which touched nothing, grew too** — `--rows`, container and
  client clamp all follow
- scrolled to row 10,498 (rows that did not exist a minute earlier), clicked
  `C10501`, typed `hello`, committed — read back off a fresh server render
- ceiling: a 1,000,000-row sheet refuses with
  *"This sheet is already 1,000,000 rows tall, which is as tall as a sheet gets.
  No rows were added."* — a toast, not a 500, and the extent does not move
- keyboard: focus the button, press Enter — the sheet grows and the selection
  does **not** move. That needed `data-on:keydown="evt.stopPropagation()"`,
  because `vpKeyExpr` listens on the window and excuses INPUT/TEXTAREA/SELECT but
  not a button, so Enter was being answered with `preventDefault` (move down one
  row) and never became a click.
- no console errors on either viewer

Screenshots: `/tmp/ssobs/shots/grow-1-foot-at-bottom.png`,
`grow-2-viewer-two-grew.png`, `grow-3-typed-in-new-rows.png`,
`grow-4-ceiling-refusal.png`, `grow-5-keyboard.png`.

### The one observable that moved: 34 px of scroll past the last row

The bar is part of the scrollable content, exactly as it is in Sheets, so
`scrollHeight` is `footHeightPx` larger than the grid. **No row arithmetic
changed** — every row-from-scrollTop calculation is `floor((y - CH) / RH)`
measured from the top, `#vp`'s height still gives up only `--tb` and `--fb`
(`vp=741 win=813 tb=44 fb=28`, unchanged), and `T.pageRows()` reads clientHeight.

`extent.mjs` computed the bottom of the scroll range as `rows*RH + CH -
clientHeight` and was short by exactly 34 px in two places. It reads the bar's own
height now. **The corrected harness scores 30/30 on the build WITHOUT the bar**
(where it reads 0), which is what makes it a correction and not an accommodation.

### DefaultRows 1,000 -> 10,000: seven hashes, one line each

Storage is sparse and rendering is windowed, so a blank 10,000-row sheet stores
zero cells and renders the same 531 nodes. The band table goes 20 -> 200 bands
(~0.3 ms, extrapolated from the measured 29-35 ms for 20,000) and the scroll
container 22,000 -> 220,000 px, three orders of magnitude inside Firefox's ~17.9M
px element limit. **The real cost is the scrollbar** — 20 rows of data in a
10,000-row container is a thumb 0.2% of the track — and it is accepted rather
than hidden; the better fix (size from `UsedRows()` plus slack) is written up in
BACKLOG.md.

The same seven golden hashes that moved when this went 10,000 -> 1,000 moved
back, all deletes, because the delete floor IS this constant. Verified by the
method that change established rather than regenerated: **exactly one line differs
in each**, it is a `# window` header, and patching only that line back reproduces
the old hash byte for byte.

| case | last window | delta |
| --- | --- | --- |
| `DeleteRows(0,1)` | 2,574 -> 2,600 cells | 1 row x 26 |
| `DeleteRows(48,4)` band edge | 2,496 -> 2,600 | 4 rows |
| `DeleteRows(60,120)` whole bands | **0** -> 2,600 | window was past the end |
| insert then delete the same row | 2,574 -> 2,600 | net 1 row |
| interleaved edits and structure | 2,548 -> 2,600 | net 2 rows |
| rows then cols | 2,574 -> 2,600 | net 1 row |
| FullSheet `DeleteRows(0,1)` | 7,774 -> 7,800 | window 9700..9999 |

No cell moved, no value changed, no event changed, no width changed, no
dependency edge changed. The other nine hashes did not move in either direction,
which is `goldenExtent` doing the job it was introduced for.

### Harness sweep, this build

`regress` 19/19, `sel` 53, `range` 29, `range-bytes` 7, `extent` **30/30**,
`style` 33, `stylenodes` 3, `geom` 15, `shift` 5, `fbar` 34, `lat` 23/23 (0 FAIL;
the baseline binary also scores 23, so the pin of 24 is stale), `presence` 23,
`handover` 6, `reject` 12, `viewer2` 6, `flick@90` 14, `flick@180` 14,
`pending@90` 17, `bfcache` 9/9. `keys` 36/37 — the one FAIL is the formula bar's
28 px at a 900 px window, documented above, and the **baseline binary scores the
same 36/37 with the same single failure**.
