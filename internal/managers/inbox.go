// Package managers supervises the fleet's MANAGER entities: persistent,
// single-instance watchers declared at src/managers/<id>/ in the hooks
// tree, each run as ONE long-lived container (an "instance") that the
// supervisor restarts flat on any exit and hands events through a bounded
// in-memory inbox. A manager instance is a FIRST-CLASS identity — it is
// deliberately NOT a run: it never appears in the runs list, the run
// store, or the timeline (operator directive — a forever-running bar would
// permanently pollute the chart). The run-shaped mechanisms it needs (the
// state token, cooperative locks incl. pinning, /wait, /title, /spawn
// parentage, the activity watchdog) are wired against the instance
// identity instead.
package managers

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrNotSession: the presented identity is not the manager's CURRENT
// instance — a stale token from a previous (dead) instance, or a foreign
// caller. The API maps it to 409; refusing stale instances here is a
// second, API-level single-instance enforcement behind the supervisor's
// lease.
var ErrNotSession = errors.New("managers: not the current manager instance")

// Event kinds a manager receives from POST /inbox/next.
const (
	// KindDelivery: an authenticated, skip_if-filtered POST /hook/<id>
	// delivery — headers + raw payload, verbatim.
	KindDelivery = "delivery"
	// KindTick: the runner-injected reconcile signal (reconcile_interval;
	// coalesced — at most one queued at a time — plus one at session
	// start). The ground-truth floor: a manager reconciles on ticks even
	// when deliveries were lost.
	KindTick = "tick"
	// KindStart: session start for EVENT-ONLY managers (no
	// reconcile_interval): a chance to warm up, never a sweep mandate.
	KindStart = "start"
)

