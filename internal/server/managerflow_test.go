package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/managers"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// fakeManagers is a ManagerControl the server tests drive directly: one
// real inbox per id (the actual completion/settle semantics), instance
// binding under test control.
type fakeManagers struct {
	mu        sync.Mutex
	inboxes   map[string]*managers.Inbox
	instances map[string]string
	titles    map[string]string
	stopped   []string
	onChange  func()
}

func newFakeManagers(ids ...string) *fakeManagers {
	f := &fakeManagers{
		inboxes:   map[string]*managers.Inbox{},
		instances: map[string]string{},
		titles:    map[string]string{},
	}
	for _, id := range ids {
		f.inboxes[id] = managers.NewInbox(0, nil)
	}
	return f
}

func (f *fakeManagers) bind(id, instanceID string) {
	f.mu.Lock()
	f.instances[id] = instanceID
	f.mu.Unlock()
	f.inboxes[id].BindInstance(instanceID, nil, nil, nil)
}

func (f *fakeManagers) Deliver(id string, headers http.Header, payload []byte) *managers.Delivered {
	ib := f.inboxes[id]
	if ib == nil {
		return nil
	}
	return ib.PushDelivery(headers, payload)
}

func (f *fakeManagers) InboxNext(ctx context.Context, id, instanceID string, wait time.Duration) (managers.Event, bool, error) {
	ib := f.inboxes[id]
	if ib == nil {
		return managers.Event{}, false, managers.ErrNotSession
	}
	return ib.Next(ctx, instanceID, wait)
}

func (f *fakeManagers) IsCurrentInstance(id, instanceID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return instanceID != "" && f.instances[id] == instanceID
}

func (f *fakeManagers) TouchInstance(id, instanceID string) bool {
	return f.IsCurrentInstance(id, instanceID)
}

func (f *fakeManagers) SetInstanceTitle(id, instanceID, title string) bool {
	if !f.IsCurrentInstance(id, instanceID) {
		return false
	}
	f.mu.Lock()
	f.titles[id] = title
	f.mu.Unlock()
	return true
}

func (f *fakeManagers) Statuses() []managers.Status {
	return []managers.Status{{ID: "coord", State: "running"}}
}

func (f *fakeManagers) StatusFor(id string) (managers.Status, bool) {
	if _, ok := f.inboxes[id]; !ok {
		return managers.Status{}, false
	}
	return managers.Status{ID: id, State: "running"}, true
}

func (f *fakeManagers) OutputTail(string) []string { return []string{"line"} }

func (f *fakeManagers) RequestStop(id, reason string) {
	f.mu.Lock()
	f.stopped = append(f.stopped, id+":"+reason)
	f.mu.Unlock()
}

func (f *fakeManagers) Poke(string) {}

// SetOnChange mirrors the real supervisor's wiring: the seam the server
// turns into a "managers" section signal, fed here by the inboxes (depth
// and stamps are exactly what the roster shows).
func (f *fakeManagers) SetOnChange(fn func()) {
	f.mu.Lock()
	f.onChange = fn
	f.mu.Unlock()
	for _, ib := range f.inboxes {
		ib.SetOnChange(fn)
	}
}

