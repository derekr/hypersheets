package main

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// bandkey.go — storage keys, so that a row's position is not its identity.
//
// A cell's primary key is a storage key, `k`, not its (row, col) coordinate:
// keying on the coordinate makes an insert at the top rewrite the clustered
// b-tree for every row below it, at a cost linear in the cells moved. A storage
// band owns a contiguous, deliberately sparse interval of the key space and
// records how many display rows it holds, so
//
//	rank(k) = (rows in every earlier band) + (k - base of its band)
//
// and an insert renumbers the rows below it without writing to them: slots
// shift inside one band, and that band's row count absorbs the rest.
//
// Everything rests on one property — band bases ascend with display order and
// slots ascend within a band — so a contiguous run of ranks is a contiguous run
// of keys, and the hot read path (`WHERE k BETWEEN ? AND ?`) stays a single
// ordered primary-key range scan with no sort step (TestHotPathQueryPlans).
//
// References are stored as keys, not ranks, which is what bounds a write rather
// than merely cheapening it: a formula pointing at a row that did not move needs
// no write however far its rank shifted. Only references pointing into the moved
// key range are rewritten, and partial indexes find them.
//
// A storage band is not a display band. `BandOf(row) = row/50` (grid.go) is the
// invalidation and subscription unit, computed from display rows; these are an
// allocation unit that drifts out of alignment and splits, and nothing above the
// store ever sees one.

const (
	// keyStride is the default width of a storage band's key slice. A band
	// normally carries BandHeight (50) display rows, so 4,096 slots leaves room
	// for ~4,046 consecutive inserts into one band before it has to be given
	// more space.
	keyStride = 4096

	// bandSplitRows is when a band is split in two. It is not about running out
	// of slots but about bounding the shift: an insert moves at most the rows
	// below it inside its own band, so capping a band at 100 rows caps a
	// structural mutation at ~2,600 moved cells whatever else the sheet does.
	bandSplitRows = 2 * BandHeight

	// minSplitSlots is the narrowest interval still worth splitting. A split
	// halves the interval it divides, so a band that has already been split
	// several times eventually cannot give both halves room for bandSplitRows
	// rows; below this width the answer is a rebalance instead.
	minSplitSlots = 2 * bandSplitRows

	// deadKey is the stored reference key of a destroyed reference — the
	// key-space spelling of grid.go's off-grid CellRef{-1,-1}, which renders as
	// #REF!. It is negative so that every `BETWEEN lo AND hi` predicate in the
	// mutation path ignores it, and it is not NULL, because NULL means "this
	// formula has no second operand" and confusing the two turns `=#REF!+A8`
	// into `=A8`.
	deadKey = int64(-1)
)

// keyBand is one storage band: an interval of the key space, and how many
// display rows currently live in it. Rows occupy slots [base, base+nrows) with
// no holes; [base+nrows, base+slots) is the slack an insert consumes.
type keyBand struct {
	base  int64
	slots int64
	nrows int
}

func (b keyBand) end() int64  { return b.base + b.slots }
func (b keyBand) last() int64 { return b.base + int64(b.nrows) - 1 }
func (b keyBand) free() int   { return int(b.slots) - b.nrows }

// bandIndex is the whole rank<->key mapping: the bands, plus their prefix sum.
//
// It is immutable once installed on a *Sheet. A mutation builds a clone and
// commits the transaction and the clone together, so readers hold whichever one
// the lock handed them and no window read translates post-commit keys with
// pre-commit counts. The prefix sum is a linear array and a binary search rather
// than a Fenwick tree: a couple of hundred counters are immaterial against the
// mutation they accompany.
type bandIndex struct {
	bands  []keyBand
	prefix []int // exclusive prefix sum of nrows

	// rows is the sheet's row extent, and the only place it lives. It is
	// durable (the sum of bands.nrows on disk) and snapshot-consistent (the
	// index is immutable and installed under idxMu with the commit that
	// produced it). There is no compile-time row limit below RowCeiling.
	rows int
}

// newBandIndex takes ownership of bands and computes the derived sums.
func newBandIndex(bands []keyBand) *bandIndex {
	bi := &bandIndex{bands: bands}
	bi.reindex()
	return bi
}

// canonicalBands is how many bands the canonical layout of an n-row sheet has.
func canonicalBands(rows int) int {
	n := (rows + BandHeight - 1) / BandHeight
	if n < 1 {
		n = 1
	}
	return n
}

