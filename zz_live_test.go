package main

import (
	"os"
	"testing"
	"time"
)

func TestProbeLiveMigration(t *testing.T) {
	dir := os.Getenv("SS_LIVE_COPY")
	if dir == "" {
		t.Skip("no SS_LIVE_COPY")
	}
	c := NewSheetCache(dir, 4, time.Minute)
	defer c.Close()
	start := time.Now()
	sh, err := c.Open("demo")
	if err != nil {
		t.Fatalf("open live sheet: %v", err)
	}
	t.Logf("open+migrate: %v", time.Since(start).Round(time.Millisecond))

	t.Logf("cells=%d with-refs=%d", countRows(t, sh, "cells"),
		countRows(t, sh, `cells WHERE ref0_k IS NOT NULL`))
	for _, a1 := range []string{"U1", "V1", "W1", "X51", "Y1", "Y51", "A1"} {
		c := mustCell(t, sh, a1)
		t.Logf("%-5s raw=%-22q computed=%-10q kind=%v", a1, c.Raw, c.Computed, c.Kind)
	}
	before := mustCell(t, sh, "U1")
	d, err := sh.InsertRows(0, 1)
	if err != nil {
		t.Fatalf("InsertRows on live sheet: %v", err)
	}
	s := d.Stats
	t.Logf("InsertRows(0,1): %v total = shift cells %v + scan %v + shift deps %v + recalc %v + write cells %v + write deps %v",
		s.Elapsed.Round(time.Millisecond), s.ShiftCells.Round(time.Millisecond),
		s.Scan.Round(time.Millisecond), s.ShiftDeps.Round(time.Millisecond),
		s.Recalc.Round(time.Millisecond), s.WriteCells.Round(time.Millisecond),
		s.WriteDeps.Round(time.Millisecond))
	after := mustCell(t, sh, "U2")
	t.Logf("U1 %q -> U2 %q (computed %q -> %q)", before.Raw, after.Raw, before.Computed, after.Computed)
	if after.Raw == before.Raw {
		t.Errorf("U2 raw %q did not move: references were not structural", after.Raw)
	}
	if after.Computed != before.Computed {
		t.Errorf("a uniform shift changed U's value: %q -> %q", before.Computed, after.Computed)
	}
	wantCell(t, sh, "A1", "", "")
}
