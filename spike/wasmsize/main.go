// Command wasmsize answers the gate question on the "Offline is a COPY of the
// sheet" backlog entry: if this Go server were relocated into a browser service
// worker — the same handler, the same HTML, compiled GOOS=js GOARCH=wasm — how
// many bytes would a visitor download?
//
// The entry's estimate is ~1.7 MB gzip, extrapolated from ../sw-datastar-sync-archive/gosw,
// which measured 6.01 MB raw / 1.68 MB gzip for a bare net/http handler and
// observed that the size "barely moves with handler code". This program tests
// that observation against this codebase rather than a toy.
//
// Findings are in SPIKE-WASM-STANDIN.md.
//
// ─── How it measures, and why it is built this way ────────────────────────────
//
// A program under spike/ cannot import the repo root's `package main`
// (SPIKE-FORK-MERGE.md says so, and it is still true), so this command does not
// link the code it is measuring. It *assembles* it: for each stage it copies a
// named subset of the root's .go files into a scratch module alongside the
// shim/ files, and builds that for js/wasm.
//
// Two mechanical steps make the numbers mean something:
//
//   - Closure. A subset of a flat package rarely compiles. The build errors name
//     the missing symbols, an index of the root's declarations says which file
//     declares each, and those files are added and the build retried. So the
//     COMPILE set is a seed plus whatever the compiler forced, and the report
//     prints both. What a seed drags in is itself a finding — in this codebase
//     every seed drags in all 43 files, which is finding 4.
//
//   - Forced reachability, restricted to the seeds. The Go linker discards
//     unreachable code, so compiling a file adds nothing on its own. zz_reach.go
//     is generated per stage and takes the address of every non-generic
//     top-level function and method declared in the stage's SEED files only.
//     Everything the closure dragged in is compiled but not anchored, so dead
//     code elimination decides how much of it this domain really reaches. That
//     is a crisp, judgement-free denominator: everything the seeds declare, plus
//     everything they transitively call.
//
//     Cross-check that this does not inflate: the `all` stage, driven entirely
//     by forced reachability, lands within 1.4% of building the root package
//     with its own real main() as the only entry point.
//
// Usage:
//
//	go run ./spike/wasmsize                 # every stage
//	go run ./spike/wasmsize -stage=floor,model
//	go run ./spike/wasmsize -keep /tmp/wz   # keep the assembled trees
package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/andybalholm/brotli"
)

var (
	only     = flag.String("stage", "", "comma-separated stages to run; empty runs all. names: "+stageNames())
	keepDir  = flag.String("keep", "", "assemble into this directory instead of a temp dir, and keep it")
	verbose  = flag.Bool("v", false, "print the closure expansion and every build error")
	why      = flag.Bool("why", false, "for every file the compiler pulls in, print the reference that forced it")
	deps     = flag.Bool("deps", false, "instead of stages, price each third-party dependency against the floor")
	cut      = flag.String("cut", "", "delete these top-level declarations from the copies, as file.go:Name[,file.go:Name...]; use it to price one reference (see -why)")
	maxIters = flag.Int("max-iter", 40, "give up on a stage's closure after this many build attempts")
)

// ─── The stages ───────────────────────────────────────────────────────────────

type stage struct {
	name    string
	what    string
	seeds   []string // root files; nil means "shim only"; []string{"*"} means every root file
	compile []string // extra files to compile but NOT hold reachable; "*" means all
	cuts    []string // declarations to delete from the copies, as file.go:Name
	graft   string   // a file under spike/wasmsize/graft/, copied only for this stage
	provide []string // names the graft supplies, so the closure stops looking for them
}

