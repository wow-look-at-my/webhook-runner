package runner

// Per-run temp-file plumbing — split from runner.go for the 750-line cap.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

// writeTempFiles materializes the payload and headers in a temp dir
// dedicated to this run. The returned cleanup removes the directory.
func (r *Runner) writeTempFiles(runID string, payload []byte, headers http.Header) (payloadPath, headersPath string, cleanup func(), err error) {
	dir, err := os.MkdirTemp(r.tmpDir, "wh-"+runID+"-")
	if err != nil {
		return "", "", func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	payloadPath = filepath.Join(dir, "payload")
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	headersPath = filepath.Join(dir, "headers.json")
	hb, err := json.MarshalIndent(headers, "", "  ")
	if err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	if err := os.WriteFile(headersPath, hb, 0o600); err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	// Loosen perms so the in-container user can read the files even if
	// the container runs as a non-root user that doesn't share UID with
	// the host process.
	_ = os.Chmod(dir, 0o755)
	_ = os.Chmod(payloadPath, 0o644)
	_ = os.Chmod(headersPath, 0o644)
	return payloadPath, headersPath, cleanup, nil
}
