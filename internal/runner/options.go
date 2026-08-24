package runner

// Runner construction: the Options surface and its defaults, split from
// runner.go for the 750-line cap.

import (
	"log/slog"
	"os"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// Options configure a Runner.
type Options struct {
	Tracker  *runs.Tracker
	Logger   *slog.Logger
	TmpDir   string // directory for payload/header temp files; "" = os.TempDir()
	OnStart  HookStartedFunc
	OnFinish HookFinishedFunc
	Docker   string // docker binary path; "" = "docker"

	// UsernsRemapped is what the daemon reported at startup: whether it maps
	// container root to an unprivileged host uid. It gates the seccomp.userns
	// opt-in, which has to clear docker's masked and read-only /proc paths
	// before bubblewrap can mount a procfs -- safe under remap, host code
	// execution without it. Boot-scoped: a daemon does not gain remap under a
	// running process. false is the safe default, so a caller that never asks
	// simply cannot hand a container an unmasked /proc.
	UsernsRemapped bool

	// ScratchDir is the host directory under which each run declaring
	// hook.json `scratch` paths gets its own subtree, bind-mounted over
	// those paths so the writes miss docker's data-root. "" = unconfigured,
	// which FAILS any run whose hook declares scratch paths. See scratch.go.
	ScratchDir string

	Secrets *hooks.SecretsLoader // per-hook sops secrets; nil disables decryption
	Events  *events.Recorder     // activity feed for the dashboard; nil drops events
	Groups  *concurrency.Manager // named concurrency groups; nil = no group is declared

	// GlobalCap bounds how many hook executions run containers at once,
	// across ALL hooks (excess runs queue as pending). nil = no cap. See
	// concurrency.Global; serve always wires one (default 64).
	GlobalCap *concurrency.Global

	// KV mints per-hook state tokens; KVSocket is the host path of the KV
	// API's Unix socket and KVShim is the host path of webhook-runner's own
	// binary (the in-container proxy entrypoint), both bind-mounted into
	// state-hook containers. KV nil or either path empty disables KV injection.
	KV       KVInjector
	KVSocket string
	KVShim   string
}

// New constructs a Runner.
func New(opts Options) *Runner {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Docker == "" {
		opts.Docker = "docker"
	}
	if opts.TmpDir == "" {
		opts.TmpDir = os.TempDir()
	}
	return &Runner{
		tracker:        opts.Tracker,
		log:            opts.Logger,
		tmpDir:         opts.TmpDir,
		onStart:        opts.OnStart,
		onFinish:       opts.OnFinish,
		secrets:        opts.Secrets,
		events:         opts.Events,
		groups:         opts.Groups,
		globalCap:      opts.GlobalCap,
		kv:             opts.KV,
		kvSocket:       opts.KVSocket,
		kvShim:         opts.KVShim,
		dockerBin:      opts.Docker,
		usernsRemapped: opts.UsernsRemapped,

		scratchDir: opts.ScratchDir,
	}
}
