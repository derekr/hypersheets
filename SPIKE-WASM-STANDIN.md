# Spike — what the stand-in weighs, and whether the store seam is real

**Question.** Two, in dependency order. **(A)** If this server were relocated into a
browser service worker — the same Go, the same HTML, `GOOS=js GOARCH=wasm` — how many
bytes would a visitor download? The backlog entry estimates **~1.7 MB gzip**, extrapolated
from `../sw-datastar-sync-archive/gosw`, which measured 6.01 MB raw / 1.68 MB gzip for a
bare `net/http` handler and observed that the size *"barely moves with handler code"*.
**(B)** `modernc.org/sqlite` does not build for wasm. The entry claims the fix is cheap
because `sql.Open("sqlite", dsn)` appears exactly once and everything goes through
`(*Sheet).use`. Is that seam real?

**Answer.** **The estimate is out by nearly a factor of three, because the observation it
rests on is false here: the handler code costs more than the Go runtime does.** The
stand-in the entry describes — the drop list applied, sheet + view + routing + store — is
**18,426,857 bytes raw, 4,643,859 gzip, 3,330,726 brotli**, and it still needs an 848 KB
`sqlite3.wasm` beside it. That is **4.83 MB gzip all in, 177x the 27.9 KB first visit**,
not the 60x the entry budgets for. **The seam is real** — `sql.Open` really does appear
once — but it is a `database/sql` seam sitting on top of a *filesystem* seam nobody has
counted, and the pragma it carries (`journal_mode(WAL)`) is meaningless in the storage
model `sw-store-spike` landed on.

**Amended below.** Two of these figures were re-measured at link level and one claim did
not survive: the stand-in binary still contains NATS. A deliberately smaller local
stand-in — the model without the view layer — is **2.58 MB gzip**, which is 1.5x the
entry's estimate rather than 2.9x. See findings 10 to 15.

Evidence: `spike/wasmsize`. Run `go run ./spike/wasmsize`, one stage with `-stage=standin`,
the dependency prices with `-deps`, the closure explanation with `-why`, and finding 5's
one-function experiment with `-cut='otel.go:connAttrs'`.

Toolchain, because these numbers mean nothing without it: **go1.26.1 darwin/arm64 host,
`GOOS=js GOARCH=wasm`, `-trimpath -ldflags="-s -w"`** — the same version and the same
flags gosw used, so the comparison is like for like. gzip is `compress/gzip` at
`BestCompression` (= `gzip -9`); brotli is `github.com/andybalholm/brotli` at
`BestCompression` (= `brotli -q 11`), used because no `brotli` binary is installed here.

---

## What was measured, and what was faked

A size number with an unclear denominator is worthless, so this comes first.

**Every stage except `floor` compiles all 43 of the repo root's non-test `.go` files**
(finding 4 is why there is no smaller set), copied into a scratch module and built for
js/wasm. Exactly two rewrites are applied to the copies, both mechanical and both in
`patch()` in `spike/wasmsize/main.go`:

1. the blank import `_ "modernc.org/sqlite"` is deleted — it appears in **five** files
   (`store.go` and the four the `SPIKE-LAYOUT` split produced), not one;
2. `main.go`'s `func main` is renamed to `swRootMain`, so the shim can own `main` without
   deleting boot.

A third rewrite exists but is used only for finding 5's experiment: `-cut` deletes named
top-level declarations from the copies, so one reference can be priced.

**Nothing in the repository is modified.** No source file was touched to make any of this
build; the harness works on copies.

**Added to every stage**, so the floor is comparable with gosw's:

- `shim/jsutil.go`, `shim/bridge.go`, `shim/stream.go` — **taken from
  `gosw/cmd/app/`**, with identifiers prefixed `sw` so they cannot collide with the root
  package. That is the JS `Request` ↔ `*http.Request` bridge and the streaming
  `http.ResponseWriter` + `Flusher` over a `ReadableStream`. gosw's rules came with them
  and were not re-derived: no `net.Listen` under `GOOS=js` so the handler is published to
  JS as a callable; `main()` ends in `select {}` or every `js.Func` dies; nothing blocks
  inside a `js.FuncOf`; every `js.Func` is released except the stream's `cancel`.
- `shim/serve.go` — the entry point. Identical in every stage.
- `shim/sqlstub.go` — a `database/sql` driver registered as `"sqlite"`.

**What is faked: SQLite itself.** No SQL executes anywhere in these numbers. **Every
figure below excludes the SQLite engine.** `../hypermedia-sw-demo/vendor/sqlite3.wasm`
measures **864,752 bytes raw, 399,968 gzip** — add 391 KB gzip to every stand-in number.
`wasm_exec.js` adds a further 16,992 bytes.

