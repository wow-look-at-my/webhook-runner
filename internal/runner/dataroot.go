package runner

// Where the daemon this runner talks to keeps its store.
//
// Every container webhook-runner creates is transient — a hook run is one
// disposable container, and its image rebuilds from the hook directory on
// demand — so the whole store can live on a filesystem that is allowed to lose
// data. Pointing DOCKER_HOST at a daemon rooted there puts image layers,
// writable layers and volumes on it in one move, without touching the host's
// main daemon (which holds things that are NOT reconstructible).
//
// Nothing about that routing is visible at run time, though: a DOCKER_HOST
// typo, a socket that moved, or a unit that failed to start all leave the
// runner quietly talking to the default daemon and writing to whatever disk IT
// is rooted on. That is the failure this probe exists to make impossible to
// miss — the whole point of the arrangement is which disk absorbs the writes,
// so being wrong about it silently defeats it entirely.
//
// see docs/internals/scratch-dirs.md

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

// DataRootMismatchMessage renders the misconfiguration text shared by the
// startup log, the activity feed and the attention surface, so every surface
// says the same thing.
func DataRootMismatchMessage(want, got string) string {
	return fmt.Sprintf(
		"docker's store is at %q, not under the expected %q: this runner's containers are writing to the wrong filesystem. "+
			"Check DOCKER_HOST points at the intended daemon and that it is running (WEBHOOK_RUNNER_EXPECT_DATA_ROOT declares where its store belongs)",
		got, want)
}

// DataRootUnknownMessage is the verdict when the daemon could not be asked.
const DataRootUnknownMessage = "could not read docker's store location, so the filesystem this runner's containers write to is UNVERIFIED (WEBHOOK_RUNNER_EXPECT_DATA_ROOT is set, so it was meant to be checked)"

// CheckDataRoot asks the daemon where its store is and reports whether it sits
// under want. It ALWAYS logs the observed location — an operator asking "which
// disk is this writing to" should not have to guess — and returns a non-empty
// message when the answer is wrong or unavailable.
//
// want == "" means the deployment made no claim: the location is logged and
// nothing is reported. A claim that cannot be checked is treated as a failure
// rather than a pass, because "unverified" and "correct" are the same picture
// from here and only one of them is safe to assume.
func CheckDataRoot(dockerBin, want string, logger *slog.Logger, rec *events.Recorder) string {
	out, err := exec.Command(dockerBin, "info", "--format", "{{.DockerRootDir}}").Output()
	if err != nil {
		if want == "" {
			logger.Warn("could not read docker's store location", "err", err)
			return ""
		}
		logger.Error(DataRootUnknownMessage, "err", err)
		rec.Record("server.misconfigured", DataRootUnknownMessage, nil)
		return DataRootUnknownMessage
	}
	got := strings.TrimSpace(string(out))
	logger.Info("docker store location", "data_root", got, "expected_prefix", want)
	if want == "" || underPath(got, want) {
		return ""
	}
	msg := DataRootMismatchMessage(want, got)
	logger.Error(msg)
	rec.Record("server.misconfigured", msg, nil)
	return msg
}

// underPath reports whether p is want or sits beneath it. Compared by path
// SEGMENT, so "/mnt/pool2/docker" does not satisfy an expectation of
// "/mnt/pool" the way a plain string prefix would.
func underPath(p, want string) bool {
	p = strings.TrimSuffix(p, "/")
	want = strings.TrimSuffix(want, "/")
	return p == want || strings.HasPrefix(p, want+"/")
}
