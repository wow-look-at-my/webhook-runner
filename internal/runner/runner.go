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
	mountedPayload     = "/var/run/webhook-runner/payload"
	mountedHeaders     = "/var/run/webhook-runner/headers.json"
	mountedSourceDir   = "/hook"
	// The hook's own configuration (hook.json `settings`), validated at load
	// against its settings.schema.json. Mounted read-only like the payload:
	// the hook reads its config, and never the manifest that carries it.
	mountedSettings = "/var/run/webhook-runner/settings.json"
	// mountedStateSocket is where a state hook's container sees the KV API's
	// Unix socket (bind-mounted from the host-shared tmp dir).
	mountedStateSocket = "/run/webhook-runner/state.sock"
	// mountedShim is where the container sees webhook-runner's own binary,
	// bind-mounted in and set as the entrypoint of a state hook: it proxies
	// HOOK_KV_URL (http://localhost:9002) to the Unix socket, then execs the
	// hook's real command, so hooks use a plain URL with any HTTP client.
	mountedShim = "/run/webhook-runner/whr-shim"
)

// HookFinishedFunc is invoked once the container exits (or fails to
// start). Implementations typically push GitHub commit-status updates.
type HookFinishedFunc func(hook *hooks.Hook, run *runs.Run, payload []byte)

// HookStartedFunc is invoked just before the container is launched.
// Implementations typically push the GitHub "pending" commit status.
type HookStartedFunc func(hook *hooks.Hook, run *runs.Run, payload []byte)

// KVInjector mints the per-run bearer token injected into containers that
// opt into the state store — bound to both the hook's namespace and THIS
// run's identity, which is what lets the state API attribute cooperative
// locks to their holding run (and the finish seam free them). It is a
// one-method seam (satisfied by *kv.Store) so the runner needn't import the
// kv package's whole surface.
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

	// globalCap is the server-wide ceiling on simultaneously RUNNING hook
	// containers (all hooks together) — the Docker-bridge IPv4 guard. nil
	// applies no cap. Acquired in execute strictly AFTER the hook's
	// concurrency-group slot (group-then-global everywhere: no ordering
	// cycles, and global slots are never consumed by runs still parked on
	// a group queue), released when the run finishes. Manager instances
	// and image builds deliberately sit outside the cap.
	globalCap *concurrency.Global

	// kv, kvSocket, and kvShim inject state-store access into containers whose
	// hook sets state: true. kv == nil (or an empty socket/shim path) disables
	// injection. kvSocket is the host path of the KV API's Unix socket and
	// kvShim is the host path of webhook-runner's own binary; both are
	// bind-mounted in, and the shim (set as the container entrypoint) proxies
	// localhost:9002 to the socket so the hook uses a plain http URL.
	kv       KVInjector
	kvSocket string
	kvShim   string

	// dockerBin is the docker executable, configurable for testing.
	dockerBin string

	wg sync.WaitGroup

	// draining is set once shutdown begins: no NEW runs may start (a run
	// launched by a dying process races the state-socket handover and the
	// docker-kill teardown). In-flight runs are unaffected — Wait drains
	// them. See BeginShutdown.
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

	// GlobalCap bounds how many hook executions run containers at once,
	// across ALL hooks (excess runs queue as pending). nil = no cap. See
	// concurrency.Global; serve always wires one (default 64).
	GlobalCap *concurrency.Global

	// KV mints per-hook state tokens; KVSocket is the host path of the KV
	// API's Unix socket and KVShim is the host path of webhook-runner's own
	// binary (the in-container proxy entrypoint), both bind-mounted into
	// state-hook containers. KV nil or either path empty disables KV injection.
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

// Wait blocks until all in-flight runs have finished. Useful for tests
// and graceful shutdown.
func (r *Runner) Wait() { r.wg.Wait() }

// Start launches a hook in the background. The returned Run is already
// registered with the tracker and will be updated as the container runs.
//
// Caller-supplied payload and headers are written to disk before the
// container starts; the temp files are removed when the run finishes.
//
// title is the run's friendly display title, resolved by the caller from
// the hook's run_title template ("" = untitled; the dashboard falls back
// to the run id). The caller resolves it — not this package — because
// resolution context is the caller's: the HTTP path renders it once before
// skip evaluation, and the scheduler applies its own "schedule" fallback.
func (r *Runner) Start(parent context.Context, hook *hooks.Hook, payload []byte, headers http.Header, title string) (*runs.Run, error) {
	return r.start(parent, hook, payload, headers, title, "", "")
}

