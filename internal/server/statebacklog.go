package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/backlog"
)

const (
	// maxBacklogBody bounds a push. A backlog push is a list of short ids
	// ("owner/repo#123"), and the depth cap is what actually bounds the
	// queue — this only keeps one request from being a denial of service.
	maxBacklogBody = 256 * 1024
	// maxBacklogTake bounds one drain. A caller that wants more calls again;
	// an unbounded take would hand a run more work than its own timeout can
	// possibly cover, which is the failure the backlog exists to prevent.
	maxBacklogTake = 1000
)

type backlogPushRequest struct {
	Items []string `json:"items"`
}

type backlogTakeRequest struct {
	Count int `json:"count"`
}

type backlogTakeResponse struct {
	Items []string `json:"items"`
	Depth int      `json:"depth"`
}

// handleBacklogPush appends work to one of the calling hook's named queues.
// Already-queued items are reported as duplicates and keep their original
// position, so a caller can re-push its whole candidate set every tick — the
// stateless way to say "this is the work that exists" — without the queue
// growing without bound or its tail starving.
func (s *Server) handleBacklogPush(w http.ResponseWriter, r *http.Request, ns, _ string) {
	if s.backlogs == nil {
		writeError(w, http.StatusServiceUnavailable, "backlog store not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBacklogBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req backlogPushRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "items must be a non-empty array")
		return
	}
	res, err := s.backlogs.Push(ns, r.PathValue("name"), req.Items)
	if err != nil {
		s.writeBacklogError(w, ns, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleBacklogTake removes and returns up to `count` items from the head. The
// items are GONE from the queue when this returns — at-most-once, no lease to
// expire and no in-flight state to leak. Callers of a backlog like this
// re-derive their work each tick, so the next push restores anything a dying
// run drops (see internal/queue's package comment).
func (s *Server) handleBacklogTake(w http.ResponseWriter, r *http.Request, ns, _ string) {
	if s.backlogs == nil {
		writeError(w, http.StatusServiceUnavailable, "backlog store not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxIncrBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	req := backlogTakeRequest{Count: 1}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
	}
	if req.Count < 1 || req.Count > maxBacklogTake {
		writeError(w, http.StatusBadRequest, "invalid count: must be 1..1000")
		return
	}
	items, depth, err := s.backlogs.Take(ns, r.PathValue("name"), req.Count)
	if err != nil {
		s.writeBacklogError(w, ns, err)
		return
	}
	writeJSON(w, http.StatusOK, backlogTakeResponse{Items: items, Depth: depth})
}

// handleBacklogStat is the cheap "is my backlog draining?" read: one queue's
// depth, never its contents.
func (s *Server) handleBacklogStat(w http.ResponseWriter, r *http.Request, ns, _ string) {
	if s.backlogs == nil {
		writeError(w, http.StatusServiceUnavailable, "backlog store not configured")
		return
	}
	name := r.PathValue("name")
	if !backlog.ValidName(name) {
		writeError(w, http.StatusBadRequest, backlog.ErrBadName.Error())
		return
	}
	writeJSON(w, http.StatusOK, backlog.Stat{Name: name, Depth: s.backlogs.Depth(ns, name)})
}

// handleBacklogList reports every non-empty queue the calling hook owns.
func (s *Server) handleBacklogList(w http.ResponseWriter, _ *http.Request, ns, _ string) {
	if s.backlogs == nil {
		writeError(w, http.StatusServiceUnavailable, "backlog store not configured")
		return
	}
	writeJSON(w, http.StatusOK, s.backlogs.List(ns))
}

func (s *Server) writeBacklogError(w http.ResponseWriter, ns string, err error) {
	switch {
	case errors.Is(err, backlog.ErrItemTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, backlog.ErrTooManyQueues), errors.Is(err, backlog.ErrTooManyNS):
		writeError(w, http.StatusInsufficientStorage, err.Error())
	case errors.Is(err, backlog.ErrBadName), errors.Is(err, backlog.ErrBadNamespace), errors.Is(err, backlog.ErrEmptyItem):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Error("backlog: write failed", "ns", ns, "err", err)
		s.events.Record("backlog.write_failed", ns+": backlog write failed (rolled back): "+err.Error(),
			map[string]string{"hook": ns})
		writeError(w, http.StatusInternalServerError, "backlog store error: "+err.Error())
	}
}
