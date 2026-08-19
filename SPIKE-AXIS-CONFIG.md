# Spike — variable row heights, and axis config as a layer

> **Outcome: all three recommended steps shipped.** Strips and axis backgrounds,
> then variable heights with the drag, then wrapping and fit-to-contents. The
> row-groups arm of the measurement below was *reversed* on a second run against
> a buffer that actually held data — see the note under the table. What shipped
> is presentation-only strips at a flat pitch, which is the arrangement the
> corrected measurement favours by 7.8×.

## The reframing

Row height, column width, row background and column background are all the same
kind of thing: **a default for the cells on that axis**. Today the styles half of
that is resolved *into* each cell (the style index), and the size half is CSS
custom properties. Splitting them that way is what makes the current model both
expensive and, in one case, wrong.

The proposal is to treat axis config as **a layer painted underneath the cells**,
on strips that belong to the axis rather than to any cell. Cells then carry only
their own overrides.

## What is already true (measured, not assumed)

**Column strips already exist.** `#b` contains 27 `<i>` elements, one per column,
each spanning the full grid height (measured: 220,000px on a 10,000-row sheet).
They draw the vertical grid lines today and carry nothing else. A column
background is one `background` declaration on an element that is already there,
already positioned, already free.

**Row strips do not exist — but the code to build them does**, behind
`SS_ROW_GROUPS=1`, and it measures *better* than the flat mode it was compared
against:

| | nodes in `#g` | dense raw | dense brotli | blank |
| --- | --- | --- | --- | --- |
| flat | 5,796 | 252,106 | 29,277 | identical |
| row groups | 6,046 | **177,841** | **28,101** | identical |

`+250 nodes (+4.3%)` buys **−29% raw bytes**, because the wrapper carries `--r`
once per row instead of every cell carrying it:

    <div class="w" id="r1" style="--r:0"><b id="A1">0</b><b id="B1">7</b>…

A blank sheet is byte-for-byte identical, because a wrapper is only emitted where
a row has cells.

**And a row background is broken today.** Setting `bg` on `A3:Z3` where only `C3`
has a value stores the row style, generates the rule, and paints exactly one
cell. Sparse rendering emits nothing in empty regions, so an axis-level fill has
nothing to land on. This is not a rendering nicety — it is the feature failing.

## The design

**Axis strips carry axis config.**

- Column strip `i:nth-child(n)` — width (already `--w-N`), background, and the
  vertical rule it already draws.
- Row strip `div.w` — height, background, and the horizontal rule.

Cells sit above both and carry only what is theirs. That removes the reason a
row or column style has to be resolved into every covered cell, and it fixes
empty cells for free because the fill is no longer a property of a cell.

**This retires the gradient.** The horizontal grid line becomes `border-bottom`
on the row strip, which lands at the correct offset by construction whatever the
row's height. The `repeating-linear-gradient` only ever worked because the pitch
was uniform; it is the first thing variable heights break and the first thing
this removes.

**Row strips must be emitted for every buffered row**, not only rows with cells,
or empty rows have no height, no background and no line. That is the cost:
a blank buffer goes from 531 nodes to roughly 781. Still 18x better than the
14,005 the table-based render cost, and the byte cost is ~250 near-identical
elements, which brotli takes to almost nothing.

## Empty rows: one strip each, or grouped runs? — measured

A run of empty rows could collapse into one element drawing its own lines with
the gradient (correct within a run, since the pitch there really is uniform). It
was worth measuring rather than arguing. Prototyped both, blank sheet, six buffer
slides each:

| | nodes at rest | nodes after scrolling | morph median | blank brotli | dense brotli |
| --- | --- | --- | --- | --- | --- |
| gradient | 282 | 482 | 6.0 ms | 10,026 | 29,277 |
| one strip per row | 532 | 732 | 6.3 ms | 10,407 | 29,817 |