**How a stage is defined.** A subset of a flat package rarely compiles, so the harness
compiles whatever the closure demands but holds only the stage's *own* files reachable, by
generating `zz_reach.go` — the address of every non-generic top-level function and method
those files declare. The linker's dead-code elimination then decides how much of the rest
the domain actually reaches. That is a judgement-free denominator: *everything these files
declare, plus everything they transitively call.*

**Reproducibility.** Rebuilding a stage reproduces its **raw** byte count exactly. The
compressed sizes move by a handful of bytes (gzip) and up to ~600 (brotli), because the
linker embeds a build id that varies without changing the binary's length — so treat the
last three digits of a compressed figure as noise. Every stage in this document was
rebuilt end to end after the amendment and every raw figure was identical. The assembled
package is `gofmt` clean and passes `GOOS=js GOARCH=wasm go vet`.

**Cross-check that the method does not inflate.** Building the root package with its own
real `main()` as the sole entry point — no shim, no reachability file, nothing but the
five deleted imports — gives **38,182,269 raw / 8,765,338 gzip**. The `all` stage, driven
entirely by forced reachability, gives **38,707,992 / 8,963,156**: **1.4% apart**. Forcing
reachability reproduces the real program rather than padding it.

---

## Finding 1 — "it barely moves with handler code" is false here, and that is the whole answer

gosw's note is the load-bearing sentence in the entry's estimate, and it does not survive
contact with this codebase.

| | raw | gzip -9 | brotli -q11 | over floor (gzip) |
| --- | ---: | ---: | ---: | ---: |
| **floor** — Go runtime + `net/http` + `database/sql` + the gosw bridge | 6,043,406 | **1,703,434** | 1,263,367 | — |
| **standin** — the entry's drop list applied | 18,426,857 | **4,643,859** | 3,330,726 | **+2,940,425** |

**The handler code adds 2.80 MB gzip to a 1.62 MB floor. It is 1.7x the size of the
runtime it runs on.** Whatever was true of a four-route demo is not true of 24,780 lines
of spreadsheet.

## Finding 2 — the floor reproduces gosw, so the base was right and only the slope was wrong

| | raw | gzip -9 |
| --- | ---: | ---: |
| gosw `app.wasm` (its README, same Go version, same flags) | 6,306,280 | 1,757,649 |
| this spike's `floor` | 6,043,406 | 1,703,434 |

Within 4% raw and 3% gzip. The two differ only in handler surface — gosw carries
`encoding/json` and four routes, this carries `database/sql` and three. So the
extrapolation was not built on a bad measurement. It was built on a good measurement and a
bad derivative.

## Finding 3 — the whole repo compiles for js/wasm today, and the backward edges are not the obstacle

The dispatch expected getting sheetstream's real code to compile for `GOOS=js` to be the
hard part, because of the `web` ↔ `view` backward edges. It is not hard at all:

    GOOS=js GOARCH=wasm go build .   ->   succeeds, after deleting five blank imports

Everything else builds: `net/http`, `database/sql`, the embedded NATS server, the OTel SDK,
datastar-go, httpcompression. `modernc.org/sqlite` is the **only** thing in the module
graph that fails, and it fails identically for both wasm targets (verified again here,
libc v1.74.4):

    GOOS=js GOARCH=wasm     -> modernc.org/libc/{errno,limits,pthread,signal,stdio,
    GOOS=wasip1 GOARCH=wasm     sys/types,time,unistd}: build constraints exclude all Go files

**The backward edges do not stop the code compiling. They stop anything being left out** —
which is finding 4, and is the expensive half.

## Finding 4 — there is no subset. The "pure model" drags in all 43 files

Seed the closure with the parts that have no I/O — `grid.go`, `bandkey.go`, `formula.go`,
`pxnum.go`, `style.go` — and ask the compiler what it needs. It needs everything, in
eleven rounds:

    iter 0  +store.go +mutate.go
    iter 1  +recalc.go +storeread.go +storeseed.go +storewrite.go
    iter 2  +logging.go +otel.go +storecache.go
    iter 3  +actor.go +screen.go
    iter 4  +anchor.go +presence.go +registry.go +render.go
    iter 5  +bus.go +keys.go +rowheight.go
    ...
    iter 10 +analyze.go +headers.go            43 of 43 files

The first two rounds are the interesting ones, because they say the model is not a model:

    formula.go:484: undefined: Cell     -> declared in store.go
    formula.go:493: undefined: Kind     -> declared in store.go
    style.go:1044:  undefined: Sheet    -> declared in store.go

**`Cell`, `Kind` and `Sheet` — the three nouns the domain is made of — are declared in
`store.go`.** There is no layer below the store to extract.

## Finding 5 — one twelve-line function connects the storage model to the embedded NATS server

