package runner

import (
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// imageRepoPrefix namespaces the locally built hook images.
const imageRepoPrefix = "whr-hook/"

// ImageTag returns the local image tag for a Dockerfile hook at its
// current content: whr-hook/<id>:<content-hash>. Hook IDs are directory
// names (lowercase kebab-case by convention), which are valid repo names.
func ImageTag(hook *hooks.Hook) (string, error) {
	h, err := hook.ContentHash()
	if err != nil {
		return "", err
	}
	return imageRepoPrefix + hook.ID + ":" + h, nil
}

// EnsureImage makes sure the image for the hook's current content exists
// locally, building it from the hook directory when it doesn't, and
// returns the tag to run. The content-hash tag is what makes runs
// immutable: a changed hook gets a fresh build on its next run, an
// unchanged one reuses the existing image, and in-flight runs keep the
// image they started with. Build output is streamed to out.
func EnsureImage(dockerBin string, hook *hooks.Hook, out io.Writer) (string, error) {
	tag, err := ImageTag(hook)
	if err != nil {
		return "", err
	}
	if exec.Command(dockerBin, "image", "inspect", tag).Run() == nil {
		return tag, nil
	}
	cmd := exec.Command(dockerBin, "build", "-t", tag, hook.Dir())
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker build %s: %w", tag, err)
	}
	removeSupersededImages(dockerBin, hook.ID, tag)
	return tag, nil
}

// removeSupersededImages deletes older builds of the hook's image,
// best-effort: an image still backing a running container makes rmi fail,
// which is fine — it gets another chance after the next rebuild.
func removeSupersededImages(dockerBin, id, keep string) {
	out, err := exec.Command(dockerBin, "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", imageRepoPrefix+id).Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || line == keep {
			continue
		}
		_ = exec.Command(dockerBin, "rmi", line).Run()
	}
}

// slogLineWriter adapts an io.Writer contract to per-line logging via the
// supplied log function (used to surface docker build output).
type slogLineWriter struct {
	logFn func(line string)
	buf   []byte
}

func (w *slogLineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimRight(string(w.buf[:i]), "\r"); line != "" {
			w.logFn(line)
		}
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}
