package runner

// Container output streaming and the kill path, split from runner.go for the
// 750-line cap.

import (
	"bufio"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

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
