// GET /concurrency (admin port): the live state of every declared
// concurrency group — and, per group, the ADVISORY queue detail: which runs
// hold its slots and which are waiting, in order. This is the operator's
// "what is holding the locks" drill-down: a saturated group (Active == the
// limit, Waiting > 0) is a wedge you can only unstick if the holders are
// one click away, so each entry carries enough run metadata to render a
// run link without another round trip.
package server

import (
	"net/http"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
)

// groupRunView is one run in a group's holder or waiting list. RunID is
// always present; the rest is best-effort enrichment from the live tracker
// (a run evicted from the tracker window keeps its ID and Since so the
// operator still sees THAT something holds the slot).
type groupRunView struct {
	RunID  string `json:"run_id"`
	HookID string `json:"hook_id,omitempty"`
	// Title is the run's friendly display title, when it has one.
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
	// Since is when the run took its slot (holders) or joined the queue (waiting) — the manager's advisory stamp, not a run lifecycle field.
	Since time.Time `json:"since,omitzero"`
	// Started/StartedAt mirror the run row fields (queued instant / container launch) so renderers can reuse the same formatting.
	Started   time.Time `json:"started,omitzero"`
	StartedAt time.Time `json:"started_at,omitzero"`
}

// concurrencyGroupView is one per-group entry: the embedded GroupStatus keeps the original shape (name/limit/declared/overridden/active/waiting) — Holders and WaitingRuns are.
type concurrencyGroupView struct {
	concurrency.GroupStatus
	Holders     []groupRunView `json:"holders,omitempty"`
	WaitingRuns []groupRunView `json:"waiting_runs,omitempty"`
}

// globalCapView is the global run cap's entry: its GlobalStatus (limit/default/overridden/active/waiting) plus the same holder/waiting drill-down lists the groups carry.
type globalCapView struct {
	concurrency.GlobalStatus
	Holders     []groupRunView `json:"holders,omitempty"`
	WaitingRuns []groupRunView `json:"waiting_runs,omitempty"`
}

// concurrencyView is the GET /concurrency document: the global run cap (the ceiling across ALL runs; omitted when no cap is configured) plus.
type concurrencyView struct {
	Global *globalCapView         `json:"global,omitempty"`
	Groups []concurrencyGroupView `json:"groups"`
}

// handleConcurrency reports the global run cap and the live state of every declared concurrency group — limit, active, queued — plus the.
func (s *Server) handleConcurrency(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.concurrencyDoc())
}

func (s *Server) concurrencyDoc() concurrencyView {
	doc := concurrencyView{Groups: s.groupViews()}
	if s.globalCap != nil {
		v := &globalCapView{GlobalStatus: s.globalCap.Status()}
		holders, waiting := s.globalCap.QueueDetail()
		v.Holders = s.groupRunViews(holders)
		v.WaitingRuns = s.groupRunViews(waiting)
		doc.Global = v
	}
	return doc
}

func (s *Server) groupViews() []concurrencyGroupView {
	sts := s.concurrency.Status()
	out := make([]concurrencyGroupView, 0, len(sts))
	for _, st := range sts {
		v := concurrencyGroupView{GroupStatus: st}
		holders, waiting := s.concurrency.QueueDetail(st.Name)
		v.Holders = s.groupRunViews(holders)
		v.WaitingRuns = s.groupRunViews(waiting)
		out = append(out, v)
	}
	return out
}

// groupRunViews enriches the manager's advisory {run id, since} pairs with
// live run metadata from the tracker. Never fails: an unknown run id (e.g.
// evicted from the bounded tracker window) still yields its ID + Since.
func (s *Server) groupRunViews(grs []concurrency.GroupRun) []groupRunView {
	if len(grs) == 0 {
		return nil
	}
	out := make([]groupRunView, 0, len(grs))
	for _, gr := range grs {
		v := groupRunView{RunID: gr.ID, Since: gr.Since}
		if s.tracker != nil {
			if run := s.tracker.Get(gr.ID); run != nil {
				snap := run.Snapshot(0)
				v.HookID = snap.HookID
				v.Title = snap.Title
				v.Status = string(snap.Status)
				v.Started = snap.Started
				v.StartedAt = snap.StartedAt
			}
		}
		out = append(out, v)
	}
	return out
}

// GroupWaiterKeyPrefix marks a Waiter.Key as a concurrency-group wait ("group:<name>") in the derived holder-side waiter lists.
const GroupWaiterKeyPrefix = "group:"

func groupWaiterKey(group string) string { return GroupWaiterKeyPrefix + group }
