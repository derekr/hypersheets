# Backlog — earmarked, not built

Design decisions already reasoned through, with the constraint that makes each one
non-obvious. Ordered roughly by value-per-effort. Nothing here is committed to.

## Markdown in a cell — `FmtMarkdown`

Inline markdown (`**bold**`, `_em_`, `` `code` ``, `~~del~~`) rendered server-side.

**Why it is cheap:** the server already renders HTML, so this is a display transform
exactly like currency formatting. It fits the existing three-text split — `Raw` is
`**bold**` (what F2 shows), `Computed` is the machine value, `Display` is
`<strong>bold</strong>`. **Zero client bytes**, where an SPA would ship a markdown
parser plus a sanitizer (20-40 KB). This is one of the few places the architecture is
strictly better rather than merely competitive.

**It is not a new subsystem — it is an eighth number format.** The seven existing
formats follow the rule "every format is a no-op on a non-number"; markdown is the
mirror, a no-op on a non-text cell. It inherits the style-index dedup and, with the
column/row cascade, a whole column can be set to rich text with one record.

**The one real cost is a new security boundary.** `Display` is text today and gets
escaped. Markdown makes it *markup* inserted unescaped, so `<script>` must not execute
— and formulas are a vector too (`=A1&"<img onerror=...>"`). It needs a strict
**allowlist** that renders to a fixed tag set and escapes everything else, not a
sanitizer that removes bad things. Precedent: hex colours are whitelisted because that
is the only thing between a user string and a `<style>` element. Same reasoning,
higher stakes.

**Inline only.** Block markdown introduces newlines, which breaks the renderer's
one-line-per-render invariant (the Datastar SDK splits payloads on `\n`) and would need
variable row heights. A cell is one line.

## The fan-out fix — DONE, and what it did not fix

**Built.** Every render path now reads through one shared, single-flighted cache
keyed on `(sheet handle, mutation version, row range)`, so N viewers woken by one
edit cause one database read instead of N.

Measured, same harness both ways, one edit on a hot sheet:

| viewers | database reads caused by ONE edit, before | after |
| --- | --- | --- |
| 32 | 32 | **1** |
| 128 | 128 | **1** |

The first attempt cached the wrong thing and is worth recording. The obvious
target is the window render, but an **edit does not go through it** — it takes the
incremental `pushCells` path, and caching `renderScreen` changed nothing at all
(measured: 33 renders for 32 viewers). The bottleneck was `sh.WindowCtx`, the read
underneath *every* path, which is what queues behind the eight-connection pool.
Caching the read rather than the markup is also what makes it general: first
paint, scroll edges and edits all share one entry.

**The markup is still per viewer, and that is not laziness.** Whether a cell
arrives as a morph or an insert depends on what that browser already holds
(`screen.heldCell`), so the patch genuinely differs between two people looking at
the same rows. Only the data behind it is common. `renderScreen` is the one
exception — its window markup is viewer-independent apart from the selection — so
it caches its rendered halves too, above the same read.

**Invalidation is a mutation counter bumped by the actor**, which is the single
point every write passes through, so a new write verb is covered without anyone
remembering to add it. `TestEveryWriteVerbMovesTheVersion` drives all ten verbs
through the real actor and insists the version moved; the failure it guards is not
a slow page but every viewer being served a window that silently predates the
edit. The handle pointer is part of the key as well, because a reaped id revived
as a fresh sheet restarts its counter at zero and would otherwise collide with an
entry cached at zero.

**What this does NOT justify yet: raising `StreamsTotal` back above 64.** The
reads are flat now, but the per-viewer work that remains — building each patch,
and writing it to each socket — still scales with N, and that was never separately
measured. Raising the cap is its own measurement, not a corollary of this one.

## Bulk write performance

The last O(cells) hole. Measured: clear/fill/paste are ~0.09-0.13 ms per cell, so 1,000
cells is ~130 ms and Ctrl+A over a full grid would be ~34 s **holding the sheet's single
writer**. Hence the shared 2,000-cell cap.

