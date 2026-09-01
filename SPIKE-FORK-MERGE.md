# Spike — offline as a fork, and what merging two forks costs

**Question.** If "take a copy offline" minted a real fork — its own sheet id, its own
SQLite file, nothing merging automatically — is merging two forks back together
unusually cheap in this codebase? Three claims were under test: that band keys make row
matching a join rather than an alignment problem; that only inputs need merging because
`recalc.go` re-derives the rest; and that formula references, being display coordinates,
make two independent structural mutations non-commuting.

**Answer.** One of the three is right. **Claim 2 is confirmed and is the whole of the
cheapness** — a value merge is exact, and re-deriving is cell-for-cell identical to
applying the same edits in one order. **Claim 1 is refuted**: a band key is not a row
identity, it is a cost bound, and a single row insert changes the key of 47 rows and
makes a key-join three-way diff report **1,379 changed cells where zero were edited**.
**Claim 3's conclusion is right and its mechanism is wrong**: formula references are
stored as band keys, not display coordinates, and they survived every replay intact —
what does not commute is the *arguments of the structural operations*, and the raw text
the merge reads.

Evidence: `spike/forkmerge`. Run `go run ./spike/forkmerge`, or one part with
`-case=keys`.

Two notes on shape before the findings. The backlog entry this spike was dispatched
against (*"Offline is a COPY of the sheet, not a mode on it"*) **is not in `BACKLOG.md`
on `main`** — the claims tested here are the ones in the dispatch. And the experiment
lives in `forkmerge_spike_test.go` at the repo root rather than in `spike/forkmerge/`,
because a program in `spike/` cannot import the root's `package main`, and `*bandIndex`
— the subject of claim 1 — is unexported besides. `spike/forkmerge/main.go` is the
runnable front door; that is a small cost of the arrangement `SPIKE-LAYOUT.md` settled
on, worth knowing before the next spike needs the real API.

---

## Finding 1 — a band key is not a row identity. Claim 1 is refuted.

`bandkey.go` says an insert "renumbers the rows below it without writing to them". That
is true of the rows in *later* bands. It is not true of the rows in the band holding the
insertion point: `applyRowOp` calls `shiftKeyRange(r, +n)` over them, which is
copy-to-temp / delete / re-insert **at new primary keys**.

Seeded sheet, 250 rows x 26 columns, one insert, nothing edited:

| operation | rows whose key changed | spurious cells in a key-join 3-way diff |
| --- | ---: | ---: |
| `InsertRows(0,1)` | 50 of 253 | **1,442** |
| `InsertRows(3,1)` | 47 of 253 | **1,379** |
| `InsertRows(60,1)` | 40 of 253 | **1,133** |
| `InsertRows(175,1)` | 25 of 253 | **627** |

Zero cells were edited in every row of that table. The band model bounds the damage to
one band — 1,035 cells rewritten in 2.5 ms for the `at=3` case, 0 formulas rewritten —
and **that bound is what the band keys buy: a cost, not an identity.**

It gets worse over a session. Hammering one position on a single fork:

    600 inserts at rank 3: first band split at insert #51,
                           first full rebalance at insert #329,
                           58,300 cells rewritten in total

A rebalance is the one O(sheet) operation in the system and it re-keys everything, so
after insert #329 **every band key on that fork has changed at once**. A key join across
that fork is not merely noisy, it is total nonsense.

And the column half of the join key has no band model at all:

    InsertCols(1,1): 0 rows changed storage key,
                     5,264 (key,col) pairs changed content, 10.3 ms

There is no persistent row identity anywhere in this schema. `cells` is keyed by
`(k, col)`, the `rows` side table is keyed by `k` and `shiftRowMeta` moves it with the
cells, and `k` is arithmetic over the band index. Nothing survives a shift.

## Finding 2 — the surprise: the formula references never broke.

Claim 3 predicted that replaying one fork's structural mutation against the other would
"silently repoint formulas at the wrong cells", because `Slot{Ref CellRef, Pos, Len}` is
over `CellRef{Row, Col int}`.

`Slot` is the **parsed, in-memory** form. Storage is `ref0_k, ref0_col, ref1_k, ref1_col`
(`store.go`, `slotCols`) and the row component is a **band key**, mapped on the way in by
`slotArgs` and back out by `nullSlots.refs`. `shiftKeyRange` ends with four indexed
`UPDATE cells SET ref0_k = ref0_k + delta` statements. References move with their rows.

