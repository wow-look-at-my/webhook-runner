package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// runContext returns the context bounding container processing: the parent with the hook's absolute timeout applied, or — when the hook sets no timeout (0) — a plain cancellable child with NO deadline, so an.
func runContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}

// Skip records a first-class "no work was done" run for a delivery matched by one of the hook's skip_if conditions. It is the whole pipeline for a skip: a real, tracked run that goes terminal immediately with status "skipped" — its output names the matched condition, its Finish drives the tracker's OnFinish seam exactly like any other run (so the skip persists to run history), and a run.skipped event lands on the activity feed.
func (r *Runner) Skip(hook *hooks.Hook, reason, title string) *runs.Run {
	run := r.tracker.New(hook.ID)
	run.SetTitle(title)
	run.AppendOutput("skipped: " + reason)
	run.Finish(runs.StatusSkipped, 0, "")
	r.log.Info("hook skipped", "hook", hook.ID, "run", run.ID(), "reason", reason)
	r.events.Record("run.skipped", fmt.Sprintf("%s run %s skipped: %s", hook.ID, runRef(run), reason),
		map[string]string{"hook": hook.ID, "run": run.ID(), "status": string(runs.StatusSkipped)})
	return run
}

// runRef names a run for the activity feed: the id, plus the friendly title when one is set — `abc… (wow-look-at-my/go-toolchain#47)` — so the.
func runRef(run *runs.Run) string {
	if t := run.Title(); t != "" {
		return run.ID() + " (" + t + ")"
	}
	return run.ID()
}

func (r *Runner) streamPipe(wg *sync.WaitGroup, rc io.Reader, hookID string, run *runs.Run, stream string) {
	defer wg.Done()
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		run.AppendOutput(line)
		r.log.Info("hook output",
			"hook", hookID, "run", run.ID(), "stream", stream, "line", line)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		r.log.Warn("output scanner error",
			"hook", hookID, "run", run.ID(), "stream", stream, "err", err)
	}
}

func (r *Runner) killContainer(name string) {
	cmd := exec.Command(r.dockerBin, "kill", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		// docker kill exits non-zero if the container is already
		// gone; that's not interesting, so log at debug level.
		r.log.Debug("docker kill",
			"name", name, "err", err, "out", strings.TrimSpace(string(out)))
	}
}
