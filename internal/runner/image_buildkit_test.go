// guarantees about hook image builds, both learned from outage:
// the build must run under BuildKit (the legacy builder rejects `# syntax=`
// frontends and flags like `ADD --unpack`), and a FAILED build must carry
// docker's own error back to the caller — it used to surface as a bare
// "exit status " with the real message only in the server's log.
package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeScriptedDocker fakes docker: `image inspect` reports not-built so
// EnsureImage proceeds to build, and `build` runs the caller's snippet.
func writeScriptedDocker(t *testing.T, dir, buildBody string) string {
	t.Helper()
	bin := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"image\" ] && [ \"$2\" = \"inspect\" ]; then exit 1; fi\n" +
		"if [ \"$1\" = \"build\" ]; then\n" + buildBody + "\nfi\n" +
		"exit 0\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	return bin
}

func buildKitTestHook(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	writeTree(t, root, map[string]string{
		"h/hook.json":  layoutTestHookJSON,
		"h/Dockerfile": "FROM alpine:3.20\n",
	})
	return root
}

// The legacy builder is what produced `dockerfile parse error: unknown flag:
// --unpack` on a host whose daemon and buildx both supported it — the CLI
// never selected BuildKit. Pin the env that selects it.
func TestEnsureImageSelectsBuildKit(t *testing.T) {
	root := buildKitTestHook(t)
	envLog := filepath.Join(t.TempDir(), "env.log")
	bin := writeScriptedDocker(t, t.TempDir(), "echo \"DOCKER_BUILDKIT=$DOCKER_BUILDKIT\" > "+envLog+"; exit 0")

	_, built, err := EnsureImage(bin, loadOnlyHook(t, root), os.Stderr)
	require.NoError(t, err)
	require.True(t, built)

	got, err := os.ReadFile(envLog)
	require.NoError(t, err)
	assert.Equal(t, "DOCKER_BUILDKIT=1", strings.TrimSpace(string(got)),
		"the build must select BuildKit; the legacy builder cannot parse modern Dockerfiles")
}

// A build failure's error must quote what docker printed. Without this the
// operator sees only "exit status " and has to guess at the cause.
func TestEnsureImageFailureCarriesBuildOutput(t *testing.T) {
	root := buildKitTestHook(t)
	const dockerErr = "Error response from daemon: dockerfile parse error on line 70: unknown flag: --unpack"
	bin := writeScriptedDocker(t, t.TempDir(),
		"echo 'Sending build context to Docker daemon  3.951MB'; echo '"+dockerErr+"' >&2; exit 1")

	_, _, err := EnsureImage(bin, loadOnlyHook(t, root), os.Stderr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), dockerErr,
		"a failed build must carry docker's own message, not just an exit status")
}

func TestTailWriterKeepsBoundedTail(t *testing.T) {
	w := &tailWriter{max: 3}
	for _, l := range []string{"one", "two", "three", "four", "five"} {
		_, err := w.Write([]byte(l + "\n"))
		require.NoError(t, err)
	}
	assert.Equal(t, "three\nfour\nfive", w.String(), "only the last max lines are retained")

	// Blank lines are noise; an unterminated final line still counts (docker does not always end its last write with a newline).
	w2 := &tailWriter{max: 5}
	_, err := w2.Write([]byte("first\n\n   \nlast-no-newline"))
	require.NoError(t, err)
	assert.Equal(t, "first\nlast-no-newline", w2.String())
}
