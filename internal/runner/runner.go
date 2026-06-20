// Package runner spawns disposable Docker containers for hook executions.
//
// Each Run gets:
//   - a temp file holding the raw request body (HOOK_PAYLOAD_FILE)
//   - a temp file holding request headers as JSON (HOOK_HEADERS_FILE)
//
// Both are bind-mounted into the container read-only and the env
// variables point at the mount paths — per-run data, never code. A hook's
// code is immutable per run: either it's part of a stock image, or (for
// hooks shipping a Dockerfile) it's baked into an image built from the
// hook directory and tagged by content hash (see EnsureImage). hook.json
// env values may reference secrets/host environment variables as ${NAME}
// (expanded at run time; see hooks.ExpandEnvRefs). Container output is
// streamed to the server logger and to the Run's bounded ring buffer.
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

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// In-container mount paths. These are part of the documented contract:
// hook commands reference them directly (argv isn't shell-expanded).
const (
	mountedPayload = "/var/run/webhook-runner/payload"
	mountedHeaders = "/var/run/webhook-runner/headers.json"
)

// HookFinishedFunc is invoked once the container exits (or fails to
// start). Implementations typically push GitHub commit-status updates.
type HookFinishedFunc func(hook *hooks.Hook, run *runs.Run, payload []byte)

// HookStartedFunc is invoked just before the container is launched.
// Implementations typically push the GitHub "pending" commit status.
type HookStartedFunc func(hook *hooks.Hook, run *runs.Run, payload []byte)

// KVInjector mints the per-hook bearer token injected into containers that
// opt into the state store. It is a one-method seam (satisfied by *kv.Store)
// so the runner needn't import the kv package's whole surface.
type KVInjector interface {
	Token(namespace string) string
}