// maxKeyBandsFor caps how many storage bands a sheet of this extent may
// accumulate before it is rebalanced back to the canonical layout: splits add
// bands, and without a ceiling a long-lived sheet grows an index every read has
// to search. The cap tracks the extent because the canonical band count does — a
// 1,000,000-row sheet has 20,000 bands when perfectly balanced, so a fixed
// ceiling would call a healthy sheet crowded and force a full re-key, the one
// O(sheet) operation in the system, on every delete.
func maxKeyBandsFor(rows int) int { return 4 * canonicalBands(rows) }

// canonicalBandIndex is the layout a fresh (or rebalanced) sheet has: one band
// per BandHeight display rows, evenly spaced, every band with the same slack.
func canonicalBandIndex(stride int64) *bandIndex { return layoutFor(DefaultRows, stride) }

// layoutFor is the canonical layout for a sheet of any extent. A mid-insert
// rebalance needs that, because the rows the insert pushes off the bottom are
// already gone and the rows it adds are not there yet; so does a grown sheet,
// whose extent is whatever it has grown to.
func layoutFor(rows int, stride int64) *bandIndex {
	if stride < keyStride {
		stride = keyStride
	}
	n := canonicalBands(rows)
	bands := make([]keyBand, n)
	left := rows
	for i := range bands {
		rows := BandHeight
		if rows > left {
			rows = left
		}
		left -= rows
		bands[i] = keyBand{base: int64(i) * stride, slots: stride, nrows: rows}
	}
	return newBandIndex(bands)
}

func (b *bandIndex) reindex() {
	if cap(b.prefix) >= len(b.bands) {
		b.prefix = b.prefix[:len(b.bands)]
	} else {
		b.prefix = make([]int, len(b.bands))
	}
	n := 0
	for i, band := range b.bands {
		b.prefix[i] = n
		n += band.nrows
	}
	b.rows = n
}

func (b *bandIndex) clone() *bandIndex {
	bands := make([]keyBand, len(b.bands))
	copy(bands, b.bands)
	return newBandIndex(bands)
}

// locate maps a display rank to (band, slot). A rank at or past the end comes
// back as the append position of the last band, which is where a mutation
// working at the very bottom of the grid wants to land.
func (b *bandIndex) locate(rank int) (int, int) {
	if rank < 0 {
		return 0, 0
	}
	if rank >= b.rows {
		last := len(b.bands) - 1
		return last, b.bands[last].nrows
	}
	// Bands holding zero rows share a prefix with their neighbour, so search
	// for the last band with prefix <= rank and nrows > 0.
	i := sort.Search(len(b.bands), func(i int) bool {
		return b.prefix[i]+b.bands[i].nrows > rank
	})
	if i >= len(b.bands) {
		i = len(b.bands) - 1
	}
	return i, rank - b.prefix[i]
}

// keyOf is the storage key of a display rank. An out-of-grid rank returns
// deadKey, which the cells table's CHECK constraint rejects — so a bad ref
// cannot be silently written at a valid key.
func (b *bandIndex) keyOf(rank int) int64 {
	if rank < 0 || rank >= b.rows {
		return deadKey
	}
	i, slot := b.locate(rank)
	return b.bands[i].base + int64(slot)
}

// keyAt is keyOf extended by one position: a rank of exactly b.rows — the append
// position, where an insert at the bottom of the sheet works — returns the first
// slot past the last row instead of deadKey.
//
// deadKey is negative, so a probe written as `k >= keyOf(at)` at the append
// position would match the entire table rather than nothing, and an append would
// report every band in the sheet dirty. Only that class of half-open bound wants
// this; keyOf is still what names an actual row.
func (b *bandIndex) keyAt(rank int) int64 {
	if rank >= b.rows {
		last := b.bands[len(b.bands)-1]
		return last.base + int64(last.nrows)
	}
	return b.keyOf(rank)
}

// bandOfKey is the band whose interval contains k, or -1.
func (b *bandIndex) bandOfKey(k int64) int {
	if k < 0 || len(b.bands) == 0 || k < b.bands[0].base {
		return -1
	}
	i := sort.Search(len(b.bands), func(i int) bool {
		return b.bands[i].base > k
	}) - 1
	if i < 0 || k >= b.bands[i].end() {
		return -1
	}
	return i
}

