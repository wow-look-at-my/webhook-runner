// Package ts0gen regenerates the dashboard's TypeScript build artifacts.
// It is invoked via the //go:generate directive in
// internal/server/dashboard/dashboard.go — `go run` of the ignore-tagged
// main.go stub next to this file — and therefore runs with that package
// directory as its working directory (it refuses to run anywhere else —
// the ts0.json check below).
//
// Deliberately a LIBRARY plus an //go:build ignore stub, NOT a normal
// main package: go-toolchain builds (and CI's matrix cross-compiles +
// autorelease publishes to buildhost) every main package it finds, and a
// second binary would both waste six cross-builds per CI run and flip the
// repo's buildhost release from the flat single-binary layout to
// per-binary namespaced projects — a release-layout change no dashboard
// tool should cause. The stub-by-file-path form is the stdlib's own
// generator convention (`go run` on explicitly named files ignores build
// constraints), and keeping the logic in a regular package keeps it under
// vet and `go test ./...`.
//
// What it does, in order:
//
//  1. Downloads the PINNED prebuilt ts0 bundle from buildhost
//     (https://dl.pazer.build/ts0 — a platform-neutral, Node-run .cjs;
//     the download is anonymous) into the user cache dir, skipping the
//     download when that version is already cached.
//  2. Re-fetches the <timeline-view> component's type declarations from
//     js-snippets' GitHub Pages into ts/js-snippets/ (committed, with a
//     DO-NOT-EDIT provenance header). The write is skipped when only the
//     fetch date would change, so regenerating against an unchanged
//     upstream is a no-op for git — and an upstream API change shows up
//     as a real diff (CI's freshness gate turns red by design).
//  3. Runs `node <cached ts0> build` here (type-check + bundle per
//     ts0.json, emitting assets/timeline.js).
//
// Node >= 22 is the only prerequisite (ts0 is a Node bundle; on first run
// it self-extracts and fetches its esbuild native into TS0_CACHE_DIR ||
// ~/.cache/ts0/). No npm, npx, or git auth anywhere.
package ts0gen

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ts0Version pins the buildhost release of ts0 used for regeneration.
//
// To bump: change this constant and run the generator (`go generate
// ./internal/server/dashboard`, or go-toolchain with the approval flag),
// then commit the regenerated outputs. The //go:generate directive text
// does NOT change on a pin bump, so the go-toolchain --generate approval
// hash survives it — only edits to the directive line or dashboard.go's
// package doc comment change that hash.
const ts0Version = 2

const (
	// ts0BaseURL is buildhost's download endpoint for the ts0 project.
	ts0BaseURL = "https://dl.pazer.build/ts0"
	// pagesBaseURL is where js-snippets' GitHub Pages serves the
	// <timeline-view> module and its .d.ts siblings.
	pagesBaseURL = "https://wow-look-at-my.github.io/js-snippets/ui/"
	// declDir is where the fetched declarations land, relative to the
	// dashboard package directory (this command's working directory).
	declDir = "ts/js-snippets"
	// minNodeMajor is the minimum Node.js major version ts0 supports.
	minNodeMajor = 22
)

// declFiles are the declaration files fetched from Pages into declDir.
// timeline-view.d.ts imports './timeline-view-math.ts' — TypeScript
// resolves that specifier to the sibling .d.ts via extension
// substitution, so both files must sit side by side under these names.
var declFiles = []string{"timeline-view.d.ts", "timeline-view-math.d.ts"}

// Run executes the full regeneration (see the package comment). It must
// be called with internal/server/dashboard as the working directory.
func Run() error {
	if _, err := os.Stat("ts0.json"); err != nil {
		wd, _ := os.Getwd()
		return fmt.Errorf("no ts0.json in %s — ts0gen must run from internal/server/dashboard (the //go:generate directive in dashboard.go does)", wd)
	}

	node, err := findNode()
	if err != nil {
		return err
	}

	ts0, err := ensureTS0()
	if err != nil {
		return err
	}

	if err := fetchDecls(); err != nil {
		return err
	}

	cmd := exec.Command(node, ts0, "build")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ts0 build failed: %w", err)
	}
	return nil
}

// findNode locates the node binary and enforces the minimum version.
func findNode() (string, error) {
	node, err := exec.LookPath("node")
	if err != nil {
		return "", fmt.Errorf("node not found on PATH — ts0 is a Node bundle and needs Node >= %d (install it, e.g. actions/setup-node in CI)", minNodeMajor)
	}
	out, err := exec.Command(node, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("node --version failed: %w", err)
	}
	major, err := nodeMajor(string(out))
	if err != nil {
		return "", fmt.Errorf("cannot parse node version %q: %w", strings.TrimSpace(string(out)), err)
	}
	if major < minNodeMajor {
		return "", fmt.Errorf("node %s is too old — ts0 needs Node >= %d", strings.TrimSpace(string(out)), minNodeMajor)
	}
	return node, nil
}

