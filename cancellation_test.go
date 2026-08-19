package main

import (
	"regexp"
	"strings"
	"testing"
)

// TestEveryWriteOptsOutOfRequestCancellation.
//
// Datastar keys request cancellation to the ELEMENT, and its default is `auto`:
// a second request from an element aborts the first. For a read that is right —
// a newer viewport or selection makes the older answer worthless. For a WRITE it
// is data loss, because two writes are two operations and not a correction of
// one.
//
// The failure is invisible where it happens and surfaces somewhere else later.
// Column resize shipped without it: every resize posts from `#cols`, so dragging
// a second column aborted the first column's commit, and the abandoned column
// held its optimistic width only until `$rc` moved off it — then snapped back to
// the stale stored value. It was reported as "I resize the sixth one and one of
// the ones I resized before just changes size", which names neither the request
// nor the column that was actually lost.
//
// So the rule is checked against what the page emits rather than remembered.
func TestEveryWriteOptsOutOfRequestCancellation(t *testing.T) {
	// Shell AND a rendered window: the cell editor lives inside `#g`, so a shell
	// with an empty grid emits no /cell post at all and the scan would pass
	// vacuously.
	page := pageShell("demo", 0, 249, renderWindow(nil, 0, 249, "demo", selRange{}), zeroAnchor())

	// The verbs limits.go classifies as writes. Kept in step with classify() by
	// TestTheWriteVerbListMatchesTheLimiter below.
	writes := map[string]bool{
		"cell": true, "clear": true, "fill": true, "paste": true,
		"style": true, "colwidth": true, "rowheight": true, "rows": true, "cols": true,
	}
	// Reads, where `auto` is correct and deliberate: a newer one supersedes.
	reads := map[string]bool{"viewport": true, "sel": true}

	// `@post('/s/demo/<verb>'` followed by whatever options it was given, up to
	// the closing paren of the action.
	re := regexp.MustCompile(`@post\('/s/[^/]+/([a-z]+)'([^)]*(?:\)[^)]*)?)\)`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		verb, opts := m[1], m[2]
		seen[verb] = true
		disabled := strings.Contains(opts, "requestCancellation:'disabled'")
		switch {
		case writes[verb] && !disabled:
			t.Errorf("@post to /%s is a WRITE and does not disable request cancellation — "+
				"a second one aborts the first, silently, and the loss shows up later\n  opts: %q",
				verb, opts)
		case reads[verb] && disabled:
			t.Errorf("@post to /%s is a read; `auto` is correct for it and disabling "+
				"cancellation keeps stale requests alive", verb)
		case !writes[verb] && !reads[verb]:
			t.Errorf("@post to /%s is neither a known write nor a known read; classify it", verb)
		}
	}
	if len(seen) == 0 {
		t.Fatal("found no @post in the page shell; the scan is broken, not the page")
	}
	// The two that matter most, so a page that stopped emitting them fails loudly
	// rather than passing vacuously.
	for _, verb := range []string{"cell", "colwidth"} {
		if !seen[verb] {
			t.Errorf("the page emits no @post to /%s", verb)
		}
	}
}

// The write list above is a copy of the limiter's, so it has to be checked
// against it: a verb the limiter treats as a write but this test does not would
// be exempt from the rule for no reason anyone chose.
func TestTheWriteVerbListMatchesTheLimiter(t *testing.T) {
	for _, verb := range []string{"cell", "clear", "fill", "paste", "style", "colwidth", "rowheight", "rows", "cols"} {
		if class, _ := classify("POST", "/s/demo/"+verb); class != classWrite {
			t.Errorf("/%s is listed as a write here but the limiter classifies it as %v", verb, class)
		}
	}
	for _, verb := range []string{"viewport", "sel"} {
		if class, _ := classify("POST", "/s/demo/"+verb); class != classNav {
			t.Errorf("/%s is listed as a read here but the limiter classifies it as %v", verb, class)
		}
	}
}
