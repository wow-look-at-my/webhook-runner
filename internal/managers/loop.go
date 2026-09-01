// Supervision loops: manager's whole supervised life (start an instance, watch it end, restart flat) plus the small helpers the loop.

package managers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// startLoopLocked launches a manager's supervision loop (caller holds mu).
func (s *Supervisor) startLoopLocked(mg *managed) {
	if mg.loopRunning || s.shutdown {
		return
	}
	mg.loopRunning = true
	s.wg.Add(1)
	go s.managerLoop(mg)
}

// managerLoop is manager's whole supervised life: start an instance,
// watch it end, restart flat — forever, until the manager is removed,
// disabled (parks), or the supervisor shuts down.
func (s *Supervisor) managerLoop(mg *managed) {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		if s.shutdown {
			mg.loopRunning = false
			mg.state = "stopping"
			s.mu.Unlock()
			s.changed()
			return
		}
		m := s.desired[mg.id]
		if m == nil {
			mg.loopRunning = false
			delete(s.states, mg.id)
			s.mu.Unlock()
			s.reportAttention()
			s.changed()
			return
		}
		disabled := s.disabledFn != nil && s.disabledFn(mg.id, m.EnabledByDefault())
		if disabled {
			mg.state = "disabled"
			s.mu.Unlock()
			s.reportAttention()
			s.changed()
			if !s.sleepFlat(ParkPoll) {
				s.markLoopDone(mg)
				return
			}
			continue
		}
		stopCh := make(chan StopRequest, 1)
		mg.sessionStop = stopCh
		mg.state = "starting"
		instanceID := NewInstanceID()
		mg.instanceID = instanceID
		mg.instanceStarted = time.Now().UTC()
		// The instance's panel title: the run_title template's static render (placeholders resolve against nothing — a manager's title context is.
		mg.title = m.RenderRunTitle(nil, http.Header{})
		hash, _ := m.ContentHash()
		mg.runningHash = hash
		sink := s.outputSink(mg)
		s.mu.Unlock()
		s.reportAttention()
		s.changed()

		name := ContainerName(mg.id)
		// Reap any orphan/stale container : a crashed predecessor process leaves its (dockerd-owned) instance running; the deterministic.
		s.runner.RemoveManagerContainer(name)

		// Seed the instance's event BEFORE it starts, so the very /inbox/next returns it: the tick is the handover/crash recovery pass.
		if m.ReconcileInterval() > 0 {
			mg.inbox.PushTick()
		} else {
			mg.inbox.PushStart()
		}
		tickStop := make(chan struct{})
		if iv := m.ReconcileInterval(); iv > 0 {
			go tickPump(iv, mg.inbox, tickStop)
		}

		onStarted := func() { s.instanceRunning(mg) }
		outcome := s.runner.RunManagerSession(s.runContext(), m, mg.inbox, instanceID, name, stopCh, onStarted, sink)
		close(tickStop)

		// The finish-seam analog: the instance is over — release whatever
		// run-shaped resources it held (cooperative locks, incl. pinned).
		if s.onInstanceEnd != nil {
			s.onInstanceEnd(instanceID)
		}

		s.mu.Lock()
		mg.sessionStop = nil
		mg.runningHash = ""
		mg.instanceID = ""
		mg.instanceStarted = time.Time{}
		mg.touch = nil
		mg.restarts++
		if outcome.RequestedStop {
			mg.consecFails = 0
			mg.lastError = ""
			mg.lastStopReason = outcome.Err
		} else if outcome.Status == runs.StatusCancelled {
			// An operator restart/bounce, not a failure.
			mg.consecFails = 0
			mg.lastError = ""
			mg.lastStopReason = outcome.Err
		} else {
			mg.consecFails++
			mg.lastError = fmt.Sprintf("instance ended: %s", outcome.Status)
			if outcome.Err != "" {
				mg.lastError += ": " + outcome.Err
			}
			mg.lastStopReason = ""
		}
		requested := outcome.RequestedStop
		s.mu.Unlock()
		s.reportAttention()
		s.changed()

		if requested {
			continue // re-evaluate immediately: disable parks, replace restarts, removal exits
		}
		s.mu.Lock()
		mg.state = "restart-wait"
		s.mu.Unlock()
		s.changed()
		if !s.sleepFlat(RestartDelay) {
			s.markLoopDone(mg)
			return
		}
	}
}

// instanceRunning flips the manager to running the moment its container
// actually launched, and captures the live watchdog touch for /wait.
func (s *Supervisor) instanceRunning(mg *managed) {
	s.mu.Lock()
	mg.state = "running"
	mg.touch = mg.inbox.sessionTouch()
	s.mu.Unlock()
	s.reportAttention()
	s.changed()
}

func (s *Supervisor) markLoopDone(mg *managed) {
	s.mu.Lock()
	mg.loopRunning = false
	s.mu.Unlock()
	s.changed()
}

// runContext returns the supervisor's run context (Background before Run —
// tests that drive loops without Run).
func (s *Supervisor) runContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runCtx != nil {
		return s.runCtx
	}
	return context.Background()
}

// sleepFlat waits d, checking for shutdown on a flat ms cadence.
// false = shutting down.
func (s *Supervisor) sleepFlat(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		s.mu.Lock()
		down := s.shutdown
		s.mu.Unlock()
		if down {
			return false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		step := remaining
		if step > 250*time.Millisecond {
			step = 250 * time.Millisecond
		}
		time.Sleep(step)
	}
}

func (s *Supervisor) requestStopLocked(mg *managed, reason string) {
	if mg.sessionStop == nil {
		return
	}
	select {
	case mg.sessionStop <- StopRequest{Reason: reason}:
	default: // a stop is already pending; the reason wins
	}
}

func (s *Supervisor) pokeLocked(mg *managed) {
	select {
	case mg.poke <- struct{}{}:
	default:
	}
}
