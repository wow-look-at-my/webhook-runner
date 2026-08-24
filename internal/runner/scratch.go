package runner

// Per-run scratch directories on an operator-chosen filesystem.
//
// A hook's heavy writes — a CI job's checkout tree, a nested dockerd's image
// store — otherwise land in the container's writable overlay layer or an
// anonymous volume, and both of those live under docker's data-root. A hook
// declares the container paths carrying that churn (hook.json `scratch`); the
// operator points WEBHOOK_RUNNER_SCRATCH_DIR at the filesystem that should
// absorb it. Each run gets its own subtree there, bind-mounted over those
// paths and removed when the run ends.
//
// see docs/internals/scratch-dirs.md

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// scratchRunDir is the per-run subtree under the scratch root. Named by run
// ID so an operator inspecting the filesystem can map a directory back to a
// run, and so the startup sweep can recognize what it is reaping.
func (r *Runner) scratchRunDir(runID string) string {
	return filepath.Join(r.scratchDir, "run-"+runID)
}

// scratchMounts materializes this run's scratch subtree and returns the docker
// mount args for it. The cleanup removes the subtree; it is safe to call when
// nothing was created.
//
// A hook that declares scratch paths on a server with no scratch root
// configured is an ERROR, never a silent fallback: the declaration exists
// precisely to keep those writes off docker's data-root, so quietly running
// without it would defeat the one thing the hook asked for, invisibly.
func (r *Runner) scratchMounts(runID string, paths []string) (args []string, cleanup func(), err error) {
	cleanup = func() {}
	if len(paths) == 0 {
		return nil, cleanup, nil
	}
	if r.scratchDir == "" {
		return nil, cleanup, fmt.Errorf("hook declares scratch paths %s but no scratch directory is configured (set WEBHOOK_RUNNER_SCRATCH_DIR)",
			strings.Join(paths, ", "))
	}
	base := r.scratchRunDir(runID)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, cleanup, fmt.Errorf("create scratch dir: %w", err)
	}
	cleanup = func() {
		if err := os.RemoveAll(base); err != nil {
			r.log.Warn("remove run scratch dir", "dir", base, "err", err)
		}
	}
	for i, p := range paths {
		host := filepath.Join(base, scratchLeafName(i, p))
		// 0777: the container decides its own user (hook.json `user`, or the
		// image's), and the job tree must be writable by whoever that is. The
		// parent run dir is 0755 and lives under the operator's root, so the
		// exposure is one run's own scratch, for that run's lifetime.
		if err := os.MkdirAll(host, 0o777); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("create scratch dir for %s: %w", p, err)
		}
		if err := os.Chmod(host, 0o777); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("chmod scratch dir for %s: %w", p, err)
		}
		args = append(args, "--mount", "type=bind,src="+host+",dst="+p)
	}
	return args, cleanup, nil
}

// storageArgs returns the docker args placing a hook's declared writable paths
// off the container's writable layer: scratch binds from this run's subtree,
// tmpfs mounts in RAM, and --read-only so anything the hook did NOT declare
// fails loudly instead of landing under docker's data-root.
//
// Shared by live runs and `webhook-runner test` so a hook's tests exercise the
// same filesystem it gets in production — which is what makes read_only_rootfs
// provable in CI rather than hopeful.
func (r *Runner) storageArgs(hook *hooks.Hook, runID string) (args []string, cleanup func(), err error) {
	args, cleanup, err = r.scratchMounts(runID, hook.Scratch)
	if err != nil {
		return nil, func() {}, err
	}
	for _, p := range hook.Tmpfs {
		args = append(args, "--tmpfs", p)
	}
	if hook.ReadOnlyRootfs {
		args = append(args, "--read-only")
	}
	return args, cleanup, nil
}

// scratchLeafName maps a container path to a directory name under the run's
// scratch dir. The index prefix is what makes it unique — two distinct paths
// can sanitize to the same string ("/a/b" and "/a_b") — and the sanitized
// tail is there so an operator listing the pool can tell which mount is which.
func scratchLeafName(i int, containerPath string) string {
	tail := strings.Trim(containerPath, "/")
	tail = strings.ReplaceAll(tail, "/", "_")
	return fmt.Sprintf("%d-%s", i, tail)
}

// SweepOrphanScratch removes per-run scratch subtrees left behind by a server
// process that died with runs in flight. Call it ONCE at serve startup,
// alongside SweepOrphanContainers and under the same safety argument (the run
// store's bbolt flock proves this is the only live serve process on this data
// dir, and this process has started no runs yet), so every "run-*" directory
// present is by definition an orphan.
//
// Without this, a crash mid-run strands the whole job tree on the scratch
// filesystem with nothing that will ever remove it — the failure mode being a
// pool that fills up over weeks of restarts.
func (r *Runner) SweepOrphanScratch() {
	if r.scratchDir == "" {
		return
	}
	entries, err := os.ReadDir(r.scratchDir)
	if err != nil {
		if !os.IsNotExist(err) {
			r.log.Warn("scratch sweep: read scratch dir failed", "dir", r.scratchDir, "err", err)
		}
		return
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "run-") {
			continue
		}
		dir := filepath.Join(r.scratchDir, e.Name())
		if err := os.RemoveAll(dir); err != nil {
			r.log.Warn("remove orphaned run scratch dir", "dir", dir, "err", err)
			continue
		}
		removed++
		r.log.Info("removed orphaned run scratch dir", "dir", dir)
	}
	if removed > 0 {
		r.events.Record("run.orphan_scratch_removed",
			fmt.Sprintf("removed %d orphaned run scratch director(ies) left by a previous server process", removed),
			nil)
	}
}
