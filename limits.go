package main

// limits.go — the abuse policy for a shared demo, in one file.
//
// The threat model is accidental and casual abuse, not an attacker. There is no
// authentication and there is not meant to be: a sheet id is 14 characters of
// crypto/rand over 33 symbols (~70 bits), so the URL is the capability. What is
// defended is the machine — a bored visitor holding down a key, a script that
// discovers `POST /sheets` returns a fresh database, a tab left open in a loop.
// Every number here is a ceiling far above any human, not a quota.
//
// It is middleware and nothing else, which is why it fits in one file: every
// policy decision is made before a handler runs. The three things a middleware
// cannot see — a cell value's length, the event log's growth, and the row a
// write addresses — are not enforced here; the per-sheet byte cap is the
// backstop that makes their absence survivable.
//
//	class    what it covers                                     rate      burst
//	create   POST /sheets                                       6/min     12
//	write    /cell /clear /fill /paste /style /colwidth         60/s      300
//	         /rows /cols
//	nav      /viewport /sel                                     120/s     600
//	stream   GET /s/{id}/live                                   2/s       30
//	page     GET / and GET /s/{id}                              60/s      300
//
//	plus: 32 concurrent streams per IP, 64 in the process, 500 sheets on
//	disk, 25 live sheets per IP, 2 GB of sheets on disk, 256 MB per sheet,
//	64 KB per request body.
//
// The classes hold separate buckets so they cannot starve one another. A
// held-open SSE stream is the read half of the architecture and must never be
// killed by a write limit — a viewer who typed too fast should be told "no" and
// go on watching, not be disconnected. `nav` is separate for the mirror reason:
// a scroll burst crossing a buffer edge must not spend the tokens an edit
// needs.
//
// Where the numbers come from:
//
//   - Writes. Fast typing commits ~10 cells/s and key auto-repeat on Delete is
//     ~30/s, so 60/s per IP is 6x a fast typist and 2x a held key — five people
//     behind one NAT typing flat out still fit. The 300 burst absorbs a
//     legitimate flurry (paste, fill, bold, colour, five cells).
//   - Creation. Each create writes a new SQLite file, so unbounded creation is
//     the top concern. Twelve in a row covers clicking "New sheet" as fast as
//     anyone can mean it; the sustained 6/min is still 8,640 a day, which is
//     why the count and byte caps sit underneath it and why the count is capped
//     per address too — a rate limit decides how fast one address fills the
//     shared 500-slot ceiling, not whether it can.
//   - Pages. `GET /s/{id}` is the one read that is expensive without being a
//     stream: it materializes a ~6,500-cell window in 3.7-4.9 ms. 60/s is far
//     past reload-mashing and bounds a flood to about a quarter of one core.
//     `GET /` shares the bucket; it is a directory read plus a stat per sheet,
//     so it is cheap only while the sheet count is.
//   - Streams. Each open stream is a goroutine that re-renders on every
//     invalidation of its bands, so concurrency is the number that matters and
//     the rate only smooths reload-mashing.
//   - Bodies. Datastar posts its signal store on every command, well under
//     2 KB. 64 KB is 30x that and still stops a multi-megabyte cell value from
//     reaching the store — an imperfect stand-in for a cell-length cap.
//
// The 64-stream process cap is measured rather than reasoned. Every viewer
// independently re-reads the same dirty cells on every edit, so the cost is
// streams x edits, and edits is the same ~10 cells/s the write bucket is sized
// against. At the measured ~1.1 ms of CPU per stream per edit, 64 streams under
// a typing burst cost 1.4-2.1 cores of the 2 vCPU deployment box. It survives;
// four times that queues, and the realtime demo stops being realtime. 64 also
// keeps the server's contribution to push latency under the network round trip
// the demo already pays, so the server is never the larger half of the delay.
// 128 is the "breadth over feel" setting if reach ever matters more. The
// contention is real work, not a pool-sizing accident: widening a sheet's
// connection pool moves the queue into the Go scheduler rather than removing it.
//
// 32 concurrent per IP is half the process cap rather than a small fraction of
// it, on purpose: the per-IP number is about a room of people behind one NAT,
// not about defending the box — the process cap does that — and refusing the
// seventeenth person in a room is a worse failure than letting one address hold
// half the connections.
//
// X-Forwarded-For is not trusted by default: a spoofed header turns one client
// into an unlimited number of them, which would make every bucket here
// decorative. `-trust-forwarded-for` opts in when there really is a proxy in
// front, and reads the rightmost entry — the one the nearest proxy appended and
// therefore the only one the client could not have written.

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ─── Token buckets ────────────────────────────────────────────────────────────

// bucket is one key's allowance. Refill is lazy — no timer, no goroutine — so
// an idle key costs exactly the two words it occupies.
type bucket struct {
	tokens float64
	last   time.Duration
}

// tokenBuckets is one policy class: a rate, a burst, and a map of keys to
// allowances. The map is swept rather than reaped on a timer, for the same
// reason the refill is lazy.
type tokenBuckets struct {
	rate  float64 // tokens per second; <= 0 disables the class entirely
	burst float64

	mu    sync.Mutex
	at    map[string]*bucket
	swept time.Duration
}

// bucketSweepEvery and bucketKeyCap bound the map. A full bucket is
// indistinguishable from a key that was never seen, so sweeping full ones is
// lossless — which is why the sweep can be this crude.
const (
	bucketSweepEvery = 5 * time.Minute
	bucketKeyCap     = 8192
)