// The four stages the spike was dispatched with, plus `all` as the upper bound.
// Seeds follow the domain grouping spike/layout reports and ARCHITECTURE.md
// prints — `refined` there — so "the model" and "the view layer" mean the same
// files in both documents.
var stages = []stage{
	{name: "floor", what: "Go runtime + net/http + database/sql + the gosw service-worker bridge. No hypersheets code."},
	{name: "model", what: "+ the pure model: grid, bandkey, formula, pxnum, style. The parts with no I/O.", seeds: modelFiles},
	{name: "view", what: "+ render and the view layer: render, keys, formulabar, anchor, assets, window, rowheight.",
		seeds: append(modelFiles, "render.go", "keys.go", "formulabar.go", "anchor.go", "assets.go", "window.go", "rowheight.go")},
	{name: "store", what: "+ the store and the actor, against the stub driver: store*, mutate, recalc, actor, editlog.",
		seeds: append(modelFiles, "render.go", "keys.go", "formulabar.go", "anchor.go", "assets.go", "window.go", "rowheight.go",
			"store.go", "storeread.go", "storewrite.go", "storecache.go", "storeseed.go",
			"mutate.go", "recalc.go", "actor.go", "editlog.go")},
	// ARCHITECTURE.md's `sheet` domain exactly, as spike/layout's `refined`
	// grouping defines it, plus the four files the store.go split produced.
	{name: "sheet", what: "ARCHITECTURE.md's `sheet` domain: the model, the store, recalc, the actor, the edit log.", seeds: []string{
		"grid.go", "bandkey.go", "formula.go", "style.go", "rowheight.go", "editlog.go", "actor.go",
		"store.go", "storeread.go", "storewrite.go", "storecache.go", "storeseed.go",
		"mutate.go", "recalc.go",
	}},
	// The stand-in the backlog entry describes: everything still reachable
	// except its drop list. Routing, rendering, the store and the SSE push all
	// stay. NOTE finding 10: dropping those files from the REACH set does not
	// drop them from the BUILD, and NATS is in this binary.
	{name: "standin", what: "the drop list from the backlog entry applied: no bus/registry/presence, no limits/reaper/readonly/headers, no index or boot.",
		seeds: standinFiles},
	{name: "all", what: "every non-test file in the repo root, including bus/registry/presence (embedded NATS) and otel.", seeds: []string{"*"}},
	// The control. Every file compiled, nothing held reachable: what the
	// package-level var initialisers and init() functions cost on their own.
	{name: "inits", what: "all 43 files compiled, nothing held reachable — the cost of package initialisation alone.", compile: []string{"*"}},

	// ─── The minimal local stand-in ───────────────────────────────────────────
	//
	// The three edges SPIKE-LAYOUT.md already calls cheap, actually cut, so the
	// sheet domain compiles without the realtime layer. `bus.go` is then not in
	// the build at all, so nats-server is not in the module graph — which is the
	// difference between "unreachable" and "absent" that finding 10 turns on.
	{name: "local", what: "the smallest thing that can own a forked sheet: sheet + store + actor + recalc, the gosw bridge, no view layer, no NATS in the build.",
		seeds: localFiles, cuts: relocations, graft: "relocated.go.txt", provide: relocated},
	{name: "local-ro", what: "local, read path only: no mutate, no recalc, no actor, no seed, no write.",
		seeds: localReadFiles, compile: localFiles, cuts: relocations, graft: "relocated.go.txt", provide: relocated},
	{name: "local-inits", what: "the local file set compiled, nothing reachable — how much of the init cost the split recovers.",
		compile: localFiles, cuts: relocations, graft: "relocated.go.txt", provide: relocated},
	{name: "local-nosdk", what: "local with StartTracing cut: does dropping the one call site take the otel SDK out?",
		seeds: localFiles, cuts: append(relocations, "otel.go:StartTracing"), graft: "relocated.go.txt", provide: relocated},
}

var modelFiles = []string{"grid.go", "bandkey.go", "formula.go", "pxnum.go", "style.go"}

var standinFiles = []string{
	"grid.go", "bandkey.go", "formula.go", "pxnum.go", "style.go", "rowheight.go",
	"editlog.go", "actor.go", "mutate.go", "recalc.go",
	"store.go", "storeread.go", "storewrite.go", "storecache.go", "storeseed.go",
	"render.go", "keys.go", "formulabar.go", "anchor.go", "assets.go", "window.go",
	"screen.go", "push.go", "windowcache.go",
	"http.go", "structure.go", "rangeops.go", "styleui.go", "growrows.go", "latency.go",
	"otel.go", "logging.go",
}

// localFiles is the sheet domain, whole: the model, the store, the actor, the
// mutation and recalculation paths, and the edit log.
var localFiles = []string{
	"grid.go", "bandkey.go", "formula.go", "pxnum.go", "style.go", "rowheight.go",
	"editlog.go", "actor.go", "mutate.go", "recalc.go",
	"store.go", "storeread.go", "storewrite.go", "storecache.go", "storeseed.go",
}

