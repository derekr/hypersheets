package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// store.go — one SQLite file per sheet: the schema, and the handle that owns it.

// ─── Storage model ────────────────────────────────────────────────────────────
//
// One SQLite file per sheet. The event log lives in the same file as the cells
// so a mutation and the event describing it commit in a single transaction —
// an atomicity commitment from SPEC.md. No code path writes a cell without
// writing its event.
//
// WAL + synchronous=NORMAL means readers never block the writer and the writer
// never blocks readers, which is what lets the windowed render (the hot read
// path, run on every push) bypass the per-sheet actor entirely. See actor.go.

var (
	// ErrSheetClosed is returned when a *Sheet handle is used after the LRU
	// evicted and closed it. Callers hold a handle for the duration of one
	// operation and re-Open on the next; the fix on seeing this is to call
	// OpenSheet again, not to retry the method.
	ErrSheetClosed = errors.New("sheet handle closed (evicted)")

	// ErrSheetDeleting is returned by Open for the few milliseconds a sheet's
	// files are being unlinked by the reaper. It is a transient refusal, not a
	// missing sheet: the next request opens a fresh empty sheet at the same id.
	ErrSheetDeleting = errors.New("sheet is being deleted")

	// ErrBadSheetID rejects ids that are not safe as a filename.
	ErrBadSheetID = errors.New("bad sheet id")
)

// Kind classifies what a cell holds. Stored as an INTEGER so the renderer can
// switch on it without re-parsing raw.
type Kind int

const (
	KindEmpty   Kind = 0
	KindNumber  Kind = 1
	KindText    Kind = 2
	KindFormula Kind = 3
	// KindError marks a formula whose evaluation failed; computed holds the
	// error token ("#CYCLE!", "#ERR!") and raw still holds the formula.
	KindError Kind = 4
)