func newTokenBuckets(rate, burst float64) *tokenBuckets {
	if rate <= 0 || burst < 1 {
		return nil // disabled; allow() short-circuits on nil
	}
	return &tokenBuckets{rate: rate, burst: burst, at: make(map[string]*bucket)}
}

// allow takes one token for key. It returns false plus how long until a token
// exists, which is what the Retry-After header wants.
//
// `key` is a slice of the request rather than a fresh string — hostOf slices
// RemoteAddr — so the lookup allocates nothing on the common path of an
// existing key. The clone happens only when a key is first inserted.
func (b *tokenBuckets) allow(key string, now time.Duration) (bool, time.Duration) {
	if b == nil {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.at[key]
	if e == nil {
		if now-b.swept > bucketSweepEvery || len(b.at) >= bucketKeyCap {
			b.sweepLocked(now)
		}
		e = &bucket{tokens: b.burst, last: now}
		b.at[strings.Clone(key)] = e
	} else if d := now - e.last; d > 0 {
		e.tokens = math.Min(b.burst, e.tokens+float64(d)/float64(time.Second)*b.rate)
		e.last = now
	}

	if e.tokens >= 1 {
		e.tokens--
		return true, 0
	}
	wait := time.Duration((1 - e.tokens) / b.rate * float64(time.Second))
	if wait < time.Second {
		wait = time.Second // Retry-After is whole seconds; never advertise 0
	}
	return false, wait
}

// sweepLocked drops every key whose bucket has refilled to full. Such a key
// carries no information: a request from it would be allowed either way.
func (b *tokenBuckets) sweepLocked(now time.Duration) {
	for k, e := range b.at {
		refilled := e.tokens + float64(now-e.last)/float64(time.Second)*b.rate
		if refilled >= b.burst {
			delete(b.at, k)
		}
	}
	b.swept = now
	// A sweep that freed nothing (every key genuinely active) must not leave
	// the map to grow without bound. Dropping it wholesale forgives everyone
	// once, which is the friendly failure for a friendly demo.
	if len(b.at) >= bucketKeyCap {
		b.at = make(map[string]*bucket)
	}
}

// ─── Concurrency counters ─────────────────────────────────────────────────────

// liveCounts tracks how many streams each key is holding open right now, plus
// the process total. Unlike a rate, concurrency has to be given back, so every
// acquire is paired with a release in a defer.
type liveCounts struct {
	mu    sync.Mutex
	n     map[string]int
	total int
}

func (c *liveCounts) acquire(key string, perKey, total int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if total > 0 && c.total >= total {
		return false
	}
	if perKey > 0 && c.n[key] >= perKey {
		return false
	}
	if c.n == nil {
		c.n = make(map[string]int)
	}
	c.n[strings.Clone(key)]++
	c.total++
	return true
}

func (c *liveCounts) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := c.n[key]; n <= 1 {
		delete(c.n, key)
	} else {
		c.n[key] = n - 1
	}
	if c.total > 0 {
		c.total--
	}
}

func (c *liveCounts) snapshot() (perKey int, total int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.n), c.total
}

// counts is what one key is holding and what the process is holding. A refusal
// needs to say which cap it hit: "close a tab" and "the demo is full" are each
// wrong advice in the other's case.
func (c *liveCounts) counts(key string) (held int, total int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[key], c.total
}

// ─── Per-IP sheet quota ───────────────────────────────────────────────────────
//
// A global 500-sheet cap alone is a denial of service by accumulation: 6/min
// sustained fills it in about 83 minutes, and from then on nobody can create a
// sheet. So the count is capped per address as well, and on live sheets rather
// than lifetime creations — the reaper hands the slot back when it clears a
// sheet, so somebody who used the demo properly a month ago carries no debt.
//
// The ledger is in memory and a restart forgives it. Persisting it would mean a
// per-sheet record of who created it, which is both a catalog (store.go refuses
// to have one, on purpose) and a log of visitor IP addresses on a service that
// otherwise keeps none. In-memory still buys the property that matters — one
// address cannot fill the box during a process lifetime — and the global caps
// underneath survive a restart, so the floor holds.

// sheetQuota is the per-IP ledger of live sheets. Two maps, because both
// questions get asked: "how many does this address have" on every create, and
// "whose was this" on every reap.
type sheetQuota struct {
	mu    sync.Mutex
	owner map[string]string // sheet id -> the address that created it
	n     map[string]int    // address -> live sheets
}

func newSheetQuota() *sheetQuota {
	return &sheetQuota{owner: make(map[string]string), n: make(map[string]int)}
}

// held is how many live sheets this address is credited with.
func (q *sheetQuota) held(ip string) int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.n[ip]
}

// add records that ip created sheet id. Idempotent: a second call for the same
// id does not double-charge, which is what keeps a retried create honest.
func (q *sheetQuota) add(ip, id string) {
	if q == nil || ip == "" || id == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.owner[id]; ok {
		return
	}
	ip = strings.Clone(ip)
	q.owner[strings.Clone(id)] = ip
	q.n[ip]++
}

// release gives the slot back. Called by the reaper the instant a sheet's file
// is gone; a no-op for a sheet this process did not see created.
func (q *sheetQuota) release(id string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	ip, ok := q.owner[id]
	if !ok {
		return
	}
	delete(q.owner, id)
	if n := q.n[ip]; n <= 1 {
		delete(q.n, ip)
	} else {
		q.n[ip] = n - 1
	}
}

