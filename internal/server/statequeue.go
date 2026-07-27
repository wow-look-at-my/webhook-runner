package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/queue"
)

const (
	// maxQueueBody bounds a push. A backlog push is a list of short ids
	// ("owner/repo#123"), and the depth cap is what actually bounds the
	// queue — this only keeps one request from being a denial of service.
	maxQueueBody = 256 * 1024
	// maxQueueTake bounds one drain. A caller that wants more calls again;
	// an unbounded take would hand a run more work than its own timeout can
	// possibly cover, which is the failure the backlog exists to prevent.
	maxQueueTake = 1000
)

type queuePushRequest struct {
	Items []string `json:"items"`
}

type queueTakeRequest struct {
	Count int `json:"count"`
}

type queueTakeResponse struct {
	Items []string `json:"items"`
	Depth int      `json:"depth"`
}

// handleQueuePush appends work to one of the calling hook's named queues.
// Already-queued items are reported as duplicates and keep their original
// position, so a caller can re-push its whole candidate set every tick — the
// stateless way to say "this is the work that exists" — without the queue
// growing without bound or its tail starving.
func (s *Server) handleQueuePush(w http.ResponseWriter, r *http.Request, ns, _ string) {
	if s.queues == nil {
		writeError(w, http.StatusServiceUnavailable, "queue store not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxQueueBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req queuePushRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "items must be a non-empty array")
		return
	}
	res, err := s.queues.Push(ns, r.PathValue("name"), req.Items)
	if err != nil {
		s.writeQueueError(w, ns, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleQueueTake removes and returns up to `count` items from the head. The
// items are GONE from the queue when this returns — at-most-once, no lease to
// expire and no in-flight state to leak. Callers of a backlog like this
// re-derive their work each tick, so the next push restores anything a dying
// run drops (see internal/queue's package comment).
func (s *Server) handleQueueTake(w http.ResponseWriter, r *http.Request, ns, _ string) {
	if s.queues == nil {
		writeError(w, http.StatusServiceUnavailable, "queue store not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxIncrBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	req := queueTakeRequest{Count: 1}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
	}
	if req.Count < 1 || req.Count > maxQueueTake {
		writeError(w, http.StatusBadRequest, "invalid count: must be 1..1000")
		return
	}
	items, depth, err := s.queues.Take(ns, r.PathValue("name"), req.Count)
	if err != nil {
		s.writeQueueError(w, ns, err)
		return
	}
	writeJSON(w, http.StatusOK, queueTakeResponse{Items: items, Depth: depth})
}

// handleQueueStat is the cheap "is my backlog draining?" read: one queue's
// depth, never its contents.
func (s *Server) handleQueueStat(w http.ResponseWriter, r *http.Request, ns, _ string) {
	if s.queues == nil {
		writeError(w, http.StatusServiceUnavailable, "queue store not configured")
		return
	}
	name := r.PathValue("name")
	if !queue.ValidName(name) {
		writeError(w, http.StatusBadRequest, queue.ErrBadName.Error())
		return
	}
	writeJSON(w, http.StatusOK, queue.Stat{Name: name, Depth: s.queues.Depth(ns, name)})
}

// handleQueueList reports every non-empty queue the calling hook owns.
func (s *Server) handleQueueList(w http.ResponseWriter, _ *http.Request, ns, _ string) {
	if s.queues == nil {
		writeError(w, http.StatusServiceUnavailable, "queue store not configured")
		return
	}
	writeJSON(w, http.StatusOK, s.queues.List(ns))
}

func (s *Server) writeQueueError(w http.ResponseWriter, ns string, err error) {
	switch {
	case errors.Is(err, queue.ErrItemTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, queue.ErrTooManyQueues), errors.Is(err, queue.ErrTooManyNS):
		writeError(w, http.StatusInsufficientStorage, err.Error())
	case errors.Is(err, queue.ErrBadName), errors.Is(err, queue.ErrBadNamespace), errors.Is(err, queue.ErrEmptyItem):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Error("queue: write failed", "ns", ns, "err", err)
		s.events.Record("queue.write_failed", ns+": queue write failed (rolled back): "+err.Error(),
			map[string]string{"hook": ns})
		writeError(w, http.StatusInternalServerError, "queue store error: "+err.Error())
	}
}
