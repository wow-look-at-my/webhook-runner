package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"github.com/wow-look-at-my/testify/require"
)

func TestVerify(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	secret := "whsec_test"

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	cases := []struct {
		name	string
		header	string
		ok	bool
	}{
		{"github-style", "sha256=" + want, true},
		{"bare-hex", want, true},
		{"github-style-uppercase-prefix", "SHA256=" + want, true},
		{"wrong-algo-prefix", "sha512=" + want, false},
		{"truncated", want[:30], false},
		{"empty", "", false},
		{"invalid-hex", "sha256=zzzz", false},
		{"wrong-mac", "sha256=" + reverseHex(want), false},
		{"wrong-secret", "sha256=" + macHex([]byte("other"), body), false},
		{"wrong-body", "sha256=" + macHex([]byte(secret), []byte("other")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Verify(body, tc.header, secret)
			require.Equal(t, tc.ok, got)

		})
	}
}

func macHex(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func reverseHex(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		out[i] = s[len(s)-1-i]
	}
	return string(out)
}
