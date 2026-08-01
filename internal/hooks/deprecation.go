package hooks

import (
	"fmt"
	"sort"
)

// A deprecation is a manifest field that still WORKS but is on its way out.
// It is deliberately not a load error: a field cannot be removed in one step
// across two repos that deploy independently. The runner would have to reject
// the fleet that still declares it, or the fleet would declare something the
// running binary has never heard of -- either way there is an instant where
// something cannot be served, and no rollback fixes it because the two sides
// changed at different times.
//
// So the old field keeps working for one release and every use of it is
// named: in the log, in the activity feed, and on the needs-attention
// surface. Loud and harmless, rather than silent or fatal.

// Deprecation is one entity's use of one superseded manifest field.
type Deprecation struct {
	EntityID string // hook or manager id
	Field    string // the manifest key, e.g. "env"
	Message  string // what to do instead
}

func (d Deprecation) Error() string {
	return fmt.Sprintf("%s: %s", d.EntityID, d.Message)
}

// envDeprecationMessage is the one place the advice is written.
const envDeprecationMessage = "declares the superseded `env` block; move that configuration to " +
	"`settings` (one JSON object, validated at load against the settings.schema.json shipped " +
	"next to the manifest, mounted read-only at $HOOK_SETTINGS_FILE). `env` still works in this " +
	"release and will be removed in the next one"

// Deprecations reports every superseded field this hook uses. Nil-safe, and
// empty for a clean manifest -- so the caller can range over it
// unconditionally.
func (h *Hook) Deprecations() []Deprecation {
	if h == nil || len(h.Env) == 0 {
		return nil
	}
	return []Deprecation{{EntityID: h.ID, Field: "env", Message: envDeprecationMessage}}
}

// CollectDeprecations gathers the deprecations of every loaded hook and
// manager, ordered by entity id so the log, the feed and the attention
// surface agree on ordering across reloads.
func CollectDeprecations(hooks map[string]*Hook, managers map[string]*Manager) []Deprecation {
	var out []Deprecation
	for _, h := range hooks {
		out = append(out, h.Deprecations()...)
	}
	for _, m := range managers {
		if m != nil {
			out = append(out, m.Hook.Deprecations()...)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EntityID != out[j].EntityID {
			return out[i].EntityID < out[j].EntityID
		}
		return out[i].Field < out[j].Field
	})
	return out
}