// changed fires the seam as a supervision-side mutation would (an output
// line, a state transition).
func (f *fakeManagers) changed() {
	f.mu.Lock()
	fn := f.onChange
	f.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// managerServer builds a Server with one declared manager (secret-authed,
// one skip_if condition) and the fake control.
func managerServer(t *testing.T, doc string) (*Server, *fakeManagers, *hooks.Manager, *overrides.Store, *events.Recorder, *kv.Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := filepath.Join(t.TempDir(), "coord")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manager.json"), []byte(doc), 0o644))
	mgr, err := hooks.ParseManager("coord", filepath.Join(dir, "manager.json"), []byte(doc))
	require.NoError(t, err)

	reg := hooks.NewRegistry()
	reg.ReplaceManagers(map[string]*hooks.Manager{"coord": mgr})

	ov, err := overrides.Open(filepath.Join(t.TempDir(), "overrides.json"))
	require.NoError(t, err)
	store, err := kv.New(kv.Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("test-secret"), logger)
	require.NoError(t, err)
	rec := events.NewRecorder(100)
	fm := newFakeManagers("coord")
	tr := runs.NewTracker()
	s := New(Options{
		Logger:    logger,
		Registry:  reg,
		Tracker:   tr,
		Runner:    runner.New(runner.Options{Tracker: tr, Logger: logger}),
		Events:    rec,
		Overrides: ov,
		Managers:  fm,
		KV:        store,
	})
	return s, fm, mgr, ov, rec, store
}

const managerDoc = `{
  "$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json",
  "description": "coordinator",
  "enable": true,
  "secret": "hmac-secret",
  "reconcile_interval": "3m",
  "command": ["run"],
  "skip_if": [ {"header:x-github-event": {"ne": "workflow_job"}} ]
}`

func signedManagerReq(t *testing.T, s *Server, body, event string) *httptest.ResponseRecorder {
	t.Helper()
	mac := hmac.New(sha256.New, []byte("hmac-secret"))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest("POST", "/hook/coord", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	rr := httptest.NewRecorder()
	s.HookHandler().ServeHTTP(rr, req)
	return rr
}

// The manager delivery pipeline: kill switch → auth → skip_if → inbox.
// The delivery comes back out of /inbox/next byte-identical.
func TestManagerTriggerFlow(t *testing.T) {
	s, fm, _, _, rec, store := managerServer(t, managerDoc)

	// Unauthenticated: 401, nothing enqueued, conditions never probed.
	rr := httptest.NewRecorder()
	s.HookHandler().ServeHTTP(rr, httptest.NewRequest("POST", "/hook/coord", strings.NewReader(`{}`)))
	require.Equal(t, 401, rr.Code)

	// skip_if match (non-workflow_job): 200 skipped, no inbox entry, loud.
	rr = signedManagerReq(t, s, `{"zen":"ok"}`, "ping")
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), "skipped")
	assert.Contains(t, eventKinds(rec.ListByHook("coord", 10)), "manager.skipped")

	// A real delivery: 202 queued with an event id.
	rr = signedManagerReq(t, s, `{"action":"queued"}`, "workflow_job")
	require.Equal(t, 202, rr.Code)
	var acc struct {
		Manager, Event, Status string
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &acc))
	assert.Equal(t, "queued", acc.Status)
	assert.NotEmpty(t, acc.Event)

	// The manager's instance pops it via POST /inbox/next (state socket,
	// instance token).
	fm.bind("coord", "inst-1")
	tok := store.Token("coord", "inst-1")
	nr := stateReq(t, s, "POST", "/inbox/next", tok, strings.NewReader(`{"wait_seconds":1}`))
	require.Equal(t, 200, nr.Code)
	var ev managers.Event
	require.NoError(t, json.Unmarshal(nr.Body.Bytes(), &ev))
	assert.Equal(t, "delivery", ev.Kind)
	assert.JSONEq(t, `{"action":"queued"}`, string(ev.Payload))
	assert.Equal(t, "workflow_job", ev.Headers.Get("X-Github-Event"))

	// Elapsed wait → 204; stale instance → 409.
	require.Equal(t, 204, stateReq(t, s, "POST", "/inbox/next", tok, strings.NewReader(`{"wait_seconds":1}`)).Code)
	require.Equal(t, 409, stateReq(t, s, "POST", "/inbox/next", store.Token("coord", "stale"), nil).Code)
}

// Enable semantics match hooks: absent `enable` means enabled, explicit
// enable:false loads disabled, and the operator's dashboard switch
// outranks the default in both directions.
func TestManagerEnableDefaultsAndKillSwitch(t *testing.T) {
	// Absent enable → deliveries accepted with no operator action.
	docDefault := strings.Replace(managerDoc, `"enable": true,`, ``, 1)
	s, _, _, ov, _, _ := managerServer(t, docDefault)
	rr := signedManagerReq(t, s, `{"action":"queued"}`, "workflow_job")
	require.Equal(t, 202, rr.Code, "default-enabled managers must accept deliveries on deploy")

	// The operator's dashboard disable overrides the default.
	_, err := ov.SetHookDisabled("coord", true)
	require.NoError(t, err)
	require.Equal(t, 503, signedManagerReq(t, s, `{"action":"queued"}`, "workflow_job").Code)

	// Explicit enable:false is the config-side off switch...
	docOff := strings.Replace(managerDoc, `"enable": true,`, `"enable": false,`, 1)
	s2, _, _, ov2, _, _ := managerServer(t, docOff)
	require.Equal(t, 503, signedManagerReq(t, s2, `{"action":"queued"}`, "workflow_job").Code)

	// ...and the operator's enable override outranks it.
	_, err = ov2.SetHookDisabled("coord", false)
	require.NoError(t, err)
	require.Equal(t, 202, signedManagerReq(t, s2, `{"action":"queued"}`, "workflow_job").Code)
}

