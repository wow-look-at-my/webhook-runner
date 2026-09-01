package reloadgate

import (
	"context"
	"sync"
	"time"
)

// DefaultPollTick is the poller run-loop resolution (the internal/scheduler convention): intervals are hour-scale, so -.
const DefaultPollTick = time.Second

// Poller drives Gate.Reconcile on a fixed interval — pure timing with an injected clock, mirroring internal/scheduler: it owns nothing but.
type Poller struct {
	interval time.Duration
	fire     func()
	now      func() time.Time
	tick     time.Duration

	mu   sync.Mutex
	next time.Time
}

// PollerOptions configure a Poller. Fire is required when Interval > .
type PollerOptions struct {
	// Interval between reconcile passes; <= disables the poller.
	Interval time.Duration
	// Fire runs reconcile pass. It is called inline on the Run goroutine, so passes never overlap.
	Fire func()
	// Now is an injectable clock for tests; defaults to time.Now.
	Now func() time.Time
	// Tick is the run-loop resolution; defaults to DefaultPollTick.
	Tick time.Duration
}

// NewPoller constructs a Poller.
func NewPoller(opts PollerOptions) *Poller {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Tick <= 0 {
		opts.Tick = DefaultPollTick
	}
	p := &Poller{
		interval: opts.Interval,
		fire:     opts.Fire,
		now:      opts.Now,
		tick:     opts.Tick,
	}
	if p.interval > 0 {
		p.next = p.now() // due immediately: the startup catch-up pass
	}
	return p
}

// Run drives the poller until ctx is cancelled: immediate pass, then
// per interval. Disabled (interval <= ) returns immediately — no
// timer, no fire.
func (p *Poller) Run(ctx context.Context) {
	if p.interval <= 0 {
		return
	}
	p.fireDue() // the immediate startup pass — don't wait out the tick
	t := time.NewTicker(p.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.fireDue()
		}
	}
}

// fireDue fires when the next-fire time has passed, advancing it by the interval from "now" — a long pause (slept/restarted process) fires and resumes interval out, never.
func (p *Poller) fireDue() {
	if p.interval <= 0 {
		return
	}
	p.mu.Lock()
	due := !p.now().Before(p.next)
	if due {
		p.next = p.now().Add(p.interval)
	}
	p.mu.Unlock()
	if due {
		p.fire()
	}
}