// rankOf is the display rank of a storage key, or -1 if the key names no row.
// The read path uses rankScan instead, which walks bands in step with the
// ordered scan; the reference columns are random access and need this.
func (b *bandIndex) rankOf(k int64) int {
	i := b.bandOfKey(k)
	if i < 0 {
		return -1
	}
	off := k - b.bands[i].base
	if off >= int64(b.bands[i].nrows) {
		return -1 // in the band's slack: a key that names no display row
	}
	return b.prefix[i] + int(off)
}

// rankScan converts keys to ranks for a scan that returns them in ascending
// order — the window read. It is a cursor over the bands rather than a search
// per row, so a whole window costs one pass over the band list.
type rankScan struct {
	bi *bandIndex
	i  int
}

func (b *bandIndex) scanner() *rankScan { return &rankScan{bi: b} }

// rank must be called with non-decreasing keys.
func (s *rankScan) rank(k int64) int {
	bands := s.bi.bands
	for s.i < len(bands) && k >= bands[s.i].base+int64(bands[s.i].nrows) {
		s.i++
	}
	if s.i >= len(bands) || k < bands[s.i].base {
		return -1
	}
	return s.bi.prefix[s.i] + int(k-bands[s.i].base)
}

// ─── Invariants ───────────────────────────────────────────────────────────────

// validate checks every property the rank<->key mapping depends on. A violation
// here is the class of bug that surfaces much later as a window silently
// rendering the wrong rows, so the tests call it after every mutation.
func (b *bandIndex) validate() error {
	if len(b.bands) == 0 {
		return fmt.Errorf("band index has no bands")
	}
	var prevEnd int64 = -1
	for i, band := range b.bands {
		if band.slots <= 0 {
			return fmt.Errorf("band %d has %d slots", i, band.slots)
		}
		if band.nrows < 0 || int64(band.nrows) > band.slots {
			return fmt.Errorf("band %d holds %d rows in %d slots", i, band.nrows, band.slots)
		}
		if band.base <= prevEnd {
			return fmt.Errorf("band %d base %d overlaps the previous band ending at %d",
				i, band.base, prevEnd)
		}
		prevEnd = band.end() - 1
	}
	// The extent is bounded, not fixed: zero rows would make locate/keyOf
	// meaningless and anything past RowCeiling is a corrupt file or a bug.
	// Both must fail to open rather than open wrong.
	if b.rows < 1 || b.rows > RowCeiling {
		return fmt.Errorf("band index holds %d display rows, want 1..%d", b.rows, RowCeiling)
	}
	return nil
}

// ─── Reshaping ────────────────────────────────────────────────────────────────

// splitAt divides band i in two at its midpoint, moving the upper half of its
// rows into the upper half of its own key interval. It returns the key range
// that has to move and by how much; an empty range means the split was pure
// bookkeeping. The new band takes the upper half, so band bases still ascend and
// the read path is untouched. Each split halves the interval it divides, which
// is why minSplitSlots exists and why a sheet hammered at one position
// eventually rebalances instead.
func (b *bandIndex) splitAt(i int) (lo, hi, delta int64) {
	band := b.bands[i]
	m := band.nrows / 2
	half := band.slots / 2
	lower := keyBand{base: band.base, slots: half, nrows: m}
	upper := keyBand{base: band.base + half, slots: band.slots - half, nrows: band.nrows - m}

	lo, hi = band.base+int64(m), band.last()
	delta = upper.base - lo
	if upper.nrows == 0 {
		lo, hi, delta = 0, -1, 0
	}

	b.bands = append(b.bands, keyBand{})
	copy(b.bands[i+2:], b.bands[i+1:])
	b.bands[i], b.bands[i+1] = lower, upper
	b.reindex()
	return lo, hi, delta
}

// canSplit reports whether splitting band i would leave both halves able to
// hold their rows with room to work in.
func (b *bandIndex) canSplit(i int) bool {
	band := b.bands[i]
	if band.slots < minSplitSlots || band.nrows < 2 {
		return false
	}
	half := band.slots / 2
	m := band.nrows / 2
	return int64(m) <= half && int64(band.nrows-m) <= band.slots-half
}

// growTo extends the sheet's row extent to at least `rows`, laying the new rows
// out the way a fresh sheet is: BandHeight rows to a band, each with a full
// keyStride of slack. It reports whether anything changed, and costs no cell
// writes because a blank row is slack.
//
// It is deliberately not appendTail. appendTail gives back rows a delete just
// took, so it refills slack that is already there and must not change the band
// shape; growTo creates rows that never existed and wants the canonical shape,
// because a band arriving with 4,000 rows would make every future insert into it
// move 4,000 rows — the cost bandSplitRows exists to bound.
func (b *bandIndex) growTo(rows int) bool {
	if rows <= b.rows {
		return false
	}
	n := rows - b.rows
	for n > 0 {
		last := &b.bands[len(b.bands)-1]
		if room := min(last.free(), BandHeight-last.nrows); room > 0 {
			take := min(room, n)
			last.nrows += take
			n -= take
			continue
		}
		b.bands = append(b.bands, keyBand{base: last.end(), slots: keyStride})
	}
	b.reindex()
	return true
}

