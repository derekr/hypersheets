package main

// bus.go — the fan-out layer.
//
// The bus carries a *subject and nothing else*. There is no payload, so a
// message is idempotent, unordered and loss-tolerant: it means "something in
// this band changed, go look at the database". That is why this is NATS core
// pub/sub with no JetStream, no stream, no consumer and no acking. NATS is the
// fan-out, not the log; the log lives in SQLite next to the data it describes.
//
// Subject scheme:
//
//	sheet.{sheetID}.band.{n}     something in this band changed
//	sheet.{sheetID}.presence     who is here, and where they are looking
//
// The presence subject is a sibling of the bands rather than a child of one, and
// that separation is why it is cheap. Presence goes stale when a stream opens or
// closes or a cursor moves — none of which is a write — so it must not wake a
// band subscriber, and an edit must not wake a presence subscriber. Two subjects
// with no prefix relationship is all it takes: `sheet.x.band.3` and
// `sheet.x.presence` never match each other's subscriptions. (`sheet.x.>` still
// covers both, which is what debug tooling wants; ParseBandSubject rejects the
// presence subject on token count, so SubscribeSheet's callback ignores it.)
//
// Publish emits leaf subjects only — one per dirty band, never the prefix chain
// (`sheet.{id}`, `sheet.{id}.band`, ...). NATS wildcards already do lineage
// matching: a subscriber wanting one band subscribes to that exact subject, one
// wanting a whole sheet subscribes to `sheet.{id}.>`.
//
// Expanding the chain on the publish side as well is the failure to avoid, and
// it is silent: sibling scopes meet at their shared parent, so a write to band 3
// publishing `sheet.x` reaches a subscriber that only wanted band 180 but also
// subscribed to `sheet.x`. Nothing errors; the bus quietly becomes a broadcast.
// Expand on one side only, and that side is the broker's.

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// subjectRoot is the first token of every subject this process publishes.
const subjectRoot = "sheet"

// BusOptions configures the embedded NATS server.
type BusOptions struct {
	// Listen is an optional "host:port" for a TCP listener. Empty (the
	// default) means in-process only: no socket, no port, nothing to secure.
	// Set it only when you want to point `nats sub 'sheet.>'` at a running
	// server to watch the fan-out.
	Listen string

	// Verbose enables nats-server logging. Off by default so tests stay quiet.
	Verbose bool
}

// Bus owns the embedded NATS server and this process's two connections to it.
//
// `nc` carries band traffic — one message per committed edit, and every
// subscription on it is a buffer band some browser is looking at. `pc` carries
// presence, which is a different kind of traffic: it ticks while nobody is
// writing, at up to presenceMaxHz per sheet, on joins, leaves and selection
// commits.
//
// They are separate so a presence flood cannot spend the band connection's
// pending-message budget (SubscribeBand caps it so a stalled callback drops
// rather than grows), and so the band connection's subscription count stays
// exactly the number of bands in the buffers, which is what makes the
// subscription count a meaningful measure of what a screen is watching.
type Bus struct {
	ns *server.Server
	nc *nats.Conn
	pc *nats.Conn
}

