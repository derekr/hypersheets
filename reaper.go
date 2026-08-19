package main

// reaper.go — retention. Sheets nobody has edited for a while are cleared, and
// their URLs keep working.
//
// This is a public demo with no authentication and no delete button, so without
// a reaper the disk and the 500-sheet ceiling are both one-way ratchets and the
// only thing that removes a sheet is somebody logging in and running `rm`. Three
// rules shape it.
//
// Idle means unwritten, not unread. `SheetInfo.ModTime` is the mtime of the
// sheet's `.db` file, and under WAL a read never writes to it
// (TestReadingASheetDoesNotAgeIt). A sheet somebody watches on a wall display
// but never edits is therefore aging, which is correct: the next rule catches
// the case where they are still there.
//
// A sheet with a live connection is never reaped. Unlinking a database out from
// under an open stream corrupts nothing — the handle survives the unlink on
// POSIX — but it leaves a reader looking at a sheet that no longer exists, whose
// next edit lands in a file nothing can find.
//
// A reaped URL still works. Deletion leaves a tombstone (store.go) and the next
// visit opens a fresh empty sheet at the same id. The id is the capability;
// taking the data is defensible, taking the address is not.
//
// Deletion is irreversible and there is no export, so the TTL default is set by
// the worst plausible case — somebody makes a sheet, shares the link, and comes
// back after a holiday. It is two weeks rather than the obvious one because the
// acute defence against accumulation is the per-IP sheet quota in limits.go,
// which refuses the twenty-sixth sheet from one address within minutes. The
// reaper is the chronic one and can afford to be gentle because something faster
// stands in front of it.