The chain from the store to NATS runs through exactly one reference:

    store.go:562     obsLog          -> logging.go
    storeread.go:177 tracer          -> otel.go
    otel.go:217      screen          -> screen.go      <-- the whole bridge
    screen.go:34     Conn            -> registry.go
    registry.go:146  Bus             -> bus.go         -> nats-server

`otel.go:217` is `func connAttrs(scr *screen) []attribute.KeyValue`. It is the
`obs → live` edge ARCHITECTURE.md names (4 symbols) and `SPIKE-LAYOUT.md` files under
*"genuine misplacements worth fixing"* — *"moving `screenObs` into `screen.go`... removes
two of the four backward edges for the sake of eight symbols."* Priced:

    go run ./spike/wasmsize -stage=model -cut='otel.go:connAttrs'

| model stage | files compiled | raw | gzip -9 | brotli |
| --- | ---: | ---: | ---: | ---: |
| as it stands | **43** | 11,316,093 | 3,128,903 | 2,288,821 |
| with `connAttrs` removed | **15** | 8,197,099 | **2,249,449** | 1,636,590 |

**Deleting one function drops 28 files out of the build and 879,454 bytes of gzip**, and
takes the embedded NATS server out of the compile set entirely. The cheapest tidy-up in
`SPIKE-LAYOUT.md`'s "should be decided" list is the single most valuable edit in the
codebase for this purpose, and neither document knew it.

The 15 files that remain are `sheet` almost exactly: the five seeds plus `store*.go`,
`mutate.go`, `recalc.go`, `actor.go`, `logging.go`, `otel.go`. Two files that
`ARCHITECTURE.md` assigns to `sheet` are *not* in it, and both are the other misfiling the
same document already names:

    editlog.go:140  undefined: rowRange     -> window.go     (the `sheet -> view` edge)
    rowheight.go:68 undefined: rowHeightPx  -> render.go     (the `sheet -> view` edge)

`rowHeightPx` pulls in `render.go`, which pulls in `keys.go` → `styleui.go` /
`presenceui.go` / `readonly.go` → `http.go` → `screen.go` → `bus.go` → NATS again.
**So the `sheet` domain is separable from the realtime layer, and the three edges that
prevent it are precisely the three `SPIKE-LAYOUT.md` calls cheap.**

## Finding 6 — the drop list is worth 4.12 MB gzip, and the embedded NATS server is 3.85 of it

Each third-party dependency, priced as floor + one reference to it
(`go run ./spike/wasmsize -deps`):

| probe | raw | gzip -9 | brotli | delta gzip | reached from |
| --- | ---: | ---: | ---: | ---: | --- |
| **nats-server** | 22.42 MB | 5.47 MB | 3.84 MB | **+3.85 MB** | `bus.go` |
| nats.go (client only) | 10.75 MB | 2.84 MB | 2.07 MB | +1.22 MB | `bus.go`, `registry.go` |
| otel sdk | 7.01 MB | 1.94 MB | 1.42 MB | +0.31 MB | `otel.go` |
| otel stdouttrace | 6.82 MB | 1.89 MB | 1.39 MB | +0.27 MB | `otel.go` |
| datastar-go | 5.78 MB | 1.63 MB | 1.21 MB | +0.01 MB | `push.go`, `http.go` |
| httpcompression | 5.78 MB | 1.63 MB | 1.21 MB | +0.01 MB | `main.go` |

This confirms the entry's own reasoning from a different direction. `gosw-nats` rejected
the embedded NATS server at 5.0 MB brotli; measured here against a bare floor it is
3.84 MB brotli, and it is the single largest thing in the tree. **Dropping
`bus`/`registry`/`presence` is not an optimisation, it is the only reason a stand-in is
discussable at all.**

**Read this with finding 10.** Dropping those files from a stage's *reach* set does not
drop them from the *build*: the `standin` binary contains 836 `nats-server` symbols.
Only the `local` rows below are NATS-free, and they are NATS-free because those files are
not compiled at all.

Two things worth noticing. Datastar and httpcompression are **free** — 10 KB between them.
And the NATS *client* alone is 1.22 MB gzip, so a stand-in that talked to a remote NATS
rather than embedding one would still pay most of a megabyte for the privilege.

## Finding 7 — package initialisation alone costs 1.12 MB gzip, before anything is called

The control: compile all 43 files, hold **nothing** reachable. Only package-level variable
initialisers and `init()` functions run.

| | raw | gzip -9 | brotli |
| --- | ---: | ---: | ---: |
| floor (no repo files at all) | 6,043,406 | 1,703,434 | 1,263,367 |
| all 43 files compiled, nothing reachable | 10,151,536 | **2,882,123** | 2,096,386 |

**Every stage that compiles the whole flat package pays 1,178,689 bytes of gzip — 1.12 MB
— for code it never calls**, a quarter of the whole stand-in. That is the price of *not*
splitting the package, stated as a number for the first time. It bounds what a real
`internal/` split would recover: somewhere between zero and 1.12 MB gzip, on top of
whatever finding 5's edges are worth.

