//go:build ignore

// Command gen rebuilds assets/timeline.js from the TypeScript in ts/, by
// running ts0 the way ts0's own README prescribes for build wiring: fetch the
// pinned prebuilt bundle, run it with the local Node.
//
// see docs/timeline-bundle-generation.md
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ts0Version pins an immutable buildhost release, which is what makes the
// output byte-reproducible. Bump it and commit the regenerated bundle together.
const ts0Version = "5"

// ts0.cjs is platform-neutral -- buildhost addresses artifacts by os/arch, so
// the parameters are required, but every supported pair returns identical bytes.
const ts0URL = "https://dl.pazer.build/ts0?v=" + ts0Version + "&os=linux&arch=amd64"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	// ts0 resolves ts0.json from the working directory, so running this from
	// anywhere else silently builds the wrong thing. Say so instead.
	if _, err := os.Stat("ts0.json"); err != nil {
		return fmt.Errorf("no ts0.json here; run `go generate ./internal/server/dashboard/`")
	}
	ts0, err := ts0Bundle()
	if err != nil {
		return err
	}
	// `ts0 build` reads ts0.json from the working directory, which go:generate
	// sets to this package -- so it rewrites assets/timeline.js in place.
	cmd := exec.Command("node", ts0, "build")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ts0 build (needs Node 22+ on PATH): %w", err)
	}
	return nil
}

// ts0Bundle returns a local path to the pinned bundle, downloading it once.
// WHR_TS0_CJS points at a pre-fetched copy instead, for offline use.
func ts0Bundle() (string, error) {
	if p := os.Getenv("WHR_TS0_CJS"); p != "" {
		return p, nil
	}
	dest := filepath.Join(os.TempDir(), "whr-ts0-v"+ts0Version+".cjs")
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	fmt.Fprintln(os.Stderr, "gen: fetching", ts0URL)
	if err := download(ts0URL, dest); err != nil {
		return "", fmt.Errorf("fetch ts0: %w", err)
	}
	return dest, nil
}

// download writes url to dest via a temp file, so an interrupted fetch cannot
// leave a truncated bundle that the next run would happily execute.
func download(url, dest string) error {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), "ts0-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}
