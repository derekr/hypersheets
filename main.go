package main

// main.go — boot order and nothing else.
//
//	store  →  bus  →  seed  →  registry  →  server  →  http
//
// The order is forced: the registry subscribes on the bus, the server needs the
// registry, and seeding wants the store configured. Everything is closed in
// reverse on shutdown, with the HTTP server first so no new request can arrive
// while the actors are draining.

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"

	"github.com/CAFxX/httpcompression"
	brotlienc "github.com/CAFxX/httpcompression/contrib/andybalholm/brotli"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

var (
	addr       = flag.String("addr", ":8080", "HTTP listen address")
	dataDir    = flag.String("data", "data/sheets", "directory holding one SQLite file per sheet")
	natsListen = flag.String("nats-listen", "", "optional NATS TCP listen address (e.g. :4222) so `nats sub 'sheet.>'` can watch the fan-out. Empty = in-process only.")
	throttleMs = flag.Int("throttle-ms", 0, "minimum gap between two renders of one connection. 0 disables.")
	// 4, not 2: at 2 bands the slack is only ~100 rows per side, which a fast
	// drag outruns, leaving about half of all scroll bursts needing a round trip.
	// Doubling the over-fetch costs one render each way and is the cheapest lever
	// available.
	bufferBands = flag.Int("buffer-bands", 4, "bands of over-fetch on EACH side of the viewport — the slack the client scrolls through without touching the network")
	latencyMs   = flag.Int("latency-ms", 0, "symmetric artificial network delay per leg. A request pays two legs, a push on an open stream pays one.")
	// The two readings of "pending" this prototype is comparing, on one build.
	// 0 shows the marker for the whole round trip; ~150 shows it only when the
	// commit is slow enough to be worth noticing. See pendingDelay in keys.go.
	pendingDelayMs = flag.Int("pending-delay-ms", 0, "how long a commit may be in flight before the cell is marked pending. 0 = mark it immediately.")
	sheetID        = flag.String("sheet", "demo", "sheet id to seed and serve at /")
	// Fewer than the grid's height, because a real sheet is mostly empty and the
	// demo should look like one. Pass `-seed-rows 10000` to reproduce the
	// worst-case mutation benchmarks.
	seedRows = flag.Int("seed-rows", 500, "rows to seed with data when creating the sheet (the grid starts DefaultRows tall and grows)")

	traceFile = flag.String("trace-file", "trace.jsonl", "OpenTelemetry spans as JSON lines. Empty disables tracing.")
	logFile   = flag.String("log-file", "sheetstream.log", "structured JSON log destination. Empty logs to stderr.")
	analyze   = flag.String("analyze", "", "read a trace file, print a p50/p95 table per span plus byte stats, and exit. Starts no server.")

	// Abuse limits. The policy and the reasoning behind every number live in
	// limits.go; these exist so it can be tuned or switched off without a
	// rebuild. They default to on, because a shared demo with no authentication
	// needs a floor under it from the first boot. Setting any individual number
	// to 0 disables that one limit.
	limitsOn = flag.Bool("limits", true, "enforce the abuse limits in limits.go (rate limits, disk caps, body cap). Off makes every route unbounded.")

	limitCreatePerMin = flag.Float64("limit-create-per-min", 6, "sustained rate of POST /sheets per client IP. 0 disables.")
	limitCreateBurst  = flag.Float64("limit-create-burst", 12, "how many sheets one client IP may create back to back.")
	limitWritePerSec  = flag.Float64("limit-write-per-sec", 60, "sustained rate of write commands (cell/clear/fill/paste/style/colwidth/rows/cols) per client IP. 0 disables.")
	limitWriteBurst   = flag.Float64("limit-write-burst", 300, "how many write commands one client IP may issue back to back.")
	limitNavPerSec    = flag.Float64("limit-nav-per-sec", 120, "sustained rate of viewport/selection commands per client IP. Deliberately separate from writes so a scroll burst cannot spend an edit's tokens. 0 disables.")
	limitNavBurst     = flag.Float64("limit-nav-burst", 600, "how many viewport/selection commands one client IP may issue back to back.")
	limitPagePerSec   = flag.Float64("limit-page-per-sec", 60, "sustained rate of page loads (GET / and GET /s/{id}) per client IP. A page materializes a ~6,500-cell window, so it is the cheapest expensive read in the system. 0 disables.")
	limitPageBurst    = flag.Float64("limit-page-burst", 300, "how many page loads one client IP may issue back to back.")
	limitStreamPerSec = flag.Float64("limit-stream-per-sec", 2, "sustained rate of SSE stream opens per client IP. 0 disables.")
	limitStreamBurst  = flag.Float64("limit-stream-burst", 30, "how many SSE streams one client IP may open back to back.")
	limitStreamsPerIP = flag.Int("limit-streams-per-ip", 32, "SSE streams one client IP may hold open AT ONCE. This is the number that bounds goroutines and fan-out cost. 0 disables.")
	// 64 rather than 256: at 256 held-open streams on one hot sheet a single edit
	// costs ~291 ms of CPU and the server's share of the push delay is 36 ms p50
	// / 67 ms p95 on a 10-core laptop, at one edit per second. At the ~10
	// commits/s a fast typist produces, that needs several times the CPU this 2
	// vCPU box has. See the table in limits.go.
	limitStreamsTotal = flag.Int("limit-streams-total", 64, "SSE streams this process will hold open at once, across all clients. This is the number that decides whether the demo is realtime under fan-out; see limits.go for the measurement behind it. 0 disables.")

	limitMaxSheets = flag.Int("limit-max-sheets", 500, "how many sheet databases may exist on disk before POST /sheets is refused. 0 disables.")
	// A global cap alone is a denial of service by accumulation: at the allowed
	// 6/min one address fills all 500 slots in ~83 minutes and creation then
	// fails for everybody. This is the per-address share of that ceiling, and it
	// counts live sheets rather than lifetime creations because the reaper hands
	// slots back. See limits.go for why 25.
	limitSheetsPerIP = flag.Int("limit-sheets-per-ip", 25, "how many LIVE sheets one client IP may hold. The reaper returns the slot when it clears a sheet. 0 disables.")
	limitDiskMB      = flag.Int64("limit-disk-mb", 2048, "total megabytes of sheet files before POST /sheets is refused. 0 disables.")
	limitSheetMB     = flag.Int64("limit-sheet-mb", 256, "megabytes one sheet (db + WAL) may reach before it is refused further writes. This is the only ceiling on the unbounded event log. 0 disables.")
	limitBodyKB      = flag.Int64("limit-body-kb", 64, "maximum request body in kilobytes. Datastar's signal store is under 2 KB. 0 disables.")

	trustForwarded = flag.Bool("trust-forwarded-for", false, "read the client IP from X-Forwarded-For (rightmost entry). OFF BY DEFAULT: without a proxy in front, a spoofed header lets one client look like an unlimited number of them.")

	// Retention. The reasoning is in reaper.go: a demo with no auth and no delete
	// button accumulates forever, so sheets nobody has edited for the retention
	// period are cleared. Their URLs keep working and reopen an empty sheet at
	// the same id, because the id is the only capability anybody has and taking
	// it away is a worse thing than taking the data.
	sheetTTL     = flag.Duration("sheet-ttl", DefaultSheetTTL, "clear sheets that have gone this long without a WRITE (reads do not count). The URL keeps working and reopens an empty sheet. 0 disables retention entirely.")
	sweepEvery   = flag.Duration("sheet-sweep-every", DefaultSweepEvery, "how often the retention sweep runs.")
	tombstoneTTL = flag.Duration("sheet-tombstone-ttl", 0, "how long a cleared sheet's URL keeps working before it 404s again. 0 (the default) keeps it forever — the marker is one line on disk and silently breaking a bookmark is the worse failure.")
	// The LRU's own numbers, exposed because retention depends on them: a sheet's
	// `.db` mtime only moves when its handle is closed and the WAL checkpointed,
	// so the cache's idle timeout is the lag between the last write and the last
	// write the reaper can see. Five minutes against a two-week TTL is nothing in
	// production; making it a flag lets the whole retention path be exercised end
	// to end in under a minute.
	cacheCap  = flag.Int("sheet-cache", DefaultSheetCacheCap, "how many sheet databases stay open at once (the LRU of handles).")
	cacheIdle = flag.Duration("sheet-cache-idle", DefaultSheetIdleTTL, "how long an untouched sheet handle stays open before the LRU closes it. Closing checkpoints the WAL, which is what makes the last write visible to the retention sweep.")

	readOnly = flag.String("readonly-sheets", "", "comma-separated sheet ids that refuse every write. A sheet listed here is also NEVER cleared by the reaper: its clock is time-since-last-edit, and a sheet that cannot be edited could never reset it. See readonly.go.")

	keepSeeded = flag.Bool("keep-seeded-sheet", true, "never clear the -sheet sheet. It is the demo's front door, it is recreated by the boot path when missing, and clearing it would empty the landing page rather than somebody's document.")

	indexMode  = flag.String("index", "list", "what GET / does: `list` (every sheet on disk — the existing behaviour), `create` (the New sheet button only, no listing), `off` (404). A sheet id is ~70 bits of randomness, so listing them is what turns the URL from a capability into a directory.")
	adminEmail = flag.String("exedev-admin-email", "", "address allowed to see the full sheet listing when -index is create/off, matched against the X-ExeDev-Email header the exe.dev proxy sets on authenticated requests. Requires -trust-forwarded-for, because the header is only trustworthy from a proxy that strips forged copies. Empty means the listing does not exist.")

	// Where this process is, in words a reader recognises — "Amsterdam",
	// "us-east". It is the actual explanation for a slow round trip on a one-VM
	// demo, and the latency chip's tooltip says so. It must be a human place and
	// never a host, an IP or a machine id: this service has no authentication,
	// and infrastructure detail on an unauthenticated endpoint invites probing.
	// Empty omits the clause rather than printing a placeholder, since the
	// tooltip reads correctly without it.
	region = flag.String("region", "", "where this server runs, in words a reader recognises (e.g. `Amsterdam`). Shown in the latency chip's tooltip as the explanation for a long round trip. Empty omits the clause. Never put a hostname or an IP here.")
)

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// The latency chip reads the region at render time through this pointer, the
	// same arrangement pendingDelayMs has with keys.go: the flag is declared here
	// because that is where flags live, and every test that renders a page shell
	// works without a flag parse.
	regionFlag = region

	// -analyze is a reader, not a server. It must not seed, open the store or
	// bind a port.
	if *analyze != "" {
		if err := AnalyzeTrace(*analyze, os.Stdout); err != nil {
			log.Fatalf("analyze: %v", err)
		}
		return
	}

	shutdownTracing, err := StartTracing(*traceFile)
	if err != nil {
		log.Fatalf("tracing: %v", err)
	}
	logCloser, err := StartLogging(*logFile)
	if err != nil {
		log.Fatalf("logging: %v", err)
	}
	defer logCloser.Close()
	if *traceFile != "" {
		log.Printf("traces → %s   logs → %s", *traceFile, *logFile)
	}

	ConfigureStore(*dataDir, *cacheCap, *cacheIdle)
	defer func() {
		if err := CloseStore(); err != nil {
			log.Printf("close store: %v", err)
		}
	}()

	bus, err := StartBus(BusOptions{Listen: *natsListen, Verbose: *natsListen != ""})
	if err != nil {
		log.Fatalf("start bus: %v", err)
	}
	defer bus.Close()
	if *natsListen != "" {
		log.Printf("NATS listening on %s — try: nats sub 'sheet.>' -s %s", *natsListen, bus.ClientURL())
	}

	// Seed on first boot only. Seeding is idempotent but it rewrites 260,000
	// cells, so an existing sheet is left exactly as the user left it.
	if !SheetExists(*sheetID) {
		start := time.Now()
		if err := Seed(*sheetID, *seedRows, MaxCols); err != nil {
			log.Fatalf("seed %s: %v", *sheetID, err)
		}
		log.Printf("seeded sheet %q: %d rows x %d cols in %s", *sheetID, *seedRows, MaxCols, time.Since(start).Round(time.Millisecond))
	}

	reg := NewRegistry(bus, *throttleMs)
	srv := NewServer(ServerOptions{
		Registry:     reg,
		Bus:          bus,
		Recalc:       Recalc,
		DefaultSheet: *sheetID,
		BufferBands:  *bufferBands,
		Latency:      time.Duration(*latencyMs) * time.Millisecond,
	})

	// The limiter wraps the mux, not the other way around: every refusal has to
	// happen before a handler opens a sheet, takes an actor turn or holds a
	// goroutine open, which is the whole reason it is middleware. It also sits
	// outside withLatency, so a refusal does not pay the artificial delay.
	//
	// A typo'd -index must not silently fall back to the most exposing option.
	if !validIndexMode(IndexMode(*indexMode)) {
		log.Fatalf("-index %q: want list, create or off", *indexMode)
	}
	// Fail closed and loud rather than quietly ignoring the setting. An admin
	// address is only meaningful behind a proxy that authenticates users and
	// strips forged identity headers; without that declaration the header is
	// attacker-controlled, and a listing of every sheet on the box is exactly
	// the thing not to hand out on a guess. See Limiter.isAdmin.
	if *adminEmail != "" && !*trustForwarded {
		log.Fatal("-exedev-admin-email requires -trust-forwarded-for: the identity header " +
			"is only trustworthy from a proxy that strips client-supplied copies")
	}
	lim := NewLimiter(LimitPolicy{
		Enabled:           *limitsOn,
		CreatePerMin:      *limitCreatePerMin,
		CreateBurst:       *limitCreateBurst,
		WritePerSec:       *limitWritePerSec,
		WriteBurst:        *limitWriteBurst,
		NavPerSec:         *limitNavPerSec,
		NavBurst:          *limitNavBurst,
		PagePerSec:        *limitPagePerSec,
		PageBurst:         *limitPageBurst,
		StreamPerSec:      *limitStreamPerSec,
		StreamBurst:       *limitStreamBurst,
		StreamsPerIP:      *limitStreamsPerIP,
		StreamsTotal:      *limitStreamsTotal,
		MaxSheets:         *limitMaxSheets,
		MaxSheetsPerIP:    *limitSheetsPerIP,
		SheetTTL:          *sheetTTL,
		MaxTotalBytes:     *limitDiskMB << 20,
		MaxSheetBytes:     *limitSheetMB << 20,
		MaxBodyBytes:      *limitBodyKB << 10,
		DataDir:           *dataDir,
		TrustForwardedFor: *trustForwarded,
		Index:             IndexMode(*indexMode),
		AdminEmail:        *adminEmail,
	})
	// A refused write must be visible, not lost: the range commands raise their
	// pending chip on the click and rely on the server to lower it, so a bare 429
	// would leave a spinner up for the client's full timeout. This hands the
	// refusal to the one screen that asked, over the stream it already has open.
	// See Limiter.tellScreen.
	lim.Notify(srv.notify)
	log.Print(lim.Describe())

	// The reaper comes after the registry and the limiter because it needs both:
	// the registry answers "is anybody connected to this sheet right now" (a
	// sheet with a live stream is never cleared) and the limiter owns the per-IP
	// quota ledger a reap has to credit back. It starts before the listener so
	// the first sweep happens on a box that is not yet serving, and closes after
	// HTTP shutdown so a sweep cannot race the last stream.
	keep := map[string]bool{}
	if *keepSeeded && *sheetID != "" {
		keep[*sheetID] = true
	}
	// Read-only implies reap-exempt, wired here rather than left to the operator
	// to remember: the reaper measures time since the last write, and a sheet
	// that refuses writes can never restart that clock, so an ordinary TTL would
	// silently schedule it for deletion on the day it had survived longest. See
	// readonly.go.
	readOnlySheets = map[string]bool{}
	for _, id := range strings.Split(*readOnly, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if err := validSheetID(id); err != nil {
			log.Fatalf("-readonly-sheets: %q: %v", id, err)
		}
		readOnlySheets[id] = true
		keep[id] = true
	}
	if anyReadOnly() {
		ids := make([]string, 0, len(readOnlySheets))
		for id := range readOnlySheets {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		log.Printf("read-only: %s (writes refused, never reaped)", strings.Join(ids, ", "))
	}
	sheetTTLFlag = *sheetTTL
	reaper := NewReaper(ReaperPolicy{
		Enabled:      *sheetTTL > 0,
		TTL:          *sheetTTL,
		Every:        *sweepEvery,
		TombstoneTTL: *tombstoneTTL,
		Keep:         keep,
	}, reg, lim.Quota())
	log.Print(reaper.Describe())
	reaper.Start()

	// Compress the document responses. Datastar already brotli-compresses the SSE
	// stream itself, so text/event-stream is blacklisted here to keep it from
	// being compressed twice, and the adapter skips any response that already
	// carries Content-Encoding. This sits outside the limiter so a 429 body is
	// compressed too and, more importantly, so the limiter sees the untouched
	// request.
	//
	// Brotli is re-registered above zstd, which is the point of the six lines
	// below. The library's priorities are hardcoded — gzip -200, brotli -100,
	// zstd -50 — so zstd wins against every modern browser, since Chrome offers
	// `gzip, deflate, br, zstd`; on a real page of this application zstd costs
	// about 17% more bytes than brotli q5.
	//
	// q5 and not q6: they land within a handful of bytes of each other and q6
	// costs ~40% more CPU. Compression here is scored by time and bytes rather
	// than by ratio (see the SSE stream's q2 choice), and q6 fails that test —
	// it is the default only because it is the library's default. q11 is smaller
	// still and far slower per response, which this 2 vCPU box cannot spend.
	brEnc, berr := brotlienc.New(brotlienc.Options{Quality: 5})
	if berr != nil {
		log.Fatalf("brotli: %v", berr)
	}
	compress, cerr := httpcompression.DefaultAdapter(
		httpcompression.ContentTypes([]string{"text/event-stream"}, true),
		httpcompression.MinSize(1024),
		// Priority 0 beats zstd's -50. Registering the encoding again REPLACES
		// the default registration rather than adding a second one.
		httpcompression.Compressor(brotlienc.Encoding, 0, brEnc),
	)
	if cerr != nil {
		log.Fatalf("compression: %v", cerr)
	}

	hs := &http.Server{
		Addr: *addr,
		// securityHeaders is outermost so a refused request carries them too.
		Handler: securityHeaders(compress(lim.Middleware(srv.Routes()))),
		// No WriteTimeout: /live is a held-open stream and any write deadline
		// would guillotine it mid-session.
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("sheet %q ready at http://localhost%s/s/%s  (buffer=%d bands, throttle=%dms, latency=%dms/leg)",
			*sheetID, *addr, *sheetID, *bufferBands, *throttleMs, *latencyMs)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	ls := lim.Stats()
	reaped, freed := reaper.Stats()
	log.Printf("shutting down (%d live screens); refused: %d create, %d write, %d viewport, %d stream, %d page, %d oversize, %d disk, %d over-quota; retention cleared %d sheets (%s), ledger holds %d sheets across %d addresses",
		srv.Screens(), ls.Create, ls.Write, ls.Nav, ls.Stream, ls.Page, ls.Body, ls.Disk, ls.Quota,
		reaped, humanSize(int64(freed)), ls.QuotaSheets, ls.QuotaAddrs)

	// Stop accepting first. Held-open streams do not end on their own, so the
	// grace period is short and the shutdown proceeds regardless: the streams
	// carry no unacknowledged state, so dropping them loses nothing.
	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	// The reaper stops AFTER the listener, so the last sweep can never unlink a
	// sheet somebody is still connected to on the way out.
	reaper.Close()
	// And the registry after that: it owns the presence hub's goroutine, which
	// has nothing left to announce once no connection can be registered.
	reg.Close()

	// Flush the tracer LAST and with its own deadline. The batch processor
	// holds up to half a second of spans in memory; exiting without draining it
	// truncates the trace file, and a half-written span line would break the
	// analyzer on the run you actually cared about.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer flushCancel()
	if err := shutdownTracing(flushCtx); err != nil {
		log.Printf("trace shutdown: %v", err)
	} else if *traceFile != "" {
		log.Printf("traces flushed to %s", *traceFile)
	}
}