Measured, with `Z1 = "=A20*1"` reading a landmark value `777777` at `A20`:

| sheet | Z1 raw | Z1 computed | landmark at |
| --- | --- | --- | --- |
| base | `=A20*1` | `777777` | A20 |
| ours — `InsertRows(3,1)` | `=A21*1` | `777777` | A21 |
| theirs — `InsertRows(7,1)` | `=A21*1` | `777777` | A21 |
| replay ours-then-theirs, untransformed | `=A22*1` | `777777` | A22 |
| replay theirs-then-ours, untransformed | `=A22*1` | `777777` | A22 |
| replay with transformed arguments | `=A22*1` | `777777` | A22 |

**The formula tracked its referent through every one of them, transformed or not.** The
stated mechanism for the design rule does not exist.

## Finding 3 — what actually does not commute, and by how much.

The same experiment, looking at the cells instead of the formula. Ours inserted at rank 3
and typed `A4`; theirs inserted at rank 7 and typed `A8`.

    ours-then-theirs (arguments as recorded): "ours" at A4, "theirs" at A8
    theirs-then-ours (arguments as recorded): "ours" at A4, "theirs" at A9

    the two orders DISAGREE in 44 cells:
      A8: ours-then-theirs="theirs"   theirs-then-ours="186"
      A9: ours-then-theirs="186"      theirs-then-ours="theirs"
      B8: ours-then-theirs=""         theirs-then-ours="193"

So claim 3's *conclusion* holds: replaying recorded operations in either order is wrong.
But the cause is ordinary operational transform — theirs' insertion rank 7 becomes rank 8
once ours has inserted a row above it — and it is entirely about **the operation's
arguments and the value edits' addresses**, neither of which is a formula.

The awkward part, and the reason "just replay it" is a trap: **`theirs-then-ours` was
accidentally correct.** It differed from the transformed replay in 0 cells, while
`ours-then-theirs` differed in 44. Replaying without transforming gets the right answer
about half the time, which is the worst possible failure rate for a silent one.

## Finding 4 — claim 2 is confirmed, and it is the entire cheapness.

Only raw text is merged; `fmMerge` never reads `.computed`. After the merge, one
`ApplyBatch` re-derives everything downstream.

**(a) disjoint value edits.** Ours `A5=999, C7=111`; theirs `B7=888, A100=222`.

    merge took 4 raw cells, 0 conflicts
    recalc re-derived 18 cells in 1.4 ms
    merged vs linear (both edits applied in one order to one sheet): 0 differing cells
    computed cells that moved but that nobody merged: 14

**(b) the same cell on both sides.** `A5` -> `1` on ours, `2` on theirs; `D9` -> `same`
on both.

    CONFLICT at A5 (k=4,A): base="124" ours="1" theirs="2"
    conflicts=1, auto-merged=1   (D9 is a convergent edit, not a conflict)

**(c) a formula here, its input there.** Ours writes `Z10 = "=A10*3"`, where `A10` is
still `279`, so on ours alone `Z10` computes `837`. Theirs writes `A10 = 500` and has
never seen `Z10`.

    merged: Z10 = "=A10*3" computed "1500", A10 computed "500"
    11 cells re-derived, depth 5, 1.4 ms
    merged is cell-for-cell identical to applying both edits in one order

This is the claim, and it is exactly as clean as advertised. Neither fork's `computed`
column was consulted; the value `837` that ours held was simply discarded and redone.

## Finding 5 — the join key is stable exactly when there is no structural divergence.

Findings 1 and 4 are the same fact seen twice. Claim 1's cheapness holds precisely under
the condition claim 3 proposes to refuse, so they are not two independent properties —
they are one condition and its consequence.

**(d) a row insert on one side only.** Ours `InsertRows(3,1)` + `A4="inserted"`; theirs
`A100="777"`. Honest answer: two cells.

| merge strategy | cells taken | result vs linear |
| --- | ---: | ---: |
| join on `(band key, col)` | **1,378**, of which 22 named no row on the target | **4,449 cells wrong** |
| replay the op, then transform theirs' diff by rank | **2**, 0 conflicts, 0 orphans | **identical** |

The naive merge left the landmark `Z100` at `Z100` where ours had moved it to `Z101`, and
shifted a whole band of column A up by one against the linear answer
(`A100: naive="777" linear="38"`, `A101: naive="100" linear="777"`).

