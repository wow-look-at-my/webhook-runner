package server

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

func (s *Server) authenticate(hook *hooks.Hook, r *http.Request, body []byte) error {
	switch {
	case hook.APIKey != "":
		return checkAPIKey(hook, r)
	case hook.PublicKey != "":
		return checkPublicKey(hook, r, body)
	case hook.Secret != "":
		return checkLegacyHMAC(hook, r, body)
	default:
		return nil
	}
}

func checkAPIKey(hook *hooks.Hook, r *http.Request) error {
	got := r.Header.Get(hook.APIKeyHdr())
	if subtle.ConstantTimeCompare([]byte(got), []byte(hook.APIKey)) != 1 {
		return errors.New("invalid api key")
	}
	return nil
}

func checkPublicKey(hook *hooks.Hook, r *http.Request, body []byte) error {
	pubKey, err := decodePublicKey(hook.PublicKey)
	if err != nil {
		return errors.New("server misconfigured: bad public key")
	}

	sigHeader := r.Header.Get(hook.SigHeader())
	sig, err := decodeSig(sigHeader)
	if err != nil {
		return errors.New("invalid signature encoding")
	}

	if !ed25519.Verify(pubKey, body, sig) {
		return errors.New("invalid signature")
	}
	return nil
}

func checkLegacyHMAC(hook *hooks.Hook, r *http.Request, body []byte) error {
	got := r.Header.Get(hook.SigHeader())
	if !verifyLegacyHMAC(body, got, hook.Secret) {
		return errors.New("invalid signature")
	}
	return nil
}

func decodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil {
		b, err = hex.DecodeString(s)
	}
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, errors.New("wrong key size")
	}
	return ed25519.PublicKey(b), nil
}

func decodeSig(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil {
		b, err = hex.DecodeString(s)
	}
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.SignatureSize {
		return nil, errors.New("wrong signature size")
	}
	return b, nil
}
