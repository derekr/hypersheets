# Spike — DuckDB as a read model

**Question.** Would DuckDB earn a place beside the sheet store, and if so, where?

**Answer.** Yes, as a *fed* read model over a *pivoted* table, built per open
analytics panel. No, in every other arrangement — and the cheapest-looking
arrangement, `ATTACH`ing the SQLite file in place, is the one that fails hardest.

Run it: `go build -o /tmp/duckspike . && /tmp/duckspike -db /tmp/bigdata/big.db`

## The fixture

A sheet seeded through the app's own `Seed`, so the schema and the band-key
layout are real, not a mock-up:

```
200,000 rows x 26 cols = 4,408,004 cells, 136 MB SQLite file
```

The feature under test is a column-analytics surface — the aggregate a status
bar shows, the group-by behind a pivot, the sort behind "order by this column",
the filter behind an autofilter. Those are the four shapes the current design
cannot answer at scale. The fifth query is the control: the app's own hot path,
one screen of cells, which every push already reads.

Four engines. `tall` and `wide` hold the same data; the gap between them is the
cost of the storage model, not of DuckDB.

## Query

| query | sqlite | duck/attach | duck/tall | duck/wide |
| --- | ---: | ---: | ---: | ---: |
| aggregate | 195 ms | 266 ms | 3.3 ms | **0.80 ms** |
| group-by (pivot) | 325 ms | 229 ms | 10.3 ms | **1.2 ms** |
| sort top-N | 171 ms | 242 ms | 3.6 ms | **0.53 ms** |
| filter + aggregate | 167 ms | 216 ms | 3.3 ms | **0.43 ms** |
| **window (the app's hot path)** | **1.7 ms** | 300 ms | 1.6 ms | 0.78 ms |

Median of five, after a warm-up, every row drained.

**ATTACH is a dud, and it was the obvious idea.** Reading the SQLite file in
place needs no copy and no migration, which is exactly why it looks like the
right first move — and it buys nothing (0.7x–1.4x), because the scan still goes
through SQLite's pages one row at a time. DuckDB's execution engine over a row
store is not a column store. Worse, it is catastrophic on the hot path: 300 ms
against SQLite's 1.7 ms, ~180x slower, because the predicate does not reach the
b-tree.

**The pivot is where the wins are.** `cells` is `(k, col, value)` — an EAV
layout, which is the right call for a sparse grid and the wrong shape for a
column store. Handed the tall table DuckDB gets 31–60x; handed one column per
sheet column it gets **245–390x**. Most of the benefit is the pivot, not the
engine.

## Freshness, which decides it

Speedups are worthless on their own here: this app's premise is that somebody is
always typing, so the real question is what one edit costs to fold in. The write
path currently commits a cell in ~0.1 ms.

| refresh | cost |
| --- | ---: |
| full rebuild | 590 ms |
| re-read one row from SQLite | 261 ms |
| re-read 250 rows from SQLite | 246 ms |
| **one cell, `UPDATE`** | **0.21 ms** |
| **250 cells, one `UPDATE`** | **0.24 ms** |

**A read model here cannot be polled.** Re-reading one row costs almost what
re-reading all 200,000 costs — the predicate does not reach SQLite, so every
"incremental" refresh is a full scan.

**It does not have to be polled.** The write path already knows what changed: it
holds the cell, the row and the value at the moment it commits, which is the
same reason the push knows what to send. Fed rather than polled, an edit is a
single-column write at 0.21 ms — the same order as the write it is following.
`Actors.exec` is already the single write chokepoint, so there is exactly one
place to tee from.

## What it costs

- **cgo.** There is no pure-Go DuckDB. This ends
  `CGO_ENABLED=0 GOOS=linux go build` and the one-line scp deploy.
- **62 MB binary**, against the app's 24 MB.
- **6.5 MB of memory** per sheet model (200,000 rows). Treat that as a floor:
  the seeded values cycle through 1,000 distinct numbers and compress
  accordingly, so real data will be larger — but the order of magnitude means a
  model is affordable per *open panel*, and 590 ms is a perfectly good cost for
  opening one.

## Shape, if it is built

    SQLite stays the truth and the write path.       unchanged
    Actors.exec tees committed cells to DuckDB.      one place, 0.21ms
    DuckDB holds ONE pivoted table per open panel.   6.5MB, 590ms to build
    Analytical reads go to DuckDB.                   245-390x
    Window reads never do.                           SQLite is already 1.7ms

Never `ATTACH`. Never poll. Never route the hot path through it.

## What this does not answer

- Text columns. Everything measured is numeric; the pivot casts to `DOUBLE` and
  drops anything else. A real model needs a type per column, which is a
  question about the sheet's own type system before it is a question about
  DuckDB.
- Recalc. Formula evaluation is a dependency walk, not a scan, and nothing here
  suggests it wants a column store.
- Cross-sheet queries, which BACKLOG.md calls structurally impossible because a
  dirty set spanning two SQLite files cannot be one transaction. DuckDB can
  `ATTACH` several databases at once; whether that extends to transactional
  writes across them was not tested and should not be assumed.
