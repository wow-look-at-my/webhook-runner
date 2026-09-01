package managers

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// fakeRunner is a SessionRunner whose sessions block until stopped (or a
// scripted exit), recording every call.
type fakeRunner struct {
	mu       sync.Mutex
	started  []string // instance ids in start order
	removed  []string // container names rm -f'ed
	exitOnce chan SessionOutcome
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{exitOnce: make(chan SessionOutcome, 8)}
}

func (f *fakeRunner) RunManagerSession(ctx context.Context, m *hooks.Manager, ib *Inbox, instanceID, containerName string, stop <-chan StopRequest, onStarted func(), sink func(string)) SessionOutcome {
	f.mu.Lock()
	f.started = append(f.started, instanceID)
	f.mu.Unlock()
	ib.BindInstance(instanceID, nil, nil, nil)
	defer ib.UnbindInstance()
	if onStarted != nil {
		onStarted()
	}
	if sink != nil {
		sink("hello from " + instanceID)
	}
	select {
	case req := <-stop:
		return SessionOutcome{Status: runs.StatusCancelled, RequestedStop: true, Err: req.Reason}
	case out := <-f.exitOnce:
		return out
	case <-ctx.Done():
		return SessionOutcome{Status: runs.StatusCancelled, RequestedStop: true, Err: "ctx"}
	}
}

func (f *fakeRunner) RemoveManagerContainer(name string) {
	f.mu.Lock()
	f.removed = append(f.removed, name)
	f.mu.Unlock()
}

func (f *fakeRunner) startedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

func testManager(t *testing.T, id string, doc string) *hooks.Manager {
	t.Helper()
	dir := filepath.Join(t.TempDir(), id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644))
	p := filepath.Join(dir, "manager.json")
	require.NoError(t, os.WriteFile(p, []byte(doc), 0o644))
	m, err := hooks.ParseManager(id, p, []byte(doc))
	require.NoError(t, err)
	return m
}

func shrinkCadences(t *testing.T) {
	t.Helper()
	oldRestart, oldPark := RestartDelay, ParkPoll
	RestartDelay, ParkPoll = 30*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { RestartDelay, ParkPoll = oldRestart, oldPark })
}

// The core loop: an ENABLED manager starts an instance (after reaping the
// deterministic container name), restarts FLAT on exit, and its failures
// surface on the attention seam until an instance holds.
func TestSupervisorRestartsFlat(t *testing.T) {
	shrinkCadences(t)
	fr := newFakeRunner()
	var attn []AttentionEntry
	var attnMu sync.Mutex
	s := New(Options{
		Runner: fr,
		Events: events.NewRecorder(50),
		OnAttention: func(e []AttentionEntry) {
			attnMu.Lock()
			attn = e
			attnMu.Unlock()
		},
	})
	m := testManager(t, "m1", `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json","enable":true,"command":["run"]}`)
	s.Update(map[string]*hooks.Manager{"m1": m})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	require.Eventually(t, func() bool { return fr.startedCount() >= 1 }, 2*time.Second, 10*time.Millisecond)
	st, ok := s.StatusFor("m1")
	require.True(t, ok)
	assert.Equal(t, "running", st.State)
	assert.NotEmpty(t, st.InstanceID)
	assert.Contains(t, s.OutputTail("m1")[0], "hello from")
	fr.mu.Lock()
	assert.Contains(t, fr.removed, ContainerName("m1"), "orphan reap precedes every start")
	fr.mu.Unlock()

	// Crash → flat restart with a FRESH instance id; the failure surfaces on attention until the next instance runs.
	fr.exitOnce <- SessionOutcome{Status: runs.StatusFailure, Err: "exit 1"}
	require.Eventually(t, func() bool { return fr.startedCount() >= 2 }, 2*time.Second, 10*time.Millisecond)
	fr.mu.Lock()
	assert.NotEqual(t, fr.started[0], fr.started[1])
	fr.mu.Unlock()

	cancel()
	s.Shutdown()
	attnMu.Lock()
	defer attnMu.Unlock()
	_ = attn // the seam fired; content asserted in TestSupervisorAttention
}

