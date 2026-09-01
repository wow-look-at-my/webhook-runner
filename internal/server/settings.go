// The settings editor's API (admin port, behind Trust — same trust
// model as the kill switches next door in overrides.go):
//
//	GET /hooks/{id}/settings schema + manifest + effective + overrides
//	PUT /hooks/{id}/settings pin field: {"pointer":"/a/b","value":…}
//	DELETE /hooks/{id}/settings revert: ?pointer=/a/b, or all when omitted
//
// WHY THE SCHEMA IS SERVED RAW. The dashboard builds the whole form from
// settings.schema.json — types, ranges, enums, per-value descriptions. That
// is deliberate: the schema already exists, the loader already enforces it,
// and every form control derived from it is a control that cannot drift from
// what the runner will accept. The alternative — a server-side "form
// description" endpoint — is a schema to keep in sync, and the moment
// it disagrees the UI offers values the loader rejects.
//
// WHY A WRITE VALIDATES . The merged document is validated HERE, so a
// bad value is a with the schema's own message while the operator is
// looking at the field. It is validated AGAIN at load (hooks.ApplySettings-
// Overrides), because the tree can move under a stored override. Neither
// check makes the other redundant: this is about the value being typed,
// that about the value still being legal later.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// maxSettingsValueBytes bounds pinned value.
const maxSettingsValueBytes = 64 << 10

// SettingsField is pinned field as the editor sees it.
type SettingsField struct {
	Pointer string `json:"pointer"`
	// Value is what the operator pinned.
	Value json.RawMessage `json:"value"`
	// Manifest is what hook.json says at that pointer, so the editor can show what "revert" would restore.
	Manifest json.RawMessage `json:"manifest,omitempty"`
	// Stale marks exactly that case, so the UI does not have to infer it from an absent field.
	Stale bool `json:"stale,omitempty"`
}

// SettingsView is GET /hooks/{id}/settings.
type SettingsView struct {
	Hook string `json:"hook"`
	// Schema is the entity's settings.schema.json verbatim (comments stripped).
	Schema json.RawMessage `json:"schema,omitempty"`
	// Effective is the document the next run will be handed: the manifest with every accepted override already merged in.
	Effective json.RawMessage `json:"effective"`
	// Overrides are the operator's pins, sorted by pointer.
	Overrides []SettingsField `json:"overrides,omitempty"`
	// Rejected is set when this entity's overrides did not survive the last load and it is serving manifest values instead.
	Rejected string `json:"rejected,omitempty"`
}

