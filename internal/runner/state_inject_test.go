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
		Tracker:     runs.NewTracker(),
		Logger:      newSilentLogger(),
		TmpDir:      dir,
		Docker:      writeArgDumpDocker(t, dir),
		KV:          fakeKV{token: "stateful.SIG"},
		KVAdvertise: "http://host.docker.internal:9002",
	})
	run, err := r.Start(context.Background(), stateHook(t, dir, "stateful", true), []byte("p"), http.Header{})
	require.NoError(t, err)
	r.Wait()

	out := run.Snapshot(-1).Output
	assert.Contains(t, out, "arg=--add-host=host.docker.internal:host-gateway")
	assert.Contains(t, out, "arg=HOOK_KV_URL=http://host.docker.internal:9002")
	assert.Contains(t, out, "arg=HOOK_KV_TOKEN=stateful.SIG")
}

func TestRunnerSkipsStateEnvWhenNotOptedIn(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{
		Tracker:     runs.NewTracker(),
		Logger:      newSilentLogger(),
		TmpDir:      dir,
		Docker:      writeArgDumpDocker(t, dir),
		KV:          fakeKV{token: "plain.SIG"},
		KVAdvertise: "http://host.docker.internal:9002",
	})
	run, err := r.Start(context.Background(), stateHook(t, dir, "plain", false), []byte("p"), http.Header{})
	require.NoError(t, err)
	r.Wait()

	for _, line := range run.Snapshot(-1).Output {
		assert.NotContains(t, line, "host-gateway")
		assert.NotContains(t, line, "HOOK_KV_URL")
		assert.NotContains(t, line, "HOOK_KV_TOKEN")
	}
}