## Finding 8 — the curve

All stages, all 43 files compiled, reachability restricted to the named files:

| stage | reachable files | funcs held | raw | gzip -9 | brotli -q11 |
| --- | ---: | ---: | ---: | ---: | ---: |
| `floor` — runtime + `net/http` + `database/sql` + bridge | 0 | 0 | 6,043,406 | 1,703,434 | 1,263,367 |
| `inits` — all compiled, none reachable *(control)* | 0 | 0 | 10,151,536 | 2,882,123 | 2,096,386 |
| `model` — grid, bandkey, formula, pxnum, style | 5 | 133 | 11,316,093 | 3,128,903 | 2,288,821 |
| `sheet` — ARCHITECTURE.md's `sheet` domain | 14 | 320 | 11,976,669 | 3,264,259 | 2,382,516 |
| `view` — + render, keys, formulabar, anchor, assets, window | 12 | 239 | 15,035,957 | 3,912,516 | 2,835,251 |
| `store` — + store\*, mutate, recalc, actor, editlog | 21 | 423 | 15,962,808 | 4,115,836 | 2,959,091 |
| **`standin` — the entry's drop list applied** | **32** | **574** | **18,426,857** | **4,643,859** | **3,330,726** |
| `all` — every file, NATS and otel included | 43 | 731 | 38,707,992 | 8,963,156 | 6,309,311 |

The shape: the floor is 37% of the stand-in's gzip, package initialisation is another 25%,
and the domains the stand-in exists to run are the remaining 38%. Nothing here is a
rounding error that better flags would remove.

**These rows are not NATS-free.** All eight compile the whole flat package; finding 10
shows what that means at link level. The NATS-free measurements are in finding 11.

`view` (3.73) sits below `store` (3.93) and below `sheet`+`view` because the reach sets are
different domains, not nested — read the rows as *"hold this domain reachable"*, not as a
cumulative sum.

## Finding 9 — the verdict on the gate

| | gzip | brotli |
| --- | ---: | ---: |
| stand-in `app.wasm` | 4,643,859 | 3,330,726 |
| `sqlite3.wasm` (measured, `../hypermedia-sw-demo/vendor/`, 864,752 raw) | 399,968 | — |
| `wasm_exec.js` | 16,992 | — |
| **total first download of a copy** | **5,060,819 ≈ 4.83 MB** | **≈ 3.6 MB** |

Against the entry's numbers:

| | entry says | measured | |
| --- | ---: | ---: | --- |
| stand-in, gzip | ~1.7 MB | **4.83 MB** | **2.9x the estimate** |
| multiple of the 27.9 KB first visit | ~60x | **177x** | |

**The ~1.7 MB estimate does not hold.** But note carefully what that does and does not
decide. The entry's *conclusion* — that this is survivable only as an explicit opt-in, and
must never be on the first-paint path — is unchanged and if anything strengthened. What
changes is the framing of the opt-in. 1.7 MB is a podcast episode. **4.8 MB gzip is a
download with a progress bar**, and the fork model already gives it one: a copy is taken
while online, deliberately, with a person waiting. The honest version of the button is
*"take a copy offline (4.8 MB)"*, and brotli at 3.6 MB is worth having for exactly this
reason.

Two levers exist if that is too much, both measured above and neither speculative:
finding 5's one function (−879,454 bytes of gzip observed on the model stage) and
finding 7's package split (≤ 1,178,689 bytes). A stand-in in the low 3 MBs is reachable.
A stand-in at 1.7 MB is not, without TinyGo — which does not support all of `net/http`,
which is the entire premise.

---

## The store seam

### S1 — the seam is real, and the entry undercounts it by four files

Verified, non-test code:

    sql.Open("sqlite", dsn)                       store.go:573       exactly once
    _ "modernc.org/sqlite"                        FIVE files         store.go, storeread.go,
                                                                     storewrite.go, storecache.go,
                                                                     storeseed.go
    func (s *Sheet) use(fn func(db *sql.DB) error) store.go:523
    func inTx(db *sql.DB, fn func(tx *sql.Tx) error) store.go:638
    type queryer interface{ Query; QueryRow }     bandkey.go:540     shared by *sql.DB and *sql.Tx

The claim holds where it matters: **one `sql.Open`, and no query anywhere reaches below
`database/sql`.** Nothing imports a sqlite-specific type, registers a custom function, or
touches a `driver.Conn`. Swapping the driver really does swap the store without editing a
query. The blank import in five files is a side effect of the `SPIKE-LAYOUT.md` split
copying the import block; it is a `sed`, not a design problem, but the entry says "one
line" and it is five.

### S2 — what a driver would actually have to implement

