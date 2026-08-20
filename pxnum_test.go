package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// A POINTER COORDINATE IS NOT AN INTEGER. On any display that is not at 1:1
// device pixels — every Retina screen, every fractional OS scale, every browser
// zoom that is not 100% — `clientY` arrives fractional, so a drag computes a
// height like 130.51953125. As an `int` field that is not a number the command
// can read: json.Unmarshal refuses it, the handler answers 400 before it logs
// anything, and the browser drops the response. The row does not move, no error
// reaches the user, and nothing reaches the log.
//
// It hid for three rounds of debugging behind a signature that looked like
// something else entirely: the only resizes that ever succeeded landed on
// exactly the minimum, because the clamp returns an integer constant and that
// was the sole path by which any value survived the wire at all.
func TestAPixelLengthAcceptsWhatAPointerActuallyReports(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"130.51953125", 131},
		{"39.0078125", 39},
		{"22", 22},
		{"21.5", 22},
		{"-3.2", -3},
		{"0", 0},
	} {
		var p pxNum
		if err := json.Unmarshal([]byte(tc.in), &p); err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if p.Int() != tc.want {
			t.Errorf("%s -> %d, want %d", tc.in, p.Int(), tc.want)
		}
	}
	// A float outside int range must not be converted, which is undefined.
	var p pxNum
	if err := json.Unmarshal([]byte("1e300"), &p); err != nil {
		t.Fatalf("a huge float should clamp, not fail: %v", err)
	}
	if p.Int() <= 0 {
		t.Errorf("1e300 -> %d, want a large positive clamp", p.Int())
	}
	for _, bad := range []string{`"22"`, `null`, `{}`} {
		if err := json.Unmarshal([]byte(bad), &p); err == nil {
			t.Errorf("%s was accepted as a pixel length", bad)
		}
	}
	if err := json.Unmarshal([]byte("1e400"), &p); err == nil {
		t.Error("an infinite float was accepted as a pixel length")
	}
	_ = math.Round
}

// And the command's own signal structs take one, because the type is only half
// of it — the field has to be declared as one. These two are exactly what a
// browser posts off a Retina trackpad.
func TestTheResizeCommandsDecodeAFractionalDrag(t *testing.T) {
	var row rowResizeSignals
	if err := json.Unmarshal([]byte(`{"rr":4,"rh":130.51953125,"rf":66.5,"conn":"c"}`), &row); err != nil {
		t.Fatalf("the row resize refuses a real pointer coordinate: %v", err)
	}
	if row.Rh.Int() != 131 || row.Rf.Int() != 67 {
		t.Errorf("row drag decoded to h=%d f=%d, want 131/67", row.Rh.Int(), row.Rf.Int())
	}

	var col colResizeSignals
	if err := json.Unmarshal([]byte(`{"rc":2,"rw":181.7265625,"conn":"c"}`), &col); err != nil {
		t.Fatalf("the column resize refuses a real pointer coordinate: %v", err)
	}
	if col.Rw.Int() != 182 {
		t.Errorf("column drag decoded to w=%d, want 182", col.Rw.Int())
	}

	// The row index stays an int: it is an address, not a measurement, and a
	// fractional one is a bug worth hearing about.
	if err := json.Unmarshal([]byte(`{"rr":4.5,"rh":22}`), &row); err == nil {
		t.Error("a fractional row index was accepted")
	}
}

// The client must not send the fraction in the first place. Rounding on both
// sides is deliberate: the server is lenient so a gesture is never silently
// refused, and the client is exact so the number the guide showed is the number
// that lands.
func TestBothDragsRoundBeforeTheyCommit(t *testing.T) {
	js := anchorScript()
	for _, want := range []string{
		"T.rzAt=function(x){var v=Math.round(rzW+(x-rzX));",
		"T.rzAtR=function(y){var v=Math.round(rzHh+(y-rzYy));",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("a drag commits a fractional pixel length: %q is missing", want)
		}
	}
	// offsetHeight is fractional on the same displays, so fit rounds too.
	if !strings.Contains(js, "h=h?Math.ceil(h)+1:RH;") {
		t.Error("fit-to-contents commits a fractional height")
	}
}
