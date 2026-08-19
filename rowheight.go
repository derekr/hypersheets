package main

// rowheight.go — per-row heights.
//
// The storage was already here: `rows` has carried a `height` column alongside
// `style` since the row record existed, keyed by storage key rather than display
// rank, so a height follows its row through an insert, a delete or a rebalance on
// the same three primitives a row style does. This file is the half that reaches
// it.
//
// A height is axis config in the same sense a column width is — a default for the
// cells on that axis — and it is stored the same way: sparse, absent meaning
// default, and a row reset to every default is deleted rather than storing zeroes.

import (
	"database/sql"
	"errors"
	"fmt"
)

const (
	// MinRowHeight is one line of the smallest text the grid renders plus its
	// rule. Below this a row cannot show what is in it, and the drag handle
	// becomes hard to hit.
	MinRowHeight = 16

	// MaxRowHeight bounds one row to something a viewport can still show
	// alongside its neighbours. It is generous because fit-to-contents on a
	// wrapped paragraph is a real reason to be tall.
	MaxRowHeight = 400
)

// ErrBadHeight rejects a height outside MinRowHeight..MaxRowHeight.
var ErrBadHeight = errors.New("bad row height")

// ClampRowHeight puts a height inside the bounds rather than refusing it, which
// is what a drag wants: dragging past the minimum should stop, not fail.
func ClampRowHeight(px int) int {
	if px < MinRowHeight {
		return MinRowHeight
	}
	if px > MaxRowHeight {
		return MaxRowHeight
	}
	return px
}

// SetRowHeight sets the height of one or more display rows.
//
// Passing rowHeightPx resets a row to the default, and the record is deleted when
// nothing else is left on it — the same rule SetColWidth follows, so an
// unformatted sheet keeps zero rows in this table.
//
// Must be called from the sheet's actor goroutine.
func (s *Sheet) SetRowHeight(rows []int, px int) error {
	if len(rows) == 0 {
		return nil
	}
	if px == 0 {
		return fmt.Errorf("%w: 0 (use %d..%d, or %d to reset)",
			ErrBadHeight, MinRowHeight, MaxRowHeight, rowHeightPx)
	}
	h := ClampRowHeight(px)

	need := 0
	for _, r := range rows {
		if r < 0 || r >= RowCeiling {
			return fmt.Errorf("%w: row %d out of 0..%d", ErrBadRef, r, RowCeiling-1)
		}
		need = max(need, r+1)
	}
	if err := s.ensureRows(need); err != nil {
		return err
	}

	return s.use(func(db *sql.DB) error {
		bi := s.index()
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin row height: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

		// The default is stored as 0, not as rowHeightPx: an absent or zero
		// height means "whatever the grid's default is", so changing that default
		// later moves every unset row rather than leaving a sheet full of rows
		// pinned to the old number.
		stored := h
		if h == rowHeightPx {
			stored = 0
		}

		// ON CONFLICT names only `height`, so a row's style survives a resize —
		// the mirror of what SetRowStyle does for height.
		if err := bulkInsert(tx, len(rows), 3,
			`INSERT INTO rows (k, style, height) VALUES `,
			` ON CONFLICT(k) DO UPDATE SET height = excluded.height`,
			func(i int, args []any) []any {
				return append(args, bi.keyOf(rows[i]), 0, stored)
			}); err != nil {
			return fmt.Errorf("apply row heights: %w", err)
		}
		if stored == 0 {
			lo, hi := rows[0], rows[0]
			for _, r := range rows {
				lo, hi = min(lo, r), max(hi, r)
			}
			if _, err := tx.Exec(
				`DELETE FROM rows WHERE k BETWEEN ? AND ? AND style = 0 AND height = 0`,
				bi.keyOf(lo), bi.keyOf(hi)); err != nil {
				return fmt.Errorf("prune row records: %w", err)
			}
		}
		return tx.Commit()
	})
}

// RowHeights returns the non-default heights covering [loRow,hiRow], keyed by
// display row. Rows at the default are absent, which is what makes the render's
// per-row rules O(resized rows) rather than O(buffer).
func (s *Sheet) RowHeights(loRow, hiRow int) (map[int]int, error) {
	if loRow > hiRow {
		loRow, hiRow = hiRow, loRow
	}
	if loRow < 0 {
		loRow = 0
	}
	out := map[int]int{}
	err := s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			hi := min(hiRow, bi.rows-1)
			if loRow > hi {
				return nil
			}
			rows, err := db.Query(
				`SELECT k, height FROM rows WHERE k BETWEEN ? AND ? AND height <> 0`,
				bi.keyOf(loRow), bi.keyOf(hi))
			if err != nil {
				return fmt.Errorf("row heights %d..%d: %w", loRow, hi, err)
			}
			defer rows.Close()
			for rows.Next() {
				var k int64
				var h int
				if err := rows.Scan(&k, &h); err != nil {
					return fmt.Errorf("row height scan: %w", err)
				}
				// A key outside the requested span can come back when the band
				// holding it straddles the edge; rankOf is the authority.
				if r := bi.rankOf(k); r >= loRow && r <= hi {
					out[r] = h
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

// TotalHeight is the sheet's full pixel height: every row at the default, plus
// the difference for the rows that are not.
//
// It is one aggregate rather than a sum over RowHeights because the scroll
// container needs it for the whole sheet, not for a window, and a sheet may have
// a million rows.
func (s *Sheet) TotalHeight() (int, error) {
	total := 0
	err := s.use(func(db *sql.DB) error {
		return s.readIndex(func(bi *bandIndex) error {
			total = bi.rows * rowHeightPx
			if bi.rows == 0 {
				return nil
			}
			var sum, n sql.NullInt64
			err := db.QueryRow(
				`SELECT SUM(height), COUNT(*) FROM rows WHERE k BETWEEN ? AND ? AND height <> 0`,
				bi.keyOf(0), bi.keyOf(bi.rows-1)).Scan(&sum, &n)
			if err != nil {
				return fmt.Errorf("total height: %w", err)
			}
			if n.Valid && n.Int64 > 0 {
				total += int(sum.Int64) - int(n.Int64)*rowHeightPx
			}
			return nil
		})
	})
	return total, err
}
