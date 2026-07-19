package runner

// SDK-layout builds: context = <repo>/src with the hook's own Dockerfile
// via -f; legacy builds keep today's exact invocation (context = hook dir,
// no -f). Asserted against a recording mock docker.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// writeRecordingDocker fakes docker: `image inspect` reports not-built so
// EnsureImage proceeds to build, and `build` appends its full argv to
// <dir>/build.log.
func writeRecordingDocker(t *testing.T, dir string) (bin, logPath string) {
	t.Helper()
	logPath = filepath.Join(dir, "build.log")
	bin = filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"image\" ] && [ \"$2\" = \"inspect\" ]; then exit 1; fi\n" +
		"if [ \"$1\" = \"build\" ]; then echo \"$@\" >> " + logPath + "; exit 0; fi\n" +
		"exit 0\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	return bin, logPath
}

func loadOnlyHook(t *testing.T, root string) *hooks.Hook {
	t.Helper()
	loaded, errs := hooks.LoadDir(root)
	require.Empty(t, errs)
	require.Len(t, loaded, 1)
	for _, h := range loaded {
		return h
	}
	return nil
}

func writeTree(t *testing.T, base string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(base, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
}

const layoutTestHookJSON = `{"$schema": "https://x/hook.schema.json", "description": "d", "command": ["sh"], "api_key": "k"}`

func TestEnsureImageLegacyInvocationUnchanged(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"h/hook.json":  layoutTestHookJSON,
		"h/Dockerfile": "FROM alpine:3.20\n",
	})
	h := loadOnlyHook(t, root)
	dockerDir := t.TempDir()
	bin, logPath := writeRecordingDocker(t, dockerDir)

	tag, built, err := EnsureImage(bin, h, os.Stderr)
	require.NoError(t, err)
	assert.True(t, built)

	logged, err := os.ReadFile(logPath)
	require.NoError(t, err)
	line := strings.TrimSpace(string(logged))
	assert.Equal(t, "build -t "+tag+" "+h.Dir(), line,
		"legacy build invocation must stay exactly context-only (no -f)")
}

func TestEnsureImageSDKLayoutBuildsFromSrcContext(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/hooks/h/hook.json":  layoutTestHookJSON,
		"src/hooks/h/Dockerfile": "FROM alpine:3.20\nCOPY sdk/ /app/sdk/\nCOPY hooks/h/ /app/hooks/h/\n",
		"src/hooks/h/main.ts":    "console.log(1);\n",
		"src/sdk/util.ts":        "export const x = 1;\n",
	})
	h := loadOnlyHook(t, root)
	require.True(t, h.SDKLayout())
	dockerDir := t.TempDir()
	bin, logPath := writeRecordingDocker(t, dockerDir)

	tag, built, err := EnsureImage(bin, h, os.Stderr)
	require.NoError(t, err)
	assert.True(t, built)

	logged, err := os.ReadFile(logPath)
	require.NoError(t, err)
	line := strings.TrimSpace(string(logged))
	wantDockerfile := filepath.Join(h.Dir(), "Dockerfile")
	wantContext := filepath.Join(root, "src")
	assert.Equal(t, "build -t "+tag+" -f "+wantDockerfile+" "+wantContext, line,
		"sdk-layout build must use src/ as context with the hook's Dockerfile via -f")
}
