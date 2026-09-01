package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// A hook that declares no timeout (an uncapped run) must not translate into
// a -length synchronous hold — time.After() would degrade every
// ?wait=true request to a instantly. The HTTP hold falls back to
// defaultSyncHold; the run itself stays uncapped, and ?timeout= still wins.
func TestParseWaitParamsUncappedHookFallsBackToDefaultHold(t *testing.T) {
	sync, hold, err := parseWaitParams(
		httptest.NewRequest(http.MethodPost, "/hook/h?wait=true", nil),
		&hooks.Hook{ID: "h"})
	require.NoError(t, err)
	assert.True(t, sync)
	assert.Equal(t, defaultSyncHold, hold)
}

// A hook with its own timeout still uses it as the sync hold, and an
// explicit ?timeout= overrides either.
func TestParseWaitParamsHookTimeoutAndOverride(t *testing.T) {
	h := &hooks.Hook{ID: "h", TimeoutRaw: "90s"}

	_, hold, err := parseWaitParams(
		httptest.NewRequest(http.MethodPost, "/hook/h?wait=true", nil), h)
	require.NoError(t, err)
	assert.Equal(t, "1m30s", hold.String())

	_, hold, err = parseWaitParams(
		httptest.NewRequest(http.MethodPost, "/hook/h?wait=true&timeout=10s", nil), h)
	require.NoError(t, err)
	assert.Equal(t, "10s", hold.String())
}