// localReadFiles is the read path: open a sheet, read a window, read styles.
// Everything else is compiled but not anchored, so the linker decides.
var localReadFiles = []string{
	"grid.go", "bandkey.go", "formula.go", "pxnum.go", "style.go", "rowheight.go",
	"store.go", "storeread.go", "storecache.go",
}

// relocations performs, on the copies, the three moves SPIKE-LAYOUT.md lists
// under "what was not done, and should be decided". shim/relocated.go supplies
// the declarations on the side they belong on.
var relocations = []string{
	"otel.go:connAttrs",     // the obs -> live edge: `screenObs` knows what a `screen` is
	"render.go:rowHeightPx", // the sheet -> view edge: a layout constant filed in the renderer
	"window.go:rowRange",    // the sheet -> view edge: the window range type
}

var relocated = []string{"connAttrs", "rowHeightPx", "rowRange"}

func stageNames() string {
	n := make([]string, len(stages))
	for i, s := range stages {
		n[i] = s.name
	}
	return strings.Join(n, " | ")
}

func main() {
	flag.Parse()
	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	idx, err := indexRoot(root)
	if err != nil {
		fatal(err)
	}

	fmt.Printf("toolchain: %s %s/%s (host), target js/wasm\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Printf("root package: %d non-test files, %d top-level declarations\n\n", len(idx.files), len(idx.decl))

	if *deps {
		priceDeps(root, idx)
		return
	}

	want := map[string]bool{}
	if *only != "" {
		for _, n := range strings.Split(*only, ",") {
			want[strings.TrimSpace(n)] = true
		}
	}

	var results []result
	for _, st := range stages {
		if len(want) > 0 && !want[st.name] {
			continue
		}
		r, err := run(root, idx, st)
		if err != nil {
			fmt.Printf("STAGE %-6s BLOCKED\n%s\n\n", st.name, indent(err.Error()))
			results = append(results, result{stage: st, blocked: err})
			continue
		}
		fmt.Printf("STAGE %-6s %s\n", st.name, st.what)
		fmt.Printf("  files:  %d seeded, %d pulled in by the compiler, %d total\n",
			len(r.seeded), len(r.pulled), len(r.seeded)+len(r.pulled))
		if len(r.pulled) > 0 {
			fmt.Printf("  pulled: %s\n", strings.Join(r.pulled, " "))
		}
		fmt.Printf("  reach:  %d functions and methods held reachable\n", r.reach)
		fmt.Printf("  size:   raw %s   gzip-9 %s   brotli-11 %s\n\n",
			mb(r.raw), mb(r.gzip), mb(r.brotli))
		results = append(results, r)
	}

	report(results)
}

// ─── Running one stage ────────────────────────────────────────────────────────

type result struct {
	stage   stage
	seeded  []string
	pulled  []string
	reach   int
	raw     int
	gzip    int
	brotli  int
	blocked error
}

func run(root string, idx *rootIndex, st stage) (result, error) {
	dir := *keepDir
	if dir != "" {
		dir = filepath.Join(dir, st.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return result{}, err
		}
	} else {
		var err error
		dir, err = os.MkdirTemp("", "wasmsize-"+st.name+"-")
		if err != nil {
			return result{}, err
		}
		defer os.RemoveAll(dir)
	}

	seeds := st.seeds
	if len(seeds) == 1 && seeds[0] == "*" {
		seeds = append([]string(nil), idx.files...)
	}
	extra := st.compile
	if len(extra) == 1 && extra[0] == "*" {
		extra = append([]string(nil), idx.files...)
	}
	set := map[string]bool{}
	for _, f := range extra {
		set[f] = true
	}
	for _, f := range seeds {
		if !idx.has(f) {
			return result{}, fmt.Errorf("seed %s is not a non-test file in the repo root", f)
		}
		set[f] = true
	}

	provided := map[string]bool{}
	for _, n := range st.provide {
		provided[n] = true
	}

	var lastErr string
	for iter := 0; iter < *maxIters; iter++ {
		// Compile everything the closure demands, but hold only the SEED files
		// reachable. The linker's dead-code elimination then decides how much of
		// the rest this domain actually reaches — which is the number a
		// stand-in would pay, rather than the number a compiler forces.
		reach, err := assemble(root, idx, st, dir, set, seeds)
		if err != nil {
			return result{}, err
		}
		out, buildErr := build(dir, filepath.Join(dir, "out.wasm"))
		if buildErr == nil {
			raw, gz, br, err := measure(filepath.Join(dir, "out.wasm"))
			if err != nil {
				return result{}, err
			}
			return result{
				stage: st, seeded: sorted(seeds), pulled: sorted(diff(set, seeds)),
				reach: reach, raw: raw, gzip: gz, brotli: br,
			}, nil
		}
		lastErr = out
		missing := idx.resolve(out, provided)
		if *why {
			for _, r := range idx.reasons(out) {
				fmt.Printf("  [%s why] %-46s pulls in %s\n", st.name, r.where, r.file)
			}
		}
		if len(missing) == 0 {
			return result{}, fmt.Errorf("build failed and no missing root symbol explains it:\n%s", trim(out))
		}
		if *verbose {
			fmt.Printf("  [%s iter %d] +%d files: %s\n", st.name, iter, len(missing), strings.Join(sorted(missing), " "))
		}
		for _, f := range missing {
			set[f] = true
		}
	}
	return result{}, fmt.Errorf("closure did not converge in %d attempts; last error:\n%s", *maxIters, trim(lastErr))
}

// assemble writes the scratch module: go.mod, go.sum, the shim, the chosen root
// files, and the generated reachability root. It returns how many functions and
// methods that root holds live.
func assemble(root string, idx *rootIndex, st stage, dir string, set map[string]bool, reachFiles []string) (int, error) {
	for _, old := range globAll(dir, "*.go") {
		if err := os.Remove(old); err != nil {
			return 0, err
		}
	}

	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return 0, err
	}
	gomod = regexp.MustCompile(`(?m)^module .*$`).ReplaceAll(gomod, []byte("module wasmsizestage"))
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), gomod, 0o644); err != nil {
		return 0, err
	}
	if err := copyFile(filepath.Join(root, "go.sum"), filepath.Join(dir, "go.sum")); err != nil {
		return 0, err
	}

	// The shim: gosw's bridge plus the stub driver. Stored as .go.txt so a host
	// `go vet ./...` does not try to build a package whose files are all
	// excluded by //go:build js && wasm.
	shim, err := os.ReadDir(filepath.Join(root, "spike", "wasmsize", "shim"))
	if err != nil {
		return 0, err
	}
	for _, e := range shim {
		if !strings.HasSuffix(e.Name(), ".go.txt") {
			continue
		}
		dst := filepath.Join(dir, strings.TrimSuffix(e.Name(), ".txt"))
		if err := copyFile(filepath.Join(root, "spike", "wasmsize", "shim", e.Name()), dst); err != nil {
			return 0, err
		}
	}

	if st.graft != "" {
		src := filepath.Join(root, "spike", "wasmsize", "graft", st.graft)
		dst := filepath.Join(dir, strings.TrimSuffix(st.graft, ".txt"))
		if err := copyFile(src, dst); err != nil {
			return 0, err
		}
	}

	var files []string
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)

	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			return 0, err
		}
		b = patch(st, f, b)
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			return 0, err
		}
		if f == "assets.go" { // //go:embed vendorjs/...
			if err := copyTree(filepath.Join(root, "vendorjs"), filepath.Join(dir, "vendorjs")); err != nil {
				return 0, err
			}
		}
	}

	reach, n := idx.reachFile(sorted(reachFiles))
	if err := os.WriteFile(filepath.Join(dir, "zz_reach.go"), reach, 0o644); err != nil {
		return 0, err
	}
	return n, nil
}

