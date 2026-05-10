// Package signature implements HMAC-SHA256 signature verification for
// incoming webhook requests. Both the GitHub-style "sha256=<hex>" prefix
// and a bare hex digest are accepted.
package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Verify reports whether the provided signature header value is a valid
// HMAC-SHA256 of body computed with secret. Comparison is constant-time.
//
// An empty header value or malformed hex returns false.
func Verify(body []byte, header, secret string) bool {
	got := strings.TrimSpace(header)
	if i := strings.IndexByte(got, '='); i >= 0 {
		// Accept the GitHub form "sha256=<hex>". Anything else with
		// an "=" we reject — the algorithm is fixed.
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
