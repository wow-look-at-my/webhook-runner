package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// writeSpawnMockDocker fakes docker like the other mocks, additionally
// echoing the CONTENT of the mounted payload and headers files — so a test
// can assert exactly what a spawned container would see in
// HOOK_PAYLOAD_FILE and HOOK_HEADERS_FILE (the event-header plumbing).
func writeSpawnMockDocker(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "docker")
	script := `#!/bin/sh
if [ "$1" = "kill" ]; then exit 0; fi
if [ "$1" = "image" ] || [ "$1" = "build" ]; then exit 0; fi
for a in "$@"; do
  case "$a" in
    *:/var/run/webhook-runner/payload:ro) echo "payload:$(cat "${a%%:*}")" ;;
    *:/var/run/webhook-runner/headers.json:ro) cat "${a%%:*}" ;;
  esac
done
exit 0
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// spawnFixture wires everything POST /spawn touches: token auth (kv), the tracker, the registry + a real (mock-docker) runner, the events feed, and the operator-override store behind.
type spawnFixture struct {
	s       *Server
	store   *kv.Store
	reg     *hooks.Registry
	tracker *runs.Tracker
	rn      *runner.Runner
	rec     *events.Recorder
	ov      *overrides.Store
	fm      *fakeManagers
	dir     string
}

func newSpawnFixture(t *testing.T) *spawnFixture {
	t.Helper()
	dir := t.TempDir()
	docker := writeSpawnMockDocker(t, dir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := kv.New(kv.Config{Dir: filepath.Join(dir, "kv")}, []byte("server-test-secret"), logger)
	require.NoError(t, err)
	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	rec := events.NewRecorder(100)
	ov, err := overrides.Open(filepath.Join(dir, "overrides.json"))
	require.NoError(t, err)
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker, Events: rec})
	t.Cleanup(rn.Wait)
	fm := newFakeManagers("parent")
	s := New(Options{
		Registry: reg, Runner: rn, Tracker: tr, Logger: logger,
		KV: store, Events: rec, Overrides: ov, Managers: fm,
	})
	return &spawnFixture{s: s, store: store, reg: reg, tracker: tr, rn: rn, rec: rec, ov: ov, fm: fm, dir: dir}
}

// managerParent registers a MANAGER "parent" whose manifest declares the
// given spawn targets, binds a live instance, and mints its state token —
// the caller identity of the manifest-authorized spawn tests.
func (f *spawnFixture) managerParent(t *testing.T, targets ...string) (string, string) {
	t.Helper()
	m := &hooks.Manager{Hook: &hooks.Hook{ID: "parent", Command: []string{"x"}, State: true}, SpawnTargets: targets}
	f.reg.ReplaceManagers(map[string]*hooks.Manager{"parent": m})
	const inst = "inst-1"
	f.fm.bind("parent", inst)
	return inst, f.store.Token("parent", inst)
}

// liveParent registers a live (running) run of the "parent" hook and mints
// its state token — the caller identity every spawn test speaks as.
func (f *spawnFixture) liveParent(t *testing.T) (*runs.Run, string) {
	t.Helper()
	run := f.tracker.New("parent")
	run.SetRunning()
	return run, f.store.Token("parent", run.ID())
}

// targetHook registers a hook backed by a real directory (the content hash
// needs one) so the mock-docker runner can actually run it.
func (f *spawnFixture) targetHook(t *testing.T, h *hooks.Hook) *hooks.Hook {
	t.Helper()
	hookDir := filepath.Join(f.dir, h.ID)
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	h.SourcePath = filepath.Join(hookDir, "hook.json")
	f.reg.Set(h)
	return h
}

func spawnBody(hook string, count int) string {
	return fmt.Sprintf(`{"hook":%q,"count":%d,"payload":{"a":1}}`, hook, count)
}

// deniedEvents returns the spawn.denied events on the feed, oldest last.
func (f *spawnFixture) deniedEvents(max int) []events.Event {
	var out []events.Event
	for _, ev := range f.rec.List(max) {
		if ev.Kind == "spawn.denied" {
			out = append(out, ev)
		}
	}
	return out
}

func TestSpawnAuth(t *testing.T) {
	f := newSpawnFixture(t)
	body := spawnBody("t", 1)
	// Absent and garbage tokens: the same 401s as every state route.
	require.Equal(t, 401, stateReq(t, f.s, "POST", "/spawn", "", strings.NewReader(body)).Code)
	require.Equal(t, 401, stateReq(t, f.s, "POST", "/spawn", "parent.bogus", strings.NewReader(body)).Code)
	// A token signed with a different secret never verifies.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	other, err := kv.New(kv.Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("other-secret"), logger)
	require.NoError(t, err)
	require.Equal(t, 401, stateReq(t, f.s, "POST", "/spawn", other.Token("parent", "r1"), strings.NewReader(body)).Code)
}

func TestSpawnWithoutRunnerConfigured(t *testing.T) {
	// A kv-only server (no registry/runner/tracker — odd wiring, and the shape non-state test servers have) must refuse, not nil-deref.
	s, store := newStateServer(t, kv.Config{})
	rr := stateReq(t, s, "POST", "/spawn", store.Token("parent", "r1"), strings.NewReader(spawnBody("t", 1)))
	require.Equal(t, 503, rr.Code)
}

func TestSpawnValidation(t *testing.T) {
	f := newSpawnFixture(t)
	_, tok := f.managerParent(t, "t")

	for _, body := range []string{
		`not json`,
		`{"count":1,"payload":{}}`, // hook missing
		`{"hook":"  ","count":1,"payload":{}}`,
		`{"hook":"t","payload":{}}`, // count missing (0)
		`{"hook":"t","count":0,"payload":{}}`,
		`{"hook":"t","count":-2,"payload":{}}`,
		`{"hook":"t","count":101,"payload":{}}`,
		`{"hook":"t","count":1.5,"payload":{}}`,
		`{"hook":"t","count":1}`, // payload missing
		`{"hook":"t","count":1,"payload":null}`,
		`{"hook":"t","count":1,"payload":[1]}`,
		`{"hook":"t","count":1,"payload":"x"}`,
		`{"hook":"t","count":1,"payload":{},"event":"` + strings.Repeat("e", maxSpawnEventLen+1) + `"}`,
	} {
		rr := stateReq(t, f.s, "POST", "/spawn", tok, strings.NewReader(body))
		require.Equalf(t, 400, rr.Code, "body=%q -> %s", body, rr.Body.String())
	}

	// Payload bound: over maxSpawnPayloadBytes is refused even when the whole body still fits the reader cap...
	big := fmt.Sprintf(`{"hook":"t","count":1,"payload":{"pad":%q}}`, strings.Repeat("a", maxSpawnPayloadBytes))
	require.Less(t, len(big), maxSpawnBody, "test setup: body must fit the reader cap")
	require.Equal(t, 413, stateReq(t, f.s, "POST", "/spawn", tok, strings.NewReader(big)).Code)
	// ...and a body over the reader cap is refused by the reader itself.
	huge := fmt.Sprintf(`{"hook":"t","count":1,"payload":{"pad":%q}}`, strings.Repeat("a", maxSpawnBody))
	require.Equal(t, 413, stateReq(t, f.s, "POST", "/spawn", tok, strings.NewReader(huge)).Code)

	// Nothing above started anything.
	assert.Empty(t, f.tracker.ListByHook("t", 0))
}

func TestSpawnParentRunGuards(t *testing.T) {
	f := newSpawnFixture(t)
	body := spawnBody("t", 1)

	// Unknown run, another hook's run, and a finished run all 409 — the
	// /wait rule: a dead parent has nothing to attribute its spawns to.
	require.Equal(t, 409,
		stateReq(t, f.s, "POST", "/spawn", f.store.Token("parent", "nosuchrun"), strings.NewReader(body)).Code)

	run, _ := f.liveParent(t)
	require.Equal(t, 409,
		stateReq(t, f.s, "POST", "/spawn", f.store.Token("other", run.ID()), strings.NewReader(body)).Code)

	done := f.tracker.New("parent")
	done.Finish(runs.StatusSuccess, 0, "")
	require.Equal(t, 409,
		stateReq(t, f.s, "POST", "/spawn", f.store.Token("parent", done.ID()), strings.NewReader(body)).Code)
}

func TestSpawnUnknownTarget(t *testing.T) {
	f := newSpawnFixture(t)
	_, tok := f.managerParent(t, "ghost")
	rr := stateReq(t, f.s, "POST", "/spawn", tok, strings.NewReader(spawnBody("ghost", 1)))
	require.Equal(t, 404, rr.Code)
	denied := f.deniedEvents(20)
	require.NotEmpty(t, denied)
	assert.Contains(t, denied[0].Msg, `spawn target "ghost" does not exist`)
	assert.Equal(t, "parent", denied[0].Fields["hook"])
	assert.Equal(t, "ghost", denied[0].Fields["target"])
}

func TestSpawnManifestDenyByDefault(t *testing.T) {
	// A manager with NO spawn_targets spawns nothing: the manifest is the allowlist and absent/empty means deny.
	f := newSpawnFixture(t)
	_, tok := f.managerParent(t)
	f.targetHook(t, &hooks.Hook{ID: "worker", Command: []string{"x"}})
	rr := stateReq(t, f.s, "POST", "/spawn", tok, strings.NewReader(spawnBody("worker", 1)))
	require.Equal(t, 403, rr.Code)
	assert.Contains(t, rr.Body.String(), "not allowed to spawn")
	require.NotEmpty(t, f.deniedEvents(20))
	assert.Empty(t, f.tracker.ListByHook("worker", 0), "a denied spawn must start nothing")
}

func TestSpawnHookCallerDenied(t *testing.T) {
	// Hook-run callers carry no manifest field (the published hook schema is frozen) — a live hook run is denied even for an existing target.
	f := newSpawnFixture(t)
	_, tok := f.liveParent(t)
	f.targetHook(t, &hooks.Hook{ID: "worker", Command: []string{"x"}})
	rr := stateReq(t, f.s, "POST", "/spawn", tok, strings.NewReader(spawnBody("worker", 1)))
	require.Equal(t, 403, rr.Code)
	assert.Contains(t, rr.Body.String(), "only managers")
	require.NotEmpty(t, f.deniedEvents(20))
	assert.Empty(t, f.tracker.ListByHook("worker", 0))
}

func TestSpawnManifestTargetMissing(t *testing.T) {
	// A manifest that grants a DIFFERENT target still denies: entries are explicit ids, never wildcards.
	f := newSpawnFixture(t)
	_, tok := f.managerParent(t, "other-target")
	f.targetHook(t, &hooks.Hook{ID: "worker", Command: []string{"x"}})
	require.Equal(t, 403,
		stateReq(t, f.s, "POST", "/spawn", tok, strings.NewReader(spawnBody("worker", 1))).Code)
}

func TestSpawnDisabledTarget(t *testing.T) {
	f := newSpawnFixture(t)
	_, tok := f.managerParent(t, "worker", "born-off")

	// Operator kill switch: same effective-disabled state as handleTrigger and buildScheduleFire.
	f.targetHook(t, &hooks.Hook{ID: "worker", Command: []string{"x"}})
	_, err := f.ov.SetHookDisabled("worker", true)
	require.NoError(t, err)
	rr := stateReq(t, f.s, "POST", "/spawn", tok, strings.NewReader(spawnBody("worker", 1)))
	require.Equal(t, 409, rr.Code)
	assert.Contains(t, rr.Body.String(), "disabled by operator")
	denied := f.deniedEvents(20)
	require.NotEmpty(t, denied)
	assert.Contains(t, denied[0].Msg, "disabled by operator")

	// An `"enable": false` hook.json default gates spawns too.
	off := false
	f.targetHook(t, &hooks.Hook{ID: "born-off", Command: []string{"x"}, Enable: &off})
	require.Equal(t, 409,
		stateReq(t, f.s, "POST", "/spawn", tok, strings.NewReader(spawnBody("born-off", 1))).Code)

	assert.Empty(t, f.tracker.ListByHook("worker", 0))
	assert.Empty(t, f.tracker.ListByHook("born-off", 0))
}

// The happy path, end to end through the real (mock-docker) runner: N
// normal runs of the target, attributed to the parent, titled from the
// target's run_title template, with the payload and the synthetic headers —
// X-GitHub-Event included — visible inside the container.
func TestSpawnSuccess(t *testing.T) {
	f := newSpawnFixture(t)
	parentInst, tok := f.managerParent(t, "worker")
	f.targetHook(t, &hooks.Hook{ID: "worker", Command: []string{"go"}, RunTitle: "job {{job.name}}"})

	body := `{"hook":"worker","count":3,"payload":{"job":{"name":"build-42"}},"event":"workflow_job"}`
	rr := stateReq(t, f.s, "POST", "/spawn", tok, strings.NewReader(body))
	require.Equal(t, 200, rr.Code, rr.Body.String())
	var res struct {
		RunIDs []string `json:"run_ids"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
	require.Len(t, res.RunIDs, 3)

	for _, id := range res.RunIDs {
		run := f.tracker.Get(id)
		require.NotNil(t, run, "spawned runs must be tracked")
		assert.Equal(t, "worker", run.HookID())
		sb := run.SpawnedBy()
		require.NotNil(t, sb, "spawned runs must carry parent attribution")
		assert.Equal(t, parentInst, sb.RunID)
		assert.Equal(t, "parent", sb.HookID)
		assert.Equal(t, "job build-42", run.Title(),
			"run_title must render from the target's template against the spawned payload")
	}

	f.rn.Wait()
	for _, id := range res.RunIDs {
		snap := f.tracker.Get(id).Snapshot(-1)
		assert.Equal(t, runs.StatusSuccess, snap.Status)
		joined := strings.Join(snap.Output, "\n")
		// The container saw the payload verbatim...
		assert.Contains(t, joined, `payload:{"job":{"name":"build-42"}}`)
		// ...and the synthetic headers: the requested event plus the parent identity.
		assert.Contains(t, joined, "X-Github-Event")
		assert.Contains(t, joined, "workflow_job")
		assert.Contains(t, joined, "X-Webhook-Runner-Spawned-By")
		assert.Contains(t, joined, parentInst)
	}

	// run.started events name the parent inline (the runRef convention).
	started := 0
	for _, ev := range f.rec.ListByHook("worker", 50) {
		if ev.Kind == "run.started" {
			started++
			assert.Contains(t, ev.Msg, ", spawned by parent run "+parentInst)
		}
	}
	assert.Equal(t, 3, started)

	// The admin run endpoints expose the attribution (dashboard contract).
	detail := httptest.NewRecorder()
	admin(f.s).ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/runs/"+res.RunIDs[0], nil))
	require.Equal(t, 200, detail.Code)
	assert.Contains(t, detail.Body.String(), `"spawned_by"`)
	assert.Contains(t, detail.Body.String(), parentInst)
}