// snapshot is (addresses tracked, sheets attributed).
func (q *sheetQuota) snapshot() (addrs, sheets int) {
	if q == nil {
		return 0, 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.n), len(q.owner)
}

// ─── Policy ───────────────────────────────────────────────────────────────────

// IndexMode decides what `GET /` does. A listing defeats the capability-URL
// property of a random sheet id: anyone who loads the root can see and open
// every sheet on the box. For a small trusted group that may be exactly right,
// so the listing is the default and the two restrictions are opt-in.
type IndexMode string

const (
	// IndexList delegates to index.go's handler: every sheet, newest first.
	IndexList IndexMode = "list"
	// IndexCreate serves the same page furniture without the listing — the
	// "New sheet" button still works, so the front door is not broken, but a
	// sheet URL is once again the only way to reach a sheet.
	IndexCreate IndexMode = "create"
	// IndexOff 404s the root entirely.
	IndexOff IndexMode = "off"
)

func validIndexMode(m IndexMode) bool {
	return m == IndexList || m == IndexCreate || m == IndexOff
}

// LimitPolicy is every number in one struct. Zero disables the individual
// limit it belongs to; Enabled=false disables the whole middleware.
type LimitPolicy struct {
	Enabled bool

	CreatePerMin float64
	CreateBurst  float64
	WritePerSec  float64
	WriteBurst   float64
	NavPerSec    float64
	NavBurst     float64
	StreamPerSec float64
	StreamBurst  float64
	PagePerSec   float64
	PageBurst    float64

	StreamsPerIP int
	StreamsTotal int

	MaxSheets     int
	MaxTotalBytes int64
	MaxSheetBytes int64
	MaxBodyBytes  int64

	// MaxSheetsPerIP caps how many live sheets one address may hold; 0
	// leaves only the global caps. See the per-IP quota section above.
	MaxSheetsPerIP int

	// SheetTTL is the retention period, for the sentences that mention it —
	// the over-quota refusal and the index page. It enforces nothing; the
	// reaper does that. Zero omits the clause rather than promising a
	// retention policy that is not running.
	SheetTTL time.Duration

	// DataDir is where the sheet files live — the same directory the store was
	// configured with. Empty disables both disk caps, because a cap that
	// measures the wrong directory is worse than none.
	DataDir string

	TrustForwardedFor bool
	Index             IndexMode
	// AdminEmail is the address allowed to see the full sheet listing when
	// Index is create/off. It is matched against the X-ExeDev-Email header the
	// exe.dev proxy adds to authenticated requests. Empty means no one can, and
	// the listing does not exist. See serveIndex.
	AdminEmail string
}

// DefaultLimitPolicy is the shipped policy. See the header for why each number
// is what it is.
func DefaultLimitPolicy() LimitPolicy {
	return LimitPolicy{
		Enabled:      true,
		CreatePerMin: 6,
		CreateBurst:  12,
		WritePerSec:  60,
		WriteBurst:   300,
		NavPerSec:    120,
		NavBurst:     600,
		StreamPerSec: 2,
		StreamBurst:  30,
		PagePerSec:   60,
		PageBurst:    300,

		StreamsPerIP: 32,
		// Measured rather than reasoned; see the header.
		StreamsTotal: 64,

		MaxSheets:     500,
		MaxTotalBytes: 2 << 30,   // 2 GB of sheets on disk
		MaxSheetBytes: 256 << 20, // 256 MB in any one sheet
		MaxBodyBytes:  64 << 10,  // 64 KB per request body

		// Three numbers pin 25 live sheets per address: it is twice the create
		// burst, so clicking "New sheet" as fast as anyone can mean it, twice
		// over, is still not refused; it is 5% of the global cap, so filling
		// the box takes twenty distinct addresses; and at the sustained 6/min
		// rate an address reaches it in about four minutes rather than 83, so a
		// runaway loop hurts only itself long before the global cap.
		MaxSheetsPerIP: 25,
		SheetTTL:       DefaultSheetTTL,

		Index:      IndexList,
		AdminEmail: "",
	}
}

// LimitStats is what the process refused, for the log line on shutdown and for
// tests. Counters only; nothing here is on a hot path.
type LimitStats struct {
	Create uint64
	Write  uint64
	Nav    uint64
	Stream uint64
	Page   uint64
	Body   uint64
	Disk   uint64
	Quota  uint64
	// ReadOnly is writes refused because the sheet is fixed rather than
	// because anybody was going too fast. See readonly.go.
	ReadOnly uint64
	Streams  int

	// QuotaAddrs and QuotaSheets are the ledger's size: how many addresses are
	// holding sheets, and how many sheets this process has attributed.
	QuotaAddrs  int
	QuotaSheets int
}