// cutSet is -cut parsed: file -> declaration names to delete from that file's
// copy. Nothing in the repo is touched; only the scratch copy loses them.
func cutSet(extra []string) map[string]map[string]bool {
	m := map[string]map[string]bool{}
	for _, spec := range append(strings.Split(*cut, ","), extra...) {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		file, name, ok := strings.Cut(spec, ":")
		if !ok {
			fatal(fmt.Errorf("-cut %q: want file.go:Name", spec))
		}
		if m[file] == nil {
			m[file] = map[string]bool{}
		}
		m[file][name] = true
	}
	return m
}

// cutDecls removes the named top-level function declarations from one file's
// source, so the closure can be re-run without one reference and the difference
// priced.
func cutDecls(src []byte, names map[string]bool) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var keep []ast.Decl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			if names[fd.Name.Name] {
				continue
			}
			// A method on a cut type has to go with it.
			if recv, _, _ := recvType(fd.Recv); recv != "" && names[recv] {
				continue
			}
			keep = append(keep, d)
			continue
		}
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			keep = append(keep, d)
			continue
		}
		var specs []ast.Spec
		for _, sp := range gd.Specs {
			switch sp := sp.(type) {
			case *ast.TypeSpec:
				if names[sp.Name.Name] {
					continue
				}
			case *ast.ValueSpec:
				drop := false
				for _, id := range sp.Names {
					if names[id.Name] {
						drop = true
					}
				}
				if drop {
					continue
				}
			}
			specs = append(specs, sp)
		}
		if len(specs) == 0 && len(gd.Specs) > 0 {
			continue
		}
		gd.Specs = specs
		keep = append(keep, gd)
	}
	f.Decls = keep
	pruneImports(f)
	var out bytes.Buffer
	if err := printer.Fprint(&out, fset, f); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// pruneImports drops imports no surviving declaration qualifies against. Cutting