`spike/wasmsize/shim/sqlstub.go` is the enumeration, written by working out what
`database/sql` demands of a driver given how this codebase calls it. The measured call
surface, non-test:

    tx.Exec x69   rows.Close x49   rows.Scan x41   rows.Next x41   rows.Err x33
    db.Exec x30   db.QueryRow x21  db.Query x21    tx.Rollback x15 db.Begin x14
    tx.QueryRow x12  tx.Query x12  tx.Commit x7    tx.Prepare x5   db.Prepare x1

**There is not one `*Context` call in the repository.** Every access is the plain form.
That does not excuse a driver from the context interfaces — `database/sql` prefers them
when a driver offers them — but it does mean **nothing in this codebase can cancel a
query**, which matters more in a single-threaded wasm engine than it does on a server.

Required:

| interface | why this codebase reaches it |
| --- | --- |
| `driver.Driver`, `driver.DriverContext` + `Connector` | `sql.Open` with a DSN carrying four pragmas the driver must parse itself |
| `driver.Conn`, `ConnPrepareContext` | `db.Prepare` for the two cached dependents statements, `tx.Prepare` x5 |
| `driver.ConnBeginTx` | `db.Begin` x14; every write is `Begin / defer Rollback / Commit` |
| `driver.ExecerContext`, `QueryerContext` | optional, but without them `database/sql` prepares and closes a statement around each of ~150 one-shot calls — three engine round trips per keystroke instead of one |
| `driver.Stmt` + `StmtExecContext` / `StmtQueryContext` | as above |
| `driver.Tx` | `Rollback` after a successful `Commit` must be a no-op: return `driver.ErrTxDone` |
| `driver.Rows`, `driver.Result` | 41 read sites; `LastInsertId` for `events.seq` |
| `driver.Pinger`, `SessionResetter`, `Validator` | pool hygiene at `SetMaxOpenConns(8)` |

`driver.Value` types needed: **INTEGER, TEXT, BLOB, NULL only.** Scan targets across the
store are `int64`, `string`, `[]byte` and `sql.NullInt64` — no REAL, no `time.Time`.

**The trap is `NumInput`.** The hottest query in the system, `dependentsSQL`
(`storeread.go:312`), is a three-arm `UNION ALL` in which `?1` and `?2` each appear
**six** times but bind **two** parameters. A driver that reports the number of `?`
occurrences rather than `sqlite3_bind_parameter_count()`'s highest ordinal will make
`database/sql` refuse every recalculation with *"expected 6 arguments, got 2"*. Returning
`-1` hides the bug; returning the wrong positive number is worse than either.

### S3 — the SQL features are all fine, because sqlite-wasm is real SQLite

Nothing in the schema or the mutation paths is exotic for SQLite, only for a
reimplementation:

- **`WITHOUT ROWID`** on `cells`, `cols`, `bands`, `styles`, `rows`, and on `temp.kmap`.
- **Six partial indexes** (`indexDDL`), whose predicates are repeated verbatim in every
  query because SQLite only uses a partial index when the `WHERE` clause visibly implies it.
- **`INDEXED BY`** in `dependentsSQL`, to force the span arm onto a chosen index.
- **`CREATE TEMP TABLE ... AS SELECT`** and `temp.mut_cells` / `temp.kmap` / `temp.rebal`
  in `mutate.go`, which exist because `SET k = k + 1` collides with itself in a
  `WITHOUT ROWID` primary key.
- **`PRAGMA user_version`**, read and written; `AUTOINCREMENT` on `events.seq`;
  `CHECK` constraints throughout.

All of these are ordinary SQLite and work in the official wasm build. **This is the half
of the seam that is genuinely free**, and it is free because the codebase leaned on SQLite
rather than around it.

### S4 — WAL is neither available nor needed, and that deletes `SPIKE-FORK-MERGE` finding 8

`openSheetFile`'s DSN opens every sheet `_pragma=journal_mode(WAL)`, and
`SPIKE-FORK-MERGE.md` finding 8 is entirely about the consequence: after one edit the
`.db` was 4,096 bytes and the `-wal` 358,472, so a bare `.db` copy **silently produced a
near-empty sheet**, and `VACUUM INTO` is the recommended fork.

In the browser storage model `sw-store-spike` landed on, none of that exists. That design
is **in-memory sqlite-wasm → an IndexedDB WAL of session-extension changesets → a two-slot
OPFS snapshot**. The database is in memory; there is no second file to forget; SQLite's
own journal mode plays no part, and durability is supplied *above* SQLite by a WAL that is
not SQLite's. So:

- **`journal_mode(WAL)` should be dropped from the DSN in a wasm build, not emulated.**
  It is meaningless against an in-memory database, and a driver that silently accepts it
  is exactly the kind of quiet lie this codebase treats as a bug.
