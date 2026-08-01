package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wow-look-at-my/webhook-runner/internal/githubstatus"
	"github.com/wow-look-at-my/webhook-runner/internal/secretserver"
)

// newGitHubStatusClient builds the runner's own GitHub client from whichever
// credential the deployment configured.
//
// Two sources, in precedence order:
//
//  1. WEBHOOK_RUNNER_GITHUB_TOKEN -- the credential pasted straight into the
//     environment. Explicit beats derived, so it wins when both are set.
//  2. WEBHOOK_RUNNER_SECRET_SERVER_TOKEN -- an sst_ machine token for
//     secret-server, from which the client reads PRIVATE_ORG_REPO_READ (or
//     WEBHOOK_RUNNER_GITHUB_TOKEN_SECRET) per call.
//
// The second exists because the first is provisioning by hand, and hand
// provisioning silently does not happen: a deployment that never received the
// variable answers every reconciliation poll with "no GitHub token configured
// ... serving <old sha>" and holds deploys until a human notices. The org
// already keeps that credential in secret-server; this lets the runner go get
// it.
//
// A misconfigured machine token is a STARTUP ERROR, not a warning. The whole
// point of pointing the runner at secret-server is to stop shipping a
// deployment that looks healthy and cannot read a commit status -- so a token
// that is malformed (wrong prefix, empty) fails the boot with the reason. What
// is deliberately NOT fatal is secret-server being unreachable right now: that
// resolves per call, so an outage during a restart heals on the next poll
// instead of leaving the process blind until someone restarts it again.
func newGitHubStatusClient(o *serveOptions, logger *slog.Logger) (*githubstatus.Client, error) {
	if o.ghToken != "" {
		if o.secretServerTok != "" {
			logger.Info("github credential: using WEBHOOK_RUNNER_GITHUB_TOKEN (set explicitly; secret-server is configured but not consulted)")
		}
		return githubstatus.New(o.ghToken, logger), nil
	}

	if o.secretServerTok == "" {
		// Neither source configured: the historical no-token client, whose
		// posts are no-ops and whose reads fail closed with a message naming
		// both ways to fix it.
		return githubstatus.New("", logger), nil
	}

	client, err := secretserver.New(o.secretServerURL, o.secretServerTok)
	if err != nil {
		return nil, fmt.Errorf("WEBHOOK_RUNNER_SECRET_SERVER_TOKEN: %w", err)
	}
	provider := secretserver.NewProvider(client, 0)
	logger.Info("github credential: reading from secret-server",
		"url", o.secretServerURL, "secret", o.ghTokenSecret)

	return githubstatus.NewFromSource(func(ctx context.Context) (string, error) {
		return provider.Secret(ctx, o.ghTokenSecret)
	}, logger), nil
}