// settingsEntity resolves an id across the namespace hooks and managers
// share, returning the underlying *Hook (a Manager embeds ).
func (s *Server) settingsEntity(id string) (*hooks.Hook, bool) {
	if h, ok := s.registry.Get(id); ok {
		return h, true
	}
	if m, ok := s.registry.GetManager(id); ok {
		return m.Hook, true
	}
	return nil, false
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h, ok := s.settingsEntity(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such hook")
		return
	}
	view, err := s.settingsView(h, id)
	if err != nil {
		// An unreadable schema is a real fault, not an empty form: the editor must never present "no configuration" for an entity whose contract.
		writeError(w, http.StatusInternalServerError, "read settings schema: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// settingsView builds the shape every settings endpoint answers with.
//
// Shared deliberately: a write used to answer a trimmed view without the
// schema, and since the editor re-renders from whatever a write returns, the
// entire form vanished the moment an operator changed their value — it
// read the missing schema as "this hook takes no configuration". A partial
// view is not a smaller version of the full , it is a different claim.
func (s *Server) settingsView(h *hooks.Hook, id string) (SettingsView, error) {
	schema, err := h.SettingsSchemaJSON()
	if err != nil {
		return SettingsView{}, err
	}
	view := SettingsView{
		Hook:      id,
		Schema:    schema,
		Effective: json.RawMessage(h.SettingsJSON()),
	}
	stored := s.overrides.SettingsOverrides(id)
	for _, ptr := range sortedOverridePointers(stored) {
		field := SettingsField{Pointer: ptr, Value: stored[ptr]}
		if manifest, ok := h.ManifestPointerValue(ptr); ok {
			field.Manifest = manifest
		} else {
			field.Stale = true
		}
		view.Overrides = append(view.Overrides, field)
	}
	// A pin the served document does not carry means the merge refused this entity's overrides at the last load.
	if reason := s.settingsRejection(h, view.Overrides); reason != "" {
		view.Rejected = reason
	}
	return view, nil
}

// settingsRejection reports why the served document does not carry the
// operator's pins, by re-deriving it: if applying them to the manifest
// succeeds, the served document already has them and nothing is wrong.
func (s *Server) settingsRejection(h *hooks.Hook, fields []SettingsField) string {
	if len(fields) == 0 {
		return ""
	}
	probe := *h
	if err := probe.ApplySettingsOverrides(s.overrides.SettingsOverrides(h.ID)); err != nil {
		return err.Error()
	}
	return ""
}

func (s *Server) handleSettingsSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h, ok := s.settingsEntity(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such hook")
		return
	}
	if s.overrides == nil {
		writeError(w, http.StatusInternalServerError, "override store not configured")
		return
	}
	var body struct {
		Pointer string          `json:"pointer"`
		Value   json.RawMessage `json:"value"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettingsValueBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, `body must be {"pointer": "/a/b", "value": <json>}: `+err.Error())
		return
	}
	if body.Pointer == "" || len(body.Value) == 0 {
		writeError(w, http.StatusBadRequest, `both "pointer" and "value" are required`)
		return
	}

	// Validate the MERGED document before storing anything.
	merged := s.overrides.SettingsOverrides(id)
	if merged == nil {
		merged = map[string]json.RawMessage{}
	}
	merged[body.Pointer] = body.Value
	probe := *h
	if err := probe.ApplySettingsOverrides(merged); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	changed, err := s.overrides.SetSettingOverride(id, body.Pointer, body.Value)
	if err != nil {
		s.overrideWriteError(w, "settings of "+id, err)
		return
	}
	if changed {
		s.log.Warn("hook setting overridden by operator", "hook", id, "pointer", body.Pointer)
		s.events.Record("settings.overridden",
			fmt.Sprintf("%s: %s overridden by operator to %s (takes effect on the next run)", id, body.Pointer, truncateForEvent(body.Value)),
			map[string]string{"hook": id})
	}
	s.applySettingsChange(w, id, changed)
}

func (s *Server) handleSettingsClear(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.settingsEntity(id); !ok {
		writeError(w, http.StatusNotFound, "no such hook")
		return
	}
	if s.overrides == nil {
		writeError(w, http.StatusInternalServerError, "override store not configured")
		return
	}
	pointer := r.URL.Query().Get("pointer")
	var (
		changed bool
		err     error
		what    string
	)
	if pointer == "" {
		changed, err = s.overrides.ClearSettingOverrides(id)
		what = "every overridden setting"
	} else {
		changed, err = s.overrides.ClearSettingOverride(id, pointer)
		what = pointer
	}
	if err != nil {
		s.overrideWriteError(w, "settings of "+id, err)
		return
	}
	if changed {
		s.log.Info("hook setting override cleared by operator", "hook", id, "pointer", pointer)
		s.events.Record("settings.reverted",
			fmt.Sprintf("%s: %s reverted to the manifest value by operator", id, what),
			map[string]string{"hook": id})
	}
	s.applySettingsChange(w, id, changed)
}

// applySettingsChange re-runs the reload closure so the merged document
// becomes what the registry serves, then answers with the fresh view.
//
// Persist , apply — the same ordering as the concurrency
// override next door: if the process dies between them the restart re-applies
// from disk. The reverse would let a live change vanish on restart.
//
// A reload failure is NOT swallowed. The override is already stored, so the
// honest report is "stored, but the fleet did not pick it up" — silence here
// would leave the operator believing a value is live when the registry is
// still serving the old document.
func (s *Server) applySettingsChange(w http.ResponseWriter, id string, changed bool) {
	if changed && s.onReload != nil {
		if err := s.onReload(); err != nil {
			s.log.Error("settings override stored but the reload failed", "hook", id, "err", err)
			s.events.Record("settings.reload_failed",
				fmt.Sprintf("%s: settings override stored but the reload failed, so the fleet is still serving the previous document: %v", id, err),
				map[string]string{"hook": id})
			writeError(w, http.StatusInternalServerError,
				"the override was stored but the reload failed, so it is not live yet: "+err.Error())
			return
		}
	}
	h, ok := s.settingsEntity(id)
	if !ok {
		// The reload dropped the entity. Say so rather than ing on a
		// write that did land.
		writeJSON(w, http.StatusOK, map[string]any{"hook": id, "changed": changed,
			"note": "the override was stored, but the entity is no longer loaded"})
		return
	}
	view, err := s.settingsView(h, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read settings schema: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// sortedOverridePointers orders the pins so the editor renders them in a stable order across refreshes — map order would reshuffle the list on every poll, which reads as the page flickering for.
func sortedOverridePointers(ptrs map[string]json.RawMessage) []string {
	out := make([]string, 0, len(ptrs))
	for ptr := range ptrs {
		out = append(out, ptr)
	}
	sort.Strings(out)
	return out
}

// truncateForEvent bounds a value in an activity-feed line.
func truncateForEvent(v json.RawMessage) string {
	const max = 120
	if len(v) <= max {
		return string(v)
	}
	return string(v[:max]) + "… (truncated)"
}
