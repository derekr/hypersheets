package main

import (
	"container/list"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// storecache.go — which sheets are open, and what happens when one is not.
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
