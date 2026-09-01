package queue

import (
	"log/slog"
	"sync"
	"time"
)

// Dispatcher turns due entries into runs. It sleeps until the soonest entry is due — NOT on a fixed tick — and wakes early the moment an enqueue lands, so a doorbell ring costs channel send and a run starts immediately while an idle fleet costs nothing at all.
type Dispatcher struct {
	store *Store
	fire  func(Entry)
	now   func() time.Time
	log   *slog.Logger

	// Bounds how long the loop sleeps with nothing due. This is NOT a poll: with a signal on every enqueue it never has to expire to do its job.
	idle time.Duration

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// DefaultIdleWake bounds a sleep with nothing due (see Dispatcher.idle).
const DefaultIdleWake = time.Minute

// DispatcherOptions configure a Dispatcher. Store and Fire are required.
type DispatcherOptions struct {
	Store *Store
	// Fire dispatches a run for claimed entry. It must not block for long — the serve wiring starts an async run and returns.
	Fire func(Entry)
	// Now is an injectable clock for tests; defaults to time.Now.
	Now func() time.Time
	// IdleWake overrides DefaultIdleWake.
	IdleWake time.Duration
	Log      *slog.Logger
}

// NewDispatcher builds a Dispatcher. Start must be called to run it.
func NewDispatcher(o DispatcherOptions) *Dispatcher {
	d := &Dispatcher{
		store: o.Store,
		fire:  o.Fire,
		now:   o.Now,
		log:   o.Log,
		idle:  o.IdleWake,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	if d.idle <= 0 {
		d.idle = DefaultIdleWake
	}
	return d
}

// Start runs the dispatch loop until Stop.
func (d *Dispatcher) Start() {
	if d == nil || d.store == nil || d.fire == nil {
		return
	}
	go d.loop()
}

// Stop ends the loop and waits for it to exit.
func (d *Dispatcher) Stop() {
	if d == nil {
		return
	}
	d.once.Do(func() { close(d.stop) })
	<-d.done
}

func (d *Dispatcher) loop() {
	defer close(d.done)
	timer := time.NewTimer(d.idle)
	defer timer.Stop()
	for {
		d.drain()

		wait := d.idle
		if next, ok := d.store.NextDue(); ok {
			if until := next.Sub(d.now()); until < wait {
				wait = until
			}
		}
		if wait < 0 {
			wait = 0
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)

		select {
		case <-d.stop:
			return
		case <-d.store.wake: // an enqueue landed — re-read immediately
		case <-timer.C: // the soonest entry came due (or the idle bound elapsed)
		}
	}
}

// drain claims and fires everything due right now.
func (d *Dispatcher) drain() {
	for _, e := range d.store.Claim(d.now()) {
		d.log.Info("queue: firing", "hook", e.Namespace, "key", e.Key, "enqueues", e.Enqueues,
			"queued_for", d.now().Sub(e.EnqueuedAt).Round(time.Millisecond).String())
		d.fire(e)
	}
}
