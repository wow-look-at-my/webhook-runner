// Package attention aggregates the server's ACTIVE misconfigurations into
// one current problem set for the admin dashboard — the persistent
// "needs attention" surface behind GET /attention and the red banner.
//
// The activity feed already announces every misconfiguration as it happens,
// but events scroll away; this package holds the CURRENT set, and every
// entry has an explicit lifecycle:
//
//   - STATE-DERIVED sources re-derive from scratch on every hooks reload
//     (ReplaceSource): dropped hooks ("load"), the zero-hooks guard
//     ("zero-hooks"), and the serve-time static probe of ${NAME}
//     api_key/env references + sops decrypt ("secrets"). Their clear rule
//     is structural: an entry vanishes on the first reload where the
//     underlying problem is gone.
//   - The BOOT-SCOPED "server" source holds verdicts computed once at
//     startup (today: the containerized-without-TMPDIR hazard). A running
//     process's environment cannot change, so these cannot clear without a
//     restart — documented per entry.
//   - EVENT-DERIVED entries ("event") come from recognized activity-event
//     kinds via registered rules (the seam for hook-emitted signals — see
//     RegisterStandardEventRules). Each rule documents the clear rule for
//     the entries it creates; every rule's clear condition is one that can
//     actually fire.
//
// Entry identity is (source, hook, key). Since is the first time the
// problem became active and is PRESERVED across re-derivations while the
// same identity persists (the message may update in place); it resets only
// when the entry clears and later recurs. Everything is in-memory — the
// events/request-log stance: a restart re-derives the state-sourced
// entries at the boot load and loses event-derived ones until their events
// recur.
package attention

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Sources — the derivation family of an entry, part of its identity.
const (
	// SourceLoad: a hook (or the hooks tree / concurrency.json) failed to
	// load or validate and was DROPPED. Re-derived per reload; clears on
	// the first reload where it loads (or is removed).
	SourceLoad = "load"
	// SourceZeroHooks: the loader found NO hooks at all (hooks.ZeroHooksError
	// — the fleet-offline guard). Clears on the first reload that loads one.
	SourceZeroHooks = "zero-hooks"
	// SourceSecrets: the serve-time static probe — unresolvable ${NAME}
	// api_key/env references and sops decrypt failures on LOADED hooks.
	// Re-probed per reload; clears when the reference resolves, the
	// secrets file decrypts, or the hook goes away.
	SourceSecrets = "secrets"
	// SourceServer: boot-scoped server-topology verdicts (the containerized
	// TMPDIR hazard). Cannot clear without a restart.
	SourceServer = "server"
	// SourceEvent: entries derived from recognized activity-event kinds
	// via registered rules. Clear rules are per-rule — see
	// RegisterStandardEventRules.
	SourceEvent = "event"
)

// Entry is one active problem. Identity is (Source, Hook, Key); Message is
// display text and may update in place without resetting Since. Messages
// must be VALUE-FREE: name the hook, the reference (${NAME}), the file —
// never a resolved secret value.
type Entry struct {
	Source  string    `json:"source"`
	Hook    string    `json:"hook,omitempty"`
	Key     string    `json:"key"`
	Message string    `json:"message"`
	Since   time.Time `json:"since"`
}

func (e Entry) identity() string {
	return e.Source + "\x00" + e.Hook + "\x00" + e.Key
}

// Resolution names entries an event rule clears: every entry under
// (Source, Hook) whose Key starts with KeyPrefix ("" matches all keys for
// that source+hook).
type Resolution struct {
	Source    string
	Hook      string
	KeyPrefix string
}

// RuleFunc maps one recorded activity event (its hook field and message)
// onto attention mutations: entries to report (added, or refreshed in
// place if the identity is already active) and resolutions to clear.
type RuleFunc func(hook, message string) (report []Entry, resolve []Resolution)

// Aggregator is the concurrency-safe current problem set. A nil
// *Aggregator is valid and drops/answers-empty everywhere, so callers
// never need to nil-check (the events.Recorder convention).
type Aggregator struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]Entry
	rules   map[string][]RuleFunc

	// onChange, when set, is invoked after any mutation that actually
	// changed the set (added, removed, or reworded an entry) —
	// synchronously on the mutating goroutine, under the aggregator mutex,
	// so keep it trivial: it must be fast, never block, and never call
	// back into the Aggregator (the SetOnChange convention; the server
	// wires it to the stream hub's "attention" section signal).
	onChange func()
}

// New returns an empty aggregator.
func New() *Aggregator {
	return &Aggregator{
		now:     time.Now,
		entries: map[string]Entry{},
		rules:   map[string][]RuleFunc{},
	}
}

