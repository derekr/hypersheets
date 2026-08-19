package main

// window.go — what changed between two buffers.
//
// The registry diffs band sets for subscription purposes; this computes the
// equivalent diff in row space, which is the space a DOM patch is expressed in,
// so a 50-row scroll costs 50 rows on the wire instead of the whole window.
//
// The output is four ranges rather than "a direction and a distance" because a
// viewport command can move both ends at once — a window resize, or a client
// that has finally measured its own height — and a model assuming the buffer
// only slides one way corrupts the DOM the first time both ends move.

// rowRange is an inclusive row range. Hi < Lo means empty, which is the normal
// result for three of the four ranges below, so it is a value and not an error.
type rowRange struct{ Lo, Hi int }

func (r rowRange) empty() bool { return r.Hi < r.Lo }

func (r rowRange) rows() int {
	if r.empty() {
		return 0
	}
	return r.Hi - r.Lo + 1
}

// windowDiff is how to turn the buffer the client is holding into the buffer it
// should be holding.
//
// Full means "do not try": either there is no overlap (a scrollbar drag to the
// far end) or there is nothing to diff against (first paint). Sliding a window
// that shares no rows with its predecessor costs a remove of every row plus an
// append of every row — more bytes and more DOM churn than a single morph.
type windowDiff struct {
	Full     bool
	Prepend  rowRange // newly revealed ABOVE the old buffer — scrolling up
	Append   rowRange // newly revealed BELOW — scrolling down
	DropHead rowRange // fell off the top
	DropTail rowRange // fell off the bottom
}

// Rows is how many rows this diff renders. A full morph counts zero here
// because Full carries no ranges.
func (d windowDiff) Rows() int { return d.Prepend.rows() + d.Append.rows() }

// Dropped is how many rows the diff removes.
func (d windowDiff) Dropped() int { return d.DropHead.rows() + d.DropTail.rows() }

// NoOp reports a diff that neither adds nor removes anything. The viewport
// command returns early in that case, but a wake can still arrive for a window
// that has not moved.
func (d windowDiff) NoOp() bool { return !d.Full && d.Rows() == 0 && d.Dropped() == 0 }

// DropRowNums is the `remove` selector for the row numbers this diff drops,
// both ends in one string so a shrink costs one frame rather than two.
//
// It covers only the row numbers, not the cells: sparse rendering has no row
// element to remove, since cells are flat positioned children of `#b`, and only
// the server's held mask knows which of them exist. See pushWindow in http.go.
func (d windowDiff) DropRowNums() string {
	return appendSelector(
		rowNumSelector(d.DropHead.Lo, d.DropHead.Hi),
		rowNumSelector(d.DropTail.Lo, d.DropTail.Hi))
}

// diffWindow computes the four ranges for old buffer [oldLo,oldHi] -> new
// buffer [newLo,newHi]. Both ranges are inclusive and are expected to be band
// snapped, but nothing here depends on that.
func diffWindow(oldLo, oldHi, newLo, newHi int) windowDiff {
	// Degenerate or disjoint. `newLo > oldHi` also covers exact adjacency (old
	// [0,349], new [350,699] share no row): nothing to preserve, so nothing to
	// be incremental about.
	if oldHi < oldLo || newHi < newLo || newHi < oldLo || newLo > oldHi {
		return windowDiff{Full: true}
	}
	return windowDiff{
		Prepend:  rowRange{Lo: newLo, Hi: min(newHi, oldLo-1)},
		Append:   rowRange{Lo: max(newLo, oldHi+1), Hi: newHi},
		DropHead: rowRange{Lo: oldLo, Hi: min(oldHi, newLo-1)},
		DropTail: rowRange{Lo: max(oldLo, newHi+1), Hi: oldHi},
	}
}
