package main

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// storeseed.go — making sheets: the seeded demo grid, and the empty ones.
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
			panic("hypersheets: crypto/rand failed: " + err.Error())
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
