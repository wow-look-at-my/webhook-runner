package kv

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
)

// Token mints a bearer token that authorizes access to exactly one namespace.
// It is a stateless HMAC — "<ns>.<base64url(HMAC-SHA256(secret, ns))>" — so
// there is nothing to store or expire server-side: the runner mints a fresh
// token per run, and the state API recovers the namespace by verifying the
// MAC. Because only the server holds the secret, a hook cannot forge a token
// for another hook's namespace.
func (s *Store) Token(namespace string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(namespace))
	return namespace + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyToken returns the namespace a token authorizes, or ok=false for any
// malformed, tampered, or wrong-secret token. The comparison is
// constant-time. Hook IDs never contain '.', so the last '.' always separates
// the namespace from its MAC.
func (s *Store) VerifyToken(token string) (string, bool) {
	i := strings.LastIndexByte(token, '.')
	if i <= 0 || i == len(token)-1 {
		return "", false
	}
	ns := token[:i]
	presented, err := base64.RawURLEncoding.DecodeString(token[i+1:])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(ns))
	if subtle.ConstantTimeCompare(presented, mac.Sum(nil)) != 1 {
		return "", false
	}
	return ns, true
}
