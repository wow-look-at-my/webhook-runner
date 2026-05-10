// Package runner spawns disposable Docker containers for hook executions.
//
// Each Run gets:
//   - a temp file holding the raw request body (HOOK_PAYLOAD_FILE)
//   - a temp file holding request headers as JSON (HOOK_HEADERS_FILE)
//
// Both files are bind-mounted into the container read-only and the env
// variables point at the mount paths. Container output is streamed to
// the server logger and to the Run's bounded ring buffer.
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// HookFinishedFunc is invoked once the container exits (or fails to
// start). Implementations typically push GitHub commit-status updates.
type HookFinishedFunc func(hook *hooks.Hook, run *runs.Run, payload []byte)

// HookStartedFunc is invoked just before the container is launched.
// Implementations typically push the GitHub "pending" commit status.
type HookStartedFunc func(hook *hooks.Hook, run *runs.Run, payload []byte)

// Runner launches docker containers and tracks the resulting runs.
type Runner struct {
	tracker  *runs.Tracker
	log      *slog.Logger
	tmpDir   string
	onStart  HookStartedFunc
	onFinish HookFinishedFunc

	// dockerBin is the docker executable, configurable for testing.
	dockerBin string

	wg sync.WaitGroup
}

// Options configure a Runner.
type Options struct {
	Tracker  *runs.Tracker
	Logger   *slog.Logger
	TmpDir   string // directory for payload/header temp files; "" = os.TempDir()
	OnStart  HookStartedFunc
	OnFinish HookFinishedFunc
	Docker   string // docker binary path; "" = "docker"
}

// New constructs a Runner.
func New(opts Options) *Runner {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Docker == "" {
		opts.Docker = "docker"
	}
	if opts.TmpDir == "" {
		opts.TmpDir = os.TempDir()
	}
	return &Runner{
		tracker:   opts.Tracker,
		log:       opts.Logger,
		tmpDir:    opts.TmpDir,
		onStart:   opts.OnStart,
		onFinish:  opts.OnFinish,
		dockerBin: opts.Docker,
	}
}

// Wait blocks until all in-flight runs have finished. Useful for tests
// and graceful shutdown.
func (r *Runner) Wait() { r.wg.Wait() }

// Start launches a hook in the background. The returned Run is already
// registered with the tracker and will be updated as the container runs.
//
// Caller-supplied payload and headers are written to disk before the
// container starts; the temp files are removed when the run finishes.
func (r *Runner) Start(parent context.Context, hook *hooks.Hook, payload []byte, headers http.Header) (*runs.Run, error) {
	run := r.tracker.New(hook.ID)

	payloadPath, headersPath, cleanup, err := r.writeTempFiles(run.ID(), payload, headers)
	if err != nil {
		run.Finish(runs.StatusError, -1, fmt.Sprintf("write temp files: %v", err))
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return run, err
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer cleanup()
		r.execute(parent, hook, run, payload, payloadPath, headersPath)
	}()
	return run, nil
}

