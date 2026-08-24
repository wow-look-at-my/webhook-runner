package runner

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// scratchHook builds an on-disk hook declaring the given storage, mirroring
// dindHook — a real source dir so the image tag / content hash resolve.
func scratchHook(t *testing.T, dir, id string, h hooks.Hook) *hooks.Hook {
	t.Helper()
	hookDir := filepath.Join(dir, id)
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	h.ID = id
	h.SourcePath = filepath.Join(hookDir, "hook.json")
	if len(h.Command) == 0 {
		h.Command = []string{"x"}
	}
	return &h
}

// runWithScratch starts one run against the arg-dumping docker stub and
// returns the argv lines it recorded.
func runWithScratch(t *testing.T, scratchDir string, hook *hooks.Hook) ([]string, *runs.Run) {
	t.Helper()
	dir := filepath.Dir(filepath.Dir(hook.SourcePath))
	r := New(Options{
		Tracker:    runs.NewTracker(),
		Logger:     newSilentLogger(),
		TmpDir:     dir,
		Docker:     writeArgDumpDocker(t, dir),
		ScratchDir: scratchDir,
	})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()
	return run.Snapshot(-1).Output, run
}

// A declared scratch path becomes a bind mount from a per-run directory on the
// scratch filesystem — the whole point being that those writes never reach
// docker's data-root.
func TestScratchPathsBindFromScratchDir(t *testing.T) {
	dir := t.TempDir()
	scratch := filepath.Join(dir, "pool")
	require.NoError(t, os.MkdirAll(scratch, 0o755))
	hook := scratchHook(t, dir, "worker", hooks.Hook{Scratch: []string{"/home/runner/_work"}})

	out, run := runWithScratch(t, scratch, hook)
	want := "arg=type=bind,src=" + filepath.Join(scratch, "run-"+run.ID(), "0-home_runner__work") + ",dst=/home/runner/_work"
	require.NotEqual(t, -1, indexOfArg(out, want), "expected scratch bind mount in %v", out)

	tag, err := ImageTag(hook)
	require.NoError(t, err)
	assert.Less(t, indexOfArg(out, want), indexOfArg(out, "arg="+tag),
		"the mount must precede the image")
}

// The per-run subtree is removed when the run ends: a scratch filesystem that
// only ever grows is a pool that fills up.
func TestScratchDirRemovedAfterRun(t *testing.T) {
	dir := t.TempDir()
	scratch := filepath.Join(dir, "pool")
	require.NoError(t, os.MkdirAll(scratch, 0o755))
	hook := scratchHook(t, dir, "worker", hooks.Hook{Scratch: []string{"/home/runner/_work"}})

	_, run := runWithScratch(t, scratch, hook)
	_, err := os.Stat(filepath.Join(scratch, "run-"+run.ID()))
	assert.True(t, os.IsNotExist(err), "run scratch dir must be gone, stat err = %v", err)
}

// Declaring scratch paths on a server with no scratch filesystem configured
// FAILS the run. The alternative — running anyway — would put exactly the
// writes the declaration exists to divert back onto docker's data-root, with
// nothing to show for it, which is the failure this field exists to prevent.
func TestScratchWithoutScratchDirFailsTheRun(t *testing.T) {
	dir := t.TempDir()
	hook := scratchHook(t, dir, "worker", hooks.Hook{Scratch: []string{"/home/runner/_work"}})

	_, run := runWithScratch(t, "", hook)
	snap := run.Snapshot(-1)
	assert.Equal(t, runs.StatusError, snap.Status)
	assert.Contains(t, snap.Error, "WEBHOOK_RUNNER_SCRATCH_DIR")
}

