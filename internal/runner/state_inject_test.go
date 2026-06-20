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

func (f fakeKV) Token(string) string { return f.token }

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
	})
	run, err := r.Start(context.Background(), stateHook(t, dir, "stateful", true), []byte("p"), http.Header{})
	require.NoError(t, err)
	r.Wait()

	out := run.Snapshot(-1).Output
	// Reaches the KV API over a bind-mounted Unix socket — never networking.
	assert.NotContains(t, out, "arg=--network")
	assert.NotContains(t, out, "arg=--add-host=host.docker.internal:host-gateway")
	assert.Contains(t, out, "arg=/tmp/whr/whr-state.sock:/run/webhook-runner/state.sock")
	assert.Contains(t, out, "arg=HOOK_KV_SOCKET=/run/webhook-runner/state.sock")
	assert.Contains(t, out, "arg=HOOK_KV_URL=http://localhost")
	assert.Contains(t, out, "arg=HOOK_KV_TOKEN=stateful.SIG")
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
	})
	run, err := r.Start(context.Background(), stateHook(t, dir, "plain", false), []byte("p"), http.Header{})
	require.NoError(t, err)
	r.Wait()

	for _, line := range run.Snapshot(-1).Output {
		assert.NotContains(t, line, "whr-state.sock")
		assert.NotContains(t, line, "HOOK_KV_SOCKET")
		assert.NotContains(t, line, "HOOK_KV_TOKEN")
	}
}
