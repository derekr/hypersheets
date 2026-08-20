// Command layout answers one question about this codebase: if the files were
// grouped into domains, would the groups form a layering or a knot?
//
// It exists because that question is usually settled by taste, and taste is
// exactly what a reader arriving at 25,000 lines in one flat package does not
// have. The compiler already knows the answer — every reference from one file to
// another is an edge — so this type-checks the package, resolves each identifier
// to the object it actually denotes, collapses the file graph onto a proposed
// grouping, and reports the cycles.
//
// Type-checking rather than matching names, and the difference is not academic:
// `sheetID` is a flag in main.go and a local in a dozen handlers, and a
// name-matching pass reports every one of those as a dependency on boot. The
// rule that falls out of real resolution is pleasingly simple — a use of an
// object declared in another file of the same package is an edge, and a local
// can never be one, because a local is declared where it is used.
//
// A grouping with no cycles can become Go packages, and the compiler will then
// keep it honest for free. A grouping with cycles is a diagram, not a structure:
// it can still be a reading order, but it cannot be a boundary.
//
// It reports the offending symbols alongside each cycle, because "these two
// domains are tangled" is not actionable and "these two domains are tangled by
// these four names" is.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var (
	dir      = flag.String("dir", ".", "directory of the flat package to analyse")
	grouping = flag.String("grouping", "proposed", "which grouping to test: "+groupNames)
	verbose  = flag.Bool("v", false, "list every cross-domain symbol, not just the ones in cycles")
	unbound  = flag.Bool("unbound", false, "list files the grouping does not mention")
	strict   = flag.Bool("strict", false, "fail on type errors instead of resolving what it can")
	explain  = flag.String("explain", "", "a domain pair like sheet->web: show which FILES make that edge, and why")
	sections = flag.Int("sections", 0, "files longer than this many lines, split along the section banners they already carry")
	cost     = flag.Bool("cost", false, "what turning this grouping into Go packages would actually cost")
)

// A domain is a name and the files it claims. The orderings below are proposals
// to be tested, not conclusions — the whole point is that the tool can say no.
type domain struct {
	name  string
	files []string
	why   string
}

const groupNames = "proposed | refined | layers | current"

// proposed: bounded contexts, cut where the vocabulary changes.
var proposed = []domain{
	{"sheet", []string{
		"store.go", "bandkey.go", "grid.go", "mutate.go", "structure.go",
		"recalc.go", "formula.go", "style.go", "rowheight.go", "rangeops.go",
		"editlog.go", "actor.go", "window.go", "reaper.go",
	}, "the model: cells, keys, formulas, and the one goroutine that writes them"},
	{"live", []string{
		"bus.go", "registry.go", "screen.go", "push.go", "windowcache.go", "presence.go",
	}, "the realtime layer: who is watching, what woke them, what goes on the wire"},
	{"web", []string{
		"http.go", "render.go", "keys.go", "styleui.go", "presenceui.go",
		"formulabar.go", "anchor.go", "growrows.go", "latency.go", "assets.go",
		"index.go", "headers.go", "readonly.go", "limits.go", "pxnum.go",
	}, "the hypermedia surface: routes, HTML, the client's own script"},
	{"obs", []string{"otel.go", "logging.go", "analyze.go"}, "traces, logs, and the tool that reads them back"},
	{"boot", []string{"main.go"}, "flags and start order"},
}

// refined: what `proposed` becomes once the tool has been listened to. Two
// changes, both of which the evidence forced rather than suggested:
//
//   - structure.go and rangeops.go move out of the model. Their names say
//     "insert rows" and "operations over a selection", but their contents are
//     HTTP handlers — they reach for Server, respondCommand and authorName. They
//     were the WHOLE of the sheet->live edge and most of sheet->web.
//   - render splits out of web. push.go renders, and a layer that renders is not
//     the same layer as one that routes. With them merged, "the push layer needs
//     the HTML layer" reads as a cycle; separated, it is an ordinary descent.
var refined = []domain{
	{"sheet", []string{
		"store.go", "bandkey.go", "grid.go", "mutate.go", "recalc.go", "formula.go",
		"style.go", "rowheight.go", "editlog.go", "actor.go",
	}, "the model: cells, keys, formulas, and the one goroutine that writes them"},
	{"view", []string{
		"render.go", "keys.go", "formulabar.go", "anchor.go", "assets.go", "window.go",
	}, "cells in, HTML and client script out — no request, no connection"},
	{"live", []string{
		"bus.go", "registry.go", "screen.go", "push.go", "windowcache.go", "presence.go",
	}, "who is watching, what woke them, what goes on the wire"},
	{"web", []string{
		"http.go", "structure.go", "rangeops.go", "styleui.go", "presenceui.go",
		"growrows.go", "latency.go", "index.go", "headers.go", "limits.go",
		"readonly.go", "pxnum.go", "reaper.go",
	}, "routes, commands, policy — everything that starts from a request"},
	{"obs", []string{"otel.go", "logging.go", "analyze.go"}, "traces, logs, and the tool that reads them back"},
	{"boot", []string{"main.go"}, "flags and start order"},
}