// appendTail adds n blank display rows at the bottom of the grid — what a row
// delete does so that deleting a row does not shorten the sheet. Blank rows are
// slack, so it costs no cell writes. See growTo for why extension does not come
// through here.
func (b *bandIndex) appendTail(n int) {
	for n > 0 {
		last := &b.bands[len(b.bands)-1]
		if room := last.free(); room > 0 {
			take := min(room, n)
			last.nrows += take
			n -= take
			continue
		}
		b.bands = append(b.bands, keyBand{base: last.end(), slots: keyStride})
	}
	b.reindex()
}

// dropTail removes the last n display rows and returns the key ranges whose
// cells have to be deleted with them — what a row insert does to the bottom of
// the grid when those rows are blank. An insert whose tail rows hold content
// extends the sheet instead and never calls this; see rowInsertGrows in
// mutate.go.
func (b *bandIndex) dropTail(n int) []keyRange {
	var out []keyRange
	for n > 0 {
		i := len(b.bands) - 1
		for i > 0 && b.bands[i].nrows == 0 {
			i--
		}
		band := &b.bands[i]
		take := min(band.nrows, n)
		if take == 0 {
			break
		}
		out = append(out, keyRange{lo: band.base + int64(band.nrows-take), hi: band.last()})
		band.nrows -= take
		n -= take
	}
	b.reindex()
	return out
}

// keyRange is an inclusive run of storage keys.
type keyRange struct{ lo, hi int64 }

func (r keyRange) empty() bool { return r.hi < r.lo }

// ─── Persistence ──────────────────────────────────────────────────────────────

const bandsDDL = `
CREATE TABLE IF NOT EXISTS bands (
  idx   INTEGER PRIMARY KEY,
  base  INTEGER NOT NULL,
  slots INTEGER NOT NULL,
  nrows INTEGER NOT NULL
) WITHOUT ROWID;
`

// ─── The row-keyed side table ─────────────────────────────────────────────────
//
// `rows` is per-row state keyed by the storage key, not the display rank — the
// same choice `cells` makes, so a row that does not move needs no write however
// far its rank shifts. What is left is exactly the three moments a key changes:
// a run of keys shifts inside a band, ceases to exist, or is re-keyed by a
// rebalance. Each already has a primitive here that this table rides along on.
//
// `height` is declared alongside `style` although nothing populates it yet: same
// shape, same key, same three primitives, so adding it later would cost a schema
// version for one integer.
//
// The table is sparse the way `cols` is: an absent key means "default style,
// default height", and resetting a row to both defaults deletes it rather than
// storing zeroes. That keeps an unformatted sheet at zero rows here and bounds
// the garbage collector's sweep.
const rowMetaDDL = `
CREATE TABLE IF NOT EXISTS rows (
  k      INTEGER NOT NULL PRIMARY KEY CHECK (k >= 0),
  style  INTEGER NOT NULL DEFAULT 0,
  height INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
`

// dropRowMeta removes the per-row state of a run of keys that ceases to exist.
// Called from where the cells of those keys are deleted: it is the same event.
func dropRowMeta(tx *sql.Tx, r keyRange) error {
	if r.empty() {
		return nil
	}
	if _, err := tx.Exec(fmt.Sprintf(
		`DELETE FROM rows WHERE k BETWEEN %d AND %d`, r.lo, r.hi)); err != nil {
		return fmt.Errorf("drop row state %d..%d: %w", r.lo, r.hi, err)
	}
	return nil
}

