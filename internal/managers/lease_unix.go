//go:build unix

package managers

import (
	"context"
	"os"
	"syscall"
	"time"
)

// acquireLease takes the single-instance manager lease: an EXCLUSIVE
// kernel flock on LeasePath, flat-polled until acquired or ctx ends.
//
// Why flock rather than a heartbeat-TTL lease record: the kernel releases
// a flock the INSTANT its holder dies — no renewals, no TTL tuning, no
// clock skew, no false expiry when the holder stalls — and cross-process
// flock arbitration is already this codebase's pattern (runstore's bbolt
// file lock gates the whole process the same way during rolling-update
// overlap). The lease is host-scoped, matching the single-host dockerd
// deployment; the ORPHAN half of crash-safety (a dead runner's session
// containers live on under dockerd) is the deterministic container name +
// rm -f-on-acquire in the manager loop.
//
// Deploy handover: the OLD process stops its sessions before its flock
// releases (process exit releases it at the latest), while the NEW process
// flat-polls here — so live sessions of manager cannot overlap.
func (s *Supervisor) acquireLease(ctx context.Context) (release func(), ok bool) {
	if s.leasePath == "" {
		return func() {}, true
	}
	f, err := os.OpenFile(s.leasePath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		// Refusing to supervise without the single-instance guarantee is the fail-closed choice; the error is loud and the operator fixes the data.
		s.log.Error("manager lease file unavailable; managers will NOT run", "path", s.leasePath, "err", err)
		if s.events != nil {
			s.events.Record("manager.lease_error",
				"manager lease file unavailable; managers will NOT run: "+err.Error(), nil)
		}
		<-ctx.Done()
		return nil, false
	}

	announced := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, true
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			s.log.Error("manager lease flock failed; managers will NOT run", "path", s.leasePath, "err", err)
			if s.events != nil {
				s.events.Record("manager.lease_error",
					"manager lease flock failed; managers will NOT run: "+err.Error(), nil)
			}
			_ = f.Close()
			<-ctx.Done()
			return nil, false
		}
		if !announced {
			announced = true
			s.log.Info("manager lease held elsewhere; waiting (another runner process is draining)", "path", s.leasePath)
			if s.events != nil {
				s.events.Record("manager.lease_waiting",
					"manager lease held by another runner process; waiting for handover", nil)
			}
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, false
		case <-time.After(ParkPoll):
		}
	}
}