// A dind hook that puts /var/lib/docker on scratch must NOT also get the
// anonymous volume: two mounts on one destination is a docker error, so
// getting this wrong fails every dind run outright.
func TestDindScratchReplacesAnonymousVolume(t *testing.T) {
	dir := t.TempDir()
	scratch := filepath.Join(dir, "pool")
	require.NoError(t, os.MkdirAll(scratch, 0o755))
	hook := scratchHook(t, dir, "dinder", hooks.Hook{
		Dind:    true,
		Scratch: []string{"/var/lib/docker"},
	})

	out, run := runWithScratch(t, scratch, hook)
	assert.Equal(t, -1, indexOfArg(out, "arg=--privileged"),
		"dind never adds --privileged, scratch or not")
	assert.Equal(t, -1, indexOfArg(out, "arg=type=volume,dst=/var/lib/docker"),
		"the anonymous volume must be dropped when scratch covers it")
	want := "arg=type=bind,src=" + filepath.Join(scratch, "run-"+run.ID(), "0-var_lib_docker") + ",dst=/var/lib/docker"
	assert.NotEqual(t, -1, indexOfArg(out, want), "expected the scratch bind for the inner daemon in %v", out)
}

// tmpfs paths and the read-only rootfs reach docker, before the image.
func TestTmpfsAndReadOnlyRootfs(t *testing.T) {
	dir := t.TempDir()
	hook := scratchHook(t, dir, "worker", hooks.Hook{
		Tmpfs:          []string{"/tmp"},
		ReadOnlyRootfs: true,
	})

	out, _ := runWithScratch(t, "", hook)
	tmpfs := indexOfArg(out, "arg=--tmpfs")
	ro := indexOfArg(out, "arg=--read-only")
	require.NotEqual(t, -1, tmpfs, "expected --tmpfs in %v", out)
	require.NotEqual(t, -1, ro, "expected --read-only in %v", out)
	assert.NotEqual(t, -1, indexOfArg(out, "arg=/tmp"))

	tag, err := ImageTag(hook)
	require.NoError(t, err)
	img := indexOfArg(out, "arg="+tag)
	assert.Less(t, tmpfs, img, "--tmpfs must precede the image")
	assert.Less(t, ro, img, "--read-only must precede the image")
}

// A hook declaring nothing gets no storage args at all — the feature is
// strictly opt-in, so existing hooks keep their current filesystem.
func TestNoStorageDeclarationAddsNothing(t *testing.T) {
	dir := t.TempDir()
	hook := scratchHook(t, dir, "plain", hooks.Hook{})

	out, _ := runWithScratch(t, filepath.Join(dir, "pool"), hook)
	assert.Equal(t, -1, indexOfArg(out, "arg=--read-only"))
	assert.Equal(t, -1, indexOfArg(out, "arg=--tmpfs"))
	for _, l := range out {
		assert.NotContains(t, l, "type=bind,src="+filepath.Join(dir, "pool"))
	}
}

// The startup sweep reaps run subtrees a dead process left behind, and leaves
// anything else on the filesystem alone — the scratch root may well be a
// dataset the operator keeps other things on.
func TestSweepOrphanScratch(t *testing.T) {
	dir := t.TempDir()
	scratch := filepath.Join(dir, "pool")
	orphan := filepath.Join(scratch, "run-abc123")
	keep := filepath.Join(scratch, "operator-data")
	require.NoError(t, os.MkdirAll(filepath.Join(orphan, "0-home_runner__work"), 0o755))
	require.NoError(t, os.MkdirAll(keep, 0o755))

	r := New(Options{
		Tracker:    runs.NewTracker(),
		Logger:     newSilentLogger(),
		TmpDir:     dir,
		Docker:     writeArgDumpDocker(t, dir),
		ScratchDir: scratch,
	})
	r.SweepOrphanScratch()

	_, err := os.Stat(orphan)
	assert.True(t, os.IsNotExist(err), "orphaned run dir must be removed, stat err = %v", err)
	_, err = os.Stat(keep)
	assert.NoError(t, err, "unrelated entries must survive the sweep")
}

// With no scratch filesystem configured the sweep is a no-op rather than an
// error: most deployments declare no scratch at all.
func TestSweepOrphanScratchNoopWithoutScratchDir(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{
		Tracker: runs.NewTracker(),
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  writeArgDumpDocker(t, dir),
	})
	assert.NotPanics(t, r.SweepOrphanScratch)
}

// Two container paths that sanitize to the same string still get distinct
// directories — the index prefix is what guarantees it.
func TestScratchLeafNamesAreDistinct(t *testing.T) {
	assert.NotEqual(t, scratchLeafName(0, "/a/b"), scratchLeafName(1, "/a_b"))
}
