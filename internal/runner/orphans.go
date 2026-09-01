package runner

// Reaps hook-run containers orphaned when the server dies with runs in
// flight: docker's --rm only removes a container on clean exit, and
// killing the server does not kill the containers its docker CLI children
// launched.
// see docs/internals/runs-concurrency-and-overrides.md

import (
	"fmt"
	"os/exec"
	"strings"
)

const (
	// RunContainerLabel marks a hook-run container for orphan sweeps.
	// Changing it strands containers started by older binaries.
	RunContainerLabel       = "io.webhook-runner.run"
	runContainerLabelValue  = "1"
	runContainerLabelFilter = "label=" + RunContainerLabel + "=" + runContainerLabelValue
)

// SweepOrphanContainers force-removes hook-run containers left behind by a
// previous server process. Call it ONCE, at serve startup, after the run
// store is open and before any run starts. Best-effort: a warning on an
// unreachable daemon, then the server boots anyway.
func (r *Runner) SweepOrphanContainers() {
	// -a catches created-but-never-started strays too; rm -f handles either state.
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
