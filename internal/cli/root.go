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
and exposes POST /_reload on the hook port to accept a GitHub push webhook
(authenticated with WEBHOOK_RUNNER_HOOKS_REPO_SECRET via HMAC-SHA256).

Configuration via environment:
  WEBHOOK_RUNNER_HOOKS_DIR            hooks directory (or positional arg)
  WEBHOOK_RUNNER_HOOKS_REPO           Git URL to clone hooks from (SSH recommended)
  WEBHOOK_RUNNER_HOOKS_BRANCH         branch to track (default: repo default)
  WEBHOOK_RUNNER_HOOKS_REPO_SECRET    HMAC-SHA256 secret for POST /_reload
  WEBHOOK_RUNNER_ADDR                 hook port (default :9000)
  WEBHOOK_RUNNER_ADMIN_ADDR           admin port (default :9001)
  WEBHOOK_RUNNER_DATA_DIR             dir for KV state + token secret (default: hooks-dir parent)
  WEBHOOK_RUNNER_STATE_SOCKET         KV API Unix socket path (default: $TMPDIR/whr-state.sock)
  WEBHOOK_RUNNER_STATE_SECRET         HMAC secret for KV tokens (default: generated + persisted)
  WEBHOOK_RUNNER_KV_MAX_KEYS          max keys per hook KV namespace (default 5000)
  WEBHOOK_RUNNER_RUN_RETENTION        persisted run-history retention, Go duration (default 48h)
  WEBHOOK_RUNNER_RUN_RETENTION_MAX    persisted runs kept per hook, disk safety net (default 200000)
  WEBHOOK_RUNNER_GITHUB_TOKEN         GitHub token for commit-status updates
  WEBHOOK_RUNNER_LOG_FORMAT           "text" (default) or "json"`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		applyServeEnv(serveOptsRoot)
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
}

// Execute runs the CLI. It is the only entry point main.go needs.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		// Cobra has already printed the error to stderr.
		os.Exit(1)
	}
}