// Runner launches docker containers and tracks the resulting runs.
type Runner struct {
	tracker  *runs.Tracker
	log      *slog.Logger
	tmpDir   string
	onStart  HookStartedFunc
	onFinish HookFinishedFunc
	secrets  *hooks.SecretsLoader
	events   *events.Recorder
	groups   *concurrency.Manager

	// kv and kvAdvertise inject the state-store env into containers whose
	// hook sets state: true. kv == nil (or an empty advertise URL) disables
	// injection entirely.
	kv          KVInjector
	kvAdvertise string

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
	Docker   string               // docker binary path; "" = "docker"
	Secrets  *hooks.SecretsLoader // per-hook sops secrets; nil disables decryption
	Events   *events.Recorder     // activity feed for the dashboard; nil drops events
	Groups   *concurrency.Manager // named concurrency groups; nil = no group is declared

	// KV mints per-hook state tokens; KVAdvertise is the base URL containers
	// use to reach the state API. Both empty/nil disables KV injection.
	KV          KVInjector
	KVAdvertise string
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
		tracker:     opts.Tracker,
		log:         opts.Logger,
		tmpDir:      opts.TmpDir,
		onStart:     opts.OnStart,
		onFinish:    opts.OnFinish,
		secrets:     opts.Secrets,
		events:      opts.Events,
		groups:      opts.Groups,
		kv:          opts.KV,
		kvAdvertise: opts.KVAdvertise,
		dockerBin:   opts.Docker,
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

	// A cancel that arrives while the run is still pending skips the
	// container entirely.
	select {
	case <-run.Cancelled():
		run.Finish(runs.StatusCancelled, -1, "cancelled before start")
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	default:
	}

	// Decrypt the hook's repo-stored secrets (if any) before building the
	// container env. A hook that ships a secrets file expects them, so a
	// decrypt failure fails the run loudly instead of starting without them.
	var secrets map[string]string
	if r.secrets != nil {
		var err error
		secrets, err = r.secrets.Load(hook)
		if err != nil {
			run.Finish(runs.StatusError, -1, fmt.Sprintf("hook secrets: %v", err))
			if r.onFinish != nil {
				r.onFinish(hook, run, payload)
			}
			return
		}
	}
	lookup := hooks.SecretsFirstLookup(secrets)

	// Every hook runs an image built from its directory, tagged by content
	// hash — code is baked in, so a concurrent hooks-repo pull can't
	// change what an in-flight run executes. The build is a cheap no-op
	// when the image for the current content already exists.
	buildLog := &slogLineWriter{logFn: func(line string) {
		r.log.Info("hook image build", "hook", hook.ID, "run", run.ID(), "line", line)
	}}
	buildStart := time.Now()
	image, built, err := EnsureImage(r.dockerBin, hook, buildLog)
	if err != nil {
		r.events.Record("image.build_failed", fmt.Sprintf("image build for %s failed: %v", hook.ID, err),
			map[string]string{"hook": hook.ID, "run": run.ID()})
		run.Finish(runs.StatusError, -1, fmt.Sprintf("hook image: %v", err))
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	}
	if built {
		r.events.Record("image.built", fmt.Sprintf("built %s in %s", image, time.Since(buildStart).Round(time.Millisecond)),
			map[string]string{"hook": hook.ID, "run": run.ID(), "tag": image})
	}
	// A build can take a while; honor a cancel that arrived during it
	// instead of starting a container nobody wants anymore.
	select {
	case <-run.Cancelled():
		run.Finish(runs.StatusCancelled, -1, "cancelled before start")
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	default:
	}

	// Queue: reserve a slot in the hook's concurrency group before doing
	// any actual processing. When a hook (or several hooks sharing a group)
	// is flooded, the excess runs wait here — staying "pending", not
	// "running" — instead of all launching containers at once. The timeout
	// is deliberately NOT started yet: a run must not burn its budget while
	// sitting in the queue.
	release, acquired, qErr := r.acquireSlot(hook, run)
	if qErr != nil {
		// An undeclared group is a misconfiguration; fail closed rather
		// than silently running unbounded.
		r.events.Record("run.misconfigured", fmt.Sprintf("%s run %s: %v", hook.ID, run.ID(), qErr),
			map[string]string{"hook": hook.ID, "run": run.ID(), "group": hook.ConcurrencyGroup})
		run.Finish(runs.StatusError, -1, qErr.Error())
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	}
	if !acquired {
		// Cancelled while waiting in the queue.
		run.Finish(runs.StatusCancelled, -1, "cancelled before start")
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	}
	defer release()

	// The timeout clock starts now — we hold a slot and are about to launch
	// — so it bounds only real container processing, never the time spent
	// decrypting secrets, building the image, or queued behind other runs.
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

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
	// State store: only opted-in hooks get a token + the host-gateway
	// mapping, so non-stateful hooks gain no new host exposure. Injected
	// among the reserved env entries (before secrets/hook env) so these keys
	// can't be shadowed — ReservedEnvKey already covers them, but docker's
	// last--e-wins makes ordering matter too.
	if hook.State && r.kv != nil && r.kvAdvertise != "" {
		args = append(args,
			"--add-host=host.docker.internal:host-gateway",
			"-e", "HOOK_KV_URL="+r.kvAdvertise,
			"-e", "HOOK_KV_TOKEN="+r.kv.Token(hook.ID),
		)
	}
	for _, n := range hook.Networks {
		args = append(args, "--network", n)
	}
	for _, v := range hook.Volumes {
		args = append(args, "-v", v)
	}
	// Decrypted secrets are injected first, so an explicit hook.json env
	// entry wins on conflict (docker keeps the last -e for a key).
	for k, v := range secrets {
		if hooks.ReservedEnvKey(k) {
			r.log.Warn("hook secret shadows a reserved env key; skipped",
				"hook", hook.ID, "run", run.ID(), "env", k)
			continue
		}
		args = append(args, "-e", k+"="+v)
	}
	// hook.json env values resolve ${NAME} from the hook's secrets first,
	// then the host environment.
	for k, v := range hook.Env {
		expanded, missing := hooks.ExpandEnvRefs(v, lookup)
		for _, name := range missing {
			r.log.Warn("hook env references unset variable",
				"hook", hook.ID, "run", run.ID(), "env", k, "var", name)
			// Also surface it on the dashboard: a hook silently running with
			// an empty secret (e.g. an AI key that never resolved) looks
			// healthy from the outside while every run fails downstream.
			r.events.Record("env.unresolved",
				hook.ID+": env "+k+" references unset ${"+name+"}; the container gets an empty value",
				map[string]string{"hook": hook.ID, "run": run.ID()})
		}
		args = append(args, "-e", k+"="+expanded)
	}
	if hook.User != "" {
		args = append(args, "--user", hook.User)
	}
	if hook.Workdir != "" {
		args = append(args, "--workdir", hook.Workdir)
	}
	args = append(args, hook.ExtraDockerArgs...)
	args = append(args, image)
	// With no command override, the image's CMD/ENTRYPOINT runs.
	args = append(args, hook.Command...)

	r.log.Info("hook starting",
		"hook", hook.ID, "run", run.ID(), "image", image, "timeout", timeout)
	r.events.Record("run.started", fmt.Sprintf("%s run %s started (%s)", hook.ID, run.ID(), image),
		map[string]string{"hook": hook.ID, "run": run.ID(), "tag": image})

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

	// Watch for timeout (ctx) and explicit cancel requests in parallel
	// with cmd.Wait. Either one kills the container by name. A cancel
	// requested before this goroutine started selects immediately (the
	// channel is already closed), so the pre-start race is covered.
	timedOut := make(chan struct{})
	cancelled := make(chan struct{})
	stopWatcher := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				close(timedOut)
			}
		case <-run.Cancelled():
			close(cancelled)
		case <-stopWatcher:
			return
		}
		r.killContainer(containerName)
		killTimer := time.AfterFunc(2*time.Second, func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
		defer killTimer.Stop()
		<-stopWatcher
	}()

	waitErr := cmd.Wait()
	close(stopWatcher)

	// In the normal case the process has exited and its pipe ends
	// are closed, so streamWG.Wait returns immediately. On a kill
	// (timeout or cancel), orphaned child processes (e.g. the real
	// docker container's descendants) can keep the write end open;
	// force-close the read ends so the scanner goroutines unblock.
	select {
	case <-timedOut:
		stdoutR.Close()
		stderrR.Close()
	default:
		select {
		case <-cancelled:
			stdoutR.Close()
			stderrR.Close()
		default:
		}
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
	// Checked after timeout so an explicit cancel takes precedence when
	// both raced to kill the container.
	select {
	case <-cancelled:
		status = runs.StatusCancelled
		if exitCode == 0 {
			exitCode = -1
		}
		errMsg = "cancelled"
	default:
	}
	run.Finish(status, exitCode, errMsg)
	r.log.Info("hook finished",
		"hook", hook.ID, "run", run.ID(), "status", status, "exit", exitCode)
	r.events.Record("run.finished", fmt.Sprintf("%s run %s finished: %s (exit %d)", hook.ID, run.ID(), status, exitCode),
		map[string]string{"hook": hook.ID, "run": run.ID(), "status": string(status)})
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

// acquireSlot reserves a concurrency-group slot for the run, recording a
// one-time "queued" activity event the moment the run actually has to wait
// (not when it gets a slot immediately). A hook with no concurrency_group
// returns instantly with a no-op release. The returned release must be
// called exactly once when the run finishes.
func (r *Runner) acquireSlot(hook *hooks.Hook, run *runs.Run) (release func(), acquired bool, err error) {
	return r.groups.Acquire(hook.ConcurrencyGroup, run.Cancelled(), func() {
		r.log.Info("hook run queued",
			"hook", hook.ID, "run", run.ID(), "group", hook.ConcurrencyGroup)
		r.events.Record("run.queued",
			fmt.Sprintf("%s run %s queued on concurrency group %q", hook.ID, run.ID(), hook.ConcurrencyGroup),
			map[string]string{"hook": hook.ID, "run": run.ID(), "group": hook.ConcurrencyGroup})
	})
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