// shiftRowMeta moves a contiguous run of per-row state by delta. It is called
// from shiftKeyRange — the one primitive every row mutation is made of — so an
// insert, a delete and a band split all carry it without knowing this table
// exists.
//
// Copy / delete / re-insert for the same reason the cells shift is: `k` is the
// primary key of a WITHOUT ROWID table, so an in-place `SET k = k + 1` collides
// with the row it is about to move. The scratch table is separate from mut_cells
// because the cardinalities differ by the column count — at most one row per
// display row in the band, against 26 cells for each of them.
func shiftRowMeta(tx *sql.Tx, r keyRange, delta int64) error {
	if r.empty() || delta == 0 {
		return nil
	}
	// Probe first: the copy below is four statements including a temp-table
	// create and drop, and the common case is a sheet with no per-row state at
	// all, where all four would move zero rows. One indexed seek costs less than
	// the create alone.
	var one int64
	switch err := tx.QueryRow(fmt.Sprintf(
		`SELECT k FROM rows WHERE k BETWEEN %d AND %d LIMIT 1`, r.lo, r.hi)).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("probe row state %d..%d: %w", r.lo, r.hi, err)
	}
	if err := scratch(tx, "mut_rows", fmt.Sprintf(
		`SELECT k + %d AS nk, style, height FROM rows WHERE k BETWEEN %d AND %d`,
		delta, r.lo, r.hi)); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(
		`DELETE FROM rows WHERE k BETWEEN %d AND %d`, r.lo, r.hi)); err != nil {
		return fmt.Errorf("clear shifted row state %d..%d: %w", r.lo, r.hi, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO rows (k, style, height) SELECT nk, style, height FROM temp.mut_rows`,
	); err != nil {
		return fmt.Errorf("re-insert shifted row state %d..%d: %w", r.lo, r.hi, err)
	}
	if _, err := tx.Exec(`DROP TABLE temp.mut_rows`); err != nil {
		return fmt.Errorf("drop row scratch table: %w", err)
	}
	return nil
}

// rekeyRowMeta re-keys the whole table through the rebalance's rank map, which
// is the third and last moment a key changes. A row whose key is not in the map
// named no display rank and is dropped, which is the same rule the cells arm
// applies — except that a cell so filed is an error there (it is counted and
// refused) and here it is simply state for a row that no longer exists.
func rekeyRowMeta(tx *sql.Tx) error {
	if err := scratch(tx, "rebal_rows",
		`SELECT m.new_k AS k, r.style, r.height
		   FROM rows r JOIN temp.kmap m ON m.old_k = r.k`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM rows`); err != nil {
		return fmt.Errorf("clear row state for rebalance: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO rows (k, style, height) SELECT k, style, height FROM temp.rebal_rows`,
	); err != nil {
		return fmt.Errorf("re-key row state: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE temp.rebal_rows`); err != nil {
		return fmt.Errorf("drop row rebalance scratch: %w", err)
	}
	return nil
}

// queryer is the read surface shared by *sql.DB and *sql.Tx, so the band index
// can be loaded either on open or inside a migration.
type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func loadBandIndex(q queryer) (*bandIndex, error) {
	rows, err := q.Query(`SELECT base, slots, nrows FROM bands ORDER BY idx`)
	if err != nil {
		return nil, fmt.Errorf("read bands: %w", err)
	}
	defer rows.Close()
	var bands []keyBand
	for rows.Next() {
		var b keyBand
		if err := rows.Scan(&b.base, &b.slots, &b.nrows); err != nil {
			return nil, fmt.Errorf("scan band: %w", err)
		}
		bands = append(bands, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read bands: %w", err)
	}
	if len(bands) == 0 {
		return nil, nil
	}
	return newBandIndex(bands), nil
}

// writeBandIndex replaces the stored band table wholesale. Used when the shape
// changes (a split, a rebalance) and when a sheet is created.
func writeBandIndex(tx *sql.Tx, bi *bandIndex) error {
	if _, err := tx.Exec(`DELETE FROM bands`); err != nil {
		return fmt.Errorf("clear bands: %w", err)
	}
	return bulkInsert(tx, len(bi.bands), 4,
		`INSERT INTO bands (idx, base, slots, nrows) VALUES `, ``,
		func(i int, args []any) []any {
			b := bi.bands[i]
			return append(args, i, b.base, b.slots, b.nrows)
		})
}

// syncBandIndex persists the difference between two band layouts. The common
// case — one band's row count changed — is a single UPDATE, which is what keeps
// the bookkeeping off the cost of a mutation.
func syncBandIndex(tx *sql.Tx, old, next *bandIndex) error {
	if old == nil || len(old.bands) != len(next.bands) {
		return writeBandIndex(tx, next)
	}
	for i, b := range next.bands {
		if old.bands[i] == b {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE bands SET base = ?, slots = ?, nrows = ? WHERE idx = ?`,
			b.base, b.slots, b.nrows, i); err != nil {
			return fmt.Errorf("update band %d: %w", i, err)
		}
	}
	return nil
}
