package main

import (
	"container/list"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	_ "modernc.org/sqlite"
)

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
const schemaVersion = 7

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

// ─── Reads ────────────────────────────────────────────────────────────────────

// Window returns a dense rectangle of cells for the inclusive display-row range
// [loRow, hiRow] across all MaxCols columns, ordered row-major. Cells with no
// stored row come back as KindEmpty rather than being omitted, so the renderer
// can walk the slice as a grid without checking for gaps. hiRow is clamped to
// the sheet's current extent, so a caller asking beyond the bottom gets a short
// window rather than an error.
//
// This is the hot read path — it runs on every push to every viewer — and it
// deliberately does not go through the actor: WAL readers are concurrent with
// the writer, so serializing them would add latency and buy nothing.
func (s *Sheet) Window(loRow, hiRow int) ([]Cell, error) {
	if loRow > hiRow {
		loRow, hiRow = hiRow, loRow
	}
	if loRow < 0 {
		loRow = 0
	}
	if hiRow < 0 {
		return nil, nil
	}

	var out []Cell
	err := s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			// The clamp is inside the held index, with the query, for the same
			// reason the rank translation is: the extent and the key mapping are
			// two halves of one fact, and reading them a moment apart is how a
			// window renders the right cells against the wrong row numbers.
			hiRow := min(hiRow, bi.rows-1)
			if loRow > hiRow {
				return nil
			}
			nRows := hiRow - loRow + 1
			// The style state comes from the same held lock as the mapping (see
			// Sheet.sty), so the ids these rows carry are resolvable against the
			// table this loop uses, and the column level they resolve through is
			// the one current when the keys were read.
			sty := s.styles()
			// The row level is the only extra query the cascade adds, skipped
			// unless the sheet has a row style at all. It is one primary-key
			// range scan over `rows`, deliberately not a join: a join measured
			// 45-81% slower on exactly this path.
			var rowSty []int
			if sty.anyRow {
				var err error
				if rowSty, err = readRowStylesFor(db, bi, loRow, hiRow); err != nil {
					return err
				}
			}
			cascade := sty.anyCol || rowSty != nil
			out = make([]Cell, nRows*MaxCols)
			for r := 0; r < nRows; r++ {
				// A cell with no stored row still inherits: a blank cell in a
				// yellow row is yellow, and the store says so rather than
				// leaving the render layer to re-derive it. Resolved once per
				// row, since the row half is constant across the 26 columns.
				rs := 0
				if rowSty != nil {
					rs = rowSty[r]
				}
				for c := 0; c < MaxCols; c++ {
					cell := Cell{Ref: CellRef{Row: loRow + r, Col: c}}
					if cascade {
						cell.Effective = resolve(0, rs, sty.col[c])
					}
					out[r*MaxCols+c] = cell
				}
			}
			// A window is a contiguous run of display ranks, so it is a
			// contiguous run of keys — the property the whole storage model is
			// built to preserve, and why this is one ordered primary-key range
			// scan and not a search per row.
			rows, err := db.Query(
				`SELECT k, col, raw, computed, kind, `+slotCols+`, style FROM cells
				  WHERE k BETWEEN ? AND ?
				  ORDER BY k, col`, bi.keyOf(loRow), bi.keyOf(hiRow))
			if err != nil {
				return fmt.Errorf("window %d..%d: %w", loRow, hiRow, err)
			}
			defer rows.Close()
			// The scan targets and the []any pointing at them are built once and
			// reused: they are stable addresses that Scan overwrites, and
			// rebuilding them per row would be two allocations per cell to
			// describe a list of ten pointers that never changes.
			var (
				k             int64
				col, kind     int
				style         int
				raw, computed string
				slots         nullSlots
			)
			args := make([]any, 0, 6+2*maxSlots)
			args = append(args, &k, &col, &raw, &computed, &kind)
			args = append(args, slots.scanArgs()...)
			args = append(args, &style)
			// Keys arrive ascending, so ranks come from one walk over the bands
			// rather than a binary search per cell.
			scan := bi.scanner()
			for rows.Next() {
				// Scan writes every target, NULL included (a NULL slot comes
				// back Valid == false), so there is nothing to reset here.
				if err := rows.Scan(args...); err != nil {
					return fmt.Errorf("window scan: %w", err)
				}
				row := scan.rank(k)
				if row < loRow || row > hiRow || col < 0 || col >= MaxCols {
					continue
				}
				rs := 0
				if rowSty != nil {
					rs = rowSty[row-loRow]
				}
				// A1 text is materialized only for a row that actually has
				// structural references, which is exactly the formula cells. A
				// literal's raw is already its display text.
				if refs := slots.refs(bi); refs != nil {
					raw = RenderTemplate(raw, refs)
				}
				// The cascade is two integer compares rather than a lookup.
				// `cascade` is false for every sheet nobody has styled by row or
				// column, so resolution costs one bool test there.
				eff := style
				if cascade && eff == 0 {
					eff = resolve(0, rs, sty.col[col])
				}
				// Runs only for a cell that actually has a number format, so an
				// unstyled sheet pays one integer compare per cell and no
				// allocation. It reads the effective style, so a
				// currency-formatted column formats its cells without any of
				// them storing a style id.
				display := computed
				if eff != 0 {
					if f := sty.tab.get(eff).Fmt; f != FmtPlain {
						display = FormatValue(computed, f)
					}
				}
				out[(row-loRow)*MaxCols+col] = Cell{
					Ref:       CellRef{Row: row, Col: col},
					Raw:       raw,
					Computed:  computed,
					Display:   display,
					Kind:      Kind(kind),
					Style:     style,
					Effective: eff,
				}
			}
			return rows.Err()
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// WindowCtx is Window with a span around it, so the hot read path shows up as
// `store.window` in a trace with the size of the read attached. It is separate
// rather than a signature change because Window is the documented read API and
// is called from tests and benchmarks that have no context to give it.
func (s *Sheet) WindowCtx(ctx context.Context, loRow, hiRow int) ([]Cell, error) {
	ctx, span := tracer.Start(ctx, "store.window")
	defer span.End()
	cells, err := s.Window(loRow, hiRow)
	span.SetAttributes(
		attribute.Int("rows", hiRow-loRow+1),
		attribute.Int("cells", len(cells)),
		attribute.Int("cols", MaxCols),
	)
	if err != nil {
		span.RecordError(err)
	}
	_ = ctx
	return cells, err
}

// WriteCellCtx is WriteCell with a span around it. Same reasoning as WindowCtx:
// the write path is most of what a `cell.command` trace is made of, so it needs
// to be visible, but WriteCell's contract (called from the actor goroutine,
// inside one transaction) must not change.
func (s *Sheet) WriteCellCtx(ctx context.Context, ref CellRef, raw string, computed []ComputedCell) error {
	_, span := tracer.Start(ctx, "store.write_cell")
	defer span.End()
	span.SetAttributes(
		attribute.String("cell.ref", ref.String()),
		attribute.Int("computed", len(computed)),
	)
	err := s.WriteCell(ref, raw, computed)
	if err != nil {
		span.RecordError(err)
	}
	return err
}

// GetCell returns one cell. A cell that was never written comes back as the
// zero value with its Ref set and Kind == KindEmpty, not as an error — "empty"
// is a normal state in a spreadsheet, not a miss.
func (s *Sheet) GetCell(ref CellRef) (Cell, error) {
	if !ref.Valid() {
		return Cell{}, fmt.Errorf("%w: %v out of grid", ErrBadRef, ref)
	}
	out := Cell{Ref: ref}
	err := s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			var kind, style int
			var slots nullSlots
			args := append([]any{&out.Raw, &out.Computed, &kind}, slots.scanArgs()...)
			args = append(args, &style)
			err := db.QueryRow(
				`SELECT raw, computed, kind, `+slotCols+`, style FROM cells WHERE k = ? AND col = ?`,
				bi.keyOf(ref.Row), ref.Col).Scan(args...)
			if errors.Is(err, sql.ErrNoRows) {
				// A cell that was never written still inherits from its row and
				// its column: under a cascade "empty" is a look as much as it is
				// a state, and a blank cell in a yellow row is yellow.
				eff, cerr := s.effectiveStyle(db, bi, ref, 0)
				if cerr != nil {
					return cerr
				}
				out.Effective = eff
				return nil
			}
			if err != nil {
				return fmt.Errorf("get cell %s: %w", ref, err)
			}
			out.Kind = Kind(kind)
			out.Style = style
			if refs := slots.refs(bi); refs != nil {
				out.Raw = RenderTemplate(out.Raw, refs)
			}
			eff, err := s.effectiveStyle(db, bi, ref, style)
			if err != nil {
				return err
			}
			out.Effective = eff
			out.Display = out.Computed
			if eff != 0 {
				if f := s.styles().tab.get(eff).Fmt; f != FmtPlain {
					out.Display = FormatValue(out.Computed, f)
				}
			}
			return nil
		})
	})
	if err != nil {
		return Cell{}, err
	}
	return out, nil
}

