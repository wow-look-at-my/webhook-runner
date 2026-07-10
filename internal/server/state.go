package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/kv"
)

// maxIncrBody caps the tiny JSON body of an increment request.
const maxIncrBody = 512

// maxLockBody caps the tiny JSON body of a lock acquire request.
const maxLockBody = 512

// Explicit lock TTL bounds (whole seconds). The TTL is a SECONDARY backstop
// — run-finish release is the primary mechanism — so the range only keeps
// callers from disabling the belt entirely (0/negative) or arming one so far
// out it stops being a backstop.
const (
	minLockTTLSeconds = 1
	maxLockTTLSeconds = 3600
)

// nsHandler is a state-port handler that has already had its caller's
// namespace and run identity resolved from the bearer token.
type nsHandler func(w http.ResponseWriter, r *http.Request, ns, runID string)

// withNamespace authenticates a state-port request by its bearer token and
// resolves the namespace — and the calling run's identity — from it, never
// from the URL: a hook can only ever touch its own data, and the lock verbs
// know which run is asking without any client-managed owner tokens.
func (s *Server) withNamespace(next nsHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.kv == nil {
			writeError(w, http.StatusServiceUnavailable, "state store not configured")
			return
		}
		tok := bearerToken(r)
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		ns, runID, ok := s.kv.VerifyToken(tok)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r, ns, runID)
	}
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func (s *Server) handleKVGet(w http.ResponseWriter, r *http.Request, ns, _ string) {
	v, ok := s.kv.Get(ns, r.PathValue("key"))
	if !ok {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(v)
}

func (s *Server) handleKVPut(w http.ResponseWriter, r *http.Request, ns, _ string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(s.kv.MaxValueBytes())+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "value too large")
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	ttl, err := parseTTL(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.kv.Set(ns, r.PathValue("key"), body, ttl); err != nil {
		s.writeKVError(w, ns, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleKVDelete(w http.ResponseWriter, r *http.Request, ns, _ string) {
	if err := s.kv.Delete(ns, r.PathValue("key")); err != nil {
		s.writeKVError(w, ns, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleKVList(w http.ResponseWriter, _ *http.Request, ns, _ string) {
	writeJSON(w, http.StatusOK, map[string][]string{"keys": s.kv.List(ns)})
}

func (s *Server) handleKVIncr(w http.ResponseWriter, r *http.Request, ns, _ string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxIncrBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	delta := int64(1)
	if len(bytes.TrimSpace(body)) > 0 {
		var req struct {
			Delta *int64 `json:"delta"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
		if req.Delta != nil {
			delta = *req.Delta
		}
	}
	ttl, err := parseTTL(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := s.kv.Incr(ns, r.PathValue("key"), delta, ttl)
	if err != nil {
		s.writeKVError(w, ns, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"value": n})
}

// handleKVAcquire is the cooperative lock take: atomic under the store's
// lock-table mutex, owned by the CALLING RUN (the identity in the bearer
// token — there are no client-managed owner tokens). 200 when this run took
// or already held the lock (idempotent re-acquire); 409 when another live
// run holds it (nothing is mutated — in particular the holder's backstop
// expiry is never restamped by a contender). Optional body
// {"ttl_seconds": N} sets the secondary backstop expiry; absent, the store
// default applies (run-finish release is the primary mechanism either way).
func (s *Server) handleKVAcquire(w http.ResponseWriter, r *http.Request, ns, runID string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLockBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	ttl := time.Duration(0) // 0 = the store's DefaultLockTTL
	if len(bytes.TrimSpace(body)) > 0 {
		var req struct {
			TTLSeconds *int `json:"ttl_seconds"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
		if req.TTLSeconds != nil {
			if *req.TTLSeconds < minLockTTLSeconds || *req.TTLSeconds > maxLockTTLSeconds {
				writeError(w, http.StatusBadRequest,
					"invalid ttl_seconds: must be "+strconv.Itoa(minLockTTLSeconds)+".."+strconv.Itoa(maxLockTTLSeconds))
				return
			}
			ttl = time.Duration(*req.TTLSeconds) * time.Second
		}
	}
	info, err := s.kv.AcquireLock(ns, r.PathValue("key"), runID, ttl)
	if err != nil {
		s.writeKVError(w, ns, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleKVRelease frees a lock the calling run holds (early release — the
// runner also frees everything a run still holds when it finishes). The
// owner check is server-side, from the token's run identity: 204 released,
// 404 not held (absent or already expired), 409 held by another run. Any
// request body is ignored — there is nothing a caller could need to say.
func (s *Server) handleKVRelease(w http.ResponseWriter, r *http.Request, ns, runID string) {
	if err := s.kv.ReleaseLock(ns, r.PathValue("key"), runID); err != nil {
		s.writeKVError(w, ns, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseTTL reads the optional TTL from the X-KV-TTL header or the ?ttl= query
// parameter, in whole seconds. Absent means no expiry (0). Negative or
// non-numeric is a client error.
func parseTTL(r *http.Request) (time.Duration, error) {
	raw := r.Header.Get("X-KV-TTL")
	if raw == "" {
		raw = r.URL.Query().Get("ttl")
	}
	if raw == "" {
		return 0, nil
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("invalid ttl: must be whole seconds")
	}
	if secs < 0 {
		return 0, errors.New("invalid ttl: must not be negative")
	}
	return time.Duration(secs) * time.Second, nil
}

// writeKVError maps the store's typed errors onto HTTP status codes. The
// typed errors are the caller's fault; anything else is an internal store
// failure — in practice a failed disk persist, after which the store has
// already rolled the in-memory mutation back. Those must be loud end-to-end:
// the hook gets a 5xx carrying the reason (its write did NOT happen), the
// server log gets the error, and the activity feed gets a kv.write_failed
// event so the dashboard can answer "are state writes failing?".
func (s *Server) writeKVError(w http.ResponseWriter, ns string, err error) {
	switch {
	case errors.Is(err, kv.ErrValueTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, kv.ErrTooManyKeys), errors.Is(err, kv.ErrTooManyNS):
		writeError(w, http.StatusInsufficientStorage, err.Error())
	case errors.Is(err, kv.ErrNotInteger):
		writeError(w, http.StatusConflict, err.Error())
	// Lock contention/ownership outcomes are normal control flow for the
	// caller (409/404), never write failures — no log, no event.
	case errors.Is(err, kv.ErrLockHeld):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, kv.ErrLockNotHeld):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, kv.ErrBadNamespace):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Error("kv: state write failed", "ns", ns, "err", err)
		s.events.Record("kv.write_failed", ns+": state write failed (rolled back): "+err.Error(),
			map[string]string{"hook": ns})
		writeError(w, http.StatusInternalServerError, "state store error: "+err.Error())
	}
}