// a function is not a source edit a compiler will accept on its own — Go rejects
// an unused import — so this is the second half of "delete this function", and
// the set it removes is itself a finding: see the otel SDK in finding 13.
func pruneImports(f *ast.File) {
	used := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			used[id.Name] = true
		}
		return true
	})
	var keep []ast.Decl
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			keep = append(keep, d)
			continue
		}
		var specs []ast.Spec
		for _, sp := range gd.Specs {
			im := sp.(*ast.ImportSpec)
			name := ""
			if im.Name != nil {
				name = im.Name.Name
			} else {
				path := strings.Trim(im.Path.Value, `"`)
				name = path[strings.LastIndexByte(path, '/')+1:]
			}
			if name == "_" || name == "." || used[name] {
				specs = append(specs, sp)
			}
		}
		if len(specs) == 0 {
			continue
		}
		gd.Specs = specs
		keep = append(keep, gd)
	}
	f.Decls = keep
}

// patch applies the rewrites this measurement needs to the copied root files.
// All of them are recorded in SPIKE-WASM-STANDIN.md; nothing else is touched.
func patch(st stage, name string, b []byte) []byte {
	// 1. modernc.org/sqlite does not build for GOOS=js or GOOS=wasip1. The blank
	//    import appears in five files (store.go and the four the SPIKE-LAYOUT
	//    split produced); shim/sqlstub.go registers "sqlite" instead.
	b = regexp.MustCompile(`(?m)^\s*_ "modernc\.org/sqlite"\n`).ReplaceAll(b, nil)
	// 2. shim/serve.go owns func main. The root's is renamed, not deleted, so it
	//    stays in the reachability set and still drags in everything boot does.
	if name == "main.go" {
		b = bytes.Replace(b, []byte("\nfunc main() {"), []byte("\nfunc swRootMain() {"), 1)
	}
	// 3. -cut, if asked: delete named declarations so one reference can be priced.
	if names := cutSet(st.cuts)[name]; len(names) > 0 {
		out, err := cutDecls(b, names)
		if err != nil {
			fatal(fmt.Errorf("cut %s: %w", name, err))
		}
		b = out
	}
	return b
}

func build(dir, out string) (string, error) {
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

func measure(path string) (raw, gz, br int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, err
	}
	var g bytes.Buffer
	zw, err := gzip.NewWriterLevel(&g, gzip.BestCompression) // gzip -9
	if err != nil {
		return 0, 0, 0, err
	}
	if _, err := zw.Write(b); err != nil {
		return 0, 0, 0, err
	}
	if err := zw.Close(); err != nil {
		return 0, 0, 0, err
	}
	var r bytes.Buffer
	bw := brotli.NewWriterLevel(&r, brotli.BestCompression) // brotli -q 11
	if _, err := bw.Write(b); err != nil {
		return 0, 0, 0, err
	}
	if err := bw.Close(); err != nil {
		return 0, 0, 0, err
	}
	return len(b), g.Len(), r.Len(), nil
}

