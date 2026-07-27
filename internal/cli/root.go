// Package cli wires the cobra command tree for webhook-runner.
//
// The root command runs the HTTP server. Subcommands (validate, test,
// version) register themselves via init() in their own files.
package cli

import (
	"errors"
	"os"

	"github.com/spf13/cobra"
)

var serveOptsRoot = &serveOptions{}

var rootCmd = &cobra.Command{
	Use:   "webhook-runner [hooks-dir]",
	Short: "HTTP server that executes webhooks in disposable Docker containers",
	Long: `webhook-runner serves incoming webhooks, executing each one in a disposable
Docker container. Each hook is configured by a hook.json file in its own
folder. The server hot-reloads hook definitions when the directory changes.

The server listens on two ports: the hook port (default :9000) handles
incoming webhooks and should be publicly accessible, while the admin port
(default :9001) serves the dashboard, hook list, and run history and should
be placed behind authentication (e.g. Cloudflare Zero Trust). The per-hook
KV store is served on a Unix socket (not a port) that the runner bind-mounts
into state hooks; each request is authenticated by the bearer token the
runner injects.

Hooks can be loaded from a local directory or cloned from a Git repository.
When WEBHOOK_RUNNER_HOOKS_REPO is set, the server clones the repo on startup
and exposes POST /_reload on the hook port to accept the repo's GitHub
webhook — push AND status events (authenticated with
WEBHOOK_RUNNER_HOOKS_REPO_SECRET via HMAC-SHA256). Reloads are CI-gated by
default: a push only records the new tip, and the tree switches when GitHub
reports a successful gating commit status (context "all-builds") for it;
set WEBHOOK_RUNNER_HOOKS_GATE_CONTEXT to another context, or to an empty
string to disable the gate (legacy reload-on-any-signed-POST).

Configuration via environment:
  WEBHOOK_RUNNER_HOOKS_DIR            hooks directory (or positional arg)
  WEBHOOK_RUNNER_HOOKS_REPO           Git URL to clone hooks from (SSH recommended)
  WEBHOOK_RUNNER_HOOKS_BRANCH         branch to track (default: repo default)
  WEBHOOK_RUNNER_HOOKS_REPO_SECRET    HMAC-SHA256 secret for POST /_reload
  WEBHOOK_RUNNER_HOOKS_GATE_CONTEXT   commit-status context gating reloads (unset: all-builds; empty: gate disabled)
  WEBHOOK_RUNNER_RELOAD_POLL_INTERVAL reload-gate reconciliation poll cadence, Go duration (default 1h; 0 disables)
  WEBHOOK_RUNNER_RESTART_MAX_DEFER    how long GET /restart-ready may refuse an update while runs are in flight, Go duration (default 6h; negative never forces)
  WEBHOOK_RUNNER_ADDR                 hook port (default :9000)
  WEBHOOK_RUNNER_ADMIN_ADDR           admin port (default :9001)
  WEBHOOK_RUNNER_DATA_DIR             dir for KV state + token secret (default: hooks-dir parent)
  WEBHOOK_RUNNER_STATE_SOCKET         KV API Unix socket path (default: $TMPDIR/whr-state.sock)
  WEBHOOK_RUNNER_STATE_SECRET         HMAC secret for KV tokens (default: generated + persisted)
  WEBHOOK_RUNNER_RUN_RETENTION        persisted run-history retention, Go duration (default 48h)
  WEBHOOK_RUNNER_RUN_RETENTION_MAX    persisted runs kept per hook, disk safety net (default 200000)
  WEBHOOK_RUNNER_MAX_CONCURRENT_RUNS  global cap on simultaneously running hook containers (default 64;
                                      the dashboard's persisted override wins over it; excess runs queue)
  WEBHOOK_RUNNER_GITHUB_TOKEN         GitHub token for commit-status updates
  WEBHOOK_RUNNER_LOG_FORMAT           "text" (default) or "json"`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// An explicitly passed --hooks-gate-context wins over the env var;
		// the distinction matters because an EMPTY value disables the gate.
		serveOptsRoot.gateContextSet = cmd.Flags().Changed("hooks-gate-context")
		if err := applyServeEnv(serveOptsRoot); err != nil {
			return err
		}
		if len(args) == 1 {
			serveOptsRoot.hooksDir = args[0]
		}
		if serveOptsRoot.hooksDir == "" && serveOptsRoot.hooksRepo == "" {
			return errors.New("hooks directory required (positional arg, WEBHOOK_RUNNER_HOOKS_DIR, or WEBHOOK_RUNNER_HOOKS_REPO)")
		}
		return runServe(cmd.Context(), serveOptsRoot)
	},
}

func init() {
	rootCmd.Flags().StringVar(&serveOptsRoot.addr, "addr", "", "hook listen address (default :9000, env WEBHOOK_RUNNER_ADDR)")
	rootCmd.Flags().StringVar(&serveOptsRoot.adminAddr, "admin-addr", "", "admin listen address (default :9001, env WEBHOOK_RUNNER_ADMIN_ADDR)")
	rootCmd.Flags().StringVar(&serveOptsRoot.dataDir, "data-dir", "", "directory for KV state and the token secret (default: hooks-dir parent, env WEBHOOK_RUNNER_DATA_DIR)")
	rootCmd.Flags().StringVar(&serveOptsRoot.stateSocket, "state-socket", "", "KV API Unix socket path (default: $TMPDIR/whr-state.sock, env WEBHOOK_RUNNER_STATE_SOCKET)")
	rootCmd.Flags().StringVar(&serveOptsRoot.logFormat, "log-format", "", "log format: text or json (env WEBHOOK_RUNNER_LOG_FORMAT)")
	rootCmd.Flags().StringVar(&serveOptsRoot.hooksRepo, "hooks-repo", "", "Git URL to clone hooks from (env WEBHOOK_RUNNER_HOOKS_REPO)")
	rootCmd.Flags().StringVar(&serveOptsRoot.hooksBranch, "hooks-branch", "", "branch to track (env WEBHOOK_RUNNER_HOOKS_BRANCH)")
	rootCmd.Flags().StringVar(&serveOptsRoot.gateContext, "hooks-gate-context", "all-builds", "commit-status context gating hooks-repo reloads; empty disables the gate (env WEBHOOK_RUNNER_HOOKS_GATE_CONTEXT)")
}

// Execute runs the CLI. It is the only entry point main.go needs.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		// Cobra has already printed the error to stderr.
		os.Exit(1)
	}
}
