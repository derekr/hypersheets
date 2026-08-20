package main

import (
	"strings"
	"testing"
)

// THE GUTTER SERVES THREE GESTURES ON OVERLAPPING PIXELS: click a row number to
// select the row, drag its bottom edge to resize, double-click that edge to fit.
// `#rn` is inside `#vp`, so a pointerdown that starts a resize also reaches the
// selection unless the resize claims it — and the way that failure presents is
// not "the resize is dead". The resize works; a row selection is dragged along
// behind it, repainting on every row crossed while the resize shows nothing
// until it commits, so what the user sees is the selection.
//
// Both sides are pinned because both are load-bearing: the resize stops the
// event, and the selection independently declines the same band.
func TestTheGutterArbitratesItsThreeGestures(t *testing.T) {
	for _, tc := range []struct{ name, expr string }{
		{"drag", rzRowDownExpr},
		{"fit", rzRowFitExpr("demo")},
	} {
		if !strings.Contains(tc.expr, "gripAt(evt)") {
			t.Errorf("the row %s does not decide the hit through the shared test: %q", tc.name, tc.expr)
		}
		if !strings.Contains(tc.expr, "evt.stopPropagation()") {
			t.Errorf("the row %s lets the gesture reach the viewport as well: %q", tc.name, tc.expr)
		}
	}

	// And the selection's own hit test declines the grip band, which is the row
	// axis's counterpart to hdrAt ignoring the column grips.
	js := selectScript()
	i := strings.Index(js, "T.rowAt=function")
	if i < 0 {
		t.Fatal("T.rowAt is gone")
	}
	body := js[i:]
	if j := strings.Index(body, "\nT."); j >= 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "T.gripAt(e)") {
		t.Errorf("row selection claims the resize grip: %s", body)
	}

	// One definition of where the grip is. Two would be two chances to disagree,
	// and disagreeing is exactly the bug above.
	if n := strings.Count(anchorScript(), "T.gripAt=function"); n != 1 {
		t.Errorf("T.gripAt is defined %d times", n)
	}
}

// RELOAD IS THE KEY SOMEBODY REACHES FOR WHEN A PAGE IS MISBEHAVING. Binding
// fill-right to Cmd+R means that recovery keystroke silently writes to their
// sheet instead — which is how this was reported: "cmd+r seems to paste/repeat".
// The Excel spelling (Ctrl) stays; the Command spelling goes.
func TestFillIsNotOnTheCommandKey(t *testing.T) {
	js := gridKeysScript()
	i := strings.Index(js, "T.key=function")
	if i < 0 {
		t.Fatal("T.key is gone")
	}
	body := js[i : i+strings.Index(js[i:], "T.edit=function")]

	fill := strings.Index(body, "{fill:'r'}")
	if fill < 0 {
		t.Fatal("fill-right is gone")
	}
	// The switch that reaches fill must be guarded on ctrlKey WITHOUT metaKey.
	guard := strings.LastIndex(body[:fill], "switch(k)")
	if guard < 0 {
		t.Fatal("no switch guards the fill actions")
	}
	line := body[strings.LastIndex(body[:guard], "\n")+1 : guard]
	if !strings.Contains(line, "e.ctrlKey") || !strings.Contains(line, "!e.metaKey") {
		t.Errorf("fill is reachable with the Command key held: %q", line)
	}

	// And nothing else may preventDefault on a chord that resolved to no action,
	// which is what actually lets the browser reload.
	if !strings.Contains(vpKeyExpr("demo"), "if(!a)return;evt.preventDefault();") {
		t.Error("the keymap swallows chords it does not handle, so reload never happens")
	}
}