// start is the shared dispatch behind Start and StartSpawned (spawn.go);
// empty parent IDs mean an ordinary, unattributed run.
func (r *Runner) start(parent context.Context, hook *hooks.Hook, payload []byte, headers http.Header, title, parentHookID, parentRunID string) (*runs.Run, error) {
	run := r.tracker.New(hook.ID)
	run.SetTitle(title)
	// Parent attribution stamps BEFORE any path that can Finish the run
	// (drain refusal below included), so every terminal snapshot — and the
	// persisted history — carries it. No-op for the empty IDs Start passes.
	run.SetSpawnedBy(parentHookID, parentRunID)

	// Drain gate: a run launched by a dying process races the state-socket
	// handover and the shutdown teardown — refuse loudly instead. The run
	// record exists (status error, the reason in run history); the caller
	// maps ErrDraining to a retryable 503.
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

// Skip records a first-class "no work was done" run for a delivery matched
// by one of the hook's skip_if conditions. It is the whole pipeline for a
// skip: a real, tracked run that goes terminal immediately with status
// "skipped" — its output names the matched condition, its Finish drives the
// tracker's OnFinish seam exactly like any other run (so the skip persists
// to run history), and a run.skipped event lands on the activity feed. What
// it deliberately does NOT do is any work: no temp files, no secrets
// decrypt, no image build, no concurrency-group slot, and above all NO
// container. The onStart/onFinish callbacks (GitHub commit statuses) are
// not invoked either — they report container work, and none happened.
// Cancellation and timeout cannot apply: the run is terminal on return.
//
// title carries the run's friendly display title like Start's — set BEFORE
// Finish, so the terminal snapshot the OnFinish seam persists is titled: a
// skip should still say which PR it was about.
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

// runRef names a run for the activity feed: the id, plus the friendly
// title when one is set — `abc… (wow-look-at-my/go-toolchain#47)` — so the
// feed's run-scoped lines are readable without a lookup. The id stays
// first: it is the stable handle everything else (logs, /runs/{id},
// container names) keys on.
func runRef(run *runs.Run) string {
	if t := run.Title(); t != "" {
		return run.ID() + " (" + t + ")"
	}
	return run.ID()
}

func (r *Runner) execute(parent context.Context, hook *hooks.Hook, run *runs.Run, payload []byte, payloadPath, headersPath, settingsPath string) {
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
	if err := resolveSettingsFile(hook, secrets, settingsPath); err != nil {
		run.Finish(runs.StatusError, -1, err.Error())
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	}

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

	// Queue: reserve a slot in the hook's concurrency group before doing
	// any actual processing. When a hook (or several hooks sharing a group)
	// is flooded, the excess runs wait here — staying "pending", not
	// "running" — instead of all launching containers at once. The timeout
	// watchdog is deliberately NOT armed yet: a run must not burn its
	// budget while sitting in the queue. While queued, the run's waiting_on
	// mirrors its place in the line (kind "group": holders + position) so
	// the dashboard can answer "what is it stuck behind".
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
		// Cancelled while waiting in the queue. (Finish clears waiting_on
		// itself, so no explicit clearQueued is needed on this path.)
		run.Finish(runs.StatusCancelled, -1, "cancelled before start")
		if r.onFinish != nil {
			r.onFinish(hook, run, payload)
		}
		return
	}
	clearQueued()
	defer release()

	// The GLOBAL run cap: after the group slot (group-then-global ordering
	// everywhere — no lock-order cycles, and the global slots can never
	// fill up with runs still parked on tiny group queues), before the
	// container starts. Same queue semantics as a group: the run stays
	// pending, the watchdog stays unarmed, waiting_on mirrors its place in
	// line, and cancellation is honored while queued.
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
	// Both slots held: from here to PhaseSpawned is pure launch preparation
	// (argv assembly, and for state hooks the imageCommand inspect), with no
	// queueing left in it.
	run.Mark(runs.PhaseSlotAcquired)

	containerName := "webhook-runner-" + run.ID()

	args := []string{
		"run", "--rm",
		"--name", containerName,
		// The orphan-sweep marker (see orphans.go): lets the next serve
		// boot find and reap containers whose owning server process died
		// before their run finished.
		"--label", RunContainerLabel + "=" + runContainerLabelValue,
		"-v", payloadPath + ":" + mountedPayload + ":ro",
		"-v", headersPath + ":" + mountedHeaders + ":ro",
		"-v", filepath.Dir(hook.SourcePath) + ":" + mountedSourceDir + ":ro",
		"-v", settingsPath + ":" + mountedSettings + ":ro",
		"-e", "HOOK_PAYLOAD_FILE=" + mountedPayload,
		"-e", "HOOK_HEADERS_FILE=" + mountedHeaders,
		"-e", "HOOK_SOURCE_DIR=" + mountedSourceDir,
		"-e", "HOOK_SETTINGS_FILE=" + mountedSettings,
		"-e", "HOOK_ID=" + hook.ID,
		"-e", "HOOK_RUN_ID=" + run.ID(),
	}
	// State store: opted-in hooks reach the KV API at a plain
	// http://localhost:9002 URL. The runner bind-mounts the KV Unix socket and
	// webhook-runner's own binary, sets the binary as the container entrypoint
	// (the shim) — it proxies that port to the socket, then execs the hook's
	// real command — so there's no networking and any HTTP client works. Only
	// state hooks get the mounts + token. Injected among the reserved env
	// entries (before secrets/hook env) so these keys can't be shadowed —
	// ReservedEnvKey already covers them, but docker's last--e-wins matters too.
	stateForwarding := hook.State && r.kv != nil && r.kvSocket != "" && r.kvShim != ""
	if stateForwarding {
		args = append(args,
			"--entrypoint", mountedShim,
			"-v", r.kvShim+":"+mountedShim+":ro",
			"-v", r.kvSocket+":"+mountedStateSocket,
			"-e", "HOOK_KV_SOCKET="+mountedStateSocket,
			"-e", "HOOK_KV_URL=http://localhost:9002",
			"-e", "HOOK_KV_TOKEN="+r.kv.Token(hook.ID, run.ID()),
		)
	}
	// github-state-mirror routing (unconditional — see GSMBaseURL): the
	// GITHUB_API_URL fleet default, injected BEFORE secrets/hook env so an
	// explicit hook.json value still wins.
	args = append(args, r.gsmArgs()...)
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
	if hook.User != "" {
		args = append(args, "--user", hook.User)
	}
	if hook.Workdir != "" {
		args = append(args, "--workdir", hook.Workdir)
	}
	// Docker-in-Docker: --privileged (host-root-equivalent) grants the
	// container the capabilities to run its own nested dockerd, and the
	// anonymous /var/lib/docker volume gives that inner daemon container-local
	// storage on a real filesystem — its overlay driver can't stack on the
	// outer container's overlay rootfs. --rm above auto-removes the anonymous
	// volume, so inner storage never leaks between runs. The host's daemon is
	// never exposed (no socket mount). Injected before extra_docker_args and
	// the image so a hook's raw args and command still trail.
	if hook.Dind {
		args = append(args, "--privileged", "--mount", "type=volume,dst=/var/lib/docker")
	}
	args = append(args, hook.ExtraDockerArgs...)
	args = append(args, image)
	if stateForwarding {
		// The shim is the entrypoint; hand it the command the image would have
		// run (its ENTRYPOINT+CMD, or hook.Command when set) to exec after
		// starting the proxy.
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
		args = append(args, "kv-forward")
		args = append(args, childArgv...)
	} else {
		// With no command override, the image's CMD/ENTRYPOINT runs.
		args = append(args, hook.Command...)
	}

	r.log.Info("hook starting",
		"hook", hook.ID, "run", run.ID(), "image", image, "timeout", timeout)
	r.events.Record("run.started", fmt.Sprintf("%s run %s started (%s)%s", hook.ID, runRef(run), image, spawnNote(run)),
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
	// The handoff instant: everything after this and before the container's
	// own first instruction (PhaseContainerEntry, reported by the injected
	// shim) is Docker's create/namespace/overlay/entrypoint cost.
	run.Mark(runs.PhaseSpawned)
	run.SetRunning()

	timedOut := make(chan struct{})
	cancelled := make(chan struct{})
	stopWatcher := make(chan struct{})

	// The timeout watchdog arms NOW — only after the concurrency slot was
	// acquired and the container actually launched, so a queued run never
	// ticks — and any output byte on either stream resets it via the
	// touchReader wrappers. The timeout is activity-based: it fires only
	// after `timeout` of NO output, so a run that keeps logging progress
	// runs as long as it needs (there is no absolute wall-clock ceiling),
	// while one that has gone silent is killed.
	wd := newIdleWatchdog(timeout, time.Now)
	wd.Arm()
	// Declared waits (the state API's POST /wait) count as activity: hand
	// the run a handle to this watchdog so an in-flight wait keeps touching
	// it — an announced sleep is forward progress, not silence. Touches on
	// a finished run are harmless, so this is never deregistered.
	run.SetActivityTouch(wd.Touch)
	silent := wd.Watch(stopWatcher)
	stdout := &touchReader{r: stdoutR, touch: wd.Touch}
	stderr := &touchReader{r: stderrR, touch: wd.Touch}

	var streamWG sync.WaitGroup
	streamWG.Add(2)
	go r.streamPipe(&streamWG, stdout, hook.ID, run, "stdout")
	go r.streamPipe(&streamWG, stderr, hook.ID, run, "stderr")

	// Watch for the no-output timeout, parent-context cancellation, and
	// explicit cancel requests in parallel with cmd.Wait. Any one kills the
	// container by name. A cancel requested before this goroutine started
	// selects immediately (the channel is already closed), so the pre-start
	// race is covered.
	go func() {
		select {
		case <-silent:
			close(timedOut)
		case <-parent.Done():
			// Parent cancelled (e.g. the caller tearing down): kill the
			// container and let cmd.Wait's error shape the terminal status.
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
	// The docker CLI has returned: the container exited AND `--rm` teardown
	// is done. Whatever separates this from Finished is the runner's own
	// bookkeeping, not container cost.
	run.Mark(runs.PhaseExited)
	close(stopWatcher)

	// In the normal case the process has exited and its pipe ends
	// are closed, so streamWG.Wait returns immediately. On a kill
	// (timeout or cancel), orphaned child processes (e.g. the real docker
	// container's descendants) can keep the write end open; force-close
	// the read ends so the scanner goroutines unblock.
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
		// The message names the activity semantics: the run died for going
		// silent, not for running long.
		errMsg = fmt.Sprintf("timed out after %s (no output)", timeout)
	default:
	}
	// Checked after the timeout so an explicit cancel takes precedence when
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
	// what forced callers to add a second Runner.Wait() barrier. See
	// runs.Run.SetOnTerminal.
	run.SetOnTerminal(func(runs.RunState) {
		r.log.Info("hook finished",
			"hook", hook.ID, "run", run.ID(), "status", status, "exit", exitCode)
		// runRef, not run.ID(): a title set mid-run via the state API's /title
		// lands here too, so the feed's terminal line names the subject.
		finishedMsg := fmt.Sprintf("%s run %s finished: %s (exit %d)%s", hook.ID, runRef(run), status, exitCode, spawnNote(run))
		if errMsg != "" && (status == runs.StatusTimeout ||
			(status == runs.StatusCancelled && errMsg != "cancelled")) {
			// Carry the reason (a timeout's "no output" verdict, a cancel's
			// steal explanation) so the activity feed shows what killed the run.
			finishedMsg += ": " + errMsg
		}
		r.events.Record("run.finished", finishedMsg,
			map[string]string{"hook": hook.ID, "run": run.ID(), "status": string(status)})
	})
	run.Finish(status, exitCode, errMsg)
	// The GitHub commit-status POST stays OUTSIDE the terminal seam: it is a
	// network call, and blocking every Done() observer on it would trade one
	// footgun for a worse one.
	if r.onFinish != nil {
		r.onFinish(hook, run, payload)
	}
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
