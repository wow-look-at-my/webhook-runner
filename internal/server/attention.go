// GET /attention (admin port): the persistent "needs attention" surface —
// the CURRENT set of active misconfigurations the attention aggregator
// holds (dropped hooks, unresolvable ${NAME} references, sops decrypt
// failures, the -hooks guard, the containerized-TMPDIR hazard,
// recognized event-derived problems), each with what's wrong and since
// when. The dashboard renders it as the red banner + the Needs attention
// panel, refetched on the stream's "attention" changed-section signal
// (the aggregator's own onChange seam — see server.New).
package server

import (
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
)

// attentionView is the GET /attention response: the active problem count
// (the banner's number) plus every entry, oldest .
type attentionView struct {
	Count   int               `json:"count"`
	Entries []attention.Entry `json:"entries"`
}

func (s *Server) handleAttention(w http.ResponseWriter, _ *http.Request) {
	all := s.attention.Snapshot() // nil-aggregator safe: empty, never nil
	// A hook that is effectively disabled (operator override, or its hook.json `enable: false` default) has its problems filtered out here, at.
	entries := make([]attention.Entry, 0, len(all))
	for _, e := range all {
		if e.Hook != "" && s.effectiveDisabled(e.Hook) {
			continue
		}
		entries = append(entries, e)
	}
	writeJSON(w, http.StatusOK, attentionView{Count: len(entries), Entries: entries})
}
