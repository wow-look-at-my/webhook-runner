package cli

// Unit tests for the CLI plumbing that doesn't need a running server: env
// option parsing, the validate/test/version subcommands, and the small
// serve.go helpers.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

func findCommand(t *testing.T, name string) *cobra.Command {
	t.Helper()
	for _, c := range rootCmd.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("command %q not registered", name)
	return nil
}

func TestApplyServeEnvDefaults(t *testing.T) {
	for _, k := range []string{
		"WEBHOOK_RUNNER_ADDR", "WEBHOOK_RUNNER_ADMIN_ADDR", "WEBHOOK_RUNNER_HOOKS_DIR",
		"WEBHOOK_RUNNER_DATA_DIR", "WEBHOOK_RUNNER_STATE_SOCKET", "WEBHOOK_RUNNER_STATE_SECRET",
		"WEBHOOK_RUNNER_RUN_RETENTION", "WEBHOOK_RUNNER_RUN_RETENTION_MAX",
		"WEBHOOK_RUNNER_LOG_FORMAT", "WEBHOOK_RUNNER_GITHUB_TOKEN", "WEBHOOK_RUNNER_HOOKS_REPO",
		"WEBHOOK_RUNNER_HOOKS_BRANCH", "WEBHOOK_RUNNER_HOOKS_REPO_SECRET", "WEBHOOK_RUNNER_HOOK_BASE_URL",
	} {
		t.Setenv(k, "")
	}
	o := &serveOptions{}
	require.NoError(t, applyServeEnv(o))
	assert.Equal(t, ":9000", o.addr)
	assert.Equal(t, ":9001", o.adminAddr)
	assert.Equal(t, "text", o.logFormat)
	assert.Zero(t, o.runRetention)
	assert.Zero(t, o.runRetentionMax)
	assert.Empty(t, o.hooksDir)
	assert.Equal(t, time.Hour, o.reloadPollInterval, "reconciliation poll defaults to hourly")
}

func TestApplyServeEnvReadsEnvironment(t *testing.T) {
	t.Setenv("WEBHOOK_RUNNER_ADDR", ":1900")
	t.Setenv("WEBHOOK_RUNNER_ADMIN_ADDR", ":1901")
	t.Setenv("WEBHOOK_RUNNER_HOOKS_DIR", "/hooks")
	t.Setenv("WEBHOOK_RUNNER_DATA_DIR", "/data")
	t.Setenv("WEBHOOK_RUNNER_STATE_SOCKET", "/tmp/s.sock")
	t.Setenv("WEBHOOK_RUNNER_STATE_SECRET", "sec")
	t.Setenv("WEBHOOK_RUNNER_RUN_RETENTION", "72h")
	t.Setenv("WEBHOOK_RUNNER_RUN_RETENTION_MAX", "123")
	t.Setenv("WEBHOOK_RUNNER_LOG_FORMAT", "json")
	t.Setenv("WEBHOOK_RUNNER_GITHUB_TOKEN", "tok")
	t.Setenv("WEBHOOK_RUNNER_HOOKS_REPO", "git@example.com:x/y.git")
	t.Setenv("WEBHOOK_RUNNER_HOOKS_BRANCH", "main")
	t.Setenv("WEBHOOK_RUNNER_HOOKS_REPO_SECRET", "hmac")
	t.Setenv("WEBHOOK_RUNNER_HOOK_BASE_URL", "https://hooks.example.com")

	o := &serveOptions{}
	require.NoError(t, applyServeEnv(o))
	assert.Equal(t, ":1900", o.addr)
	assert.Equal(t, ":1901", o.adminAddr)
	assert.Equal(t, "/hooks", o.hooksDir)
	assert.Equal(t, "/data", o.dataDir)
	assert.Equal(t, "/tmp/s.sock", o.stateSocket)
	assert.Equal(t, "sec", o.stateSecret)
	assert.Equal(t, 72*time.Hour, o.runRetention)
	assert.Equal(t, 123, o.runRetentionMax)
	assert.Equal(t, "json", o.logFormat)
	assert.Equal(t, "tok", o.ghToken)
	assert.Equal(t, "git@example.com:x/y.git", o.hooksRepo)
	assert.Equal(t, "main", o.hooksBranch)
	assert.Equal(t, "hmac", o.hooksRepoSecret)
	assert.Equal(t, "https://hooks.example.com", o.hookBaseURL)

	// Invalid numeric/duration values fall back to the built-in defaults.
	t.Setenv("WEBHOOK_RUNNER_RUN_RETENTION", "soon")
	t.Setenv("WEBHOOK_RUNNER_RUN_RETENTION_MAX", "-1")
	o2 := &serveOptions{}
	require.NoError(t, applyServeEnv(o2))
	assert.Zero(t, o2.runRetention)
	assert.Zero(t, o2.runRetentionMax)
}

