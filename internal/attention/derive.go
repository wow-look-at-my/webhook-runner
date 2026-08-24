package attention

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wow-look-at-my/go-containers/set"
	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// Stable entry keys within a source. Every entry carries a non-empty key
// so a Resolution's ""-prefix wildcard is unambiguous.
const (
	// KeyLoad: the single per-hook "failed to load/validate, dropped" slot
	// (a hook has at most one load error per reload; the message updates
	// in place if the reason morphs while it stays broken).
	KeyLoad = "load"
	// KeyZeroHooks: the one zero-hooks-discovered entry.
	KeyZeroHooks = "zero"
	// KeySops: the hook's secrets.sops.env failed to decrypt.
	KeySops = "sops"
	// KeyAPIKey: the hook's api_key does not resolve to a usable value —
	// shared by the probe ("secrets" source) and the request-time event
	// rule ("event" source).
	KeyAPIKey = "api_key"
	// KeyReportedPrefix + <message>: a hook-emitted misconfiguration
	// signal (the reserved future event class; see
	// RegisterStandardEventRules).
	KeyReportedPrefix = "reported:"
	// KeyTmpDir: the boot-scoped containerized-without-TMPDIR verdict.
	KeyTmpDir = "tmpdir"
	// KeyDataRoot: the boot-scoped verdict that docker's store is NOT where
	// the deployment said it would be (WEBHOOK_RUNNER_EXPECT_DATA_ROOT), so
	// this runner's containers are writing to the wrong filesystem.
	KeyDataRoot      = "data_root"
	KeyHostIsolation = "host-isolation"
	// KeyReloadHeld: a newer hooks-repo commit is held by the reload CI
	// gate — awaiting the gating status, or that status came back red.
	// Reported/resolved by internal/reloadgate ("reload" source, no hook).
	KeyReloadHeld = "held"
	// KeyReloadUnverified: the serving hooks tree has no recorded green
	// gating status (fresh install, or the last-good commit vanished).
	// Reported/resolved by internal/reloadgate ("reload" source, no hook).
	KeyReloadUnverified = "unverified"
	// KeyReloadPoll: the reload gate's reconciliation poll found a newer
	// hooks-repo tip but could NOT read its gating status (no GitHub token
	// configured, API error, underivable owner/repo) — the tree stays put,
	// blind. Reported by the poll pass; resolved when a later pass
	// determines the status, when the pending hold clears (switch, force,
	// or up-to-date), i.e. everywhere KeyReloadHeld resolves.
	// ("reload" source, no hook.)
	KeyReloadPoll = "poll"
	// KeyGitHubStatus: the entity declares `github_status` but the runner
	// has no GitHub credential, so shouldPost drops every status before it
	// is built. ("github-status" source, per entity.)
	KeyGitHubStatus = "github_status"
)

// Recognized activity-event kinds the standard rules subscribe to.
const (
	// KindHookMisconfigured is recorded by the hook-port auth path when a
	// delivery is denied because the hook's api_key ${NAME} reference does
	// not resolve (internal/server/auth.go — exists today).
	KindHookMisconfigured = "hook.misconfigured"
	// KindHookReported is the RESERVED kind for hook-emitted
	// misconfiguration signals ("permission missing", "feature inert" —
	// the silent-fail audit's loud lines). Nothing records it yet; when a
	// mechanism does, it must use the standard event shape: the "hook"
	// field names the hook, the message describes the problem.
	KindHookReported = "hook.reported_misconfigured"
	// KindHookReportResolved is KindHookReported's paired all-clear: one
	// event clears EVERY reported entry for that hook.
	KindHookReportResolved = "hook.reported_healthy"
)

// RegisterStandardEventRules wires the recognized event-derived entry
// classes — the seam future hook-emitted signals plug into (register
// another kind + RuleFunc; no redesign needed). Clear rules, per class:
//
//   - KindHookMisconfigured → ("event", hook, "api_key"): cleared by the
//     next reload whose static probe finds the hook's api_key resolvable —
//     or the hook no longer loaded (ApplyServeProbe applies both; probes
//     run on EVERY reload, so the rule genuinely fires).
//   - KindHookReported → ("event", hook, "reported:<message>"): one entry
//     per distinct message; a repeat of the same message keeps its Since.
//     Cleared by a KindHookReportResolved event from the same hook (all
//     reported entries at once), or by the hook leaving the loaded set on
//     a reload (ApplyServeProbe). Deliberately NOT cleared by a later
//     successful run: the audit's whole point is that a run can succeed
//     while the feature it should have exercised stayed inert.
func RegisterStandardEventRules(a *Aggregator) {
	a.RegisterEventRule(KindHookMisconfigured, func(hook, message string) ([]Entry, []Resolution) {
		if hook == "" {
			return nil, nil
		}
		return []Entry{{
			Source: SourceEvent,
			Hook:   hook,
			Key:    KeyAPIKey,
			// The event message already names the broken ${NAME} reference
			// (value-free by construction); strip the "<hook>: " prefix the
			// feed convention adds — the entry has its own Hook field.
			Message: "delivery denied at request time: " + strings.TrimPrefix(message, hook+": "),
		}}, nil
	})
	a.RegisterEventRule(KindHookReported, func(hook, message string) ([]Entry, []Resolution) {
		if hook == "" {
			return nil, nil
		}
		return []Entry{{
			Source:  SourceEvent,
			Hook:    hook,
			Key:     KeyReportedPrefix + message,
			Message: "reported by the hook: " + strings.TrimPrefix(message, hook+": "),
		}}, nil
	})
	a.RegisterEventRule(KindHookReportResolved, func(hook, _ string) ([]Entry, []Resolution) {
		if hook == "" {
			return nil, nil
		}
		return nil, []Resolution{{Source: SourceEvent, Hook: hook, KeyPrefix: KeyReportedPrefix}}
	})
}