// StartBus boots an embedded NATS server (core only — no JetStream, no store
// dir, nothing on disk) and connects to it in-process.
func StartBus(opts BusOptions) (*Bus, error) {
	sopts := &server.Options{
		// No JetStream. See the file comment: the bus is fan-out, not a log.
		JetStream: false,
		NoSigs:    true,
		NoLog:     !opts.Verbose,
	}
	if opts.Listen == "" {
		sopts.DontListen = true
	} else {
		host, port, err := net.SplitHostPort(opts.Listen)
		if err != nil {
			return nil, fmt.Errorf("parse nats-listen %q: %w", opts.Listen, err)
		}
		if host == "" {
			host = "127.0.0.1"
		}
		p, err := strconv.Atoi(port)
		if err != nil {
			return nil, fmt.Errorf("parse nats-listen port %q: %w", port, err)
		}
		sopts.Host = host
		sopts.Port = p
		// Standard NATS monitoring endpoint, handy alongside a TCP listener.
		sopts.HTTPHost = "127.0.0.1"
		sopts.HTTPPort = 8222
	}
	if opts.Verbose {
		sopts.Debug = false
		sopts.Trace = false
	}

	ns, err := server.NewServer(sopts)
	if err != nil {
		return nil, fmt.Errorf("create nats server: %w", err)
	}
	if opts.Verbose {
		ns.ConfigureLogger()
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		return nil, fmt.Errorf("nats server not ready after 5s")
	}

	nc, err := nats.Connect("", nats.InProcessServer(ns), nats.Name("sheetstream"))
	if err != nil {
		ns.Shutdown()
		return nil, fmt.Errorf("nats connect (in-process): %w", err)
	}
	pc, err := nats.Connect("", nats.InProcessServer(ns), nats.Name("sheetstream-presence"))
	if err != nil {
		nc.Close()
		ns.Shutdown()
		return nil, fmt.Errorf("nats connect presence (in-process): %w", err)
	}

	if opts.Verbose {
		if opts.Listen == "" {
			log.Printf("embedded NATS (core, in-process, no TCP) ready")
		} else {
			log.Printf("embedded NATS (core) ready, TCP listener on %s", opts.Listen)
		}
	}
	return &Bus{ns: ns, nc: nc, pc: pc}, nil
}

// Conn exposes the process's NATS connection for band traffic.
func (b *Bus) Conn() *nats.Conn { return b.nc }

// PresenceConn exposes the separate connection presence rides on.
func (b *Bus) PresenceConn() *nats.Conn { return b.pc }

// ClientURL is the URL an external client would use; only meaningful when a
// TCP listener was configured.
func (b *Bus) ClientURL() string { return b.ns.ClientURL() }

// Publish emits one empty message per dirty band. Leaves only — the broker
// expands nothing and neither do we.
func (b *Bus) Publish(sheetID string, bands []int) error {
	for _, band := range bands {
		if err := b.nc.Publish(BandSubject(sheetID, band), nil); err != nil {
			return fmt.Errorf("publish band %d: %w", band, err)
		}
	}
	return nil
}

// PublishDirty converts a write's dirty cell set into bands and publishes them.
// The affected set is discovered while walking the dependency graph during the
// write, so the subjects cannot be declared up front — the handler hands them
// back here.
func (b *Bus) PublishDirty(sheetID string, cells []CellRef) error {
	return b.Publish(sheetID, BandsFor(cells))
}

// SubscribeBand subscribes to exactly one band of one sheet. cb runs on the
// NATS callback goroutine, so it must not block: poke a channel and return.
func (b *Bus) SubscribeBand(sheetID string, band int, cb func()) (*nats.Subscription, error) {
	sub, err := b.nc.Subscribe(BandSubject(sheetID, band), func(*nats.Msg) { cb() })
	if err != nil {
		return nil, fmt.Errorf("subscribe %s band %d: %w", sheetID, band, err)
	}
	// Empty payloads and a non-blocking callback, but cap the pending queue
	// anyway: a stalled callback must drop messages, never grow memory. The
	// bus is loss-tolerant by design, so a drop costs at most one wake.
	_ = sub.SetPendingLimits(1024, 1<<20)
	return sub, nil
}