// layers: the classic three-tier cut, offered as the obvious alternative so the
// comparison is against something and not against nothing.
var layers = []domain{
	{"model", []string{
		"store.go", "bandkey.go", "grid.go", "mutate.go", "structure.go", "recalc.go",
		"formula.go", "style.go", "rowheight.go", "rangeops.go", "editlog.go",
		"actor.go", "window.go", "reaper.go", "windowcache.go", "presence.go",
	}, "everything that knows what a cell is"},
	{"transport", []string{
		"bus.go", "registry.go", "screen.go", "push.go", "http.go", "index.go",
		"headers.go", "limits.go", "pxnum.go", "readonly.go",
	}, "everything that knows what a request is"},
	{"view", []string{
		"render.go", "keys.go", "styleui.go", "presenceui.go", "formulabar.go",
		"anchor.go", "growrows.go", "latency.go", "assets.go",
	}, "everything that produces bytes for a browser"},
	{"obs", []string{"otel.go", "logging.go", "analyze.go"}, "traces and logs"},
	{"boot", []string{"main.go"}, "flags and start order"},
}

func groupingBy(name string) []domain {
	switch name {
	case "proposed":
		return proposed
	case "refined":
		return refined
	case "layers":
		return layers
	case "current":
		var all []domain
		for _, f := range goFiles(*dir) {
			all = append(all, domain{f, []string{f}, ""})
		}
		return all
	}
	fmt.Fprintf(os.Stderr, "unknown grouping %q, want one of: %s\n", name, groupNames)
	os.Exit(2)
	return nil
}