// synchronous: the delivery holds until the manager finishes processing
// THAT event (its next /inbox/next call), then answers 200 processed.
func TestManagerSynchronousDelivery(t *testing.T) {
	syncDoc := strings.Replace(managerDoc, `"reconcile_interval"`, `"synchronous": true, "reconcile_interval"`, 1)
	s, fm, _, _, _, store := managerServer(t, syncDoc)
	fm.bind("coord", "inst-1")
	tok := store.Token("coord", "inst-1")

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- signedManagerReq(t, s, `{"action":"queued"}`, "workflow_job") }()

	// The manager pops the event, then comes back for the next one — that
	// second call settles the first event as processed.
	require.Eventually(t, func() bool {
		return fm.inboxes["coord"].Depth() > 0 || len(done) > 0
	}, 2*time.Second, 5*time.Millisecond)
	nr := stateReq(t, s, "POST", "/inbox/next", tok, strings.NewReader(`{"wait_seconds":2}`))
	require.Equal(t, 200, nr.Code)
	go stateReq(t, s, "POST", "/inbox/next", tok, strings.NewReader(`{"wait_seconds":1}`))

	rr := <-done
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), "processed")
}

// /spawn accepts a manager instance as PARENT (first-class identity, not a
// run): the liveness check rides the supervisor, and a stale instance
// token still 409s.
func TestSpawnManagerParent(t *testing.T) {
	s, fm, _, _, _, store := managerServer(t, managerDoc)
	// The server needs spawn wiring: a target hook + allowlist + runner are
	// exercised in spawn_test.go; here the PARENT gate is the subject — an
	// unknown target after a PASSING parent check proves the manager parent
	// was accepted (404, not the parent 409).
	fm.bind("coord", "inst-1")
	tok := store.Token("coord", "inst-1")
	rr := stateReq(t, s, "POST", "/spawn", tok,
		strings.NewReader(`{"hook":"worker","count":1,"payload":{}}`))
	assert.Equal(t, 404, rr.Code, "parent accepted; target lookup is the next gate")

	stale := store.Token("coord", "dead-instance")
	rr = stateReq(t, s, "POST", "/spawn", stale,
		strings.NewReader(`{"hook":"worker","count":1,"payload":{}}`))
	assert.Equal(t, 409, rr.Code, "a stale instance token is refused — the API-level single-instance guard")
}

// /wait and /title work against the instance identity: the wait holds and
// returns waited; the title lands on the panel via SetInstanceTitle.
func TestManagerWaitAndTitle(t *testing.T) {
	s, fm, _, _, rec, store := managerServer(t, managerDoc)
	fm.bind("coord", "inst-1")
	tok := store.Token("coord", "inst-1")

	rr := stateReq(t, s, "POST", "/wait", tok, strings.NewReader(`{"seconds":1,"reason":"settling"}`))
	require.Equal(t, 200, rr.Code)
	var res struct{ Waited int }
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
	assert.Equal(t, 1, res.Waited)
	assert.Contains(t, eventKinds(rec.ListByHook("coord", 10)), "manager.wait")

	require.Equal(t, 204, stateReq(t, s, "POST", "/title", tok, strings.NewReader(`{"title":"reconciled 3 repos"}`)).Code)
	fm.mu.Lock()
	assert.Equal(t, "reconciled 3 repos", fm.titles["coord"])
	fm.mu.Unlock()

	// A stale instance gets the run-is-not-active 409 on both.
	stale := store.Token("coord", "stale")
	assert.Equal(t, 409, stateReq(t, s, "POST", "/wait", stale, strings.NewReader(`{"seconds":1,"reason":"x"}`)).Code)
	assert.Equal(t, 409, stateReq(t, s, "POST", "/title", stale, strings.NewReader(`{"title":"x"}`)).Code)
}

// The admin surface: roster, detail (+output tail), kill switch with
// manager semantics (disable stops the instance), restart bounce.
func TestManagerAdminSurface(t *testing.T) {
	s, fm, _, _, _, _ := managerServer(t, managerDoc)

	rr := adminReq(t, s, "GET", "/managers", nil)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), "coord")

	rr = adminReq(t, s, "GET", "/managers/coord", nil)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), "output")
	require.Equal(t, 404, adminReq(t, s, "GET", "/managers/nope", nil).Code)

	require.Equal(t, 200, adminReq(t, s, "POST", "/managers/coord/disable", nil).Code)
	fm.mu.Lock()
	assert.Contains(t, fm.stopped, "coord:disabled by operator")
	fm.mu.Unlock()
	rrD := signedManagerReq(t, s, `{"action":"queued"}`, "workflow_job")
	assert.Equal(t, 503, rrD.Code, "the disable gates deliveries immediately")

	require.Equal(t, 200, adminReq(t, s, "POST", "/managers/coord/enable", nil).Code)
	require.Equal(t, 202, adminReq(t, s, "POST", "/managers/coord/restart", nil).Code)
	fm.mu.Lock()
	assert.Contains(t, fm.stopped, "coord:restarted by operator")
	fm.mu.Unlock()
}

// adminReq drives the admin mux.
func adminReq(t *testing.T, s *Server, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	rr := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rr, req)
	return rr
}