// Limiter is the middleware. One per process.
type Limiter struct {
	p LimitPolicy

	create *tokenBuckets
	write  *tokenBuckets
	nav    *tokenBuckets
	stream *tokenBuckets
	page   *tokenBuckets

	live  liveCounts
	quota *sheetQuota

	// notify delivers a refusal to the one screen that asked, over its own SSE
	// stream, so a refused write releases the pending chip and says why instead
	// of hanging until the client's 20-second timeout. Optional: without it a
	// refusal is still a 4xx, which the editor's error path turns into a
	// visible revert (see T.fail in keys.go).
	notify func(connID, msg string)

	// now is the clock, as a monotonic duration since construction. A field so
	// a test can drive time instead of sleeping.
	now func() time.Duration

	// indexNext is the wrapped mux, kept so IndexList can delegate `GET /` to
	// index.go's real handler instead of reimplementing it. Set by Middleware,
	// which is called once at boot.
	indexNext http.Handler

	// indexHTML is the create-only index page, built once because the
	// retention sentence it carries comes from the policy.
	indexHTML string

	refCreate, refWrite, refNav, refStream, refPage, refBody, refDisk, refQuota atomic.Uint64
	// refReadOnly counts writes refused because the sheet is configured
	// read-only. Not a rate limit and sharing no bucket with one; it lives
	// here because this is where refusals are counted.
	refReadOnly atomic.Uint64
}

// NewLimiter builds the middleware. A policy with Enabled=false still returns a
// Limiter — Middleware then hands the request straight through — so the wiring
// in main.go has no conditional in it.
func NewLimiter(p LimitPolicy) *Limiter {
	if !validIndexMode(p.Index) {
		p.Index = IndexList
	}
	origin := time.Now()
	l := &Limiter{p: p, now: func() time.Duration { return time.Since(origin) }}
	l.quota = newSheetQuota()
	l.indexHTML = indexCreateOnlyHTML(p.SheetTTL)
	if !p.Enabled {
		return l
	}
	l.create = newTokenBuckets(p.CreatePerMin/60, p.CreateBurst)
	l.write = newTokenBuckets(p.WritePerSec, p.WriteBurst)
	l.nav = newTokenBuckets(p.NavPerSec, p.NavBurst)
	l.stream = newTokenBuckets(p.StreamPerSec, p.StreamBurst)
	l.page = newTokenBuckets(p.PagePerSec, p.PageBurst)
	return l
}

// Notify wires the per-screen refusal channel. Called from main.go once the
// server exists, because the server is what owns the screens.
func (l *Limiter) Notify(fn func(connID, msg string)) { l.notify = fn }

// Quota is the per-IP live-sheet ledger. The reaper needs it so that clearing
// a sheet hands its owner's slot back.
func (l *Limiter) Quota() *sheetQuota { return l.quota }

// Stats reports what has been refused so far.
func (l *Limiter) Stats() LimitStats {
	_, total := l.live.snapshot()
	addrs, sheets := l.quota.snapshot()
	return LimitStats{
		Create:      l.refCreate.Load(),
		Write:       l.refWrite.Load(),
		Nav:         l.refNav.Load(),
		Stream:      l.refStream.Load(),
		Page:        l.refPage.Load(),
		Body:        l.refBody.Load(),
		Disk:        l.refDisk.Load(),
		Quota:       l.refQuota.Load(),
		ReadOnly:    l.refReadOnly.Load(),
		Streams:     total,
		QuotaAddrs:  addrs,
		QuotaSheets: sheets,
	}
}

// Describe is the one-line startup summary, so a running server states its own
// policy rather than making anyone read this file.
func (l *Limiter) Describe() string {
	if !l.p.Enabled {
		return "limits DISABLED"
	}
	var b strings.Builder
	b.WriteString("limits: create ")
	b.WriteString(strconv.FormatFloat(l.p.CreatePerMin, 'g', -1, 64))
	b.WriteString("/min (burst ")
	b.WriteString(strconv.FormatFloat(l.p.CreateBurst, 'g', -1, 64))
	b.WriteString("), writes ")
	b.WriteString(strconv.FormatFloat(l.p.WritePerSec, 'g', -1, 64))
	b.WriteString("/s (burst ")
	b.WriteString(strconv.FormatFloat(l.p.WriteBurst, 'g', -1, 64))
	b.WriteString("), pages ")
	b.WriteString(strconv.FormatFloat(l.p.PagePerSec, 'g', -1, 64))
	b.WriteString("/s, streams ")
	b.WriteString(strconv.FormatFloat(l.p.StreamPerSec, 'g', -1, 64))
	b.WriteString("/s, ")
	b.WriteString(strconv.Itoa(l.p.StreamsPerIP))
	b.WriteString(" concurrent/IP, ")
	b.WriteString(strconv.Itoa(l.p.StreamsTotal))
	b.WriteString(" total; ")
	b.WriteString(strconv.Itoa(l.p.MaxSheets))
	b.WriteString(" sheets (")
	b.WriteString(strconv.Itoa(l.p.MaxSheetsPerIP))
	b.WriteString("/IP) / ")
	b.WriteString(humanSize(l.p.MaxTotalBytes))
	b.WriteString(" on disk, ")
	b.WriteString(humanSize(l.p.MaxSheetBytes))
	b.WriteString("/sheet, ")
	b.WriteString(humanSize(l.p.MaxBodyBytes))
	b.WriteString(" bodies; index=")
	b.WriteString(string(l.p.Index))
	if l.p.TrustForwardedFor {
		b.WriteString("; TRUSTING X-Forwarded-For")
	}
	return b.String()
}

// ─── Request classification ───────────────────────────────────────────────────

type reqClass uint8

const (
	classOther reqClass = iota
	classIndex
	classCreate
	classStream
	classWrite
	classNav
	classPage
)