func TestApplyServeEnvReloadPollInterval(t *testing.T) {
	parse := func(v string) (*serveOptions, error) {
		t.Setenv("WEBHOOK_RUNNER_RELOAD_POLL_INTERVAL", v)
		o := &serveOptions{}
		return o, applyServeEnv(o)
	}

	o, err := parse("30m")
	require.NoError(t, err)
	assert.Equal(t, 30*time.Minute, o.reloadPollInterval)

	// An explicit 0 disables the reconciliation poll.
	o, err = parse("0")
	require.NoError(t, err)
	assert.Zero(t, o.reloadPollInterval)

	// Empty behaves like unset: the hourly default.
	o, err = parse("")
	require.NoError(t, err)
	assert.Equal(t, time.Hour, o.reloadPollInterval)

	// Unlike the fall-back-quietly options, an unparseable or negative value FAILS startup — a typo must not silently change deploy latency.
	_, err = parse("soonish")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WEBHOOK_RUNNER_RELOAD_POLL_INTERVAL")
	_, err = parse("-5m")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be >= 0")
}

func TestApplyServeEnvMaxConcurrentRuns(t *testing.T) {
	parse := func(v string) (*serveOptions, error) {
		t.Setenv("WEBHOOK_RUNNER_MAX_CONCURRENT_RUNS", v)
		o := &serveOptions{}
		return o, applyServeEnv(o)
	}

	// Unset (empty) means the built-in default 64.
	o, err := parse("")
	require.NoError(t, err)
	assert.Equal(t, concurrency.DefaultGlobalLimit, o.maxConcurrentRuns)
	assert.Equal(t, 64, o.maxConcurrentRuns)

	o, err = parse("128")
	require.NoError(t, err)
	assert.Equal(t, 128, o.maxConcurrentRuns)

	// A set-but-invalid cap FAILS startup (the reload-poll rule): a typo must not silently fall back and mask a deliberately tightened limit.
	_, err = parse("lots")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WEBHOOK_RUNNER_MAX_CONCURRENT_RUNS")
	_, err = parse("0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be >= 1")
	_, err = parse("-2")
	require.Error(t, err)
}

