package runner

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

type fakeKV struct{ token string }

func (f fakeKV) Token(string, string) string { return f.token }

func stateHook(t *testing.T, dir, id string, state bool) *hooks.Hook {
	t.Helper()
	hookDir := filepath.Join(dir, id)
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	return &hooks.Hook{
		ID:         id,
		SourcePath: filepath.Join(hookDir, "hook.json"),
		Command:    []string{"x"},
		State:      state,
	}
}

func TestRunnerInjectsStateEnv(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{
		Tracker:  runs.NewTracker(),
		Logger:   newSilentLogger(),
		TmpDir:   dir,
		Docker:   writeArgDumpDocker(t, dir),
		KV:       fakeKV{token: "stateful.SIG"},
		KVSocket: "/tmp/whr/whr-state.sock",
		KVShim:   "/tmp/whr/whr-shim",
	})
	run, err := r.Start(context.Background(), stateHook(t, dir, "stateful", true), []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	out := run.Snapshot(-1).Output
	// Reaches the KV API at a plain localhost URL via the injected proxy shim — no networking.
	assert.NotContains(t, out, "arg=--network")
	assert.Contains(t, out, "arg=--entrypoint")
	assert.Contains(t, out, "arg=/run/webhook-runner/whr-shim")
	assert.Contains(t, out, "arg=/tmp/whr/whr-shim:/run/webhook-runner/whr-shim:ro")
	assert.Contains(t, out, "arg=/tmp/whr/whr-state.sock:/run/webhook-runner/state.sock")
	assert.Contains(t, out, "arg=HOOK_KV_URL=http://localhost:9002")
	assert.Contains(t, out, "arg=HOOK_KV_TOKEN=stateful.SIG")
	assert.Contains(t, out, "arg=kv-forward")
}

func TestRunnerSkipsStateEnvWhenNotOptedIn(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{
		Tracker:  runs.NewTracker(),
		Logger:   newSilentLogger(),
		TmpDir:   dir,
		Docker:   writeArgDumpDocker(t, dir),
		KV:       fakeKV{token: "plain.SIG"},
		KVSocket: "/tmp/whr/whr-state.sock",
		KVShim:   "/tmp/whr/whr-shim",
	})
	run, err := r.Start(context.Background(), stateHook(t, dir, "plain", false), []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	for _, line := range run.Snapshot(-1).Output {
		assert.NotContains(t, line, "kv-forward")
		assert.NotContains(t, line, "whr-shim")
		assert.NotContains(t, line, "HOOK_KV_TOKEN")
	}
}

func TestImageCommandReconstructs(t *testing.T) {
	dir := t.TempDir()
	// A docker mock whose `inspect` prints an image's Entrypoint then Cmd as JSON arrays (the format imageCommand asks for).
	docker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = inspect ]; then printf '%s\\n%s\\n' '[\"node\"]' '[\"app.js\"]'; exit 0; fi\nexit 0\n"
	require.NoError(t, os.WriteFile(docker, []byte(script), 0o755))

	// No hook command -> entrypoint + cmd.
	argv, err := imageCommand(docker, "img", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"node", "app.js"}, argv)

	// Hook command overrides cmd but keeps entrypoint.
	argv, err = imageCommand(docker, "img", []string{"other.js"})
	require.NoError(t, err)
	assert.Equal(t, []string{"node", "other.js"}, argv)
}
