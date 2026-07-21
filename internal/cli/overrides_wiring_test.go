package cli

// Tests for the operator kill-switch wiring in serve.go: overrides survive
// hooks-repo reloads (buildLoadAndApply re-applies them, announcing — never
// dropping — orphans) and server restarts (loaded from disk before the
// first load), and the scheduler's Fire path skips disabled hooks.

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func writeTestHook(t *testing.T, root, id string) {
	t.Helper()
	dir := filepath.Join(root, id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, hooks.DockerfileName), []byte("FROM alpine\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook.json"),
		[]byte(`{"$schema":"s","command":["x"]}`), 0o644))
}

func writeConcurrencyJSON(t *testing.T, root, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, concurrency.FileName), []byte(body), 0o644))
}

func countEvents(rec *events.Recorder, kind string) int {
	n := 0
	for _, ev := range rec.List(0) {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// seedManager mirrors runServe's startup seeding: persisted concurrency
// overrides are pushed into the manager before the first load.
func seedManager(t *testing.T, mgr *concurrency.Manager, ov *overrides.Store) {
	t.Helper()
	for group, limit := range ov.ConcurrencyLimits() {
		require.NoError(t, mgr.SetLimitOverride(group, limit))
	}
}

func groupStatus(t *testing.T, mgr *concurrency.Manager, name string) concurrency.GroupStatus {
	t.Helper()
	for _, st := range mgr.Status() {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("group %q not in status", name)
	return concurrency.GroupStatus{}
}

// The reload-survival contract: a hooks-repo reload must never silently
// wipe an operator override. Disabled hooks stay disabled, limit overrides
// stay applied; an override whose target vanishes is kept inert and
// announced with ONE override.orphaned event per orphaning, and re-applies
// when the target returns.
func TestLoadAndApplyReappliesOverridesAndAnnouncesOrphans(t *testing.T) {
	root := t.TempDir()
	writeTestHook(t, root, "h1")
	writeConcurrencyJSON(t, root, `{"groups":{"g":{"limit":3}}}`)

	ov, err := overrides.Open(filepath.Join(t.TempDir(), "overrides.json"))
	require.NoError(t, err)
	_, err = ov.SetHookDisabled("h1", true)
	require.NoError(t, err)
	_, err = ov.SetConcurrencyLimit("g", 1)
	require.NoError(t, err)

	reg := hooks.NewRegistry()
	mgr := concurrency.NewManager(nil)
	seedManager(t, mgr, ov)
	rec := events.NewRecorder(200)
	loadAndApply := buildLoadAndApply(root, reg, mgr, nil, nil, ov, nil, nil, testLogger(), rec)

	// Initial load: the hook is registered (disabling never unloads it) and
	// the limit override is effective on top of the declared config.
	loadAndApply()
	_, ok := reg.Get("h1")
	require.True(t, ok, "a disabled hook stays loaded/registered")
	assert.True(t, ov.HookDisabled("h1", true))
	st := groupStatus(t, mgr, "g")
	assert.Equal(t, 1, st.Limit, "the limit override must be effective after the first load")
	assert.Equal(t, 3, st.Declared)
	assert.True(t, st.Overridden)
	assert.Zero(t, countEvents(rec, "override.orphaned"))

	// An ordinary reload changes nothing: overrides still applied, no
	// orphan noise.
	loadAndApply()
	assert.True(t, ov.HookDisabled("h1", true))
	st = groupStatus(t, mgr, "g")
	assert.Equal(t, 1, st.Limit)
	assert.True(t, st.Overridden)
	assert.Zero(t, countEvents(rec, "override.orphaned"))

	// The hook and the group disappear from the repo: both overrides become
	// orphans — KEPT in the store, announced exactly once each.
	require.NoError(t, os.RemoveAll(filepath.Join(root, "h1")))
	require.NoError(t, os.Remove(filepath.Join(root, concurrency.FileName)))
	loadAndApply()
	assert.Equal(t, 2, countEvents(rec, "override.orphaned"))
	assert.True(t, ov.HookDisabled("h1", true), "an orphaned disable override is never dropped")
	_, hasLimit := ov.ConcurrencyLimit("g")
	assert.True(t, hasLimit, "an orphaned limit override is never dropped")

	// Still orphaned on the next reload: no repeat announcements.
	loadAndApply()
	assert.Equal(t, 2, countEvents(rec, "override.orphaned"))

	// The targets come back: overrides re-apply, nothing new announced.
	writeTestHook(t, root, "h1")
	writeConcurrencyJSON(t, root, `{"groups":{"g":{"limit":3}}}`)
	loadAndApply()
	assert.Equal(t, 2, countEvents(rec, "override.orphaned"))
	_, ok = reg.Get("h1")
	require.True(t, ok)
	assert.True(t, ov.HookDisabled("h1", true), "the kill switch re-applies when the hook returns")
	st = groupStatus(t, mgr, "g")
	assert.Equal(t, 1, st.Limit, "the limit override re-applies when the group returns")
	assert.True(t, st.Overridden)

	// Orphaned a second time: announced again (returning reset the dedup).
	require.NoError(t, os.RemoveAll(filepath.Join(root, "h1")))
	require.NoError(t, os.Remove(filepath.Join(root, concurrency.FileName)))
	loadAndApply()
	assert.Equal(t, 4, countEvents(rec, "override.orphaned"))
}

// The restart-survival contract at the serve wiring level: a fresh process
// (new Store from the same file, new manager seeded from it, first load)
// boots with the overrides already effective.
func TestOverridesSurviveRestart(t *testing.T) {
	root := t.TempDir()
	writeTestHook(t, root, "h1")
	writeConcurrencyJSON(t, root, `{"groups":{"g":{"limit":3}}}`)
	ovPath := filepath.Join(t.TempDir(), "overrides.json")

	// "First process": flip the switches.
	ov1, err := overrides.Open(ovPath)
	require.NoError(t, err)
	_, err = ov1.SetHookDisabled("h1", true)
	require.NoError(t, err)
	_, err = ov1.SetConcurrencyLimit("g", 2)
	require.NoError(t, err)

	// "Restart": everything rebuilt from disk, in runServe's order —
	// open store, seed manager, then the first load.
	ov2, err := overrides.Open(ovPath)
	require.NoError(t, err)
	reg := hooks.NewRegistry()
	mgr := concurrency.NewManager(nil)
	seedManager(t, mgr, ov2)
	rec := events.NewRecorder(50)
	buildLoadAndApply(root, reg, mgr, nil, nil, ov2, nil, nil, testLogger(), rec)()

	assert.True(t, ov2.HookDisabled("h1", true), "the kill switch must survive a restart")
	st := groupStatus(t, mgr, "g")
	assert.Equal(t, 2, st.Limit, "the limit override must be effective from the first post-boot load")
	assert.Equal(t, 3, st.Declared)
	assert.True(t, st.Overridden)
}

// The scheduler path of the kill switch: Fire on a disabled hook skips the
// run with a schedule.skipped event carrying the reason; re-enabling lets
// the next fire dispatch normally.
func TestScheduleFireSkipsDisabledHook(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	require.NoError(t, os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	reg := hooks.NewRegistry()
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}, Schedule: "5m"})
	tracker := runs.NewTracker()
	ov, err := overrides.Open(filepath.Join(dir, "overrides.json"))
	require.NoError(t, err)
	rec := events.NewRecorder(50)
	rn := runner.New(runner.Options{Tracker: tracker, Logger: testLogger(), TmpDir: dir, Docker: docker})
	fire := buildScheduleFire(reg, tracker, ov, rn, testLogger(), rec)

	_, err = ov.SetHookDisabled("h", true)
	require.NoError(t, err)
	fire("h")
	assert.Empty(t, tracker.ListAll(0), "a disabled hook's scheduled run must not start")
	require.Equal(t, 1, countEvents(rec, "schedule.skipped"))
	assert.Zero(t, countEvents(rec, "schedule.fired"))
	skip := rec.List(0)[0]
	assert.Equal(t, "schedule.skipped", skip.Kind)
	assert.Equal(t, "disabled by operator", skip.Fields["reason"])
	assert.Equal(t, "h", skip.Fields["hook"])

	// Re-enabled: the next tick dispatches through the normal pipeline.
	_, err = ov.SetHookDisabled("h", false)
	require.NoError(t, err)
	fire("h")
	assert.Equal(t, 1, countEvents(rec, "schedule.fired"))
	assert.Len(t, tracker.ListAll(0), 1)

	// The hook.json enable:false DEFAULT gates the schedule path too, with
	// no override stored; an explicit enable override outranks it.
	off := false
	reg.Set(&hooks.Hook{ID: "d", Command: []string{"x"}, Schedule: "5m", Enable: &off})
	fire("d")
	assert.Equal(t, 2, countEvents(rec, "schedule.skipped"), "a default-disabled hook's tick must skip")
	_, err = ov.SetHookDisabled("d", false)
	require.NoError(t, err)
	fire("d")
	assert.Equal(t, 2, countEvents(rec, "schedule.fired"), "the explicit enable must win over enable:false")
}

// The global run cap's restart-survival at the serve wiring level: a fresh
// process rebuilds the cap from the env/built-in default and applies the
// persisted dashboard override on top, in runServe's order.
func TestGlobalCapOverrideSurvivesRestart(t *testing.T) {
	ovPath := filepath.Join(t.TempDir(), "overrides.json")

	// "First process": the operator overrides the cap on the dashboard.
	ov1, err := overrides.Open(ovPath)
	require.NoError(t, err)
	_, err = ov1.SetGlobalRunLimit(8)
	require.NoError(t, err)

	// "Restart": open store, build the cap at its default, seed the
	// persisted override — exactly runServe's sequence.
	ov2, err := overrides.Open(ovPath)
	require.NoError(t, err)
	g := concurrency.NewGlobal(concurrency.DefaultGlobalLimit)
	limit, ok := ov2.GlobalRunLimit()
	require.True(t, ok)
	require.NoError(t, g.SetLimitOverride(limit))

	st := g.Status()
	assert.Equal(t, 8, st.Limit, "the persisted cap override must be effective at boot")
	assert.True(t, st.Overridden)
	assert.Equal(t, concurrency.DefaultGlobalLimit, st.Default)

	// Clearing reverts to the default — what DELETE /concurrency-global/limit does.
	g.ClearLimitOverride()
	assert.Equal(t, concurrency.DefaultGlobalLimit, g.Status().Limit)
}
