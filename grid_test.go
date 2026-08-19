package main

import (
	"errors"
	"reflect"
	"strconv"
	"testing"
)

func TestParseRef(t *testing.T) {
	tests := []struct {
		in      string
		want    CellRef
		wantErr bool
	}{
		{in: "A1", want: CellRef{0, 0}},
		{in: "B7", want: CellRef{6, 1}},
		{in: "Z1", want: CellRef{0, 25}},
		{in: "A10000", want: CellRef{9999, 0}},
		{in: "Z10000", want: CellRef{9999, 25}},
		{in: "b7", want: CellRef{6, 1}},   // lowercase accepted
		{in: " C3 ", want: CellRef{2, 2}}, // surrounding space trimmed

		{in: "", wantErr: true},
		{in: "A", wantErr: true},                // no row
		{in: "1", wantErr: true},                // no column
		{in: "AA1", wantErr: true},              // multi-letter column
		{in: "A0", wantErr: true},               // rows are 1-based
		{in: "A-1", wantErr: true},              // negative
		{in: "A1000001", wantErr: true},         // past RowCeiling
		{in: "A10001", want: CellRef{10000, 0}}, // past the DEFAULT extent, but a valid ref
		{in: "A1.5", wantErr: true},             // not an integer row
		{in: "[1", wantErr: true},               // char just before 'A'
		{in: "{1", wantErr: true},               // char just after 'z'
		{in: "A1:A2", wantErr: true},            // a range, not a ref
		{in: "A 1", wantErr: true},              // interior space
		{in: "A1 ", want: CellRef{0, 0}},        // trailing space is trimmed
	}
	for _, tc := range tests {
		got, err := ParseRef(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseRef(%q) = %v, want error", tc.in, got)
				continue
			}
			if !errors.Is(err, ErrBadRef) {
				t.Errorf("ParseRef(%q) error %v does not wrap ErrBadRef", tc.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRef(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseRef(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestRefStringRoundTrip(t *testing.T) {
	tests := []struct {
		ref  CellRef
		want string
	}{
		{CellRef{0, 0}, "A1"},
		{CellRef{6, 1}, "B7"},
		{CellRef{49, 25}, "Z50"},
		{CellRef{9999, 25}, "Z10000"}, // past a default sheet's extent, still a ref

		{CellRef{-1, 0}, "#REF!"},
		{CellRef{0, 26}, "#REF!"},
		{CellRef{DefaultRows, 0}, "A" + strconv.Itoa(DefaultRows+1)}, // past the default extent
		{CellRef{RowCeiling, 0}, "#REF!"},
	}
	for _, tc := range tests {
		if got := tc.ref.String(); got != tc.want {
			t.Errorf("%#v.String() = %q, want %q", tc.ref, got, tc.want)
		}
	}
	// Round-trip every valid column and a spread of rows.
	for _, row := range []int{0, 1, 49, 50, 99, 100, DefaultRows / 2, DefaultRows - 1,
		DefaultRows, RowCeiling - 1} {
		for col := 0; col < MaxCols; col++ {
			ref := CellRef{row, col}
			back, err := ParseRef(ref.String())
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", ref.String(), err)
			}
			if back != ref {
				t.Fatalf("round trip %v -> %q -> %v", ref, ref.String(), back)
			}
		}
	}
}

func TestParseRange(t *testing.T) {
	tests := []struct {
		in      string
		want    []CellRef
		wantErr bool
	}{
		{in: "A1:A3", want: []CellRef{{0, 0}, {1, 0}, {2, 0}}},
		{in: "A1:B2", want: []CellRef{{0, 0}, {0, 1}, {1, 0}, {1, 1}}},
		{in: "C3:C3", want: []CellRef{{2, 2}}},
		// Reversed endpoints normalize to the same rectangle.
		{in: "B2:A1", want: []CellRef{{0, 0}, {0, 1}, {1, 0}, {1, 1}}},

		{in: "A1", wantErr: true},          // not a range
		{in: "A1:", wantErr: true},         // missing end
		{in: ":A1", wantErr: true},         // missing start
		{in: "A1:AA2", wantErr: true},      // bad column in end
		{in: "A0:A5", wantErr: true},       // bad row in start
		{in: "A1:A1000001", wantErr: true}, // past RowCeiling
		{in: "A1:A10001", wantErr: true},   // 10,001 rows: past maxRangeRows
	}
	for _, tc := range tests {
		got, err := ParseRange(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseRange(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRange(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseRange(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	// A 10-cell SUM range, the shape recalc actually sees.
	got, err := ParseRange("A1:A10")
	if err != nil {
		t.Fatalf("ParseRange(A1:A10): %v", err)
	}
	if len(got) != 10 || got[0] != (CellRef{0, 0}) || got[9] != (CellRef{9, 0}) {
		t.Fatalf("ParseRange(A1:A10) = %v", got)
	}

	// The size cap must fire before allocating.
	if _, err := ParseRange("A1:Z9999"); err == nil {
		t.Fatalf("ParseRange(A1:Z9999) should exceed maxRangeCells")
	}
}

func TestBandMath(t *testing.T) {
	bandTests := []struct {
		row  int
		want int
	}{
		{0, 0}, {1, 0}, {49, 0},
		{50, 1}, {99, 1},
		{100, 2}, {150, 3}, {200, 4},
		{9999, 199},
		{-5, 0}, // clamped
	}
	for _, tc := range bandTests {
		if got := BandOf(tc.row); got != tc.want {
			t.Errorf("BandOf(%d) = %d, want %d", tc.row, got, tc.want)
		}
	}

	rowTests := []struct {
		band   int
		lo, hi int
	}{
		{0, 0, 49},
		{1, 50, 99},
		{3, 150, 199},
		{199, 9950, 9999},
		{-1, 0, 49}, // clamped
	}
	for _, tc := range rowTests {
		lo, hi := BandRows(tc.band)
		if lo != tc.lo || hi != tc.hi {
			t.Errorf("BandRows(%d) = %d,%d want %d,%d", tc.band, lo, hi, tc.lo, tc.hi)
		}
	}

	// Every row must map back into its own band's range.
	for _, row := range []int{0, 49, 50, 51, DefaultRows / 2, DefaultRows - 1, RowCeiling - 1} {
		lo, hi := BandRows(BandOf(row))
		if row < lo || row > hi {
			t.Errorf("row %d not inside band %d range %d..%d", row, BandOf(row), lo, hi)
		}
	}
	if want := (DefaultRows - 1) / BandHeight; BandOf(DefaultRows-1) != want {
		t.Errorf("BandOf(DefaultRows-1) = %d, want %d", BandOf(DefaultRows-1), want)
	}
}

func TestBandsFor(t *testing.T) {
	tests := []struct {
		name  string
		cells []CellRef
		want  []int
	}{
		{name: "empty", cells: nil, want: nil},
		{name: "single", cells: []CellRef{{0, 0}}, want: []int{0}},
		{
			name:  "dedupes within a band",
			cells: []CellRef{{0, 0}, {10, 5}, {49, 25}},
			want:  []int{0},
		},
		{
			name:  "sorts and dedupes across bands",
			cells: []CellRef{{200, 24}, {0, 0}, {150, 24}, {50, 24}, {100, 24}, {5, 0}},
			want:  []int{0, 1, 2, 3, 4},
		},
		{
			name:  "band boundary rows",
			cells: []CellRef{{49, 0}, {50, 0}},
			want:  []int{0, 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BandsFor(tc.cells)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("BandsFor(%v) = %v, want %v", tc.cells, got, tc.want)
			}
		})
	}
}

func TestBandRange(t *testing.T) {
	tests := []struct {
		lo, hi int
		want   []int
	}{
		{0, 0, []int{0}},
		{0, 49, []int{0}},
		{0, 50, []int{0, 1}},
		{49, 150, []int{0, 1, 2, 3}},
		{150, 49, []int{0, 1, 2, 3}},   // swapped
		{9990, 10049, []int{199, 200}}, // past the default extent, not clamped there
		{-10, 10, []int{0}},            // clamped
	}
	for _, tc := range tests {
		if got := BandRange(tc.lo, tc.hi); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("BandRange(%d,%d) = %v, want %v", tc.lo, tc.hi, got, tc.want)
		}
	}
}