func main() {
	flag.Parse()
	files := goFiles(*dir)
	fset := token.NewFileSet()

	loc := map[string]int{}
	parsed := make([]*ast.File, 0, len(files))
	for _, f := range files {
		src, err := os.ReadFile(filepath.Join(*dir, f))
		if err != nil {
			fatal(err)
		}
		loc[f] = strings.Count(string(src), "\n")
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			fatal(err)
		}
		parsed = append(parsed, file)
	}

	// The importer reads export data from the build cache, so the package's own
	// dependencies do not have to be re-analysed. Type errors are collected
	// rather than fatal: a missing third-party symbol still leaves every
	// reference WITHIN this package resolved, which is all this needs.
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{
		Importer: importer.ForCompiler(fset, "gc", nil),
		Error:    func(error) {},
	}
	pkg, err := conf.Check("sheetstream", fset, parsed, info)
	if err != nil && *strict {
		fatal(err)
	}
	_ = pkg

	// THE WHOLE RULE: a use of an object declared in another file of this package
	// is an edge. A local cannot produce one, because a local is declared in the
	// file that uses it — which is why resolution, rather than name matching,
	// removes the noise instead of merely reducing it.
	refs := map[string]map[string]map[string]bool{}
	for id, obj := range info.Uses {
		if obj == nil || obj.Pkg() == nil || !obj.Pos().IsValid() {
			continue
		}
		from := filepath.Base(fset.Position(id.Pos()).Filename)
		to := filepath.Base(fset.Position(obj.Pos()).Filename)
		if from == to || loc[from] == 0 || loc[to] == 0 {
			continue
		}
		if refs[from] == nil {
			refs[from] = map[string]map[string]bool{}
		}
		if refs[from][to] == nil {
			refs[from][to] = map[string]bool{}
		}
		refs[from][to][symbolName(obj)] = true
	}

	doms := groupingBy(*grouping)
	of := map[string]string{}
	for _, d := range doms {
		for _, f := range d.files {
			of[f] = d.name
		}
	}
	if *unbound {
		var missing []string
		for _, f := range files {
			if of[f] == "" {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			fmt.Printf("files not claimed by the %q grouping: %s\n\n", *grouping, strings.Join(missing, " "))
		}
	}

	// Collapse the file graph onto the domains.
	edge := map[string]map[string]map[string]bool{} // from -> to -> symbols
	for f, tos := range refs {
		a := of[f]
		if a == "" {
			continue
		}
		for t, syms := range tos {
			b := of[t]
			if b == "" || a == b {
				continue
			}
			if edge[a] == nil {
				edge[a] = map[string]map[string]bool{}
			}
			if edge[a][b] == nil {
				edge[a][b] = map[string]bool{}
			}
			for s := range syms {
				edge[a][b][s] = true
			}
		}
	}

	fmt.Printf("grouping: %s\n\n", *grouping)
	fmt.Printf("%-10s %6s %5s  %s\n", "domain", "loc", "files", "what it is")
	fmt.Println(strings.Repeat("-", 100))
	for _, d := range doms {
		n := 0
		for _, f := range d.files {
			n += loc[f]
		}
		fmt.Printf("%-10s %6d %5d  %s\n", d.name, n, len(d.files), d.why)
	}

	fmt.Printf("\ndependency edges (weight = distinct symbols crossed):\n")
	names := domainNames(doms)
	for _, a := range names {
		var outs []string
		for _, b := range names {
			if n := len(edge[a][b]); n > 0 {
				outs = append(outs, fmt.Sprintf("%s(%d)", b, n))
			}
		}
		if len(outs) > 0 {
			fmt.Printf("  %-10s -> %s\n", a, strings.Join(outs, "  "))
		}
	}

	cycles := sccs(names, edge)
	fmt.Println()
	if len(cycles) == 0 {
		fmt.Printf("ACYCLIC. This grouping can be Go packages, and the compiler will hold it.\n")
	} else {
		fmt.Printf("CYCLIC — %d knot(s). These cannot be packages as drawn:\n", len(cycles))
		for _, c := range cycles {
			fmt.Printf("\n  %s\n", strings.Join(c, " <-> "))
			for _, a := range c {
				for _, b := range c {
					if a == b || len(edge[a][b]) == 0 {
						continue
					}
					fmt.Printf("    %s -> %s via %s\n", a, b, top(edge[a][b], 12))
				}
			}
		}
	}

	// A domain edge is only actionable once you know which files draw it. A knot
	// is usually not "these two domains are entangled" but "one file is standing
	// in both of them", and that file is the thing to move or split.
	if *explain != "" {
		a, b, ok := strings.Cut(*explain, "->")
		if !ok {
			fatal(fmt.Errorf("want -explain from->to"))
		}
		a, b = strings.TrimSpace(a), strings.TrimSpace(b)
		fmt.Printf("\nfiles making %s -> %s:\n", a, b)
		type fe struct {
			from, to string
			syms     []string
		}
		var out []fe
		for f, tos := range refs {
			if of[f] != a {
				continue
			}
			for t, syms := range tos {
				if of[t] != b {
					continue
				}
				var ss []string
				for x := range syms {
					ss = append(ss, x)
				}
				sort.Strings(ss)
				out = append(out, fe{f, t, ss})
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if len(out[i].syms) != len(out[j].syms) {
				return len(out[i].syms) > len(out[j].syms)
			}
			return out[i].from < out[j].from
		})
		byFile := map[string]int{}
		for _, e := range out {
			byFile[e.from] += len(e.syms)
			fmt.Printf("  %-18s -> %-18s %s\n", e.from, e.to, strings.Join(e.syms, " "))
		}
		fmt.Printf("\n  per source file:\n")
		type kv struct {
			f string
			n int
		}
		var tot []kv
		for f, n := range byFile {
			tot = append(tot, kv{f, n})
		}
		sort.Slice(tot, func(i, j int) bool { return tot[i].n > tot[j].n })
		for _, t := range tot {
			fmt.Printf("    %-18s %d\n", t.f, t.n)
		}
	}

	// THE OTHER HALF OF "CAN A READER FIND ANYTHING". Domains answer where a
	// thing lives; length answers whether it can be read once found. A 160-line
	// file about one feature is a good afternoon; a 2,900-line file is a
	// reference work, and nobody groks a reference work.
	//
	// The seams are already drawn — the long files carry section banners — so
	// this reports them as a split plan rather than as a complaint.
	if *sections > 0 {
		fmt.Printf("\nfiles over %d lines, and the seams they already carry:\n", *sections)
		var long []string
		for _, f := range files {
			if loc[f] > *sections {
				long = append(long, f)
			}
		}
		sort.Slice(long, func(i, j int) bool { return loc[long[i]] > loc[long[j]] })
		total, pieces := 0, 0
		for _, f := range long {
			total += loc[f]
			cuts := banners(filepath.Join(*dir, f))
			fmt.Printf("\n  %s  (%d lines, %s, %d sections)\n", f, loc[f], of[f], len(cuts))
			for i, c := range cuts {
				end := loc[f]
				if i+1 < len(cuts) {
					end = cuts[i+1].line
				}
				fmt.Printf("      %5d  %s\n", end-c.line, c.title)
				pieces++
			}
		}
		fmt.Printf("\n  %d files, %d lines, %d existing sections -> ~%d lines per file after a split along them\n",
			len(long), total, pieces, total/max(pieces, 1))
	}

	// WHAT PACKAGES WOULD COST, in the only currency that matters here: how much
	// of the code a reader would have to see differently.
	//
	// In Go, moving files into directories IS making packages — there is no
	// cheaper version. So every symbol that crosses a domain boundary has to
	// become exported, and every use of it has to become qualified. Both are
	// permanent changes to how the code reads, paid on every line forever, so
	// the number is worth having before the taste argument starts.
	if *cost {
		exports := map[string]map[string]bool{} // domain -> symbols it must export
		sites := 0
		for id, obj := range info.Uses {
			if obj == nil || obj.Pkg() == nil || !obj.Pos().IsValid() {
				continue
			}
			from := of[filepath.Base(fset.Position(id.Pos()).Filename)]
			owner := filepath.Base(fset.Position(obj.Pos()).Filename)
			to := of[owner]
			if from == "" || to == "" || from == to {
				continue
			}
			sites++
			name := obj.Name()
			if name == "" || !strings.ContainsAny(name[:1], "abcdefghijklmnopqrstuvwxyz") {
				continue // already exported
			}
			if exports[to] == nil {
				exports[to] = map[string]bool{}
			}
			exports[to][symbolName(obj)] = true
		}
		fmt.Printf("\ncost of making this grouping into packages:\n")
		total := 0
		for _, d := range doms {
			n := len(exports[d.name])
			total += n
			fmt.Printf("  %-10s must export %3d symbols that are unexported today\n", d.name, n)
		}
		fmt.Printf("  %-10s %d symbols, and %d call sites become qualified names\n", "TOTAL", total, sites)
	}

	if *verbose {
		fmt.Printf("\nevery cross-domain reference:\n")
		for _, a := range names {
			for _, b := range names {
				if len(edge[a][b]) > 0 {
					fmt.Printf("  %s -> %s: %s\n", a, b, top(edge[a][b], 1000))
				}
			}
		}
	}
}

