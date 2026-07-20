package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/reloadgate"
)

// bothPorts runs a subtest against the hook and the admin handler — the
// build-identity endpoints are deliberately registered on both.
func bothPorts(t *testing.T, s *Server, fn func(t *testing.T, h http.Handler)) {
	t.Helper()
	for name, h := range map[string]http.Handler{"hook": hook(s), "admin": admin(s)} {
		t.Run(name, func(t *testing.T) { fn(t, h) })
	}
}

func TestVersionEndpoint(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	bothPorts(t, s, func(t *testing.T, h http.Handler) {
		req := httptest.NewRequest(http.MethodGet, "/version", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		var got VersionInfo
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, testVersion, got)
	})
}

func TestHealthIncludesVersion(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	bothPorts(t, s, func(t *testing.T, h http.Handler) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		var got struct {
			Status  string `json:"status"`
			Version string `json:"version"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, "ok", got.Status, "existing health field must be unchanged")
		assert.Equal(t, testVersion.Version, got.Version)
	})
}

// versionResponse decodes /version for the hooks_tree assertions. raw
// holds the hooks_tree object as a map so key ABSENCE (vs empty string)
// is assertable — the honest-rendering contract.
type versionResponse struct {
	VersionInfo
	HooksTree struct {
		State        string `json:"state"`
		ServingSHA   string `json:"serving_sha"`
		Verified     *bool  `json:"verified"`
		PendingSHA   string `json:"pending_sha"`
		PendingState string `json:"pending_state"`
		Reason       string `json:"reason"`
		Mode         string `json:"mode"`
	} `json:"hooks_tree"`
}

func getVersion(t *testing.T, h http.Handler) (versionResponse, map[string]json.RawMessage) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got versionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	var top struct {
		HooksTree map[string]json.RawMessage `json:"hooks_tree"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &top))
	require.NotNil(t, top.HooksTree, "hooks_tree must always be present")
	return got, top.HooksTree
}

func treeServer(t *testing.T, hooksRepo string, ts func() reloadgate.TreeState) *Server {
	t.Helper()
	return New(Options{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:   testVersion,
		HooksRepo: hooksRepo,
		TreeState: ts,
	})
}

// The gate has a settled last-good commit and nothing pending: /version
// reports it as "serving" — with the binary identity intact.
func TestVersionReportsServingTree(t *testing.T) {
	s := treeServer(t, "git@github.com:o/hooks.git", func() reloadgate.TreeState {
		return reloadgate.TreeState{ServingSHA: "aaaa1111", Verified: true, Context: "all-builds"}
	})
	bothPorts(t, s, func(t *testing.T, h http.Handler) {
		got, raw := getVersion(t, h)
		assert.Equal(t, testVersion, got.VersionInfo, "binary identity fields must be unchanged")
		assert.Equal(t, "serving", got.HooksTree.State)
		assert.Equal(t, "aaaa1111", got.HooksTree.ServingSHA)
		require.NotNil(t, got.HooksTree.Verified)
		assert.True(t, *got.HooksTree.Verified)
		assert.NotContains(t, raw, "pending_sha")
		assert.NotContains(t, raw, "reason")
		assert.NotContains(t, raw, "mode")
	})
}

// The gate is holding a newer commit: /version reports "held" with the
// pending sha and the hold reason, alongside what is still serving.
func TestVersionReportsHeldTree(t *testing.T) {
	for _, tc := range []struct {
		pendingState string
		wantReason   string
	}{
		{"pending", "awaiting all-builds"},
		{"failure", "all-builds failure"},
		{"error", "all-builds error"},
	} {
		t.Run(tc.pendingState, func(t *testing.T) {
			s := treeServer(t, "git@github.com:o/hooks.git", func() reloadgate.TreeState {
				return reloadgate.TreeState{
					ServingSHA:   "aaaa1111",
					Verified:     true,
					PendingSHA:   "bbbb2222",
					PendingState: tc.pendingState,
					Context:      "all-builds",
				}
			})
			bothPorts(t, s, func(t *testing.T, h http.Handler) {
				got, _ := getVersion(t, h)
				assert.Equal(t, testVersion, got.VersionInfo)
				assert.Equal(t, "held", got.HooksTree.State)
				assert.Equal(t, "aaaa1111", got.HooksTree.ServingSHA)
				assert.Equal(t, "bbbb2222", got.HooksTree.PendingSHA)
				assert.Equal(t, tc.pendingState, got.HooksTree.PendingState)
				assert.Equal(t, tc.wantReason, got.HooksTree.Reason)
			})
		})
	}
}

// A gate with no serving commit recorded (boot HEAD read failed, nothing
// settled since) renders the explicit "unknown" marker: no serving_sha
// key at all, never an ambiguous empty string.
func TestVersionUnknownTree(t *testing.T) {
	s := treeServer(t, "git@github.com:o/hooks.git", func() reloadgate.TreeState {
		return reloadgate.TreeState{Context: "all-builds"}
	})
	bothPorts(t, s, func(t *testing.T, h http.Handler) {
		got, raw := getVersion(t, h)
		assert.Equal(t, testVersion, got.VersionInfo, "binary identity fields must be unchanged")
		assert.Equal(t, "unknown", got.HooksTree.State)
		assert.NotContains(t, raw, "serving_sha", "unknown must omit the key, not send an empty string")
		require.NotNil(t, got.HooksTree.Verified)
		assert.False(t, *got.HooksTree.Verified)
	})
}

// No gate tracks the tree (nil TreeState): /version names the mode
// instead — the legacy gate-disabled flow when a hooks repo is
// configured, the local hooks dir otherwise — and omits verified.
func TestVersionUntrackedTree(t *testing.T) {
	for _, tc := range []struct {
		name, hooksRepo, wantMode string
	}{
		{"gate-disabled", "git@github.com:o/hooks.git", "ci gate disabled (legacy reload)"},
		{"no-hooks-repo", "", "local hooks dir (no hooks repo)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := treeServer(t, tc.hooksRepo, nil)
			bothPorts(t, s, func(t *testing.T, h http.Handler) {
				got, raw := getVersion(t, h)
				assert.Equal(t, testVersion, got.VersionInfo)
				assert.Equal(t, "untracked", got.HooksTree.State)
				assert.Equal(t, tc.wantMode, got.HooksTree.Mode)
				assert.NotContains(t, raw, "serving_sha")
				assert.NotContains(t, raw, "verified")
				assert.NotContains(t, raw, "pending_sha")
			})
		})
	}
}

// An embedding caller that sets no Version still reports something ("dev",
// the version command's own fallback) rather than an empty string.
func TestVersionDefaultsToDev(t *testing.T) {
	s := New(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got VersionInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "dev", got.Version)
	assert.Empty(t, got.Revision)
}