import (
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ─── Policy ───────────────────────────────────────────────────────────────────

const (
	// DefaultSheetTTL is how long a sheet may go without a write before it is
	// cleared. See the header for why it is two weeks and not one.
	DefaultSheetTTL = 14 * 24 * time.Hour
	// DefaultSweepEvery is how often the retention sweep runs. Hourly is far more
	// often than a two-week TTL needs, but it costs one directory read plus one
	// stat per sheet — the same work `GET /` already does — and it keeps the reap
	// log a steady trickle rather than a once-a-day cliff.
	DefaultSweepEvery = time.Hour
)

// ReaperPolicy is every retention number in one struct. Zero TTL disables the
// reaper entirely.
type ReaperPolicy struct {
	Enabled bool

	// TTL is the idle-since-last-write age at which a sheet is cleared.
	TTL time.Duration
	// Every is the sweep interval.
	Every time.Duration
	// TombstoneTTL expires the `{id}.gone` markers themselves, after which the
	// URL 404s again. Zero keeps them forever, which is the default: a marker is
	// one line on disk, and breaking a bookmark silently, weeks after the data
	// went, is a worse failure than a directory with a lot of small files in it.
	TombstoneTTL time.Duration

	// Keep is the set of ids the sweep never touches. main.go puts the seeded
	// demo sheet in it: that sheet is furniture, recreated by the boot path when
	// missing, and clearing it would empty the front door rather than somebody's
	// document.
	Keep map[string]bool
}

// DefaultReaperPolicy is the shipped retention policy.
func DefaultReaperPolicy() ReaperPolicy {
	return ReaperPolicy{
		Enabled: true,
		TTL:     DefaultSheetTTL,
		Every:   DefaultSweepEvery,
	}
}

// ─── The sweep ────────────────────────────────────────────────────────────────

// SweepResult is one sweep, for the log line and for tests.
type SweepResult struct {
	Scanned     int   // sheets on disk
	Reaped      int   // sheets cleared
	Bytes       int64 // bytes freed
	SkippedLive int   // over age, but somebody is connected
	SkippedKept int   // over age, but on the keep list
	// SkippedWriting is over age by the `.db` mtime but not by the WAL's —
	// a sheet under continuous writing whose checkpoint has not caught up.
	SkippedWriting int
	Tombstones     int // markers expired (only when TombstoneTTL > 0)
	Errors         int
}

// viewerSource is the half of the registry the reaper needs: "which of these
// sheets has somebody connected to it right now". It is an interface so the
// tests can answer the question without standing up a bus.
type viewerSource interface {
	sheetsWithViewers(ids []string) map[string]bool
}

// Reaper runs the retention sweep on a ticker.
type Reaper struct {
	p     ReaperPolicy
	conns viewerSource
	quota *sheetQuota

	// now, list, del and tombs are seams so a test can drive the clock and the
	// filesystem. In production they are time.Now and the process-wide store.
	now    func() time.Time
	list   func() ([]SheetInfo, error)
	del    func(id string) error
	tombs  func() ([]SheetInfo, error)
	untomb func(id string) error
	// size is db + WAL, not SheetInfo.Size. A sheet whose last write has not
	// been checkpointed keeps most of itself in the `-wal` sidecar, so the
	// listing's `.db` size can be a fraction of what the reap actually frees,
	// and an audit log that under-reports by 10x is worse than none.
	size func(id string) int64
	// lastWrite refines a candidate's age with the WAL — see sheetLastWrite.
	lastWrite func(id string, dbMod time.Time) time.Time

	reaped atomic.Uint64
	freed  atomic.Uint64

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

// NewReaper builds the reaper over the process-wide store. conns may be nil, in
// which case no sheet is ever reaped: refusing to delete is the correct failure
// when the thing that answers "is anybody looking at this" is missing.
func NewReaper(p ReaperPolicy, conns viewerSource, quota *sheetQuota) *Reaper {
	if p.Every <= 0 {
		p.Every = DefaultSweepEvery
	}
	return &Reaper{
		p:      p,
		conns:  conns,
		quota:  quota,
		now:    time.Now,
		list:   ListSheets,
		del:    DeleteSheet,
		tombs:  func() ([]SheetInfo, error) { c, _ := defaults(); return c.Tombstones() },
		untomb: func(id string) error { c, _ := defaults(); return c.ForgetTombstone(id) },
		size:   func(id string) int64 { c, _ := defaults(); return sheetFileBytes(c.dir, id) },
		lastWrite: func(id string, dbMod time.Time) time.Time {
			c, _ := defaults()
			return sheetLastWrite(c.dir, id, dbMod)
		},
		stop: make(chan struct{}),
	}
}

// useCache points the reaper at one cache instead of the process-wide store.
// Production uses the defaults NewReaper installs; this is how a test gets its
// own directory without touching global state.
func (r *Reaper) useCache(c *SheetCache) *Reaper {
	r.list, r.del, r.tombs, r.untomb = c.List, c.Delete, c.Tombstones, c.ForgetTombstone
	r.size = func(id string) int64 { return sheetFileBytes(c.dir, id) }
	r.lastWrite = func(id string, dbMod time.Time) time.Time { return sheetLastWrite(c.dir, id, dbMod) }
	return r
}

// active reports whether this reaper will ever delete anything.
func (r *Reaper) active() bool { return r != nil && r.p.Enabled && r.p.TTL > 0 }

// Start runs a sweep immediately and then every `Every`. The first sweep is
// immediate so that the policy can be verified by restarting the server rather
// than by waiting an hour after a deploy.
func (r *Reaper) Start() {
	if !r.active() {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.logSweep(r.Sweep())
		t := time.NewTicker(r.p.Every)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.logSweep(r.Sweep())
			}
		}
	}()
}

// Close stops the sweeper. A sweep already running finishes first, because
// abandoning one halfway through would leave a sheet unlinked with no
// tombstone.
func (r *Reaper) Close() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() { close(r.stop) })
	r.wg.Wait()
}