func (r *Runner) execute(parent context.Context, hook *hooks.Hook, run *runs.Run, payload []byte, payloadPath, headersPath string) {
	timeout := hook.Timeout()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	const (
		mountedPayload = "/var/run/webhook-runner/payload"
		mountedHeaders = "/var/run/webhook-runner/headers.json"
	)
	containerName := "webhook-runner-" + run.ID()

	args := []string{
		"run", "--rm",
		"--name", containerName,
		"-v", payloadPath + ":" + mountedPayload + ":ro",
		"-v", headersPath + ":" + mountedHeaders + ":ro",
		"-e", "HOOK_PAYLOAD_FILE=" + mountedPayload,
		"-e", "HOOK_HEADERS_FILE=" + mountedHeaders,
		"-e", "HOOK_ID=" + hook.ID,
		"-e", "HOOK_RUN_ID=" + run.ID(),
	}
	for _, n := range hook.Networks {
		args = append(args, "--network", n)
	}
	for _, v := range hook.Volumes {
		args = append(args, "-v", v)
	}
	for k, v := range hook.Env {
		args = append(args, "-e", k+"="+v)
	}
	if hook.User != "" {
		args = append(args, "--user", hook.User)
	}
	if hook.Workdir != "" {
		args = append(args, "--workdir", hook.Workdir)
	}
	args = append(args, hook.ExtraDockerArgs...)
	args = append(args, hook.Image)
	args = append(args, hook.Command...)

	r.log.Info("hook starting",
		"hook", hook.ID, "run", run.ID(), "image", hook.Image, "timeout", timeout)

	if r.onStart != nil {
		r.onStart(hook, run, payload)
	}

	// We use exec.Command (not exec.CommandContext) so that on context
	// cancellation we can issue an explicit "docker kill <name>" — that
	// reliably stops the container even when the docker CLI is the one
	// being killed by the kernel. exec.CommandContext would SIGKILL the
	// docker CLI process, which races against the actual container.
	cmd := exec.Command(r.dockerBin, args...)

	// Use os.Pipe instead of cmd.StdoutPipe/StderrPipe. The cmd
	// variants add the read end to closeAfterWait, meaning cmd.Wait
	// closes the pipe before we finish reading — dropping output in
	// a race. With our own pipes, Wait does not touch them.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		run.Finish(runs.StatusError, -1, fmt.Sprintf("stdout pipe: %v", err))
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		run.Finish(runs.StatusError, -1, fmt.Sprintf("stderr pipe: %v", err))
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		run.Finish(runs.StatusError, -1, fmt.Sprintf("start docker: %v", err))
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	}
	// Close write ends in the parent; only the child holds them now.
	stdoutW.Close()
	stderrW.Close()
	run.SetRunning()

	var streamWG sync.WaitGroup
	streamWG.Add(2)
	go r.streamPipe(&streamWG, stdoutR, hook.ID, run, "stdout")
	go r.streamPipe(&streamWG, stderrR, hook.ID, run, "stderr")

	// Watch ctx for cancellation/timeout in parallel with cmd.Wait.
	timedOut := make(chan struct{})
	stopWatcher := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				close(timedOut)
			}
			r.killContainer(containerName)
			killTimer := time.AfterFunc(2*time.Second, func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			})
			defer killTimer.Stop()
			<-stopWatcher
		case <-stopWatcher:
		}
	}()

	waitErr := cmd.Wait()
	close(stopWatcher)

	// In the normal case the process has exited and its pipe ends
	// are closed, so streamWG.Wait returns immediately. On timeout,
	// orphaned child processes (e.g. the real docker container's
	// descendants) can keep the write end open; force-close the
	// read ends so the scanner goroutines unblock.
	select {
	case <-timedOut:
		stdoutR.Close()
		stderrR.Close()
	default:
	}
	streamWG.Wait()

	exitCode := 0
	status := runs.StatusSuccess
	errMsg := ""
	if waitErr != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.As(waitErr, &exitErr):
			exitCode = exitErr.ExitCode()
			status = runs.StatusFailure
		default:
			exitCode = -1
			status = runs.StatusError
			errMsg = waitErr.Error()
		}
	}
	select {
	case <-timedOut:
		status = runs.StatusTimeout
		if exitCode == 0 {
			exitCode = -1
		}
		errMsg = fmt.Sprintf("timed out after %s", timeout)
	default:
	}
	run.Finish(status, exitCode, errMsg)
	r.log.Info("hook finished",
		"hook", hook.ID, "run", run.ID(), "status", status, "exit", exitCode)
	if r.onFinish != nil {
		r.onFinish(hook, run, payload)
	}
}

func (r *Runner) streamPipe(wg *sync.WaitGroup, rc io.ReadCloser, hookID string, run *runs.Run, stream string) {
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

// writeTempFiles materializes the payload and headers in a temp dir
// dedicated to this run. The returned cleanup removes the directory.
func (r *Runner) writeTempFiles(runID string, payload []byte, headers http.Header) (payloadPath, headersPath string, cleanup func(), err error) {
	dir, err := os.MkdirTemp(r.tmpDir, "wh-"+runID+"-")
	if err != nil {
		return "", "", func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	payloadPath = filepath.Join(dir, "payload")
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	headersPath = filepath.Join(dir, "headers.json")
	hb, err := json.MarshalIndent(headers, "", "  ")
	if err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	if err := os.WriteFile(headersPath, hb, 0o600); err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	// Loosen perms so the in-container user can read the files even if
	// the container runs as a non-root user that doesn't share UID with
	// the host process.
	_ = os.Chmod(dir, 0o755)
	_ = os.Chmod(payloadPath, 0o644)
	_ = os.Chmod(headersPath, 0o644)
	return payloadPath, headersPath, cleanup, nil
}