// ─── The index of the root package ────────────────────────────────────────────

type rootIndex struct {
	files []string            // non-test .go files in the repo root
	decl  map[string]string   // top-level name -> file
	meth  map[string][]string // method name -> files declaring it
	qmeth map[string]string   // "Recv.Method" -> file
	funcs map[string][]string // file -> reachability expressions
}

func (x *rootIndex) has(f string) bool {
	for _, g := range x.files {
		if g == f {
			return true
		}
	}
	return false
}

func indexRoot(root string) (*rootIndex, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	x := &rootIndex{
		decl:  map[string]string{},
		meth:  map[string][]string{},
		qmeth: map[string]string{},
		funcs: map[string][]string{},
	}
	fset := token.NewFileSet()
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(root, n), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		x.files = append(x.files, n)
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					switch sp := sp.(type) {
					case *ast.TypeSpec:
						x.decl[sp.Name.Name] = n
					case *ast.ValueSpec:
						for _, id := range sp.Names {
							if id.Name != "_" {
								x.decl[id.Name] = n
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil {
					if d.Name.Name != "_" {
						x.decl[d.Name.Name] = n
					}
					if generic(d) || d.Name.Name == "init" || d.Name.Name == "_" {
						continue
					}
					name := d.Name.Name
					if name == "main" {
						name = "swRootMain" // patch() renames it
					}
					x.funcs[n] = append(x.funcs[n], name)
					continue
				}
				recv, ptr, tparams := recvType(d.Recv)
				if recv == "" {
					continue
				}
				x.meth[d.Name.Name] = append(x.meth[d.Name.Name], n)
				x.qmeth[recv+"."+d.Name.Name] = n
				if generic(d) || tparams || d.Name.Name == "_" {
					continue // a method expression on a generic type needs an instantiation
				}
				if ptr {
					x.funcs[n] = append(x.funcs[n], fmt.Sprintf("(*%s).%s", recv, d.Name.Name))
				} else {
					x.funcs[n] = append(x.funcs[n], fmt.Sprintf("%s.%s", recv, d.Name.Name))
				}
			}
		}
	}
	sort.Strings(x.files)
	return x, nil
}

func generic(d *ast.FuncDecl) bool {
	return d.Type.TypeParams != nil && len(d.Type.TypeParams.List) > 0
}

// recvType reports the receiver's type name, whether it is a pointer receiver,
// and whether the receiver type is generic.
func recvType(recv *ast.FieldList) (name string, ptr bool, tparams bool) {
	if recv == nil || len(recv.List) == 0 {
		return "", false, false
	}
	t := recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		ptr = true
		t = star.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok { // Foo[T]
		return ident(ix.X), ptr, true
	}
	if ix, ok := t.(*ast.IndexListExpr); ok { // Foo[K, V]
		return ident(ix.X), ptr, true
	}
	return ident(t), ptr, false
}

