# Spike — organising this for a reader

**Question.** 24,780 lines in one flat `package main`. What arrangement would let
somebody new find the domains and follow how the system works?

**Answer.** Not packages. The obstacle to understanding this codebase is not that
the files are undivided — it is that five of them are enormous. Split those along
seams they already carry, add a map, and fix the two files whose names lie.

Evidence: `spike/layout`, which type-checks the package and reports the real
reference graph. Run `go run ./spike/layout -grouping refined -sections 900`.

## What the tool does

It answers one question that is normally settled by taste: **if these files were
grouped into domains, would the groups form a layering or a knot?** Every
reference from one file to another is an edge the compiler already knows about,
so the tool resolves each identifier to the object it denotes, collapses the file
graph onto a proposed grouping, and reports the strongly connected components.

Type resolution rather than name matching, and the difference is not academic:
`sheetID` is a flag in `main.go` and a local in a dozen handlers, and a
name-matching first attempt reported every one of those as a dependency on boot —
inventing 40-odd edges and one enormous fake knot. With real resolution the rule
becomes simple enough to trust: *a use of an object declared in another file of
the same package is an edge, and a local can never be one.*

## Finding 1 — two files are misfiled, and they were most of the knot

The first grouping put `structure.go` and `rangeops.go` in the model, because
their doc comments say "inserting and deleting rows and columns" and "the
operations that take the selection as an argument". Their *contents* are HTTP
handlers — they reach for `Server`, `respondCommand`, `authorName`,
`commandStatus`.

Between them they were **the entire `sheet → live` edge** and most of
`sheet → web`. Two files, filed by their subject rather than by their layer,
produced a cycle spanning all five domains.

That is the cheapest possible fix and the most valuable: a file whose name
misleads costs a reader more than a file that is merely long.

## Finding 2 — the codebase is feature-sliced, and that is why it cannot be packaged

With those two moved and rendering separated from routing, the forward edges are
a clean descent — `web → live → view → sheet` — and only four run backwards:

| backward edge | symbols | what it is |
| --- | ---: | --- |
| `view → web` | 23 | the page shell asks each feature slice for its fragment |
| `live → web` | 15 | `push.go` reaches the server for latency and patch helpers |
| `sheet → view` | 4 | `rowHeightPx` and the window range types, on the wrong side |
| `obs → live` | 4 | `screenObs` knows what a `screen` is |

The last two are genuine misplacements worth fixing. The first two are not
mistakes — they are the design.

**`growrows.go` holds the CSS, the HTML, the client script, the signals, the
policy and the HTTP handler for "Add more rows at bottom", in 161 lines.** So do
`latency.go`, `styleui.go`, `presenceui.go`, `anchor.go` for their features. Each
is a vertical you can read start to finish and then close, which is precisely the
property a newcomer needs.

It is also precisely what makes them unpackageable: the page shell calls into
each slice for its fragment, and each slice calls back for its handler, so
`view → web` and `web → view` are both real and both correct. Layered packages
would mean cutting every feature into three pieces in three directories —
trading the thing that makes them readable for a directory tree.

**Conclusion: slices are the unit. The layering is a reading aid, not a
boundary.** And the tool is how the reading aid stays true, since it can be
re-run.

## Finding 3 — length is the actual barrier

| file | lines | sections it already carries |
| --- | ---: | ---: |
| `store.go` | 2,931 | 10 |
| `render.go` | 2,408 | 10 |
| `http.go` | 2,220 | 10 |
| `mutate.go` | 2,142 | 9 |
| `style.go` | 1,908 | 6 |

**11,609 lines — 47% of the codebase — in five files.** Meanwhile `growrows.go`
is 161 lines and completely graspable. The difference between a file you read and
a file you search is somewhere in between, and these five are well past it.

They already carry the seams. Every one of them is divided by `─── section ───`
banners the authors drew, and splitting along those lines yields pieces averaging
about 130 lines. Nothing moves between packages, no symbol changes name, no
import graph changes — within one package this is pure re-filing, and the
compiler proves it.

The worst single case: `http.go`'s *GET /s/{id}/live* section is **1,090 lines**,
larger than all but four entire files in the repo.

## What was done

- `spike/layout` — the analyser, checked in so the map can be regenerated rather
  than maintained.
- `ARCHITECTURE.md` — the map: the path of one edit through the files, the
  measured domain graph, a reading order, and the five ideas the rest follows
  from.
- `store.go` split along its own banners, as proof the operation is mechanical.

## What was not done, and should be decided

- **The remaining four splits** (`render.go`, `http.go`, `mutate.go`,
  `style.go`). Same operation, ~9,000 more lines of pure re-filing. Large diff,
  zero risk, big readability win.
- **Renaming `structure.go` and `rangeops.go`** so their names say "command
  handler". Cheap, and removes the misdirection that fooled the first grouping.
- **Moving `rowHeightPx` to `grid.go`** and `screenObs` into `screen.go`, which
  removes two of the four backward edges for the sake of eight symbols.
- **Enforcing the layering.** If the boundary is ever wanted for real, the tool
  can be made a test: assert the grouping is acyclic and fail the build when a
  new edge appears. That is a decision about how much ceremony a demo wants, not
  a technical obstacle.
