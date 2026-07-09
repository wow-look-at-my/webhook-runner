package runner

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

// The containerized-without-shared-TMPDIR topology silently breaks every
// hook run (payload bind mounts resolve on the docker host), so startup
// must flag it on the dashboard. A set TMPDIR is the deployment declaring
// the temp dir host-shared, and silences the warning.
func TestWarnIfContainerized(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	marker := filepath.Join(t.TempDir(), ".dockerenv")
	require.NoError(t, os.WriteFile(marker, nil, 0o644))

	misconfigured := func(rec *events.Recorder) bool {
		for _, e := range rec.List(0) {
			if e.Kind == "server.misconfigured" {
				return true
			}
		}
		return false
	}

	t.Run("warns when a container marker exists and TMPDIR is unset", func(t *testing.T) {
		t.Setenv("TMPDIR", "")
		os.Unsetenv("TMPDIR") // Setenv registered the restore; make it truly unset
		rec := events.NewRecorder(5)
		WarnIfContainerized(logger, rec, marker)
		assert.True(t, misconfigured(rec))
	})

	t.Run("silent when TMPDIR is set (deployment declares a shared dir)", func(t *testing.T) {
		t.Setenv("TMPDIR", "/var/lib/webhook-runner/tmp")
		rec := events.NewRecorder(5)
		WarnIfContainerized(logger, rec, marker)
		assert.False(t, misconfigured(rec))
	})

	t.Run("silent outside a container (no marker files)", func(t *testing.T) {
		t.Setenv("TMPDIR", "")
		os.Unsetenv("TMPDIR")
		rec := events.NewRecorder(5)
		WarnIfContainerized(logger, rec, filepath.Join(t.TempDir(), "absent"))
		assert.False(t, misconfigured(rec))
	})
}
