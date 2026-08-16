package server

// Section-changed signals on /runs/stream: the "changed → refetch once"
// multiplex that lets the dashboard stop polling the non-run admin sections
// (/hooks /images /concurrency /kv /events). These tests drive the three
// real seams — the activity recorder, the kv store, the run tracker — over
// the real HTTP stream, plus the hub-level coalescing and never-drop
// guarantees that distinguish signals from run deltas.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/go-containers/set"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// waitChangedCovering reads stream events (skipping run deltas and
// heartbeats) until the union of received `changed` sections covers want.
// Union-based on purpose: signals coalesce server-side, so one changed
// event may carry several sections and several events may split them.
func waitChangedCovering(t *testing.T, ch <-chan sseEvent, want ...string) {
	t.Helper()
	got := set.New[string]()
	for {
		ev := nextEvent(t, ch, "changed event covering "+strings.Join(want, ","))
		if ev.name != "changed" {
			continue
		}
		var payload struct {
			Sections []string `json:"sections"`
		}
		require.NoError(t, json.Unmarshal([]byte(ev.data), &payload), "changed payload must be JSON")
		require.NotEmpty(t, payload.Sections, "a changed event must name at least one section")
		for _, s := range payload.Sections {
			got.Add(s)
		}
		covered := true
		for _, w := range want {
			if !got.Contains(w) {
				covered = false
			}
		}
		if covered {
			return
		}
	}
}

// The end-to-end path: every seam that mutates a dashboard section pushes a
// changed signal onto the SAME /runs/stream connection.
func TestStreamSectionSignals(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	rec := events.NewRecorder(64)
	store, err := kv.New(kv.Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("test-secret"), logger)
	require.NoError(t, err)
	defer store.Close()
	s := New(Options{Registry: reg, Tracker: tr, Events: rec, KV: store, Logger: logger, Version: testVersion})

	srv := httptest.NewServer(admin(s))
	defer srv.Close()
	evs, resp, cancel := openStream(t, srv.URL)
	defer cancel()
	defer resp.Body.Close()
	nextEvent(t, evs, "retry")
	nextEvent(t, evs, "snapshot")

	// A plain activity event dirties the events section.
	rec.Record("git.pulled", "pulled", nil)
	waitChangedCovering(t, evs, "events")

	// An image event dirties images too (the runner records through the
	// same shared recorder, so this is exactly the build-status path).
	rec.Record("image.built", "hook x image built", nil)
	waitChangedCovering(t, evs, "images")

	// A reload can change the roster, image tags, and group definitions.
	rec.Record("hooks.reloaded", "5 hooks", nil)
	waitChangedCovering(t, evs, "hooks", "images", "concurrency")

	// A kv entry mutation signals kv — the store's own seam, no activity
	// event involved (successful writes are deliberately not events).
	require.NoError(t, store.Set("ns", "k", []byte("v"), 0))
	waitChangedCovering(t, evs, "kv")

	// Run lifecycle dirties concurrency: group active/waiting/holder state
	// moves exactly with run mutations (the OnChange superset signal).
	r := tr.New("h")
	waitChangedCovering(t, evs, "concurrency")
	r.Finish(runs.StatusSuccess, 0, "")
}

// The operator kill switch over real HTTP: the disable/enable handlers
// record hook.disabled/hook.enabled, which must dirty the hooks section.
func TestStreamSectionSignalsOnKillSwitch(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := hooks.NewRegistry()
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}})
	rec := events.NewRecorder(64)
	ov, err := overrides.Open(filepath.Join(t.TempDir(), "overrides.json"))
	require.NoError(t, err)
	s := New(Options{Registry: reg, Tracker: runs.NewTracker(), Events: rec, Overrides: ov, Logger: logger, Version: testVersion})

	srv := httptest.NewServer(admin(s))
	defer srv.Close()
	evs, resp, cancel := openStream(t, srv.URL)
	defer cancel()
	defer resp.Body.Close()
	nextEvent(t, evs, "retry")
	nextEvent(t, evs, "snapshot")

	post, err := http.Post(srv.URL+"/hooks/h/disable", "application/json", nil)
	require.NoError(t, err)
	require.Equal(t, 200, post.StatusCode)
	post.Body.Close()
	waitChangedCovering(t, evs, "hooks", "events")
}