func ident(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

var (
	reUndef  = regexp.MustCompile(`undefined: ([A-Za-z_][A-Za-z0-9_]*)`)
	reNoMeth = regexp.MustCompile(`type \*?([A-Za-z_][A-Za-z0-9_]*)(?:\[[^\]]*\])? has no field or method ([A-Za-z_][A-Za-z0-9_]*)`)
	reField  = regexp.MustCompile(`unknown field ([A-Za-z_][A-Za-z0-9_]*) in struct literal of type \*?([A-Za-z_][A-Za-z0-9_]*)`)
)

// resolve turns a build failure into the set of root files that would supply
// what it says is missing.
func (x *rootIndex) resolve(out string, provided map[string]bool) []string {
	add := map[string]bool{}
	for _, m := range reUndef.FindAllStringSubmatch(out, -1) {
		if provided[m[1]] {
			continue
		}
		if f, ok := x.decl[m[1]]; ok {
			add[f] = true
		}
	}
	for _, m := range reNoMeth.FindAllStringSubmatch(out, -1) {
		if provided[m[1]] || provided[m[2]] {
			continue
		}
		if f, ok := x.qmeth[m[1]+"."+m[2]]; ok {
			add[f] = true
			continue
		}
		for _, f := range x.meth[m[2]] {
			add[f] = true
		}
	}
	for _, m := range reField.FindAllStringSubmatch(out, -1) {
		if f, ok := x.decl[m[2]]; ok {
			add[f] = true
		}
	}
	var out2 []string
	for f := range add {
		out2 = append(out2, f)
	}
	return out2
}

type reason struct {
	where string // "style.go:1234:5: undefined: writeCell"
	file  string // the root file that declares it
}

// reasons is resolve with the evidence kept, so -why can print the single
// reference that drags a file into a stage.
func (x *rootIndex) reasons(out string) []reason {
	seen := map[string]bool{}
	var rs []reason
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "./"))
		var file string
		if m := reUndef.FindStringSubmatch(line); m != nil {
			file = x.decl[m[1]]
		} else if m := reNoMeth.FindStringSubmatch(line); m != nil {
			if f, ok := x.qmeth[m[1]+"."+m[2]]; ok {
				file = f
			} else if fs := x.meth[m[2]]; len(fs) > 0 {
				file = fs[0]
			}
		} else if m := reField.FindStringSubmatch(line); m != nil {
			file = x.decl[m[2]]
		}
		if file == "" || seen[line] {
			continue
		}
		seen[line] = true
		rs = append(rs, reason{where: line, file: file})
	}
	return rs
}

// ─── Pricing the third-party dependencies ─────────────────────────────────────
//
// Once a stage's closure turns out to be the whole package, subsetting stops
// producing a curve. What is left that still decomposes the total is the module
// graph: build the floor plus one dependency at a time and take the delta.

type depProbe struct {
	name string
	imp  string // import line
	use  string // an expression that makes the package reachable
	site string // where this codebase reaches it
}

var depProbes = []depProbe{
	{"nats-server", `"github.com/nats-io/nats-server/v2/server"`, `server.NewServer`, "bus.go — the embedded NATS server"},
	{"nats.go", `"github.com/nats-io/nats.go"`, `nats.Connect`, "bus.go, registry.go — the client"},
	{"otel-sdk", `sdktrace "go.opentelemetry.io/otel/sdk/trace"`, `sdktrace.NewTracerProvider`, "otel.go"},
	{"otel-stdout", `"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"`, `stdouttrace.New`, "otel.go — the exporter"},
	{"datastar", `"github.com/starfederation/datastar-go/datastar"`, `datastar.NewSSE`, "push.go, http.go"},
	{"httpcompression", `"github.com/CAFxX/httpcompression"`, `httpcompression.ContentTypes`, "main.go — the SSE compressor"},
}

func priceDeps(root string, idx *rootIndex) {
	base, err := runSynthetic(root, idx, "floor", "", "")
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%-16s %12s %12s %12s   %s\n", "probe", "raw", "gzip -9", "brotli", "delta gzip vs floor")
	fmt.Printf("%-16s %12s %12s %12s\n", "floor", mb(base.raw), mb(base.gzip), mb(base.brotli))
	for _, p := range depProbes {
		r, err := runSynthetic(root, idx, p.name, p.imp, p.use)
		if err != nil {
			fmt.Printf("%-16s %s\n", p.name, "BLOCKED: "+firstLine(err.Error()))
			continue
		}
		fmt.Printf("%-16s %12s %12s %12s   %+.2f MB   (%s)\n", p.name,
			mb(r.raw), mb(r.gzip), mb(r.brotli),
			float64(r.gzip-base.gzip)/(1<<20), p.site)
	}
}