- `busy_timeout(5000)` is equally meaningless: nothing else can hold the write lock.
- **Forking is `sqlite3_js_db_export()`, not `VACUUM INTO`** — and finding 8's whole
  correctness argument (that `VACUUM INTO` does not depend on the writer being idle)
  evaporates, because the actor already guarantees it and there is no second process.

**One consequence nobody has costed.** If the IndexedDB WAL stores session-extension
changesets, then a WAL record is proportional to the *rows a commit rewrote*, not to the
edit. `SPIKE-FORK-MERGE` finding 1 measured `InsertRows(3,1)` on a 250-row sheet at
**1,035 cells rewritten**, and a full rebalance at **58,300**. `sw-store-spike` measured
0.217 ms per commit on todo-shaped rows. A row insert here is three orders of magnitude
more changeset than a todo, and a rebalance four. That is the first thing to measure
before believing this storage model transfers.

### S5 — the actor makes the driver's job easier and the reader's job worse

`Actors.exec` is the only place a sheet is mutated, so a driver never sees two concurrent
writers on one database and needs no internal write lock. But the property
`ARCHITECTURE.md` states next to it — *"readers do not go through it: WAL readers run
concurrently with the writer, which is why a push never waits on an edit"* — **is a
property of WAL, and S4 just removed WAL.** Against one in-memory SQLite behind one wasm
module, `SetMaxOpenConns(8)` is eight `database/sql` handles onto one serialized engine,
and a push *does* wait on an edit.

That is not a size problem, it is the entry's constraint 2 arriving early: the cost will
be render fan-out, and `kanban-envelope-spike` already caught someone misdiagnosing
exactly this. It is unmeasured here and it is the next thing to measure.

### S6 — the seam is not only a driver. The store is filesystem-shaped

This is the part the entry does not mention, and it is the reason "swap the driver and
you are done" is optimistic. The sheet lifecycle is built on the filesystem, not on the
database handle:

    store.go        os.MkdirAll(dir)          create the sheet directory
    store.go        os.Remove(tombPath(...))  resurrect a tombstoned id
    storecache.go   os.Stat x2, os.ReadDir, os.WriteFile     the cache, tombstones
    storeseed.go    os.Stat x2, os.ReadDir                   disk usage, sheet enumeration
    store.go        path := filepath.Join(dir, id+".db")     a sheet IS a file path

Eleven call sites. They **compile** for `GOOS=js` — that is why every number above exists
— and they will not **run**: `$(go env GOROOT)/lib/wasm/wasm_exec.js` answers **23 of its
filesystem entry points with `ENOSYS`**, and the only one that does anything is `write` to
fd 1 and 2. `os.MkdirAll` fails on the first sheet. A stand-in needs either a JS
filesystem shim (`gosw-nats/web/gofs.js`, ~480 lines, which the archive README explicitly
lists as the reusable part of a rejected spike) or a second seam that makes "a sheet" an
opaque handle instead of a path. **The second is the better answer and it is not one line.**

---

## What was not tested

- **Anything in a browser.** Deliberate, and the dispatch said so: no service worker
  registration, no Chrome, no end-to-end. `gosw` proved that half. Nothing here has been
  observed to *run*; every claim is a compile-time or link-time fact.
- **SQLite.** No SQL executes in any of these binaries. Every size excludes the engine;
  `sqlite3.wasm` is measured separately and added arithmetically, which assumes it is not
  further compressible alongside the Go binary and that no glue code is needed. Both
  assumptions are optimistic.
- **Whether the stand-in is correct.** The stub driver returns errors. Nothing has ever
  opened a sheet, rendered a cell, or recalculated anything in wasm.
- **Runtime memory.** `gosw-nats` was rejected as much for 46–100 MB of non-reclaimable
  wasm memory as for its size, and a 17.57 MB module's linear memory has not been looked
  at here. A service worker cold start pays instantiation on every wake.
- **TinyGo.** Not attempted. It is the only route to a materially smaller number and it
  does not support all of `net/http`, which is the premise of the whole entry.
- **`wasip1`.** Only `js/wasm` was built. `wasip1` fails identically on
  `modernc.org/sqlite` and has no service-worker story.
- **Whether a real package split recovers finding 7's 1.13 MB.** The bound is measured;
  the recovery is not, because performing the split is not a spike.
- **Streaming under load.** gosw measured SSE arriving incrementally at +503…+2518 ms with
  five events. This project's push path is `windowcache` → `render` → brotli → SSE for
  every viewer, and none of it has been run in a worker.
- **Compression of the shipped artefact.** brotli here is `andybalholm/brotli` at quality
  11 in-process. `kanban-bootstrap-quick` found **q2, not q5**, was the right setting for
  a bootstrap payload; nobody has checked what q2 does to a 17 MB wasm module, and for a
  one-time opt-in download the tradeoff is the opposite of theirs anyway.

