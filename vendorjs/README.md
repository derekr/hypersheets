# vendorjs

Third-party JavaScript, committed rather than fetched.

## datastar-1-0-2-4efd7a457ef99e73.js

A **custom Datastar Pro v1.0.2 build** containing only the eleven plugins this
application uses. 30,260 bytes, against 34,137 for the stock v1.0.1 bundle.

    sha256  ee67e070d3a168cdca0920bd25adad953074f4420a4196892f854bc01bfa405b

The `.json` beside it is the builder's manifest. **It is committed on purpose and
it is not documentation** — `TestBundleManifestMatchesWhatThePageUses` reads it
and compares the plugin list against a rendered page, in both directions: a
plugin the page needs and the bundle lacks is an error, and so is a plugin the
bundle carries that nothing uses.

The file is named for its exact build, so swapping it means editing the `go:embed`
line in `assets.go` by hand. That friction is deliberate; see below.

## The plugin selection

| group | in the bundle | left out |
|---|---|---|
| actions | `FETCH` | `PEEK` `SETALL` `TOGGLEALL` |
| attributes | `BIND` `EFFECT` `INIT` `ON` `SHOW` `SIGNALS` `STYLE` `TEXT` | `ATTR` `CLASS` `COMPUTED` `INDICATOR` `JSONSIGNALS` `ONINTERSECT` `ONINTERVAL` `ONSIGNALPATCH` `REF` |
| watchers | `PATCHELEMENTS` `PATCHSIGNALS` | — |
| pro | — | all thirteen |

`ON` earns its place 57 times over. `SIGNALS` appears exactly once, on `<body>`,
and is not optional for that.

## Why the watchers are the dangerous two

**An omitted Datastar plugin is inert, not loud.** It does not throw, warn, or
fail to parse — the attribute simply does nothing.

The first custom build tried here omitted `PATCHELEMENTS` and `PATCHSIGNALS`.
They are the only two plugins with no `data-*` spelling — the server drives both —
so any process that derives the plugin list by reading the markup misses them,
including a careful one. The result loaded, booted Datastar, accepted keystrokes,
posted commands, received `204`s, and **silently discarded every SSE frame the
server sent**. It was indistinguishable from a working spreadsheet until a second
browser failed to see an edit.

Measured, same page, same server, only the bundle swapped:

| bundle | `C7` after an external `POST /cell` |
|---|---|
| stock v1.0.1 | `PATCHED` |
| custom, 9 plugins | element absent |
| custom, 11 plugins | `PATCHED` |

`PATCHELEMENTS` is the fat morph the whole design rests on. `PATCHSIGNALS`
carries every suppressed-digest update, the row extent, and the 15-second `_hb`
keepalive.

## Why vendored at all

A CDN is a third party on the critical path of a demo whose entire claim is about
latency and control. Vendoring removes a DNS lookup, a TCP connection and a TLS
handshake to a second origin, removes an availability dependency, and stops the
reader's browser telling anyone else which page they opened.

The honest cost: jsdelivr served from an edge near the reader while this origin is
a single box, so on a COLD cache a distant reader may fetch these bytes
more slowly than before. Every warm load and every navigation is faster, and the
bytes are `immutable` under a content-addressed URL, so "cold" means once.

## If you rebuild

1. Drop the new `.js` and `.json` in here.
2. Update the `go:embed` line in `assets.go` and `datastarBuild` in `render.go`.
3. Update `datastarBuildSHA256` in `assets_test.go` and the checksum above.
4. Run the tests. The manifest check will tell you if the selection is wrong
   before a browser ever does.

**Do not strip the licence header.** The first line of the bundle reads
`// Datastar Pro v1.0.2 licensed to GitHub user @derekr`, and it is meant to stay
in the output — it is part of the artifact, not an accident of the builder. It
does mean the licence identity travels with the repository, which is worth
knowing before publishing, but the answer to that is a decision about the
repository, never an edit to this file.

Editing it would also break `TestVendoredDatastarIsTheBuildWeThinkItIs`, which is
the correct outcome: the checksum exists so that the committed bytes are provably
the bytes the builder produced.
