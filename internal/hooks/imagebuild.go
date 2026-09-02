package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DockerfileName is the file every hook must ship next to its hook.json: hooks run images built from their own directory, code baked in.
const DockerfileName = "Dockerfile"

func (h *Hook) hasDockerfile() bool {
	dir := h.Dir()
	if dir == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, DockerfileName))
	return err == nil && !fi.IsDir()
}

// BaseDir is the absolute directory of the base image this entity declares, or "" when it declares none.
func (h *Hook) BaseDir() string {
	if h.Base == "" || h.SrcRoot == "" {
		return ""
	}
	return filepath.Join(h.SrcRoot, BaseDirName, h.Base)
}

// checkBase fails a manifest whose base cannot be built, at LOAD: the
// alternative is a build that dies on an unresolvable FROM when a delivery
// arrives, which is the same error later and against a live webhook. A name
// is a single path segment, so a manifest cannot reach outside src/base/ to
// pick up an entity directory or anything above the tree.
func (h *Hook) checkBase() error {
	if h.Base == "" {
		return nil
	}
	if h.Base != filepath.Base(h.Base) || h.Base == "." || h.Base == ".." || strings.ContainsRune(h.Base, '/') {
		return fmt.Errorf("base %q must be a single directory name under src/%s", h.Base, BaseDirName)
	}
	if h.SrcRoot == "" {
		return errors.New("base needs the src layout: a legacy hook builds from its own directory and has no shared tree to hold one")
	}
	dockerfile := filepath.Join(h.BaseDir(), DockerfileName)
	if fi, err := os.Stat(dockerfile); err != nil || fi.IsDir() {
		return fmt.Errorf("base %q declares no %s at src/%s/%s/%s", h.Base, DockerfileName, BaseDirName, h.Base, DockerfileName)
	}
	return nil
}

// DirHash digests a single directory by itself, in the form ContentHash uses for a src-layout tree: relative path, mode, content.
func DirHash(dir string) (string, error) {
	digest := sha256.New()
	if err := hashTree(digest, dir, dir, true); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil))[:16], nil
}