// symbolName is how a crossing is reported. A method is named on its receiver,
// because "web reaches into sheet for Window" is not useful and "for
// (*Sheet).Window" is.
func symbolName(obj types.Object) string {
	if fn, ok := obj.(*types.Func); ok {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			recv := sig.Recv().Type().String()
			if i := strings.LastIndex(recv, "."); i >= 0 {
				recv = recv[i+1:]
			}
			return recv + "." + fn.Name()
		}
	}
	return obj.Name()
}

// sccs returns the strongly connected components with more than one member —
// the knots. Tarjan, iterative depth kept simple because the graph is tiny.
func sccs(nodes []string, edge map[string]map[string]map[string]bool) [][]string {
	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	next := 0
	var out [][]string

	var strong func(v string)
	strong = func(v string) {
		index[v] = next
		low[v] = next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for w := range edge[v] {
			if _, seen := index[w]; !seen {
				strong(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			if len(comp) > 1 {
				sort.Strings(comp)
				out = append(out, comp)
			}
		}
	}
	for _, v := range nodes {
		if _, seen := index[v]; !seen {
			strong(v)
		}
	}
	return out
}

func domainNames(doms []domain) []string {
	var out []string
	for _, d := range doms {
		out = append(out, d.name)
	}
	return out
}

func top(set map[string]bool, n int) string {
	var out []string
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) > n {
		return strings.Join(out[:n], " ") + fmt.Sprintf(" … +%d", len(out)-n)
	}
	return strings.Join(out, " ")
}

func goFiles(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		fatal(err)
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

type banner struct {
	line  int
	title string
}

// banners finds the section rules the codebase already writes, which is where a
// long file would be cut if it were cut at all. Reading the seams out of the
// source rather than inventing them is the point: a split along lines the author
// already drew keeps every explanation attached to what it explains.
func banners(path string) []banner {
	src, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	var out []banner
	for i, line := range strings.Split(string(src), "\n") {
		if !strings.HasPrefix(line, "// \u2500\u2500\u2500 ") {
			continue
		}
		title := strings.TrimPrefix(line, "// \u2500\u2500\u2500 ")
		title = strings.TrimRight(strings.TrimRight(title, "\u2500"), " ")
		out = append(out, banner{i + 1, title})
	}
	return out
}
