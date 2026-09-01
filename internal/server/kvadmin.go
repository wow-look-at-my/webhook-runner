// Admin KV inspection: GET /kv/{namespace} and GET /kv/{namespace}/{key}.
//
// Unlike the bare /kv stats and /hooks/{id} — which stay value-free — these
// routes DO return stored keys and, level down, stored values. That is a
// deliberate reversal of the store's original "never values" stance, made at
// the operator's explicit request: debugging a state-backed hook (pr-minder's
// dedup markers, revive SHAs, describe hashes) means reading what it actually
// stored, and the admin port is operator-only behind Trust. Values still
// never appear on the public hook port.
package server

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/wow-look-at-my/webhook-runner/internal/kv"
)

// kvNamespaceView is the GET /kv/{namespace} response: namespace's keys with per-key metadata (name, size, expiry), values more.
type kvNamespaceView struct {
	Namespace string       `json:"namespace"`
	Keys      []kv.KeyInfo `json:"keys"`
}

// handleKVNamespace lists namespace's keys (admin port): name, value size, and — for keys with a TTL — the absolute expiry plus remaining seconds. Sorted by key; ?prefix= narrows it (same cheap semantics as the store's List). A namespace with no data returns an empty list, matching the state API's list behavior; namespace == hook ID.
func (s *Server) handleKVNamespace(w http.ResponseWriter, r *http.Request) {
	if s.kv == nil {
		writeError(w, http.StatusServiceUnavailable, "state store not configured")
		return
	}
	ns := r.PathValue("namespace")
	writeJSON(w, http.StatusOK, kvNamespaceView{
		Namespace: ns,
		Keys:      s.kv.Keys(ns, r.URL.Query().Get("prefix")),
	})
}

// kvEntryView is the GET /kv/{namespace}/{key} response: the key's metadata plus its value.
type kvEntryView struct {
	Namespace   string     `json:"namespace"`
	Key         string     `json:"key"`
	Size        int        `json:"size"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	TTLSeconds  *int64     `json:"ttl_seconds,omitempty"`
	ValueBase64 string     `json:"value_base64"`
	ValueUTF8   *string    `json:"value_utf8,omitempty"`
}

// handleKVEntry reads entry, value included (admin port). Absent and
// expired keys are equally — GetEntry applies the same lazy-expiry rule
// as the state API's Get, so this can never show a ghost the hook itself
// would not see. Keys are path segment: URL-encode them (%F for "/",
// % for "#" — e.g. pr-minder-style "pr:{owner}/{repo}#{num}" markers).
func (s *Server) handleKVEntry(w http.ResponseWriter, r *http.Request) {
	if s.kv == nil {
		writeError(w, http.StatusServiceUnavailable, "state store not configured")
		return
	}
	ns, key := r.PathValue("namespace"), r.PathValue("key")
	e, ok := s.kv.GetEntry(ns, key)
	if !ok {
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("no such key %q in namespace %q (absent or expired)", key, ns))
		return
	}
	view := kvEntryView{
		Namespace:   ns,
		Key:         e.Key,
		Size:        e.Size,
		ExpiresAt:   e.ExpiresAt,
		TTLSeconds:  e.TTLSeconds,
		ValueBase64: base64.StdEncoding.EncodeToString(e.Value),
	}
	if utf8.Valid(e.Value) {
		v := string(e.Value)
		view.ValueUTF8 = &v
	}
	writeJSON(w, http.StatusOK, view)
}
