package main

import (
	"errors"
	"testing"
	"time"
)

func heightSheet(t *testing.T) *Sheet {
	t.Helper()
	c := newTestCache(t, 4, time.Minute)
	return mustOpen(t, c, "heights")
}

func TestRowHeightRoundTrips(t *testing.T) {
	sh := heightSheet(t)
	if err := sh.SetRowHeight([]int{3, 7}, 60); err != nil {
		t.Fatal(err)
	}
	got, err := sh.RowHeights(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got[3] != 60 || got[7] != 60 {
		t.Fatalf("heights = %v, want 3 and 7 at 60", got)
	}
	if len(got) != 2 {
		t.Errorf("heights = %v, want only the two rows that were set", got)
	}
}

// A row at the default must be ABSENT, not stored as 22. That is what keeps the
// render's per-row rules proportional to the number of resized rows.
func TestADefaultHeightIsNotStored(t *testing.T) {
	sh := heightSheet(t)
	if err := sh.SetRowHeight([]int{4}, 60); err != nil {
		t.Fatal(err)
	}
	if err := sh.SetRowHeight([]int{4}, rowHeightPx); err != nil {
		t.Fatal(err)
	}
	got, err := sh.RowHeights(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("heights = %v, want none: a row reset to the default is not a record", got)
	}
}

// Resetting a height must not take the row's style with it.
func TestAResetHeightKeepsTheRowStyle(t *testing.T) {
	sh := heightSheet(t)
	bold := true
	if _, err := sh.SetRowStyle([]int{5}, StylePatch{Bold: &bold}); err != nil {
		t.Fatal(err)
	}
	if err := sh.SetRowHeight([]int{5}, 80); err != nil {
		t.Fatal(err)
	}
	if err := sh.SetRowHeight([]int{5}, rowHeightPx); err != nil {
		t.Fatal(err)
	}
	styles, err := sh.RowStyles(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if styles[5] == 0 {
		t.Error("resetting the height deleted the row's style; the two ride the same record but are independent")
	}
}

// And the mirror: restyling a row must not disturb its height.
func TestRestylingARowKeepsItsHeight(t *testing.T) {
	sh := heightSheet(t)
	if err := sh.SetRowHeight([]int{6}, 90); err != nil {
		t.Fatal(err)
	}
	bold := true
	if _, err := sh.SetRowStyle([]int{6}, StylePatch{Bold: &bold}); err != nil {
		t.Fatal(err)
	}
	got, err := sh.RowHeights(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got[6] != 90 {
		t.Fatalf("height after a restyle = %v, want 90", got)
	}
}

func TestRowHeightIsClamped(t *testing.T) {
	sh := heightSheet(t)
	if err := sh.SetRowHeight([]int{1}, 5); err != nil {
		t.Fatal(err)
	}
	if err := sh.SetRowHeight([]int{2}, 99999); err != nil {
		t.Fatal(err)
	}
	got, _ := sh.RowHeights(0, 10)
	if got[1] != MinRowHeight {
		t.Errorf("row 1 = %d, want clamped to %d", got[1], MinRowHeight)
	}
	if got[2] != MaxRowHeight {
		t.Errorf("row 2 = %d, want clamped to %d", got[2], MaxRowHeight)
	}
}

func TestZeroHeightIsRefused(t *testing.T) {
	sh := heightSheet(t)
	if err := sh.SetRowHeight([]int{1}, 0); !errors.Is(err, ErrBadHeight) {
		t.Fatalf("err = %v, want ErrBadHeight: zero is ambiguous between "+
			"'default' and 'invisible'", err)
	}
}

func TestTotalHeightCountsOnlyTheDifference(t *testing.T) {
	sh := heightSheet(t)
	base, err := sh.TotalHeight()
	if err != nil {
		t.Fatal(err)
	}
	if want := sh.Rows() * rowHeightPx; base != want {
		t.Fatalf("a fresh sheet is %d tall, want %d", base, want)
	}
	if err := sh.SetRowHeight([]int{2}, 62); err != nil { // +40
		t.Fatal(err)
	}
	if err := sh.SetRowHeight([]int{9}, 12); err != nil { // clamps to 16, -6
		t.Fatal(err)
	}
	got, err := sh.TotalHeight()
	if err != nil {
		t.Fatal(err)
	}
	if want := base + 40 + (MinRowHeight - rowHeightPx); got != want {
		t.Fatalf("total = %d, want %d (base %d, +40, %+d)", got, want, base, MinRowHeight-rowHeightPx)
	}
}

// A height is keyed by storage key, so it must follow its row through an insert
// exactly as a row style does.
func TestAHeightFollowsItsRowThroughAnInsert(t *testing.T) {
	sh := heightSheet(t)
	if err := sh.SetRowHeight([]int{5}, 70); err != nil {
		t.Fatal(err)
	}
	if _, err := sh.InsertRows(2, 3); err != nil {
		t.Fatal(err)
	}
	got, err := sh.RowHeights(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got[8] != 70 {
		t.Fatalf("heights = %v, want the 70 to have moved from row 5 to row 8", got)
	}
	if got[5] != 0 {
		t.Errorf("row 5 kept a height it should have handed to row 8: %v", got)
	}
}
