package hooks

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

func eventually(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestWatcherInitialLoad(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, "first", `{"image":"alpine","command":["x"]}`)

	reg := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { _ = Watch(ctx, root, reg, logger) }()

	eventually(t, 2*time.Second, func() bool {
		_, ok := reg.Get("first")
		return ok
	})
}

func TestWatcherDetectsNewHook(t *testing.T) {
	root := t.TempDir()
	reg := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { _ = Watch(ctx, root, reg, logger) }()

	// Wait for initial scan to finish.
	time.Sleep(100 * time.Millisecond)

	writeHook(t, root, "later", `{"image":"alpine","command":["x"]}`)

	eventually(t, 3*time.Second, func() bool {
		_, ok := reg.Get("later")
		return ok
	})
}

func TestWatcherDetectsRemoval(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, "doomed", `{"image":"alpine","command":["x"]}`)

	reg := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { _ = Watch(ctx, root, reg, logger) }()

	eventually(t, 2*time.Second, func() bool {
		_, ok := reg.Get("doomed")
		return ok
	})

	require.NoError(t, os.RemoveAll(filepath.Join(root, "doomed")))

	eventually(t, 3*time.Second, func() bool {
		_, ok := reg.Get("doomed")
		return !ok
	})
}

func TestWatcherDetectsModification(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, "h", `{"image":"alpine","command":["x"],"description":"v1"}`)

	reg := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { _ = Watch(ctx, root, reg, logger) }()

	eventually(t, 2*time.Second, func() bool {
		h, ok := reg.Get("h")
		return ok && h.Description == "v1"
	})

	require.NoError(t, os.WriteFile(
		filepath.Join(root, "h", "hook.json"),
		[]byte(`{"image":"alpine","command":["x"],"description":"v2"}`),
		0o644,
	))

	eventually(t, 3*time.Second, func() bool {
		h, ok := reg.Get("h")
		return ok && h.Description == "v2"
	})
}

func TestWatcherErrorOnMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	reg := NewRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := Watch(context.Background(), missing, reg, logger)
	assert.Error(t, err)
}
