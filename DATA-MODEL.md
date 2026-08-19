# Data model — what we changed, what it cost, what's left

A record of the storage-model work, written because the measurements repeatedly
contradicted the reasoning that motivated them. Numbers here are from an **idle**
machine unless flagged otherwise — see the warning at the bottom, which is the most
transferable thing in this document.

## The underlying defect: position *was* identity

`cells` was keyed `(row, col)`, which is simultaneously the primary key and the display
coordinate. That conflation is why insert-row was O(sheet): changing where something
appears required rewriting what it *is*, plus every formula naming it by position.

Everything below is an attempt to pay less for that without giving up the property that
makes reads fast — `cells` is `WITHOUT ROWID`, so a window read is a contiguous PK range
scan that emerges already sorted, pinned by `TestHotPathQueryPlans`.

Changes 1 and 2 attacked everything *except* that conflation and bought 10-20%. Change 3
attacked the conflation itself and bought 100x. The order that happened in is the lesson:
two rounds of removing derived data from the hot path only made it clearer that the
primary key was the problem.

## Change 1 — structural references

**Was:** references stored as A1 text inside `cells.raw`. A row insert had to parse and
rewrite every formula, then re-derive the dependency graph.

**Now:** `ref0_row`, `ref0_col`, `ref1_row`, `ref1_col`, `ref_span` columns on `cells`.
The formula grammar is capped at two operands or one range, so two slots suffice.
`cells.raw` holds a template; the store materializes A1 display text on read, which is
why the render layer needed no change at all.

**Result:** total elimination of what it targeted. A worst-case insert now reports
`formulas seen 0, rewritten 0, recomputed 0, cells re-persisted 0`, against 20,402 of
each before.

**And yet the totals barely moved**, because two costs grew to fill the gap:

1. `cells` rows got wider — five more columns in every row of a clustered b-tree
   rewrite.
2. `deps` had to be *translated* rather than discarded. The old code had a shortcut:
   when every formula was being rewritten anyway, drop the whole `deps` table and
   re-derive it in one statement. That was faster than translating 40,509 edges.

**The lesson: the bottleneck was never the text.** It was data movement, and we added
more data to move. Removing 20,402 formula rewrites bought less than adding five columns
cost.

## Change 2 — delete the `deps` table

Once references live on `cells`, the reverse lookup ("who depends on this cell") can come
from an index on those columns. `deps` was a second derived structure maintained on every
write for a query the primary table could already answer.

**Design.** Four **partial** indexes. Partial is load-bearing: 20,402 of 220,382 cells
hold a reference and only 202 hold a range, so the indexes carry ~30,800 entries rather
than one per cell.

Ranges are the wrinkle — "which spans contain this cell" is an interval query, and a
b-tree can only drive on one of two inequalities. So there are **two** span indexes, on
the low and high endpoint, and the query picks whichever is nearer its edge of the grid.
Probing row 9,500: 190 index entries visited driving on the low endpoint, **10** on the
high one. The scan is bounded by the number of range formulas — never by sheet size or
range width.

**Measured (idle machine, 9,999 x 26 — 220,382 cells, 20,402 formulas, 202 ranges):**

| operation | with `deps` | without |
| --- | --- | --- |
| `InsertRows(0,1)` | 385 ms | **331 ms** (−14%) |
| `InsertRows(9500,1)` | 80 ms | **62 ms** (−22%) |
| `DeleteRows(0,1)` | 369 ms | **327 ms** (−11%) |
| range-formula edit | 358 µs | **280 µs** (−22%) |
| single-cell edit, cascade edit | — | parity |
| file size, VACUUMed | 6.51 MB | **5.85 MB** (−10%) |

**39 ms of the saving comes back as index maintenance** on the 220k-row shift, isolated
by dropping the indexes and re-measuring. Verdict: keep it, but it is a 10-20% win, not
the elimination the reasoning predicted.

The cascade edit being *parity* is the informative part: its cost is evaluation, not edge
writes. The win landed exactly where the hypothesis said it would — a formula whose own
reference set changes no longer writes rows to a second table for one keystroke.

### Two traps found while doing it

- **`UNION` vs `UNION ALL` was a 220,000-row landmine.** `UNION … ORDER BY 1,2` makes the
  planner want each arm pre-sorted, so it read the span arm as `SCAN cells` — a full
  table scan *per cascade hop*. `UNION ALL` plus a Go-side sort fixes it. All four arms
  are now pinned by `TestHotPathQueryPlans`.
- **Statement preparation dominated at small scale.** Three statements instead of one
  tripled per-lookup cost (16.1 µs vs 6.5 µs); prepared once per `*Sheet` handle it is
  3.6 µs. Without that the edit path regressed 27%.

### An optimization rejected with numbers

Dropping the indexes around the shift and rebuilding after — the trick the old
`deps_reverse` code used — improved top-insert 354 → 329 ms but made bottom-insert
**63 → 102 ms**, because the rebuild is a fixed full-table cost regardless of how few rows
moved. Reverted.

## Where the remaining cost was

A worst-case insert was ~331 ms and **~90% pure b-tree rewriting** with almost no
computation in it: shift cells ~299 ms, formula scan ~9 ms, everything else ~0.

That was the honest price of clustering on `(row, col)`, and clustering is what buys the
windowed read. There is no clever way to have both — only a different shape. So the shape
changed.

## Change 3 — position stops being identity (storage keys + bands)

**Was:** `PRIMARY KEY ("row", col)`. The display coordinate WAS the primary key, so
changing where a row appears meant rewriting what it *is*, for every row below it.

**Now:** `PRIMARY KEY (k, col)`, where `k` is a **storage key** handed out by a table of
**storage bands** — `bands(idx, base, slots, nrows)`, ~200 rows, each owning a
deliberately sparse 4,096-slot interval of the key space and recording how many display
rows it holds. Display rank is the prefix sum of those counts, so

    rank(k) = rows in every earlier band + (k - base of its band)

and inserting a row into one band renumbers every row below it **without writing to
them**: slots shift inside that band only, and one integer absorbs the renumbering of the
rest of the sheet. References are stored **as keys**, so a formula naming a row that did
not move needs no write however far its rank shifted. A storage band is not a display
band (`BandOf(row) = row/50`); they drift apart the moment anything is inserted.

The read path is untouched, and that is the property the whole thing rests on: band bases
ascend with display order, so a contiguous run of display ranks is still a contiguous run
of keys, and the window read is the same single ordered primary-key range scan with no
sort step. `TestHotPathQueryPlans` pins it, over keys now.

