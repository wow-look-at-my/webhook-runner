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
// returns the tag to run plus whether a build actually happened. The
// content-hash tag is what makes runs immutable: a changed hook gets a
// fresh build on its next run, an unchanged one reuses the existing
// image, and in-flight runs keep the image they started with. Build
// output is streamed to out.
func EnsureImage(dockerBin string, hook *hooks.Hook, out io.Writer) (tag string, built bool, err error) {
	tag, err = ImageTag(hook)
	if err != nil {
		return "", false, err
	}
	if exec.Command(dockerBin, "image", "inspect", tag).Run() == nil {
		return tag, false, nil
	}
	cmd := exec.Command(dockerBin, "build", "-t", tag, hook.Dir())
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("docker build %s: %w", tag, err)
	}
	removeSupersededImages(dockerBin, hook.ID, tag)
	return tag, true, nil
}

// ImageInfo describes one locally present build of a hook's image.
type ImageInfo struct {
	Tag     string `json:"tag"`
	ID      string `json:"id"`
	Size    string `json:"size"`
	Created string `json:"created"`
	Current bool   `json:"current"` // matches the hook's current content hash
}

// ImageStatus is the per-hook image state exposed on the admin port: the
// tag the hook's current content resolves to, whether that image already
// exists (false = the next run or test will build it), and every
// whr-hook/<id> image currently on disk.
type ImageStatus struct {
	HookID string      `json:"hook_id"`
	Tag    string      `json:"tag,omitempty"`
	Built  bool        `json:"built"`
	Images []ImageInfo `json:"images,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// ImageStatus reports image state for the given hooks (sorted by caller).
func (r *Runner) ImageStatus(hs []*hooks.Hook) []ImageStatus {
	out := make([]ImageStatus, 0, len(hs))
	for _, h := range hs {
		st := ImageStatus{HookID: h.ID}
		tag, err := ImageTag(h)
		if err != nil {
			st.Error = err.Error()
			out = append(out, st)
			continue
		}
		st.Tag = tag
		st.Built = exec.Command(r.dockerBin, "image", "inspect", tag).Run() == nil
		st.Images = listHookImages(r.dockerBin, h.ID, tag)
		out = append(out, st)
	}
	return out
}

func listHookImages(dockerBin, id, current string) []ImageInfo {
	raw, err := exec.Command(dockerBin, "image", "ls",
		"--format", "{{.Repository}}:{{.Tag}}\t{{.ID}}\t{{.Size}}\t{{.CreatedSince}}",
		imageRepoPrefix+id).Output()
	if err != nil {
		return nil
	}
	var infos []ImageInfo
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) < 4 || parts[0] == "" {
			continue
		}
		infos = append(infos, ImageInfo{
			Tag:     parts[0],
			ID:      parts[1],
			Size:    parts[2],
			Created: parts[3],
			Current: parts[0] == current,
		})
	}
	return infos
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
