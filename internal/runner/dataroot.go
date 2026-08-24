package runner

// Where the daemon this runner talks to keeps its store.
//
// A container's writable layer is an overlayfs upperdir whose location is a
// DAEMON property (data-root), never a per-container one, so relocating it is
// the only thing that covers the writes no mount enumerated. Which disk absorbs
// them is the whole point of the arrangement.
//
// Nothing about it is visible at run time: a daemon.json that did not parse, a
// mount that was not there when dockerd started, or a data-root pointing
// somewhere other than the intended dataset all leave the runner writing to the
// system disk with no symptom. That is the failure this probe exists to make
// impossible to miss.
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
			"Check the daemon's data-root and that its mount was present when dockerd started (WEBHOOK_RUNNER_EXPECT_DATA_ROOT declares where the store belongs)",
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