// skip_if is BYPASSED for spawns, exactly like scheduled fires: the same
// payload/headers that WOULD match the target's skip conditions still runs.
func TestSpawnBypassesSkipIf(t *testing.T) {
	f := newSpawnFixture(t)
	parentInst, tok := f.managerParent(t, "skippy")
	target := f.targetHook(t, &hooks.Hook{
		ID: "skippy", Command: []string{"x"},
		SkipIf: hooks.SkipConditions{
			{"header:x-github-event": &hooks.SkipMatcher{Eq: strPtr("workflow_job")}},
		},
	})

	// Control: these exact inputs DO match the skip condition — a delivery carrying them would be skipped.
	payload := []byte(`{"a":1}`)
	_, skip := target.EvaluateSkip(payload, spawnHeaders("parent", parentInst, "workflow_job"))
	require.True(t, skip, "test setup: the condition must match the spawn inputs")

	rr := stateReq(t, f.s, "POST", "/spawn", tok,
		strings.NewReader(`{"hook":"skippy","count":1,"payload":{"a":1},"event":"workflow_job"}`))
	require.Equal(t, 200, rr.Code, rr.Body.String())
	var res struct {
		RunIDs []string `json:"run_ids"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
	require.Len(t, res.RunIDs, 1)

	f.rn.Wait()
	run := f.tracker.Get(res.RunIDs[0])
	require.NotNil(t, run)
	assert.Equal(t, runs.StatusSuccess, run.Status(),
		"a spawn must run the target even when skip_if would match — the scheduled-fire rule")
}

// The target's concurrency group gates spawned runs like any others: two
// spawns into a limit-1 group run one at a time — the second queues as
// pending — and /spawn still answers immediately with both run IDs.
func TestSpawnConcurrencyGroupGates(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	require.NoError(t, os.WriteFile(docker, []byte(`#!/bin/sh
if [ "$1" = "kill" ]; then exit 0; fi
if [ "$1" = "image" ] || [ "$1" = "build" ]; then exit 0; fi
sleep 0.6
exit 0
`), 0o755))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := kv.New(kv.Config{Dir: filepath.Join(dir, "kv")}, []byte("server-test-secret"), logger)
	require.NoError(t, err)
	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	mgr := concurrency.NewManager(&concurrency.Config{
		Groups: map[string]concurrency.Group{"g": {Limit: 1}},
	})
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker, Groups: mgr})
	t.Cleanup(rn.Wait)
	fm := newFakeManagers("parent")
	s := New(Options{
		Registry: reg, Runner: rn, Tracker: tr, Logger: logger,
		KV: store, Concurrency: mgr, Managers: fm,
	})

	hookDir := filepath.Join(dir, "gated")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	reg.Set(&hooks.Hook{
		ID: "gated", Command: []string{"x"}, ConcurrencyGroup: "g",
		SourcePath: filepath.Join(hookDir, "hook.json"),
	})
	reg.ReplaceManagers(map[string]*hooks.Manager{"parent": {
		Hook: &hooks.Hook{ID: "parent", Command: []string{"x"}, State: true}, SpawnTargets: []string{"gated"},
	}})
	fm.bind("parent", "inst-1")
	tok := store.Token("parent", "inst-1")

	begin := time.Now()
	rr := stateReq(t, s, "POST", "/spawn", tok, strings.NewReader(spawnBody("gated", 2)))
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assert.Less(t, time.Since(begin), 500*time.Millisecond,
		"/spawn is async dispatch — it must not wait on group slots")
	var res struct {
		RunIDs []string `json:"run_ids"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
	require.Len(t, res.RunIDs, 2)

	// One run holds the slot (running), the other queues (pending with a
	// group wait naming "g"). Start order races, so identify by state.
	require.Eventually(t, func() bool {
		var running, queued int
		for _, id := range res.RunIDs {
			snap := tr.Get(id).Snapshot(0)
			switch {
			case snap.Status == runs.StatusRunning:
				running++
			case snap.Status == runs.StatusPending &&
				snap.WaitingOn != nil && snap.WaitingOn.Kind == runs.WaitingOnGroup && snap.WaitingOn.Key == "g":
				queued++
			}
		}
		return running == 1 && queued == 1
	}, 3*time.Second, 10*time.Millisecond,
		"expected exactly one running and one group-queued spawned run")

	rn.Wait()
	for _, id := range res.RunIDs {
		assert.Equal(t, runs.StatusSuccess, tr.Get(id).Status())
	}
}