// Default-on + the kill switch: a manager without an `enable` field starts
// on deploy; the operator's disable gracefully stops it and parks the
// loop, and re-enabling starts a fresh instance.
func TestSupervisorDefaultOnAndKillSwitch(t *testing.T) {
	shrinkCadences(t)
	fr := newFakeRunner()
	var disabledMu sync.Mutex
	disabled := map[string]bool{} // operator overrides; absent = default
	s := New(Options{
		Runner: fr,
		Disabled: func(id string, defaultEnabled bool) bool {
			disabledMu.Lock()
			defer disabledMu.Unlock()
			if v, ok := disabled[id]; ok {
				return v
			}
			return !defaultEnabled
		},
	})
	m := testManager(t, "m1", `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json","command":["run"]}`) // no enable → default ON
	s.Update(map[string]*hooks.Manager{"m1": m})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// No flip needed: the declared manager runs on deploy.
	require.Eventually(t, func() bool { return fr.startedCount() == 1 }, 2*time.Second, 10*time.Millisecond)

	// Operator disables → graceful stop, parks, no restart.
	disabledMu.Lock()
	disabled["m1"] = true
	disabledMu.Unlock()
	s.RequestStop("m1", "disabled by operator")
	require.Eventually(t, func() bool {
		st, _ := s.StatusFor("m1")
		return st.State == "disabled"
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, 1, fr.startedCount(), "a parked manager must not restart")

	// Operator re-enables → a fresh instance starts.
	disabledMu.Lock()
	disabled["m1"] = false
	disabledMu.Unlock()
	s.Poke("m1")
	require.Eventually(t, func() bool { return fr.startedCount() == 2 }, 2*time.Second, 10*time.Millisecond)

	cancel()
	s.Shutdown()
}

// Update with changed content stops the running instance ("superseded by
// reload") and the loop starts the new tree's image; removal exits the
// loop entirely. The finish-seam analog fires per instance end.
func TestSupervisorReplaceAndRemove(t *testing.T) {
	shrinkCadences(t)
	fr := newFakeRunner()
	var endedMu sync.Mutex
	var ended []string
	s := New(Options{
		Runner:        fr,
		OnInstanceEnd: func(id string) { endedMu.Lock(); ended = append(ended, id); endedMu.Unlock() },
	})
	m := testManager(t, "m1", `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json","enable":true,"command":["run"]}`)
	s.Update(map[string]*hooks.Manager{"m1": m})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	require.Eventually(t, func() bool { return fr.startedCount() == 1 }, 2*time.Second, 10*time.Millisecond)

	// Change the manager's content (a new file changes the content hash) and re-Update: the running instance is superseded and a new starts.
	require.NoError(t, os.WriteFile(filepath.Join(m.Dir(), "code.ts"), []byte("v2"), 0o644))
	s.Update(map[string]*hooks.Manager{"m1": m})
	require.Eventually(t, func() bool { return fr.startedCount() == 2 }, 2*time.Second, 10*time.Millisecond)
	endedMu.Lock()
	assert.Len(t, ended, 1, "the finish-seam analog fired for the superseded instance")
	endedMu.Unlock()

	// Removal: the instance stops and the manager leaves the roster.
	s.Update(map[string]*hooks.Manager{})
	require.Eventually(t, func() bool {
		_, ok := s.StatusFor("m1")
		return !ok
	}, 2*time.Second, 10*time.Millisecond)

	cancel()
	s.Shutdown()
}

// Deliveries buffer while no instance is live and drain into the next ;
// InboxNext refuses stale instances.
func TestSupervisorDeliveryBuffering(t *testing.T) {
	shrinkCadences(t)
	fr := newFakeRunner()
	s := New(Options{Runner: fr})
	m := testManager(t, "m1", `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json","command":["run"]}`)
	s.Update(map[string]*hooks.Manager{"m1": m}) // Run() never started: no instance is live

	d := s.Deliver("m1", nil, []byte(`{"buffered":true}`))
	require.NotNil(t, d)
	st, _ := s.StatusFor("m1")
	assert.Equal(t, 1, st.InboxDepth)

	assert.Nil(t, s.Deliver("nope", nil, nil))
	_, _, err := s.InboxNext(context.Background(), "m1", "stale-instance", time.Millisecond)
	assert.ErrorIs(t, err, ErrNotSession)
}

// The single-instance lease: with a real flock file, a supervisor
// blocks until the releases (shutdown), then acquires and runs.
func TestSupervisorLeaseHandover(t *testing.T) {
	shrinkCadences(t)
	lease := filepath.Join(t.TempDir(), "managers.lock")
	frA, frB := newFakeRunner(), newFakeRunner()
	mkSup := func(fr *fakeRunner) *Supervisor {
		return New(Options{Runner: fr, LeasePath: lease})
	}
	m := testManager(t, "m1", `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json","enable":true,"command":["run"]}`)

	supA := mkSup(frA)
	supA.Update(map[string]*hooks.Manager{"m1": m})
	ctxA, cancelA := context.WithCancel(context.Background())
	go supA.Run(ctxA)
	require.Eventually(t, func() bool { return frA.startedCount() == 1 }, 2*time.Second, 10*time.Millisecond)

	supB := mkSup(frB)
	supB.Update(map[string]*hooks.Manager{"m1": m})
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	go supB.Run(ctxB)
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 0, frB.startedCount(), "the lease holder's instances block the successor")

	// Handover: A shuts down (instances stop BEFORE the flock releases); B acquires and starts its own instance.
	cancelA()
	supA.Shutdown()
	require.Eventually(t, func() bool { return frB.startedCount() == 1 }, 3*time.Second, 10*time.Millisecond)

	cancelB()
	supB.Shutdown()
}

// Attention: a failing (enabled) manager surfaces entry naming its
// consecutive failures; a running instance clears it.
func TestSupervisorAttention(t *testing.T) {
	shrinkCadences(t)
	fr := newFakeRunner()
	var mu sync.Mutex
	var current []AttentionEntry
	s := New(Options{
		Runner:      fr,
		OnAttention: func(e []AttentionEntry) { mu.Lock(); current = e; mu.Unlock() },
	})
	m := testManager(t, "m1", `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json","enable":true,"command":["run"]}`)
	s.Update(map[string]*hooks.Manager{"m1": m})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	require.Eventually(t, func() bool { return fr.startedCount() == 1 }, 2*time.Second, 10*time.Millisecond)

	fr.exitOnce <- SessionOutcome{Status: runs.StatusError, Err: "boom"}
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(current) == 1 && current[0].Failures >= 1
	}, 2*time.Second, 10*time.Millisecond, "a failed instance surfaces while none is running")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(current) == 0
	}, 2*time.Second, 10*time.Millisecond, "the restart clears the entry once an instance runs")

	cancel()
	s.Shutdown()
}
