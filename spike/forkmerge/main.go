// Command forkmerge answers one question about this codebase: if "offline" were
// a deliberate FORK of a sheet — its own id, its own SQLite file, nothing
// merging automatically — how expensive is merging two forks back together?
//
// The claim being tested is that it is unusually cheap here, for two reasons and
// with one exception:
//
//  1. band keys mean rows never shift, so matching rows across a fork is a join
//     on the storage key rather than an alignment problem;
//  2. only inputs need merging, because formulas are deterministic and recalc.go
//     re-derives every computed value from the literals;
//  3. except that a formula's references are display coordinates, so two
//     independent structural mutations from a common base do not commute.
//
// Findings are in SPIKE-FORK-MERGE.md. Two of the three are wrong as stated, and
// the third is right for a different reason than the one given.
//
// ─── Why this file only launches the work ─────────────────────────────────────
//
// The spike needs the real Sheet API — WriteCell, ApplyBatch, InsertRows,
// Window — and the band index behind them, and every one of those lives in the
// repo root's `package main`. A main package cannot be imported, so a program in
// spike/forkmerge/ cannot reach any of it; and *bandIndex in particular is
// unexported, so even converting the root to a library would not expose the one
// thing this spike most has to look at.
//
// The experiment therefore lives in forkmerge_spike_test.go at the repo root,
// inside the package it is measuring, and this command is the runnable front
// door the spike convention asks for. That is itself a small finding about the
// arrangement SPIKE-LAYOUT.md settled on: the flat package is readable, and it
// is unreachable from a sibling directory.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

var (
	seedRows = flag.Int("seed-rows", 250, "rows to seed the base sheet with (>=201 exercises the five-band Y cascade)")
	only     = flag.String("case", "", "run one scenario by name (keys, fork, log, a..f); empty runs all")
	verbose  = flag.Bool("v", false, "print every cell the merge touched, not just the summary")
	keep     = flag.String("keep", "", "keep the sheet files in this directory instead of a temp dir")
)

func main() {
	flag.Parse()
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "forkmerge:", err)
		os.Exit(1)
	}
	args := []string{"test", "-count=1", "-run", "TestForkMergeSpike", "-v", root,
		"-args",
		"-fm.seed-rows=" + strconv.Itoa(*seedRows),
		"-fm.case=" + *only,
		"-fm.keep=" + *keep,
	}
	if *verbose {
		args = append(args, "-fm.verbose")
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "forkmerge:", err)
		os.Exit(1)
	}
}

// repoRoot walks up from the working directory for the go.mod that declares the
// module, so `go run ./spike/forkmerge` works from anywhere in the tree.
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