func TestValidateCommand(t *testing.T) {
	cmd := findCommand(t, "validate")

	// Happy path: one group, one hook in it, one unbounded hook.
	root := t.TempDir()
	writeConcurrencyJSON(t, root, `{"groups":{"g":{"limit":2}}}`)
	writeTestHook(t, root, "plain")
	dir := filepath.Join(root, "grouped")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook.json"),
		[]byte(`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","command":["x"],"concurrency_group":"g"}`), 0o644))

	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	require.NoError(t, cmd.RunE(cmd, []string{root}))
	assert.Contains(t, out.String(), "group g (limit 2)")
	assert.Contains(t, out.String(), "ok  plain")
	assert.Contains(t, out.String(), "ok  grouped")
	assert.Contains(t, out.String(), "[group: g]")
	assert.Contains(t, out.String(), "2 hook(s) validated")

	// A hook referencing an undeclared group fails validation.
	bad := t.TempDir()
	badDir := filepath.Join(bad, "orphan")
	require.NoError(t, os.MkdirAll(badDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(badDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(badDir, "hook.json"),
		[]byte(`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","command":["x"],"concurrency_group":"nope"}`), 0o644))
	out.Reset()
	errOut.Reset()
	require.Error(t, cmd.RunE(cmd, []string{bad}))
	assert.Contains(t, errOut.String(), "undeclared concurrency group")

	// A MIXED layout — a stray top-level hook alongside src/hooks/ — must turn validate RED with a clear message (the failsafe for an incomplete.
	mixed := t.TempDir()
	srcHook := filepath.Join(mixed, "src", "hooks", "alpha")
	require.NoError(t, os.MkdirAll(srcHook, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcHook, "Dockerfile"), []byte("FROM alpine\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcHook, "hook.json"),
		[]byte(`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","command":["x"]}`), 0o644))
	writeTestHook(t, mixed, "leftover") // a top-level hook dir left behind
	out.Reset()
	errOut.Reset()
	require.Error(t, cmd.RunE(cmd, []string{mixed}), "mixed layout must fail validate")
	assert.Contains(t, errOut.String(), "mixed hook layout")
	assert.Contains(t, errOut.String(), "leftover")
	assert.Contains(t, errOut.String(), "hard error")
	assert.Contains(t, out.String(), "ok  alpha", "the src hook still validates; only the stray top-level dir is rejected")
}

func TestTestCommand(t *testing.T) {
	cmd := findCommand(t, "test")
	docker := filepath.Join(t.TempDir(), "docker")
	require.NoError(t, os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, cmd.Flags().Set("docker", docker))
	defer func() { _ = cmd.Flags().Set("docker", "") }()

	// No hooks declare tests: reported, not an error.
	root := t.TempDir()
	writeTestHook(t, root, "untested")
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	require.NoError(t, cmd.RunE(cmd, []string{root}))
	assert.Contains(t, out.String(), "no hooks declare tests")

	// A hook with tests runs them in its (mock-docker) image.
	dir := filepath.Join(root, "tested")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook.json"),
		[]byte(`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","command":["x"],"tests":[["true"]]}`), 0o644))
	out.Reset()
	require.NoError(t, cmd.RunE(cmd, []string{root}))
	assert.Contains(t, out.String(), "1 test command(s) passed across 1 hook(s)")

	// --hook naming an unknown hook is an error.
	require.NoError(t, cmd.Flags().Set("hook", "missing"))
	require.Error(t, cmd.RunE(cmd, []string{root}))
}

func TestVersionCommand(t *testing.T) {
	cmd := findCommand(t, "version")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.Run(cmd, nil)
	assert.NotEmpty(t, out.String())
	assert.Equal(t, out.String(), versionString()+"\n")
}

func TestNewLogger(t *testing.T) {
	assert.NotNil(t, newLogger("json"))
	assert.NotNil(t, newLogger("text"))
	assert.NotNil(t, newLogger(""))
}

func TestBuildReloadFuncWithoutRepo(t *testing.T) {
	called := 0
	fn := buildReloadFunc(nil, func() error { called++; return nil }, events.NewRecorder(10))
	require.NoError(t, fn())
	assert.Equal(t, 1, called)
}

func TestCopyExecutable(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "shim")
	require.NoError(t, copyExecutable(dst))
	fi, err := os.Stat(dst)
	require.NoError(t, err)
	assert.NotZero(t, fi.Size())
	assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm())

	// Destination in a directory that doesn't exist fails.
	require.Error(t, copyExecutable(filepath.Join(t.TempDir(), "missing", "shim")))
}

func TestSchedulePayloadAndHeaders(t *testing.T) {
	p := schedulePayload("h1")
	assert.Contains(t, string(p), `"trigger":"schedule"`)
	assert.Contains(t, string(p), `"hook":"h1"`)
	h := scheduleHeaders("h1")
	assert.Equal(t, "h1", h.Get("X-Webhook-Runner-Schedule"))
	assert.Equal(t, "application/json", h.Get("Content-Type"))
}