**Measured** on the same 9,999 x 26 fixture, same machine, load ~2.2 (another agent
working alongside; the baseline row reproduces this document's idle figures):

| operation | (row, col) | (k, col) | cells moved |
| --- | --- | --- | --- |
| `InsertRows(0,1)` | 335 ms | **3.1 ms** | 220,382 -> 1,102 |
| `InsertRows(9500,1)` | 76 ms | **3.2 ms** | 10,998 -> 1,102 |
| `DeleteRows(0,1)` | 328 ms | **4.4 ms** | 220,380 -> 1,078 |
| rejection on a full grid | 89 µs | 83 µs | — |
| window read, 250 rows | 5.90 ms | **5.96 ms** | (A/B, one process) |
| single-cell edit | 30 µs | **31 µs** | (A/B, one process) |

The real result is not the 107x. It is that **the cost no longer depends on where in the
sheet the mutation happens** — 3.1 ms at the top against 3.2 ms at row 9,500, where the
old implementation spanned 335 ms to 76 ms. The worst case stops existing rather than
getting cheaper.

Both hot paths were re-measured as an A/B in ONE process against a byte-identical
row-keyed copy of the same cells (`TestShiftLabWindowAB`), because that is the only
instrument this document has found trustworthy. Parity, ~1%.

### What it costs, honestly

- **Band splits.** A band that has absorbed 50 inserts holds 100 rows, so every further
  insert into it moves twice as many cells. At `bandSplitRows` it is split in two, which
  is one bounded shift (~1-3 ms) per ~50 inserts into the same band. A split takes the
  upper half of the band's own interval, so bases still ascend and the read path never
  notices.
- **Rebalances, the one O(sheet) operation left.** A split *halves* the interval it
  divides, so it makes no new slack. A band that has been split down to its last slots —
  ~330 inserts at one position, or a single insert of more rows than a band can hold —
  forces a re-key of the whole sheet into a fresh layout. **Measured: 400 ms** for
  220,382 cells, once per ~330 same-position inserts, so ~1 ms amortized. A 500-insert
  hammer at one position on a full sheet: 491 plain at 0.49 ms, 8 splits at 1.04 ms, 1
  rebalance at 400 ms.
- **Column ops are untouched and still O(sheet).** A column is not a contiguous range of
  the primary key, so inserting one still rewrites every cell to its right. The same trick
  would work on that axis; the axis is 26 wide, so it was not worth it yet.

### Migration

A v3 file cannot be read as a v4 file — 4,096 keys stand between one display row and the
next, so every cell would be filed at a key meaning a different row and *nothing would
error*. So `PRAGMA user_version` gates a rebuild of the table on open, once, inside one
transaction. **Measured on a real 220,382-cell v3 file written by the shipped binary:
459 ms, and all 220,382 cells verified identical afterwards** (`TestMigratesRealV3File`,
which reads the v3 table directly before the conversion and compares cell for cell). The
file roughly doubles in size until SQLite reuses the freed pages; a `VACUUM` reclaims it.

## Change 4 — the row cap goes away

**Was:** `MaxRows = 10000`, a compile-time constant, baked into `Window`'s clamp, the
band index's `validate()` (`rows` had to equal it exactly), `maxKeyBands`, `ParseRef`,
and the would-truncate rule. A sheet seeded to all 10,000 rows could **never** accept a
row insert again, and the refusal — "the last row has content, and the sheet is a fixed
10,000 rows" — was accurate and useless. `main.go` carried a workaround: seed 500 rows,
not 10,000, so the last row stays empty.

**Now:** a sheet has **two row extents**, and conflating them is the bug this change is
mostly about not having.

| | what it is | what reads it | where it lives |
| --- | --- | --- | --- |
| **allocated** | how tall the grid nominally is | the scroll container, the viewport clamp | `(*Sheet).Rows()` |
| **used** | one past the last row holding data | jump-to-end, "how big is this really" | `(*Sheet).UsedRows()` |

Neither is a constant and neither is derived from the other. A blank sheet has a full
allocated extent and **zero stored cells** — there has to be somewhere to scroll and
start typing.

**The allocated extent is the band index's `rows` field**, which is the prefix sum of
`bands.nrows` — already persisted, already immutable-snapshot-protected by `idxMu`, and
already recomputed by `reindex()` on every reshape. No new table, no new column, nothing
to cache and nothing to invalidate. The extent was *always* stored; v4 merely required it
to be 10,000.

**The used extent is computed, never cached.** It is a backward primary-key scan
(`ORDER BY k DESC LIMIT 1` over "holds anything") that stops at the first hit, so it pays
only for trailing cells that exist but are blank — which happens only when something was
typed and then cleared. Written as `SELECT MAX(k) … WHERE …` it sorts, and finding the
end of a sheet costs a sheet; `TestHotPathQueryPlans` pins the absence of the temp b-tree.
The reason it is computed is Excel's most notorious defect: a cached used range that
deletes and clears forget to invalidate, so `Ctrl+End` lands in the void forever and the
documented workaround is "delete the empty rows and re-save".

**Neither extent is monotonic.** An insert takes its rows from the blank tail when it can
and grows the sheet when it cannot; a delete gives them back down to a **`DefaultRows`
floor** and shrinks the sheet above it. The floor is what keeps the two symmetric: on a
default-sized sheet neither operation changes the extent, so insert/delete cycles do not
drift, and above the floor every insert grows by n and every delete shrinks by n. An
allocated extent that only grew would be the scrollbar half of the same Excel bug.

**`RowCeiling = 1,000,000`** is the only row limit left, and it is a sanity bound rather
than a design one — a typo'd count must not allocate 2^63 rows. One million is the number
a spreadsheet user recognizes (Excel stops at 1,048,576), and the storage arithmetic stays
comfortable: 20,000 canonical bands, largest key ~82 million, six orders of magnitude
inside int64. **The rendering ceiling is lower and is not storage's to enforce**: at 22px
a row, a million rows is a 22,000,000px scroll container — legal in Chrome (~33.5M px per
element), not legal in Firefox (~17.9M, so ~813k rows).

**Cost: none, and that is the interesting part.** The rows an insert creates are created
by the band it inserts into — `nrows += n` is the only place rows are added, so "reuse the
blank tail" and "grow" differ by one skipped `dropTail`. There is no key allocation, no
cell write and no new primitive. Measured on the same 9,999 x 26 fixture, baseline at load
2.86-2.95 and the after column at load 2.57-2.71 (another agent working alongside; the
sets taken while they were rebuilding, at load 3.3-4.7, moved every operation up ~25%
together and were discarded):

| operation | before | after, 3 runs | cells moved |
| --- | --- | --- | --- |
| `InsertRows(0,1)` | 3.29 ms | 3.24 / 3.33 / 3.18 ms | 1,102 |
| `InsertRows(9500,1)` | 3.26 ms | 3.29 / 3.37 / 3.49 ms | 1,102 |
| `DeleteRows(0,1)` | 4.55 ms | 4.45 / 4.55 / 5.33 ms | 1,078 |
| insert on a **genuinely full** grid | **87 µs refusal** | **3.05 / 3.13 / 3.27 ms, succeeds** | 1,102 |
| window read, 250 rows | — | 3.02 ms vs 3.10 ms row-keyed | (A/B, one process) |
| single-cell edit | — | 15 µs vs 14 µs row-keyed | (A/B, one process) |

The last row is the whole change: the operation that used to be a fast refusal is now an
ordinary insert that costs what every other insert costs.

Growth's own worst case, for completeness: one write at row 1,000,000 on a fresh sheet
allocates 20,000 storage bands in **29-35 ms**, and the window read at the ceiling still
resolves keys near 82 million to the right display ranks.

### What growth surfaced that the fixed grid was hiding

1. **`maxKeyBands` was a constant and had to be a function of the extent.** It was
   `4*(MaxBand+1)` = 800, four times the band count of a 10,000-row sheet. A
   1,000,000-row sheet has **20,000 bands in its canonical layout**, so the fixed ceiling
   would have declared a perfectly healthy sheet crowded and forced the one O(sheet)
   operation in the system — a full re-key, ~300-400 ms — on **every single delete**. The
   fixed grid hid this completely because 200 was the only band count that ever existed.
2. **Extension must not go through `appendTail`.** That function refills the slack a
   delete just gave back and must not change the band shape; used to add 5,000 new rows it
   would put ~4,000 of them in ONE band, and every future insert into that band would move
   4,000 cells — the exact cost `bandSplitRows` exists to bound. Growth got its own
   `growTo`, which lays out canonical bands.
3. **A range's cell cap was not enough once rows went to a million.** `maxRangeCells` is
   100,000, so `A1:A100000` passes it — but a range is evaluated by materializing a
   **dense** window, so its cost is rows x 26 however narrow the range is. A separate
   `maxRangeRows = 10000` keeps the worst-case dense materialization exactly what it was.
4. **"Insert below the last row" was a silent no-op**, because a fixed grid had nothing
   below the last row to insert before. `at == extent` is now the append position, and it
   is the only asymmetry in argument validation (a delete still has to name a row that
   exists).

### Migration: v4 -> v5, and it changes no bytes

Which needs saying, because "no rewrite" is exactly the claim that was false for v3 -> v4.
It is true here because v4 already stored the extent as the sum of `bands.nrows`; it
merely *required* that sum to be 10,000, where v5 accepts 1..`RowCeiling`. Every v4 file
is a valid v5 file saying "10,000 rows", which is what it meant, and every cell is
addressed by the key it always had. There is no reinterpretation to get wrong, so the step
only **validates** and refuses to open a file whose bands do not describe a usable sheet.

It is stamped anyway, and that is the point of stamping: a v5 file grown to 15,000 rows
opened by a v4 binary fails loudly in `validate()` ("holds 15000 display rows, want
10000") instead of rendering the first 10,000 and losing the rest.

**Measured on real files:** a 220,382-cell v4 file, **1 ms**, all 220,382 cells verified
identical (`TestMigratesRealV4File`, which reads the cells by v4's own key arithmetic
before the store touches the file). Against v3 -> v4's 459 ms for the same population.
The 15 live v3 sheets under `/tmp/ss-run` migrate straight through v3 -> v5 in 2-3 ms
each, all verified.

### What the render layer must now call

**`MaxRows` is a deprecated alias for `DefaultRows` and is no longer the truth about any
sheet.** It is still defined, and every file below still compiles and behaves exactly as
it did — a default-sized sheet is unaffected. What breaks is a *grown* sheet: it renders
a scroll container of the wrong height and cannot be scrolled past row 10,000. Verified
live — writing `C15000` grew the file to 15,000 rows and `GET /s/demo?at=C15000` still
clamped to row 9,999 and emitted `--rows:10000`.

| call site | now | should be |
| --- | --- | --- |
| `render.go` — the `--rows` custom property | `MaxRows` | `sh.Rows()` |
| `render.go` — window hi clamp | `MaxRows-1` | `sh.Rows()-1` |
| `render.go` — client `ROWS=` in the scroll script | `MaxRows` | `sh.Rows()` |
| `keys.go` — client `ROWS=` | `MaxRows` | `sh.Rows()` |
| `keys.go` — select-all far row `$_sfr` | `MaxRows-1` | `sh.Rows()-1` |
| `http.go` — viewport hi clamp | `MaxRows-1` | `sh.Rows()-1` |
| `http.go` — buffer band clamp | `MaxBand` | `BandOf(sh.Rows()-1)` |
| `anchor.go` — anchor viewport clamp | `MaxRows-1` | `sh.Rows()-1` |
| the column-width dirty set | `AllBands()` | `sh.AllBands()` |

(`grep -n 'MaxRows\|MaxBand' render.go keys.go http.go anchor.go` finds all of them; the
line numbers move too fast to write down.)

`--rows` is O(config), not O(cells), so it belongs in the stylesheet and arrives as a
small patch when it changes — the same rule the column widths already follow. It changes
rarely: `Dirty.Stats.Grew` is the net change in the extent for a structural mutation and
is zero for almost all of them, so a render can ask "did the container height change"
without recomputing anything.

Two things for that change to handle:

- **A shrinking extent invalidates the client's `scrollTop`.** The browser clamps it,
  which fires a scroll event, which trips the throttled viewport handler.
- **`(*Sheet).UsedRows()`** is the other extent, and it is what a jump-to-end gesture
  means. It is a query, not a field, so it takes an error return.

## Change 5 — cell styling and number formats

**The design is XLSX's, for XLSX's reason.** A sheet has thousands of cells and a
dozen distinct looks, so a cell stores an *index* into a per-sheet table of distinct
`(bold, italic, fg, bg, align, numfmt)` tuples — `<c r="B5" s="3"/>` — rather than its
own appearance. The number format is folded into the same record, as XLSX's `xf` does,
because there is no case where a caller wants a cell's colour and not its format, and
splitting them would make the hot read two lookups for one fact.

That shape is also the O(config)-in-CSS / O(cells)-in-markup rule this codebase already
follows for the column widths and the generated `[id^=D]{grid-column:5}` rules: the
render layer emits `.s3{font-weight:700;color:#cc0000}` once into the page shell, and
every cell sharing that look carries the identical four bytes `s3`.

### Where the style id lives — MEASURED, and the band keys did change the answer

Two candidates: a `style` INTEGER column **on `cells`**, or a **sparse side table** keyed
`(k, col)`. The old objection to the column was width in the clustered b-tree — Change 1
records five columns costing more than deleting 20,402 formula rewrites saved. But that
was measured on a shift that moved **220,382 cells**; a shift now moves **~1,100**, so
the same width cost is ~200x smaller, while a join on the read is unchanged and is paid
on **every push to every viewer**.

Measured as an A/B in ONE process, alternating, over identical data (`TestStyleLabPlacement`,
9,999 x 26, window of 250 rows / 5,510 cells, load 2.3-2.5):

| | 5% of cells styled | every cell styled |
| --- | --- | --- |
| `style` column on `cells` | **3.21 ms** | **3.12 ms** |
| sparse side table, `LEFT JOIN` | 4.67 ms (**+45%**) | 5.65 ms (**+81%**) |

The join plan is `SEARCH c USING PRIMARY KEY` + `SEARCH s USING PRIMARY KEY … LEFT-JOIN`
— one extra b-tree probe per cell in the window, 5,510 of them, on the path that runs on
every push. The column is already in the row the range scan returns.

And what the column costs, same instrument, against a byte-identical style-less copy of
the same cells (`TestStyleLabColumnCost`, three runs at load 2.33-2.46):

| | with `style` | without | delta |
| --- | --- | --- | --- |
| shift 50 keys x 26 cols (what an insert does) | 3.19 / 3.09 / 3.13 ms | 3.27 / 3.09 / 3.29 ms | **−2.6 / 0.0 / −4.6%** |
| window read, 250 rows / 5,510 cells | 3.90-4.01 ms | 3.58-3.61 ms | **+8.2 to +11.7%** |
| single-cell edit, median of 200 | 22 µs | 21 µs | +1 µs |

The +10% on the read is real — 5,510 extra integers decoded through `database/sql` — and
it was **paid for by deleting an allocation the read path never needed**. `Window` used
to rebuild its ten-pointer `[]any` scan target *per row*: two allocations per cell,
~11,000 per window. Hoisted out of the loop, the read WITH the style column is **3.1-6.2%
faster than the v5 read path as it actually shipped**, which is the comparison the render
layer experiences. Both arms of that are in the same table above.

**Verdict: a column on `cells`.** The side table's only advantage — no shift cost — is an
advantage over a cost the band keys already deleted.

### What a style command costs

`SetStyle` is a batched upsert, not a statement per cell — a rectangular selection is
thousands of cells and one round trip each would be a visible pause for work six integers
wide. Measured on a seeded 500-row sheet at load 1.79 (`TestSetStyleRangeCost`, three
runs), styling a buffer-sized 250 x 26 selection:

| operation | cells | time | per cell |
| --- | --- | --- | --- |
| `SetStyle` (bold + background) | 6,500 | **21.5 / 21.6 / 21.5 ms** | 3.3 µs |
| `ClearStyle` | 6,500 | 24.4 / 24.7 / 24.4 ms | 3.8 µs |

3.3 µs a cell is parity with the row shift's 2.7 µs a cell, which is the right comparison:
this IS 6,500 cell writes and there is no way for it not to be. A realistic selection — a
column of a hundred cells — is ~0.3 ms. The per-cell merge itself is free: the result is
memoized per SOURCE style, so a range over one look is one `intern` and 6,500 map hits.

### The style survives everything, by construction rather than by care

- **A cell edit.** The write upsert never names `style`, so `ON CONFLICT DO UPDATE` leaves
  it alone. Formatting a column and then typing into it works, and clearing a cell keeps
  its colour, which is what every spreadsheet does.
- **A structural mutation.** `style` rides along in the shift's scratch-table copy, in
  `shiftCols`, and in the rebalance's re-key. It moves because it *is* part of the cell.
- **A rebalance**, the one O(sheet) path, is pinned by `TestStylesSurviveARebalance`.

### The one thing styling changed about the rest of the model

**A styled empty cell is a stored row, and it counts as content.** "Does this hold
anything" gained `OR style <> 0` in three places — `UsedRows`, the insert's tail probe,
and the column-overflow check — because a yellow empty cell is something the user put
there. Without it, `Ctrl+End` skips past it and an insert at the bottom pushes it off the
grid. `ClearStyle` deletes rows it leaves holding nothing at all, so the table stays sparse.

### Number formatting is a display transform, applied in the store

`1234.5` under a currency format is **stored** `1234.5` — that is what `SUM` adds — and
**read** `$1,234.50`. `Cell` now carries three texts and the difference is load-bearing:
`Raw` (what was typed), `Computed` (the machine value — what recalc reads, what the event
log records, what the golden fixtures hash, never formatted), and `Display` (`Computed`
with this cell's format applied, equal to `Computed` for every unstyled cell).

It happens in `Window`/`GetCell` rather than being exposed for the renderer because it is
a function of stored state only the store holds, and because doing it on the way out makes
"formatting never reaches storage" **structural** — nothing writes `Display` anywhere —
rather than a rule someone has to remember. `TestFormattingNeverReachesStorage` sums three
currency-formatted cells and gets `3001.5`.

Seven formats, each an enum and a Go function: plain, integer, 2dp, currency, percent,
date, datetime. **No format-string parser.** Two things worth knowing:

- Every format is a **no-op on a value that does not parse as a finite number**, which is
  what keeps `#CYCLE!` from rendering as `$0.00` and is why it is safe to run over a whole
  window without asking what is in each cell.
- **Rounding is half away from zero, not Go's half-to-even.** `strconv.FormatFloat` renders
  1234.5 as `1,234` and 1235.5 as `1,236` — correct IEEE, obviously broken spreadsheet.
- Colours are **whitelisted** to `#rgb`/`#rrggbb` and normalized to lowercase `#rrggbb` at
  the API boundary. That is what makes `#F00` and `#ff0000` one style row instead of two,
  and it is also the only thing standing between a user string and a `<style>` element.

### Garbage collection, and the bound if it never ran

The style table is swept when it passes `styleGCLimit` (64 rows): delete every style no
cell points at. A **sweep, not a refcount** — a count would have to be maintained by every
path that writes a cell, including the shift and the rebalance, which is a counter to get
wrong in six places for a table of tens of rows.

The sweep is `SELECT DISTINCT style FROM cells WHERE style <> 0`, and it is the one query
in the styling path not bounded by the selection, so it is bounded by a fifth **partial**
index instead. MEASURED over 11,000 styled cells of 220,382:

| | plan | time |
| --- | --- | --- |
| with `cells_style` | `SCAN cells USING COVERING INDEX cells_style` | **0.31 ms** |
| without it | `SCAN cells` + `USE TEMP B-TREE FOR DISTINCT` | **12.00 ms** |

**The bound if it never ran** is one row per distinct tuple *ever applied*, and since
colours are normalized the growth is bounded by the user's palette rather than by the
number of commands. The cost of not collecting is **not disk** — a row is ~50 bytes — it
is **CSS**: `StyleRules` goes into the page shell, so a dead style is bytes on every first
paint, forever. That is the reason the sweep exists.

### Migration: v5 -> v6, one ALTER, no cell rewritten

`ALTER TABLE cells ADD COLUMN style INTEGER NOT NULL DEFAULT 0` plus
`CREATE TABLE styles`. SQLite's ADD COLUMN rewrites the schema text and leaves the table
pages alone, and a row short by one column reads the default.

**Why it cannot misread**, stated rather than assumed, because "it's just a new column" is
how a file gets silently reinterpreted: a v5 cell row does not encode a style anywhere, so
there is no bit a v6 reader could take to mean something it did not mean before. Every
existing row has exactly one correct v6 value, `0`. That is the property v3 -> v4 did not
have.

The column is declared **last** in `schemaDDL` because ADD COLUMN appends; anywhere else
and a migrated file and a fresh file would have different column *orders* for the same
version. `TestMigratedAndFreshSchemasMatch` pins it.

**Measured on real files** (`TestMigratesRealV5File`, all verified cell for cell):

| file | cells | time | mismatches |
| --- | --- | --- | --- |
| synthetic 220,382-cell v5 | 220,382 | **12 ms** | 0 |
| `/tmp/ssqa-data/demo.db` | 11,024 | 2 ms | 0 |
| 15 real sheets from `/tmp/ss-run`, v5 | 0-11,025 | 1-2 ms each | 0 |
| the same 15 at v3, straight through v3 -> v6 | 0-11,025 | 2-16 ms each | 0 |

The 220,382-cell file went **6,696,960 -> 6,709,248 bytes**: three pages, for the schema
text and the roots of the new table and its two indexes. v3 -> v4 doubled the same file.

### What the render layer must call

| what | call |
| --- | --- |
| the stylesheet, into the page shell | `for _, r := range sh.StyleRules()` -> `.{StyleClass(r.ID)}{{r.Style.CSS()}}` |
| the per-cell class | `StyleClass(cell.Style)`, "" for the default — combine with the existing `t`/`f`/`e` kind classes |
| the cell's text | **`cell.Display`, not `cell.Computed`** |
| the edit buffer / formula bar | `cell.Raw`, unchanged |

`StyleRules()` is O(distinct styles) and changes only when a style is created or collected,
so it belongs in the shell and arrives as a small patch when it moves — the same rule
`--rows` and the column widths follow. Two things it has to handle:

- **A style on an EMPTY cell is currently invisible.** `renderCells` skips `KindEmpty`, so
  a background colour on a blank cell is stored, is reported dirty, and is never drawn.
  That is a render-layer decision, not a storage one; the store says the cell exists and
  carries a style id.
- `SetStyle` returns `Dirty{Cells, Bands}` with `Structural: false` — a per-cell patch
  list, the same shape an edit returns, so it goes down the `pushCells` path.

### What this makes easier or harder for row heights

**Easier, and the same shape is available.** A row height is O(rows) config, not O(cells)
— it belongs beside `cols(col, width)`, as `rows(k, height)` keyed by the **storage key**,
not the display row. Keyed that way it inherits the whole band-key result for free: a row
that does not move needs no write however far its rank shifts, and `shiftKeyRange` already
has the `k + delta` primitive (it is what `shiftWidths` does on the other axis, in Go,
because there are only 26 of them).

Three things this change learned that apply directly:

1. **Do not put it on `cells`.** The style id is on `cells` because it is per-CELL; a row
   height is per-ROW and would be 26 copies of the same integer, 26 places to disagree,
   and 26 rows to rewrite for one drag.
2. **A height must count as content** the same way a style does — the `OR style <> 0` list
   grows an `OR` or the tail probe pushes a resized row off the bottom.
3. **The one hard part is the same one styling did not have to solve:** a row keyed table
   has to be *dropped* when its rows are deleted and *shifted* when they move, where a
   column on `cells` gets both from the statement that moves the cell. That is ~20 lines
   in `applyRowOp` next to the `dead` ranges it already computes.

The read side is cheaper than styling's: heights are O(rows in the window) rather than
O(cells), so a window read fetches at most 250 of them in one range scan and hands the
render layer a map — no join, and no per-cell lookup at all.

## Options not taken

**Full stable row/col ids with a separate display order.** The clean fix — insert becomes
O(1) and formula rewriting disappears by construction. But a spreadsheet addresses by
**rank** ("row 500"), and rank over stable ids needs an order-statistic structure SQLite
does not provide. Change 3 above is this design with the order-statistic structure made
out of bands, which is exactly why it works. Reach for full stable ids only if you need
cross-sheet references, row-level permissions, or move-row-preserving-identity.

**ECS / data-oriented framing.** The useful half is the component split: give each
optional aspect (comment, format, validation) its own sparse table rather than widening
`cells` — and note that widening `cells` is exactly what cost us 39 ms above, so this is
not theoretical. Putting formulas in their own table would let recalc scan ~20k rows
instead of 220k.

The half that does not apply is archetype iteration. ECS wins when you sweep every entity
with a component set each frame; a spreadsheet iterates a *window* (spatial locality) and
a *dependency subgraph* (graph locality). Neither is a full-population scan, and ECS has
no answer for rank, which is the actual hard problem.

## Blank sheets and sparse data — DONE, and the prediction was half right

Storage was already sparse — only written cells exist. **Rendering was dense regardless**:
`Window()` materialized `(hi-lo+1) x 26` cells and emitted a `<td>` for each, so a blank
sheet cost the same as a full one, ~13,000 nodes per buffer.

The reasoning was that brotli crushes 13,000 near-identical empty `<td>`s to almost
nothing, so dense-empty is **cheap on the wire and expensive in the DOM**, and the fix is
therefore node count rather than compression. That was right, and the measurement is in
RESULTS.md: a blank screen went **14,005 nodes → 531** and client morph time fell 8-19x on
every sheet.

**What the prediction missed is that sparse cells have to carry their own position, so
they are not free.** A cell went from `<td id="D7">490</td>` to
`<b id="D7" style="--r:6">490</b>`, and a *full* sheet is therefore 34% more raw bytes.
The break-even is **58.4% fill raw and 88.6% compressed** — and the 30-point gap between
those two numbers is the same lesson arriving again, since the thing dense rendering
spends its bytes on is exactly what a compressor deletes for free.

Three things fell out that the note did not anticipate:

- **The column belongs in CSS, not in the markup.** Cell ids are A1 notation, so 26
  generated `#b [id^=D]{grid-column:5/6}` rules place every cell in the sheet and the cell
  states only its row. Worth ~10 points of fill raw and ~41 compressed. The general rule —
  O(config) in the stylesheet, O(cells) in the markup — is the same one the column-width
  custom properties already followed.
- **The server had to start remembering which cells the client holds** (a 26-bit column
  mask per buffer row, ~2 KB per screen), because "morph, insert or remove" and "which ids
  does this dropped band contain" are questions a dense grid never had to ask.
- **DOM order stopped carrying information**, which collapsed prepend+append into one
  append, made a cell insert position-independent, and let the buffer's `translateY`
  offset be deleted outright.

Cost, as predicted: `<table>` semantics and natural column sizing. Column widths were
already explicit server state, so most of that was paid.

## What is now the dominant cost of an insert

Not the write. A top-of-sheet insert writes 1,102 cells in ~3 ms and then reports **200
dirty bands**, because inserting at row 0 changes the display rank of every row below it
and every band is therefore stale. That invalidation is now an order of magnitude more
expensive than the storage work it describes, and it is a rendering question rather than a
storage one: the store cannot say "these rows are the same cells, renumbered" through a
band-set, which is all the bus carries.

The storage model can express it — a row's key does not change when its rank does, which
is exactly the fact a client would need to renumber without re-rendering — so the
information is there if the render layer ever wants it.

## The warning: measure on an idle machine

Numbers taken while other work was running were wrong by up to **2x**, and one of them
reversed a conclusion. A `DeleteRows(0,1)` measured at 760 ms under load reproduces at
369 ms idle — which turned an apparent 46% regression into no regression at all, and sent
the analysis chasing a cost that was not there.

Every table in this document is idle-machine unless stated. The contaminated figures were
discarded rather than corrected, because there is no way to know how much load each one
absorbed.

## Change 6 — styling stops being per-cell: a three-level cascade

**The defect, in the user's words:** *"you can't format more than 2k rows. ideally if i
select a column it should be like applying the style/condition for all rows in that col."*

Change 5 put the style id on `cells`, which is right for a CELL and is the wrong unit for
an intention. "Make column D currency" was 10,000 cell writes on a default sheet — past
`maxWriteCells`, the cap every large-range command shares — and on a **blank** column it
also **materialized** 10,000 rows that did not exist, because a cell carrying a style has
to be a row and the renderer draws any cell that has one. One intention, ten thousand
writes, a sheet 26x bigger on disk, and a `Ctrl+End` that now lands at row 10,000.

**The fix is XLSX's, again, and for XLSX's reason.** A spreadsheet resolves a cell's look
from three records — `<col style="5"/>`, `<row s="3"/>`, and the cell's own `s`:

```
effective = cell.style ?: row.style ?: col.style ?: default
```

A column style is **one record** and materializes **nothing**. Measured on the 9,999 x 26
fixture, four runs: `SetColStyle` over a whole column is **260-441 µs and 0 cells
written**, against **12.0-12.3 ms and 2,000 cells created** for the per-cell equivalent
capped at the 2,000 `maxWriteCells` allows — and that capped version covers 2,000 of the
column's 9,999 rows, so it does not even finish the job it is 27-47x slower at.

### The contract, which is the part above the store that matters

`Cell` carries **two** style ids and the difference is the whole contract:

| field | what it is | who reads it |
| --- | --- | --- |
| `Style` | the cell's **own** record, 0 for a cell nobody styled individually | the render layer emits `StyleClass(cell.Style)` per cell — **unchanged** |
| `Effective` | the **resolved** id: cell ?: row ?: col ?: 0 | `Display`, and anything computed server-side |

**The renderer must not emit a class for `Effective`.** If it did, and it also emitted the
column and row rules that the cascade exists to express as O(config) CSS, every level
would be applied twice and the cascade would be a more expensive way to write the same
bytes. `Effective` exists so that the store — which has both levels in hand and resolves
them for free — is the only place the precedence is implemented.

The two agree (`Effective == Style`) for every cell of every sheet nobody has styled by
row or column, which is why nothing above the store needed changing to keep working.

**What makes record-level resolution and a per-property CSS cascade agree** is not the
read, it is the WRITE: `SetStyle` merges its patch against the cell's **effective** style,
not its own. Bolding a cell in a currency-formatted column produces a bold **currency**
cell, so the record the renderer emits is self-contained and CSS underneath it cannot
contradict it. Merging against the cell's own (absent) style would produce a bold PLAIN
cell and silently drop the format the column was giving it — the same class of bug as the
toolbar deleting formatting, one level up.

Three consequences worth stating because none of them is obvious:

- **Clearing a CELL reveals the level.** `ClearStyle` removes the cell's override and the
  cell falls back to its row and column, which is XLSX's semantic and the only one under
  which the cascade means anything. `ClearRowStyle` / `ClearColStyle` are different verbs.
- **Re-applying what a level already says writes nothing.** The no-op test is `to == base`
  (the effective style) and not `to == own`, so "bold a column the column level already
  bolds" stays zero writes instead of materializing 10,000 cells to repeat it.
- **A cell must be able to override a level BACK to plain**, and that is the one hole a
  record-level cascade has that per-cell styling did not. Style 0 means "ask the level
  above me", so it cannot also mean "I have deliberately chosen to look like nothing" —
  bold a row, un-bold one cell in it, and a naive implementation writes 0 and the bold
  comes straight back. `internForce` allocates a REAL id for the default tuple, but only
  when a level is actually underneath (`under != 0`); with nothing underneath the default
  really is 0, and allocating a row for it would be a dead rule in the page shell forever.

  **This is a requirement on the renderer, not only on the store.** The rule for that id
  is empty under `CSS()`, and an empty rule is not an override — so cell-level rules must
  be emitted with the new **`Style.CSSFull()`**, which states every property including the
  ones the style leaves alone (`font-weight:400`, `color:inherit`, …). It is safe to use
  for all three levels; the extra bytes are O(distinct styles) and compress away.

  **Alignment is the one property this cannot express**, deliberately: `AlignDefault`
  means "whatever the stylesheet says", which for a spreadsheet is numbers right and text
  left — a rule storage does not know and must not overwrite with `text-align:left`. So a
  full rule states an alignment only when there is one. Everything a toolbar actually
  toggles off — bold, italic, colour, background — works.

### What the render layer calls

| what | call |
| --- | --- |
| the stylesheet | `sh.StyleRules()` — unchanged, still every distinct look |
| the **column** rules | `sh.ColStyles()` -> `map[col]styleID`, served from memory, no query |
| the **row** rules | `sh.RowStyles(loRow, hiRow)` -> `map[displayRow]styleID`, for the window it just rendered |
| the per-cell class | `StyleClass(cell.Style)` — **the cell's own**, unchanged |
| the rule BODY | `r.Style.CSSFull()`, **not** `CSS()` — see the override note above |
| the cell's text | `cell.Display`, unchanged (now formatted through the EFFECTIVE format) |

Emit the column rules first, then the row rules, then the cell rules, so that source order
breaks ties the way the cascade does. `RowStyles` takes a window rather than answering for
the sheet because the row level is unbounded; the column level is 26 integers and is
resident.

`SetColStyle` / `SetRowStyle` return `Dirty{Bands, Config: true}` with **no dirty cells**,
because no cell changed. `Config` is new and says "this was shared configuration, not an
edit": re-send the stylesheet, then re-render the affected bands — the shape a column
WIDTH change already has. A column style dirties the whole grid (every rendered row
contains every column); a row style dirties the bands of the rows it touched.

### The storage, and why the row level is keyed by `k`

`cols` gains a `style` column beside `width` — same table, same row, moved by the same
statement, because they are the same kind of fact.

`rows` is a new table, `(k, style, height)`, keyed by the **storage key** and not the
display rank. That is the previous change's recommendation taken verbatim, and it pays
exactly as predicted: a row that does not move needs no write however far its rank shifts.
There are only three moments a key actually changes and each already had a primitive:

| moment | where | cost |
| --- | --- | --- |
| a run of keys shifts inside a band | `shiftKeyRange`, one call | bounded by the band |
| a run of keys ceases to exist | `applyRowOp`'s `dead` loop | bounded by the delete |
| a rebalance re-keys the sheet | the same `temp.kmap` | bounded by STYLED rows, not by the sheet |

**`height` is declared and not populated.** Row heights are the next change; they are the
same shape, keyed the same way, moved by the same three primitives, and adding the column
later would be a schema version for one integer.

### Does a level style extend the sheet? Row yes, column no

The previous change flagged three places `OR style <> 0` had to be added. All three were
revisited and they do **not** get the same answer, which is the interesting part:

| | `UsedRows` | insert tail probe | column-insert overflow |
| --- | --- | --- | --- |
| **row style** | **yes** — a yellow row is content and `Ctrl+End` must reach it | **yes** — reusing the tail would delete the only record of it | n/a |
| **column style** | **no** — it applies to rows that do not exist, so it names no extent | n/a | **no** — see below |

The asymmetry is not an oversight, it is a cost argument. On the ROW axis, counting a
style costs nothing: the alternative to reusing the blank tail is allocating one more row,
and the sheet grows. On the COLUMN axis there is no growth — `MaxCols` is fixed — so the
only alternative to letting a column style fall off the right-hand edge is **refusing the
whole mutation**, which would turn "I coloured column Z" into "this sheet can never accept
a column insert again": the row-cap bug, in a new place. A column style also destroys no
data and materializes no cell, and it now travels in the same statement as the column
WIDTH, which has never blocked a column insert either.

`UsedRows` is therefore **two** backward primary-key scans rather than a UNION, because a
UNION over two tables ordered by `k` needs a sort and the entire point of that query is
that it has none. The row arm is skipped outright unless the sheet has a row style.

### Garbage collection now has three sources of a reference

The sweep was `SELECT DISTINCT style FROM cells WHERE style <> 0`, and a look worn ONLY by
a column — which is the normal case for a level — has no cell pointing at it. Collecting
it deletes a live CSS rule out from under the stylesheet. Three statements now, not one, so
each keeps its own plan: `cells_style` covering, a new partial `rows_style` covering, and
`cols` (26 rows, a scan of nothing). `TestLevelStylesCountAsReferencesForGC` pins it.

### The hot read path, MEASURED, because that is the constraint

Two instruments, both alternating arms in ONE process against identical data.

**THE MACHINE WAS NOT IDLE.** Another agent was working in the same tree throughout;
load averages were 2.15-4.1 where every earlier table in this document was taken at
2.3-2.9. Per the warning at the bottom, that is enough to move a number by tens of
percent, so what follows is reported as RANGES OVER REPEATED RUNS with the load stated,
and single-run deltas under ~5% should be read as "not distinguishable from noise".

**Did the cascade regress the read that does not use it?** `windowV6` is the pre-cascade
`Window` body compiled into the test file, so both arms run against the SAME open sheet,
alternating, in one process (9,999 x 26, 5% of cells styled, 250-row window = 6,500 cells,
25 alternating runs per arm). Four runs, loads 2.15 / 3.35 / 3.25 / 3.31:

| | run 1 | run 2 | run 3 | run 4 |
| --- | --- | --- | --- | --- |
| with the cascade, median | 2.79 ms | 2.77 ms | 2.85 ms | 3.00 ms |
| pre-cascade body, median | 2.84 ms | 2.70 ms | 2.76 ms | 2.97 ms |
| **delta, median** | −1.9% | +2.4% | +3.3% | +1.0% |
| **delta, fastest sample** | −0.2% | +1.4% | +1.5% | +2.0% |

Mean **+1.2%** on both statistics, and the sign is not stable across runs. The only thing
that could account for a real ~1% is the one unavoidable cost: `Cell` is eight bytes wider
for `Effective`, which is 52 KB more to allocate and zero per window. Everything else on
the no-level path is two resident booleans, and the extra query is never issued.

**What a level style costs when you have one** (four copies of the same sheet, 15
alternating runs each; three runs at loads 3.2 / 3.35 / 3.25 / 3.31):

| arm | vs none, run 1 | run 2 | run 3 | run 4 |
| --- | --- | --- | --- | --- |
| one column styled | +5.2% | −1.2% | −0.2% | +4.0% |
| every row of the window styled | +5.2% | −0.1% | +2.6% | +3.3% |
| both | +5.7% | +0.2% | +3.1% | +4.6% |

Call it **0-5%, centred near +3%** for the full cascade over a window in which every row
and two columns carry a level style — which is a pathological sheet, not a normal one. The
row arm's cost is one extra primary-key range scan returning 250 narrow rows against the
window's 5,510 cells; the column arm's is 6,500 array reads. Neither is a join and neither
is a probe per cell — `TestHotPathQueryPlans` now pins six more plans to keep it that way,
including that the row level is `SEARCH rows USING PRIMARY KEY` and never a `LEFT-JOIN`.

**And the mutations, which is where a key-shaped side table could have hurt.** Two
identical 9,999 x 26 sheets, alternating in one process — one with no level style, one
carrying two column styles and 200 row styles — 7 rounds, load 3.15:

| | plain | levelled |
| --- | --- | --- |
| `InsertRows(0,1)` | median **2.87 ms**, fastest 2.77 | median **2.87 ms**, fastest 2.83 |
| `DeleteRows(0,1)` | median **3.06 ms**, fastest 2.92 | median **3.08 ms**, fastest 3.04 |
| single-cell edit, median of 200 | 58 µs | 56 µs |

Carrying 200 rows of per-row state through a shift is free to the resolution of this
instrument, which is the band-key result arriving exactly as predicted: the shift touches
one band, so it touches the handful of row records in that band and nothing else.

The PLAIN arm is the regression check, and it needed one change to stay flat.
`shiftRowMeta` is four statements including a temp-table create and drop, and it is called
from `shiftKeyRange` — the primitive every row mutation is made of — so on a sheet with no
row state it would have been four statements moving zero rows on every mutation forever.
It now probes first (`SELECT k FROM rows WHERE k BETWEEN ? AND ? LIMIT 1`, one indexed
seek) and returns.

Against the standalone worst-case fixture (`TestStructuralWorstCaseCost`, four runs at
load 3.7-4.1, against this document's 3.24-3.33 / 4.45-5.33 baseline taken at load
2.5-2.9): `InsertRows(0,1)` **3.21 / 3.30 / 3.31 / 3.32 ms** — on the baseline despite
noticeably more load — and `DeleteRows(0,1)` **8.38 / 6.43 / 4.93 / 5.24 ms**, whose
spread is the load and not the change (the levelled-vs-plain A/B above puts the two arms
0.7% apart).

### Migration: v6 -> v7, one ALTER and one CREATE

`ALTER TABLE cols ADD COLUMN style INTEGER NOT NULL DEFAULT 0` plus `CREATE TABLE rows`.

**Why it cannot misread**, stated rather than assumed. A v6 `cols` row encodes a WIDTH and
nothing else, so there is no bit a v7 reader takes to mean something it did not mean
before, and every existing row has exactly one correct v7 value for the new column: `0`.
An **empty** `rows` table says "no row carries a style or a height", which is precisely
what a v6 file said by not having the table. `style` is declared LAST in the cols DDL
because ADD COLUMN appends, the same rule `cells.style` follows;
`TestMigratedAndFreshColsSchemasMatch` pins it.

**Measured on real files, all verified cell for cell** (`TestMigratesRealFile`, which reads
the cells by the file's own key arithmetic before the store touches it):

| file | version | cells | rows | time | mismatches |
| --- | --- | --- | --- | --- | --- |
| synthetic 220,382-cell v6 | 6 | 220,382 | 10,000 | **1 ms**, **+0 bytes** | 0 |
| `/tmp/ssqa2-data/demo.db` | 6 | 11,024 | 10,000 | 1 ms | 0 |
| `/tmp/ssgrow2/data/4e2omjn9ynegcp.db` | 6 | 1 | **115,000** | 2 ms | 0 |
| 15 sheets under `/tmp/ss-run`, straight through v3 -> v7 | 3 | 0-11,025 | 10,000 | 2-16 ms | 0 |

The 220,382-cell file did not change size at all, which is the claim "rewrites no cell"
made as a number.

## DefaultRows is 10,000 again — and it is 1,000's story that made it cheap

Read the section below this one first: it is the record of `DefaultRows` going 10,000 ->
1,000, and this is that change being made again in reverse. **It cost seven hashes and
nothing else**, which is the payoff for the discipline the first change paid for.

**Why it went back up.** 1,000 was right about the number and wrong about the world it
was in. A blank Google Sheet is 1,000 rows because Sheets puts an *"Add 1,000 more rows
at bottom"* control at the foot of the grid; this prototype had the number and not the
control, so a blank sheet was a 1,000-row box with no door. The reported bug was `"can't
scroll past 1001"`, and it was accurate: the sheet grew on WRITE (`A1500` -> extent
1,500) and never on SCROLL, because a viewport command is a read and reads do not
mutate. Both halves landed together — the foot control (`growrows.go`, `mop:"ap"` on
`POST /rows`, which is `InsertRows(sh.Rows(), n)` and no new store API) and this number.

**What 10,000 costs, measured.** Storage is sparse and rendering is windowed, so a blank
10,000-row sheet stores **zero cells** and renders the same **531 nodes** a 1,000-row one
does — `#g` on a blank sheet is byte-for-byte identical at 9,906 bytes across 1,000,
10,000, and 10,000-plus-the-control builds, and so is the one-cell edit patch
(`<b id="B3" style="--r:2">42</b>`). Two things scale and both are trivial: the band
table (20 -> 200 bands; a 1,000,000-row sheet lays out 20,000 bands in a measured 29-35
ms, so 200 is ~0.3 ms) and the scroll container (22,000 -> 220,000 px, against Firefox's
~17.9M px element limit).

**The real cost is the scrollbar**, and it is accepted rather than hidden: twenty rows of
data in a 10,000-row container is a thumb 0.2% of the track. The better fix is to size
the container from `UsedRows()` plus slack instead of from the allocated extent, which is
a render-layer change and is written up in BACKLOG.md.

**Seven hashes moved and they are the same seven, moving back.** All deletes, because the
delete floor IS this constant: at 1,000 a delete on a 10,000-row fixture shrank the grid
(extent 9,999), and at 10,000 the floor is the extent again so the row comes straight
back. Verified by the method the first change established — take each new dump, patch
ONLY the last window's cell count back, and reproduce the old hash byte for byte:

| case | last window | delta |
| --- | --- | --- |
| `DeleteRows(0,1)` | 2,574 -> 2,600 cells | 1 row x 26 |
| `DeleteRows(48,4)` band edge | 2,496 -> 2,600 | 4 rows |
| `DeleteRows(60,120)` whole bands | **0** -> 2,600 | the window was past the end |
| insert then delete the same row | 2,574 -> 2,600 | net 1 row |
| interleaved edits and structure | 2,548 -> 2,600 | net 2 rows |
| rows then cols | 2,574 -> 2,600 | net 1 row |
| FullSheet `DeleteRows(0,1)` | 7,774 -> 7,800 | window 9700..9999 |

**Exactly one line differs in each**, and it is a `# window` header. No cell moved, no
value changed, no event changed, no width changed, no dependency edge changed. The other
nine did not move in either direction, which is `goldenExtent` doing the job it was
introduced for — the fixtures state their own grid, so they no longer inherit this
constant at all.

**`legacyGridRows` stays a constant of its own** and this is the reason it exists. It is
10,000 again *by coincidence* right now, which is precisely the coincidence that hid a
data-loss bug for two versions (see below). Nothing may re-derive it from `DefaultRows`.

## DefaultRows was 1,000, and what it cost to get there

A blank Google Sheet is ~1,000 rows. `DefaultRows` was 10,000 because the row cap used to
be a cap, and the previous change left it there because flipping it broke 15 suites and
moved all 16 golden hashes.

**It is 1,000 now, and nine of the sixteen hashes did not move.**

The fixtures were the problem and the fix was to stop them inheriting: `Cache.Seed` sizes a
sheet to `max(seededRows, DefaultRows)`, so a 300-row fixture silently became however tall
the default happened to be, and `goldenWindows` probes rows 9900..9999 — which existed only
because the default was 10,000. Two new constants, `goldenExtent` (golden_test.go) and
`costFixtureRows` (mutate_test.go), state the grid each fixture was pinned against, and the
harness grows the sheet to it after seeding. `growTo` lays out CANONICAL bands, so growing a
1,000-row layout to 10,000 appends bands at exactly the bases `layoutFor(10000)` would have
given them: **every key in the fixture is the key it was**, which is why the hashes hold.

**The seven that moved are all DELETES, and the reason is one number.** A delete gives its
rows back only down to the `DefaultRows` floor. On a 10,000-row fixture the old floor WAS
the extent — delete a row, get it back, extent unchanged — and the new floor is far below
it, so the extent goes to 9,999. The last golden window reaches past the bottom of the
sheet on purpose, so its cell count shrinks by one row's worth.

**Verified, not regenerated.** Taking each new dump and patching ONLY the last window's
cell count back to its pre-shrink value reproduces the OLD hash byte for byte, and exactly
**one line** differs in each of the seven. No cell moved, no value changed, no event
changed, no dependency edge changed.

### It surfaced a real bug, which is the argument for having done it

`convertRowsToKeys` — the v3 -> v4 rebuild — laid every migrated file out with
`canonicalBandIndex`, which is `layoutFor(DefaultRows)`. That was correct only while
`DefaultRows` happened to equal the fixed 10,000-row grid every pre-v5 file has. With
`DefaultRows` at 1,000 it files every row above 1,000 at a key that names no display rank
and they **vanish silently** — the exact failure mode the migration rules in store.go
exist to prevent, hidden for two versions behind a coincidence of constants.

The height of a file is a fact about that file, so it is now `legacyGridRows = 10000` (with
a `MAX(k)` belt-and-braces), and the 15 real v3 sheets migrate to 10,000-row sheets as they
always did. `TestMigratesV3SheetOnOpen` is what caught it.
