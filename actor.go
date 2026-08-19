package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// One goroutine per sheet.
//
// Every write to a sheet runs on that sheet's own goroutine, which buys a total
// order per sheet without OT or CRDTs: two people editing the same cell at the
// same instant are two commands in one queue, and the event log's seq is the
// order they happened in. Different sheets have different queues and never wait
// on each other.
//
// Reads bypass the actor. SQLite in WAL mode already gives readers a consistent
// snapshot concurrent with the single writer, so a reader never blocks the
// writer and vice versa. Pushing the windowed render (which runs for every
// viewer on every change) through the write queue would serialize N renders
// behind whatever edit is in flight to buy an isolation guarantee SQLite
// provides for free. The actor protects write ordering only.
//
// The consequence: a read may observe the state just before or just after a
// write, but never a partial write, because WriteCell is one transaction
// (store.go).

// ErrActorsClosed is returned by Do after Close.
var ErrActorsClosed = errors.New("actors closed")

// laneQueueDepth is how many commands can be queued per sheet before Do
// blocks. Backpressure is intentional: if one sheet's writer falls behind,
// slowing its writers is better than growing an unbounded queue.
const laneQueueDepth = 64

type actorCmd struct {
	ctx   context.Context
	fn    func(*Sheet) error
	reply chan error
}

type lane struct {
	id string
	ch chan actorCmd
}

// Actors is the collection of per-sheet writer goroutines. Lanes are created
// on first write to a sheet and live until Close.
type Actors struct {
	cache *SheetCache

	// mu is RLocked while sending on a lane channel and Locked to close
	// them, which is what makes "close the channels" safe against concurrent
	// senders without a second signalling mechanism.
	mu     sync.RWMutex
	lanes  map[string]*lane
	closed bool

	wg sync.WaitGroup
}

// NewActors builds a writer pool over a sheet cache.
func NewActors(cache *SheetCache) *Actors {
	return &Actors{cache: cache, lanes: make(map[string]*lane)}
}

// Do runs fn on sheetID's writer goroutine and waits for it to finish,
// returning fn's error. Writes to one sheet are serialized in call order;
// writes to different sheets run concurrently.
func (a *Actors) Do(sheetID string, fn func(*Sheet) error) error {
	return a.DoCtx(context.Background(), sheetID, fn)
}

// DoCtx is Do with cancellation. Cancelling only abandons the *wait* — a
// command already handed to the lane still runs, because half-applying a
// transaction is not something the caller gets to opt into.
func (a *Actors) DoCtx(ctx context.Context, sheetID string, fn func(*Sheet) error) error {
	if err := validSheetID(sheetID); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("actors: nil fn")
	}
	cmd := actorCmd{ctx: ctx, fn: fn, reply: make(chan error, 1)}

	a.mu.RLock()
	if a.closed {
		a.mu.RUnlock()
		return ErrActorsClosed
	}
	l := a.lanes[sheetID]
	a.mu.RUnlock()

	if l == nil {
		l = a.ensureLane(sheetID)
		if l == nil {
			return ErrActorsClosed
		}
	}

	// Hold the read lock across the send so Close (which takes the write
	// lock) cannot close the channel underneath us.
	a.mu.RLock()
	if a.closed {
		a.mu.RUnlock()
		return ErrActorsClosed
	}
	select {
	case l.ch <- cmd:
		a.mu.RUnlock()
	case <-ctx.Done():
		a.mu.RUnlock()
		return ctx.Err()
	}

	select {
	case err := <-cmd.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Actors) ensureLane(sheetID string) *lane {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	if l, ok := a.lanes[sheetID]; ok {
		return l
	}
	l := &lane{id: sheetID, ch: make(chan actorCmd, laneQueueDepth)}
	a.lanes[sheetID] = l
	a.wg.Add(1)
	go a.run(l)
	return l
}

// run is the sheet's writer goroutine. It exits when its channel is closed,
// after draining whatever is still queued — that drain is the graceful part of
// shutdown: accepted commands are not silently dropped.
func (a *Actors) run(l *lane) {
	defer a.wg.Done()
	for cmd := range l.ch {
		cmd.reply <- a.exec(l.id, cmd)
	}
}

func (a *Actors) exec(sheetID string, cmd actorCmd) (err error) {
	// A panic in a handler must not take the writer goroutine (and with it
	// every future write to this sheet) down with it.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in sheet %s writer: %v", sheetID, r)
		}
	}()
	// Open through the cache on every command so eviction stays correct: the
	// lane never pins a handle across commands.
	sh, oerr := a.cache.Open(sheetID)
	if oerr != nil {
		return oerr
	}
	return cmd.fn(sh)
}

// Close stops accepting new commands, lets every lane drain what it already
// accepted, and waits for the writer goroutines to exit.
func (a *Actors) Close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		a.wg.Wait()
		return
	}
	a.closed = true
	for _, l := range a.lanes {
		close(l.ch)
	}
	a.mu.Unlock()
	a.wg.Wait()
}

// Lanes is the number of live writer goroutines. Test/debug aid.
func (a *Actors) Lanes() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.lanes)
}
