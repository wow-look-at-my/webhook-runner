//go:build !unix

package managers

import "context"

// acquireLease on non-unix platforms: no kernel flock available. The
// runner's production deployment (docker on one linux host) never runs
// here; refuse to supervise rather than pretend single-instance holds.
func (s *Supervisor) acquireLease(ctx context.Context) (release func(), ok bool) {
	if s.leasePath == "" {
		return func() {}, true
	}
	s.log.Error("manager lease requires a unix host (flock); managers will NOT run", "path", s.leasePath)
	<-ctx.Done()
	return nil, false
}