Per-cell cost tracks the **dependency cascade, not the loop** — paste was 30% cheaper
than fill only because its dirty set was smaller. So optimizing means attacking recalc
fan-out, not the write loop. The column/row style cascade removes the most common
reason to hit the cap (styling a whole column is now one record), which lowers the
urgency considerably.

## Variable row heights

Manual row resize, then fit-to-contents. The 22px constant is load-bearing in ~8 places
(scroll handler, hit-testing, editor position, active-cell box, selection box, keyboard
reveal, container height, the grid-line gradient).

**The structure already exists**: offset = `row × defaultHeight + Σ(deltas before row)`,
and the prefix-sum-over-bands shape is exactly what `bands.nrows` already does for the
extent. Heights go in the `rows` table keyed by storage key `k`, which the cascade work
is already creating with room for a `height` column.

**Rendering gets simpler, not harder**: ship `--y` as an absolute pixel offset instead
of `--r` and the client does no arithmetic at all. Hit-testing still needs a pixel→row
map (ship the buffer's exceptions, not every boundary). The horizontal grid-line
`repeating-linear-gradient` is the casualty — try a server-generated multi-stop gradient
before per-row elements.

**Manual resize reuses the guide line** (`T.gdShow`) already built for both axes.

**Autofit is the one genuine crack in the thesis**: the server cannot know rendered text
height without a layout engine. The honest design is *measurement is a client
capability, height is server state* — the client measures and sends it as a command.
That is the same shape as the editor, where only the client knows what was typed.

## Conditional formatting

A predicate plus a style, evaluated per cell. Distinct from static styling. Fits the
cascade as a fourth level and the recalc graph already knows how to invalidate on
dependency changes. Wait until the cascade has settled.

## Size the scroll container from `UsedRows()`, not from the allocated extent

`DefaultRows` is 10,000, so a blank sheet is a 220,000 px scroll container holding
nothing, and twenty rows of data give you a thumb that is 0.2% of the track. **This is
the only real cost of the 10,000 default** and it was accepted knowingly — Google Sheets
ships 1,000 for exactly this reason and then makes you press a button, which is the
trade we took in the other direction (10,000 plus a button, growrows.go).

**The fix is to stop conflating the two extents in the render layer.** `--rows` is
`(*Sheet).Rows()` — the ALLOCATED extent, "how far you may address". What a scrollbar
should describe is `(*Sheet).UsedRows()` plus enough slack to type into — the same
number `Ctrl+End` means. Both already exist and neither is new work; the change is one
value in `patchExtent`/`vpInlineStyle` plus a rule for the slack.

**The two things that make it more than a substitution:**

1. **`UsedRows()` is a query, not a field.** It takes an error return and it is a
   backward primary-key scan. `--rows` is currently free (the band index already sums
   it), so this puts a query on a path that had none — it has to be cached per push or
   folded into the same read the window does.
2. **The slack rule decides whether the grid feels broken.** Too little and you cannot
   scroll below your data to start typing; too much and the scrollbar is dishonest
   again. Sheets' answer is roughly "a screen or two past the end, then the button".
   The allocated extent stays the authority for *addressing* — `?at=A9000` on a blank
   sheet must still work — so this is a rendering rule and not a storage one.

## Synthesized scrollbar

Only needed past ~750k rows, where a real scroll container hits browser element-height
limits (Chrome ~33.5M px, Firefox ~17.9M; at 22px/row that is ~1.5M and ~810k rows).
Excel's own 1,048,576 rows does not fit in Firefox as one div.

Grow-on-demand (what Sheets does, what we do) carries us well past any realistic sheet.
The escape hatch past that is a **compressed mapping** — fixed container, wheel scrolls
row-precise, scrollbar drag scrolls proportionally — which is additive and needs no
storage change. Full synthesis trades away trackpad momentum for a capability with
near-zero demand; do not.

## A cell-count budget instead of a coordinate ceiling

`RowCeiling` is 1,000,000, which bounds nothing real because storage is sparse. Google
Sheets caps **10 million cells per document** — a resource limit that actually
corresponds to memory, recalc time and payload. That is the limit worth enforcing.

## Smaller items

- **System-clipboard TSV paste** (paste from Excel/Sheets). Skipped as a stretch during
  range ops; good demo value.
- **Vestigial parameters**: `WriteCell`'s `deps []DepEdge` and `RecalcResult.Deps` are
  accepted and ignored since the `deps` table was deleted. `MutateStats.DepsMoved` and
  friends are always zero. Two-line cleanup, kept only because `structure.go` logs them.
- **Cross-sheet formulas** (`=Other!A1`) are **structurally impossible**, not merely
  disallowed: a dirty set spanning two SQLite files cannot be one transaction.
  Supporting them means changing storage, not the parser.
- **`RESULTS.md:622`** still calls insert-row "the known worst case, deliberately
  unbuilt". Stale since the band-key work took it from 330 ms to ~3 ms.

## Columns past Z (AA, AB, …) — two projects wearing one name

Sounds like a one-liner. It is not, and the split matters.

**Trivial:** bijective base-26 naming in `ParseRef`/`String` — ~30 lines plus tests.
Note it is *not* standard base-26; there is no zero digit, which is where people get it
wrong. Excel stops at `XFD` (16,384).

**Annoying, and worth doing early:** `[id^=A]{grid-column:2}` breaks the moment a second
letter exists, because `[id^="A"]` also matches `AA1` and `AAA1`. Those generated rules
are what keep the column *out* of per-cell markup — worth +41 points of compressed
break-even when measured. The fix is a separator in the id (`A_1`, selector
`[id^="A_"]`, which `AA_1` does not match): **one byte per cell**, against ~11 for a
class or `data-c` attribute.

Do this part **before** the ids are any more entrenched. They are already used by patch
selectors, the editor's `$ref`, `heldMask`, the remove selector, flash attribution and a
lot of tests — it is mechanical but broad, and it only gets broader.

**The actual cliff:** the window read is a **dense `(rows × MaxCols)` rectangle**, which
is only affordable because all 26 columns are always on screen. There is no horizontal
windowing anywhere in the system. At Excel's 16,384 columns a 250-row window is **4.1
million cells per read**, and `--w-N` becomes 16,384 custom properties on the page shell.

Going wide therefore means building **the column axis of everything already built for
rows**: horizontal buffer with over-fetch, column bands, edge detection, a 2D viewport
command, and incremental patches in both dimensions. Comparable in size to the original
buffer work.

| target | cost |
| --- | --- |
| 26 → ~100 | Easy. Naming + separator. Dense window 6,500 → 25,000 cells; measure. |
| → ~500 | ~125,000 cells per read. Needs measuring seriously; probably column banding. |
| → 16,384 | Full 2D windowing. Weeks, not days. |

**Recommendation:** do the naming and the `A_1` separator now, cap at a few hundred, and
treat 2D windowing as its own project only if something actually demands it. Real sheets
overwhelmingly use fewer than 50 columns and Sheets itself defaults to 26.

## Live style preview (queued, not backlogged — next dispatch)

Colour picker previews on the selection in realtime, persists on blur. Same
manipulate-locally-commit-discretely pattern as the editor, the resize guide and the
selection box. `<input type="color">` fires `input` continuously and `change` on commit.

The preview splits cleanly with no gap: **fill colour tints the existing `#sb` selection
overlay** (one element, works over empty cells — which is exactly where sparse rendering
gives no elements to colour), while **text colour / bold / italic toggle a class on the
selected cells' elements client-side** (~3 ms for 1,000 elements, measured). Text colour
only matters where there is text, and cells with text always have elements.

Must revert if the commit fails (reuse the pending marker's error path) and must clear
when the confirming push lands, or a stale preview class fights the real one.

## Timeline scrubber (deferred — wants a full-featured version)

Every write already appends to `events(seq, ts, ref, raw, prev)`, and **`prev` is the
enabling detail**: you can walk *backwards* from current state instead of replaying from
zero. `(*Sheet).Events(afterSeq, limit)` and `LastSeq()` already exist. The log is
trimmed at 100,000 entries (gated at `seq % 1000`), so history is deep but not infinite.

**Scope it per-screen, not globally.** A scrub position is per-connection state exactly
like the buffer window — the server already renders each connection's own view from its
own parameters, so scrubbing is one more render argument. A global scrub would rewind
every viewer's screen from one person's private act of looking, and would turn a read
into a shared-state write.

That also makes it cheap: reconstruct only the **visible window** at seq N by taking the
current window and reverse-applying the `prev` of each event after N that touches a cell
in it. Bounded by recent history, not by sheet size.

**The two things that make a good version hard, and why this is deferred:**

1. **Formulas.** Events store `raw`, not `computed`. Reverse-applying restores the text
   but not the values at that moment, and a formula can reference cells outside the
   window, so a faithful historical view needs a historical recalc over an unbounded
   set. The cheap version shows raw content (what F2 shows) marked as historical; the
   full version needs snapshotting or a recalc-at-seq path.
2. **Structural mutations.** Row/column inserts and deletes are in the same log, and
   reverse-applying an insert means deleting a row — which shifts display ranks and
   interacts with the band keys. A v1 would have to stop at a structural event and say
   so.

A full-featured version probably wants periodic **snapshots** so reconstruction is
bounded, plus a recalc-at-seq path, plus structural replay. That is a project, not a
control.

Note it composes with attribution once the logging work lands: with `conn` and the
presence name on every write, the same log supports "who changed this, and when" — which
is arguably more valuable than scrubbing and much cheaper.

## Latency transparency chip (NEXT UP — dispatch when render.go frees)

A small readout in the top bar showing the viewer their own round trip alongside the
server's own cost, so someone far from the origin can see *where* the latency is instead
of assuming the app is slow.

**Measure the real round trip, not a synthetic ping.** A browser cannot ICMP, and the
number that matters is keystroke → confirmed value. The client already measures exactly
this and reports it as `client_prev_wait_ms`; surface it rather than inventing a probe.
Measured today: **32 ms p50** from a machine 11.8 ms away.

**Show it as a split**, because the split is the whole point:

    you ↔ server 180 ms  ·  server 0.4 ms

That makes it unambiguous that distance is the cost. A single blended number invites
exactly the misreading this feature exists to prevent.

**What is safe to publish**, and the reasoning:

| show | why |
| --- | --- |
| server region | the actual explanation for a slow round trip |
| server-side p50 command time | proves the app is not the bottleneck (0.35 ms today) |
| the viewer's own round trip | their real experience, not an average |
| viewers on this sheet | already visible via presence chips |

**Do not show**: any IP, other sheets or a global sheet count, disk paths, load average or
memory. Load average in particular is a weak signal that invites "is it struggling?" for
no diagnostic gain, and infrastructure detail invites probing on a service with no auth.

**Implementation notes.** Region is a static flag. The server's p50 is a rolling figure
patched as a signal **only when it moves materially** — do not add a per-interval push per
viewer, since idle viewers currently cost zero and that property is worth keeping (47% of
pushes are already suppressed). The client's own RTT is already in hand. The chip is
page-shell furniture: it must not touch per-cell markup, and a stale value is better than
a push.

Keep it quiet visually — this is an honesty affordance, not a dashboard. One line, muted,
with detail on hover if anything.

**The tooltip is the point of the feature**, not decoration on it. The chip shows a
number; the tooltip explains that the number is a *deployment* property, not an
application one. Something close to:

> This demo runs on a single server in **{region}**. Your round trip is **{rtt} ms** of
> network distance; the server itself handled your last change in **{server} ms**.
> A real deployment would run closer to you — nothing here is doing that work.

Three things that phrasing gets right and are worth preserving in whatever wording ships:

1. **It names the cause before the excuse.** "One server, in this region" is the fact;
   everything else follows from it.
2. **It quantifies both halves**, so the reader can check the claim rather than take it.
   With the split visible, someone 200 ms away can see that 199 ms of it is not the app.
3. **It says what would change**, which is the difference between an explanation and a
   disclaimer. The fix is topological — more origins, or one nearer the reader — and that
   is a deployment decision, not an architectural one.

Do not apologise, and do not hedge the app's own numbers — 0.35 ms median render under 64
concurrent connections is the strongest thing this demo has to say, and burying it inside
an apology for network distance would waste it.

---

## A second region (PARKED 2026-08-18 — research only, no decision taken)

The tooltip above says the fix is topological. This is what acting on it would cost,
measured rather than estimated, so nobody has to re-derive it.

**The gap, measured from the LA box (`ping`, 5 packets, min/avg/max):**

| From the LA box to | RTT |
|---|---|
| AARNet (Sydney / Canberra) | **136 ms** |
| iiNet, Internode (Perth, Adelaide) | **201 ms** |
| kernel.org (US) | 32 ms |
| sheetstream's own work | **~1 ms** (p50 render 0.35 ms, edit 0.96 ms) |

So ~99% of an Australian tester's felt delay is the Pacific, and every commit,
selection and buffer-edge scroll pays it once. This is the number the tooltip is
about; it is not an estimate.

**Regions available on exe.dev:** `lax nyc dal fra tyo syd sgp lon`.

**Shared sheet state across regions is NOT a goal** (stated 2026-08-18). That makes
this N independent demos rather than a distributed system — one actor and one SQLite
file per sheet on one box is the design being proven, and replication would dismantle
it. The one consequence to design for: **a link crosses regions even though nothing
else does**, so a SYD sheet opened from the US is slow, and that should be visible in
the chrome rather than discovered.

**The actual prerequisite is Phase 0, not the VM.** The LA box was hand-built — user,
dirs, systemd unit, logrotate, binary — and there is no provisioning script anywhere
in this repo. Cloning that by hand means the two boxes drift immediately and every
later region is another hand-build. What is needed first is `deploy/`: `provision.sh`
(usable as exe.dev's `--setup-script`, so a region provisions at VM creation),
`sheetstream.service`, `logrotate.conf`, `push.sh <host...>` (cross-compile once,
install to N), and the logo-seeding script, which we now want on every region.

The app itself is trivially portable: **total state is 2.8 MB of sheets + 34 MB of
logs.** The 5.2 GB on the box is the `exeuntu` base image.

**The idea worth keeping if this is ever picked up.** Add `GET /ping` (204,
`Access-Control-Allow-Origin: *`, no-store) and have the index page MEASURE every
region from the visitor's own browser, show the medians, and point "New sheet" at the
fastest. ~10 lines of Go and ~60 of JS. It converts the demo's one persistent
criticism into its best moment: instead of a tooltip *claiming* latency is a
deployment property, a Sydney visitor reads `Sydney 11 ms · Los Angeles 154 ms` and
is routed. The claim becomes a measurement the reader takes themselves — which is the
same move the latency chip already makes, applied one level up.

**Constraints that would bite, in the order they would bite:**

1. **Small shared-core hosting means regions contend for one CPU pool.** The
   64-stream cap was measured on a 2-vCPU box *in isolation*; two busy boxes drawing
   on a shared allowance contend, so the cap is optimistic the moment a second region
   is also under load. Re-measure before trusting it across regions.
2. Sheet ids are unique PER BOX. Harmless while hostnames differ; never assume
   otherwise. Cheap hedge if one hostname is ever wanted: encode the region in the id
   (`syd-abc123`) so routing is a pure function of the URL. Free now, awkward later.
3. `-index-reveal` becomes a secret shared across more machines the moment it is in a
   provisioning script. Per-box value, or drop it.
4. Cloudflare CNAME flattening must stay OFF, the record must be DNS-only, and
   exe.dev's resolver caches — `domain add` failed for ~10 minutes after the record
   was already correct, then succeeded unchanged. Budget for that, do not debug it.
5. Disk and bandwidth are non-issues at 2.8 MB of state and 51x compression.
