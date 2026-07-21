//go:build ignore

// Command gen regenerates the dashboard's embedded timeline bundle,
// assets/timeline.js, from its TypeScript source in ts/.
//
// It fetches the prebuilt, platform-neutral ts0 bundle (ts0.cjs) from the org
// buildhost — pinned to an immutable release so the output is byte-reproducible
// — and runs it with the local Node.js runtime (Node 22+, ts0's only external
// requirement per its own packaging). No npm, no npx, no node_modules, no git.
// ts0 downloads its one native piece (esbuild) into its own cache on first run.
//
// This program is excluded from the normal build (the //go:build ignore tag),
// so a plain `go build`/`go test` never compiles it and the committed bundle
// stays the source of truth for go:embed. It is invoked two ways:
//
//   - the //go:generate directive in generate.go (working directory = this
//     package dir), e.g. `go generate ./internal/server/dashboard/`; or
//   - directly: `go run internal/server/dashboard/gen.go`.
//
// CI runs it through go-toolchain's approved generate step and then fails on a
// dirty working tree, so a committed bundle that is stale versus ts/ turns CI
// red instead of silently drifting.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// ts0Version pins the buildhost ts0 release used to build the bundle. Pinning
// an immutable release (?v=N) is what makes regeneration byte-reproducible: the
// same ts0 — and thus the same inlined esbuild — always produces the same
// bytes. To bump it, change this constant, run the generate directive, and
// commit the (possibly changed) assets/timeline.js in the same change.
const ts0Version = "5"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	dashDir, err := packageDir()
	if err != nil {
		return err
	}
	if !fileExists(filepath.Join(dashDir, "ts0.json")) {
		return fmt.Errorf("ts0.json not found in %s (run via `go generate ./internal/server/dashboard/`)", dashDir)
	}

	ts0, err := ts0Bundle()
	if err != nil {
		return err
	}

	node, err := exec.LookPath("node")
	if err != nil {
		return fmt.Errorf("node not found on PATH (Node.js 22+ is required to run ts0): %w", err)
	}

	// `ts0 build` reads ts0.json from its working directory (entry ts/timeline.ts,
	// outdir assets), so it regenerates assets/timeline.js in place.
	cmd := exec.Command(node, ts0, "build")
	cmd.Dir = dashDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ts0 build failed: %w", err)
	}
	return nil
}

// packageDir returns the dashboard package directory (where ts0.json, ts/, and
// assets/ live), independent of the caller's working directory. It prefers this
// file's own location so `go run internal/server/dashboard/gen.go` works from
// the repo root too, and falls back to the working directory (the //go:generate
// contract runs the directive there).
func packageDir() (string, error) {
	if _, self, _, ok := runtime.Caller(0); ok {
		if dir := filepath.Dir(self); fileExists(filepath.Join(dir, "ts0.json")) {
			return dir, nil
		}
	}
	return os.Getwd()
}

// ts0Bundle returns a local path to the pinned ts0 bundle, downloading and
// caching it on first use. Set WHR_TS0_CJS to point at a pre-fetched bundle
// (offline use); WHR_TS0_VERSION overrides the pinned version.
func ts0Bundle() (string, error) {
	if p := os.Getenv("WHR_TS0_CJS"); p != "" {
		return p, nil
	}
	version := ts0Version
	if v := os.Getenv("WHR_TS0_VERSION"); v != "" {
		version = v
	}
	cacheDir := filepath.Join(os.TempDir(), "whr-ts0-cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(cacheDir, "ts0-v"+version+".cjs")
	if fileExists(dest) {
		return dest, nil
	}
	url := ts0URL(version, runtime.GOOS, runtime.GOARCH)
	fmt.Fprintln(os.Stderr, "gen: fetching ts0 "+url)
	if err := download(url, dest); err != nil {
		return "", fmt.Errorf("fetch ts0 from %s: %w", url, err)
	}
	return dest, nil
}

// ts0URL builds the buildhost download URL for the pinned ts0 bundle. The
// bundle is platform-neutral (buildhost serves identical bytes for every
// os/arch), but buildhost addresses artifacts by os/arch, so the parameters are
// required. Unrecognized GOOS/GOARCH values fall back to a known-good pair —
// the bytes are the same regardless.
func ts0URL(version, goos, goarch string) string {
	osParam := goos
	switch osParam {
	case "linux", "darwin", "windows":
	default:
		osParam = "linux"
	}
	archParam := goarch
	switch archParam {
	case "amd64", "arm64":
	default:
		archParam = "amd64"
	}
	return fmt.Sprintf("https://dl.pazer.build/ts0?v=%s&os=%s&arch=%s", version, osParam, archParam)
}

// download fetches url into dest atomically (temp file + rename). It follows
// redirects (buildhost's dl endpoint redirects to static storage) via the
// default http client policy.
func download(url, dest string) error {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
