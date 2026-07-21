package cli

// The serve command's option set and its WEBHOOK_RUNNER_* environment
// parsing — split from serve.go, which holds the actual server wiring.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/runner"
)

type serveOptions struct {
	addr            string
	adminAddr       string
	hooksDir        string
	dataDir         string
	logFormat       string
	ghToken         string
	hooksRepo       string
	hooksBranch     string
	hooksRepoSecret string
	hookBaseURL     string
	stateSocket     string
	stateSecret     string
	runRetention    time.Duration
	runRetentionMax int

	// gateContext is the commit-status context that gates hooks-repo
	// reloads ("" = gate disabled, legacy pull-on-any-signed-POST).
	// gateContextSet marks an explicit flag value so applyServeEnv can
	// tell "--hooks-gate-context=" (disable) from "not passed" (env,
	// then the all-builds default).
	gateContext    string
	gateContextSet bool

	// reloadPollInterval is the reload gate's reconciliation-poll cadence
	// (default 1h; 0 = poll disabled, gate stays purely event-driven).
	// reloadPollSet marks it parsed so a repeat applyServeEnv can't
	// stomp an explicit 0 back to the default.
	reloadPollInterval time.Duration
	reloadPollSet      bool

	// gsmURL is the enforced-GitHub-gateway knob (WEBHOOK_RUNNER_GSM_URL).
	// Unset (the shipped default) = enforcement OFF, zero behavior change.
	// Set = every hook/manager/test container except the exemption list
	// gets the api.github.com blackhole + the GITHUB_API_URL default, and
	// the runner's own GitHub client (commit statuses, the reload-gate
	// poll) follows it unless githubAPIURL overrides.
	gsmURL string
	// githubDirect (WEBHOOK_RUNNER_GITHUB_DIRECT) is the operator's
	// comma-separated exemption list: ids whose containers keep DIRECT
	// GitHub access under enforcement (the CI-runner fleets whose job
	// payloads legitimately call api.github.com). Operator-configurable,
	// never hard-coded.
	githubDirect string
	// githubAPIURL (WEBHOOK_RUNNER_GITHUB_API_URL) overrides the base URL
	// of the runner's OWN GitHub client. Empty = follow gsmURL when set,
	// else api.github.com.
	githubAPIURL string
}

func applyServeEnv(o *serveOptions) error {
	if o.addr == "" {
		o.addr = firstNonEmpty(os.Getenv("WEBHOOK_RUNNER_ADDR"), ":9000")
	}
	if o.adminAddr == "" {
		o.adminAddr = firstNonEmpty(os.Getenv("WEBHOOK_RUNNER_ADMIN_ADDR"), ":9001")
	}
	if o.hooksDir == "" {
		o.hooksDir = os.Getenv("WEBHOOK_RUNNER_HOOKS_DIR")
	}
	if o.dataDir == "" {
		o.dataDir = os.Getenv("WEBHOOK_RUNNER_DATA_DIR")
	}
	if o.stateSocket == "" {
		o.stateSocket = os.Getenv("WEBHOOK_RUNNER_STATE_SOCKET")
	}
	if o.stateSecret == "" {
		o.stateSecret = os.Getenv("WEBHOOK_RUNNER_STATE_SECRET")
	}
	if o.runRetention <= 0 {
		// Go duration (e.g. "72h"); unset or unparseable falls back to the
		// run store's built-in 48h default.
		if d, err := time.ParseDuration(os.Getenv("WEBHOOK_RUNNER_RUN_RETENTION")); err == nil && d > 0 {
			o.runRetention = d
		}
	}
	if o.runRetentionMax <= 0 {
		if n, err := strconv.Atoi(os.Getenv("WEBHOOK_RUNNER_RUN_RETENTION_MAX")); err == nil && n > 0 {
			o.runRetentionMax = n
		}
	}
	if o.logFormat == "" {
		o.logFormat = firstNonEmpty(os.Getenv("WEBHOOK_RUNNER_LOG_FORMAT"), "text")
	}
	o.ghToken = os.Getenv("WEBHOOK_RUNNER_GITHUB_TOKEN")
	if o.hooksRepo == "" {
		o.hooksRepo = os.Getenv("WEBHOOK_RUNNER_HOOKS_REPO")
	}
	if o.hooksBranch == "" {
		o.hooksBranch = os.Getenv("WEBHOOK_RUNNER_HOOKS_BRANCH")
	}
	if o.hooksRepoSecret == "" {
		o.hooksRepoSecret = os.Getenv("WEBHOOK_RUNNER_HOOKS_REPO_SECRET")
	}
	if o.hookBaseURL == "" {
		o.hookBaseURL = os.Getenv("WEBHOOK_RUNNER_HOOK_BASE_URL")
	}
	if o.gsmURL == "" {
		o.gsmURL = os.Getenv("WEBHOOK_RUNNER_GSM_URL")
	}
	if o.githubDirect == "" {
		o.githubDirect = os.Getenv("WEBHOOK_RUNNER_GITHUB_DIRECT")
	}
	if o.githubAPIURL == "" {
		o.githubAPIURL = os.Getenv("WEBHOOK_RUNNER_GITHUB_API_URL")
	}
	if !o.gateContextSet {
		// LookupEnv, not Getenv: set-to-EMPTY deliberately disables the
		// reload CI gate (legacy behavior), while unset means the default
		// gating context. An explicit --hooks-gate-context flag wins.
		if v, ok := os.LookupEnv("WEBHOOK_RUNNER_HOOKS_GATE_CONTEXT"); ok {
			o.gateContext = v
		} else if o.gateContext == "" {
			o.gateContext = "all-builds"
		}
		o.gateContextSet = true
	}
	if !o.reloadPollSet {
		// Go duration; unset (or empty) means the 1h default and an
		// explicit 0 disables the reconciliation poll. Unlike the
		// fall-back-quietly numeric options above, a value that does not
		// parse — or is negative — FAILS startup: a typo here would
		// otherwise silently change how quickly deploys converge.
		o.reloadPollInterval = time.Hour
		if v := os.Getenv("WEBHOOK_RUNNER_RELOAD_POLL_INTERVAL"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("WEBHOOK_RUNNER_RELOAD_POLL_INTERVAL %q: %w (Go duration; 0 disables the poll)", v, err)
			}
			if d < 0 {
				return fmt.Errorf("WEBHOOK_RUNNER_RELOAD_POLL_INTERVAL %q: must be >= 0 (0 disables the poll)", v)
			}
			o.reloadPollInterval = d
		}
		o.reloadPollSet = true
	}
	return nil
}

func firstNonEmpty(parts ...string) string {
	for _, p := range parts {
		if p != "" {
			return p
		}
	}
	return ""
}

// parseGithubDirect splits the WEBHOOK_RUNNER_GITHUB_DIRECT comma list
// into the exemption set (empty entries dropped, whitespace trimmed).
func parseGithubDirect(raw string) map[string]bool {
	out := map[string]bool{}
	for _, id := range strings.Split(raw, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	return out
}

// gsmFromEnv builds the enforced-GitHub-gateway config straight from the
// environment — the `test` command's path (serve builds it from its parsed
// options instead, same values).
func gsmFromEnv() runner.GSMConfig {
	return runner.GSMConfig{
		URL:    os.Getenv("WEBHOOK_RUNNER_GSM_URL"),
		Direct: parseGithubDirect(os.Getenv("WEBHOOK_RUNNER_GITHUB_DIRECT")),
	}
}
