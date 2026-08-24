package hooks

import (
	"fmt"
	"path/filepath"
	"strings"
)

// validateScratch rejects a mount entry that could not be applied, or that
// would mount somewhere destructive. A relative path has no meaning as a mount
// destination; "/" would replace the whole container rootfs; a destination
// named twice — within one list or across both — is two mounts on one target,
// which docker refuses. Catching all of it at LOAD keeps it out of a running
// fleet entirely.
func (h *Hook) validateScratch() error {
	seen := make(map[string]string, len(h.Scratch)+len(h.Tmpfs))
	for _, list := range []struct {
		field string
		paths []string
	}{{"scratch", h.Scratch}, {"tmpfs", h.Tmpfs}} {
		for i, entry := range list.paths {
			p := entry
			if list.field == "tmpfs" {
				p = TmpfsPath(entry)
				if p == entry && strings.Contains(entry, ":") {
					return fmt.Errorf("tmpfs[%d] %q has an empty option list after %q", i, entry, ":")
				}
			}
			if !strings.HasPrefix(p, "/") {
				return fmt.Errorf("%s[%d] %q must be an absolute container path", list.field, i, entry)
			}
			clean := filepath.Clean(p)
			if clean != p {
				return fmt.Errorf("%s[%d] %q must be a clean path (%q)", list.field, i, entry, clean)
			}
			if clean == "/" {
				return fmt.Errorf("%s[%d] must not be %q", list.field, i, "/")
			}
			if prev, dup := seen[clean]; dup {
				return fmt.Errorf("%s[%d] %q is already mounted by %s", list.field, i, entry, prev)
			}
			seen[clean] = list.field
		}
	}
	return nil
}

// TmpfsPath returns the mount destination of a tmpfs entry, which may carry
// docker's option suffix ("/tmp:size=4g"). Options are passed through
// untouched — docker owns that grammar, and validating a copy of it here would
// only reject options docker gains later.
//
// Sizing is not cosmetic: an unbounded tmpfs may grow to half of host RAM, and
// several of them on one container can exhaust it. A path expected to hold
// gigabytes should name a size.
func TmpfsPath(entry string) string {
	if i := strings.IndexByte(entry, ':'); i >= 0 && i < len(entry)-1 {
		return entry[:i]
	}
	return entry
}

// ScratchCovers reports whether the hook declared dst as a scratch path.
func (h *Hook) ScratchCovers(dst string) bool {
	for _, p := range h.Scratch {
		if p == dst {
			return true
		}
	}
	return false
}
