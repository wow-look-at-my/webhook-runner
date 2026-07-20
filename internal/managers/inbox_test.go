package managers

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInboxDeliverAndNext(t *testing.T) {
	ib := NewInbox(0, nil)
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
	ib := NewInbox(0, nil)
	ib.BindInstance("inst-2", nil, nil, nil)
	_, _, err := ib.Next(context.Background(), "inst-1", time.Millisecond)
	assert.ErrorIs(t, err, ErrNotSession)
	_, _, err = ib.Next(context.Background(), "", time.Millisecond)
	assert.ErrorIs(t, err, ErrNotSession)
}

// Ticks coalesce: at most one queued at a time; consuming it re-allows one.
func TestInboxTickCoalescing(t *testing.T) {
	ib := NewInbox(0, nil)
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

// Overflow drops the OLDEST (newest-wins), reports it, and settles its
// delivery handle as NOT completed.
func TestInboxOverflowDropsOldestLoudly(t *testing.T) {
	var dropped []Event
	ib := NewInbox(2, func(e Event) { dropped = append(dropped, e) })
	ib.BindInstance("i", nil, nil, nil)

	d1 := ib.PushDelivery(nil, []byte(`{"n":1}`))
	ib.PushDelivery(nil, []byte(`{"n":2}`))
	ib.PushDelivery(nil, []byte(`{"n":3}`)) // overflows: drops n=1
	require.Len(t, dropped, 1)
	assert.JSONEq(t, `{"n":1}`, string(dropped[0].Payload))
	<-d1.Done()
	assert.False(t, d1.Completed())

	ev, ok, err := ib.Next(context.Background(), "i", time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	assert.JSONEq(t, `{"n":2}`, string(ev.Payload))
}

// A checked-out event ABANDONS (settles not-completed) when the instance
// dies mid-processing; the buffered backlog survives for the successor.
func TestInboxUnbindAbandonsCheckedOut(t *testing.T) {
	ib := NewInbox(0, nil)
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
	ib := NewInbox(0, nil)
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
		_, _, _ = ib.Next(context.Background(), "i", 300*time.Millisecond)
		close(done)
	}()
	require.Eventually(t, func() bool { return disarmed.Load() == 1 }, time.Second, 5*time.Millisecond)

	// A push while PARKED delivers immediately: no wedge-guard arm, one
	// checkout arm.
	ib.PushDelivery(nil, []byte(`{}`))
	<-done
	assert.Equal(t, int32(3), armed.Load())
}

// A long-poll Next delivers the instant a push arrives (no polling
// latency), and an elapsed wait returns ok=false.
func TestInboxLongPollWakesOnPush(t *testing.T) {
	ib := NewInbox(0, nil)
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
