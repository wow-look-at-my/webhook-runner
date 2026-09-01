package server

import (
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/spool"
)

// spoolDelivery parks a delivery that arrived while the runner was draining,
// so a deploy window costs a webhook its LATENCY instead of its existence.
// Reports the spool id and whether it was parked; false means the caller must
// answer the honest (no spool configured, or the spool is at its bound).
//
// Failure here is loud on the activity feed: a dropped delivery is invisible
// on GitHub's side (it records the response, and a looks like any other
// failed attempt nobody will retry), so the runner's own feed is the only
// place the loss can surface.
func (s *Server) spoolDelivery(hookID, title string, headers http.Header, body []byte) (string, bool) {
	if s.spool == nil {
		return "", false
	}
	id, err := s.spool.Put(spool.Entry{
		HookID:  hookID,
		Title:   title,
		Headers: headers.Clone(),
		Body:    body,
	})
	if err != nil {
		s.log.Error("spooling a delivery during drain failed", "hook", hookID, "err", err)
		s.events.Record("spool.failed",
			"delivery for "+hookID+" could NOT be parked during shutdown ("+err.Error()+
				") — answering 503, and GitHub will not re-send it", map[string]string{"hook": hookID})
		return "", false
	}
	s.log.Info("delivery spooled during drain", "hook", hookID, "spool_id", id)
	s.events.Record("spool.parked",
		"delivery for "+hookID+" parked during shutdown; it runs on the next start",
		map[string]string{"hook": hookID})
	return id, true
}
