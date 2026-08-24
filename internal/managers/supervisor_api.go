package managers

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// Poke nudges a manager's loop to re-evaluate now (operator flipped the kill switch back on).
func (s *Supervisor) Poke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mg := s.states[id]; mg != nil {
		s.pokeLocked(mg)
	}
}

// RequestStop gracefully stops a manager's RUNNING instance with the given reason (the operator disable/restart path).
func (s *Supervisor) RequestStop(id, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mg := s.states[id]; mg != nil {
		s.requestStopLocked(mg, reason)
		s.pokeLocked(mg)
	}
}

// Deliver pushes an authenticated delivery into a manager's inbox (buffering while the manager restarts; overflow drops oldest, loudly) and returns its completion handle — the synchronous hold and per-delivery.
func (s *Supervisor) Deliver(id string, headers http.Header, payload []byte) *Delivered {
	s.mu.Lock()
	mg := s.states[id]
	s.mu.Unlock()
	if mg == nil {
		return nil
	}
	return mg.inbox.PushDelivery(headers, payload)
}

// InboxNext long-polls a manager's inbox on behalf of its CURRENT instance
// (see Inbox.Next). ok=false with nil error is the elapsed-wait 204.
func (s *Supervisor) InboxNext(ctx context.Context, id, instanceID string, wait time.Duration) (Event, bool, error) {
	s.mu.Lock()
	mg := s.states[id]
	s.mu.Unlock()
	if mg == nil {
		return Event{}, false, ErrNotSession
	}
	return mg.inbox.Next(ctx, instanceID, wait)
}

// IsCurrentInstance reports whether instanceID is manager id's live
// instance — the liveness guard /spawn, /wait, and /title apply to
// manager-issued tokens.
func (s *Supervisor) IsCurrentInstance(id, instanceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	mg := s.states[id]
	return mg != nil && mg.instanceID != "" && mg.instanceID == instanceID
}

// AnyCurrentInstance reports whether instanceID is the live instance of ANY manager — the id-only liveness question, for callers holding an identity without knowing which manager it belongs to (the KV lock sweeper, deciding whether an.
func (s *Supervisor) AnyCurrentInstance(instanceID string) bool {
	if instanceID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, mg := range s.states {
		if mg != nil && mg.instanceID == instanceID {
			return true
		}
	}
	return false
}

// TouchInstance credits activity to a manager's live instance (the /wait
// hold's watchdog feed). false = not the current instance.
func (s *Supervisor) TouchInstance(id, instanceID string) bool {
	s.mu.Lock()
	mg := s.states[id]
	var touch func()
	ok := mg != nil && mg.instanceID == instanceID && instanceID != ""
	if ok {
		touch = mg.touch
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	if touch != nil {
		touch()
	}
	return true
}

// SetInstanceTitle names the live instance on the panel (POST /title).
// false = not the current instance.
func (s *Supervisor) SetInstanceTitle(id, instanceID, title string) bool {
	s.mu.Lock()
	mg := s.states[id]
	if mg == nil || mg.instanceID == "" || mg.instanceID != instanceID {
		s.mu.Unlock()
		return false
	}
	mg.title = title
	s.mu.Unlock()
	s.changed()
	return true
}

// Statuses returns the roster for GET /managers, alphabetical by id.
func (s *Supervisor) Statuses() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.states))
	for id, mg := range s.states {
		out = append(out, s.statusLocked(id, mg))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// StatusFor returns one manager's row, or ok=false.
func (s *Supervisor) StatusFor(id string) (Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mg := s.states[id]
	if mg == nil {
		return Status{}, false
	}
	return s.statusLocked(id, mg), true
}

// OutputTail returns the live/most-recent instance's recent output lines
// (managers are not runs; their logs live here).
func (s *Supervisor) OutputTail(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	mg := s.states[id]
	if mg == nil {
		return nil
	}
	out := make([]string, len(mg.output))
	copy(out, mg.output)
	return out
}

func (s *Supervisor) statusLocked(id string, mg *managed) Status {
	st := Status{
		ID:               id,
		State:            mg.state,
		Title:            mg.title,
		InstanceID:       mg.instanceID,
		InstanceStarted:  mg.instanceStarted,
		Restarts:         mg.restarts,
		ConsecutiveFails: mg.consecFails,
		LastError:        mg.lastError,
		LastStopReason:   mg.lastStopReason,
		InboxDepth:       mg.inbox.Depth(),
	}
	st.LastDelivered, st.LastTick = mg.inbox.Stamps()
	if m := s.desired[id]; m != nil {
		st.Description = m.Description
		st.ReconcileInterval = m.ReconcileIntervalRaw
		st.Synchronous = m.Synchronous
		st.ConcurrencyGroup = m.ConcurrencyGroup
		st.EnabledByDefault = m.EnabledByDefault()
		st.Disabled = s.disabledFn != nil && s.disabledFn(id, m.EnabledByDefault())
	}
	if !s.leased && !s.shutdown {
		st.State = "waiting-lease"
	}
	return st
}

// outputSink returns the line appender for one instance's output ring:
// bounded, timestamped, replacing the ring at instance start (the panel
// shows the CURRENT/most recent instance's tail; history is the events
// feed's job).
func (s *Supervisor) outputSink(mg *managed) func(string) {
	mg.output = mg.output[:0]
	return func(line string) {
		stamped := time.Now().UTC().Format(time.RFC3339) + " " + line
		s.mu.Lock()
		mg.output = append(mg.output, stamped)
		if len(mg.output) > OutputTailLines {
			mg.output = mg.output[len(mg.output)-OutputTailLines:]
		}
		s.mu.Unlock()
		// The drill-down's log tail just moved.
		s.changed()
	}
}

// reportAttention re-derives the manager problem set: every declared,
// enabled manager without a live instance and with a recorded failure is a
// problem; it clears the moment an instance runs (or the manager is
// disabled/removed). Pushed whole to the aggregator's manager source.
func (s *Supervisor) reportAttention() {
	if s.onAttention == nil {
		return
	}
	s.mu.Lock()
	var entries []AttentionEntry
	for id, mg := range s.states {
		m := s.desired[id]
		if m == nil {
			continue
		}
		if s.disabledFn != nil && s.disabledFn(id, m.EnabledByDefault()) {
			continue
		}
		if mg.instanceID != "" || mg.lastError == "" {
			continue
		}
		entries = append(entries, AttentionEntry{
			ID:       id,
			Failures: mg.consecFails,
			Message: fmt.Sprintf("manager %s is not running (%d consecutive failed instance(s); last: %s); restarting every %s until it holds",
				id, mg.consecFails, mg.lastError, RestartDelay),
		})
	}
	s.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	s.onAttention(entries)
}

// tickPump pushes coalesced reconcile ticks on the manager's flat cadence
// for the life of one instance.
func tickPump(interval time.Duration, ib *Inbox, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ib.PushTick()
		}
	}
}
