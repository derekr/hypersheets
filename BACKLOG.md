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

## Variable row heights — DONE

**Built**, in the four steps SPIKE-AXIS-CONFIG.md recommended: one strip per row,
per-row heights in storage, the drag, and then wrapping with fit-to-contents.

A row's top is a prefix sum over the resized rows rather than a multiplication, on
both sides — `--t`/`--hr` in CSS with the old multiplication as the fallback, so a
sheet nobody has resized emits no geometry at all, and `topOf`/`rowAtY` over sorted
pairs on the client. Only deviating rows get a rule, so it is O(resized rows), never
O(cells).

**Autofit was the one genuine crack in the thesis, and it stayed a crack**: the server
has no font metrics, so the client measures and sends the result through the same
`rowheight` command a drag uses. There is no "automatic" height in the store — a
fitted row is an ordinary resized row afterwards. Two things that were not obvious
going in: fitting means nothing until cells can wrap, so wrap had to ship first as a
level default in the style cascade; and `scrollHeight` never reports less than the box
it is read from, so measuring in place can only ever grow a row.

**Still open on this axis**: column autofit. Same argument, but the server would have
to nominate candidate cells for the client to measure, since the widest cell in a
column is usually outside the buffered window.

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

## Vim-style navigation — three shapes, one conflict (PARKED 2026-08-20)

**The conflict, stated once**: in a grid, a plain letter types into the cell. That is
the single most-used interaction in the whole app, and `h`/`j`/`k`/`l` are letters. So
vim motions are not a keymap addition — they need somewhere to *be*, and choosing
where is the whole design.

This is the same problem as the row grip and the row selection sharing five pixels,
one level up: two modalities want the same input, and the fix is to make the
arbitration explicit rather than to guess. `gripAt` is the pointer's version.

**A. Modal.** A normal/insert distinction with the mode in the header. Normal owns
the letters (`hjkl`, `w`/`b` over used cells, `0`/`$`, `gg`/`G`, `i`/`a`/`cc`/`x`,
`v`/`V`), insert is the editor as it stands, `Esc` returns. The only shape where the
motions are actually vim.

- *Cost*: one client signal for the mode, and `T.key` splits into two tables. The
  keymap is already one function, so it is contained. The mode indicator is a header
  chip on the terms the latency chip is on.
- *Risk*: it changes what typing means for everyone, including people who have never
  used vim. Wants to be opt-in — a toggle, remembered per viewer, defaulting off.
- *Open question*: whether the mode is per-viewer local state or something the
  presence layer shows other people. Local, almost certainly: it is a property of the
  person, not of the sheet.

**B. Non-modal, behind a leader.** Letters keep typing; motions live behind a leader
that arms for one command (`Space j`, `Space g g`). Nothing existing changes meaning
and there is no mode to get stuck in. It also never feels like vim, which may make it
the worst of both — worth deciding on rather than defaulting into.

**C. A command line only.** No motions. `:` opens a bar: `:42`, `:C7`, `:A1:D20`,
`:w 200`, `:fit`, `:sort A`. One input, one parser, no key changes meaning.

**C is the piece a grid genuinely lacks** — a way to say where you want to be —
and it is independent of A and B rather than a lesser version of them. It could ship
first under any of the three, and it composes with the existing anchor (`?at=`) and
`ParseRef` rather than adding a coordinate system.

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

---

## Offline is a COPY of the sheet, not a mode on it (EARMARKED 2026-09-01 — direction chosen, nothing built)

"Does it work offline?" is the demo's most common objection, and it is four
questions wearing one word: *will it survive my elevator*, *will I lose my work*,
*can I read this on a plane*, *can I write on a plane and see the result*. The
first three are already cheap. Only the fourth is architecture, and it gets two
answers, both of which are refusals to build a sync engine:

1. **Relocate the server** — the same Go, compiled to wasm, serving the same HTML
   from a service worker. Offline becomes a deployment topology, not an
   architecture.
2. **Fork the sheet** — what you take offline is a *copy*, with its own id, its
   own database and its own URL. Nothing merges by itself.

The second is the load-bearing one, and it is what makes the first affordable.