**One strip per row costs about 0.3 ms of morph and about 400 compressed bytes**,
and the samples overlap — 2.9/5.5/5.8/6.0/6.6/6.7 against
2.3/5.5/6.2/6.3/6.6/7.1 — so even that is near the noise floor at six samples.

Grouping would win back those 400 bytes at the cost of splitting one element into
three whenever a cell is written into an empty row, which is the most common
operation on the incremental patch path. **Take the simple one.**

Two things make it cheaper than it looks. The gutter needs no new elements: it
already emits one `<b>` per buffered row for the row number, so its rule is a
`border-bottom` on an element that exists. And grouping stays available later as
a pure rendering optimisation, because offsets come from the sparse height
records rather than being measured off the DOM — the client never asks an element
where a row is.

## The hard part: offsets stop being multiplication

`top = row × 22` is currently assumed in eleven Go references, seventeen CSS
uses, and every client-side interaction: scroll → row, hit testing, editor
position, the active-cell box, the selection box, keyboard reveal, and the
container height.

**Storage.** A sparse `row_heights` record, the same shape as `col_widths` —
only non-default rows stored, and it must shift with structural edits exactly as
styles do (reuse `shiftKeyRange`). Prefix sums over bands, which is the shape
`bands.nrows` already has.

**Wire.** Two things: the sheet's total height (one number, for the scroll
container) and the sparse deltas. Typical `k` is a handful of rows, so this is
tens of bytes. For a pathological sheet, fall back to band-level prefixes —
`rows/50` entries, bounded, and band accuracy is enough for scroll targeting
because the buffer corrects it on arrival.

**Client.** `offset(row) = row×RH + cumulative(row)`, binary search over the
sorted delta array, O(log k). One function replaces every `floor(y/RH)`. With
row strips flowing in document order inside a translated container, the browser
computes most positions itself and the arithmetic is only needed for
scroll→row and the container height.

## Fit to contents cannot be computed on the server — and does not

The server has no font metrics. Fit-to-contents is a **client measurement
committed like a manual resize** — measure, then send the same `rowheight`
command a drag sends. Worth stating plainly because it runs against this
codebase's usual direction, and it is the one place the client legitimately
originates a layout fact.

It also has a prerequisite: fitting to contents means nothing while cells are
`white-space: nowrap; overflow: hidden`. **Wrapping is a separate feature and has
to come first** — and it is itself axis config, a default for the row.

Both held on contact. Wrap became a field on `Style`, so it inherited the
three-level cascade for free and "wrap this column" is one record. Fit is a
double-click on the row grip that measures the buffered cells and posts the
result to the same `rowheight` endpoint the drag posts to. One thing the spike
did not anticipate: the measurement has to release the row's height first, since
`scrollHeight` never reports less than the box it is read from — measuring in
place would have made fit a grow-only operation.

## What this touches, and what it gets for free

Already in place:

- `T.gdShow(axis, pos)` takes an axis because "a variable row height has the
  identical problem one axis over". The drag guide is already generic.
- The shared render cache keys on the sheet's mutation version, so a height
  change invalidates every cached window correctly with no new plumbing.
- Row and column styles already exist as records; this changes where they are
  *painted*, not how they are stored.

Needs care:

- **The optimistic-value lesson from the column-width bug applies exactly.** A
  row resize must write that row's own signal, not a shared "row being resized"
  slot, or a resized row reverts the moment another is dragged.
- Structural inserts/deletes must move heights with rows.
- `--rh` stops being a constant; the eleven Go references become
  `offset()`/`heightOf()` calls.

## Recommendation

Do it in three commits, each shippable:

1. **Turn row groups on and paint axis config on the strips.** No variable
   heights yet. This fixes row and column backgrounds, cuts dense payloads 29%,
   and replaces the gradient with a border — all at a uniform pitch, so no
   offset arithmetic changes. The risky part is separated from the valuable part.
2. **Variable heights**, storage plus the offset function plus the drag.
3. **Wrapping, then fit-to-contents**, which is a client measurement on top of 2.

Step 1 is worth doing on its own merits even if 2 and 3 never happen.