// runSynthetic builds the shim plus, optionally, one reference into one
// third-party package. No repo-root files are involved.
func runSynthetic(root string, idx *rootIndex, name, imp, use string) (result, error) {
	dir, err := os.MkdirTemp("", "wasmsize-dep-"+name+"-")
	if err != nil {
		return result{}, err
	}
	defer os.RemoveAll(dir)
	if _, err := assemble(root, idx, stage{}, dir, map[string]bool{}, nil); err != nil {
		return result{}, err
	}
	if imp != "" {
		src := fmt.Sprintf("//go:build js && wasm\n\npackage main\n\nimport (\n\t%s\n)\n\nvar swDepProbe = []any{%s}\n", imp, use)
		if err := os.WriteFile(filepath.Join(dir, "zz_dep.go"), []byte(src), 0o644); err != nil {
			return result{}, err
		}
		// swReach is the linker anchor; extend it so the probe cannot be discarded.
		anchor := "//go:build js && wasm\n\npackage main\n\nvar swReach = []any{swDepProbe}\n"
		if err := os.WriteFile(filepath.Join(dir, "zz_reach.go"), []byte(anchor), 0o644); err != nil {
			return result{}, err
		}
	}
	out, buildErr := build(dir, filepath.Join(dir, "out.wasm"))
	if buildErr != nil {
		return result{}, fmt.Errorf("%s: %s", name, trim(out))
	}
	raw, gz, br, err := measure(filepath.Join(dir, "out.wasm"))
	if err != nil {
		return result{}, err
	}
	return result{raw: raw, gzip: gz, brotli: br}, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// reachFile generates the file that defeats the linker's dead-code elimination.
func (x *rootIndex) reachFile(files []string) ([]byte, int) {
	var b bytes.Buffer
	b.WriteString("// Code generated by spike/wasmsize. DO NOT EDIT.\n")
	b.WriteString("//\n")
	b.WriteString("// The Go linker discards unreachable code, so compiling a file adds nothing\n")
	b.WriteString("// to the binary on its own. Taking the address of every non-generic top-level\n")
	b.WriteString("// function and method these files declare makes the stage's denominator\n")
	b.WriteString("// exact: everything declared here, plus everything it transitively calls.\n\n")
	b.WriteString("//go:build js && wasm\n\npackage main\n\n")
	var body bytes.Buffer
	n := 0
	for _, f := range files {
		fns := x.funcs[f]
		if len(fns) == 0 {
			continue
		}
		fmt.Fprintf(&body, "\t// %s\n", f)
		for _, fn := range fns {
			fmt.Fprintf(&body, "\t%s,\n", fn)
			n++
		}
	}
	if n == 0 {
		b.WriteString("var swReach = []any{}\n")
	} else {
		b.WriteString("var swReach = []any{\n")
		b.Write(body.Bytes())
		b.WriteString("}\n")
	}
	return b.Bytes(), n
}

// ─── Reporting ────────────────────────────────────────────────────────────────

func report(rs []result) {
	fmt.Println("─── summary ─────────────────────────────────────────────────────────────")
	fmt.Printf("%-8s %6s  %12s  %12s  %12s\n", "stage", "files", "raw", "gzip -9", "brotli -q11")
	for _, r := range rs {
		if r.blocked != nil {
			fmt.Printf("%-8s %6s  %12s  %12s  %12s\n", r.stage.name, "-", "BLOCKED", "", "")
			continue
		}
		fmt.Printf("%-8s %6d  %12s  %12s  %12s   raw=%d gzip=%d brotli=%d\n", r.stage.name,
			len(r.seeded)+len(r.pulled), mb(r.raw), mb(r.gzip), mb(r.brotli),
			r.raw, r.gzip, r.brotli)
	}
	fmt.Println()
	fmt.Println("for comparison:")
	fmt.Println("  gosw bare net/http handler (Go 1.26.1, same flags)   6.01 MB raw   1.68 MB gzip")
	fmt.Println("  ../hypermedia-sw-demo sqlite3.wasm                                  848 KB")
	fmt.Println("  hypersheets first visit today                                      27.9 KB")
}

func mb(n int) string {
	if n < 1<<20 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.2f MB", float64(n)/(1<<20))
}

// ─── Small helpers ────────────────────────────────────────────────────────────

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the working directory")
		}
		dir = parent
	}
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		return copyFile(p, out)
	})
}

func globAll(dir, pat string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, pat))
	return m
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func diff(set map[string]bool, seeds []string) []string {
	seen := map[string]bool{}
	for _, s := range seeds {
		seen[s] = true
	}
	var out []string
	for f := range set {
		if !seen[f] {
			out = append(out, f)
		}
	}
	return out
}

func trim(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 20 {
		lines = append(lines[:20], fmt.Sprintf("... and %d more lines", len(lines)-20))
	}
	return indent(strings.Join(lines, "\n"))
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "wasmsize:", err)
	os.Exit(1)
}
