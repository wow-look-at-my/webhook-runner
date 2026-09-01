package runner

// Per-run temp-file plumbing — split from runner.go for the -line cap.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

// writeTempFiles materializes the payload, the headers and the hook's own
// settings document in a temp dir dedicated to this run. The returned cleanup
// removes the directory.
//
// Settings are written per RUN, not baked into the image: the image content
// hash covers the hook's source, so config that lived in the image could only
// change by rebuilding it. A settings edit takes effect on the next run.
func (r *Runner) writeTempFiles(runID string, payload []byte, headers http.Header, settings []byte) (payloadPath, headersPath, settingsPath string, cleanup func(), err error) {
	dir, err := os.MkdirTemp(r.tmpDir, "wh-"+runID+"-")
	if err != nil {
		return "", "", "", func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	payloadPath = filepath.Join(dir, "payload")
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		cleanup()
		return "", "", "", func() {}, err
	}
	headersPath = filepath.Join(dir, "headers.json")
	hb, err := json.MarshalIndent(headers, "", "  ")
	if err != nil {
		cleanup()
		return "", "", "", func() {}, err
	}
	if err := os.WriteFile(headersPath, hb, 0o600); err != nil {
		cleanup()
		return "", "", "", func() {}, err
	}
	settingsPath = filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settingsPath, settings, 0o600); err != nil {
		cleanup()
		return "", "", "", func() {}, err
	}
	// Loosen perms so the in-container user can read the files even if the container runs as a non-root user that doesn't share UID with the host.
	_ = os.Chmod(dir, 0o755)
	_ = os.Chmod(payloadPath, 0o644)
	_ = os.Chmod(headersPath, 0o644)
	_ = os.Chmod(settingsPath, 0o644)
	return payloadPath, headersPath, settingsPath, cleanup, nil
}