// Event is one inbox entry, JSON-shaped exactly as POST /inbox/next
// returns it. ID identifies the event for completion tracking (the
// synchronous-delivery hold and per-delivery github_status ride it).
type Event struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	ReceivedAt time.Time       `json:"received_at"`
	Headers    http.Header     `json:"headers,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// Delivered is a pushed delivery's completion handle: Done closes when the
// manager finished processing the event (its NEXT /inbox/next call after
// checking it out), when the event was dropped on overflow, or when the
// instance died with it checked out. Completed reports which it was —
// true only for the processed case.
type Delivered struct {
	Event Event
	done  chan struct{}

	mu        sync.Mutex
	completed bool
}

// Done closes when the delivery's fate is settled.
func (d *Delivered) Done() <-chan struct{} { return d.done }

// Completed reports whether the manager actually finished processing the
// event (vs dropped/abandoned). Only meaningful once Done is closed.
func (d *Delivered) Completed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.completed
}

func (d *Delivered) settle(ok bool) {
	if d == nil {
		return
	}
	d.mu.Lock()
	select {
	case <-d.done:
		d.mu.Unlock()
		return
	default:
	}
	d.completed = ok
	close(d.done)
	d.mu.Unlock()
}

// DefaultInboxSize bounds each manager's event buffer. Overflow drops the
// OLDEST entry (newest-wins, the fleet's latest-event-wins doctrine),
// loudly via the onDrop callback. 256 is far beyond any healthy backlog —
// a manager consumes events in milliseconds-to-seconds; a full inbox means
// it is down or wedged, and the reconcile pass on its next instance start
// re-derives whatever the drops lost.
const DefaultInboxSize = 256

// nextWakeInterval bounds how late Next notices its deadline or a
// cancelled context (pushes broadcast immediately, so event latency is
// unaffected). Flat, per doctrine.
const nextWakeInterval = 100 * time.Millisecond

// entry pairs a queued event with its completion handle (nil for
// ticks/starts — only deliveries have consumers awaiting fates).
type entry struct {
	ev Event
	d  *Delivered
}

// Inbox is one manager's bounded event queue plus its instance binding. It
// exists per DECLARED manager (created at reload, surviving instance
// restarts — buffering while the manager is down is the point) and is
// bound to the CURRENT instance's identity while one is live, which is
// what lets POST /inbox/next refuse a stale instance's token.
//
// The inbox also owns the manager-shaped WATCHDOG arming (the arm/disarm
// instance hooks): the instance's idle watchdog runs only while an event
// is CHECKED OUT — delivered by Next and not yet followed by the manager's
// next Next call — or when events sit queued with no consumer parked and
// nothing checked out (a manager that stopped calling Next is as wedged as
// one that went silent mid-event). A manager parked in its long-poll with
// an empty inbox is healthy and owes nothing.
type Inbox struct {
	mu   sync.Mutex
	cond *sync.Cond

	buf        []entry
	max        int
	tickQueued bool

	instanceID string
	arm        func() // watchdog Arm — nil-safe
	disarm     func() // watchdog Disarm — nil-safe
	touch      func() // watchdog Touch — the /wait activity feed; nil-safe
	checkedOut *entry // the event delivered by the last Next, until the next Next
	parked     int    // Next calls currently blocked waiting for an event

	// lastDelivered/lastTick are observability stamps for the admin API.
	lastDelivered time.Time
	lastTick      time.Time

	onDrop func(Event) // overflow observer — nil-safe
}

// NewInbox constructs an inbox with the given capacity (<=0 =
// DefaultInboxSize). onDrop observes each overflow-dropped event (loud —
// the caller records the activity event); nil is safe.
func NewInbox(size int, onDrop func(Event)) *Inbox {
	if size <= 0 {
		size = DefaultInboxSize
	}
	ib := &Inbox{max: size, onDrop: onDrop}
	ib.cond = sync.NewCond(&ib.mu)
	return ib
}

// BindInstance attaches the current instance: its identity (the token
// /inbox/next must present) and the watchdog hooks. Clears any stale
// checked-out state from a previous instance — a fresh container starts
// with a clean slate and the buffered backlog intact; an event the dead
// instance had checked out settles as abandoned.
func (ib *Inbox) BindInstance(instanceID string, arm, disarm, touch func()) {
	ib.mu.Lock()
	stale := ib.checkedOut
	ib.instanceID = instanceID
	ib.arm = arm
	ib.disarm = disarm
	ib.touch = touch
	ib.checkedOut = nil
	ib.mu.Unlock()
	if stale != nil {
		stale.d.settle(false)
	}
	ib.cond.Broadcast()
}

// sessionTouch returns the live instance's watchdog Touch (nil when none)
// — the supervisor hands it to the /wait hold.
func (ib *Inbox) sessionTouch() func() {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return ib.touch
}

// UnbindInstance detaches the ended instance. Buffered events stay — the
// next instance drains them; a checked-out event settles as ABANDONED (the
// instance died mid-processing; synchronous holders and github_status see
// the failure).
func (ib *Inbox) UnbindInstance() {
	ib.mu.Lock()
	stale := ib.checkedOut
	ib.instanceID = ""
	ib.arm = nil
	ib.disarm = nil
	ib.touch = nil
	ib.checkedOut = nil
	ib.mu.Unlock()
	if stale != nil {
		stale.d.settle(false)
	}
	ib.cond.Broadcast()
}

// InstanceID reports the bound instance's identity ("" = none live).
func (ib *Inbox) InstanceID() string {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return ib.instanceID
}

// Depth reports how many events are queued (a checked-out event left the
// buffer and does not count).
func (ib *Inbox) Depth() int {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return len(ib.buf)
}

// Stamps reports when the inbox last accepted a delivery and a tick (zero
// = never) — the admin surface's "last event / last tick" columns.
func (ib *Inbox) Stamps() (lastDelivered, lastTick time.Time) {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return ib.lastDelivered, ib.lastTick
}

// PushDelivery enqueues an authenticated delivery and returns its
// completion handle (the synchronous hold and per-delivery github_status
// await it). The request's header map and body are cloned — they belong
// to the HTTP handler's lifetime, not ours.
func (ib *Inbox) PushDelivery(headers http.Header, payload []byte) *Delivered {
	var h http.Header
	if headers != nil {
		h = headers.Clone()
	}
	p := make(json.RawMessage, len(payload))
	copy(p, payload)
	d := &Delivered{
		Event: Event{ID: newEventID(), Kind: KindDelivery, ReceivedAt: time.Now().UTC(), Headers: h, Payload: p},
		done:  make(chan struct{}),
	}
	ib.push(entry{ev: d.Event, d: d})
	return d
}

// PushTick enqueues a reconcile tick, COALESCED: at most one tick sits in
// the buffer at a time (a slow manager never accumulates a tick backlog —
// the next tick after it catches up covers everything). Reports whether a
// tick was actually enqueued.
func (ib *Inbox) PushTick() bool {
	ib.mu.Lock()
	if ib.tickQueued {
		ib.mu.Unlock()
		return false
	}
	ib.mu.Unlock()
	ib.push(entry{ev: Event{ID: newEventID(), Kind: KindTick, ReceivedAt: time.Now().UTC()}})
	return true
}

// PushStart enqueues the event-only session-start event.
func (ib *Inbox) PushStart() {
	ib.push(entry{ev: Event{ID: newEventID(), Kind: KindStart, ReceivedAt: time.Now().UTC()}})
}

func (ib *Inbox) push(e entry) {
	var dropped *entry
	ib.mu.Lock()
	if len(ib.buf) >= ib.max {
		victim := ib.buf[0]
		ib.buf = append(ib.buf[:0], ib.buf[1:]...)
		if victim.ev.Kind == KindTick {
			ib.tickQueued = false
		}
		dropped = &victim
	}
	ib.buf = append(ib.buf, e)
	switch e.ev.Kind {
	case KindTick:
		ib.tickQueued = true
		ib.lastTick = e.ev.ReceivedAt
	case KindDelivery:
		ib.lastDelivered = e.ev.ReceivedAt
	}
	// The wedge guard: an event queued while the manager is neither parked
	// in Next nor mid-event means it is not consuming — arm the watchdog so
	// a manager whose loop broke gets reaped and restarted instead of
	// sitting on a growing backlog forever. (Mid-event: already armed.
	// Parked: the waiter picks the event up immediately and arms on
	// checkout.)
	arm := ib.arm
	shouldArm := ib.parked == 0 && ib.checkedOut == nil && ib.instanceID != ""
	onDrop := ib.onDrop
	ib.mu.Unlock()

	if shouldArm && arm != nil {
		arm()
	}
	if dropped != nil {
		dropped.d.settle(false)
		if onDrop != nil {
			onDrop(dropped.ev)
		}
	}
	ib.cond.Broadcast()
}

// Next is the long-poll pop behind POST /inbox/next: called by the CURRENT
// instance (instanceID must match the binding), it first settles the
// previous checked-out event as PROCESSED — disarming the watchdog before
// any parking, so a long empty-inbox park can never be reaped as silence —
// then waits up to wait for an event. Delivering one arms the watchdog and
// checks it out; an elapsed wait returns ok=false (the 204 re-poll).
// ErrNotSession reports a stale or foreign instance token. ctx aborts the
// wait (client disconnect / server shutdown) without popping anything.
func (ib *Inbox) Next(ctx context.Context, instanceID string, wait time.Duration) (Event, bool, error) {
	ib.mu.Lock()
	if instanceID == "" || ib.instanceID != instanceID {
		ib.mu.Unlock()
		return Event{}, false, ErrNotSession
	}
	var done *entry
	var disarm func()
	if ib.checkedOut != nil {
		done = ib.checkedOut
		ib.checkedOut = nil
		disarm = ib.disarm
	}
	ib.mu.Unlock()
	// The manager came back for more: whatever it was processing is DONE.
	// Settle + disarm strictly BEFORE parking — the park must never run
	// armed, and a synchronous holder must never outwait a finished event.
	if done != nil {
		done.d.settle(true)
	}
	if disarm != nil {
		disarm()
	}

	// cond.Wait has no timeout; a ticking broadcaster keeps the loop honest
	// against the deadline and the context.
	deadline := time.Now().Add(wait)
	stopWake := make(chan struct{})
	go func() {
		t := time.NewTicker(nextWakeInterval)
		defer t.Stop()
		for {
			select {
			case <-stopWake:
				return
			case <-ctx.Done():
				ib.cond.Broadcast()
				return
			case <-t.C:
				ib.cond.Broadcast()
			}
		}
	}()
	defer close(stopWake)

	ib.mu.Lock()
	ib.parked++
	for {
		if ib.instanceID != instanceID {
			ib.parked--
			ib.mu.Unlock()
			return Event{}, false, ErrNotSession
		}
		if len(ib.buf) > 0 {
			e := ib.buf[0]
			ib.buf = append(ib.buf[:0], ib.buf[1:]...)
			if e.ev.Kind == KindTick {
				ib.tickQueued = false
			}
			ib.checkedOut = &e
			arm := ib.arm
			ib.parked--
			ib.mu.Unlock()
			if arm != nil {
				arm()
			}
			return e.ev, true, nil
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			ib.parked--
			ib.mu.Unlock()
			return Event{}, false, nil
		}
		ib.cond.Wait()
	}
}

// NewInstanceID mints a manager-instance identity: 16 random bytes,
// base32-lowercased — the run-ID alphabet (a-z2-7), so everything that
// composes ids into names/tokens treats instances and runs identically.
func NewInstanceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Timestamp fallback: uniqueness within one process is all the
		// callers need (tokens bind ns+id; container names are per-manager).
		return "mgr" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(time.Now().String())))[:23]
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

func newEventID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}