// FromLoadErrors converts one load/reload's error list (exactly what
// buildLoadAndApply collects: the loader's per-hook errors, layout errors,
// concurrency.json problems, undeclared-group rejections) into the "load"
// and "zero-hooks" entry sets for ReplaceSource. Hook attribution comes
// from the typed errors the loader/concurrency checker produce.
// KeyTreeRefused is the single refused-tree entry's key: one entry however
// many entities failed, since the fleet-level fact is one fact.
const KeyTreeRefused = "tree-refused"

// TreeRefusedEntries is the fleet-level companion to FromLoadErrors: ONE
// entry stating that nothing from this load was applied, and what is running
// instead. `serving` is the entity count still being served (0 = the startup
// load, where there is no previous fleet and serve exits instead).
func TreeRefusedEntries(failed, serving int) []Entry {
	if failed == 0 {
		return nil
	}
	msg := fmt.Sprintf("hooks tree REFUSED: %d entit(y/ies) failed to load, so NONE of this tree was applied", failed)
	if serving > 0 {
		msg += fmt.Sprintf(" — still serving the previous %d entit(y/ies). Fix the entries below (a fleet-wide failure usually means the deployed binary and this tree disagree about a manifest field) and the next reload applies.", serving)
	}
	return []Entry{{
		Source:  SourceTreeRefused,
		Key:     KeyTreeRefused,
		Message: msg,
	}}
}

func FromLoadErrors(errs []error) (load, zero []Entry) {
	load, zero = []Entry{}, []Entry{}
	for _, err := range errs {
		var hookErr hooks.HookLoadError
		var refErr concurrency.RefError
		var legacyErr hooks.IgnoredLegacyDirError
		var zeroErr hooks.ZeroHooksError
		switch {
		case errors.As(err, &zeroErr):
			zero = append(zero, Entry{
				Source:  SourceZeroHooks,
				Key:     KeyZeroHooks,
				Message: zeroErr.Error(),
			})
		case errors.As(err, &hookErr):
			load = append(load, Entry{
				Source:  SourceLoad,
				Hook:    hookErr.HookID,
				Key:     KeyLoad,
				Message: "failed to load — hook dropped: " + hookErr.Err.Error(),
			})
		case errors.As(err, &refErr):
			load = append(load, Entry{
				Source:  SourceLoad,
				Hook:    refErr.HookID,
				Key:     KeyLoad,
				Message: "dropped: " + refErr.Error(),
			})
		case errors.As(err, &legacyErr):
			load = append(load, Entry{
				Source: SourceLoad,
				Hook:   filepath.Base(legacyErr.Dir),
				Key:    KeyLoad,
				// The dir was skipped, not loaded — the hook is effectively
				// offline until moved.
				Message: legacyErr.Error(),
			})
		default:
			// Tree-wide problems with no per-hook identity (hooks dir
			// unreadable, concurrency.json unparseable). Keyed by their
			// message: an identical recurring error keeps its Since; a
			// changed message is a different problem.
			load = append(load, Entry{
				Source:  SourceLoad,
				Key:     "err:" + err.Error(),
				Message: err.Error(),
			})
		}
	}
	return load, zero
}

