// Package runner spawns disposable Docker containers for hook executions.
//
// Each Run gets:
// - a temp file holding the raw request body (HOOK_PAYLOAD_FILE)
// - a temp file holding request headers as JSON (HOOK_HEADERS_FILE)
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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
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
	// The hook's own configuration (hook.json `settings`), validated at load against its settings.schema.json.
	mountedSettings = "/var/run/webhook-runner/settings.json"
	// mountedStateSocket is where a state hook's container sees the KV API's Unix socket (bind-mounted from the host-shared tmp dir).
	mountedStateSocket = "/run/webhook-runner/state.sock"
	// mountedShim is where the container sees webhook-runner's own binary, bind-mounted in and set as the entrypoint of a state hook: it proxies.
	mountedShim = "/run/webhook-runner/whr-shim"
)

// HookFinishedFunc is invoked the container exits (or fails to start). Implementations typically push GitHub commit-status updates.
type HookFinishedFunc func(hook *hooks.Hook, run *runs.Run, payload []byte)

// HookStartedFunc is invoked just before the container is launched. Implementations typically push the GitHub "pending" commit status.
type HookStartedFunc func(hook *hooks.Hook, run *runs.Run, payload []byte)

// KVInjector mints the per-run bearer token injected into containers that opt into the state store — bound to both the hook's namespace and.
type KVInjector interface {
	Token(namespace, runID string) string
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

	// globalCap is the server-wide ceiling on simultaneously RUNNING hook containers (all hooks together) — the Docker-bridge IPv guard. nil.
	globalCap *concurrency.Global

	// kv, kvSocket, and kvShim inject state-store access into containers whose hook sets state: true. kv == nil (or an empty socket/shim path).
	kv       KVInjector
	kvSocket string
	kvShim   string

	// dockerBin is the docker executable, configurable for testing.
	dockerBin string

	wg sync.WaitGroup

	// draining is set shutdown begins: no NEW runs may start (a run launched by a dying process races the state-socket handover and the.
	draining atomic.Bool
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

	// GlobalCap bounds how many hook executions run containers at , across ALL hooks (excess runs queue as pending). nil = no cap.
	GlobalCap *concurrency.Global

	// KV mints per-hook state tokens; KVSocket is the host path of the KV API's Unix socket and KVShim is the host path of webhook-runner's own.
	KV       KVInjector
	KVSocket string
	KVShim   string
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
		secrets:   opts.Secrets,
		events:    opts.Events,
		groups:    opts.Groups,
		globalCap: opts.GlobalCap,
		kv:        opts.KV,
		kvSocket:  opts.KVSocket,
		kvShim:    opts.KVShim,
		dockerBin: opts.Docker,
	}
}

// Wait blocks until all in-flight runs have finished. Useful for tests and graceful shutdown.
func (r *Runner) Wait() { r.wg.Wait() }

// Start launches a hook in the background. The returned Run is already registered with the tracker and will be updated as the container runs.
func (r *Runner) Start(parent context.Context, hook *hooks.Hook, payload []byte, headers http.Header, title string) (*runs.Run, error) {
	return r.start(parent, hook, payload, headers, title, "", "")
}

// start is the shared dispatch behind Start and StartSpawned (spawn.go);
// empty parent IDs mean an ordinary, unattributed run.
func (r *Runner) start(parent context.Context, hook *hooks.Hook, payload []byte, headers http.Header, title, parentHookID, parentRunID string) (*runs.Run, error) {
	run := r.tracker.New(hook.ID)
	run.SetTitle(title)
	// Parent attribution stamps BEFORE any path that can Finish the run (drain refusal below included), so every terminal snapshot — and the.
	run.SetSpawnedBy(parentHookID, parentRunID)

	// Drain gate: a run launched by a dying process races the state-socket handover and the shutdown teardown — refuse loudly instead.
	if r.draining.Load() {
		run.Finish(runs.StatusError, -1, ErrDraining.Error())
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return run, ErrDraining
	}

	payloadPath, headersPath, settingsPath, cleanup, err := r.writeTempFiles(run.ID(), payload, headers, hook.SettingsJSON())
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
		r.execute(parent, hook, run, payload, payloadPath, headersPath, settingsPath)
	}()
	return run, nil
}

