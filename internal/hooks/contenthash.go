package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ContentHash digests the files that determine this hook's image, tagging
// the build so a changed hook rebuilds on its next run while an unchanged
// one reuses the already built image.
//
// LEGACY layout: every file under the hook's directory, hashed as
// relative path + content — byte-identical to the historical algorithm
// (existing deployments must not re-tag on upgrade).
//
// SDK (src/) layout: a deterministic walk of src/hooks/<id>/ AND every
// SHARED dir (see SharedDirs — src/sdk, src/actions-runner, whatever the
// tree has) — never sibling entity dirs — hashed as src-relative path +
// file mode + content. A shared-code edit re-tags every src-layout entity
// (lazy rebuild on its next run, intended even for non-consumers); an edit
// to hook A never re-tags hook B. The COPY-surface convention follows from
// this: an SDK-layout Dockerfile may COPY only from a shared dir and its
// own hooks/<id>/ — a sibling entity's dir is undefined-staleness territory
// (builds don't fail, but edits there never re-tag).
func (h *Hook) ContentHash() (string, error) {
	dir := h.Dir()
	if dir == "" {
		return "", errors.New("hook has no source directory")
	}
	digest := sha256.New()
	if h.SrcRoot != "" {
		if err := hashTree(digest, h.SrcRoot, dir, true); err != nil {
			return "", fmt.Errorf("hash hook dir %s: %w", dir, err)
		}
		// A src tree without shared code is fine: no shared dirs simply
		// contribute nothing.
		shared, err := SharedDirs(h.SrcRoot)
		if err != nil {
			return "", fmt.Errorf("list shared dirs under %s: %w", h.SrcRoot, err)
		}
		for _, sd := range shared {
			if err := hashTree(digest, h.SrcRoot, sd, true); err != nil {
				return "", fmt.Errorf("hash shared dir %s: %w", sd, err)
			}
		}
		return hex.EncodeToString(digest.Sum(nil))[:16], nil
	}
	if err := hashTree(digest, dir, dir, false); err != nil {
		return "", fmt.Errorf("hash hook dir %s: %w", dir, err)
	}
	return hex.EncodeToString(digest.Sum(nil))[:16], nil
}

// hashTree feeds every file under root into digest, ordered by
// filepath.WalkDir's lexical walk: relative-to-base path, optionally the
// file mode (the SDK layout hashes modes; legacy predates that and must
// stay byte-identical), then the content.
func hashTree(digest io.Writer, base, root string, withMode bool) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(digest, "%s\x00", filepath.ToSlash(rel))
		if withMode {
			info, err := d.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(digest, "%o\x00", info.Mode().Perm())
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, cpErr := io.Copy(digest, f)
		f.Close()
		if cpErr != nil {
			return cpErr
		}
		fmt.Fprint(digest, "\x00")
		return nil
	})
}
