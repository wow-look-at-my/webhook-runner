package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
)

// The settings editor's contract, from the operator's side: what they can see, what they are stopped from doing, and — the part that matters.

const editorSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["ai"],
  "properties": {
    "ai": {
      "type": "object",
      "additionalProperties": false,
      "required": ["model"],
      "properties": {
        "model": {"type": "string", "minLength": 1},
        "reasoning": {"enum": ["auto", "off"]},
        "retries": {"type": "integer", "minimum": 0, "maximum": 10}
      }
    }
  }
}`

// editManifest rewrites the hook.json settings and reloads, the way a hooks-repo push does.
type editManifest func(t *testing.T, settings string)

// settingsServer wires a server with a real override store and one hook
// whose settings.schema.json is on disk, plus the reload closure the real
// serve path installs — here it re-merges the overrides into the registry's
// hook, which is exactly what buildLoadAndApply does for the fleet.
func settingsServer(t *testing.T) (*Server, *overrides.Store, editManifest) {
	t.Helper()
	s, reg, _, _ := newTestServer(t)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.schema.json"), []byte(editorSchema), 0o644))
	manifest := `{"ai":{"model":"m","reasoning":"auto","retries":3}}`

	ov, err := overrides.Open(filepath.Join(t.TempDir(), "overrides.json"))
	require.NoError(t, err)
	s.overrides = ov

	fresh := func() *hooks.Hook {
		return &hooks.Hook{ID: "h", Description: "d", Command: []string{"x"},
			SourcePath: filepath.Join(dir, "hook.json"), Settings: json.RawMessage(manifest)}
	}
	s.onReload = func() error {
		h := fresh()
		// A rejected override leaves the manifest values in place and is NOT a load failure — the same degrade buildLoadAndApply applies.
		_ = h.ApplySettingsOverrides(ov.SettingsOverrides("h"))
		reg.Set(h)
		return nil
	}
	require.NoError(t, s.onReload())
	return s, ov, func(t *testing.T, settings string) {
		t.Helper()
		manifest = settings
		require.NoError(t, s.onReload())
	}
}

func getSettings(t *testing.T, s *Server) SettingsView {
	t.Helper()
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hooks/h/settings", nil))
	require.Equal(t, 200, rec.Code)
	var view SettingsView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	return view
}

func putSetting(s *Server, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/hooks/h/settings", strings.NewReader(body)))
	return rec
}

func deleteSetting(s *Server, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/hooks/h/settings"+query, nil))
	return rec
}

func TestSettingsGetServesSchemaAndEffectiveValues(t *testing.T) {
	s, _, _ := settingsServer(t)
	view := getSettings(t, s)

	assert.Equal(t, "h", view.Hook)
	require.NotNil(t, view.Schema, "the editor builds its whole form from this")
	assert.Contains(t, string(view.Schema), `"reasoning"`)
	assert.JSONEq(t, `{"ai":{"model":"m","reasoning":"auto","retries":3}}`, string(view.Effective))
	assert.Empty(t, view.Overrides)
	assert.Empty(t, view.Rejected)
}

func TestSettingsPutPinsAFieldAndMakesItLive(t *testing.T) {
	s, ov, _ := settingsServer(t)
	rec := putSetting(s, `{"pointer":"/ai/reasoning","value":"off"}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())

	stored := ov.SettingsOverrides("h")
	require.Len(t, stored, 1)
	assert.JSONEq(t, `"off"`, string(stored["/ai/reasoning"]))

	view := getSettings(t, s)
	assert.JSONEq(t, `{"ai":{"model":"m","reasoning":"off","retries":3}}`, string(view.Effective),
		"the reload ran, so the registry now serves the merged document")
	require.Len(t, view.Overrides, 1)
	assert.Equal(t, "/ai/reasoning", view.Overrides[0].Pointer)
	assert.JSONEq(t, `"auto"`, string(view.Overrides[0].Manifest), "revert would restore the manifest value")
	assert.False(t, view.Overrides[0].Stale)
}

// The editor re-renders from whatever a write returns, so a write MUST
// answer the same shape as a read. When it did not carry the schema, the
// whole form vanished on the operator's first change — it read the missing
// schema as "this hook takes no configuration". Caught in a browser, not
// here, which is exactly why it is pinned here now.
func TestSettingsWriteAnswersTheSameShapeAsARead(t *testing.T) {
	s, _, _ := settingsServer(t)
	for _, rec := range []*httptest.ResponseRecorder{
		putSetting(s, `{"pointer":"/ai/reasoning","value":"off"}`),
		deleteSetting(s, "?pointer=/ai/reasoning"),
	} {
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var view SettingsView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
		assert.Equal(t, "h", view.Hook)
		require.NotNil(t, view.Schema, "a write must carry the schema — the editor rebuilds the form from it")
		assert.Contains(t, string(view.Schema), `"reasoning"`)
		assert.NotEmpty(t, view.Effective)
	}
}