The disciplined version is short: replay ours' structural events onto a fresh copy of the
base, diff ours against *that* (1 cell), transform theirs' base-coordinate diff through
`rank >= 3 -> rank + 1` (1 cell), apply, re-derive. Cell-for-cell identical to
insert-then-edit.

**One trap inside the disciplined version.** The first attempt transposed theirs' whole
snapshot into the replayed geometry and diffed it, and reported **508 spurious cells** —
every formula in the moved range:

    U100: transposed="=A99*2"/"14"   target="=A100*2"/"76"
    U101: transposed="=A100*2"/"76"  target="=A101*2"/"1554"

`Cell.Raw` is *materialized* in display coordinates on every read, from the template plus
the reference keys. So the storage is coordinate-free and the thing a text merge actually
sees is not. **Transpose the diff, never the snapshot** — and note that this is the one
place claim 3's instinct was right, just about `Raw` rather than about `Slot`.

**(f) a delete on one side of a row edited on the other.** Ours `DeleteRows(5,1)`; theirs
`A6=42, B6=43` on that row.

    key-join merge: 1,313 takes, 2 conflicts
    replay-then-transform: ours 0 cells, theirs 0 cells,
                           2 cells STRANDED on the deleted row: [A6 B6]
    ours extent=10000  theirs extent=10000

The extent does not report the delete (a delete refills the tail to the `DefaultRows`
floor) and the key join does not see it at all — it silently takes the edit at `k=5`,
which now names a different row. The rank transform is *total* for an insert and
*partial* for a delete, and the rows with no destination rank are the ambiguity itself.
There is no linearization to check against here: "delete this row" and "put 42 in this
row" have no order satisfying both. **That is a finding, not a gap** — it is the one case
that genuinely has to be a question for a person.

## Finding 6 — two forks mint identical band keys. Confirmed, and it is not the problem.

    both forks InsertRows(3,1):    ours minted k=3,             theirs minted k=3
    both forks InsertRows(120,2):  ours minted k=[8212 8213],   theirs k=[8212 8213]

    k=3 holds "ours-new-row" on ours and "theirs-new-row" on theirs

Key allocation is pure arithmetic over the band index (`keyOf(rank) = band.base + slot`),
not a counter, so two forks from the same base are not merely likely to collide, they
**always** collide for the same insertion rank.

The suggested fixes — a disjoint key sub-space per fork, or a fork-id tie-break — would
work and are cheap now. But finding 1 says they solve the wrong problem: even with
collision-free minting, the *pre-existing* rows' keys move under an insert, so the join
is still wrong. What is actually missing is a durable row identity that no shift rewrites
— a monotonic `rowid` column on the `rows` side table, minted once and carried by
`shiftRowMeta` instead of being derived from `k`. **That** is the thing that is free to
add now (one column, one migration, `rows` is already keyed by `k` and already moves with
its cells) and expensive to retrofit onto sheets that exist. A fork-id in it makes forks
compose for free.

## Finding 7 — the fork point cannot come from `editlog.go`, and the reason is worse than stated.

The claim was that an offline session overflows `editLogDepth` (256). Confirmed:

    editLog: depth=256; after 306 appends, Since(1) ok=false
             Since(305) ok=true — only the last 256 edits are answerable

But depth is not the binding constraint. `editLog` is a field on `Server`
(`http.go`: `edits map[string]*editLog`), in process memory, and it holds
`[]CellRef` — **which cells changed, not what they changed to.** It does not survive a
restart and carries no values, so it could not supply a three-way base at *any* depth.

The durable log can. `events` holds `(seq, ts, ref, raw, prev)` in the sheet's own file,
100,000 entries deep, trimmed every 1,000 writes — and every structural mutation appends
to it too:

    seq=1 ref="A1"     raw="1"          prev="0"
    seq=2 ref="B2"     raw="2"          prev="38"
    seq=3 ref="#rows"  raw="insert 5 3" prev=""
    seq=4 ref="#rows"  raw="delete 9 1" prev=""

So the divergence detector findings 3 and 5 need already exists and is durable: read
`Events(forkSeq, n)`, filter `ref` to `#rows`/`#cols`. It is also a replay source, though
this spike replayed the operations by hand rather than parsing `"insert 5 3"` back into a
call.

