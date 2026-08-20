package main

import (
	"strings"
	"testing"
)

// FIT MEASURES WHAT THE ROW WOULD NEED, NOT WHAT IT IS GIVEN. scrollHeight
// reports at least the height of the box it is read from, so measuring in place
// can only ever grow a row: a row dragged to 200px would measure 200 forever and
// fitting it would be a no-op. The height has to be released first, and put back
// whether or not the measurement succeeded.
func TestFitReleasesTheRowHeightBeforeMeasuringIt(t *testing.T) {
	js := anchorScript()
	i := strings.Index(js, "T.fitR=function")
	if i < 0 {
		t.Fatal("T.fitR is gone")
	}
	body := js[i:]
	if j := strings.Index(body, "\nT."); j >= 0 {
		body = body[:j]
	}
	if strings.Contains(body, "scrollHeight") {
		t.Errorf("T.fitR measures in place, so it can only grow a row: %s", body)
	}
	for _, want := range []string{
		"classList.add('" + measureClass + "')",
		"offsetHeight",
		"classList.remove('" + measureClass + "')",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("T.fitR is missing %q: %s", want, body)
		}
	}
	// The release is a class and not an inline height, because the row level of
	// the cascade matches on the literal style attribute.
	if strings.Contains(body, ".style.height") {
		t.Error("T.fitR writes the style attribute, which is what the row cascade matches on")
	}
	if rule := `#` + bufferID + ` b.` + measureClass + `{height:auto}`; !strings.Contains(gridCSS, rule) {
		t.Errorf("the measuring class has no rule: want %q", rule)
	}
}

// Fitting is a resize. There is no "automatic" height in the store — the client
// measures, and what it sends is an ordinary number through the ordinary
// command, so a fitted row and a dragged one are the same fact afterwards.
func TestFittingCommitsThroughTheResizeCommand(t *testing.T) {
	fit := rowFitExpr("demo")
	if !strings.Contains(fit, "/s/demo/rowheight") {
		t.Errorf("fit does not post to the resize endpoint: %q", fit)
	}
	if !strings.Contains(fit, "$rr=") || !strings.Contains(fit, "$rh=") {
		t.Errorf("fit does not fill the resize signals: %q", fit)
	}
	// Two fits are two operations, exactly as two drags are.
	if !strings.Contains(fit, "requestCancellation:'disabled'") {
		t.Errorf("a fit can be cancelled by the next one: %q", fit)
	}
	// It shares the viewport's double-click with the cell editor, and answers
	// first — a double-click on the grip must not also open a cell.
	dbl := vpDblClickExpr("demo")
	if i, j := strings.Index(dbl, "fitR("), strings.Index(dbl, "$editing=true"); i < 0 || j < 0 || i > j {
		t.Errorf("a double-click on the row grip also opens the editor: %q", dbl)
	}
}