// classify decides which bucket a request draws from without allocating: the
// sheet id comes back as a slice of the path, not a copy.
//
// It duplicates Routes()'s patterns rather than reading them, which is a
// deliberate cost: http.ServeMux does not expose its routing table, and a
// middleware running after the mux could not refuse a request before the
// handler had already done the work. The duplication is one switch, and a test
// walks every route in Routes() through it.
func classify(method, path string) (reqClass, string) {
	switch method {
	case http.MethodGet:
		if path == "/" {
			return classIndex, ""
		}
		// The immutable script bundle shares the page bucket rather than
		// having one of its own: it is fetched at most once per page load and
		// then cached for a year, so it cannot outpace the pages that ask for
		// it, and a bucket of its own would be one more limit to keep in step
		// for no measurable protection.
		if strings.HasPrefix(path, "/a/") {
			return classPage, ""
		}
		id, verb, ok := splitSheetPath(path)
		if ok && verb == "live" {
			return classStream, id
		}
		if !ok {
			if id, pok := sheetPagePath(path); pok {
				return classPage, id
			}
		}
	case http.MethodPost:
		if path == "/sheets" {
			return classCreate, ""
		}
		id, verb, ok := splitSheetPath(path)
		if !ok {
			return classOther, ""
		}
		switch verb {
		case "cell", "clear", "fill", "paste", "style", "colwidth", "rows", "cols":
			return classWrite, id
		case "viewport", "sel":
			return classNav, id
		}
	}
	return classOther, ""
}

// splitSheetPath pulls `{id}` and `{verb}` out of `/s/{id}/{verb}`. A bare
// `/s/{id}` is the page: no verb, ok=false, and classified separately.
func splitSheetPath(path string) (id, verb string, ok bool) {
	if !strings.HasPrefix(path, "/s/") {
		return "", "", false
	}
	rest := path[len("/s/"):]
	i := strings.IndexByte(rest, '/')
	if i < 0 || i == 0 {
		return "", "", false
	}
	id, verb = rest[:i], rest[i+1:]
	if verb == "" || strings.IndexByte(verb, '/') >= 0 {
		return "", "", false
	}
	return id, verb, true
}

// sheetPagePath matches `/s/{id}` exactly — the page, which has no verb.
func sheetPagePath(path string) (id string, ok bool) {
	if !strings.HasPrefix(path, "/s/") {
		return "", false
	}
	id = path[len("/s/"):]
	if id == "" || strings.IndexByte(id, '/') >= 0 {
		return "", false
	}
	return id, true
}

// hostOf strips the port off a RemoteAddr by slicing, so it allocates nothing.
func hostOf(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		addr = addr[:i]
	}
	if len(addr) > 1 && addr[0] == '[' && addr[len(addr)-1] == ']' {
		addr = addr[1 : len(addr)-1]
	}
	return addr
}

// clientIP is the bucket key: the rightmost X-Forwarded-For entry, not the
// leftmost, and only when the operator says there is a proxy.
//
// Each hop appends, so the leftmost value is whatever the client claimed and
// the rightmost is what the nearest proxy observed. Taking the leftmost — the
// common mistake — hands every visitor an unlimited supply of identities and
// makes this whole file a no-op.
func (l *Limiter) clientIP(r *http.Request) string {
	if l.p.TrustForwardedFor {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.LastIndexByte(xff, ','); i >= 0 {
				xff = xff[i+1:]
			}
			if xff = strings.TrimSpace(xff); xff != "" {
				return xff
			}
		}
	}
	return hostOf(r.RemoteAddr)
}

// ─── The middleware ───────────────────────────────────────────────────────────