// A value the loader would refuse must be refused HERE, while the operator
// is looking at the field — not stored to fail silently at the next reload.
func TestSettingsPutRefusesAValueTheSchemaRejects(t *testing.T) {
	s, ov, _ := settingsServer(t)
	for _, body := range []string{
		`{"pointer":"/ai/reasoning","value":"sometimes"}`,
		`{"pointer":"/ai/retries","value":99}`,
		`{"pointer":"/ai/model","value":""}`,
	} {
		rec := putSetting(s, body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		assert.Contains(t, rec.Body.String(), "settings.schema.json", body)
	}
	assert.Nil(t, ov.SettingsOverrides("h"), "nothing was stored")
	assert.JSONEq(t, `{"ai":{"model":"m","reasoning":"auto","retries":3}}`, string(getSettings(t, s).Effective))
}

func TestSettingsPutRefusesAFieldTheManifestOmits(t *testing.T) {
	s, ov, _ := settingsServer(t)
	rec := putSetting(s, `{"pointer":"/ai/nope","value":1}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "nothing to override")
	assert.Nil(t, ov.SettingsOverrides("h"))
}

func TestSettingsPutRefusesReferenceSyntax(t *testing.T) {
	s, ov, _ := settingsServer(t)
	rec := putSetting(s, `{"pointer":"/ai/model","value":"${env:MODEL}"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "literal value")
	assert.Nil(t, ov.SettingsOverrides("h"))
}

func TestSettingsPutRejectsMalformedBodies(t *testing.T) {
	s, _, _ := settingsServer(t)
	for _, body := range []string{
		`{}`,
		`{"pointer":"/ai/model"}`,
		`{"value":"x"}`,
		`{"pointer":"/ai/model","value":"x","extra":1}`,
		`not json`,
	} {
		rec := putSetting(s, body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
	}
}

func TestSettingsDeleteRevertsOneFieldAndThenAll(t *testing.T) {
	s, ov, _ := settingsServer(t)
	require.Equal(t, 200, putSetting(s, `{"pointer":"/ai/reasoning","value":"off"}`).Code)
	require.Equal(t, 200, putSetting(s, `{"pointer":"/ai/retries","value":7}`).Code)
	require.Len(t, ov.SettingsOverrides("h"), 2)

	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/hooks/h/settings?pointer=/ai/retries", nil))
	require.Equal(t, 200, rec.Code)
	view := getSettings(t, s)
	require.Len(t, view.Overrides, 1)
	assert.JSONEq(t, `{"ai":{"model":"m","reasoning":"off","retries":3}}`, string(view.Effective),
		"the reverted field is back to the manifest value, the other pin stands")

	rec = httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/hooks/h/settings", nil))
	require.Equal(t, 200, rec.Code)
	view = getSettings(t, s)
	assert.Empty(t, view.Overrides, "no pointer means revert everything")
	assert.JSONEq(t, `{"ai":{"model":"m","reasoning":"auto","retries":3}}`, string(view.Effective))
}

// The manifest can move under a stored override. The editor has to SHOW the
// stale pin (so it can be cleared) and say why the entity is serving
// manifest values — "I set it and nothing happened" needs an answer.
func TestSettingsReportsAStalePinAndTheRejectionReason(t *testing.T) {
	s, ov, editSettings := settingsServer(t)
	require.Equal(t, 200, putSetting(s, `{"pointer":"/ai/reasoning","value":"off"}`).Code)

	// A hooks-repo push removes the pinned field from the manifest.
	editSettings(t, `{"ai":{"model":"m","retries":3}}`)

	view := getSettings(t, s)
	require.Len(t, view.Overrides, 1)
	assert.True(t, view.Overrides[0].Stale, "the pin no longer resolves in the manifest")
	assert.Empty(t, view.Overrides[0].Manifest, "there is no manifest value to revert to")
	assert.Contains(t, view.Rejected, "nothing to override",
		"the editor states why this entity is serving manifest values")

	// Clearing the stale pin is how the operator recovers.
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/hooks/h/settings?pointer=/ai/reasoning", nil))
	require.Equal(t, 200, rec.Code)
	assert.Nil(t, ov.SettingsOverrides("h"))
	assert.Empty(t, getSettings(t, s).Rejected)
}

// A stored override whose reload failed must not read as live.
func TestSettingsReloadFailureIsReportedNotSwallowed(t *testing.T) {
	s, ov, _ := settingsServer(t)
	s.onReload = func() error { return assertReloadBroken }

	rec := putSetting(s, `{"pointer":"/ai/reasoning","value":"off"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "not live yet")
	assert.Len(t, ov.SettingsOverrides("h"), 1,
		"the write DID land — the honest report is stored-but-not-live, not a pretend failure")
}

var assertReloadBroken = &reloadBrokenError{}

type reloadBrokenError struct{}

func (*reloadBrokenError) Error() string { return "tree refused" }

func TestSettingsUnknownHookIs404(t *testing.T) {
	s, _, _ := settingsServer(t)
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/hooks/nope/settings", nil),
		httptest.NewRequest(http.MethodPut, "/hooks/nope/settings", strings.NewReader(`{"pointer":"/a","value":1}`)),
		httptest.NewRequest(http.MethodDelete, "/hooks/nope/settings", nil),
	} {
		rec := httptest.NewRecorder()
		admin(s).ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code, req.Method)
	}
}

// An entity that ships no schema takes no configuration; the editor must be
// told that rather than shown an empty form it could type into.
func TestSettingsWithoutASchemaServesNoSchema(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	ov, err := overrides.Open(filepath.Join(t.TempDir(), "overrides.json"))
	require.NoError(t, err)
	s.overrides = ov
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"},
		SourcePath: filepath.Join(t.TempDir(), "hook.json")})

	view := getSettings(t, s)
	assert.Nil(t, view.Schema)
	assert.JSONEq(t, `{}`, string(view.Effective))
}

// A value big enough to be a memory sink belongs in the manifest.
func TestSettingsPutBoundsTheValueSize(t *testing.T) {
	s, _, _ := settingsServer(t)
	hugeJSON, err := json.Marshal(map[string]any{
		"pointer": "/ai/model",
		"value":   strings.Repeat("x", maxSettingsValueBytes+1),
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, putSetting(s, string(hugeJSON)).Code)
}
