package cli

// THE FAIL-CLOSED FLEET CONTRACT.
//
// These tests exist because of a near-miss: removing the `env` manifest field
// produced a binary that could not parse 11 of the fleet's 13 entities, and
// the reload path would have applied the other 2 and called it a successful
// reload. Every unparseable entity would have silently stopped serving, with
// no rollback available -- on that side of the mismatch the TREE never
// changed, only the binary, so there was nothing for the reload gate to roll
// back to. Recovery was manual and the damage invisible.
//
// The rule that replaced it: a load in which ANY entity fails applies
// NOTHING. What that buys is stated once, here, and asserted below:
//
//   1. a refused reload leaves the previous fleet serving, untouched;
//   2. a refused STARTUP load returns an error (serve turns it into a
//      non-zero exit) rather than serving a fraction of the fleet;
//   3. the refusal is visible -- an event plus the needs-attention surface,
//      naming the entities;
//   4. a later clean load applies and clears all of it.
//
// The cost is deliberate and stated in buildLoadAndApply: one broken
// manifest now blocks the whole reload. That is safe only because a broken
// manifest cannot reach a gated deploy -- the reload gate requires the hooks
// repo's CI green, and that CI runs `validate` through this same loader.
import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
)

// writeBadHook writes a manifest carrying a field this binary does not know
// -- the exact shape of a binary/tree contract mismatch (DisallowUnknownFields
// is what turns a retired field into a load error).
func writeBadHook(t *testing.T, root, id string) {
	t.Helper()
	dir := filepath.Join(root, id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, hooks.DockerfileName), []byte("FROM alpine\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook.json"),
		[]byte(`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","command":["x"],"env":{"A":"b"}}`), 0o644))
}

type refuseHarness struct {
	root string
	reg  *hooks.Registry
	agg  *attention.Aggregator
	rec  *events.Recorder
	load func() error
}

func newRefuseHarness(t *testing.T) *refuseHarness {
	t.Helper()
	root := t.TempDir()
	ov, err := overrides.Open(filepath.Join(t.TempDir(), "overrides.json"))
	require.NoError(t, err)
	h := &refuseHarness{
		root: root,
		reg:  hooks.NewRegistry(),
		agg:  attention.New(),
		rec:  events.NewRecorder(200),
	}
	h.load = buildLoadAndApply(root, h.reg, concurrency.NewManager(nil), nil, nil, ov, h.agg, nil, true, testLogger(), h.rec)
	return h
}

func (h *refuseHarness) sources() map[string]int {
	bySource := map[string]int{}
	for _, e := range h.agg.Snapshot() {
		bySource[e.Source]++
	}
	return bySource
}

// (1) + (3): a tree that half-parses is refused whole, the previous fleet
// keeps serving, and the refusal is on every operator surface.
func TestRefusedReloadKeepsThePreviousFleetServing(t *testing.T) {
	h := newRefuseHarness(t)
	writeTestHook(t, h.root, "good-one")
	writeTestHook(t, h.root, "good-two")
	require.NoError(t, h.load(), "a clean tree applies")
	require.Len(t, h.reg.All(), 2)

	// Now the tree grows an entity this binary cannot parse -- the shape of
	// a deploy carrying a manifest field the running binary lacks.
	writeBadHook(t, h.root, "from-the-future")
	err := h.load()

	require.Error(t, err, "a tree with an unloadable entity must be REFUSED, never partially applied")
	assert.Contains(t, err.Error(), "REFUSED")
	assert.Contains(t, err.Error(), "from-the-future", "the error must name the offending entity")

	// The fleet is untouched: still exactly the two that were serving, and
	// NOT the three-minus-one set a partial apply would have installed.
	ids := []string{}
	for _, hk := range h.reg.All() {
		ids = append(ids, hk.ID)
	}
	assert.ElementsMatch(t, []string{"good-one", "good-two"}, ids,
		"the serving registry must be exactly what it was before the refused load")

	// Visible, not silent.
	assert.Equal(t, 1, countEvents(h.rec, "hooks.refused"), "the refusal is an activity-feed event")
	assert.Equal(t, 1, countEvents(h.rec, "hooks.reloaded"),
		"still just the ONE from the clean load — a refused load must never report itself as a reload")
	bySource := h.sources()
	assert.Equal(t, 1, bySource[attention.SourceTreeRefused], "one fleet-level entry saying nothing was applied")
	assert.Equal(t, 1, bySource[attention.SourceLoad], "plus the per-entity entry naming the field")
}

// (2): the startup load has no previous fleet to fall back to, so it fails
// LOUDLY instead of coming up serving whatever still parses. serve turns this
// error into a non-zero exit, which is what makes a bad image a failed deploy.
func TestRefusedStartupLoadIsAnError(t *testing.T) {
	h := newRefuseHarness(t)
	writeTestHook(t, h.root, "good-one")
	writeBadHook(t, h.root, "from-the-future")

	err := h.load()

	require.Error(t, err, "a binary that cannot load the deployed tree must not serve a fraction of it")
	assert.Contains(t, err.Error(), "startup load",
		"the message must say there is no previous fleet, so the operator knows the deploy failed closed")
	assert.Empty(t, h.reg.All(), "nothing may be registered from a refused startup load")
}

// (4): the refusal is not sticky. The next load that parses whole applies and
// clears every entry the refusal raised -- so fixing the tree (or rolling the
// binary back) is the whole recovery procedure.
func TestACleanLoadAfterARefusalAppliesAndClears(t *testing.T) {
	h := newRefuseHarness(t)
	writeTestHook(t, h.root, "good-one")
	require.NoError(t, h.load())

	writeBadHook(t, h.root, "from-the-future")
	require.Error(t, h.load())
	require.NotZero(t, h.sources()[attention.SourceTreeRefused])

	require.NoError(t, os.RemoveAll(filepath.Join(h.root, "from-the-future")))
	require.NoError(t, h.load(), "the tree parses whole again")

	assert.Len(t, h.reg.All(), 1)
	bySource := h.sources()
	assert.Zero(t, bySource[attention.SourceTreeRefused], "the fleet-level entry clears")
	assert.Zero(t, bySource[attention.SourceLoad], "and so does the per-entity one")
	assert.Equal(t, 1, countEvents(h.rec, "hooks.refused"), "no new refusal was recorded")
}

// A fleet-wide mismatch fails EVERY entity at once, which is the case that
// actually happens: the message must stay readable and still say what to do.
func TestARefusalNamingEveryEntityStaysReadable(t *testing.T) {
	h := newRefuseHarness(t)
	writeTestHook(t, h.root, "good-one")
	require.NoError(t, h.load())

	for _, id := range []string{"a", "b", "c", "d", "e"} {
		writeBadHook(t, h.root, id)
	}
	err := h.load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "more", "the entity list is bounded, with the remainder counted")
	assert.Less(t, len(err.Error()), 800, "a refusal an operator cannot read is a refusal nobody acts on")
	assert.Equal(t, []string{"good-one"}, []string{h.reg.All()[0].ID})
}
