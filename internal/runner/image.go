package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// imageRepoPrefix namespaces the locally built hook images.
const imageRepoPrefix = "whr-hook/"

// buildTailLines is how much build output a failed build carries back in its
// error. Without it a build failure reads as a bare "exit status 1", with the
// real docker error only in the server's own log — invisible on the dashboard,
// which is where the operator looks.
const buildTailLines = 40

// tailWriter keeps the last N lines written to it and nothing else, so a
// failure path can quote what a subprocess actually printed without buffering
// an entire build log.
type tailWriter struct {
	max   int
	lines []string
	buf   []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.add(strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *tailWriter) add(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	w.lines = append(w.lines, line)
	if len(w.lines) > w.max {
		w.lines = w.lines[len(w.lines)-w.max:]
	}
}

// String returns the retained tail, including any unterminated final line.
func (w *tailWriter) String() string {
	lines := w.lines
	if rest := strings.TrimRight(string(w.buf), "\r"); strings.TrimSpace(rest) != "" {
		lines = append(append([]string{}, lines...), rest)
		if len(lines) > w.max {
			lines = lines[len(lines)-w.max:]
		}
	}
	return strings.Join(lines, "\n")
}

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
	// Legacy hooks build from their own directory with its Dockerfile (the
	// docker default — invocation unchanged). SDK-layout hooks build with
	// the repo's src/ directory as context and the hook's own Dockerfile
	// via -f, so tree-mirror COPYs (sdk/ + hooks/<id>/) resolve.
	args := []string{"build", "-t", tag}
	if hook.SDKLayout() {
		args = append(args, "-f", filepath.Join(hook.Dir(), hooks.DockerfileName))
	}
	args = append(args, hook.BuildContext())
	cmd := exec.Command(dockerBin, args...)
	// Select BuildKit explicitly. Without this the CLI falls back to the
	// LEGACY builder whenever the buildx plugin is absent — and the legacy
	// parser rejects `# syntax=` frontends and flags like `ADD --unpack`
	// with "dockerfile parse error: unknown flag", no matter what the host
	// daemon supports. Hook Dockerfiles are written against BuildKit, so
	// this must not depend on which plugins the runtime image happens to
	// carry.
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	// Tee the build output: `out` is the live stream, `tail` retains the
	// last lines so the FAILURE below can carry the actual docker error.
	tail := &tailWriter{max: buildTailLines}
	cmd.Stdout = io.MultiWriter(out, tail)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("docker build %s: %w\n%s", tag, err, tail.String())
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

// imageCommandCache memoizes imageCommand's answer. An image TAG is a
// content hash (see ImageTag), so a given tag's ENTRYPOINT/CMD can never
// change — the answer is immutable for the life of the process, and every
// state-hook run was paying a full docker CLI + daemon round trip to
// re-derive it on the critical path between slot acquisition and container
// launch.
//
// The key includes the hook's command override as well as the tag. That is
// belt-and-braces: hook.json lives inside the hashed content, so a changed
// `command` already yields a different tag. Keying on both means the cache
// stays correct without depending on that, and costs one string join.
//
// Unbounded by design, and bounded in practice: entries are one small
// []string per distinct (tag, command), and new tags only appear when a
// hook's content changes — the same event that builds a new image. A server
// that accumulated enough of these to matter would have filled its disk with
// images first.
var imageCommandCache sync.Map // string -> []string

// imageCommand reconstructs the argv an image would run — its ENTRYPOINT plus
// CMD, or ENTRYPOINT plus hookCommand when the hook overrides the command — via
// docker inspect. State hooks set the KV shim as the container entrypoint, so
// the shim must be handed the original command to exec after starting the proxy.
//
// Cached per (image tag, hook command): see imageCommandCache. Errors are
// never cached — a failed inspect is a transient daemon condition, not a
// property of the tag.
func imageCommand(dockerBin, image string, hookCommand []string) ([]string, error) {
	// \x00 cannot appear in an argv element or a docker tag, so it cannot
	// make two different keys collide.
	key := image + "\x00" + strings.Join(hookCommand, "\x00")
	if cached, ok := imageCommandCache.Load(key); ok {
		// Copy: callers append the shim's own argv onto the result, which
		// would otherwise write into the cached slice's spare capacity and
		// corrupt the next run's command.
		return append([]string(nil), cached.([]string)...), nil
	}
	out, err := exec.Command(dockerBin, "inspect", image,
		"--format", "{{json .Config.Entrypoint}}\n{{json .Config.Cmd}}").Output()
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
	var entrypoint, cmd []string
	_ = json.Unmarshal([]byte(parts[0]), &entrypoint)
	if len(parts) > 1 {
		_ = json.Unmarshal([]byte(parts[1]), &cmd)
	}
	tail := hookCommand
	if len(tail) == 0 {
		tail = cmd
	}
	argv := append(append([]string{}, entrypoint...), tail...)
	if len(argv) == 0 {
		return nil, errors.New("image declares no entrypoint or cmd and the hook sets no command")
	}
	imageCommandCache.Store(key, append([]string(nil), argv...))
	return argv, nil
}
