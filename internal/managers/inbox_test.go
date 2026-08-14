package managers

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInboxDeliverAndNext(t *testing.T) {
	ib := NewInbox()
	ib.BindInstance("inst-1", nil, nil, nil)

	d := ib.PushDelivery(http.Header{"X-Github-Event": []string{"push"}}, []byte(`{"a":1}`))
	ev, ok, err := ib.Next(context.Background(), "inst-1", time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, KindDelivery, ev.Kind)
	assert.Equal(t, "push", ev.Headers.Get("X-Github-Event"))
	assert.JSONEq(t, `{"a":1}`, string(ev.Payload))
	assert.NotEmpty(t, ev.ID)

	// The delivery settles as PROCESSED on the manager's next Next call.
	select {
	case <-d.Done():
		t.Fatal("delivery must not settle before the next Next call")
	default:
	}
	_, ok, err = ib.Next(context.Background(), "inst-1", 10*time.Millisecond)
	require.NoError(t, err)
	assert.False(t, ok, "empty inbox: the wait elapses")
	<-d.Done()
	assert.True(t, d.Completed())
}

func TestInboxStaleInstanceRefused(t *testing.T) {
	ib := NewInbox()
	ib.BindInstance("inst-2", nil, nil, nil)
	_, _, err := ib.Next(context.Background(), "inst-1", time.Millisecond)
	assert.ErrorIs(t, err, ErrNotSession)
	_, _, err = ib.Next(context.Background(), "", time.Millisecond)
	assert.ErrorIs(t, err, ErrNotSession)
}

// Ticks coalesce: at most one queued at a time; consuming it re-allows one.
func TestInboxTickCoalescing(t *testing.T) {
	ib := NewInbox()
	ib.BindInstance("i", nil, nil, nil)
	assert.True(t, ib.PushTick())
	assert.False(t, ib.PushTick())
	assert.False(t, ib.PushTick())
	assert.Equal(t, 1, ib.Depth())

	ev, ok, err := ib.Next(context.Background(), "i", time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, KindTick, ev.Kind)
	assert.True(t, ib.PushTick(), "consumed tick re-allows one")
}

// The inbox is UNBOUNDED: a burst far past the old 256 cap keeps every
// event, in order, and settles none of them early. The cap used to drop the
// oldest per push, which is how a fan-out tick against a large fleet lost
// real deliveries behind a wall of manager.inbox_dropped.
func TestInboxKeepsEveryEventUnderABurst(t *testing.T) {
	const burst = 1000 // ~4x the retired cap
	ib := NewInbox()
	ib.BindInstance("i", nil, nil, nil)

	handles := make([]*Delivered, 0, burst)
	for i := range burst {
		handles = append(handles, ib.PushDelivery(nil, fmt.Appendf(nil, `{"n":%d}`, i)))
	}
	assert.Equal(t, burst, ib.Depth(), "every pushed event is still queued")

	// Nothing was dropped, so no handle has settled while it waits its turn.
	for i, d := range handles {
		select {
		case <-d.Done():
			t.Fatalf("event %d settled before it was ever delivered", i)
		default:
		}
	}

	// FIFO order survives the burst — the oldest is still first out.
	for i := range burst {
		ev, ok, err := ib.Next(context.Background(), "i", time.Second)
		require.NoError(t, err)
		require.True(t, ok)
		assert.JSONEq(t, fmt.Sprintf(`{"n":%d}`, i), string(ev.Payload))
	}
}