// The manager seam over the real stream: instance output, inbox depth and
// supervision state transitions record NO activity event, so this signal
// is the only thing that keeps the Managers panel and the #manager=<id>
// drill-down live — without it the operator's only refresh was F5.
func TestStreamSectionSignalsOnManagerSurface(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := newFakeManagers("coord")
	rec := events.NewRecorder(64)
	s := New(Options{
		Registry: hooks.NewRegistry(), Tracker: runs.NewTracker(),
		Events: rec, Managers: f, Logger: logger, Version: testVersion,
	})

	srv := httptest.NewServer(admin(s))
	defer srv.Close()
	evs, resp, cancel := openStream(t, srv.URL)
	defer cancel()
	defer resp.Body.Close()
	nextEvent(t, evs, "retry")
	nextEvent(t, evs, "snapshot")

	// A supervision-side mutation: an output line or a state transition.
	f.changed()
	waitChangedCovering(t, evs, "managers")
	assert.Empty(t, rec.List(0), "the manager surface must signal without recording an event")

	// The inbox's own seam: a delivery moves depth and the last-delivered
	// stamp, both on the roster row.
	require.NotNil(t, f.Deliver("coord", http.Header{}, []byte(`{"x":1}`)))
	waitChangedCovering(t, evs, "managers")
}

// Signals can never overflow, drop, or block: a storm with NO reader
// coalesces into one pending wake over a bounded dirty set. Only run
// deltas may drop a slow client (TestRunsStreamSlowClientDropped).
func TestSectionSignalsCoalesceAndNeverDrop(t *testing.T) {
	hub := newStreamHub()
	sub := hub.subscribe()
	for i := 0; i < 10_000; i++ {
		hub.signal("kv")
		if i%3 == 0 {
			hub.signal("events", "images")
		}
	}
	assert.Equal(t, 1, hub.clients(), "signals must never drop a client")

	// Exactly one wake is pending; its drain carries the coalesced union,
	// sorted.
	<-sub.kick
	assert.Equal(t, []string{"events", "images", "kv"}, sub.drainSections())
	assert.Empty(t, sub.drainSections(), "second drain must be empty")
	select {
	case <-sub.kick:
		t.Fatal("no second wake may be pending after the drain")
	default:
	}

	// A signal after the drain re-arms the wake (nothing is lost).
	hub.signal("kv")
	<-sub.kick
	assert.Equal(t, []string{"kv"}, sub.drainSections())
}

// The kind → sections table: every event dirties "events"; the extras are
// exactly the sections whose payloads move with that kind. Rejection noise
// (denied/unknown) must NOT dirty the hooks roster.
func TestSectionsForEvent(t *testing.T) {
	cases := map[string][]string{
		"run.finished":           {"events"},
		"schedule.fired":         {"events"},
		"server.started":         {"events"},
		"lock.stolen":            {"events"},
		"kv.write_failed":        {"events"}, // rolled back — stats unchanged
		"hook.denied":            {"events"}, // rejections don't change the roster
		"hook.unknown":           {"events"},
		"hook.disabled_rejected": {"events"},
		// A reload can also change effective disabled states (a changed
		// enable default), and a kill-switch flip changes which hook-scoped
		// entries GET /attention's read-time filter hides — both dirty
		// "attention" so the banner count tracks the toggle.
		"hooks.reloaded":  {"events", "hooks", "managers", "images", "concurrency", "reload", "attention"},
		"hook.load_error": {"events", "hooks", "managers"},
		"hook.disabled":   {"events", "hooks", "attention"},
		"hook.enabled":    {"events", "hooks", "attention"},
		// Manager lifecycle moves the Managers panel; the manager kill
		// switch also changes which attention entries the read-time
		// disabled filter hides.
		"manager.disabled":             {"events", "managers", "attention"},
		"manager.enabled":              {"events", "managers", "attention"},
		"manager.started":              {"events", "managers"},
		"manager.exited":               {"events", "managers"},
		"manager.leased":               {"events", "managers"},
		"manager.skipped":              {"events", "managers"},
		"manager.wait":                 {"events", "managers"},
		"manager.lease_waiting":        {"events", "managers"},
		"manager.restart_requested":    {"events", "managers"},
		"image.built":                  {"events", "images"},
		"image.build_failed":           {"events", "images"},
		"image.inspect_failed":         {"events", "images"},
		"concurrency.overridden":       {"events", "concurrency"},
		"concurrency.override_cleared": {"events", "concurrency"},
		// Everything that moves the reload panel's view: gate verdicts,
		// git pulls, hooks-repo pushes.
		"git.pulled":            {"events", "reload"},
		"git.pull_failed":       {"events", "reload"},
		"github.push":           {"events", "reload"},
		"reload.held":           {"events", "reload"},
		"reload.held_red":       {"events", "reload"},
		"reload.switched":       {"events", "reload"},
		"reload.forced":         {"events", "reload"},
		"reload.switch_refused": {"events", "reload"},
		"reload.check":          {"events", "reload"},
		"reload.poll_blind":     {"events", "reload"},
	}
	for kind, want := range cases {
		assert.ElementsMatch(t, want, sectionsForEvent(kind), kind)
	}
}