// ApplyServeProbe re-derives the "secrets" source from the freshly loaded
// hook set and settles the event-derived entries whose clear rules key off
// it. STRICTLY a serve-path helper (called from the reload routine): it
// reads the runner host's environment and execs sops, exactly what
// `validate` in CI must never do — keep it out of every CLI/validate path.
//
// Per loaded hook it statically checks, with the same resolution the
// request/run paths use (secrets.sops.env first, then host env):
//
//   - sops: a present secrets.sops.env must decrypt. On failure ONE entry
//     is reported and the reference checks are skipped — auth and runs
//     fail on the decrypt error before any reference is even expanded, so
//     per-reference entries would be noise.
//   - api_key: must expand to a non-empty value (an unresolvable ${NAME}
//     or an empty expansion denies every delivery with a 401).
//
// A hook's own `settings` are NOT probed here: they are validated against
// the hook's settings.schema.json at LOAD, so a bad one never becomes a
// loaded hook — it surfaces as a load error instead of a running hook with
// quietly-wrong config.
//
// Clear rules applied here for the "event" source: an entry for a hook no
// longer in the loaded set clears (any key — the hook is gone), and the
// request-time api_key entry (key "api_key") clears when this probe finds
// the hook's api_key healthy again.
func ApplyServeProbe(a *Aggregator, loaded map[string]*hooks.Hook, secrets *hooks.SecretsLoader) {
	if a == nil {
		return
	}
	entries := ProbeHooks(loaded, secrets)
	a.ReplaceSource(SourceSecrets, entries)

	apiKeyBroken := set.New[string]()
	for _, e := range entries {
		if e.Key == KeyAPIKey || e.Key == KeySops {
			// A failed decrypt leaves the api_key unresolvable too (auth
			// fails closed on it), so it keeps the event entry alive.
			apiKeyBroken.Add(e.Hook)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := a.resolveWhereLocked(func(e Entry) bool {
		if e.Source != SourceEvent {
			return false
		}
		if _, ok := loaded[e.Hook]; !ok {
			return true // the hook is gone; nothing left to fix
		}
		return e.Key == KeyAPIKey && !apiKeyBroken.Contains(e.Hook)
	})
	a.notifyLocked(changed)
}

// ProbeHooks runs the static resolvability probe over the loaded hooks and
// returns the "secrets" entry set (see ApplyServeProbe for the rules and
// the serve-path-only caveat). A nil secrets loader skips decryption and
// resolves references from the host environment only — matching what the
// request path does when no loader is configured.
func ProbeHooks(loaded map[string]*hooks.Hook, secrets *hooks.SecretsLoader) []Entry {
	entries := []Entry{}
	for _, id := range sortedIDs(loaded) {
		h := loaded[id]
		var vals map[string]string
		if secrets != nil {
			var err error
			vals, err = secrets.Load(h)
			if err != nil {
				entries = append(entries, Entry{
					Source: SourceSecrets,
					Hook:   id,
					Key:    KeySops,
					Message: "secrets.sops.env cannot be decrypted (runs fail before start; api_key auth fails closed): " +
						oneLine(err.Error()),
				})
				continue
			}
		}
		lookup := hooks.SecretsFirstLookup(vals)
		if h.APIKey != "" {
			expanded, missing := hooks.ExpandEnvRefs(h.APIKey, lookup)
			switch {
			case len(missing) > 0:
				entries = append(entries, Entry{
					Source:  SourceSecrets,
					Hook:    id,
					Key:     KeyAPIKey,
					Message: "api_key reference ${" + strings.Join(missing, "}, ${") + "} does not resolve (secrets.sops.env / host env) — every delivery is denied (401)",
				})
			case expanded == "":
				entries = append(entries, Entry{
					Source:  SourceSecrets,
					Hook:    id,
					Key:     KeyAPIKey,
					Message: "api_key expands to an empty value — every delivery is denied (401)",
				})
			}
		}
	}
	return entries
}

// GitHubStatusEntries names every loaded entity that declares
// `github_status` while the runner holds no GitHub credential.
//
// Without this the failure is invisible on both sides: githubstatus's
// shouldPost returns false before a request is ever built, so nothing is
// logged, no run is marked, and the entity's own runs go green while the
// commit status they exist to publish never appears. The manifest asked for
// a status; the deployment cannot post one; that is a misconfiguration and
// belongs on the surface an operator reads.
//
// configured is `(*githubstatus.Client).Enabled()` — a credential SOURCE
// exists, not that a fetch will succeed. A source that is failing right now
// already reports itself per call, so it is not this entry's business.
func GitHubStatusEntries(loaded map[string]*hooks.Hook, managers map[string]*hooks.Manager, configured bool) []Entry {
	if configured {
		return nil
	}
	flat := make(map[string]*hooks.Hook, len(loaded)+len(managers))
	kind := make(map[string]string, len(loaded)+len(managers))
	for id, h := range loaded {
		flat[id], kind[id] = h, "hook"
	}
	for id, m := range managers {
		if m == nil || m.Hook == nil {
			continue
		}
		flat[id], kind[id] = m.Hook, "manager"
	}
	entries := []Entry{}
	for _, id := range sortedIDs(flat) {
		h := flat[id]
		if h.GitHubStatus == nil || !h.GitHubStatus.Enabled {
			continue
		}
		entries = append(entries, Entry{
			Source: SourceGitHubStatus,
			Hook:   id,
			Key:    KeyGitHubStatus,
			Message: "this " + kind[id] + " declares github_status (context " + h.GitHubStatus.Context +
				") but the runner has no GitHub credential — every commit status is dropped, and its runs still go green. " +
				"Set WEBHOOK_RUNNER_GITHUB_TOKEN, or WEBHOOK_RUNNER_SECRET_SERVER_TOKEN to read the credential from secret-server",
		})
	}
	return entries
}

func sortedIDs(m map[string]*hooks.Hook) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// oneLine collapses a (possibly multi-line sops stderr) error into one
// bounded display line. It never contains secret values — decrypt failures
// happen before any plaintext exists.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 240
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 { // don't split a UTF-8 sequence
		cut--
	}
	return s[:cut] + "..."
}
