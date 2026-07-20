package runner

// Manager INSTANCES: the long-lived supervised container behind a manager
// entity. An instance is a FIRST-CLASS identity, deliberately NOT a run
// (operator directive): it never registers with the tracker, never
// persists to the run store, and never appears in the runs list or the
// timeline — a forever-running bar would permanently pollute the chart.
// The run-shaped mechanisms it needs are wired against the instance
// identity instead: the state token is kv.Token(managerID, instanceID)
// (the existing format — instance ids share the run-id alphabet), the
// supervisor releases the instance's cooperative locks when it ends (the
// finish-seam analog), and the idle watchdog guards the container with
// manager-shaped arming (the inbox arms it only while an event is checked
// out, so an idle parked manager is never reaped).
//
// This is deliberately a SEPARATE path from execute() — the hook path
// stays byte-identical — sharing its building blocks (EnsureImage, temp
// files, the shim/imageCommand contract, the idle watchdog,
// docker-kill-by-name, the concurrency manager).
//
// Full hook feature parity (operator directive), manager-shaped:
// dind injects the same two flags; concurrency_group holds ONE slot for
// the instance's whole life (acquired before launch, queued while full,
// released at end); run_title/synchronous/github_status are handled by the
// supervisor/server around the inbox (titles, delivery holds, per-delivery
// statuses) — not here.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/managers"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// managerStopGraceSeconds is how long docker stop waits between SIGTERM
// and SIGKILL on a graceful instance stop. Managers should exit promptly
// on SIGTERM (finish the in-flight event, flush, exit); the grace bounds a
// slow one.
const managerStopGraceSeconds = 30

// RemoveManagerContainer force-removes a (possibly orphaned) manager
// instance container by its deterministic name. Called by the supervisor
// on lease acquire and before every instance start; an absent container is
// the normal case and only debug-logged.
func (r *Runner) RemoveManagerContainer(name string) {
	cmd := exec.Command(r.dockerBin, "rm", "-f", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.log.Debug("docker rm -f", "name", name, "err", err, "out", strings.TrimSpace(string(out)))
	} else {
		r.log.Info("removed stale manager container", "name", name)
	}
}

