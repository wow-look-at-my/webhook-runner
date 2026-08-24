package cli

// The host probes serve runs once, before anything is served.
//
// All three are BOOT-SCOPED: a running process's environment does not change,
// and a daemon does not gain or lose a data-root or a userns mapping underneath
// it. So each verdict stands until a restart, which is why they are attention
// entries rather than anything re-evaluated per run.
//
// None of them is a gate. Each names a host condition an operator fixes on the
// host, and refusing to serve over one would take down every hook -- pr-minder,
// required-builds, the lot -- for a misconfiguration that affects some of them.

import (
	"log/slog"
	"os"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
)

// dockerBinary is the docker the runner shells out to.
func dockerBinary() string {
	if bin := os.Getenv("WEBHOOK_RUNNER_DOCKER_BIN"); bin != "" {
		return bin
	}
	return "docker"
}

// reportHostChecks runs the probes and files an attention entry for each
// verdict that is not clean.
func reportHostChecks(dockerBin string, logger *slog.Logger, rec *events.Recorder, agg *attention.Aggregator) {
	// A containerized server whose temp dir is not host-shared breaks every hook
	// run: payload mounts resolve on the docker HOST, not in this process's
	// namespace. See runner.WarnIfContainerized.
	if runner.WarnIfContainerized(logger, rec, "/.dockerenv", "/run/.containerenv") {
		agg.Report(attention.Entry{
			Source:  attention.SourceServer,
			Key:     attention.KeyTmpDir,
			Message: runner.TmpDirHazardMessage,
		})
	}

	// Whether the daemon confines a container's root to an unprivileged host
	// user. What actually keeps a container off the host is docker's read-only
	// /proc binds; remap is the layer that still holds when one of those is
	// wrong. See docs/internals/runner-isolation.md.
	if msg := runner.CheckUsernsRemap(dockerBin, logger, rec); msg != "" {
		agg.Report(attention.Entry{
			Source:  attention.SourceServer,
			Key:     attention.KeyHostIsolation,
			Message: msg,
		})
	}

	// Which filesystem this runner's containers actually write to. A daemon
	// rooted somewhere other than the intended dataset has no run-time symptom,
	// and controlling which disk absorbs the writes is the whole arrangement.
	if msg := runner.CheckDataRoot(dockerBin, os.Getenv("WEBHOOK_RUNNER_EXPECT_DATA_ROOT"), logger, rec); msg != "" {
		agg.Report(attention.Entry{
			Source:  attention.SourceServer,
			Key:     attention.KeyDataRoot,
			Message: msg,
		})
	}
}
