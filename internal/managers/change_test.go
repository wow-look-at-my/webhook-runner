// The admin-surface change seam (Supervisor.SetOnChange): the dashboard's
// Managers panel and #manager=<id> drill-down are PUSH-fed, and the things
// that move them most — instance output lines, inbox depth, the
// last-delivery stamp, supervision state transitions — record no activity
// event at all. Every of them must fire this seam, or the panel goes
// stale until the operator hits F (the bug this covers).
package managers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// The whole lifecycle over the real supervisor: the seam fires for the
// state transitions, for the instance's output line, and for an inbox
// delivery — none of which record an event of their own.
func TestSupervisorOnChangeCoversTheAdminSurface(t *testing.T) {
	shrinkCadences(t)
	fr := newFakeRunner()
	var n atomic.Int64
	s := New(Options{Runner: fr, Events: events.NewRecorder(50)})
	s.SetOnChange(func() { n.Add(1) })

	m := testManager(t, "m1", `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json","enable":true,"command":["run"]}`)
	s.Update(map[string]*hooks.Manager{"m1": m})
	assert.Positive(t, n.Load(), "Update declares the roster: the panel changed")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	require.Eventually(t, func() bool { return fr.startedCount() >= 1 }, 2*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		st, ok := s.StatusFor("m1")
		return ok && st.State == "running"
	}, 2*time.Second, 10*time.Millisecond)

	// The fake session already emitted an output line and the state walked waiting-lease → starting → running; all of that is seam traffic.
	before := n.Load()
	assert.Positive(t, before)

	// A delivery: inbox depth and the last-delivered stamp are both on the roster row, and PushDelivery records nothing on the activity feed.
	s.Deliver("m1", nil, []byte(`{"x":1}`))
	assert.Greater(t, n.Load(), before, "an inbox delivery moves depth + stamps: signal it")

	// Consuming it moves depth back down — the same panel field.
	before = n.Load()
	fr.mu.Lock()
	instanceID := fr.started[0]
	fr.mu.Unlock()
	_, ok, err := s.InboxNext(context.Background(), "m1", instanceID, 500*time.Millisecond)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Greater(t, n.Load(), before, "a checkout moves inbox depth: signal it")
}

// Output lines are the seam's highest-rate source and the drill-down's
// whole point: EVERY line signals, unthrottled (a throttle could only lose
// the last line, which is the stale-tail bug).
func TestOutputSinkSignalsEveryLine(t *testing.T) {
	var n atomic.Int64
	s := New(Options{})
	s.SetOnChange(func() { n.Add(1) })
	mg := &managed{id: "m1", inbox: NewInbox()}
	sink := s.outputSink(mg)
	for i := 0; i < 25; i++ {
		sink("line")
	}
	assert.EqualValues(t, 25, n.Load())
	assert.Len(t, mg.output, 25)
}

// The seam is optional everywhere (the events.Recorder convention): a
// supervisor with no onChange must behave identically.
func TestOnChangeIsOptional(t *testing.T) {
	s := New(Options{})
	mg := &managed{id: "m1", inbox: NewInbox()}
	mg.inbox.SetOnChange(s.changed) // nil fn behind it
	s.outputSink(mg)("line")
	mg.inbox.PushStart()
	assert.Equal(t, 1, mg.inbox.Depth())
}

// The inbox seam fires with ib.mu released: a callback that reads the
// inbox back (as a naive consumer would) must not deadlock.
func TestInboxOnChangeRunsUnlocked(t *testing.T) {
	ib := NewInbox()
	var wg sync.WaitGroup
	wg.Add(1)
	var depth int
	ib.SetOnChange(func() {
		depth = ib.Depth()
		wg.Done()
	})
	ib.PushStart()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("inbox onChange deadlocked against ib.mu")
	}
	assert.Equal(t, 1, depth)
}