// RunManagerSession runs ONE instance of a manager: builds the image,
// launches the container, binds the inbox, and blocks until the container
// exits (crash, watchdog kill, graceful stop, shutdown). Implements
// managers.SessionRunner. sink receives every output line (the
// supervisor's bounded panel tail); onStarted fires when the container
// actually launched.
func (r *Runner) RunManagerSession(ctx context.Context, m *hooks.Manager, ib *managers.Inbox, instanceID, containerName string, stop <-chan managers.StopRequest, onStarted func(), sink func(line string)) managers.SessionOutcome {
	hook := m.Hook
	if sink == nil {
		sink = func(string) {}
	}

	fail := func(status runs.Status, msg string, requested bool) managers.SessionOutcome {
		r.log.Error("manager instance failed", "manager", hook.ID, "instance", instanceID, "err", msg)
		return managers.SessionOutcome{Status: status, RequestedStop: requested, Err: msg}
	}

	// One merged abort signal: a supervisor stop request OR the run
	// context ending. The forwarder owns the single receive from stop; the
	// launch stages and the container watcher all watch `abort`.
	abort := make(chan struct{})
	sessionDone := make(chan struct{})
	defer close(sessionDone)
	var stopMu sync.Mutex
	stopReason := ""
	stopRequested := false
	go func() {
		select {
		case req := <-stop:
			stopMu.Lock()
			stopReason = req.Reason
			stopRequested = true
			stopMu.Unlock()
			close(abort)
		case <-ctx.Done():
			close(abort)
		case <-sessionDone:
		}
	}()
	aborted := func() bool {
		select {
		case <-abort:
			return true
		default:
			return false
		}
	}
	requestedOutcome := func(status runs.Status) managers.SessionOutcome {
		stopMu.Lock()
		reason := stopReason
		req := stopRequested
		stopMu.Unlock()
		if !req {
			// ctx-driven abort (process shutting down): still a requested
			// stop from the supervisor's perspective, never a failure.
			reason = "runner shutting down"
		}
		return managers.SessionOutcome{Status: status, RequestedStop: true, Err: reason}
	}

	// The drain gate: an instance launched by a dying process races the
	// state-socket handover exactly like a hook run.
	if r.draining.Load() {
		return fail(runs.StatusError, ErrDraining.Error(), true)
	}

	var secrets map[string]string
	if r.secrets != nil {
		var err error
		secrets, err = r.secrets.Load(hook)
		if err != nil {
			return fail(runs.StatusError, fmt.Sprintf("manager secrets: %v", err), false)
		}
	}
	lookup := hooks.SecretsFirstLookup(secrets)

	buildLog := &slogLineWriter{logFn: func(line string) {
		r.log.Info("manager image build", "manager", hook.ID, "instance", instanceID, "line", line)
		sink(line)
	}}
	buildStart := time.Now()
	image, built, err := EnsureImage(r.dockerBin, hook, buildLog)
	if err != nil {
		r.events.Record("image.build_failed", fmt.Sprintf("image build for manager %s failed: %v", hook.ID, err),
			map[string]string{"hook": hook.ID})
		return fail(runs.StatusError, fmt.Sprintf("manager image: %v", err), false)
	}
	if built {
		r.events.Record("image.built", fmt.Sprintf("built %s in %s", image, time.Since(buildStart).Round(time.Millisecond)),
			map[string]string{"hook": hook.ID, "tag": image})
	}
	if aborted() {
		return requestedOutcome(runs.StatusCancelled)
	}

	// concurrency_group, manager-shaped: the INSTANCE holds one slot for
	// its whole life — acquired here (queued while the group is full,
	// abortable), released when the session ends. A persistent holder is a
	// persistent slot; sharing a group with bursty hooks is an operator
	// choice, not a footgun we silently ignore.
	release := func() {}
	if hook.ConcurrencyGroup != "" {
		var acquired bool
		release, acquired, err = r.groups.Acquire(hook.ConcurrencyGroup, instanceID, abort, func(concurrency.QueueState) {})
		if err != nil {
			r.events.Record("run.misconfigured", fmt.Sprintf("manager %s: %v", hook.ID, err),
				map[string]string{"hook": hook.ID, "group": hook.ConcurrencyGroup})
			return fail(runs.StatusError, err.Error(), false)
		}
		if !acquired {
			return requestedOutcome(runs.StatusCancelled)
		}
	}
	defer release()

	// The synthetic payload keeps the HOOK_PAYLOAD_FILE contract intact for
	// shared container tooling; a manager's real input is the inbox.
	payload, _ := json.Marshal(map[string]string{
		"trigger": "manager",
		"manager": hook.ID,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
	headers := http.Header{"Content-Type": []string{"application/json"}}
	payloadPath, headersPath, cleanup, err := r.writeTempFiles(instanceID, payload, headers)
	if err != nil {
		return fail(runs.StatusError, fmt.Sprintf("write temp files: %v", err), false)
	}
	defer cleanup()

	// Managers REQUIRE the state socket (inbox, KV, locks, /spawn all ride
	// it): refuse to run without the injection rather than start a manager
	// that cannot function.
	if r.kv == nil || r.kvSocket == "" || r.kvShim == "" {
		return fail(runs.StatusError, "manager requires the state store (KV socket/shim not configured)", false)
	}

	args := []string{
		"run", "--rm",
		"--name", containerName,
		"-v", payloadPath + ":" + mountedPayload + ":ro",
		"-v", headersPath + ":" + mountedHeaders + ":ro",
		"-e", "HOOK_PAYLOAD_FILE=" + mountedPayload,
		"-e", "HOOK_HEADERS_FILE=" + mountedHeaders,
		"-e", "HOOK_ID=" + hook.ID,
		"-e", "HOOK_RUN_ID=" + instanceID,
		"--entrypoint", mountedShim,
		"-v", r.kvShim + ":" + mountedShim + ":ro",
		"-v", r.kvSocket + ":" + mountedStateSocket,
		"-e", "HOOK_KV_SOCKET=" + mountedStateSocket,
		"-e", "HOOK_KV_URL=http://localhost:9002",
		"-e", "HOOK_KV_TOKEN=" + r.kv.Token(hook.ID, instanceID),
	}
	args = append(args, r.gsmArgs(hook.ID)...)
	for _, n := range hook.Networks {
		args = append(args, "--network", n)
	}
	for _, v := range hook.Volumes {
		args = append(args, "-v", v)
	}
	for k, v := range secrets {
		if hooks.ReservedEnvKey(k) {
			r.log.Warn("manager secret shadows a reserved env key; skipped",
				"manager", hook.ID, "instance", instanceID, "env", k)
			continue
		}
		args = append(args, "-e", k+"="+v)
	}
	for k, v := range hook.Env {
		expanded, missing := hooks.ExpandEnvRefs(v, lookup)
		for _, name := range missing {
			r.log.Warn("manager env references unset variable",
				"manager", hook.ID, "instance", instanceID, "env", k, "var", name)
			r.events.Record("env.unresolved",
				hook.ID+": env "+k+" references unset ${"+name+"}; the container gets an empty value",
				map[string]string{"hook": hook.ID})
		}
		args = append(args, "-e", k+"="+expanded)
	}
	if hook.User != "" {
		args = append(args, "--user", hook.User)
	}
	if hook.Workdir != "" {
		args = append(args, "--workdir", hook.Workdir)
	}
	// dind, exactly the hook flags (see execute): --privileged + an
	// anonymous /var/lib/docker volume; --rm reaps the volume at exit.
	if hook.Dind {
		args = append(args, "--privileged", "--mount", "type=volume,dst=/var/lib/docker")
	}
	args = append(args, hook.ExtraDockerArgs...)
	args = append(args, image)
	childArgv, err := imageCommand(r.dockerBin, image, hook.Command)
	if err != nil {
		r.events.Record("image.inspect_failed", fmt.Sprintf("inspect %s for manager %s failed: %v", image, hook.ID, err),
			map[string]string{"hook": hook.ID})
		return fail(runs.StatusError, fmt.Sprintf("inspect image command: %v", err), false)
	}
	args = append(args, "kv-forward")
	args = append(args, childArgv...)

	if aborted() {
		return requestedOutcome(runs.StatusCancelled)
	}

	r.log.Info("manager instance starting",
		"manager", hook.ID, "instance", instanceID, "image", image, "timeout", hook.Timeout())

	cmd := exec.Command(r.dockerBin, args...)
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return fail(runs.StatusError, fmt.Sprintf("stdout pipe: %v", err), false)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return fail(runs.StatusError, fmt.Sprintf("stderr pipe: %v", err), false)
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return fail(runs.StatusError, fmt.Sprintf("start docker: %v", err), false)
	}
	stdoutW.Close()
	stderrW.Close()

	// The manager-shaped watchdog: created UNARMED — the inbox arms it when
	// an event is checked out (or queued unconsumed) and disarms it when
	// the manager comes back for the next event. Output bytes touch it like
	// a hook run's; /wait holds feed it via the supervisor's TouchInstance.
	wd := newIdleWatchdog(hook.Timeout(), time.Now)
	ib.BindInstance(instanceID, wd.Arm, wd.Disarm, wd.Touch)
	defer ib.UnbindInstance()
	if onStarted != nil {
		onStarted()
	}
	r.events.Record("manager.started",
		fmt.Sprintf("%s instance %s started (%s)", hook.ID, instanceID, image),
		map[string]string{"hook": hook.ID, "tag": image})

	timedOut := make(chan struct{})
	stopped := make(chan struct{})
	stopWatcher := make(chan struct{})

	silent := wd.Watch(stopWatcher)
	stdout := &touchReader{r: stdoutR, touch: wd.Touch}
	stderr := &touchReader{r: stderrR, touch: wd.Touch}

	var streamWG sync.WaitGroup
	streamWG.Add(2)
	go r.streamManagerPipe(&streamWG, stdout, hook.ID, instanceID, "stdout", sink)
	go r.streamManagerPipe(&streamWG, stderr, hook.ID, instanceID, "stderr", sink)

	go func() {
		select {
		case <-silent:
			close(timedOut)
			r.killContainer(containerName)
		case <-abort:
			close(stopped)
			// Graceful: SIGTERM, grace, then docker's own SIGKILL. Blocking
			// is fine — this goroutine has nothing else to do, and cmd.Wait
			// unblocks the session the moment the container dies.
			r.stopContainer(containerName, managerStopGraceSeconds)
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
	close(stopWatcher)

	killedByWatcher := false
	for _, ch := range []chan struct{}{timedOut, stopped} {
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
	outcome := managers.SessionOutcome{}
	select {
	case <-timedOut:
		status = runs.StatusTimeout
		errMsg = fmt.Sprintf("timed out after %s (no progress on a checked-out event)", hook.Timeout())
		outcome = managers.SessionOutcome{Status: status, Err: errMsg}
	default:
		select {
		case <-stopped:
			// A supervisor-requested stop (or process shutdown) is a
			// deliberate lifecycle transition, never a failure.
			outcome = requestedOutcome(runs.StatusCancelled)
			status = outcome.Status
			errMsg = outcome.Err
		default:
			outcome = managers.SessionOutcome{Status: status, Err: errMsg}
		}
	}
	r.log.Info("manager instance finished",
		"manager", hook.ID, "instance", instanceID, "status", status, "exit", exitCode,
		"requested_stop", outcome.RequestedStop)
	msg := fmt.Sprintf("%s instance %s ended: %s (exit %d)", hook.ID, instanceID, status, exitCode)
	if errMsg != "" {
		msg += ": " + errMsg
	}
	r.events.Record("manager.exited", msg,
		map[string]string{"hook": hook.ID, "status": string(status)})
	return outcome
}

// streamManagerPipe scans one output pipe into the server log and the
// supervisor's panel sink — the manager analog of streamPipe, minus the
// run ring (instances are not runs; their tail lives with the supervisor).
func (r *Runner) streamManagerPipe(wg *sync.WaitGroup, rc io.Reader, managerID, instanceID, stream string, sink func(string)) {
	defer wg.Done()
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		sink(line)
		r.log.Info("manager output",
			"manager", managerID, "instance", instanceID, "stream", stream, "line", line)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		r.log.Warn("manager output scanner error",
			"manager", managerID, "instance", instanceID, "stream", stream, "err", err)
	}
}

// stopContainer gracefully stops a container: SIGTERM, the given grace,
// then docker's own SIGKILL. Blocking (up to ~grace).
func (r *Runner) stopContainer(name string, graceSeconds int) {
	cmd := exec.Command(r.dockerBin, "stop", "-t", strconv.Itoa(graceSeconds), name)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.log.Debug("docker stop", "name", name, "err", err, "out", strings.TrimSpace(string(out)))
	}
}

// gsmArgs is the enforced-GitHub-gateway injection (inert while GSMURL is
// unset — the default): a per-container fail-closed blackhole for
// api.github.com plus the fleet-default GITHUB_API_URL, for every hook,
// manager, and test container EXCEPT ids on the operator's direct-access
// exemption list (WEBHOOK_RUNNER_GITHUB_DIRECT — CI-runner fleets whose
// job payloads legitimately call GitHub; operator-configurable, never
// hard-coded). The env default is injected BEFORE secrets and hook env
// (docker keeps the last -e), so an explicit hook.json GITHUB_API_URL
// still wins — the blackhole, not the env, is the enforcement.
func (r *Runner) gsmArgs(id string) []string {
	return gsmInjectArgs(r.gsm, id)
}

// GSMConfig is the enforced-GitHub-gateway knob pair, shared by the live
// runner and the `webhook-runner test` path (run/test parity, the dind
// rule).
type GSMConfig struct {
	// URL: the gateway base (WEBHOOK_RUNNER_GSM_URL). "" = enforcement off,
	// zero behavior change — the shipped default.
	URL string
	// Direct: ids exempt from enforcement (WEBHOOK_RUNNER_GITHUB_DIRECT).
	Direct map[string]bool
}

func gsmInjectArgs(cfg GSMConfig, id string) []string {
	if cfg.URL == "" || cfg.Direct[id] {
		return nil
	}
	return []string{
		"--add-host", "api.github.com:0.0.0.0",
		"-e", "GITHUB_API_URL=" + cfg.URL,
	}
}