// nodeMajor parses the major version out of `node --version` output
// (e.g. "v22.22.2\n" -> 22).
func nodeMajor(version string) (int, error) {
	m := regexp.MustCompile(`^v?(\d+)\.`).FindStringSubmatch(strings.TrimSpace(version))
	if m == nil {
		return 0, fmt.Errorf("no major version found")
	}
	return strconv.Atoi(m[1])
}

// ts0URL is the buildhost download URL for the pinned ts0 version on the
// given platform. ts0 is published as a platform-neutral bundle with an
// any-platform alias, so GOOS/GOARCH pass through as buildhost's os/arch
// parameters (the names coincide) and every platform resolves to it.
func ts0URL(version int, goos, goarch string) string {
	return fmt.Sprintf("%s?v=%d&os=%s&arch=%s", ts0BaseURL, version, goos, goarch)
}

// ts0CachePath is where the downloaded ts0 bundle lives. The version is
// part of the path, so a pin bump naturally re-downloads. The .cjs
// extension is load-bearing: a .js file would be mis-parsed as ESM when a
// package.json with "type": "module" is in scope.
func ts0CachePath(cacheRoot string, version int) string {
	return filepath.Join(cacheRoot, "webhook-runner-ts0", fmt.Sprintf("v%d", version), "ts0.cjs")
}

// ensureTS0 returns the path to the cached ts0 bundle, downloading it
// first if this pinned version isn't cached yet.
func ensureTS0() (string, error) {
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		cacheRoot = filepath.Join(os.TempDir(), "webhook-runner-ts0-cache")
	}
	path := ts0CachePath(cacheRoot, ts0Version)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	url := ts0URL(ts0Version, runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(os.Stderr, "ts0gen: downloading ts0 v%d from %s\n", ts0Version, url)
	body, err := get(url)
	if err != nil {
		return "", fmt.Errorf("downloading ts0: %w", err)
	}
	if err := writeFileAtomic(path, body, 0o755); err != nil {
		return "", fmt.Errorf("caching ts0: %w", err)
	}
	return path, nil
}

// fetchDecls downloads the js-snippets type declarations into declDir,
// prefixing each with a provenance header. Files whose upstream content
// is unchanged are left alone (see sameIgnoringFetchDate), so the fetch
// date stamps when the content last changed, not when the generator last
// ran — and CI's regenerate produces no spurious diff.
func fetchDecls() error {
	date := time.Now().UTC().Format("2006-01-02")
	for _, name := range declFiles {
		url := pagesBaseURL + name
		body, err := get(url)
		if err != nil {
			return fmt.Errorf("fetching %s: %w", name, err)
		}
		content := append([]byte(declHeader(url, date)), body...)
		path := filepath.Join(declDir, name)
		if old, err := os.ReadFile(path); err == nil && sameIgnoringFetchDate(old, content) {
			fmt.Fprintf(os.Stderr, "ts0gen: %s unchanged\n", path)
			continue
		}
		if err := writeFileAtomic(path, content, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		fmt.Fprintf(os.Stderr, "ts0gen: fetched %s\n", path)
	}
	return nil
}

// fetchedPrefix marks the header line ignored by sameIgnoringFetchDate.
const fetchedPrefix = "// Fetched: "

// declHeader is the provenance header prepended to each fetched
// declaration file.
func declHeader(sourceURL, date string) string {
	return "// Code generated by ts0gen (internal/tools/ts0gen); DO NOT EDIT.\n" +
		"// Source: " + sourceURL + "\n" +
		fetchedPrefix + date + " (refreshed only when the upstream content changes)\n\n"
}

// sameIgnoringFetchDate reports whether two declaration files are equal
// apart from their "// Fetched:" header line.
func sameIgnoringFetchDate(a, b []byte) bool {
	return bytes.Equal(stripFetchedLines(a), stripFetchedLines(b))
}

func stripFetchedLines(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	kept := lines[:0]
	for _, l := range lines {
		if !bytes.HasPrefix(l, []byte(fetchedPrefix)) {
			kept = append(kept, l)
		}
	}
	return bytes.Join(kept, []byte("\n"))
}

// get fetches a URL, returning the body on HTTP 200 and an error
// otherwise.
func get(url string) ([]byte, error) {
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// writeFileAtomic writes data to path via a temp file + rename, creating
// parent directories as needed.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