func (k Kind) String() string {
	switch k {
	case KindEmpty:
		return "empty"
	case KindNumber:
		return "number"
	case KindText:
		return "text"
	case KindFormula:
		return "formula"
	case KindError:
		return "error"
	}
	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// Cell is one stored cell. The three texts and the two style ids are each
// distinct, and the differences are load-bearing:
//
//	Raw        what was typed. Always A1 display text: a formula is stored as
//	           a template plus reference columns (see FormulaTemplate) and
//	           every read path materializes Raw from the two, so nothing above
//	           the store sees a template.
//	Computed   the machine value — what SUM adds, what the event log records,
//	           what the golden fixtures hash. Never formatted.
//	Display    Computed with the cell's effective number format applied. Equal
//	           to Computed for any cell with no number format.
//	Style      the cell's own record, 0 for a cell nobody styled individually.
//	           This is what the render layer emits per cell.
//	Effective  the resolved look: cell ?: row ?: column ?: default (style.go's
//	           resolve). What Display was formatted with, and the answer to
//	           "what does this cell look like" for anything computed
//	           server-side — toolbar state, a copy, an export.
//
// The renderer must not emit a class for Effective: it also emits the column
// and row rules the cascade expresses in O(config) CSS, so a class per cell
// would apply every level twice.
type Cell struct {
	Ref       CellRef
	Raw       string
	Computed  string
	Display   string
	Kind      Kind
	Style     int
	Effective int
}

// maxSlots is how many structural references one cell can hold. The formula
// language tops out at two (`=A1+B2`, `=SUM(A1:A10)`), so references fit in
// fixed columns on the cells row rather than needing a table of their own, and
// ride along on a row shift for free.
const maxSlots = 2

// slotCols are the reference columns, in the order every query lists them. The
// row component of a reference is a storage key, not a display row (bandkey.go),
// which is what bounds a structural mutation: a reference to a row that did not
// move needs no write, however far its display rank shifted.
const slotCols = `ref0_k, ref0_col, ref1_k, ref1_col`

// nullSlots is the scan target for the slot columns of one row, holding them
// exactly as stored: reference keys.
type nullSlots [2 * maxSlots]sql.NullInt64

func (n *nullSlots) scanArgs() []any {
	return []any{&n[0], &n[1], &n[2], &n[3]}
}

// refs turns scanned slot columns into the display-space slice the template
// renderer wants. An absent slot ends the list; a destroyed one (deadKey) comes
// back off-grid, which is how CellRef.String renders it as #REF!.
func (n *nullSlots) refs(bi *bandIndex) []CellRef {
	out := make([]CellRef, 0, maxSlots)
	for i := 0; i < maxSlots; i++ {
		k, c := n[2*i], n[2*i+1]
		if !k.Valid {
			break
		}
		out = append(out, CellRef{Row: bi.rankOf(k.Int64), Col: int(c.Int64)})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// slotArgs renders slots as the four bind values of an INSERT, mapping each
// reference's display row to its storage key. Absent slots bind NULL, which is
// what makes "this cell has structural references" a plain `ref0_k IS NOT NULL`.
func slotArgs(bi *bandIndex, slots []CellRef) [2 * maxSlots]any {
	var out [2 * maxSlots]any
	for i := 0; i < maxSlots; i++ {
		if i < len(slots) {
			out[2*i], out[2*i+1] = bi.keyOf(slots[i].Row), slots[i].Col
		}
	}
	return out
}

// ComputedCell carries one recalc result from the formula engine into the
// write transaction. It updates computed+kind only — never raw, because the
// engine never changes what the user typed.
type ComputedCell struct {
	Ref      CellRef
	Computed string
	Kind     Kind
}

// Event is one row of the in-database log.
type Event struct {
	Seq  int64
	TS   time.Time
	Ref  string // A1 notation
	Raw  string // what was written
	Prev string // what was there before
}

// schemaVersion stamps the on-disk schema. Each choice below is load-bearing
// for a specific access pattern:
//
//   - cells is WITHOUT ROWID with PK (k, col), where k is a storage key from
//     the band index (bandkey.go), not a display row. The table is a b-tree
//     clustered on (k, col), so the windowed read `WHERE k BETWEEN ? AND ?` is
//     one contiguous range scan that emerges already ordered, while an insert
//     does not have to renumber every row below it. Band bases ascend with
//     display order, so a run of display ranks is a run of keys.
//   - the CHECK constraints are the last line of defence keeping a bad ref out
//     of the file, and are what makes a mid-transaction failure — and so the
//     rollback path — reachable and testable.
//   - cells carries its own formula's references in fixed columns (ref0_*,
//     ref1_*, ref_span) rather than in a table of their own: a structural
//     mutation already rewrites every cells row it moves, so mapping the
//     references in the same statement is free where a separate table would be
//     a second b-tree to shift. ref_span marks the two slots as the endpoints
//     of a range, which makes expand/shrink expressible without storing the
//     cells between them, and "who reads this cell" is answerable from those
//     columns directly (indexDDL), so there is no `deps` edge table.
//   - rows are not capped here and must not be. The row extent is the sum of
//     bands.nrows (bandkey.go); the only row bound is RowCeiling, enforced
//     where a mutation can grow the sheet, because a CHECK would have to be
//     written in terms of the band layout and would be wrong the moment a
//     rebalance changed the stride. The CHECK that remains keeps a key naming
//     no row (the deadKey a broken reference binds) out of the PK.
//   - style is an index into the sheet's style table. It is a column on cells
//     rather than a side table because the window read is the hot path and a
//     side table makes it a join (DATA-MODEL.md). cols carries the column
//     level of the same cascade beside the column width, and `rows` the row
//     level keyed by storage key (rowMetaDDL), so a row style survives a
//     structural mutation the way a cell does.
//
// cells.style and cols.style are last in their column lists because a migrated
// file gains them by ALTER TABLE ADD COLUMN, which appends; declaring them
// elsewhere would give a migrated and a fresh file different column orders for
// the same schema version.
const schemaVersion = 8

// styleColDDL is the exact column definition, shared by the CREATE TABLE below
// and by the v5 -> v6 ALTER, so a migrated file and a fresh one cannot disagree
// about the column they both claim to have.
const styleColDDL = `style INTEGER NOT NULL DEFAULT 0`

// colStyleColDDL is the same thing for `cols`. It is a separate constant so the
// two ALTERs and the two CREATEs cannot be confused for each other: they name
// different tables and are stamped by different versions.
const colStyleColDDL = `style INTEGER NOT NULL DEFAULT 0`

const schemaDDL = `
CREATE TABLE IF NOT EXISTS cells (
  k        INTEGER NOT NULL CHECK (k >= 0),
  col      INTEGER NOT NULL CHECK (col >= 0 AND col < 26),
  raw      TEXT    NOT NULL DEFAULT '',
  computed TEXT    NOT NULL DEFAULT '',
  kind     INTEGER NOT NULL DEFAULT 0,
  ref0_k   INTEGER,
  ref0_col INTEGER,
  ref1_k   INTEGER,
  ref1_col INTEGER,
  ref_span INTEGER NOT NULL DEFAULT 0,
  ` + styleColDDL + `,
  PRIMARY KEY (k, col)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS events (
  seq  INTEGER PRIMARY KEY AUTOINCREMENT,
  ts   INTEGER NOT NULL,
  ref  TEXT    NOT NULL,
  raw  TEXT    NOT NULL,
  prev TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS cols (
  col   INTEGER PRIMARY KEY CHECK (col >= 0 AND col < 26),
  width INTEGER NOT NULL CHECK (width >= 24 AND width <= 600),
  ` + colStyleColDDL + `
) WITHOUT ROWID;
` + bandsDDL + stylesDDL + rowMetaDDL

// indexDDL is the reverse dependency lookup — "which cells read this cell" —
// expressed as indexes over the reference columns instead of as a table.
//
// All of them are partial, which is why this works at all: a sheet is mostly
// literals, so an unfiltered index on ref0_k would carry a NULL entry per
// literal and cost more to maintain during a row shift than the edge table it
// replaces. The predicates are repeated verbatim in every query below, because
// SQLite only uses a partial index when the query's WHERE clause visibly
// implies the index's predicate.
//
// Ranges are why there are four reference indexes and not two. `=SUM(A1:A10)`
// reads ten cells but stores two endpoints, so "which ranges contain A5" is an
// interval query with two inequalities and an index can drive on only one.
// Exploding ranges back into rows would be the edge table again, so spans —
// which are rare — get indexes of their own, leaving a leftover scan over
// ranges and never over the sheet. The two let the caller pick the nearer edge
// of the grid: `ref0_k <= R` visits every range starting above R, cheap near
// the top and worst at the bottom, and `ref1_k >= R` is the other way round, so
// dependentsSQLFor's choice bounds the scan by min(R, extent-R).
//
// cells_style and rows_style serve the style garbage collector (gcStyles),
// which would otherwise scan all of cells to answer a question about tens of
// rows; they are in style order so its DISTINCT does not grow a temp b-tree.
// There is no cols index: at most 26 rows.
//
// All of these are created after migrate() rather than with the tables, since
// a version 1 file does not have the columns they index until then. They also
// answer "which references point into the key range this mutation moves"
// (`ref0_k BETWEEN ? AND ?`), bounding a structural mutation by the number of
// references into one band rather than by the size of the table.
const indexDDL = `
CREATE INDEX IF NOT EXISTS cells_ref0 ON cells (ref0_k, ref0_col)
  WHERE ref_span = 0 AND ref0_k IS NOT NULL;
CREATE INDEX IF NOT EXISTS cells_ref1 ON cells (ref1_k, ref1_col)
  WHERE ref_span = 0 AND ref1_k IS NOT NULL;
CREATE INDEX IF NOT EXISTS cells_span_lo ON cells (ref0_k, ref1_k, ref0_col, ref1_col)
  WHERE ref_span = 1;
CREATE INDEX IF NOT EXISTS cells_span_hi ON cells (ref1_k, ref0_k, ref0_col, ref1_col)
  WHERE ref_span = 1;
CREATE INDEX IF NOT EXISTS cells_style ON cells (style)
  WHERE style <> 0;
CREATE INDEX IF NOT EXISTS rows_style ON rows (style)
  WHERE style <> 0;
`

// ─── Sheet handle ─────────────────────────────────────────────────────────────

// Sheet is an open handle to one sheet's database. Obtain one from
// OpenSheet/SheetCache.Open; do not construct directly and do not retain one
// across operations (the LRU may evict it — see ErrSheetClosed).
type Sheet struct {
	ID   string
	path string

	// mu guards the handle's lifetime, not its data: RLock for any use of db,
	// Lock only to close. SQLite does its own concurrency control, so this is
	// purely "don't close the db out from under an in-flight query."
	mu     sync.RWMutex
	db     *sql.DB
	closed bool

	// ver counts mutations to this sheet, so a reader can ask "is what I cached
	// still current?" without touching the database. Bumped by the actor after
	// every write command, which is the one place every mutation passes through,
	// so a new write verb is covered without remembering to add it. It is a
	// cache key, never durable state: a restart resets it and the caches that
	// read it are in the same process.
	ver atomic.Uint64

	// depLo and depHi are the two directions of dependentsSQL, prepared once
	// for the life of the handle. The reverse dependency walk runs once per hop
	// of every cascade, and preparing the three-arm compound dominates it:
	// ~16µs per lookup unprepared against ~3.6µs prepared. The arms themselves
	// are cheap. A *sql.Stmt is safe for concurrent use and re-prepares itself
	// per pooled connection as needed.
	depLo, depHi *sql.Stmt

	// idx is the rank<->key mapping (bandkey.go). It is immutable: a structural
	// mutation builds a replacement and installs it, so a reader sees either the
	// whole change or none of it. idxMu is what makes that true across the
	// SQLite boundary — a reader holds it read-locked for its whole query, and
	// the writer holds it write-locked across COMMIT and the pointer swap, the
	// only moment the two representations of the same fact could disagree.
	// Without it a window read could translate post-commit keys with pre-commit
	// row counts and render a screen one row out, silently. Structural mutations
	// are rare and ~2ms, so readers block for approximately never.
	idxMu sync.RWMutex
	idx   *bandIndex

	// sty is the style state (style.go): the table of distinct looks, the column
	// level of the cascade, and whether the row level exists. It is guarded by
	// idxMu deliberately, not by a lock of its own. A window read needs both the
	// mapping and the table, so two locks would be two acquisitions on the hot
	// read path and a lock ordering to get wrong, for facts that are always read
	// together and only ever written by the same goroutine. Under one lock a
	// reader inside readIndex sees a consistent pair by construction.
	sty *styleState
}

// index returns the current mapping without holding the lock. Only the writer
// may use it: it is the actor goroutine, it is the only thing that installs a
// new one, and it needs the pointer to build the replacement.
func (s *Sheet) index() *bandIndex {
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	return s.idx
}

// readIndex runs fn with the mapping held stable — SQL included. Every read
// path goes through this.
func (s *Sheet) readIndex(fn func(bi *bandIndex) error) error {
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	return fn(s.idx)
}

// commitIndex commits a transaction and installs the mapping that describes the
// sheet it leaves behind, as one step. next may be nil for a mutation that did
// not move any rows.
func (s *Sheet) commitIndex(tx *sql.Tx, next *bandIndex) error {
	return s.commitState(tx, next, nil)
}

// commitState is the general form: commit, then install whichever of the two
// idxMu-guarded snapshots the transaction produced. Either may be nil for a
// mutation that did not change it.
func (s *Sheet) commitState(tx *sql.Tx, nextIdx *bandIndex, nextSty *styleState) error {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if err := tx.Commit(); err != nil {
		return err
	}
	if nextIdx != nil {
		s.idx = nextIdx
	}
	if nextSty != nil {
		s.sty = nextSty
	}
	return nil
}

// Path is the on-disk location of this sheet's database file.
func (s *Sheet) Path() string { return s.path }

// A sheet has two row extents and conflating them is a bug. Rows() is the
// allocated extent, how tall the grid nominally is; a blank sheet has a full
// allocated extent and zero stored cells, because there has to be somewhere to
// scroll to and start typing. UsedRows() is the last row that holds data, which
// is what "jump to the end" means. Neither is derived from the other and
// neither is a constant.

// Rows is the sheet's allocated row extent: the scroll-container height, the
// viewport clamp, the "select to the last row" key handler and the band range a
// buffer subscribes to are all questions about a particular sheet, with no
// compile-time answer.
//
// It is not monotonic — it grows when an insert or a write needs rows the sheet
// does not have and shrinks when rows are deleted, down to a DefaultRows floor,
// because an extent that only ever grows leaves the scrollbar describing rows
// emptied long ago. It is the prefix sum the band index maintains anyway, so
// there is nothing cached to invalidate.
// Version is the sheet's mutation counter. Two reads that see the same value
// are looking at the same content.
func (s *Sheet) Version() uint64 { return s.ver.Load() }

// bumpVersion is called by the actor after a write command.
func (s *Sheet) bumpVersion() { s.ver.Add(1) }

func (s *Sheet) Rows() int {
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	return s.idx.rows
}

// UsedRows is the sheet's used extent: one past the last display row holding
// anything, and 0 for a sheet with no data at all. Always <= Rows().
//
// It is computed, never cached: a cached used range is one more thing every
// delete and clear has to remember to invalidate. It is a backward primary-key
// scan stopping at the first row holding anything, not a MAX() over a
// predicate, because keys ascend with display rank. "Holds anything" includes a
// cell style and a row style — a yellow empty cell is content the user put
// there — but not a column style, which applies to rows that do not exist and
// so names none. The two arms are separate scans rather than a UNION, which
// would need a sort; each stops at its own first hit and the answer is the
// later.
func (s *Sheet) UsedRows() (int, error) {
	var out int
	err := s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			last := func(q string) error {
				var k int64
				err := db.QueryRow(q).Scan(&k)
				if errors.Is(err, sql.ErrNoRows) {
					return nil // nothing in this table: it names no extent
				}
				if err != nil {
					return fmt.Errorf("used extent of %s: %w", s.ID, err)
				}
				if r := bi.rankOf(k); r >= 0 {
					out = max(out, r+1)
				}
				return nil
			}
			if err := last(
				`SELECT k FROM cells
				  WHERE raw <> '' OR computed <> '' OR kind <> 0 OR style <> 0
				  ORDER BY k DESC LIMIT 1`); err != nil {
				return err
			}
			// Skipped on a sheet with no row style, so the common case is a
			// single scan.
			if !s.styles().anyRow {
				return nil
			}
			return last(`SELECT k FROM rows WHERE style <> 0 ORDER BY k DESC LIMIT 1`)
		})
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

// ensureRows extends the sheet so that row `rows-1` exists, and persists the
// extension. It is a no-op — one uncontended read lock and an integer compare —
// when the sheet is already tall enough.
//
// Must be called from the actor goroutine, the only thing allowed to install a
// band index. It is deliberately its own transaction rather than part of the
// caller's, because installing an index is a lock-ordering step (idxMu around
// COMMIT) and nesting that inside a write would hold the write lock across
// another commit. The split fails benignly: growth commits, the write fails,
// and the sheet is left with blank rows at the bottom.
func (s *Sheet) ensureRows(rows int) error {
	if rows <= 0 {
		return nil
	}
	if rows > RowCeiling {
		return fmt.Errorf("%w: row %d is past the %d-row ceiling", ErrBadRef, rows, RowCeiling)
	}
	if s.Rows() >= rows {
		return nil
	}
	return s.use(func(db *sql.DB) error {
		cur := s.index()
		if cur.rows >= rows {
			return nil
		}
		next := cur.clone()
		next.growTo(rows)
		if err := next.validate(); err != nil {
			return fmt.Errorf("grow %s to %d rows: %w", s.ID, rows, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("grow %s: begin: %w", s.ID, err)
		}
		defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
		if err := syncBandIndex(tx, cur, next); err != nil {
			return fmt.Errorf("grow %s to %d rows: %w", s.ID, rows, err)
		}
		return s.commitIndex(tx, next)
	})
}

// use runs fn with the db handle, holding the lifetime read lock.
func (s *Sheet) use(fn func(db *sql.DB) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return fmt.Errorf("sheet %s: %w", s.ID, ErrSheetClosed)
	}
	return fn(s.db)
}

func validSheetID(id string) error {
	if id == "" || len(id) > 64 {
		return fmt.Errorf("%w: %q (want 1..64 chars)", ErrBadSheetID, id)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return fmt.Errorf("%w: %q (letters, digits, - and _ only)", ErrBadSheetID, id)
		}
	}
	return nil
}

// openSheetFile opens (creating if needed) one sheet database and applies the
// schema. Pragmas ride on the DSN so they are applied to every pooled
// connection, not just the first one.
func openSheetFile(dir, id string) (*Sheet, error) {
	if err := validSheetID(id); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create sheet dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, id+".db")
	// Opening a tombstoned id resurrects it: the visitor kept the URL and the
	// schema below is about to build a working empty sheet. Dropping the marker
	// here — on the miss path only — makes it an ordinary sheet again: it
	// counts, it can be edited, and it can be reaped again.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.Remove(tombPath(dir, id)); err == nil {
			obsLog.Info("sheet.resurrect", "sheet", id)
		}
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(off)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sheet %s: %w", id, err)
	}
	// A modest pool: readers run concurrently under WAL, writers are already
	// serialized by the per-sheet actor so they never contend for the write
	// lock among themselves.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(time.Minute)

	if _, err := db.Exec(schemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema to sheet %s: %w", id, err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate sheet %s: %w", id, err)
	}
	if _, err := db.Exec(indexDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("index sheet %s: %w", id, err)
	}
	sh := &Sheet{ID: id, path: path, db: db}
	for _, p := range []struct {
		dst **sql.Stmt
		sql string
	}{{&sh.depLo, dependentsSQLLo}, {&sh.depHi, dependentsSQLHi}} {
		st, err := db.Prepare(p.sql)
		if err != nil {
			sh.closeStmts()
			db.Close()
			return nil, fmt.Errorf("prepare dependents lookup for %s: %w", id, err)
		}
		*p.dst = st
	}
	if sh.idx, err = loadBandIndex(db); err != nil {
		sh.closeStmts()
		db.Close()
		return nil, fmt.Errorf("load band index for %s: %w", id, err)
	}
	if sh.idx == nil {
		// A brand new file: hand it the canonical layout and store it, so that
		// "what rank is this key" has exactly one answer from the first write.
		sh.idx = canonicalBandIndex(keyStride)
		if err := inTx(db, func(tx *sql.Tx) error { return writeBandIndex(tx, sh.idx) }); err != nil {
			sh.closeStmts()
			db.Close()
			return nil, fmt.Errorf("initialise band index for %s: %w", id, err)
		}
	}
	if err := sh.idx.validate(); err != nil {
		sh.closeStmts()
		db.Close()
		return nil, fmt.Errorf("band index of %s is not usable: %w", id, err)
	}
	if sh.sty, err = loadStyleState(db); err != nil {
		sh.closeStmts()
		db.Close()
		return nil, fmt.Errorf("load style state for %s: %w", id, err)
	}
	return sh, nil
}

