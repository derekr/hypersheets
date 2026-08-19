package main

import (
	"strconv"
	"strings"
	"testing"
)

// TestAResizedColumnDoesNotDependOnWhichColumnIsCurrent.
//
// The optimistic width used to live in `$rc`/`$rw` — one slot shared by all 26
// columns — and `--w-N` read it only while `$rc` still pointed at N. So a column
// held its new width exactly until the next column was dragged, then fell back to
// whatever the server had last said. With a round trip in the way, that is an
// earlier column visibly snapping to its old width the moment you start resizing
// another one, and snapping back a moment later.
//
// Reproduced before the fix, at 400ms of induced latency: resize column 0 to
// 136px, drag column 1, and column 0 reads 96px until the confirming push lands.
// It was reported as "I resize the sixth one and one of the ones I resized before
// just changes size" — which names neither the column that was lost nor the
// round trip that lost it.
func TestAResizedColumnDoesNotDependOnWhichColumnIsCurrent(t *testing.T) {
	expr := colWidthStyleExpr()
	if strings.Contains(expr, "$rc") {
		t.Errorf("the width style expression still reads $rc, so a column's displayed "+
			"width depends on which column was resized most recently:\n  %s", expr)
	}
	for c := 0; c < MaxCols; c++ {
		want := `'` + colVar(c) + `':$_w` + strconv.Itoa(c)
		if !strings.Contains(expr, want) {
			t.Errorf("column %d is not driven by its own width signal (want %q)", c, want)
		}
	}
}

// The other half: the commit has to write that per-column signal, or the display
// is no longer optimistic at all and every resize waits a round trip to appear.
func TestTheResizeCommitWritesTheColumnsOwnSignal(t *testing.T) {
	expr := rzUpExpr("demo")
	for c := 0; c < MaxCols; c++ {
		i := strconv.Itoa(c)
		if want := `case ` + i + `:$_w` + i + `=v.w;`; !strings.Contains(expr, want) {
			t.Errorf("the resize commit does not set column %d's own width signal (want %q)", c, want)
		}
	}
	// `$rc`/`$rw` stay, but only as the payload the POST carries.
	for _, want := range []string{"$rc=v.c", "$rw=v.w", "/colwidth"} {
		if !strings.Contains(expr, want) {
			t.Errorf("the resize commit no longer carries %q", want)
		}
	}
}

// The server clears `rc` on every width push. That is harmless now and must stay
// harmless: if the display ever reads `$rc` again, this push becomes a way for
// one viewer's width patch to blank another viewer's in-progress resize.
func TestClearingRcCannotDisturbTheDisplayedWidths(t *testing.T) {
	if strings.Contains(colWidthStyleExpr(), "$rc") {
		t.Error("patchWidths sends rc:-1, and the display reads $rc — a width push from " +
			"anyone on the sheet would reset a column mid-gesture")
	}
}
