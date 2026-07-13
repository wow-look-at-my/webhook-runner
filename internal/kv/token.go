package kv

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
)

// Token mints a bearer token that authorizes access to exactly one namespace
// ON BEHALF OF exactly one run. It is a stateless HMAC —
// "<ns>.<runID>.<base64url(HMAC-SHA256(secret, ns+"\n"+runID))>" — so there
// is nothing to store or expire server-side: the runner mints a fresh token
// per run, and the state API recovers both identities by verifying the MAC.
// Because only the server holds the secret, a hook cannot forge a token for
// another hook's namespace — nor for another run: the run identity is what
// binds cooperative locks to their holder (AcquireLock/ReleaseLock) and what
// lets the runner free a finished run's locks at the tracker's finish seam.
//
// Neither identity can contain '.' (hook IDs are lowercase [a-z0-9-], run
// IDs are lowercase base32), so the dots are unambiguous separators; the
// '\n' in the MAC input keeps (ns, runID) pairs from colliding across the
// boundary.
func (s *Store) Token(namespace, runID string) string {
	return namespace + "." + runID + "." + base64.RawURLEncoding.EncodeToString(tokenMAC(s.secret, namespace, runID))
}

func tokenMAC(secret []byte, namespace, runID string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(namespace))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(runID))
	return mac.Sum(nil)
}

// VerifyToken returns the namespace a token authorizes and the run identity
// it was minted for, or ok=false for any malformed, tampered, or
// wrong-secret token (including the pre-run-identity two-part format — the
// runner mints per run, so no compatibility shim is kept). The comparison is
// constant-time.
func (s *Store) VerifyToken(token string) (namespace, runID string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	presented, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", "", false
	}
	if subtle.ConstantTimeCompare(presented, tokenMAC(s.secret, parts[0], parts[1])) != 1 {
		return "", "", false
	}
	return parts[0], parts[1], true
}
