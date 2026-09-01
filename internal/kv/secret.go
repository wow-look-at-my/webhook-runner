package kv

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureSecret returns the HMAC secret used to mint namespace tokens, reading
// it from path or generating (and persisting) a fresh -byte secret if the
// file does not exist. It persists like the git deploy key
// (hooks.EnsureSSHKey) but is pure-Go random bytes — an HMAC key needs no
// external tooling, so we never shell out to ssh-keygen here.
func EnsureSecret(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(b) == 0 {
			return nil, fmt.Errorf("kv: state secret at %s is empty", path)
		}
		return b, nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("kv: read state secret: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("kv: create state dir: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("kv: generate state secret: %w", err)
	}
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		return nil, fmt.Errorf("kv: write state secret: %w", err)
	}
	return secret, nil
}