---

# Amendment — can NATS and otel go entirely, and what is the smallest useful thing?

Four questions after the first pass, and **the first one refutes a claim above.**

New evidence: `-ldflags=-dumpdep`, which makes the linker print the symbol graph it
actually kept. Calibrated against the `floor` build, which contains **zero** `nats` and
**zero** `opentelemetry` symbols — so a non-zero count is a fact about the binary, not an
artefact of the tool. New stages: `-stage=local`, `local-ro`, `local-inits`, `local-nosdk`.

## Finding 10 — NATS is in the `standin` binary. Finding 6 read as if it were not; it is not

Dropping `bus`/`registry`/`presence` from a stage's *reach* set does not drop them from the
*build*, and the linker disagreed with the reachability analysis:

| build | distinct `nats-server` symbols linked | otel SDK | stdouttrace |
| --- | ---: | :---: | :---: |
| `floor` (calibration) | **0** | no | no |
| **`standin`** | **836** | **yes** | **yes** |
| `local` | **0** | yes | yes |
| `local-nosdk` | **0** | **no** | **no** |

836 distinct symbols with real bodies — `(*Account).addServiceImportWithClaim`,
`(*ApiError).Error`, and so on. And the edges say it is not merely package
initialisation:

    main.(*Bus).SubscribePresence -> nats%2ego.(*Conn).subscribe
    main.(*Bus).PublishPresence   -> nats%2ego.(*Conn).publish
    main.(*Registry).register     -> nats%2ego.(*Subscription).Unsubscribe

**The entry's drop list is not internally consistent.** It drops `bus`/`registry`/
`presence` but keeps `screen.go` and `push.go` — and those *call* the bus, because
`push.go` **is** the fan-out. You cannot keep the SSE push path and drop the fan-out. So
the honest choice is not "which files do I stop calling" but "which files do I stop
compiling", and that is a different and much more restrictive question.

**Nothing above 4.43 MB should be quoted as NATS-free.** Only the `local` rows are.

## Finding 11 — the smallest thing that can own a forked sheet is 2.29 MB gzip

`local` is the sheet domain whole — model, store, actor, `mutate`, `recalc`, `editlog`,
`rowheight` — the gosw bridge, one viewer, **no view layer, no `http.go`, no `push.go`,
no `bus.go`**. To get it to compile, the harness performs the three relocations
`SPIKE-LAYOUT.md` lists under *"what was not done, and should be decided"*, on the copies:
`otel.go:connAttrs`, `render.go:rowHeightPx`, `window.go:rowRange`. The two that are
re-supplied are **eleven lines** between them (`spike/wasmsize/graft/relocated.go.txt`).

| build | files | raw | gzip -9 | brotli -q11 |
| --- | ---: | ---: | ---: | ---: |
| `floor` | 0 | 6,043,406 | 1,703,434 | 1,263,367 |
| `local-inits` — the 17 compiled, none reachable | 17 | 7,125,972 | 1,974,345 | 1,454,041 |
| **`local-nosdk`** — `local` without the otel SDK | 17 | 8,347,901 | **2,291,911** | **1,658,786** |
| `local-ro` — read path only | 17 | 8,761,920 | 2,391,579 | 1,731,166 |
| `local` | 17 | 9,148,449 | 2,491,963 | 1,798,428 |
| `standin` (for contrast) | 43 | 18,426,857 | 4,643,857 | 3,330,348 |

It decomposes exactly: **1,703,434 floor + 270,911 package init + 317,566 domain =
2,291,911.**

All in — plus `sqlite3.wasm` (399,968 gzip) and `wasm_exec.js` (16,992) — a local
stand-in is **2,708,871 bytes ≈ 2.58 MB gzip**, or **2,023,040 ≈ 1.93 MB brotli**
(`sqlite3.wasm` compresses to 347,262 brotli, measured). Against the entry's
~1.7 MB that is **1.5x, not 2.9x**, and **95x the 27.9 KB first visit rather than 177x**.

**So the entry's estimate is roughly right for a deliberately smaller system and badly
wrong for the one it describes** — which is the Monzo shape the entry is built on,
arriving from the direction nobody expected.

**What this number does not buy: the page.** `render.go` is not in it. The promise of "the
same HTML, no client rewrite" is exactly what the 2.15 MB between `local` and `standin`
pays for, and it cannot be had at this price. A local stand-in owns the *model* and would
need a fresh, small renderer — new code, unmeasured here, and the honest cost of the
smaller ambition.

## Finding 12 — the 879,454 does NOT compose with the stand-in figure, and the real saving is bigger

Asked directly: no. That number was measured against the `model` closure; the `standin`
row is a different construction, and adding them would be wrong. Measured end to end
instead:

    standin  4,643,857 gzip  ->  local-nosdk  2,291,911 gzip
    saving   2,351,946 bytes  (51%)