// Middleware wraps the mux. Every decision here happens before the handler
// runs, which is the point: the cheapest way to serve an abusive request is not
// to serve it.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	l.indexNext = next
	if !l.p.Enabled {
		if l.p.Index == IndexList {
			return next
		}
		// Even with the rate limiting off, the index policy is an exposure
		// decision and stays in force.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/" {
				l.serveIndex(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class, sheetID := classify(r.Method, r.URL.Path)

		// The body cap comes first, because everything below might read the
		// body. Checking the declared length refuses an oversize body before a
		// byte of it is read; MaxBytesReader is the backstop for a chunked
		// body that declared nothing.
		if l.p.MaxBodyBytes > 0 && r.Body != nil && r.Method != http.MethodGet {
			if r.ContentLength > l.p.MaxBodyBytes {
				l.refBody.Add(1)
				obsLog.WarnContext(r.Context(), "limit.body",
					"path", r.URL.Path, "bytes", r.ContentLength, "max", l.p.MaxBodyBytes)
				refuse(w, http.StatusRequestEntityTooLarge, 0,
					"Request body is too large ("+humanSize(r.ContentLength)+"); the limit is "+
						humanSize(l.p.MaxBodyBytes)+".")
				return
			}
			// Only when nothing was declared. The check above already refused
			// every body stating an oversize length, so wrapping a well-formed
			// request would allocate a reader on the hot path (64 B and ~15 ns
			// per request) to guard a case that cannot happen: net/http itself
			// stops a non-chunked body at its declared Content-Length. A
			// chunked body declares -1 and is the one that needs the backstop.
			if r.ContentLength < 0 {
				r.Body = http.MaxBytesReader(w, r.Body, l.p.MaxBodyBytes)
			}
		}

		switch class {
		case classIndex, classPage:
			// A page is a read, and an expensive one: `/s/{id}` materializes a
			// window. It gets a bucket of its own so a flood of page loads
			// cannot spend the tokens an edit or a stream needs — and, in the
			// other direction, so somebody who typed too fast can still reload.
			if ok, wait := l.page.allow(l.clientIP(r), l.now()); !ok {
				l.refPage.Add(1)
				refuse(w, http.StatusTooManyRequests, wait,
					"Too many page loads from this address; wait a moment and reload.")
				return
			}
			if class == classIndex {
				l.serveIndex(w, r)
				return
			}
		case classCreate:
			if !l.allowCreate(w, r) {
				return
			}
			// The sheet is attributed from the redirect the handler wrote.
			// index.go generates the id — it must, since the id is the
			// capability and is server-random — so the middleware cannot know
			// it in advance; but `POST /sheets` answers 303 to `/s/{id}`, and
			// the header map on this ResponseWriter is the one the handler
			// set. No wrapper, no second write path, and index.go needs to know
			// nothing about the quota. It costs an allocation or two per
			// create, which is 6/min, and nothing on any other route.
			ip := l.clientIP(r)
			next.ServeHTTP(w, r)
			if id, ok := strings.CutPrefix(w.Header().Get("Location"), "/s/"); ok && id != "" {
				l.quota.add(ip, id)
			}
			return
		case classWrite:
			// Read-only is checked before the rate limit: a write to a fixed
			// sheet will not be allowed in a moment when a token frees up, so
			// spending a token on it — and answering "slow down" for something
			// that will never be permitted — would be two wrong answers. See
			// readonly.go.
			if isReadOnly(sheetID) {
				l.refReadOnly.Add(1)
				obsLog.WarnContext(r.Context(), "limit.readonly",
					"sheet", sheetID, "ip", l.clientIP(r), "path", r.URL.Path)
				refuse(w, http.StatusForbidden, 0, readOnlyRefusal)
				return
			}
			if !l.allowWrite(w, r, sheetID) {
				return
			}
		case classNav:
			if ok, wait := l.nav.allow(l.clientIP(r), l.now()); !ok {
				l.refNav.Add(1)
				refuse(w, http.StatusTooManyRequests, wait,
					"Too many viewport updates from this address; slow down for a moment.")
				return
			}
		case classStream:
			ip := l.clientIP(r)
			if ok, wait := l.stream.allow(ip, l.now()); !ok {
				l.refStream.Add(1)
				obsLog.WarnContext(r.Context(), "limit.stream.rate", "ip", ip, "sheet", sheetID)
				refuse(w, http.StatusTooManyRequests, wait,
					"Too many connections from this address; wait a moment and reload.")
				return
			}
			if !l.live.acquire(ip, l.p.StreamsPerIP, l.p.StreamsTotal) {
				l.refStream.Add(1)
				// The two caps fail for opposite reasons and must not share a
				// sentence: "close a tab" is right when this address holds too
				// many and misleading when the demo is full and the reader has
				// one tab open. acquire checks the process total first, so
				// reading the counters back here tells them apart without a
				// second lock discipline.
				perIP, total := l.live.counts(ip)
				if l.p.StreamsTotal > 0 && total >= l.p.StreamsTotal {
					obsLog.WarnContext(r.Context(), "limit.stream.process",
						"ip", ip, "sheet", sheetID, "total", total, "max", l.p.StreamsTotal)
					refuse(w, http.StatusTooManyRequests, 15*time.Second,
						"This demo is full — "+strconv.Itoa(l.p.StreamsTotal)+
							" people are connected at once, which is all one small server can keep "+
							"in realtime. Try again in a minute.")
					return
				}
				obsLog.WarnContext(r.Context(), "limit.stream.concurrent",
					"ip", ip, "sheet", sheetID, "held", perIP, "max", l.p.StreamsPerIP)
				refuse(w, http.StatusTooManyRequests, 5*time.Second,
					"Too many sheets open at once from this address. Close a tab and reload.")
				return
			}
			// The handler blocks for the life of the stream, so the release is
			// exactly "when the browser went away".
			defer l.live.release(ip)
		}

		next.ServeHTTP(w, r)
	})
}

// allowCreate is the rate limit and the two global disk caps. Order matters:
// the bucket is cheap and the directory scan is a syscall per file, so a flood
// pays for the scan once and then pays nothing.
func (l *Limiter) allowCreate(w http.ResponseWriter, r *http.Request) bool {
	ip := l.clientIP(r)
	if ok, wait := l.create.allow(ip, l.now()); !ok {
		l.refCreate.Add(1)
		obsLog.WarnContext(r.Context(), "limit.create.rate", "ip", ip)
		refuse(w, http.StatusTooManyRequests, wait,
			"Too many new sheets from this address. Try again in "+
				strconv.Itoa(retryAfterSecs(wait))+"s.")
		return false
	}
	// The per-IP cap is checked before the directory scan because it is a map
	// lookup and the scan is a syscall per file — and before the global caps
	// deliberately: when the box is nearly full, the address that filled it
	// should be the one refused, not the next visitor.
	if l.p.MaxSheetsPerIP > 0 {
		if held := l.quota.held(ip); held >= l.p.MaxSheetsPerIP {
			l.refQuota.Add(1)
			obsLog.WarnContext(r.Context(), "limit.create.per_ip",
				"ip", ip, "held", held, "max", l.p.MaxSheetsPerIP)
			refuse(w, http.StatusTooManyRequests, 0,
				"You already have "+strconv.Itoa(held)+" sheets on this demo, which is the limit "+
					"for one address. Open one you already made"+retentionClause(l.p.SheetTTL)+".")
			return false
		}
	}
	if l.p.DataDir == "" || (l.p.MaxSheets <= 0 && l.p.MaxTotalBytes <= 0) {
		return true
	}
	count, bytes, err := sheetDirUsage(l.p.DataDir)
	if err != nil {
		// A directory that cannot be read is not a reason to refuse the user;
		// it is a reason to say so in the log. The rate limit above still holds.
		obsLog.WarnContext(r.Context(), "limit.create.usage", "dir", l.p.DataDir, "err", err.Error())
		return true
	}
	if l.p.MaxSheets > 0 && count >= l.p.MaxSheets {
		l.refDisk.Add(1)
		obsLog.WarnContext(r.Context(), "limit.create.count", "sheets", count, "max", l.p.MaxSheets)
		refuse(w, http.StatusInsufficientStorage, 0,
			"This demo is holding its maximum of "+strconv.Itoa(l.p.MaxSheets)+
				" sheets. Open an existing one, or ask for some to be cleared.")
		return false
	}
	if l.p.MaxTotalBytes > 0 && bytes >= l.p.MaxTotalBytes {
		l.refDisk.Add(1)
		obsLog.WarnContext(r.Context(), "limit.create.bytes", "bytes", bytes, "max", l.p.MaxTotalBytes)
		refuse(w, http.StatusInsufficientStorage, 0,
			"This demo is using its maximum of "+humanSize(l.p.MaxTotalBytes)+
				" of sheet storage. Ask for some sheets to be cleared.")
		return false
	}
	return true
}

