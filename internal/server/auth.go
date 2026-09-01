package server

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

func (s *Server) authenticate(hook *hooks.Hook, r *http.Request, body []byte) error {
	switch {
	case hook.APIKey != "":
		return s.checkAPIKey(hook, r)
	case hook.PublicKey != "":
		return checkPublicKey(hook, r, body)
	case hook.Secret != "":
		return checkLegacyHMAC(hook, r, body)
	default:
		return nil
	}
}

func (s *Server) checkAPIKey(hook *hooks.Hook, r *http.Request) error {
	// api_key may reference a secret as ${NAME} — resolved from the hook's sops secrets file , then the host environment — so the real key.
	var secrets map[string]string
	if s.secrets != nil {
		var err error
		secrets, err = s.secrets.Load(hook)
		if err != nil {
			s.log.Error("hook secrets unavailable for api_key check", "hook", hook.ID, "err", err)
			return errors.New("server misconfigured: hook secrets unavailable")
		}
	}
	want, missing := hooks.ExpandEnvRefs(hook.APIKey, hooks.SecretsFirstLookup(secrets))
	if want == "" {
		// Every caller is about to get a because of *server-side* config,
		// not bad credentials. Name the broken reference where the operator
		// looks (log + dashboard event) — but keep the body generic; an
		// anonymous caller gets no config detail.
		if len(missing) > 0 {
			s.log.Error("hook api_key reference unresolvable; denying all callers",
				"hook", hook.ID, "missing", missing)
			s.events.Record("hook.misconfigured",
				hook.ID+": api_key reference ${"+strings.Join(missing, "}, ${")+"} did not resolve (secrets.sops.env / host env); all callers are denied",
				map[string]string{"hook": hook.ID})
		}
		return errors.New("api key not configured on the server")
	}
	got := r.Header.Get(hook.APIKeyHdr())
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
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