// effectiveStyle resolves one cell through the cascade — the single-cell form
// of what Window does in bulk, since the bulk form's range scan would be a scan
// for one answer. The column level is resident; the row level costs one
// primary-key seek, and only when the sheet carries a row style and the cell
// has none of its own.
//
// The caller must be inside readIndex, so the mapping it keys with and the
// style state it resolves against are the pair Window would have seen.
func (s *Sheet) effectiveStyle(q queryer, bi *bandIndex, ref CellRef, own int) (int, error) {
	if own != 0 {
		return own, nil
	}
	st := s.styles()
	if !st.cascades() {
		return 0, nil
	}
	row := 0
	if st.anyRow {
		var err error
		if row, err = rowStyleOf(q, bi, ref.Row); err != nil {
			return 0, err
		}
	}
	return resolve(0, row, st.col[ref.Col]), nil
}

// dependentsSQL is the reverse dependency lookup: which cells read the cell at
// (?1, ?2). Three arms — the two single-reference slots and the interval query
// over ranges — each on its own partial index, in one round trip.
//
// The repeated `ref_span = ... AND ... IS NOT NULL` terms are not redundant:
// they are what makes each arm eligible for its partial index. Drop them and
// SQLite scans the cells table once per hop of a cascade. The span arm names
// its index with INDEXED BY because both span indexes match the predicate
// equally well as far as the planner can tell — there are no statistics in
// these files — and the point of having two is to choose.
//
// UNION ALL with no ORDER BY, deliberately: `UNION ... ORDER BY 1, 2` makes the
// planner want each arm sorted by (k, col), which no span index can provide, so
// it reads the third arm as `SCAN cells`. Deduplication and ordering are a
// dozen refs in Go instead; see scanDependents.
//
// ?1 is a storage key, not a display row. The interval arm works because keys
// and ranks are ordered the same way, and asking of keys is what lets a range
// survive a row insert with no write at all.
const dependentsSQL = `
SELECT k, col FROM cells
  WHERE ref_span = 0 AND ref0_k IS NOT NULL AND ref0_k = ?1 AND ref0_col = ?2
UNION ALL
SELECT k, col FROM cells
  WHERE ref_span = 0 AND ref1_k IS NOT NULL AND ref1_k = ?1 AND ref1_col = ?2
UNION ALL
SELECT k, col FROM cells INDEXED BY %s
  WHERE ref_span = 1
    AND ref0_k <= ?1 AND ref1_k >= ?1 AND ref0_col <= ?2 AND ref1_col >= ?2`

// dependentsSQLLo drives the interval arm off the low endpoint: good for a
// probe near the top of the grid, where few ranges start above it.
var dependentsSQLLo = fmt.Sprintf(dependentsSQL, "cells_span_lo")

// dependentsSQLHi drives it off the high endpoint: good for a probe near the
// bottom, where few ranges end below it.
var dependentsSQLHi = fmt.Sprintf(dependentsSQL, "cells_span_hi")

// dependentsSQLFor picks the direction with less to scan: for a b-tree driven
// from one end, whichever endpoint is nearer its edge of the grid leaves fewer
// ranges between it and the probe. `rows` is the sheet's own extent, not a
// constant, and picking the wrong arm costs a longer scan, not a wrong answer.
func dependentsSQLFor(row, rows int) string {
	if row*2 > rows {
		return dependentsSQLHi
	}
	return dependentsSQLLo
}