// SetOnChange registers fn to run after every real mutation. Set once at
// wiring time, before concurrent use. Nil-receiver safe; nil fn disables.
func (a *Aggregator) SetOnChange(fn func()) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.onChange = fn
	a.mu.Unlock()
}

// ReplaceSource atomically replaces every entry of one source with the
// given set — the state-derived re-derivation path, called on each reload.
// Entries whose identity persists KEEP their Since (the problem never
// stopped being active); new identities are stamped now; identities absent
// from the new set clear. Source fields on the inputs are overwritten with
// source so a caller can't cross the streams.
func (a *Aggregator) ReplaceSource(source string, entries []Entry) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := false
	next := map[string]Entry{}
	for _, e := range entries {
		e.Source = source
		next[e.identity()] = e
	}
	// Remove cleared identities.
	for id, e := range a.entries {
		if e.Source != source {
			continue
		}
		if _, still := next[id]; !still {
			delete(a.entries, id)
			changed = true
		}
	}
	// Add new ones / refresh messages, preserving Since on persisting
	// identities.
	for id, e := range next {
		if old, ok := a.entries[id]; ok {
			if old.Message != e.Message {
				old.Message = e.Message
				a.entries[id] = old
				changed = true
			}
			continue
		}
		e.Since = a.now()
		a.entries[id] = e
		changed = true
	}
	a.notifyLocked(changed)
}

// Report adds one entry (or refreshes its message in place when the
// identity is already active — Since is preserved).
func (a *Aggregator) Report(e Entry) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.notifyLocked(a.reportLocked(e))
}

// Resolve clears the entry with the exact identity (source, hook, key).
// No-op when absent.
func (a *Aggregator) Resolve(source, hook, key string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	id := Entry{Source: source, Hook: hook, Key: key}.identity()
	if _, ok := a.entries[id]; !ok {
		a.notifyLocked(false)
		return
	}
	delete(a.entries, id)
	a.notifyLocked(true)
}

// RegisterEventRule registers a rule for one activity-event kind. Multiple
// rules per kind run in registration order. Register at wiring time,
// before concurrent ObserveEvent calls.
func (a *Aggregator) RegisterEventRule(kind string, rule RuleFunc) {
	if a == nil || rule == nil {
		return
	}
	a.mu.Lock()
	a.rules[kind] = append(a.rules[kind], rule)
	a.mu.Unlock()
}

// ObserveEvent feeds one recorded activity event through the registered
// rules — the event seam. hook is the event's "hook" field ("" for
// server-wide events), message its display text. Events of unrecognized
// kinds are ignored. Safe to call from the recorder's OnRecord callback
// (this only takes the aggregator's own mutex and the trivial onChange).
func (a *Aggregator) ObserveEvent(kind, hook, message string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := false
	for _, rule := range a.rules[kind] {
		report, resolve := rule(hook, message)
		for _, e := range report {
			if a.reportLocked(e) {
				changed = true
			}
		}
		for _, r := range resolve {
			if a.resolveWhereLocked(func(e Entry) bool {
				return e.Source == r.Source && e.Hook == r.Hook &&
					strings.HasPrefix(e.Key, r.KeyPrefix)
			}) {
				changed = true
			}
		}
	}
	a.notifyLocked(changed)
}

// Snapshot returns the active entries, oldest first (ties broken by
// source, hook, key for deterministic output). Never nil.
func (a *Aggregator) Snapshot() []Entry {
	if a == nil {
		return []Entry{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Entry, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		if out[i].Hook != out[j].Hook {
			return out[i].Hook < out[j].Hook
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Count reports the number of active entries.
func (a *Aggregator) Count() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.entries)
}

// reportLocked inserts or refreshes one entry; caller holds a.mu.
func (a *Aggregator) reportLocked(e Entry) bool {
	id := e.identity()
	if old, ok := a.entries[id]; ok {
		if old.Message == e.Message {
			return false
		}
		old.Message = e.Message
		a.entries[id] = old
		return true
	}
	e.Since = a.now()
	a.entries[id] = e
	return true
}

// resolveWhereLocked removes every entry matching pred; caller holds a.mu.
func (a *Aggregator) resolveWhereLocked(pred func(Entry) bool) bool {
	changed := false
	for id, e := range a.entries {
		if pred(e) {
			delete(a.entries, id)
			changed = true
		}
	}
	return changed
}

// notifyLocked fires onChange when a mutation actually happened; caller
// holds a.mu (the callback contract says trivial, so invoking under the
// mutex is fine — the events.Recorder onRecord precedent).
func (a *Aggregator) notifyLocked(changed bool) {
	if changed && a.onChange != nil {
		a.onChange()
	}
}
