package cli

// The serve command's option set and its WEBHOOK_RUNNER_* environment
// parsing — split from serve.go, which holds the actual server wiring.

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
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

	// maxConcurrentRuns is the DEFAULT global run cap — the server-wide
	// ceiling on simultaneously running hook containers
	// (WEBHOOK_RUNNER_MAX_CONCURRENT_RUNS; unset = the built-in 64). The
	// dashboard's persisted override (overrides.json) wins over it at
	// runtime; this is only what "no override" reverts to.
	maxConcurrentRuns int

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

	// restartMaxDefer bounds how long GET /restart-ready may refuse an
	// update because runs are in flight (0 = the server's default).
	restartMaxDefer time.Duration
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
	if o.maxConcurrentRuns <= 0 {
		// The global run cap default. Unset/empty means the built-in
		// default; a set-but-invalid value FAILS startup (the
		// reloadPollInterval rule) — a typo'd cap silently falling back
		// to 64 could mask a deliberately tightened limit.
		o.maxConcurrentRuns = concurrency.DefaultGlobalLimit
		if v := os.Getenv("WEBHOOK_RUNNER_MAX_CONCURRENT_RUNS"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("WEBHOOK_RUNNER_MAX_CONCURRENT_RUNS %q: %w (integer >= 1; unset means the default %d)",
					v, err, concurrency.DefaultGlobalLimit)
			}
			if n < 1 {
				return fmt.Errorf("WEBHOOK_RUNNER_MAX_CONCURRENT_RUNS %q: must be >= 1 — a 0 cap would block every run (unset means the default %d)",
					v, concurrency.DefaultGlobalLimit)
			}
			o.maxConcurrentRuns = n
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

	// How long GET /restart-ready (the docker-updater pre-check) may keep
	// refusing an update because runs are in flight. Unset = the server's
	// default; a NEGATIVE value disables the force so the check blocks for
	// as long as the fleet stays busy. Unparseable FAILS startup: a typo
	// here would silently decide whether updates ever land.
	if v := os.Getenv("WEBHOOK_RUNNER_RESTART_MAX_DEFER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("WEBHOOK_RUNNER_RESTART_MAX_DEFER %q: %w (Go duration; negative disables the force)", v, err)
		}
		if d == 0 {
			// Zero means "use the default" to the server, which would make
			// "0" here read as disable — refuse the ambiguity outright.
			return fmt.Errorf("WEBHOOK_RUNNER_RESTART_MAX_DEFER %q: use a negative duration to never force, or omit it for the default", v)
		}
		o.restartMaxDefer = d
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
