# Spike — variable row heights, and axis config as a layer

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

## Fit to contents cannot be computed on the server

The server has no font metrics. Fit-to-contents is a **client measurement
committed like a manual resize** — measure, then send the same `rowheight`
command a drag sends. Worth stating plainly because it runs against this
codebase's usual direction, and it is the one place the client legitimately
originates a layout fact.

It also has a prerequisite: fitting to contents means nothing while cells are
`white-space: nowrap; overflow: hidden`. **Wrapping is a separate feature and has
to come first** — and it is itself axis config, a default for the row.

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