// A checked-out event ABANDONS (settles not-completed) when the instance
// dies mid-processing; the buffered backlog survives for the successor.
func TestInboxUnbindAbandonsCheckedOut(t *testing.T) {
	ib := NewInbox()
	ib.BindInstance("i", nil, nil, nil)
	d := ib.PushDelivery(nil, []byte(`{}`))
	ib.PushDelivery(nil, []byte(`{"later":true}`))

	_, ok, err := ib.Next(context.Background(), "i", time.Second)
	require.NoError(t, err)
	require.True(t, ok)

	ib.UnbindInstance()
	<-d.Done()
	assert.False(t, d.Completed())
	assert.Equal(t, 1, ib.Depth(), "the backlog survives for the next instance")

	ib.BindInstance("i2", nil, nil, nil)
	ev, ok, err := ib.Next(context.Background(), "i2", time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	assert.JSONEq(t, `{"later":true}`, string(ev.Payload))
}

// The manager-shaped watchdog contract: checkout arms; the next Next call
// disarms BEFORE parking (a long empty park must never run armed); a push
// with nobody parked and nothing checked out arms (the wedge guard).
func TestInboxWatchdogArming(t *testing.T) {
	var armed, disarmed atomic.Int32
	ib := NewInbox()
	ib.BindInstance("i", func() { armed.Add(1) }, func() { disarmed.Add(1) }, nil)

	// Wedge guard: a push with no parked waiter arms.
	ib.PushDelivery(nil, []byte(`{}`))
	assert.Equal(t, int32(1), armed.Load())

	// Checkout arms again (fresh clock for the new event).
	_, ok, err := ib.Next(context.Background(), "i", time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int32(2), armed.Load())
	assert.Equal(t, int32(0), disarmed.Load())

	// Coming back disarms before parking.
	done := make(chan struct{})
	go func() {
		// A generous ceiling, not a race window: the push below lands the
		// instant we observe the park, so this wait never actually elapses --
		// it only has to outlast the hand-off.
		_, _, _ = ib.Next(context.Background(), "i", 2*time.Second)
		close(done)
	}()
	// Synchronize on the goroutine actually being PARKED, not merely on the
	// disarm. Next disarms STRICTLY before it increments parked, so waiting on
	// disarmed==1 leaves a window in which parked is still 0. A push in that
	// window sees "nobody parked, nothing checked out" and fires a spurious
	// wedge-guard arm -- and since arm() is an idempotent state re-stamp (see
	// idleWatchdog.Arm) rather than a counted event, that extra call is
	// harmless in production but makes the exact-count assertion below flake
	// ("got 4"). Observing parked==1 under the same mutex push reads closes the
	// window: the wedge guard provably cannot fire, so the delivery produces
	// exactly one checkout arm. parked==1 also proves the disarm already ran,
	// because Next disarms before parked++.
	require.Eventually(t, func() bool {
		ib.mu.Lock()
		defer ib.mu.Unlock()
		return ib.parked == 1
	}, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), disarmed.Load(), "coming back disarms before parking")

	// A push while PARKED delivers immediately: no wedge-guard arm, one
	// checkout arm.
	ib.PushDelivery(nil, []byte(`{}`))
	<-done
	assert.Equal(t, int32(3), armed.Load())
	assert.Equal(t, int32(1), disarmed.Load(), "the parked delivery checks out without disarming")
}

// A long-poll Next delivers the instant a push arrives (no polling
// latency), and an elapsed wait returns ok=false.
func TestInboxLongPollWakesOnPush(t *testing.T) {
	ib := NewInbox()
	ib.BindInstance("i", nil, nil, nil)

	got := make(chan Event, 1)
	go func() {
		ev, ok, err := ib.Next(context.Background(), "i", 5*time.Second)
		if err == nil && ok {
			got <- ev
		}
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	ib.PushDelivery(nil, []byte(`{}`))
	select {
	case <-got:
		assert.Less(t, time.Since(start), time.Second)
	case <-time.After(2 * time.Second):
		t.Fatal("push did not wake the parked Next")
	}
}

func TestNewInstanceIDShape(t *testing.T) {
	a, b := NewInstanceID(), NewInstanceID()
	assert.NotEqual(t, a, b)
	assert.Len(t, a, 26, "the run-id alphabet/length so tokens and names compose identically")
}
