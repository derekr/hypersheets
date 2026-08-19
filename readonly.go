package main

// readonly.go — sheets that refuse every write.
//
// Every other sheet here is world-writable by design: the URL is the
// capability. A showcase sheet is the one case where that is wrong, because its
// value is that it still looks the same when the next person opens the link.
//
// It is process configuration rather than a per-sheet column because a stored
// `locked` bit needs a way to set it and a permission model to decide who may
// set it, which means accounts — a whole subsystem this application does not
// have. The set is small and known to whoever runs the process, so it lives
// next to the region and the caps.
//
// Enforcement is in the limiter middleware, the one place every request is
// already classified and where `classWrite` already carries the sheet id. A
// write verb added later is therefore read-only-safe by default rather than by
// remembering.
//
// Read-only implies reap-exempt, and the two settings are not independent: the
// reaper's clock is time-since-last-edit, so a sheet that refuses edits can
// never restart it and would be deleted on exactly the day it had survived
// longest. main.go puts every read-only id into the reaper's Keep set.

// readOnlySheets is the configured set. main.go writes it once before the
// server starts serving and nothing writes it afterwards, which is what makes
// an unsynchronised map safe — the same discipline `regionFlag` uses in
// latency.go.
var readOnlySheets map[string]bool

// isReadOnly answers for one sheet. A nil map answers false for everything,
// so the zero configuration is "nothing is protected".
func isReadOnly(id string) bool { return readOnlySheets[id] }

// anyReadOnly reports whether the feature is configured at all.
func anyReadOnly() bool { return len(readOnlySheets) > 0 }

// readOnlyRefusal is what a refused write says. It names the sheet, not the
// reader: there is no account here to lack a permission, so "you do not have
// permission" would be false and would send the reader looking for a sign-in
// that does not exist.
const readOnlyRefusal = "This sheet is read-only — it is a demo somebody set up " +
	"to stay the way it is. Make your own sheet from the front page and it will " +
	"behave normally."

// readOnlyChipNote is the one-line version for the status chip; the header
// badge already carries the long form in its title attribute.
//
// It must contain no apostrophe and no backslash: it is interpolated into a
// single-quoted JavaScript string inside a Go backtick literal, where neither
// language would let an escape through. The em dash is there for that reason.
const readOnlyChipNote = "This sheet is read-only — make your own from the front page."

// readOnlyBadgeHTML marks the sheet before anything is refused. A refusal the
// reader could not have predicted reads as a bug.
//
// It is page-shell markup, so no push re-sends it, no morph removes it, and it
// costs nothing per cell — the rule every overlay in render.go follows.
func readOnlyBadgeHTML(sheetID string) string {
	if !isReadOnly(sheetID) {
		return ""
	}
	return `<span class="m ro" title="` + readOnlyRefusal + `">read-only</span>`
}
