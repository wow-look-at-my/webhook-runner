// Legacy HMAC-SHA256 webhook signature verification.
//
// This implements the GitHub-style "sha256=<hex>" signature scheme.
// Prefer api_key for new hooks — it is simpler to configure and debug.
package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func verifyLegacyHMAC(body []byte, header, secret string) bool {
	got := strings.TrimSpace(header)
	if i := strings.IndexByte(got, '='); i >= 0 {
		if !strings.EqualFold(got[:i], "sha256") {
			return false
		}
		got = got[i+1:]
	}
	gotBytes, err := hex.DecodeString(got)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	return hmac.Equal(gotBytes, want)
}