// inTx runs fn in one transaction, rolling back on error.
func inTx(db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// migrate brings an on-disk sheet up to schemaVersion.
//
// The rule is never to misread a file. A v1 formula stored its references as A1
// text in cells.raw and a v2 formula stores a template plus reference columns,
// and the two are indistinguishable by inspection — `=A1*2` is a well-formed v2
// template with no holes, and reading it as one gives a formula that never
// moves again. So the version is stamped in PRAGMA user_version, an older file
// is converted on open inside one transaction, and a file that cannot be
// converted fails to open rather than opening wrong.
//
// v1 -> v2 is O(formulas): only rows whose text starts with '=' are touched,
// and values are untouched, since re-templating changes how a formula is
// spelled and nothing about what it means. v2 -> v3 drops the `deps` table,
// every edge of which was derived from the reference columns of the formula
// that owned it by the code that still derives them (Formula.Refs).
//
// v3 -> v4 rebuilds the table, because the primary key changed from display row
// to storage key (bandkey.go). It applies the canonical layout — row r becomes
// key (r/BandHeight)*keyStride + r%BandHeight — to the cell's own row and to
// both of its stored reference rows. There is no way to read a v3 file
// correctly as v4: every key would be off by the band spacing.
//
// v4 -> v5 removes the fixed 10,000-row grid and changes nothing on disk, since
// v4 already stored the extent as the sum of bands.nrows and merely required
// that sum to be 10,000, so the step only validates. It is stamped anyway so a
// v5 file grown to 15,000 rows fails loudly under a v4 binary rather than
// rendering the first 10,000 and silently losing the rest.
//
// v5 -> v6 adds cell styling and v6 -> v7 adds the column and row levels of the
// style cascade. Both are ALTER TABLE ADD COLUMN plus CREATE TABLE and rewrite
// no cell; see each step for why neither can be a reinterpretation.
//
// v7 -> v8 adds text wrapping to the style record, which is one more column and
// a wider uniqueness constraint over the same rows.
func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if v >= schemaVersion {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	have, err := columnSet(tx, "cells")
	if err != nil {
		return err
	}
	// The shape, not the stamp, decides whether there is anything to convert. A
	// freshly created file has the current schema and a user_version of 0, so
	// running the legacy steps on it would add v1 columns to a v4 table and then
	// fail reading a "row" column that never existed.
	_, legacy := have[`row`]

	if legacy {
		if v < 2 {
			for _, col := range []string{"ref0_row", "ref0_col", "ref1_row", "ref1_col"} {
				if _, ok := have[col]; ok {
					continue
				}
				if _, err := tx.Exec(`ALTER TABLE cells ADD COLUMN ` + col + ` INTEGER`); err != nil {
					return fmt.Errorf("add column %s: %w", col, err)
				}
			}
			if _, ok := have["ref_span"]; !ok {
				if _, err := tx.Exec(
					`ALTER TABLE cells ADD COLUMN ref_span INTEGER NOT NULL DEFAULT 0`); err != nil {
					return fmt.Errorf("add column ref_span: %w", err)
				}
			}
			if err := retemplateFormulas(tx); err != nil {
				return err
			}
		}
		if v < 3 {
			if _, err := tx.Exec(`DROP INDEX IF EXISTS deps_reverse`); err != nil {
				return fmt.Errorf("drop deps_reverse: %w", err)
			}
			if _, err := tx.Exec(`DROP TABLE IF EXISTS deps`); err != nil {
				return fmt.Errorf("drop deps: %w", err)
			}
		}
		if err := convertRowsToKeys(tx); err != nil {
			return err
		}
	}
	// v5 -> v6: cells gain a style index, and the sheet gains a style table.
	// This cannot misread — a v5 cell row encodes no style, so `0` is the only
	// correct value for the new column, which ADD COLUMN's DEFAULT supplies
	// without writing a byte (in SQLite it is a schema-text change only).
	//
	// The column set is re-read rather than reusing the one taken at the top,
	// because a legacy file has just had its cells table rebuilt from schemaDDL
	// by convertRowsToKeys and already has the column.
	have, err = columnSet(tx, "cells")
	if err != nil {
		return err
	}
	if _, ok := have["style"]; !ok {
		if _, err := tx.Exec(`ALTER TABLE cells ADD COLUMN ` + styleColDDL); err != nil {
			return fmt.Errorf("add column style: %w", err)
		}
	}
	// The styles table itself is created by schemaDDL, which openSheetFile
	// applies before this function runs. Creating it again here is what makes
	// the migration self-contained for anything that calls migrate directly
	// (the tests do), and IF NOT EXISTS makes it free otherwise.
	if _, err := tx.Exec(stylesDDL); err != nil {
		return fmt.Errorf("create styles table: %w", err)
	}

	// v6 -> v7: the style cascade gains its column and row levels, and neither
	// half can misread. A v6 `cols` row encodes a width and nothing else, and an
	// empty `rows` table says "no row carries a style or a height" — what a v6
	// file said by not having the table. What the stamp buys is the other
	// direction: a v7 file with a styled column opened by a v6 binary would
	// render it unstyled and drop the fact on the next write.
	colsHave, err := columnSet(tx, "cols")
	if err != nil {
		return err
	}
	if _, ok := colsHave["style"]; !ok {
		if _, err := tx.Exec(`ALTER TABLE cols ADD COLUMN ` + colStyleColDDL); err != nil {
			return fmt.Errorf("add column style to cols: %w", err)
		}
	}
	if _, err := tx.Exec(rowMetaDDL); err != nil {
		return fmt.Errorf("create rows table: %w", err)
	}

	// v7 -> v8: styles gain wrap. A v7 style row encodes no wrapping, so 0 is
	// the only correct value and ADD COLUMN's DEFAULT supplies it as a
	// schema-text change. The index has to be rebuilt in the same step: it is
	// what makes interning return one id per distinct look, and left at its v7
	// six columns it would collapse a wrapped style onto the unwrapped one it
	// otherwise matches and hand back the wrong id.
	styleHave, err := columnSet(tx, "styles")
	if err != nil {
		return err
	}
	if _, ok := styleHave["wrap"]; !ok {
		if _, err := tx.Exec(`ALTER TABLE styles ADD COLUMN ` + wrapColDDL); err != nil {
			return fmt.Errorf("add column wrap to styles: %w", err)
		}
		if _, err := tx.Exec(`DROP INDEX IF EXISTS styles_tuple`); err != nil {
			return fmt.Errorf("drop styles_tuple: %w", err)
		}
		if _, err := tx.Exec(
			`CREATE UNIQUE INDEX styles_tuple ON styles (bold, italic, fg, bg, align, numfmt, wrap)`,
		); err != nil {
			return fmt.Errorf("rebuild styles_tuple: %w", err)
		}
	}

	// Every file arrives here key-shaped. Give it a band index if it has none —
	// which is true of a file created before this version and of a file created
	// a moment ago.
	bi, err := loadBandIndex(tx)
	if err != nil {
		return err
	}
	if bi == nil {
		bi = canonicalBandIndex(keyStride)
		if err := writeBandIndex(tx, bi); err != nil {
			return err
		}
	}
	// v4 -> v5: the band index IS the row extent, so the whole of this step is
	// making sure the extent the file claims is one this build can honour. A
	// file that fails here has a corrupt or unreadable layout and must not be
	// opened at all — every key in it would name the wrong display row.
	if err := bi.validate(); err != nil {
		return fmt.Errorf("band index is not usable: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("stamp user_version: %w", err)
	}
	return tx.Commit()
}

// legacyGridRows is the row extent every pre-v5 file has: v1..v4 enforced a
// fixed 10,000-row grid in the schema, in ParseRef and in bandIndex.validate.
//
// It is not DefaultRows, and conflating the two loses data: DefaultRows is what
// a new sheet starts at and is free to change, while the height of an existing
// file is a fact about that file. Laying a v3 file out at DefaultRows would
// file every row above it at a key naming no display rank.
const legacyGridRows = 10000

// convertRowsToKeys rebuilds a row-keyed `cells` table as a key-keyed one. It
// is one INSERT ... SELECT over the whole table, because the primary key is
// what changes and SQLite cannot alter one in place. No formula is parsed and
// no value is recomputed; the only thing that moves is where a cell is filed.
func convertRowsToKeys(tx *sql.Tx) error {
	// k(r) for a display row r, and the same for a stored reference row —
	// which may be NULL (no such operand) or negative (already #REF!), and
	// neither of those is a row that has a key.
	key := func(col string) string {
		return fmt.Sprintf(`((%s / %d) * %d + (%s %% %d))`,
			col, BandHeight, keyStride, col, BandHeight)
	}
	refKey := func(col string) string {
		return fmt.Sprintf(`CASE WHEN %s IS NULL THEN NULL WHEN %s < 0 THEN %d ELSE %s END`,
			col, col, deadKey, key(col))
	}
	if _, err := tx.Exec(`ALTER TABLE cells RENAME TO cells_v3`); err != nil {
		return fmt.Errorf("rename v3 cells: %w", err)
	}
	if _, err := tx.Exec(schemaDDL); err != nil {
		return fmt.Errorf("create v4 cells: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(
		`INSERT INTO cells (k, col, raw, computed, kind, `+slotCols+`, ref_span)
		   SELECT %s, col, raw, computed, kind, %s, ref0_col, %s, ref1_col, ref_span
		     FROM cells_v3`,
		key(`"row"`), refKey("ref0_row"), refKey("ref1_row"))); err != nil {
		return fmt.Errorf("convert cells to keys: %w", err)
	}
	// Dropping the old table takes its indexes with it, including the four
	// partial reference indexes, which were built over columns that no longer
	// exist. indexDDL rebuilds them over the key columns after the migration.
	if _, err := tx.Exec(`DROP TABLE cells_v3`); err != nil {
		return fmt.Errorf("drop v3 cells: %w", err)
	}
	// The layout is built for the grid the FILE has, not for the grid a new
	// sheet would get. legacyGridRows is that height; the MAX() is belt and
	// braces for a file that somehow holds a row past it, since a row filed
	// above the extent is a row that disappears.
	rows := legacyGridRows
	var maxKey sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(k) FROM cells`).Scan(&maxKey); err != nil {
		return fmt.Errorf("measure converted grid: %w", err)
	}
	if maxKey.Valid {
		// The keys were just written by the canonical arithmetic above, so the
		// display row of the largest one is recoverable from it directly.
		last := int(maxKey.Int64/keyStride)*BandHeight + int(maxKey.Int64%keyStride)
		rows = max(rows, last+1)
	}
	return writeBandIndex(tx, layoutFor(rows, keyStride))
}

func columnSet(tx *sql.Tx, table string) (map[string]struct{}, error) {
	rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("table_info %s: %w", table, err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("table_info scan: %w", err)
		}
		out[name] = struct{}{}
	}
	return out, rows.Err()
}

// retemplateFormulas rewrites every v1 formula (A1 text) into a v2 template
// plus slots. A formula that does not parse is left exactly as it is and gets
// no slots, which is the same state a v2 write would have produced for it.
func retemplateFormulas(tx *sql.Tx) error {
	type row struct {
		ref   CellRef
		tmpl  string
		slots []CellRef
		span  bool
	}
	rows, err := tx.Query(`SELECT "row", col, raw FROM cells WHERE substr(raw, 1, 1) = '='`)
	if err != nil {
		return fmt.Errorf("scan v1 formulas: %w", err)
	}
	var out []row
	for rows.Next() {
		var r row
		var raw string
		if err := rows.Scan(&r.ref.Row, &r.ref.Col, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("scan v1 formula: %w", err)
		}
		r.tmpl, r.slots, r.span, _ = FormulaTemplate(raw)
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan v1 formulas: %w", err)
	}
	// Row-keyed columns throughout: this runs while the table is still v1/v2
	// shaped, before convertRowsToKeys touches it.
	return bulkInsert(tx, len(out), 8,
		`INSERT INTO cells ("row", col, raw, ref0_row, ref0_col, ref1_row, ref1_col, ref_span) VALUES `,
		` ON CONFLICT("row", col) DO UPDATE SET
		    raw = excluded.raw, ref0_row = excluded.ref0_row, ref0_col = excluded.ref0_col,
		    ref1_row = excluded.ref1_row, ref1_col = excluded.ref1_col,
		    ref_span = excluded.ref_span`,
		func(i int, args []any) []any {
			r := out[i]
			var s [2 * maxSlots]any
			for j := 0; j < maxSlots; j++ {
				if j < len(r.slots) {
					s[2*j], s[2*j+1] = r.slots[j].Row, r.slots[j].Col
				}
			}
			return append(args, r.ref.Row, r.ref.Col, r.tmpl,
				s[0], s[1], s[2], s[3], boolInt(r.span))
		})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// closeStmts releases the prepared lookups. Safe to call more than once and
// safe on a half-built handle, which is what the open path needs.
func (s *Sheet) closeStmts() {
	for _, st := range []*sql.Stmt{s.depLo, s.depHi} {
		if st != nil {
			_ = st.Close()
		}
	}
	s.depLo, s.depHi = nil, nil
}

func (s *Sheet) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.closeStmts()
	return s.db.Close()
}