**This topology has already been built twice in the neighbouring trees, and most
of what follows is their measurements rather than this project's reasoning.** See
`../hypermedia-sw-demo/` (the live TypeScript demo: service worker is the HTTP
server, SQLite replica in a SharedWorker, syncular for the protocol) and
`../sw-datastar-sync-archive/` (fourteen spikes with the numbers behind each
decision, including `gosw/`, which is this exact idea in Go). Read those before
re-deriving anything below. **What is genuinely open here is not the topology —
it is whether the *sheet* domain fits inside it**, and that is a size question and
a fan-out question, both stated in Phase 0.

**The borrowed frame is [Monzo Stand-in](https://monzo.com/blog/tolerating-full-cloud-outages-with-monzo-stand-in).**
Four ideas from it, all of which apply:

- The backup is a **deliberately smaller system**, not a replica. Card payments,
  balance, freeze. Not mortgages. "A backup of last resort, not our primary
  mechanism of providing a reliable service."
- The **primary stays the system of record** throughout.
- **Failover is triggered by a human**, not by a heuristic. An engineer runs a CLI
  tool. Nothing decides on its own that the world has ended.
- Degradation is **intentional and visible**: the app shows a simplified UI and
  says which one you are in.

**Where the analogy inverts, and why that makes it cheaper here.** Monzo runs
*different software* on purpose — outages are bugs, so independence is the whole
point, and they pay for it by implementing payments twice. The failure being
tolerated here is the **network**, which is not correlated with our code at all.
So the stand-in runs the *same* Go, and the client does not change by one line.
That inversion is the reason this is affordable, and it is the claim worth making
out loud: a thick client cannot say it, because its offline story is a second
implementation that has to agree with the first.

### Why a copy, and not an outbox

An outbox shows you something that **claims to be the sheet and is not**. Everyone
who has used a sync engine has had the moment where the screen and the server
disagreed and nothing said so. A copy cannot lie about that, because it never
claimed to be the same sheet: different id, different URL, different chrome.

That is "refusals are loud" (ARCHITECTURE.md) applied to **identity** rather than
to commands, and it is the same move the latency chip already makes — put the
uncomfortable fact on screen instead of hiding it.

**So the two failures get two mechanisms, on purpose:**

| what happened | mechanism | forks? |
| --- | --- | --- |
| lift, tunnel, wifi blip | reconnect. Absolute-state push means the reconnect path *is* the normal path run once. | **no** |
| a flight, a remote site, a laptop going in a bag | **you press "take a copy offline"** while still online, the way you download an episode before boarding | **yes, deliberately** |

**Nothing ever forks automatically.** A wifi blip that silently forked your sheet
would be the worst outcome available, and it is exactly what "seamless offline"
produces. Monzo's failover is a person running a command; so is this.

**What that deletes, entirely:** the outbox, correlation ids, idempotency keys,
version conflicts, replay-time refusals, and scope authz. The stand-in stops being
"the server plus a reconciliation protocol" and becomes **the server, one user, a
fresh database** — the smallest possible version of the Monzo move.

### Merging: the shipping answer is copy and paste, and that is not a cop-out

`rangeops.go` already implements copy, paste and fill over a selection. If
merging means *open both sheets, select a range in the copy, paste it into the
live one*, then *the merge tool is already built*, the person applies full
judgment, and this ships with **zero new code**.

It is also what people actually do with spreadsheets. Ship that first and say so
— and the spike below strengthens the case, because auto-merge turned out to be
operational transform rather than the join this entry originally assumed.

### If auto-merge is ever wanted — MEASURED, and it is not a join

**Spiked 2026-09-01 on `spike/fork-merge`; see `SPIKE-FORK-MERGE.md`.** Three
claims went in, one came out intact. What follows is the corrected version; the
original reasoning is in the spike document, including the part that was wrong.

**A band key is NOT a row identity.** This was the load-bearing claim of the
cheap version of this entry and it is false. `applyRowOp` calls `shiftKeyRange`
over the band holding the insertion point, which is copy-to-scratch, delete, and
**re-insert at new primary keys** (`mutate.go`); a rebalance re-keys the entire
sheet through its rank map (`rekeyRowMeta`, `bandkey.go`). Measured on a 250x26
seeded sheet with **zero cells edited**:

| operation | rows whose key changed | spurious cells in a key-join 3-way diff |
| --- | ---: | ---: |
| `InsertRows(0,1)` | 50 of 253 | **1,442** |
| `InsertRows(3,1)` | 47 of 253 | **1,379** |
| `InsertRows(175,1)` | 25 of 253 | 627 |

600 inserts at one rank: first band split at #51, **first full rebalance at #329**
— after which every band key on that fork has moved at once. The band model buys
a **cost bound** (1,035 cells rewritten, 2.5 ms), which is what `DATA-MODEL.md`
actually claims. It does not buy identity, and there is no persistent row identity
anywhere in the schema.

**`ARCHITECTURE.md` invites this misreading and should be fixed.** *"A cell is
stored under a band key, not a row number, so inserting a row renumbers nothing"*
is true of display rank and false of storage keys, and the difference is exactly
what a merge depends on.

**The formula-reference exception does not exist either — for the opposite
reason.** `Slot{Ref CellRef}` is the *parsed* form; storage is `ref0_k`, a band
key, and `shiftKeyRange` ends with four indexed `UPDATE cells SET ref0_k =
ref0_k + delta`. References move with their rows. `Z1 = "=A20*1"` tracked its
landmark through every replay, transformed or not. What does not commute is the
**arguments of the structural operations**, which is ordinary operational
transform and has nothing to do with formulas.

**But `Cell.Raw` IS a display coordinate**, materialized from template plus keys
on every read — so the instinct was right about the wrong field. Diffing a
transposed *snapshot* reported **508 phantom cells** (`U100: "=A99*2"` vs
`"=A100*2"`). **Transpose diffs, never snapshots.**

**What survived, and it is the whole of the cheapness.** Only raw text is merged;
`.computed` is never read. Disjoint edits: 4 raw cells taken, 18 re-derived in
1.4 ms, **0 cells different from applying the same edits in one order**. The good
case works exactly as advertised — ours holds `Z10 = "=A10*3"` computing `837`,
theirs sets `A10=500`, and the merge computes `1500`, cell-for-cell identical to
linear. Merge inputs, re-derive outputs.

### The rule, narrowed by measurement

"Refuse all structural divergence" refuses a case that has a correct answer:
insert/insert at distinct ranks merged **exactly** under an argument transform
(2 cells taken, 0 conflicts, identical to linear) where the key join got **4,449
cells wrong**. So:

> 1. **Merge by rank, not by band key.** The key join is sound only when neither
>    side has a structural event — and in that case rank and key agree anyway, so
>    the key buys nothing and fails silently when it is wrong.
> 2. **No structural event either side: auto-merge.** Diff raw text, take
>    one-sided changes, report two-sided disagreement as a conflict, `ApplyBatch`,
>    let `recalc.go` do the rest.
> 3. **Structural events: replay with transformed arguments, do not refuse.**
> 4. **Refuse only where the transform is partial** — a value edit landing on a
>    row the other side deleted. Scenario (f) stranded `A6, B6`, and there is
>    genuinely no linearization of "delete this row" and "put 42 in this row".
>    That is the one case that has to be a question for a person.

**The trap that makes "just replay it" worse than it looks:** replaying untransformed,
`theirs-then-ours` was **accidentally correct** (0 cells off) while `ours-then-theirs`
was 44 off. Right about half the time is the worst possible rate for a silent failure.

**Two things that are cheap now and awkward later**, in priority order:

- **A durable row identity.** One monotonic column on the `rows` side table, minted
  once and carried by `shiftRowMeta` rather than derived from `k`. This is what the
  refuted claim assumed already existed. `rows` is already keyed by `k` and already
  moves with its cells, so it is one column and one migration now — and a retrofit
  onto existing sheets later. Put a fork id in it and forks compose for free.
  *(Band-key collision between forks is real and confirmed — both forks always mint
  `k=3` for `InsertRows(3,1)`, because allocation is arithmetic over the band index,
  not a counter. But a disjoint sub-space or fork-id tie-break solves the wrong
  problem: the pre-existing rows move regardless.)*
- **Record the fork point as `(base sheet copy, base seq)` at fork time.** The
  in-memory `editLog` cannot supply a base at *any* depth — it is a field on
  `Server` holding `[]CellRef`, which cells changed and not what they changed to,
  and it does not survive a restart. The durable `events` table can, and it already
  records structural ops as `ref="#rows" raw="insert 5 3"`, which is the divergence
  detector rule 1 needs. But `events` is trimmed, so the base copy is still the
  cheap answer: 208,896 bytes for a 250-row sheet, and forking is already a copy.

**A fork is a file copy, but not of the file you think.** Sheets open
`journal_mode(WAL)`, and after one committed edit on a seeded sheet the `.db` was
4,096 bytes with 358,472 in the `-wal`. **Copying the `.db` alone silently produced
a working, near-empty sheet** — it opened, migrated and rendered four used rows. Not
corrupt, not an error, which is precisely the failure this codebase says is a bug in
its own right. Use **`VACUUM INTO`**: 1.3 ms, 12 KB smaller than checkpoint-then-copy
because it repacks, and it does not depend on the writer being idle — which on a live
server is the whole correctness argument.

### The merge UI is not new UI

It is the grid, a style overlay and two buttons. The style cascade exists;
conditional formatting is already in this backlog and is the same mechanism.
Conflicts render as cell backgrounds, so **a merge review is itself a sheet** —
which is a good thing for a spreadsheet to be able to say.

### What the stand-in is, exactly

**`sheet` + `view`, and nothing else** — the two domains ARCHITECTURE.md's graph
already points *at*. A copy has one viewer, and much of this codebase exists
because a sheet has more than one:

| dropped | because |
| --- | --- |
| `bus` `registry` `presence` — the embedded NATS server | nobody to fan out to, and **measured elsewhere as unaffordable anyway**: `gosw-nats` got nats-server + JetStream *running* in a browser worker, at **5.0 MB brotli and 46–100 MB of non-reclaimable wasm memory**. Rejected there; rejected here. |
| `limits` `reaper` `readonly` `headers` | open-internet policy for a shared demo. A local single user on their own copy is not a threat model. Nothing replays, so nothing arrives at the origin to be refused. |
| `otel` export | nowhere to send it |

**Deliberately not supported**, and the UI should say so rather than degrade
quietly: multiplayer inside a copy (a fork is single-player by definition);
forking a sheet you have never opened online — opening it is what seeds the copy,
so a cold URL offline is an honest 404, not a spinner; the index page.

### The topology, already decided and measured next door

```
browser tab  ──fetch──▶  service worker (HTTP)  ──port──▶  SharedWorker (data plane)
                          routes, SSE streams                sheet domain + SQLite
```

`kanban-topology-spike` chose the **SharedWorker data plane** over both
dedicated-Worker and service-worker-does-everything, and the number that decided
it was **hung-tab staleness: 40 s → 6 ms**. Do not re-open that.

**`gosw/` already proved the Go half.** A real `*http.ServeMux` compiled
`GOOS=js GOARCH=wasm`, serving inside a service worker, with the origin's log
showing **zero** `/api/*` requests for a whole session. Under `GOOS=js` there is
no `net.Listen`, so the handler is published to JS as a callable rather than bound
to a socket — fine, because `http.Handler` never needed a socket. SSE streams
genuinely incrementally through a `ReadableStream` (measured at +503, +1006,
+1511, +2012, +2518 ms), so **the stream survives the move**; that was the thing
most likely to be assumed impossible.

### The idea worth keeping: the stand-in is a shadow, not a fallback

Monzo's line is that both routes are "tested rigorously and continuously, in
production". The version of that here is nearly free: **run the stand-in on every
command while online too**, discard its answer, and compare it against what the
origin pushed. Divergence becomes a number next to the latency chip.

RESULTS.md keeps finding the same thing — **the costs that decide this design are
data movement and round trips, not computation.** A second local evaluation of
every edit spends the one resource that has repeatedly turned out not to matter,
and it is the only thing that stops two evaluators drifting apart between the two
days a year one of them is used. Build it second, not last.

### Phase 0 — the questions the neighbouring trees do NOT answer

Everything about the topology is settled next door. These are not, and they are in
dependency order.

**1. Does the fork/merge model hold? — DONE 2026-09-01, `SPIKE-FORK-MERGE.md`.**
Answer: yes, but not for the reason claimed. Merging inputs and re-deriving is
exact and costs 1.4 ms; matching rows by band key is not, and the refusal rule was
too broad. See the two sections above. The remaining work this exposed is a durable
row identity and a fork point recorded as `(base copy, base seq)` — both cheap now.

**Still unmeasured on this axis, and named by the spike:** styles, column widths and
row heights are invisible to a `Raw` diff (the row level of the style cascade is keyed
by `k`, so it inherits the whole of finding 1); forking under a live writer; more than
one structural op per side; merging across a rebalance; and scale — everything was
250 rows, where the merge is a full `UsedRows` scan per side, so 10,000 rows is
260,000 cells per snapshot and none of the timings above should be assumed to carry.

**2. Size, and it is the gate on the wasm half.** `gosw` measured the real floor
for a Go `net/http` handler in a service worker — and noted it **"barely moves
with handler code"**, which is the encouraging half:

| | |
|---|---|
| `gosw` app.wasm, Go 1.26.1, `-trimpath -ldflags="-s -w"` | **6.01 MB raw, 1.68 MB gzip** |
| plus `wasm_exec.js` | 17 KB |
| `../hypermedia-sw-demo` for comparison (TypeScript) | 774 KB data plane + 848 KB `sqlite3.wasm` |
| **sheetstream first visit today** | **27.9 KB** |

So ~1.7 MB gzip is the honest expectation, roughly **60x the entire page as it
ships**. Survivable *only* as an explicit opt-in — and the fork model already
makes it one, because "take a copy offline" is a button somebody presses. **The
stand-in must never be on the first-paint path.** If it ever loads by default, the
demo has spent its best number on a feature most visitors will not use.

**3. `modernc.org/sqlite` does not build for wasm.** Measured 2026-09-01, go
1.26.1, libc v1.74.4 — both targets fail identically:

```
GOOS=js GOARCH=wasm     -> modernc.org/libc/{errno,limits,pthread,signal,stdio,sys/types}:
GOOS=wasip1 GOARCH=wasm    build constraints exclude all Go files
```

**The seam for the fix is one line.** `sql.Open("sqlite", dsn)` appears exactly
once (`store.go:573`), and every read and write goes through
`(*Sheet).use(func(db *sql.DB) error)`. A wasm-only `database/sql` driver over the
sqlite-wasm build swaps the entire store without touching a query. That is the
most valuable property the store has for this, and it was not designed for it.

**And the storage design underneath it is already solved — steal it, do not
re-derive it.** `sw-store-spike` landed on **in-memory sqlite-wasm → IndexedDB WAL
→ two-slot OPFS snapshot**, with *acked-means-durable* proven under a real
`ServiceWorker.stopWorker` kill at **1,663/1,663**. Note what that sidesteps: the
OPFS sync-access-handle question does not arise on the hot path, and
`syncular-sw-spike` found sync access handles work in a service worker anyway
(only `opfs-sahpool` fails) — including, surprisingly, in Safari.

### The seam this would actually force

ARCHITECTURE.md declines to split the five domains into packages, and gives the
reason: the backward edges. `view -> web` (23 symbols — the page shell asking each
feature slice for its fragment) and `live -> web` (15). **A stand-in build is the
first requirement that would make that split pay**, because it needs `sheet` and
`view` to compile *without* `web`.

So this is the entry that turns "one flat package, deliberately" from a settled
decision into a live trade. `spike/layout` already reports the real graph, so the
cost of cutting it is measurable before anyone commits — and the answer may still
be to keep the flat package and not build this. **Phase 0 item 1 needs none of
it**, which is another reason to do that one first.

### Constraints that would bite, in the order they would bite

1. **Size.** Phase 0 item 2 gates the wasm half and is measurable in a day.

2. **The cost will be render fan-out, not sync — and that has already fooled
   someone once.** `kanban-envelope-spike`'s headline is that the bottleneck was
   misdiagnosed: **94.6% of the cost was render fan-out, not the sync layer**, and
   "the outbox queueing phase is a sync cost" is on its list of overturned
   conclusions. This project is *made* of render fan-out. Budget accordingly, and
   do not start by optimising a protocol — especially now that there barely is one.

3. **A service worker's cold start wipes in-memory state, and this codebase keeps
   the diff baseline there.** `gosw` measured it: after 45 s idle Chrome killed the
   worker, `bootID` changed, `requestCount` reset 4 → 1, uptime 22.2 s → 0.005 s.
   Every Go global, cache and goroutine is gone. Here that is `screen.heldCell` —
   *what this browser already holds* — which is what decides morph-vs-insert. The
   good news is the degradation already exists and is already correct: `editlog.go`
   falls back to a full morph when history is lost, and "losing history must
   degrade to *send everything*, never to *send nothing*". Make the stand-in reuse
   that path rather than inventing one.

4. **Two independent version axes, and a SharedWorker has no upgrade lifecycle at
   all.** `../hypermedia-sw-demo` lost hours to this and its fix is the one to copy
   verbatim: the **build id goes in the SharedWorker's name**
   (`demo-dp-a@38caf19f0e8b`), because a new client connecting to the same name is
   handed the *existing running instance with its old code* — there is no waiting
   state to skip and no update to force, and one forgotten tab pins the whole
   origin to a stale build. The **schema id goes in the replica's database name**
   (`todos-a@<schemaId>`), because code and schema are invalidated by different
   edits; ours would hash `migrate()`'s output shape. During the overlap two
   engines can share one IndexedDB WAL and shred each other *below* the SQL layer,
   which is what the exclusive `navigator.locks` writer guard is for — 0 violations
   measured across every overlap.

5. **A stale service worker is the most likely failure, and it fires every time the
   code changes under an open browser.** Both sides carry the same build stamp and
   assert it on `page-hello`; on a mismatch, escalate `update()` → `unregister()` →
   say so. Measured recovery next door: **~205 ms, 3 document loads, local rows
   preserved.** Also: the first navigation is uncontrolled, so `skipWaiting()` on
   install plus `clients.claim()` on activate, or the first load goes to the
   network.

6. **Bootstrap is the scale limit, not concurrency.** `kanban-scale-spike` found 50
   concurrent clients fine and bootstrap the wall; the fixes were chunked,
   cursor-resumable, usable-before-complete, and **brotli q2 rather than q5**.
   Forking a `-seed-rows 10000` sheet — 220,404 cells, 5.4 MB — is precisely that
   problem, and it is the largest thing this project brings that the todo demo did
   not. The fork model helps: a copy is taken **while online, on purpose**, so the
   transfer has a progress bar and a person waiting for it rather than having to be
   invisible.

7. **Formula determinism has to stay true.** It currently is: the grammar is refs,
   numbers, four operators and `SUM` (`formula.go`), with nothing reading a clock
   or an entropy source. "Merge inputs, re-derive outputs" depends on it entirely.
   The day `NOW()` or `RAND()` is added it must be frozen at commit and stored as
   an input, not evaluated twice. Cheap to hold, expensive to retrofit.

8. **Sheet ids are unique per box**, per the second-region entry, and a fork mints
   a new one. Key a local store on `(origin, id)` from the start — free now,
   awkward later — and give a copy an id that says what it is a copy *of*.

9. **People must not lose track of which sheet is real.** The failure mode a fork
   model owns. The hypermedia answer is to make forks **visible from the origin**
   rather than hidden in a browser: the presence layer already knows who is
   attached, and "3 open copies of this sheet" in the chrome is a few lines. A copy
   should also say, in its own chrome, what it forked from and when.

10. **Instrumentation perturbs, and this project has a lot of it.** One of the
    archive's three recurring lessons, and the concrete case is brutal: **a 60 ms
    status poll prevented service-worker updates entirely** — the probe blocked the
    thing it was measuring. `otel.go`, the latency chip and any stand-in divergence
    counter are all candidates for exactly that.

11. **CSP and the shell.** Instantiating wasm in a worker needs
    `script-src 'wasm-unsafe-eval'` (`headers.go` sets our policy). And the page,
    the CSS and `vendorjs/` all have to be precached for an offline paint —
    `assets.go` already content-addresses everything as `/a/{sha256}/{name}` with
    `immutable` and a year of freshness, which is the hard half done for unrelated
    reasons.

### What would make this worth building at all

**Not offline.** Offline is one use of the mechanism, and on its own it is a weak
trade: considerable complexity for something a small fraction of visitors would
touch.

The mechanism is **branching**, and what-if scenarios are the thing spreadsheets
handle worst — everyone duplicates a tab, edits both, and loses track of which is
real. Fork, diff and merge are useful *online*, where the network is fine and the
motive is "try it without breaking the model". Offline falls out of them. So does
export, which `README.md` currently lists as missing. That is one mechanism
earning its keep three times, which is the actual test.

And then the demo moment is still there: go offline, edit a formula in the copy,
watch a four-hop dependency chain recompute correctly, come back, and merge — with
the same HTML, the same commands, and no client application code in either state.
The neighbouring todo demo cannot show that half, because a todo list has no
derived state to recompute.
