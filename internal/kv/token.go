package kv

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
)

// Token mints a bearer token that authorizes access to exactly namespace ON BEHALF OF exactly run.
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
// wrong-secret token (including the pre-run-identity -part format — the
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
