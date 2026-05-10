// Package cli wires the cobra command tree for webhook-runner.
//
// The root command runs the HTTP server. Subcommands (validate, version)
// register themselves via init() in their own files.
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

Configuration via environment:
  WEBHOOK_RUNNER_HOOKS_DIR     hooks directory (or pass as positional arg)
  WEBHOOK_RUNNER_ADDR          listen address (default :9000)
  WEBHOOK_RUNNER_GITHUB_TOKEN  GitHub token for commit-status updates
  WEBHOOK_RUNNER_LOG_FORMAT    "text" (default) or "json"`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		applyServeEnv(serveOptsRoot)
		if len(args) == 1 {
			serveOptsRoot.hooksDir = args[0]
		}
		if serveOptsRoot.hooksDir == "" {
			return errors.New("hooks directory is required (positional arg or WEBHOOK_RUNNER_HOOKS_DIR)")
		}
		return runServe(cmd.Context(), serveOptsRoot)
	},
}

func init() {
	rootCmd.Flags().StringVar(&serveOptsRoot.addr, "addr", "", "listen address (default :9000, env WEBHOOK_RUNNER_ADDR)")
	rootCmd.Flags().StringVar(&serveOptsRoot.logFormat, "log-format", "", "log format: text or json (env WEBHOOK_RUNNER_LOG_FORMAT)")
}

// Execute runs the CLI. It is the only entry point main.go needs.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		// Cobra has already printed the error to stderr.
		os.Exit(1)
	}
}
