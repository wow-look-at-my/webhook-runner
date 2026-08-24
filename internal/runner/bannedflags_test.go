// Docker flags a hook container must never get, and why each one is banned.
//
// Both defeat the same wall from different sides: this fleet executes other
// people's CI, so a run must not be able to reach the host. Docker's defaults
// already hold every property here, so each ban costs nothing today -- which is
// exactly the problem. A default is one plausible commit from gone, and each of
// these is the first hit when searching for why a nested daemon or a sandbox
// will not start. These make that commit a build failure.
//
//   - --pid: sharing the host PID namespace puts the host's process table in
//     the container's /proc. dats binds that /proc read-only when the kernel
//     refuses it a private procfs, and the bind is safe only because the procfs
//     lists nothing outside the container.
//   - systempaths=unconfined: clears docker's masked AND read-only /proc paths,
//     so /proc/sysrq-trigger becomes writable to a container root that already
//     exists. It reads as the missing half of the seccomp.userns opt-in; it is
//     not (seccomp.go).
//
// --privileged is NOT here. dind needs it and nothing else on this host makes
// /proc/sys and /sys/fs/cgroup writable, so banning it takes the fleet down --
// which is what happened (dind.go, docs/internals/nested-containers.md). It is
// confined to dindArgs, which one test pins, rather than banned outright.
package runner

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// bannedFlags are the spellings that must never reach a container.
var bannedFlags = []string{
	"--pid", "--pid=host", "--pid=container",
	"systempaths",
}

// TestNoContainerGetsABannedFlag scans every string literal this module
// compiles. One builder assembles every container start today
// (containerargs.go), and a runtime assertion covers the paths that call it --
// but a new caller hand-rolling its own args would be covered by neither.
func TestNoContainerGetsABannedFlag(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)

	fset := token.NewFileSet()
	var found []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "vendor" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		// This file names the flags it bans, and so may any future test
		// asserting their absence.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, flag := range bannedFlags {
				if strings.Contains(lit.Value, flag) {
					rel, _ := filepath.Rel(root, path)
					found = append(found, rel+":"+
						fset.Position(lit.Pos()).String()[len(path)+1:]+" "+lit.Value)
				}
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)

	assert.Empty(t, found,
		"a banned flag reached a container's docker args: %v\n"+
			"Each one hands a hook a piece of the host -- the host's process table, "+
			"every capability, or a writable /proc/sysrq-trigger. Docker's defaults "+
			"are the contract here; see this file's header for which and why.", found)
}

// TestLiveRunGetsNoBannedFlag is the runtime half: a literal scan cannot see a
// flag assembled from a variable, and this can.
func TestLiveRunGetsNoBannedFlag(t *testing.T) {
	for _, dind := range []bool{false, true} {
		name := "plain"
		if dind {
			name = "dind"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			r := New(Options{
				Tracker: runs.NewTracker(),
				Logger:  newSilentLogger(),
				TmpDir:  dir,
				Docker:  writeArgDumpDocker(t, dir),
			})
			hook := dindHook(t, dir, "banned-"+name, dind)
			run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
			require.NoError(t, err)
			r.Wait()

			out := run.Snapshot(-1).Output
			require.NotEqual(t, -1, indexOfArg(out, "arg=run"),
				"the arg dump must have captured a docker run, or this asserts on nothing")
			for _, flag := range bannedFlags {
				assert.Equal(t, -1, indexOfArg(out, "arg="+flag),
					"%s must not reach a hook container's docker args", flag)
			}
		})
	}
}