// scanDependents drains a dependentsSQL result into ascending, deduplicated
// refs. The only possible duplicate is a formula that names the same cell in
// both slots (`=A1+A1`), so the dedup is a linear check over a handful of
// entries rather than a map.
func scanDependents(bi *bandIndex, rows *sql.Rows) ([]CellRef, error) {
	var out []CellRef
	for rows.Next() {
		var c CellRef
		var k int64
		if err := rows.Scan(&k, &c.Col); err != nil {
			return nil, fmt.Errorf("dependents scan: %w", err)
		}
		c.Row = bi.rankOf(k)
		dup := false
		for _, o := range out {
			if o == c {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Row != out[j].Row {
			return out[i].Row < out[j].Row
		}
		return out[i].Col < out[j].Col
	})
	return out, nil
}

// DependentsOf returns every cell whose formula reads ref — the reverse walk
// the recalc pass uses to grow a dirty set. One hop only; the caller does the
// transitive closure and the cycle detection. It reads the reference columns of
// the formulas themselves rather than a materialized edge list, so a cell
// cannot be listed as depending on something its own reference does not name.
func (s *Sheet) DependentsOf(ref CellRef) ([]CellRef, error) {
	if !ref.Valid() {
		return nil, fmt.Errorf("%w: %v out of grid", ErrBadRef, ref)
	}
	var out []CellRef
	err := s.use(func(*sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			stmt := s.depLo
			if ref.Row*2 > bi.rows {
				stmt = s.depHi
			}
			rows, err := stmt.Query(bi.keyOf(ref.Row), ref.Col)
			if err != nil {
				return fmt.Errorf("dependents of %s: %w", ref, err)
			}
			defer rows.Close()
			out, err = scanDependents(bi, rows)
			if err != nil {
				return fmt.Errorf("dependents of %s: %w", ref, err)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PrecedentsOf returns the cells that ref's own formula reads — the forward
// walk. It is one primary-key lookup followed by expanding the stored slots,
// because that row is the edge list: two operand refs, or the two endpoints of
// a range. A destroyed reference (stored off-grid, rendered #REF!) is skipped.
func (s *Sheet) PrecedentsOf(ref CellRef) ([]CellRef, error) {
	if !ref.Valid() {
		return nil, fmt.Errorf("%w: %v out of grid", ErrBadRef, ref)
	}
	var out []CellRef
	err := s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			var slots nullSlots
			var span int
			args := append(slots.scanArgs(), &span)
			err := db.QueryRow(
				`SELECT `+slotCols+`, ref_span FROM cells WHERE k = ? AND col = ?`,
				bi.keyOf(ref.Row), ref.Col).Scan(args...)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("precedents of %s: %w", ref, err)
			}
			out = expandSlots(slots.refs(bi), span != 0)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// expandSlots turns stored slots into the cells they name, ascending. A span
// enumerates its rectangle — every cell of a SUM's range is a real dependency,
// including the empty ones, because filling one later must dirty the SUM.
func expandSlots(slots []CellRef, span bool) []CellRef {
	if span {
		if len(slots) != 2 {
			return nil
		}
		lo, hi := slots[0], slots[1]
		if !lo.Valid() || !hi.Valid() {
			return nil
		}
		out := make([]CellRef, 0, (hi.Row-lo.Row+1)*(hi.Col-lo.Col+1))
		for r := lo.Row; r <= hi.Row; r++ {
			for c := lo.Col; c <= hi.Col; c++ {
				out = append(out, CellRef{Row: r, Col: c})
			}
		}
		return out
	}
	out := make([]CellRef, 0, len(slots))
	for _, s := range slots {
		if !s.Valid() {
			continue // a destroyed reference names nothing
		}
		if len(out) == 1 && out[0] == s {
			continue // `=A1+A1` reads A1 once
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Row != out[j].Row {
			return out[i].Row < out[j].Row
		}
		return out[i].Col < out[j].Col
	})
	return out
}

// Events returns log rows with seq > afterSeq, oldest first, capped at limit
// (limit <= 0 means 1000).
func (s *Sheet) Events(afterSeq int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	var out []Event
	err := s.use(func(db *sql.DB) error {
		rows, err := db.Query(
			`SELECT seq, ts, ref, raw, prev FROM events
			  WHERE seq > ? ORDER BY seq LIMIT ?`, afterSeq, limit)
		if err != nil {
			return fmt.Errorf("events: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e Event
			var ms int64
			if err := rows.Scan(&e.Seq, &ms, &e.Ref, &e.Raw, &e.Prev); err != nil {
				return fmt.Errorf("events scan: %w", err)
			}
			e.TS = time.UnixMilli(ms).UTC()
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LastSeq is the highest event sequence written to this sheet (0 if none).
func (s *Sheet) LastSeq() (int64, error) {
	var seq int64
	err := s.use(func(db *sql.DB) error {
		return db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&seq)
	})
	return seq, err
}

// maxEvents bounds the event log to its most recent rows. Every mutation
// appends one, so without a bound a script holding down a key grows the file
// until the per-sheet 256 MB disk cap (limits.go) notices.
//
// 100,000 is far above any human: at a fast typist's ~10 commits a second it is
// almost three hours of continuous editing, and at ~60 bytes a row it costs
// ~6 MB of the sheet's 256 MB. Trimming loses nothing that is replayed — the
// table is read only by Events() and LastSeq(), the live patch history is
// editlog.go's in-memory ring, undo is out of scope per SPEC.md, and the system
// of record is `cells`.
const maxEvents = 100000

// eventTrimEvery gates the trim to one write in a thousand. Running the delete
// on every write costs 3-6µs on a path that is 47-53µs, most of it statement
// preparation rather than the row removed, since a log far below the cap pays
// the same. Gated, the cost is an int64 modulo and the log is bounded by
// maxEvents + eventTrimEvery. The write that does trim removes a thousand rows
// at ~365µs, ~0.37µs a write amortized, and lands at ~0.4ms where its
// neighbours are ~0.05ms — still an eighth of what a row insert costs.
const eventTrimEvery = 1000

// trimEvents drops everything older than the most recent maxEvents rows, on the
// writes eventTrimEvery selects. It runs inside the mutation's transaction, so
// the log a reader sees is the log the committed sheet has and a rolled-back
// mutation cannot have trimmed anything.
//
// MAX(seq) is an index lookup on the INTEGER PRIMARY KEY and the delete a range
// scan over the head of that index, so the statement is bounded by the rows it
// removes. seq is AUTOINCREMENT, so trimming the head cannot make a later event
// reuse a trimmed number; on an empty log MAX(seq) is NULL and matches nothing.
func trimEvents(tx *sql.Tx, seq int64) error {
	if seq%eventTrimEvery != 0 {
		return nil
	}
	if _, err := tx.Exec(
		`DELETE FROM events WHERE seq <= (SELECT MAX(seq) - ? FROM events)`,
		maxEvents,
	); err != nil {
		return fmt.Errorf("trim events: %w", err)
	}
	return nil
}

// appendEvent writes one log row and trims the log. Every mutation in the
// system appends through here — the cell write, the row and column ops, the
// column width and all three levels of styling — because a bound that held on
// the cell path and nowhere else could still be walked past by holding down
// insert-row.
func appendEvent(tx *sql.Tx, ref, raw, prev string) error {
	res, err := tx.Exec(
		`INSERT INTO events (ts, ref, raw, prev) VALUES (?, ?, ?, ?)`,
		time.Now().UnixMilli(), ref, raw, prev,
	)
	if err != nil {
		return err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return err
	}
	return trimEvents(tx, seq)
}

// appendEventStmt is appendEvent with the INSERT already prepared, for the
// batch write. The batch appends one event per cell, because each cell is a
// user-visible mutation and the log is what "events == user edits" is a
// statement about. The trim is still gated at seq%eventTrimEvery, so a
// 1,000-cell paste crosses that gate at most once.
func appendEventStmt(tx *sql.Tx, ins *sql.Stmt, ref, raw, prev string) error {
	res, err := ins.Exec(time.Now().UnixMilli(), ref, raw, prev)
	if err != nil {
		return err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return err
	}
	return trimEvents(tx, seq)
}

// ─── The write ────────────────────────────────────────────────────────────────

// MaxCellBytes caps the text one cell may hold. The formula grammar tops out at
// two operands or one range, so the longest legal formula is a couple of dozen
// bytes and the longest plausible label is a sentence.
//
// The number that matters is not the cell but the window: a buffer is ~6,500
// cells and every one is re-rendered into every push, so the cap is really "how
// much can one cell cost every viewer on its band". At 4 KB one cell is ~1.5%
// of a 264 KB page; an uncapped 60,000-byte cell would be 23%.
const MaxCellBytes = 4096

// ErrCellTooLong is the sentinel for a value past MaxCellBytes.
var ErrCellTooLong = errors.New("cell value too long")

// cellTooLongError is that refusal with the numbers in it. It answers errors.Is
// for ErrBadRef as well as ErrCellTooLong because every HTTP path that reaches
// WriteCell maps an ErrBadRef-wrapped error to 400 and everything else to 500,
// and a refusal arriving as a 500 would reach the editor as "something broke"
// rather than as a sentence the user can act on. The Error() text is the body
// of that 400, shown verbatim by the editor's failure path.
type cellTooLongError struct {
	ref CellRef
	n   int
}

func (e cellTooLongError) Error() string {
	return fmt.Sprintf("%v holds %d bytes of text; a cell is limited to %d",
		e.ref, e.n, MaxCellBytes)
}

func (e cellTooLongError) Is(target error) bool {
	return target == ErrCellTooLong || target == ErrBadRef
}

// WriteCell commits one edit: the cell mutation, every recalculated value the
// formula engine produced, and the event describing it — all in one
// transaction. There is no partial state a reader can observe and no way to get
// an event without its mutation.
//
// computed carries the recalc results from the formula engine (recalc.go), and
// may be empty for a literal with no dependents. An entry for ref itself wins
// for ref's computed/kind; otherwise the value is inferred from raw. Entries
// for other cells update computed+kind only and leave raw untouched.
//
// deps is vestigial and ignored: a cell's outgoing edges are its reference
// columns, written by the upsert in step 3.
//
// Ordering contract: recalc runs before this call, so the database still holds
// ref's old value while it runs. Evaluate the graph with an overlay of
// {ref: raw} and include ref's own result in computed; writing first and
// recalculating after would need a second transaction, the split SPEC.md
// forbids.
//
// Must be called from the sheet's actor goroutine — see actor.go.
func (s *Sheet) WriteCell(ref CellRef, raw string, computed []ComputedCell) error {
	if !ref.Valid() {
		return fmt.Errorf("%w: %v out of grid", ErrBadRef, ref)
	}
	// The bound lives here rather than in a handler because three commands reach
	// this function — the cell edit, applyWrites (rangeops.go) and the clear
	// loop. The 64 KB request-body cap in limits.go bounds it only by accident:
	// middleware sees how many bytes arrived, not how many are one cell's value.
	if len(raw) > MaxCellBytes {
		return cellTooLongError{ref: ref, n: len(raw)}
	}
	// A write below the bottom of the sheet grows the sheet — one of the two
	// ways a sheet gets taller, the other being an insert at the bottom. Without
	// it the ref maps to deadKey and the CHECK constraint refuses the write,
	// which is a refusal where a spreadsheet should simply have more rows. The
	// recalculated cells count too: a cascade may land on a row the edit did not
	// name.
	need := ref.Row + 1
	for _, cc := range computed {
		if !cc.Ref.Valid() {
			return fmt.Errorf("%w: computed %v out of grid", ErrBadRef, cc.Ref)
		}
		need = max(need, cc.Ref.Row+1)
	}
	if err := s.ensureRows(need); err != nil {
		return err
	}
	return s.use(func(db *sql.DB) error {
		// The mapping is held for the whole transaction: an edit does not move
		// rows, so it needs the ranks it started with to still mean the same
		// keys when it finishes.
		return s.readIndex(func(bi *bandIndex) error {
			return s.writeCellTx(db, bi, ref, raw, computed)
		})
	})
}

func (s *Sheet) writeCellTx(db *sql.DB, bi *bandIndex, ref CellRef, raw string, computed []ComputedCell) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin write %s: %w", ref, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	key := bi.keyOf(ref.Row)

	// 1. Read the previous raw so the event records what it replaced.
	var prev string
	err = tx.QueryRow(`SELECT raw FROM cells WHERE k = ? AND col = ?`,
		key, ref.Col).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read prev %s: %w", ref, err)
	}

	// 2. Append the event first. Everything after this point is a mutation
	//    that can fail, and having the log row already in the transaction
	//    means the rollback path is what removes it, so "log written but
	//    mutation lost" is unreachable.
	if err := appendEvent(tx, ref.String(), raw, prev); err != nil {
		return fmt.Errorf("append event %s: %w", ref, err)
	}

	// 3. The cell itself. computed/kind come from the engine when it had
	//    something to say about this cell, otherwise from the literal.
	selfComputed, selfKind := raw, InferKind(raw)
	if selfKind == KindFormula {
		// A formula with no engine result yet: show the formula rather
		// than a stale value. recalc.go normally supplies an entry.
		selfComputed = raw
	}
	for _, cc := range computed {
		if cc.Ref == ref {
			selfComputed, selfKind = cc.Computed, cc.Kind
			break
		}
	}
	// The reference split happens on the way in, so everything downstream of a
	// write already has structural references to move. FormulaTemplate is a
	// no-op for a literal and for a formula that does not parse, both of which
	// store their text verbatim with no slots.
	tmpl, slots, span, _ := FormulaTemplate(raw)
	sa := slotArgs(bi, slots)
	if _, err := tx.Exec(
		`INSERT INTO cells (k, col, raw, computed, kind, `+slotCols+`, ref_span)
			   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			   ON CONFLICT(k, col) DO UPDATE SET
			     raw = excluded.raw, computed = excluded.computed, kind = excluded.kind,
			     ref0_k = excluded.ref0_k, ref0_col = excluded.ref0_col,
			     ref1_k = excluded.ref1_k, ref1_col = excluded.ref1_col,
			     ref_span = excluded.ref_span`,
		key, ref.Col, tmpl, selfComputed, int(selfKind),
		sa[0], sa[1], sa[2], sa[3], boolInt(span),
	); err != nil {
		return fmt.Errorf("upsert cell %s: %w", ref, err)
	}

	// 4. Every other recalculated cell: computed+kind only, raw preserved.
	for _, cc := range computed {
		if cc.Ref == ref {
			continue
		}
		// An out-of-grid ref has no key, so it binds deadKey and the CHECK
		// constraint refuses it — which is what keeps a bad computed entry a
		// loud mid-transaction failure instead of a cell filed at a
		// nonexistent rank.
		if _, err := tx.Exec(
			`INSERT INTO cells (k, col, raw, computed, kind) VALUES (?, ?, '', ?, ?)
				   ON CONFLICT(k, col) DO UPDATE SET
				     computed = excluded.computed, kind = excluded.kind`,
			bi.keyOf(cc.Ref.Row), cc.Ref.Col, cc.Computed, int(cc.Kind),
		); err != nil {
			return fmt.Errorf("apply computed %s: %w", cc.Ref, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit write %s: %w", ref, err)
	}
	return nil
}

// ─── The batched write ────────────────────────────────────────────────────────
//
// A range clear, fill or paste builds a []BatchWrite and calls ApplyBatchCtx,
// so the cascade is walked once for the whole range and the range commits once:
// one actor turn, one dirty set, one publish, one transaction — and still one
// event per cell. A caller wanting the two halves separately (a test
// substituting an engine, an actor that wants the recalc outside the write)
// drives RecalcBatch, WriteCellsCtx and DirtyWithWrites itself.

// BatchResult is what a batched write reports back.
type BatchResult struct {
	// Dirty is the publish set: every cell whose computed value moved, plus
	// every cell the batch wrote whatever its value did. The second half is
	// needed because a viewer holding that band must see the new raw text even
	// when the number it renders to did not change.
	Dirty []CellRef

	// Wrote is how many cell writes were committed — len(writes), since the
	// batch commits every one of them and the range commands have already
	// dropped the cells whose text would not change.
	Wrote int

	Stats RecalcStats
}

// Bands is the dirty set expressed as subscription topics.
func (r BatchResult) Bands() []int { return BandsFor(r.Dirty) }

// ApplyBatch is the whole bulk write: recalculate the union of everything the
// batch touches once, then commit every raw value, every recalculated value and
// every event in one transaction. The ordering contract is the single-cell
// one — the recalc pass runs first, against an overlay of the batch, so the
// database still holds the old values while it runs and a pass that refuses
// (ErrRecalcTooLarge) has written nothing.
//
// Must be called from the sheet's actor goroutine — see actor.go.
func (s *Sheet) ApplyBatch(writes []BatchWrite) (BatchResult, error) {
	res, err := RecalcBatch(s, writes)
	if err != nil {
		return BatchResult{}, err
	}
	// The write gets the list as given, repeats included, while the recalc
	// collapsed them: a repeated address is two user edits and the event log
	// says so. Only the value is computed once, for the state the batch ends
	// in.
	if err := s.WriteCells(writes, res.Computed); err != nil {
		return BatchResult{}, err
	}
	return BatchResult{
		Dirty: DirtyWithWrites(res.Dirty, writes),
		Wrote: len(writes),
		Stats: res.Stats,
	}, nil
}

// DirtyWithWrites is withRef (http.go) for a whole batch: the recalc's dirty
// set plus every cell the batch wrote, deduped, in that order. Recalc reports
// what changed; a range command needs what moved on screen, and rewriting
// `=1+1` over `2` changes no value and still has to be pushed. It uses a set
// rather than withRef's linear scan, which at a thousand writes would be
// O(N*M).
func DirtyWithWrites(dirty []CellRef, writes []BatchWrite) []CellRef {
	if len(writes) == 0 {
		return dirty
	}
	in := make(map[CellRef]struct{}, len(dirty)+len(writes))
	for _, c := range dirty {
		in[c] = struct{}{}
	}
	for _, w := range writes {
		if _, dup := in[w.Ref]; dup {
			continue
		}
		in[w.Ref] = struct{}{}
		dirty = append(dirty, w.Ref)
	}
	return dirty
}

// ApplyBatchCtx is ApplyBatch with spans around its two halves, so a
// `clear.command` / `fill.command` / `paste.command` trace shows the recalc and
// the write separately — the split RESULTS.md reports per-cell numbers against.
func (s *Sheet) ApplyBatchCtx(ctx context.Context, writes []BatchWrite) (BatchResult, error) {
	_, span := tracer.Start(ctx, "store.apply_batch")
	defer span.End()
	span.SetAttributes(attribute.Int("writes", len(writes)))
	res, err := s.ApplyBatch(writes)
	span.SetAttributes(
		attribute.Int("cells.written", res.Wrote),
		attribute.Int("dirty_cells", len(res.Dirty)),
		attribute.Int("nodes_visited", res.Stats.Nodes),
		attribute.Int("depth", res.Stats.Depth),
		attribute.Int("cycled", res.Stats.Cycled),
		attribute.Int("queries", res.Stats.Queries),
		attribute.Int("cells_read", res.Stats.CellsRead),
		attribute.Float64("recalc_ms", msf(res.Stats.Elapsed)),
	)
	if err != nil {
		span.RecordError(err)
	}
	return res, err
}

// WriteCells commits a whole batch: every cell's new text, every recalculated
// value the engine produced for the union of their cascades, and one event per
// cell — all in one transaction. It is WriteCell's contract with the loop
// inside the transaction rather than in the caller.
//
// computed comes from RecalcBatch. Entries whose Ref is one of the written
// cells supply that cell's computed/kind; every other entry updates
// computed+kind only, exactly as WriteCell does.
//
// Everything is validated before the transaction opens, so a batch naming one
// cell past MaxCellBytes or one ref off the grid writes nothing: half a paste
// is not a state a user can reason about.
//
// Must be called from the sheet's actor goroutine — see actor.go.
func (s *Sheet) WriteCells(writes []BatchWrite, computed []ComputedCell) error {
	if len(writes) == 0 {
		return nil
	}
	need := 0
	for _, w := range writes {
		if !w.Ref.Valid() {
			return fmt.Errorf("%w: %v out of grid", ErrBadRef, w.Ref)
		}
		// The same bound WriteCell applies, applied to every cell of the batch;
		// see WriteCell on why it lives in the store.
		if len(w.Raw) > MaxCellBytes {
			return cellTooLongError{ref: w.Ref, n: len(w.Raw)}
		}
		need = max(need, w.Ref.Row+1)
	}
	for _, cc := range computed {
		if !cc.Ref.Valid() {
			return fmt.Errorf("%w: computed %v out of grid", ErrBadRef, cc.Ref)
		}
		need = max(need, cc.Ref.Row+1)
	}
	// A write below the bottom of the sheet grows the sheet, once for the whole
	// batch rather than once per cell — a paste anchored at row 15,000 on a
	// 1,000-row sheet grows it exactly once.
	if err := s.ensureRows(need); err != nil {
		return err
	}
	return s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			return s.writeCellsTx(db, bi, writes, computed)
		})
	})
}

// WriteCellsCtx is WriteCells with a span around it, the same way WriteCellCtx
// wraps WriteCell.
func (s *Sheet) WriteCellsCtx(ctx context.Context, writes []BatchWrite, computed []ComputedCell) error {
	_, span := tracer.Start(ctx, "store.write_cells")
	defer span.End()
	span.SetAttributes(
		attribute.Int("writes", len(writes)),
		attribute.Int("computed", len(computed)),
	)
	err := s.WriteCells(writes, computed)
	if err != nil {
		span.RecordError(err)
	}
	return err
}

// writeCellsTx is writeCellTx's body with the statements prepared once for the
// whole batch instead of once per cell. Statement preparation dominates this
// path at small scale (DATA-MODEL.md records 16.1µs against 6.5µs for three
// statements versus one), and a thousand-cell paste issues thousands of them.
func (s *Sheet) writeCellsTx(db *sql.DB, bi *bandIndex, writes []BatchWrite, computed []ComputedCell) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin batch write: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	prevStmt, err := tx.Prepare(`SELECT raw FROM cells WHERE k = ? AND col = ?`)
	if err != nil {
		return fmt.Errorf("prepare read prev: %w", err)
	}
	defer prevStmt.Close() //nolint:errcheck // read-only statement
	evStmt, err := tx.Prepare(`INSERT INTO events (ts, ref, raw, prev) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare event: %w", err)
	}
	defer evStmt.Close() //nolint:errcheck // rolled back with the tx on failure
	cellStmt, err := tx.Prepare(
		`INSERT INTO cells (k, col, raw, computed, kind, ` + slotCols + `, ref_span)
		   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		   ON CONFLICT(k, col) DO UPDATE SET
		     raw = excluded.raw, computed = excluded.computed, kind = excluded.kind,
		     ref0_k = excluded.ref0_k, ref0_col = excluded.ref0_col,
		     ref1_k = excluded.ref1_k, ref1_col = excluded.ref1_col,
		     ref_span = excluded.ref_span`)
	if err != nil {
		return fmt.Errorf("prepare cell upsert: %w", err)
	}
	defer cellStmt.Close() //nolint:errcheck // rolled back with the tx on failure

	// The engine's answer for each written cell, so the loop below does not
	// rescan `computed` per cell: that is O(N*M), and M is the cascade.
	self := make(map[CellRef]ComputedCell, len(writes))
	written := make(map[CellRef]struct{}, len(writes))
	for _, w := range writes {
		written[w.Ref] = struct{}{}
	}
	for _, cc := range computed {
		if _, ok := written[cc.Ref]; ok {
			self[cc.Ref] = cc
		}
	}

	for _, w := range writes {
		key := bi.keyOf(w.Ref.Row)

		// 1. The previous raw, so the event records what it replaced.
		var prev string
		err := prevStmt.QueryRow(key, w.Ref.Col).Scan(&prev)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read prev %s: %w", w.Ref, err)
		}

		// 2. The event, before the mutation, for the reason writeCellTx gives.
		//    With one transaction for the whole batch, "log written but
		//    mutation lost" is unreachable for every cell of it.
		if err := appendEventStmt(tx, evStmt, w.Ref.String(), w.Raw, prev); err != nil {
			return fmt.Errorf("append event %s: %w", w.Ref, err)
		}

		// 3. The cell itself.
		selfComputed, selfKind := w.Raw, InferKind(w.Raw)
		if cc, ok := self[w.Ref]; ok {
			selfComputed, selfKind = cc.Computed, cc.Kind
		}
		tmpl, slots, span, _ := FormulaTemplate(w.Raw)
		sa := slotArgs(bi, slots)
		if _, err := cellStmt.Exec(
			key, w.Ref.Col, tmpl, selfComputed, int(selfKind),
			sa[0], sa[1], sa[2], sa[3], boolInt(span),
		); err != nil {
			return fmt.Errorf("upsert cell %s: %w", w.Ref, err)
		}
	}

	// 4. Every other recalculated cell: computed+kind only, raw preserved. The
	//    written cells are excluded because step 3 already carried their value,
	//    and re-issuing them here would overwrite the raw it just stored.
	if len(computed) > len(self) {
		valStmt, err := tx.Prepare(
			`INSERT INTO cells (k, col, raw, computed, kind) VALUES (?, ?, '', ?, ?)
			   ON CONFLICT(k, col) DO UPDATE SET
			     computed = excluded.computed, kind = excluded.kind`)
		if err != nil {
			return fmt.Errorf("prepare computed upsert: %w", err)
		}
		defer valStmt.Close() //nolint:errcheck // rolled back with the tx on failure
		for _, cc := range computed {
			if _, ok := written[cc.Ref]; ok {
				continue
			}
			if _, err := valStmt.Exec(
				bi.keyOf(cc.Ref.Row), cc.Ref.Col, cc.Computed, int(cc.Kind),
			); err != nil {
				return fmt.Errorf("apply computed %s: %w", cc.Ref, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit batch write: %w", err)
	}
	return nil
}

// InferKind classifies literal cell contents. Anything starting with '=' is a
// formula regardless of what follows — deciding whether it is a *valid*
// formula is recalc.go's job.
func InferKind(raw string) Kind {
	if raw == "" {
		return KindEmpty
	}
	if strings.HasPrefix(raw, "=") {
		return KindFormula
	}
	if _, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
		return KindNumber
	}
	return KindText
}

// ─── LRU of open sheet handles ────────────────────────────────────────────────

const (
	// DefaultSheetCacheCap is how many sheet databases stay open at once.
	DefaultSheetCacheCap = 64
	// DefaultSheetIdleTTL is how long an untouched sheet stays open.
	DefaultSheetIdleTTL = 5 * time.Minute
)

type cacheEntry struct {
	sheet *Sheet
	used  time.Time
	elem  *list.Element // position in the recency list; front == most recent
}

// SheetCache is a capacity-bounded, idle-evicting LRU of open sheet handles.
// Thousands of sheets must not mean thousands of open file descriptors.
// Safe for concurrent use.
type SheetCache struct {
	dir  string
	cap  int
	idle time.Duration

	mu      sync.Mutex
	entries map[string]*cacheEntry
	order   *list.List // of string ids, most-recently-used at the front
	// deleting holds the ids whose files are being unlinked right now. Open
	// refuses them for those few milliseconds rather than racing the unlink and
	// leaving behind a freshly created database that nothing will ever reap. It
	// is nil until the first delete, so the hot Open path pays one branch on a
	// lock it already holds.
	deleting map[string]struct{}

	stopOnce sync.Once
	stop     chan struct{}
	janitor  sync.WaitGroup
}

// NewSheetCache creates a cache rooted at dir. capacity <= 0 and idle <= 0
// fall back to the defaults. Call Close when done.
func NewSheetCache(dir string, capacity int, idle time.Duration) *SheetCache {
	if capacity <= 0 {
		capacity = DefaultSheetCacheCap
	}
	if idle <= 0 {
		idle = DefaultSheetIdleTTL
	}
	c := &SheetCache{
		dir:     dir,
		cap:     capacity,
		idle:    idle,
		entries: make(map[string]*cacheEntry),
		order:   list.New(),
		stop:    make(chan struct{}),
	}
	tick := idle / 4
	if tick < 50*time.Millisecond {
		tick = 50 * time.Millisecond
	}
	c.janitor.Add(1)
	go func() {
		defer c.janitor.Done()
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-t.C:
				c.evictIdle()
			}
		}
	}()
	return c
}

// Open returns a live handle for id, creating the database on first use.
// Do not retain the handle beyond the current operation: eviction closes it
// and later calls return ErrSheetClosed.
func (c *SheetCache) Open(id string) (*Sheet, error) {
	if err := validSheetID(id); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if e, ok := c.entries[id]; ok {
		e.used = time.Now()
		c.order.MoveToFront(e.elem)
		c.mu.Unlock()
		return e.sheet, nil
	}
	if _, gone := c.deleting[id]; gone {
		c.mu.Unlock()
		return nil, fmt.Errorf("sheet %s: %w", id, ErrSheetDeleting)
	}
	c.mu.Unlock()

	// Open outside the lock — creating a file and applying DDL is slow enough
	// that holding the cache mutex would stall every other sheet.
	sh, err := openSheetFile(c.dir, id)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if e, ok := c.entries[id]; ok {
		// Someone else won the race; discard ours and use theirs.
		c.mu.Unlock()
		_ = sh.close()
		c.mu.Lock()
		e.used = time.Now()
		c.order.MoveToFront(e.elem)
		c.mu.Unlock()
		return e.sheet, nil
	}
	e := &cacheEntry{sheet: sh, used: time.Now()}
	e.elem = c.order.PushFront(id)
	c.entries[id] = e
	victims := c.trimLocked()
	c.mu.Unlock()

	closeAll(victims)
	return sh, nil
}

// trimLocked drops least-recently-used entries until the cache is at capacity,
// returning the sheets to close. Closing happens outside the lock because
// db.Close blocks on in-flight queries.
func (c *SheetCache) trimLocked() []*Sheet {
	var victims []*Sheet
	for len(c.entries) > c.cap {
		back := c.order.Back()
		if back == nil {
			break
		}
		id := back.Value.(string)
		if e, ok := c.entries[id]; ok {
			victims = append(victims, e.sheet)
			delete(c.entries, id)
		}
		c.order.Remove(back)
	}
	return victims
}

func (c *SheetCache) evictIdle() {
	cutoff := time.Now().Add(-c.idle)
	var victims []*Sheet
	c.mu.Lock()
	for id, e := range c.entries {
		if e.used.Before(cutoff) {
			victims = append(victims, e.sheet)
			c.order.Remove(e.elem)
			delete(c.entries, id)
		}
	}
	c.mu.Unlock()
	closeAll(victims)
}

func closeAll(sheets []*Sheet) {
	for _, s := range sheets {
		_ = s.close()
	}
}

// Len is the number of currently open handles.
func (c *SheetCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// IsOpen reports whether id currently has an open handle. Test/debug aid.
func (c *SheetCache) IsOpen(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[id]
	return ok
}

// Close stops the janitor and closes every open handle.
func (c *SheetCache) Close() error {
	c.stopOnce.Do(func() { close(c.stop) })
	c.janitor.Wait()
	c.mu.Lock()
	victims := make([]*Sheet, 0, len(c.entries))
	for id, e := range c.entries {
		victims = append(victims, e.sheet)
		c.order.Remove(e.elem)
		delete(c.entries, id)
	}
	c.mu.Unlock()
	closeAll(victims)
	return nil
}

// ─── Deletion, and the tombstone that keeps the URL alive ─────────────────────
//
// A sheet id is the only capability in this system, so deleting a sheet must
// not delete the ability to come back to it. Reaping leaves a tombstone — a
// one-line `{id}.gone` file next to where the database was — and the next visit
// to that URL opens a fresh, empty sheet at the same id. The tombstone keeps
// that from becoming "any well-formed id conjures a sheet", which would let a
// scanner create databases with GET requests and walk around the creation rate
// limit. It is not a `.db`, so it counts against neither the sheet count cap
// nor anybody's per-IP quota, and ListSheets never sees it.

// tombSuffix is the extension of a reaped sheet's marker file. It deliberately
// does not end in `.db`: every count, list and quota in the process keys off
// that suffix, and a tombstone must be invisible to all of them.
const tombSuffix = ".gone"

func tombPath(dir, id string) string { return filepath.Join(dir, id+tombSuffix) }

// isTombstoned reports whether id was reaped and has not been revisited.
func isTombstoned(dir, id string) bool {
	if validSheetID(id) != nil {
		return false
	}
	_, err := os.Stat(tombPath(dir, id))
	return err == nil
}

// Delete removes a sheet's files and leaves a tombstone. The ordering is the
// careful part:
//
//  1. The handle is evicted from the LRU first, under the cache lock, and the
//     id is parked in `deleting` so a concurrent Open cannot recreate the file
//     between the unlink and the tombstone.
//  2. The handle is closed outside the lock, because (*sql.DB).Close blocks
//     until in-flight queries finish, so any read or write already running
//     completes before a byte is unlinked. Closing also checkpoints and
//     removes the WAL.
//  3. The sidecars go before the database. A `-wal` outliving its `.db` is the
//     one ordering that could resurrect old pages into a new file.
//
// A later Open therefore cannot see a stale handle, and a retained *Sheet
// pointer returns ErrSheetClosed rather than reading a deleted file.
func (c *SheetCache) Delete(id string) error {
	if err := validSheetID(id); err != nil {
		return err
	}

	c.mu.Lock()
	e, held := c.entries[id]
	if held {
		c.order.Remove(e.elem)
		delete(c.entries, id)
	}
	if c.deleting == nil {
		c.deleting = make(map[string]struct{})
	}
	c.deleting[id] = struct{}{}
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.deleting, id)
		if len(c.deleting) == 0 {
			c.deleting = nil
		}
		c.mu.Unlock()
	}()

	if held {
		if err := e.sheet.close(); err != nil {
			return fmt.Errorf("close sheet %s before delete: %w", id, err)
		}
	}

	base := filepath.Join(c.dir, id+".db")
	for _, p := range []string{base + "-shm", base + "-wal", base} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete sheet %s: %w", id, err)
		}
	}

	// The tombstone is written last, so a crash mid-delete leaves either a
	// sheet or nothing — never a tombstone standing in front of a live file.
	line := "reaped " + time.Now().UTC().Format(time.RFC3339) + "\n"
	if err := os.WriteFile(tombPath(c.dir, id), []byte(line), 0o644); err != nil {
		return fmt.Errorf("tombstone sheet %s: %w", id, err)
	}
	return nil
}

// Tombstones lists reaped ids and when they were reaped, oldest first. The
// reaper uses it to expire markers when an operator has asked for that.
func (c *SheetCache) Tombstones() ([]SheetInfo, error) {
	ents, err := os.ReadDir(c.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list tombstones in %s: %w", c.dir, err)
	}
	var out []SheetInfo
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), tombSuffix) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), tombSuffix)
		if validSheetID(id) != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, SheetInfo{ID: id, Size: fi.Size(), ModTime: fi.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ModTime.Equal(out[j].ModTime) {
			return out[i].ModTime.Before(out[j].ModTime)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ForgetTombstone drops the marker for id. After this the URL 404s again, so
// it is only ever called deliberately (tombstone expiry) or implicitly when the
// id becomes a live sheet again.
func (c *SheetCache) ForgetTombstone(id string) error {
	if err := validSheetID(id); err != nil {
		return err
	}
	if err := os.Remove(tombPath(c.dir, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// sheetLastWrite is when a sheet was last written, which is not its database
// file's mtime. Under WAL, `{id}.db` is only touched by a checkpoint, so for a
// sheet written continuously and never idle its mtime can be arbitrarily stale
// and a retention sweep reading it alone would clear a sheet somebody is in the
// middle of using.
//
// The WAL's mtime closes the gap, and the size test is what makes it safe: a
// read-only session creates a zero-byte WAL stamped with the moment the sheet
// was opened, so trusting its mtime unconditionally would turn "idle" into
// "unread". A non-empty WAL means un-checkpointed writes, and its mtime is when
// the last one landed.
func sheetLastWrite(dir, id string, dbMod time.Time) time.Time {
	if validSheetID(id) != nil {
		return dbMod
	}
	fi, err := os.Stat(filepath.Join(dir, id+".db-wal"))
	if err != nil || fi.Size() == 0 {
		return dbMod
	}
	if w := fi.ModTime(); w.After(dbMod) {
		return w
	}
	return dbMod
}

// DeleteSheet removes a sheet from the process-wide store, leaving a tombstone.
func DeleteSheet(id string) error {
	c, _ := defaults()
	return c.Delete(id)
}

// ─── Process-wide default cache ───────────────────────────────────────────────

var (
	defaultCacheMu   sync.Mutex
	defaultCachePtr  *SheetCache
	defaultActorsPtr *Actors
	defaultDir       = filepath.Join("data", "sheets")
)

// ConfigureStore points the process-wide store at dir and sets LRU limits.
// Call once from main before serving; calling it again replaces (and closes)
// the previous cache and actors.
func ConfigureStore(dir string, capacity int, idle time.Duration) {
	defaultCacheMu.Lock()
	old, oldActors := defaultCachePtr, defaultActorsPtr
	defaultDir = dir
	defaultCachePtr = NewSheetCache(dir, capacity, idle)
	defaultActorsPtr = NewActors(defaultCachePtr)
	defaultCacheMu.Unlock()
	if oldActors != nil {
		oldActors.Close()
	}
	if old != nil {
		_ = old.Close()
	}
}

func defaults() (*SheetCache, *Actors) {
	defaultCacheMu.Lock()
	defer defaultCacheMu.Unlock()
	if defaultCachePtr == nil {
		defaultCachePtr = NewSheetCache(defaultDir, DefaultSheetCacheCap, DefaultSheetIdleTTL)
		defaultActorsPtr = NewActors(defaultCachePtr)
	}
	return defaultCachePtr, defaultActorsPtr
}

// OpenSheet returns a handle from the process-wide LRU.
func OpenSheet(id string) (*Sheet, error) {
	c, _ := defaults()
	return c.Open(id)
}

// DefaultActors is the process-wide per-sheet writer pool.
func DefaultActors() *Actors {
	_, a := defaults()
	return a
}

// WriteSheet runs fn on the sheet's actor goroutine — the only supported way
// to mutate a sheet.
func WriteSheet(sheetID string, fn func(*Sheet) error) error {
	return DefaultActors().Do(sheetID, fn)
}

// CloseStore shuts the process-wide actors and cache down.
func CloseStore() error {
	defaultCacheMu.Lock()
	c, a := defaultCachePtr, defaultActorsPtr
	defaultCachePtr, defaultActorsPtr = nil, nil
	defaultCacheMu.Unlock()
	if a != nil {
		a.Close()
	}
	if c != nil {
		return c.Close()
	}
	return nil
}

// ─── Seeding ──────────────────────────────────────────────────────────────────

// Seed layout — the deliberate shape of a seeded sheet. Recalc and the
// benchmark both depend on these plantings, so treat the column assignments as
// a fixture, not an implementation detail.
//
// Columns (0-indexed; the letter is what the user sees):
//
//	A..T (0..19)  literal numbers, value = (row*31 + col*7) % 1000
//	U    (20)     =A{r}*2                 — 1 hop, same row, same band
//	V    (21)     =U{r}+B{r}              — 2 hops (V→U→A), same band
//	W    (22)     =SUM(A{lo}:A{hi})       — first row of EVERY band only;
//	                                        sums that band's whole column A,
//	                                        so any A edit dirties its band's W
//	X    (23)     =W{first(b-1)}+W{first(b)}
//	                                      — first row of every band b >= 1, so
//	                                        an edit in band b-1's column A
//	                                        cascades A → W(b-1) → X(b): two
//	                                        bands, three hops, on every band
//	Y    (24)     the headline cascade — five cells, five bands:
//	                Y1   = SUM(A1:A10)    band 0
//	                Y51  = Y1*2           band 1
//	                Y101 = Y51+A1         band 2
//	                Y151 = SUM(Y1:Y101)   band 3
//	                Y201 = Y151*2         band 4
//	              Editing A1 dirties bands 0..4 through a 4-hop chain (plus
//	              U1/V1/W1 in band 0 and X51 in band 1) — the multi-band
//	              cascade the dispatcher and the bytes-on-the-wire measurement
//	              are meant to stress.
//	Z    (25)     left empty on purpose — a blank column for manual poking.
//
// Seed writes no events: a seed is the initial state of the world, not a
// sequence of edits, and starting the log at seq 0 keeps "events == user edits"
// true. It also writes computed values that are already correct, so a freshly
// seeded sheet renders real numbers before any recalc runs.

// Column indices of the planted formula columns.
const (
	seedColU = 20
	seedColV = 21
	seedColW = 22
	seedColX = 23
	seedColY = 24
)

// seedLiteral is the value of a literal cell in the seeded grid.
func seedLiteral(row, col int) float64 { return float64((row*31 + col*7) % 1000) }

func fmtNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

type seedCell struct {
	ref      CellRef
	raw      string
	computed string
	kind     Kind
}

// Seed creates sheet id (if needed) and populates a rows x cols grid according
// to the layout documented above. The canonical call is Seed(id, 10000, 26). It
// replaces the whole grid — every cell, every style and the row layout — and
// leaves the event log alone.
func Seed(id string, rows, cols int) error {
	c, _ := defaults()
	return c.Seed(id, rows, cols)
}

// Seed populates a sheet in this cache. See the Seed layout comment.
func (c *SheetCache) Seed(id string, rows, cols int) error {
	if rows <= 0 || rows > RowCeiling {
		return fmt.Errorf("seed %s: rows must be 1..%d, got %d", id, RowCeiling, rows)
	}
	if cols <= 0 || cols > MaxCols {
		return fmt.Errorf("seed %s: cols must be 1..%d, got %d", id, MaxCols, cols)
	}
	sh, err := c.Open(id)
	if err != nil {
		return err
	}

	nLit := min(cols, seedColU) // literal columns actually present

	cells := make([]seedCell, 0, rows*cols)

	// Literal columns A..T.
	for r := 0; r < rows; r++ {
		for col := 0; col < nLit; col++ {
			v := fmtNum(seedLiteral(r, col))
			cells = append(cells, seedCell{CellRef{r, col}, v, v, KindNumber})
		}
	}

	// U = A*2 (1 hop). V = U + B (2 hops).
	if cols > seedColU && nLit >= 1 {
		for r := 0; r < rows; r++ {
			a := CellRef{r, 0}
			u := CellRef{r, seedColU}
			val := seedLiteral(r, 0) * 2
			cells = append(cells, seedCell{u, "=" + a.String() + "*2", fmtNum(val), KindFormula})
		}
	}
	if cols > seedColV && nLit >= 2 {
		for r := 0; r < rows; r++ {
			u := CellRef{r, seedColU}
			b := CellRef{r, 1}
			v := CellRef{r, seedColV}
			val := seedLiteral(r, 0)*2 + seedLiteral(r, 1)
			cells = append(cells, seedCell{v, "=" + u.String() + "+" + b.String(), fmtNum(val), KindFormula})
		}
	}

	// W = SUM of this band's column A, on the first row of each band.
	lastBand := BandOf(rows - 1)
	bandSum := make([]float64, lastBand+1)
	bandFirst := func(b int) int { return b * BandHeight }
	if cols > seedColW && nLit >= 1 {
		for b := 0; b <= lastBand; b++ {
			lo, hi := BandRows(b)
			if hi > rows-1 {
				hi = rows - 1
			}
			w := CellRef{lo, seedColW}
			var sum float64
			for r := lo; r <= hi; r++ {
				sum += seedLiteral(r, 0)
			}
			bandSum[b] = sum
			raw := fmt.Sprintf("=SUM(%s:%s)", CellRef{lo, 0}, CellRef{hi, 0})
			cells = append(cells, seedCell{w, raw, fmtNum(sum), KindFormula})
		}
	}

	// X = W(previous band) + W(this band). The cross-band edge that exists
	// for every band, so any edit has somewhere to cascade to.
	if cols > seedColX && cols > seedColW && nLit >= 1 {
		for b := 1; b <= lastBand; b++ {
			x := CellRef{bandFirst(b), seedColX}
			wPrev := CellRef{bandFirst(b - 1), seedColW}
			wCur := CellRef{bandFirst(b), seedColW}
			raw := "=" + wPrev.String() + "+" + wCur.String()
			cells = append(cells, seedCell{x, raw, fmtNum(bandSum[b-1] + bandSum[b]), KindFormula})
		}
	}

	// Y: the headline 5-band, 4-hop cascade rooted at A1.
	if cols > seedColY && rows > 200 && nLit >= 1 {
		y1 := CellRef{0, seedColY}
		y51 := CellRef{50, seedColY}
		y101 := CellRef{100, seedColY}
		y151 := CellRef{150, seedColY}
		y201 := CellRef{200, seedColY}
		a1 := CellRef{0, 0}

		var s float64
		for r := 0; r < 10; r++ {
			s += seedLiteral(r, 0)
		}
		v1 := s
		v51 := v1 * 2
		v101 := v51 + seedLiteral(0, 0)
		// SUM(Y1:Y101) covers rows 0..100 of column Y; only Y1 and Y51 are
		// populated in that span, but every cell in the range is a real
		// dependency — filling one later must dirty Y151.
		v151 := v1 + v51
		v201 := v151 * 2

		cells = append(cells,
			seedCell{y1, fmt.Sprintf("=SUM(%s:%s)", a1, CellRef{9, 0}), fmtNum(v1), KindFormula},
			seedCell{y51, "=" + y1.String() + "*2", fmtNum(v51), KindFormula},
			seedCell{y101, "=" + y51.String() + "+" + a1.String(), fmtNum(v101), KindFormula},
			seedCell{y151, fmt.Sprintf("=SUM(%s:%s)", y1, y101), fmtNum(v151), KindFormula},
			seedCell{y201, "=" + y151.String() + "*2", fmtNum(v201), KindFormula},
		)
	}

	// A seed defines the whole grid, so it resets the row layout to the
	// canonical one; anything else would file the new cells at keys a
	// previously-mutated layout no longer agrees with. The extent is DefaultRows
	// or the seeded height, whichever is larger, so seeding 300 rows still gives
	// a sheet with room to scroll.
	next := layoutFor(max(rows, DefaultRows), keyStride)

	return sh.use(func(db *sql.DB) error {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("seed %s: begin: %w", id, err)
		}
		defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

		if _, err := tx.Exec(`DELETE FROM cells`); err != nil {
			return fmt.Errorf("seed %s: clear cells: %w", id, err)
		}
		// A seed defines the whole look of the grid too: the styles that were
		// there described cells that no longer exist, and leaving them would
		// leave dead rules in the stylesheet with nothing pointing at them.
		if _, err := tx.Exec(`DELETE FROM styles`); err != nil {
			return fmt.Errorf("seed %s: clear styles: %w", id, err)
		}
		// The two cascade levels go with it, or the sheet is left pointing at
		// style ids that no longer exist. Row records go entirely; column
		// records keep their widths, which are geometry the seed says nothing
		// about, and lose only the style.
		if _, err := tx.Exec(`DELETE FROM rows`); err != nil {
			return fmt.Errorf("seed %s: clear row state: %w", id, err)
		}
		if _, err := tx.Exec(`UPDATE cols SET style = 0 WHERE style <> 0`); err != nil {
			return fmt.Errorf("seed %s: clear column styles: %w", id, err)
		}
		if _, err := tx.Exec(
			`DELETE FROM cols WHERE style = 0 AND width = ?`, DefaultColWidth); err != nil {
			return fmt.Errorf("seed %s: drop empty column rows: %w", id, err)
		}
		if err := writeBandIndex(tx, next); err != nil {
			return fmt.Errorf("seed %s: reset bands: %w", id, err)
		}

		// Build the reference indexes after the rows, not alongside them:
		// maintaining them row by row through a 220,382-row bulk insert costs
		// ~95ms where sorting them once at the end costs ~45ms. DDL inside a
		// transaction rolls back with everything else, so a failed seed leaves
		// a sheet with its indexes rather than without them.
		for _, name := range []string{"cells_ref0", "cells_ref1", "cells_span_lo", "cells_span_hi"} {
			if _, err := tx.Exec(`DROP INDEX IF EXISTS ` + name); err != nil {
				return fmt.Errorf("seed %s: drop %s: %w", id, name, err)
			}
		}

		if err := bulkInsert(tx, len(cells), 10,
			`INSERT INTO cells (k, col, raw, computed, kind, `+slotCols+`, ref_span) VALUES `,
			` ON CONFLICT(k, col) DO UPDATE SET
			    raw = excluded.raw, computed = excluded.computed, kind = excluded.kind,
			    ref0_k = excluded.ref0_k, ref0_col = excluded.ref0_col,
			    ref1_k = excluded.ref1_k, ref1_col = excluded.ref1_col,
			    ref_span = excluded.ref_span`,
			func(i int, args []any) []any {
				c := cells[i]
				tmpl, slots, span, _ := FormulaTemplate(c.raw)
				s := slotArgs(next, slots)
				return append(args, next.keyOf(c.ref.Row), c.ref.Col, tmpl, c.computed, int(c.kind),
					s[0], s[1], s[2], s[3], boolInt(span))
			}); err != nil {
			return fmt.Errorf("seed %s cells: %w", id, err)
		}
		if _, err := tx.Exec(indexDDL); err != nil {
			return fmt.Errorf("seed %s: rebuild indexes: %w", id, err)
		}
		return sh.commitState(tx, next, newStyleState())
	})
}

// maxBindsPerStatement caps how many placeholders one batched INSERT carries.
// Batching wider is not monotonically better: inserting 220,000 ten-column rows
// takes 1.0s at 80 binds per statement, 1.9s at 1,280 and 11.0s at 10,240,
// because the cost of preparing a statement grows faster than the round trips
// it saves. The batch is therefore sized in placeholders rather than rows, so
// a five-column insert gets 25 rows a statement and a ten-column insert 12.
const maxBindsPerStatement = 128

// bulkInsert runs a multi-row INSERT in batches. One statement per row is
// ~250k round trips through database/sql for a full seed, which dominates
// everything else; batching cuts it to a few thousand.
func bulkInsert(tx *sql.Tx, n, perRow int, prefix, suffix string, fill func(i int, args []any) []any) error {
	if n == 0 {
		return nil
	}
	batch := max(1, maxBindsPerStatement/perRow)
	placeholder := "(?" + strings.Repeat(",?", perRow-1) + ")"

	var sb strings.Builder
	args := make([]any, 0, batch*perRow)
	for start := 0; start < n; start += batch {
		end := min(start+batch, n)
		sb.Reset()
		sb.WriteString(prefix)
		for i := start; i < end; i++ {
			if i > start {
				sb.WriteByte(',')
			}
			sb.WriteString(placeholder)
		}
		sb.WriteString(suffix)
		args = args[:0]
		for i := start; i < end; i++ {
			args = fill(i, args)
		}
		if _, err := tx.Exec(sb.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// SheetExists reports whether a sheet is reachable — either its database is on
// disk, or it was reaped and left a tombstone. The tombstone is deliberately
// included: `GET /s/{id}` gates on this, and the promise made when a sheet is
// cleared on a schedule is that the URL still works, so a tombstoned id opens
// (and in opening recreates) an empty sheet instead of 404ing at somebody's
// bookmark. An id that never existed is still unknown.
func SheetExists(id string) bool {
	if err := validSheetID(id); err != nil {
		return false
	}
	defaultCacheMu.Lock()
	dir := defaultDir
	defaultCacheMu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, id+".db")); err == nil {
		return true
	}
	return isTombstoned(dir, id)
}

// ─── Blank sheets, enumeration, ids ───────────────────────────────────────────
//
// There is deliberately no catalog database. A sheet is its file, so "what
// sheets exist" is a directory listing and cannot drift out of sync with the
// files. The cost is a stat per sheet on list, which stays the right trade
// until someone wants sorting or search over more sheets than a directory read
// is comfortable with.

// ErrSheetExists is returned by CreateSheet when the database file is already
// on disk. Creating is not idempotent on purpose: "make me a new sheet" and
// "open the one I have" are different intentions and silently merging them is
// how a blank sheet ends up on top of somebody's data.
var ErrSheetExists = errors.New("sheet already exists")

// SheetInfo is one row of ListSheets. Size and ModTime come straight from the
// file, which is cheap and — for a store that is one file per sheet — actually
// informative: Size is roughly how much sheet there is, ModTime is when it was
// last written.
type SheetInfo struct {
	ID      string
	Size    int64 // bytes of the main database file, excluding -wal and -shm
	ModTime time.Time
}

// CreateSheet creates an empty sheet: schema, no seed data, no events. Fails
// with ErrSheetExists if the id is taken.
func CreateSheet(id string) error {
	c, _ := defaults()
	return c.Create(id)
}

// Create makes an empty sheet in this cache. See CreateSheet.
func (c *SheetCache) Create(id string) error {
	if err := validSheetID(id); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(c.dir, id+".db")); err == nil {
		return fmt.Errorf("%w: %q", ErrSheetExists, id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("create sheet %s: %w", id, err)
	}
	// Open applies the schema, which is the whole of "create": an empty sheet
	// is a database with the tables and nothing in them.
	if _, err := c.Open(id); err != nil {
		return err
	}
	return nil
}

// ListSheets enumerates the sheets on disk, newest-written first (ties broken
// by id, so the order is total and tests are stable).
func ListSheets() ([]SheetInfo, error) {
	c, _ := defaults()
	return c.List()
}

// List enumerates the sheets in this cache's directory. A missing directory is
// an empty list, not an error — nothing has been created yet is a normal state.
func (c *SheetCache) List() ([]SheetInfo, error) {
	ents, err := os.ReadDir(c.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list sheets in %s: %w", c.dir, err)
	}
	out := make([]SheetInfo, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Only `.db` files are sheets. -wal and -shm sidecars fail this test, as
		// does a tombstone; without it a stray "x.db-wal" would become a phantom
		// sheet of that name.
		if !strings.HasSuffix(name, ".db") {
			continue
		}
		id := strings.TrimSuffix(name, ".db")
		if validSheetID(id) != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue // raced with a delete; not a sheet any more
		}
		out = append(out, SheetInfo{ID: id, Size: fi.Size(), ModTime: fi.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ModTime.Equal(out[j].ModTime) {
			return out[i].ModTime.After(out[j].ModTime)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// sheetIDAlphabet is the character set NewSheetID draws from: unambiguous
// lowercase letters and digits, all of which are legal in a NATS subject token
// and in a filename. 'l', '0' and '1' are left out so an id read aloud or
// copied by hand survives the trip.
const sheetIDAlphabet = "abcdefghijkmnopqrstuvwxyz23456789" // 33 symbols

// sheetIDLen is how many characters a generated id has. 14 characters over a
// 33-symbol alphabet is ~70 bits — unpredictable enough that a sheet URL is
// itself the capability, which is the only access control this prototype has.
const sheetIDLen = 14

// NewSheetID returns a fresh, unpredictable sheet id that satisfies
// validSheetID (and therefore is a single legal NATS subject token). It reads
// crypto/rand and panics if that fails, because a process that cannot get
// randomness must not fall back to a guessable id.
func NewSheetID() string {
	// 33 symbols is not a power of two, so reject-sample rather than take a
	// modulo — a biased id is a smaller keyspace than it looks.
	const n = len(sheetIDAlphabet)
	buf := make([]byte, sheetIDLen*2)
	out := make([]byte, 0, sheetIDLen)
	for len(out) < sheetIDLen {
		if _, err := rand.Read(buf); err != nil {
			panic("sheetstream: crypto/rand failed: " + err.Error())
		}
		for _, b := range buf {
			if len(out) == sheetIDLen {
				break
			}
			if int(b) >= 256-(256%n) {
				continue // would bias the distribution
			}
			out = append(out, sheetIDAlphabet[int(b)%n])
		}
	}
	return string(out)
}