**Keeping the fork point is still cheapest as a third file.** Forking already means
copying the database; keeping the base copy costs one more of the same. On this fixture
that is **208,896 bytes** for a 250-row seeded sheet.

## Finding 8 — a fork is a file copy, but not of the file you think.

`store.go` opens every sheet with `_pragma=journal_mode(WAL)`. A WAL sheet is two files,
and after one committed edit on a freshly seeded sheet almost all of it is in the wrong
one:

    fork-base.db = 4,096 bytes    fork-base.db-wal = 358,472 bytes    -shm = 32,768

| fork method | `A1` in the copy | used rows | size | time |
| --- | --- | ---: | ---: | ---: |
| copy `.db` only | **`""`** | **4** | — | — |
| `wal_checkpoint(TRUNCATE)` then copy `.db` | `"12345"` | 254 | 221,184 | — |
| `VACUUM INTO` | `"12345"` | 254 | 208,896 | **1.3 ms** |

**A bare `.db` copy silently produced a near-empty sheet.** Not corrupt, not an error —
it opened, migrated, and rendered four used rows. That is the exact failure mode this
codebase's own doctrine calls out (`ARCHITECTURE.md`: "a silent failure in this system is
a bug in its own right").

`VACUUM INTO` is the recommendation: SQLite writes the snapshot itself, it is 12 KB
smaller than the checkpointed copy because it repacks, it takes 1.3 ms, and unlike
checkpoint-then-copy it does not depend on the writer being idle or on nobody appending
between the checkpoint and the `io.Copy`. On a live server that difference is the whole
correctness argument.

---

## The rule, narrowed

The proposed rule — *auto-merge value edits; detect structural divergence and refuse it* —
is **right about the auto-merge half and too broad about the refusal half.**

What the measurements support:

1. **Merge by rank, not by band key.** The `(k, col)` join is only sound when neither
   side has a structural event since the fork point, and in that case rank and key agree
   anyway. Using the key buys nothing and fails silently when it is wrong (findings 1, 5).
2. **No structural event on either side: auto-merge.** Diff raw text, take one-sided
   changes, report a two-sided disagreement as a conflict, then `ApplyBatch` and let
   `recalc.go` do the rest. Exact, and 1.4 ms on this fixture (finding 4).
3. **Structural events on one or both sides: replay with transformed arguments, do not
   refuse.** Replay the base-side ops onto a copy of the base, transform the other side's
   operation arguments and value-edit ranks through them, then merge values in the
   resulting coordinate space. Insert/insert at distinct ranks merged exactly (findings
   3, 5). Refusing this whole class would refuse a case that has a correct answer.
4. **Refuse only where the transform is partial**: a value edit, or a formula reference,
   landing on a row or column the other side deleted. That is the one case with no
   linearization (finding 5f).
5. **Never transpose materialized `Raw`.** Transpose diffs, and re-derive (finding 5).

Two things that are cheap now and awkward later, in priority order: **a durable row
identity** (finding 6), which is what claim 1 assumed already existed; and **recording
the fork point as a `(base sheet copy, base seq)` pair** at fork time (finding 7),
because the event log alone cannot reconstruct a base that the trim has passed.

## What was not tested

- **Anything in wasm.** Deliberate; this spike decides whether the model is sound on the
  host, independently of the delivery.
- **Styles, column widths and row heights.** The merge diffs `Cell.Raw` only. A style
  edit, a column width or a row height on one fork is completely invisible to it, and the
  style cascade has three levels (`cells.style`, `rows.style`, `cols.style`) with the row
  level keyed by `k` — so it inherits every problem in finding 1 and was not measured.
- **Forking a sheet with a live writer.** Every copy here was taken between actor
  commands. `VACUUM INTO` is recommended partly for that reason, but concurrent
  fork-under-write was not exercised.
- **More than one structural operation per side**, and no `DeleteCols` at all;
  `InsertCols` was measured once, for the join key only.
- **Merging across a rebalance.** A rebalance was reached (insert #329) but no merge was
  attempted over one.
- **Scale.** Everything here is 250 rows and roughly 5,800 populated cells. The merge
  cost is a full `UsedRows` window scan per side, which at 10,000 rows is 260,000 cells
  per snapshot — that is the number to measure before believing the timings above
  generalize.
- **Diverging row extents.** Both forks reported `Rows()=10000` throughout.