// SubscribeSheet subscribes to every band of a sheet with a single `>`
// wildcard. This is the whole point of the scheme: one subscription covers an
// unbounded set of bands without the publisher knowing anything about it.
// Used by debug tooling and tests, not by viewport connections (which want
// exactly their buffer and nothing more).
func (b *Bus) SubscribeSheet(sheetID string, cb func(band int)) (*nats.Subscription, error) {
	sub, err := b.nc.Subscribe(SheetSubject(sheetID), func(m *nats.Msg) {
		if _, band, ok := ParseBandSubject(m.Subject); ok {
			cb(band)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("subscribe sheet %s: %w", sheetID, err)
	}
	_ = sub.SetPendingLimits(4096, 1<<20)
	return sub, nil
}

// PublishPresence emits one empty message on a sheet's presence subject. Like a
// band message it carries nothing: "the set of people here, or where one of them
// is looking, is not what you last rendered — go re-resolve it". The read model
// lives in the registry (presence.go), so there is nothing to put in a payload
// even if the design allowed one.
func (b *Bus) PublishPresence(sheetID string) error {
	if err := b.pc.Publish(PresenceSubject(sheetID), nil); err != nil {
		return fmt.Errorf("publish presence %s: %w", sheetID, err)
	}
	return nil
}

// SubscribePresence subscribes to one sheet's presence subject — one
// subscription per sheet, not one per connection. Presence is a whole-sheet
// fact, so N viewers on the same subject would only make the broker deliver N
// copies of the same empty message into this process for the registry to fan out
// anyway. cb runs on the NATS callback goroutine and must not block.
func (b *Bus) SubscribePresence(sheetID string, cb func()) (*nats.Subscription, error) {
	sub, err := b.pc.Subscribe(PresenceSubject(sheetID), func(*nats.Msg) { cb() })
	if err != nil {
		return nil, fmt.Errorf("subscribe presence %s: %w", sheetID, err)
	}
	// Presence is the most droppable traffic in the system — the next tick
	// carries the same fact — so the queue is short on purpose.
	_ = sub.SetPendingLimits(256, 1<<18)
	return sub, nil
}

// Flush blocks until the server has processed everything published so far on
// both connections. Tests need it; production code generally does not.
func (b *Bus) Flush() error {
	if err := b.nc.Flush(); err != nil {
		return err
	}
	return b.pc.Flush()
}

// Close drains the client connections and shuts the server down.
func (b *Bus) Close() {
	drainClient(b.nc)
	drainClient(b.pc)
	if b.ns != nil {
		b.ns.Shutdown()
		b.ns.WaitForShutdown()
	}
}

// drainClient lets in-flight callbacks finish before the conn closes.
func drainClient(nc *nats.Conn) {
	if nc == nil {
		return
	}
	if err := nc.Drain(); err != nil {
		nc.Close()
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for nc.IsDraining() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	nc.Close()
}

// BandSubject is the leaf subject for one band: sheet.{sheetID}.band.{n}.
func BandSubject(sheetID string, band int) string {
	return subjectRoot + "." + subjectToken(sheetID) + ".band." + strconv.Itoa(band)
}

// PresenceSubject is a sheet's presence subject: sheet.{sheetID}.presence.
// Three tokens where a band subject has four, and no prefix relationship with
// any of them.
func PresenceSubject(sheetID string) string {
	return subjectRoot + "." + subjectToken(sheetID) + ".presence"
}

// SheetSubject is the wildcard covering every band of a sheet.
func SheetSubject(sheetID string) string {
	return subjectRoot + "." + subjectToken(sheetID) + ".>"
}

// ParseBandSubject reverses BandSubject. The sheet ID it returns is the
// tokenised form (see subjectToken), which is why subscribers that care about
// identity carry the original ID alongside rather than parsing it back out.
func ParseBandSubject(subject string) (sheetID string, band int, ok bool) {
	parts := strings.Split(subject, ".")
	if len(parts) != 4 || parts[0] != subjectRoot || parts[2] != "band" {
		return "", 0, false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil {
		return "", 0, false
	}
	return parts[1], n, true
}

// subjectToken makes a sheet ID safe to embed as a single subject token. `.`
// would split the token, and ` `, `*`, `>` are reserved by the protocol or the
// wildcard syntax. Sheet IDs are server-generated slugs in this prototype, so
// this is a guard rail rather than a mapping — two IDs differing only in
// punctuation would collide onto one subject.
func subjectToken(sheetID string) string {
	if sheetID == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(sheetID))
	for _, r := range sheetID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