// Sweep clears every sheet that is over the TTL, is not on the keep list, and
// has nobody connected to it. It is safe to call directly and returns what it
// did, which is what the tests assert on.
func (r *Reaper) Sweep() SweepResult {
	var res SweepResult
	if !r.active() {
		return res
	}
	sheets, err := r.list()
	if err != nil {
		obsLog.Warn("reap.list", "err", err.Error())
		res.Errors++
		return res
	}
	res.Scanned = len(sheets)

	now := r.now()
	cutoff := now.Add(-r.p.TTL)

	// Two passes, so the registry is asked once, about the candidates only. It
	// answers under one lock, so a sweep over 500 sheets does not take and
	// release the connection lock 500 times while people are typing.
	var candidates []SheetInfo
	for _, sh := range sheets {
		if !sh.ModTime.Before(cutoff) {
			continue
		}
		if r.p.Keep[sh.ID] {
			res.SkippedKept++
			continue
		}
		// The `.db` mtime is the last checkpoint, not the last write. Refining
		// with the WAL costs one stat, and only for a sheet that has already
		// failed the cheap test — a WAL can only ever make a sheet look younger,
		// so nothing that passed the first filter needs asking twice.
		if r.lastWrite != nil {
			if w := r.lastWrite(sh.ID, sh.ModTime); !w.Before(cutoff) {
				res.SkippedWriting++
				obsLog.Info("reap.skip_uncheckpointed", "sheet", sh.ID,
					"db_mtime", sh.ModTime.UTC().Format(time.RFC3339),
					"last_write", w.UTC().Format(time.RFC3339))
				continue
			}
		}
		candidates = append(candidates, sh)
	}
	if len(candidates) == 0 {
		res.Tombstones = r.expireTombstones(now, &res)
		return res
	}

	ids := make([]string, len(candidates))
	for i, sh := range candidates {
		ids[i] = sh.ID
	}
	live := map[string]bool{}
	if r.conns != nil {
		live = r.conns.sheetsWithViewers(ids)
	} else {
		// No way to ask. Reap nothing rather than guess.
		obsLog.Warn("reap.no_registry", "candidates", len(candidates))
		res.SkippedLive = len(candidates)
		return res
	}

	for _, sh := range candidates {
		if live[sh.ID] {
			res.SkippedLive++
			obsLog.Info("reap.skip_live", "sheet", sh.ID,
				"age", humanDuration(now.Sub(sh.ModTime)), "bytes", sh.Size)
			continue
		}
		// Measured before the unlink, and including the WAL.
		bytes := sh.Size
		if r.size != nil {
			if n := r.size(sh.ID); n > bytes {
				bytes = n
			}
		}
		if err := r.del(sh.ID); err != nil {
			res.Errors++
			obsLog.Warn("reap.delete", "sheet", sh.ID, "err", err.Error())
			continue
		}
		// The quota is returned the instant the file is gone rather than on the
		// next create: the per-IP cap is a cap on live sheets, and a sheet that
		// has been cleared is not one.
		if r.quota != nil {
			r.quota.release(sh.ID)
		}
		res.Reaped++
		res.Bytes += bytes
		r.reaped.Add(1)
		r.freed.Add(uint64(bytes))
		// Id, age and size are logged for every reap, so the policy can be
		// audited from the log alone — "what did it take, and was it really idle"
		// is answerable without the sheet existing any more.
		obsLog.Info("reap.sheet",
			"sheet", sh.ID,
			"age", humanDuration(now.Sub(sh.ModTime)),
			"age_h", int(now.Sub(sh.ModTime).Hours()),
			"idle_since", sh.ModTime.UTC().Format(time.RFC3339),
			"bytes", bytes,
			"ttl", humanDuration(r.p.TTL))
	}
	res.Tombstones = r.expireTombstones(now, &res)
	return res
}

// expireTombstones removes markers older than TombstoneTTL. Disabled (0) by
// default — see ReaperPolicy.
func (r *Reaper) expireTombstones(now time.Time, res *SweepResult) int {
	if r.p.TombstoneTTL <= 0 || r.tombs == nil {
		return 0
	}
	marks, err := r.tombs()
	if err != nil {
		res.Errors++
		obsLog.Warn("reap.tombs", "err", err.Error())
		return 0
	}
	cutoff := now.Add(-r.p.TombstoneTTL)
	n := 0
	for _, m := range marks {
		if !m.ModTime.Before(cutoff) {
			break // Tombstones() is oldest-first, so the rest are younger.
		}
		if err := r.untomb(m.ID); err != nil {
			res.Errors++
			continue
		}
		n++
		obsLog.Info("reap.tombstone", "sheet", m.ID,
			"age_h", int(now.Sub(m.ModTime).Hours()))
	}
	return n
}

