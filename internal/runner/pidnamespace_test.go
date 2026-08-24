package runner

// A hook container must never share the host's PID namespace.
//
// It is what makes the container's /proc hold the container's processes and
// nothing else, and a whole isolation property downstream rests on that: dats
// sandboxes a test command by binding the container's /proc read-only when the
// kernel refuses it a private procfs, which is safe precisely because that
// procfs cannot list anything outside the container. Share the namespace and
// that bind starts handing hook code the host's process table -- with no error,
// no log line, and nothing failing.
//
// Docker's default is a fresh PID namespace, so today this holds by never
// passing the flag. That is a convention, and a convention is one plausible
// commit from gone: --pid=host is the standard fix for a monitoring sidecar and
// reads as harmless. These make it a build failure instead.

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

// pidNamespaceFlags are docker's spellings for joining another PID namespace.
// The bare "--pid" covers the two-token form as well.
var pidNamespaceFlags = []string{"--pid", "--pid=host", "--pid=container"}

// TestNoDockerArgSharesThePIDNamespace scans every string literal this module
// compiles, in all three docker-arg builders at once (runner.go for a hook run,
// managersession.go for a manager instance, tests.go for a hook's tests) plus
// anything added later. A runtime assertion covers one path at a time; a new
// builder would simply not be covered by one.
func TestNoDockerArgSharesThePIDNamespace(t *testing.T) {
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
			for _, flag := range pidNamespaceFlags {
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
		"a PID-namespace flag reached a container's docker args: %v\n"+
			"Sharing the host PID namespace puts the host's process table in the "+
			"container's /proc, which silently widens what a sandboxed hook command "+
			"can see. Docker's default (a fresh namespace) is the contract here.", found)
}

// TestLiveRunGetsNoPIDNamespaceFlag is the runtime half: a literal scan cannot
// see a flag assembled from a variable, and this can.
func TestLiveRunGetsNoPIDNamespaceFlag(t *testing.T) {
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
			hook := dindHook(t, dir, "pidns-"+name, dind)
			run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
			require.NoError(t, err)
			r.Wait()

			out := run.Snapshot(-1).Output
			require.NotEqual(t, -1, indexOfArg(out, "arg=run"),
				"the arg dump must have captured a docker run, or this asserts on nothing")
			for _, flag := range pidNamespaceFlags {
				assert.Equal(t, -1, indexOfArg(out, "arg="+flag),
					"%s must not reach a hook container's docker args", flag)
			}
		})
	}
}