// allowWrite is the per-IP write rate plus the per-sheet byte cap.
//
// The per-sheet cap is the only backstop against the unbounded event log, and
// it is deliberately blunt: past the cap a sheet becomes read-only rather than
// silently rolling its history. A ceiling on the `events` table inside the
// write transaction would be the real fix, and belongs in store.go.
func (l *Limiter) allowWrite(w http.ResponseWriter, r *http.Request, sheetID string) bool {
	ip := l.clientIP(r)
	if ok, wait := l.write.allow(ip, l.now()); !ok {
		l.refWrite.Add(1)
		obsLog.WarnContext(r.Context(), "limit.write.rate", "ip", ip, "sheet", sheetID)
		l.tellScreen(r, "That was too fast — a few edits were refused. Try again in a moment.")
		refuse(w, http.StatusTooManyRequests, wait,
			"Too many edits from this address; wait a moment and try again.")
		return false
	}
	if l.p.DataDir == "" || l.p.MaxSheetBytes <= 0 || sheetID == "" {
		return true
	}
	if size := sheetFileBytes(l.p.DataDir, sheetID); size >= l.p.MaxSheetBytes {
		l.refDisk.Add(1)
		obsLog.WarnContext(r.Context(), "limit.write.sheet_bytes",
			"sheet", sheetID, "bytes", size, "max", l.p.MaxSheetBytes)
		l.tellScreen(r, "This sheet has reached its "+humanSize(l.p.MaxSheetBytes)+
			" size limit and can no longer be edited.")
		refuse(w, http.StatusInsufficientStorage, 0,
			"This sheet has reached its size limit of "+humanSize(l.p.MaxSheetBytes)+".")
		return false
	}
	return true
}