// logSweep prints one line per sweep, and only when it did something. A
// two-week TTL means most sweeps are no-ops and an hourly "reaped 0" would
// bury the ones that matter.
func (r *Reaper) logSweep(res SweepResult) {
	if res.Reaped == 0 && res.SkippedLive == 0 && res.SkippedWriting == 0 &&
		res.Errors == 0 && res.Tombstones == 0 {
		return
	}
	log.Printf("retention sweep: %d sheets scanned, %d cleared (%s freed), %d kept for live viewers, %d kept for pending writes, %d tombstones expired, %d errors",
		res.Scanned, res.Reaped, humanSize(res.Bytes), res.SkippedLive, res.SkippedWriting,
		res.Tombstones, res.Errors)
}

// Stats is what the reaper has taken so far, for the shutdown line.
func (r *Reaper) Stats() (reaped uint64, bytes uint64) {
	if r == nil {
		return 0, 0
	}
	return r.reaped.Load(), r.freed.Load()
}

// Describe is the one-line startup summary, so a running server states its own
// retention policy rather than making somebody read this file.
func (r *Reaper) Describe() string {
	if !r.active() {
		return "retention: OFF — sheets are kept forever"
	}
	s := "retention: sheets cleared after " + humanDuration(r.p.TTL) +
		" without an edit (swept every " + humanDuration(r.p.Every) +
		"); URLs keep working and reopen an empty sheet"
	if r.p.TombstoneTTL > 0 {
		s += "; tombstones expire after " + humanDuration(r.p.TombstoneTTL)
	}
	if len(r.p.Keep) > 0 {
		s += "; never clearing"
		for id := range r.p.Keep {
			s += " " + id
		}
	}
	return s
}

// ─── The retention notice ─────────────────────────────────────────────────────

// sheetTTLFlag is the configured retention period, set once by main.go before
// the server serves and only read afterwards. Same discipline as regionFlag
// (latency.go) and readOnlySheets (readonly.go), for the same reason:
// configuration is not state, so it needs no lock and no plumbing through six
// call sites to reach a render.
var sheetTTLFlag time.Duration

// retentionChipHTML states the retention policy inside the sheet.
//
// The index page already says it, which is enough for somebody arriving through
// the front door and nothing at all for somebody opening a bookmark — and the
// bookmark case is precisely the one where a sheet has been cleared. Without
// this, a reaped sheet reopens as a working blank grid with no explanation
// anywhere on it, which reads as data loss rather than as policy.
//
// It sits next to the latency chip because it is the same kind of statement: an
// honest fact about this deployment the reader would otherwise discover by being
// surprised. Both are `flex:0 0 auto` lodgers in a strip with room to spare, and
// neither is re-sent by a push.
//
// A read-only sheet renders nothing here, because the sentence would be false:
// main.go puts every read-only id in the reaper's Keep set.
func retentionChipHTML(sheetID string) string {
	if sheetTTLFlag <= 0 || isReadOnly(sheetID) {
		return ""
	}
	d := humanDuration(sheetTTLFlag)
	return `<span id="rt" title="Sheets that go ` + d + ` without an edit are cleared. ` +
		`The link keeps working — it reopens an empty sheet.">cleared after ` + d + ` idle</span>`
}

// humanDuration says "14 days" rather than "336h0m0s". This number is shown to
// visitors — in the refusal a quota-exhausted creator gets, and on the index
// page — and `336h0m0s` is not a retention policy anybody can read.
func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return strconv.Itoa(int(d.Hours()/24)) + " days"
	case d >= 24*time.Hour:
		return "1 day"
	case d >= 2*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " hours"
	case d >= time.Hour:
		return "1 hour"
	case d >= 2*time.Minute:
		return strconv.Itoa(int(d.Minutes())) + " minutes"
	default:
		return d.String()
	}
}
