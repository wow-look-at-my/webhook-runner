// Orphaned hook-container reaping.
//
// Every hook-run container is stamped with a marker label (RunContainerLabel,
// added in execute). The clean lifecycle needs nothing more: `docker run
// --rm` auto-removes the container when it EXITS (daemon-side AutoRemove),
// and the timeout/cancel paths kill by name, so a run that ends — however it
// ends — frees its container and its bridge-network IPv4 address.
//
// The gap is a server process that DIES with runs in flight (SIGKILL when a
// deploy's stop grace expires mid-drain, an OOM kill, a crash): killing the
// server kills the `docker` CLI children, NOT the sibling containers they
// launched. Those keep running on the host daemon with no owner — no idle
// watchdog (it died with the server), no cancel path, no state socket (the
// next server replaces it) — and --rm only fires when they exit on their
// own, which a long-lived or wedged hook may never do. Each orphan holds a
// bridge IP indefinitely; accumulated across restarts they exhaust the
// pool ("no available IPv4 addresses on this network's address pools").
//
// SweepOrphanContainers force-removes every marker-labeled container at
// serve startup. Safety argument for "everything labeled is an orphan"
// at that point:
//   - The runstore's bbolt flock (Open fails fast while another process
//     holds it) gates the whole serve process during rolling-update
//     overlap, so a process that reached the sweep is the ONLY live serve
//     process on this data dir — the previous one either drained fully
//     (its containers exited and were reaped by --rm) or died (its
//     containers are exactly the orphans this sweep exists to reap).
//   - One serve process per host/daemon is already a hard assumption (the
//     fixed state-socket path and admin/hook ports), so no OTHER live
//     server's containers can carry the label.
//   - The sweep runs before this process starts any run of its own, and
//     manager instances are not swept: they use deterministic names with
//     their own rm -f-on-acquire orphan handling (managersession.go), and
//     `webhook-runner test` containers are never labeled.
//
// A run in flight during a restart "never completed and exists nowhere
// afterwards" (the tracker/runstore doctrine) — the sweep extends that to
// the container itself instead of leaving an unowned zombie holding an IP.
package runner

import (
	"fmt"
	"os/exec"
	"strings"
)

const (
	// RunContainerLabel marks every hook-run container this runner starts (value runContainerLabelValue), so a later serve boot can find and.
	RunContainerLabel       = "io.webhook-runner.run"
	runContainerLabelValue  = "1"
	runContainerLabelFilter = "label=" + RunContainerLabel + "=" + runContainerLabelValue
)

// SweepOrphanContainers force-removes hook-run containers left behind by a
// previous server process. Call it ONCE, at serve startup, after the run
// store is open (the bbolt flock is the no-concurrent-server proof — see
// the package comment) and before any run starts. Best-effort: an
// unreachable docker daemon logs a warning and the server boots anyway
// (the first real run will surface the daemon problem loudly enough).
func (r *Runner) SweepOrphanContainers() {
	// -a catches created-but-never-started strays too (rm -f handles any state); exited ones were already auto-removed by --rm.
	out, err := exec.Command(r.dockerBin, "ps", "-aq", "--filter", runContainerLabelFilter).Output()
	if err != nil {
		r.log.Warn("orphan container sweep: docker ps failed", "err", err)
		return
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return
	}
	removed := 0
	for _, id := range ids {
		if out, err := exec.Command(r.dockerBin, "rm", "-f", id).CombinedOutput(); err != nil {
			r.log.Warn("remove orphaned hook container",
				"container", id, "err", err, "out", strings.TrimSpace(string(out)))
			continue
		}
		removed++
		r.log.Info("removed orphaned hook container", "container", id)
	}
	if removed > 0 {
		r.events.Record("run.orphans_removed",
			fmt.Sprintf("removed %d orphaned hook container(s) left by a previous server process (each held a bridge-network IP)", removed),
			nil)
	}
}