func (r *Runner) execute(parent context.Context, hook *hooks.Hook, run *runs.Run, payload []byte, payloadPath, headersPath, settingsPath string) {
	timeout := hook.Timeout()         // = no absolute ceiling
	idleTimeout := hook.IdleTimeout() // = no idle limit

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

	// Decrypt the hook's repo-stored secrets (if any) before building the container env.
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
	if err := resolveSettingsFile(hook, secrets, settingsPath); err != nil {
		run.Finish(runs.StatusError, -1, err.Error())
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	}

	// Every hook runs an image built from its directory, tagged by content hash — code is baked in, so a concurrent hooks-repo pull can't change what.
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
	run.Mark(runs.PhaseImageReady)
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

	// Queue: reserve a slot in the hook's concurrency group before doing any actual processing.
	release, acquired, clearQueued, qErr := r.acquireSlot(hook, run)
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
		// Cancelled while waiting in the queue. (Finish clears waiting_on itself, so no explicit clearQueued is needed on this path.)
		run.Finish(runs.StatusCancelled, -1, "cancelled before start")
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	}
	clearQueued()
	defer release()

	// The GLOBAL run cap: after the group slot (group-then-global ordering everywhere — no lock-order cycles, and the global slots can never.
	gRelease, gAcquired, gClearQueued := r.acquireGlobalSlot(hook, run)
	if !gAcquired {
		run.Finish(runs.StatusCancelled, -1, "cancelled before start")
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	}
	gClearQueued()
	defer gRelease()
	// Both slots held: from here to PhaseSpawned is pure launch preparation (argv assembly, and for state hooks the imageCommand inspect), with.
	run.Mark(runs.PhaseSlotAcquired)

	// The timeout clock starts now — we hold a slot and are about to launch — so it bounds only real container processing, never the time spent.
	ctx, cancel := runContext(parent, timeout)
	defer cancel()

	containerName := "webhook-runner-" + run.ID()

	spec := containerSpec{
		name:  containerName,
		image: image,
		// The orphan-sweep marker (see orphans.go): lets the next serve boot find and reap containers whose owning server process died before their.
		label: RunContainerLabel + "=" + runContainerLabelValue,
		mounts: []string{
			payloadPath + ":" + mountedPayload + ":ro",
			headersPath + ":" + mountedHeaders + ":ro",
			settingsPath + ":" + mountedSettings + ":ro",
		},
		env: []string{
			"HOOK_PAYLOAD_FILE=" + mountedPayload,
			"HOOK_HEADERS_FILE=" + mountedHeaders,
			"HOOK_SETTINGS_FILE=" + mountedSettings,
			"HOOK_ID=" + hook.ID,
			"HOOK_RUN_ID=" + run.ID(),
		},
		secrets: secrets,
		onReservedSecret: func(key string) {
			r.log.Warn("hook secret shadows a reserved env key; skipped",
				"hook", hook.ID, "run", run.ID(), "env", key)
		},
		networks: hook.Networks,
		user:     hook.User,
		workdir:  hook.Workdir,
		dind:     hook.Dind,
		devices:  hook.Devices,
	}
	spec.mounts = append(spec.mounts, hook.Volumes...)
	// State store: opted-in hooks reach the KV API at a plain http://localhost: URL.
	stateForwarding := hook.State && r.kv != nil && r.kvSocket != "" && r.kvShim != ""
	if stateForwarding {
		spec.entrypoint = mountedShim
		spec.mounts = append(spec.mounts,
			r.kvShim+":"+mountedShim+":ro",
			r.kvSocket+":"+mountedStateSocket,
		)
		spec.env = append(spec.env,
			"HOOK_KV_SOCKET="+mountedStateSocket,
			"HOOK_KV_URL=http://localhost:9002",
			"HOOK_KV_TOKEN="+r.kv.Token(hook.ID, run.ID()),
		)
	}
	// seccomp.userns: a profile file the DAEMON reads while starting the container, so it must outlive `docker run`'s startup -- the cleanup is.
	seccompFlags, seccompCleanup, err := seccompArgs(hook, r.tmpDir, run.ID())
	if err != nil {
		r.events.Record("run.seccomp_failed", fmt.Sprintf("seccomp profile for %s failed: %v", hook.ID, err),
			map[string]string{"hook": hook.ID, "run": run.ID()})
		run.Finish(runs.StatusError, -1, fmt.Sprintf("seccomp profile: %v", err))
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	}
	defer seccompCleanup()
	spec.seccomp = seccompFlags
	if stateForwarding {
		// The shim is the entrypoint; hand it the command the image would have run (its ENTRYPOINT+CMD, or hook.Command when set) to exec after.
		childArgv, err := imageCommand(r.dockerBin, image, hook.Command)
		if err != nil {
			r.events.Record("image.inspect_failed", fmt.Sprintf("inspect %s for %s failed: %v", image, hook.ID, err),
				map[string]string{"hook": hook.ID, "run": run.ID()})
			run.Finish(runs.StatusError, -1, fmt.Sprintf("inspect image command: %v", err))
			if r.onFinish != nil {
				r.onFinish(hook, run, payload)
			}
			return
		}
		run.Mark(runs.PhaseInspected)
		spec.argv = append([]string{"kv-forward"}, childArgv...)
	} else {
		// With no command override, the image's CMD/ENTRYPOINT runs.
		spec.argv = hook.Command
	}
	args := spec.args()

	r.log.Info("hook starting",
		"hook", hook.ID, "run", run.ID(), "image", image, "timeout", timeout)
	r.events.Record("run.started", fmt.Sprintf("%s run %s started (%s)%s", hook.ID, runRef(run), image, spawnNote(run)),
		map[string]string{"hook": hook.ID, "run": run.ID(), "tag": image})

	if r.onStart != nil {
		r.onStart(hook, run, payload)
	}

	// We use exec.Command (not exec.CommandContext) so that on context cancellation we can issue an explicit "docker kill <name>" — that.
	cmd := exec.Command(r.dockerBin, args...)

	// Use os.Pipe instead of cmd.StdoutPipe/StderrPipe.
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
	// The handoff instant: everything after this and before the container's own instruction (PhaseContainerEntry, reported by the.
	run.Mark(runs.PhaseSpawned)
	run.SetRunning()

	timedOut := make(chan struct{})
	deadlineExceeded := make(chan struct{})
	cancelled := make(chan struct{})
	stopWatcher := make(chan struct{})

	// The idle watchdog arms NOW — only after the concurrency slot was acquired and the container actually launched, so a queued run never ticks —.
	wd := newIdleWatchdog(idleTimeout, time.Now)
	wd.Arm()
	// Declared waits (the state API's POST /wait) count as activity: hand the run a handle to this watchdog so an in-flight wait keeps touching it —.
	run.SetActivityTouch(wd.Touch)
	silent := wd.Watch(stopWatcher)
	stdout := &touchReader{r: stdoutR, touch: wd.Touch}
	stderr := &touchReader{r: stderrR, touch: wd.Touch}

	var streamWG sync.WaitGroup
	streamWG.Add(2)
	go r.streamPipe(&streamWG, stdout, hook.ID, run, "stdout")
	go r.streamPipe(&streamWG, stderr, hook.ID, run, "stderr")

	// Watch for the no-output idle timeout, the absolute `timeout`
	// deadline (or the parent context tearing down), and explicit cancel
	// requests in parallel with cmd.Wait. Any kills the container by
	// name. A cancel requested before this goroutine started selects
	// immediately (the channel is already closed), so the pre-start race
	// is covered.
	go func() {
		select {
		case <-silent:
			// Already produced no output for the full timeout: there is nothing to wait out. Hard kill, same as always.
			close(timedOut)
			r.killContainer(containerName)
		case <-ctx.Done():
			// ctx wraps parent with the hook's absolute timeout, if any: DeadlineExceeded means the ceiling fired; anything else (Canceled) means the.
			if ctx.Err() == context.DeadlineExceeded {
				close(deadlineExceeded)
			}
			r.killContainer(containerName)
		case <-run.Cancelled():
			close(cancelled)
			// Graceful: SIGTERM, grace, then docker's own SIGKILL — the same stopContainer a manager instance gets on a supervisor-requested stop.
			r.stopContainer(containerName, runCancelGraceSeconds)
		case <-stopWatcher:
			return
		}
		killTimer := time.AfterFunc(2*time.Second, func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
		defer killTimer.Stop()
		<-stopWatcher
	}()

	waitErr := cmd.Wait()
	// The docker CLI has returned: the container exited AND `--rm` teardown is done.
	run.Mark(runs.PhaseExited)
	close(stopWatcher)

	// In the normal case the process has exited and its pipe ends are closed, so streamWG.Wait returns immediately.
	killedByWatcher := false
	for _, ch := range []chan struct{}{timedOut, cancelled} {
		select {
		case <-ch:
			killedByWatcher = true
		default:
		}
	}
	if killedByWatcher {
		stdoutR.Close()
		stderrR.Close()
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
		// Distinguishable from the absolute-ceiling message below: the run died for going silent, not for running long.
		errMsg = fmt.Sprintf("idle timeout after %s (no output)", idleTimeout)
	default:
	}
	select {
	case <-deadlineExceeded:
		status = runs.StatusTimeout
		if exitCode == 0 {
			exitCode = -1
		}
		errMsg = fmt.Sprintf("timed out after %s", timeout)
	default:
	}
	// Checked after the timeouts so an explicit cancel takes precedence when
	// both raced to kill the container.
	select {
	case <-cancelled:
		status = runs.StatusCancelled
		if exitCode == 0 {
			exitCode = -1
		}
		errMsg = "cancelled"
		// A cancel can carry an explanation — e.g. a lock steal naming its
		// displacer — which belongs in run history, not just the moment.
		if reason := run.CancelReason(); reason != "" {
			errMsg = reason
		}
	default:
	}
	// Registered rather than run after Finish returns: Finish invokes this
	// BEFORE closing the done channel, so the terminal feed line is already
	// there for anything that observes <-run.Done(). Doing it afterwards is
	// what forced callers to add a Runner.Wait() barrier. See
	// runs.Run.SetOnTerminal.
	run.SetOnTerminal(func(runs.RunState) {
		r.log.Info("hook finished",
			"hook", hook.ID, "run", run.ID(), "status", status, "exit", exitCode)
		// runRef, not run.ID(): a title set mid-run via the state API's /title lands here too, so the feed's terminal line names the subject.
		finishedMsg := fmt.Sprintf("%s run %s finished: %s (exit %d)%s", hook.ID, runRef(run), status, exitCode, spawnNote(run))
		if errMsg != "" && (status == runs.StatusTimeout ||
			(status == runs.StatusCancelled && errMsg != "cancelled")) {
			// Carry the reason (a timeout's "no output" verdict, a cancel's steal explanation) so the activity feed shows what killed the run.
			finishedMsg += ": " + errMsg
		}
		r.events.Record("run.finished", finishedMsg,
			map[string]string{"hook": hook.ID, "run": run.ID(), "status": string(status)})
	})
	run.Finish(status, exitCode, errMsg)
	// The GitHub commit-status POST stays OUTSIDE the terminal seam: it is a network call, and blocking every Done() observer on it would trade.
	if r.onFinish != nil {
		r.onFinish(hook, run, payload)
	}
}

// runCancelGraceSeconds is how long an explicitly-cancelled run's container gets between SIGTERM and docker's own SIGKILL (see.
const runCancelGraceSeconds = 10