The three relocations are what make the saving *available*; the saving itself is dropping
26 files from the build.

## Finding 13 — 77% of the package-`init` cost is recoverable, and that changes the recommendation

Finding 7 measured the bound at 1,178,689 bytes of gzip and did not establish recovery.
Recovery, measured the same way for the 17-file local set:

| compiled | files | init cost over floor (gzip) |
| --- | ---: | ---: |
| the whole flat package | 43 | 1,178,689 |
| the local set | 17 | **270,911** |
| **recovered** | | **907,778 — 77%** |

**What kind of approximation this is.** It measures *not compiling* 26 files, which is
what a package split makes possible — not the split itself. A split that still had the
stand-in importing everything would recover nothing. The number is therefore an upper
bound on what splitting buys, and an exact figure for what *omission* buys.

The more interesting half: **omission needs no package split at all today.** Three
declarations moved to the side `SPIKE-LAYOUT.md` already says they belong on gets the
whole way there. So the conclusion is not "this is too big" — it is **"this is too big
until three eleven-line moves ARCHITECTURE.md already calls cheap"**, and after them the
gate is roughly where the entry thought it was.

## Finding 14 — the otel SDK is removable for one function; the otel API is not removable at all

`otel.go:55` is `var tracer trace.Tracer = noop.NewTracerProvider().Tracer("sheetstream")`,
and `StartTracing` is the **only** function in the repository that touches
`sdktrace`, `stdouttrace` or `resource`. Cutting it leaves six imports unused — the
compiler says so, which is itself the proof:

    ./otel.go: "fmt", "os", "go.opentelemetry.io/otel",
               ".../stdouttrace", ".../sdk/resource", ".../sdk/trace" imported and not used

Cut it and prune those six lines and the SDK is gone at link level: **0 `otel/sdk`, 0
`stdouttrace`**, worth **200,052 bytes gzip**. That is one function and six import lines —
but it *is* a source edit, so: **the otel SDK cannot be removed without touching
`otel.go`.**

**The otel API cannot be removed at any price.** `tracer.Start(ctx, …)` appears at **30
call sites across 9 files** (`http.go` 15, `push.go` 5, `rangeops.go` 2, `registry.go` 2,
`storeread.go` 2, `storewrite.go` 2, `structure.go`, `styleui.go`, `presenceui.go`), and
`context.Context` is threaded through the store for it. Inside the local set alone that is
four call sites in `storeread.go` and `storewrite.go`. 708 otel API symbols survive in the
smallest build measured. **That is a floor, and it is a finding about the codebase rather
than about wasm**: tracing here is not a layer that can be lifted off, it is part of the
call signature. The noop provider makes it cheap at runtime; it does not make it absent.

## Finding 15 — "read-only" is not a smaller system here, because opening a sheet writes to it

The suggestion was that read-only rendering with no write path might be much smaller. It
is 4% smaller: 2,391,579 gzip against 2,491,963. The linker says why — with only the read
path anchored, it still keeps `migrate`, `bulkInsert`, `scratch`, `rebalance`,
`commitIndex`, `commitStyles`, `bumpVersion` and `ensureRows`.

`openSheetFile` runs `schemaDDL`, then `migrate(db)`, then `indexDDL`, and the band index
is committed on open. **A sheet that has been opened has been written to.** There is no
read-only mode to have; there is only a store with fewer entry points. Dropping *formula
recalculation* was not measured for the same reason — `recalc` is called from `mutate`, so
removing it from the reach set removes nothing.

## Where this leaves the gate

Given that full offline is now Wails — the real binary against real `modernc.org/sqlite`,
no wasm — and the browser path is the compromise:

| ambition | gzip all in | vs 27.9 KB | what it gives up |
| --- | ---: | ---: | --- |
| the stand-in as the entry describes it | 5.06 MB | 177x | nothing — but it still contains NATS |
| **the model, locally, with a fresh renderer** | **2.71 MB** | **95x** | `render.go`, so "the same HTML" |
| Wails | n/a | n/a | the browser |

**The recommendation this supports:** do not build the entry's stand-in. Build the smaller
one, or nothing. And do the three relocations regardless — they cost eleven lines, they
are already recommended on readability grounds, and they are what makes any of this
possible.

## What this amendment did not test

- **The fresh renderer.** The `local` numbers contain no HTML at all. Whatever replaces
  `render.go` is new code and is not in any figure here.
- **Whether `local` runs.** Same as before: nothing has been executed. `local` still
  carries the eleven `os.*` call sites of S6 and the stub driver of S2.
- **A real package split.** Finding 13 measures omission, not `internal/`.
- **Wails.** Out of scope, and the numbers here say nothing about it — a native binary
  pays none of this.