// tellScreen delivers the refusal over the issuing screen's own SSE stream, so
// that a refused command is not a silent drop.
//
// `/cell` is covered without this — Datastar dispatches `datastar-fetch` with
// `type:'error'` on any >= 400 and keys.go's T.fail turns that into a red
// marker and a revert — but the range commands (`#cl`, `#fl`, `#pv`, `#st`)
// raise `$p` on the click and rely on the server to lower it, so a 429 alone
// would leave a spinner up for the full 20-second client timeout. The
// connection id is in the request body, and this is the only place that reads
// it: the body is touched only on the refusal path, which is rare by
// construction.
func (l *Limiter) tellScreen(r *http.Request, msg string) {
	if l.notify == nil || r.Body == nil {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil || len(body) == 0 {
		return
	}
	var sig struct {
		Conn string `json:"conn"`
	}
	if json.Unmarshal(body, &sig) != nil || sig.Conn == "" {
		return
	}
	l.notify(sig.Conn, msg)
}

// refuse writes the one refusal shape: a status, an optional Retry-After, and a
// sentence saying what happened. Plain text, because these reach a human
// through a browser's network panel, the editor's error path, or curl — never
// through a renderer.
func refuse(w http.ResponseWriter, status int, retry time.Duration, msg string) {
	h := w.Header()
	if retry > 0 {
		h.Set("Retry-After", strconv.Itoa(retryAfterSecs(retry)))
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

// retryAfterSecs is the one rounding of a wait into whole seconds, so the
// header and the sentence that accompanies it can never disagree.
func retryAfterSecs(d time.Duration) int {
	secs := int(d.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// ─── Disk accounting ──────────────────────────────────────────────────────────

// sheetDirUsage counts sheets and total bytes. `count` is `.db` files, which is
// what a sheet is (store.go has no catalog, on purpose); `bytes` is every file
// in the directory, because a `-wal` sidecar occupies the same disk.
func sheetDirUsage(dir string) (count int, bytes int64, err error) {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".db") {
			count++
		}
		if fi, ferr := e.Info(); ferr == nil {
			bytes += fi.Size()
		}
	}
	return count, bytes, nil
}

// sheetFileBytes is one sheet's footprint: the database and its write-ahead
// log, which under `journal_mode(WAL)` is where a burst of writes lands first.
// A missing file is zero, not an error — the write is about to create it.
func sheetFileBytes(dir, id string) int64 {
	if validSheetID(id) != nil {
		return 0
	}
	base := filepath.Join(dir, id+".db")
	var n int64
	if fi, err := os.Stat(base); err == nil {
		n += fi.Size()
	}
	if fi, err := os.Stat(base + "-wal"); err == nil {
		n += fi.Size()
	}
	return n
}

// ─── The index page ───────────────────────────────────────────────────────────

// adminEmailHeader is set by the exe.dev proxy on requests from a signed-in
// user, and removed from requests that merely claim it.
const adminEmailHeader = "X-ExeDev-Email"

// isAdmin reports whether this request came from the configured operator.
//
// TrustForwardedFor is required as well as AdminEmail, and that coupling is the
// point rather than an oversight. This header is only meaningful because
// something in front is authenticating users and stripping forged copies; a
// deployment that is not behind such a proxy would be handing every visitor a
// header they can set themselves. TrustForwardedFor is the existing declaration
// that a trusted proxy is in front, so it is reused as the interlock, and main
// refuses to start if one is set without the other.
func (l *Limiter) isAdmin(r *http.Request) bool {
	if l.p.AdminEmail == "" || !l.p.TrustForwardedFor {
		return false
	}
	got := strings.TrimSpace(r.Header.Get(adminEmailHeader))
	return got != "" && strings.EqualFold(got, l.p.AdminEmail)
}

// serveIndex applies the exposure policy to `GET /`. IndexList delegates to
// index.go's real handler through the wrapped mux; the other two answer here.
func (l *Limiter) serveIndex(w http.ResponseWriter, r *http.Request) {
	// The operator's way back in. With the listing off, every sheet URL is a
	// ~70-bit capability that nothing else recovers, so a lost link is a lost
	// sheet and the person running the box needs some way to enumerate them.
	//
	// The identity comes from the hosting proxy, which adds X-ExeDev-Email to
	// requests it has authenticated and strips any copy the client sent — that
	// stripping is what makes the header trustworthy, and it is a property of
	// the proxy, not of this code. isAdmin refuses to believe the header unless
	// the operator has also declared they are behind such a proxy.
	//
	// Nothing distinguishes this from an ordinary visit: an unauthenticated
	// reader gets the same create-only page they would get anyway, with no hint
	// that a listing exists and no URL to guess. That is the whole advantage
	// over the query-string parameter it replaces, which was a master key to
	// every sheet that landed in browser history and access logs.
	if l.isAdmin(r) {
		l.indexNext.ServeHTTP(w, r)
		return
	}
	// A convenience for arriving from a browser with no session: bounce through
	// the proxy's login and come back. Only offered when an admin is configured,
	// so it reveals nothing on a deployment without one.
	if l.p.AdminEmail != "" && r.URL.Query().Get("login") == "1" {
		http.Redirect(w, r, "/__exe.dev/login?redirect=/", http.StatusSeeOther)
		return
	}
	switch l.p.Index {
	case IndexOff:
		http.NotFound(w, r)
	case IndexCreate:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, l.indexHTML)
	default:
		l.indexNext.ServeHTTP(w, r)
	}
}

// retentionClause is the half-sentence that names the retention policy, for the
// refusals that need to explain that a slot is not gone forever. Empty when
// there is no retention policy, because promising one that is not running is
// worse than saying nothing.
func retentionClause(ttl time.Duration) string {
	if ttl <= 0 {
		return ""
	}
	return ", or wait — sheets are cleared " + humanDuration(ttl) + " after their last edit"
}

// retentionNote states the retention policy before it bites rather than after.
// Somebody who comes back to a URL and finds an empty sheet has to be able to
// tell "cleared on a schedule I was told about" from "the demo lost my data",
// and the only way to give them that is to have said so on the way in. The
// wording names the trigger (no edits), the period, and the thing that does not
// happen (the URL dying), because the last is the part a reader will not
// assume.
func retentionNote(ttl time.Duration) string {
	if ttl <= 0 {
		return ""
	}
	return `<p class="sub" style="margin-top:-16px">Sheets that go ` + humanDuration(ttl) +
		` without an edit are cleared. The link keeps working — it reopens an empty sheet.</p>`
}

// indexCreateOnlyHTML is the index without the listing. It reuses index.go's
// stylesheet so the two pages cannot drift apart visually, and keeps the one
// thing the root is genuinely for — making a sheet — working.
//
// It is the page `-index create` serves, so it is where the retention policy is
// stated. index.go's listing page needs the same sentence.
func indexCreateOnlyHTML(ttl time.Duration) string {
	return `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>sheetstream</title><link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Crect width='16' height='16' fill='%23fff'/%3E%3Cpath d='M0 5h16M0 10h16M5 0v16M10 0v16' stroke='%23cdd5e2'/%3E%3Crect x='5' y='5' width='5' height='5' fill='%233b5e8b'/%3E%3C/svg%3E"><style>` + indexCSS + `</style></head><body><main>` +
		`<h1>sheetstream</h1><p class="sub">A sheet's URL is its key — keep the link, ` +
		`it is the only way back to it.</p>` +
		retentionNote(ttl) +
		`<div class="bar"><span class="sp"></span>` +
		`<form method="post" action="/sheets" style="margin:0">` +
		`<button class="btn p" type="submit">New sheet</button></form></div>` +
		`<div class="empty">Open a sheet with its link.</div>` +
		`</main></body></html>`
}
