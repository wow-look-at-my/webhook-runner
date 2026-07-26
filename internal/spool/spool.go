// Package spool durably parks webhook deliveries that arrive while the
// server is draining for shutdown, and replays them once the next process
// has loaded its hooks.
//
// WHY: the drain gate refuses new runs for a real reason — a run launched by
// a dying process races the state-socket handover and dies on its first lock
// call (see runner/drain.go). But refusing meant answering 503, and the
// premise written beside that 503 ("the sender redelivers") is false:
// GitHub does NOT re-send a failed delivery. The hooks repo's own
// delivery-gap replay SDK exists precisely because deliveries are
// "consumed-and-lost during webhook-runner downtime", and it applies no
// status filter because even a 202'd delivery can be lost. So every 503 in a
// deploy window was an errored response AND a dropped webhook.
//
// Spooling keeps the protection and drops the loss: the delivery is written
// to disk and answered 202, and the next process runs it. Deliveries are
// spooled ONLY after they have passed authentication and skip_if, so the
// spool never holds an unauthenticated body.
package spool

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Bounds. A spool is a deploy-window buffer, not a queue: if a delivery
// storm fills it the handler must fall back to the honest 503 rather than
// grow without limit.
const (
	DefaultMaxEntries = 1000
	DefaultMaxBytes   = 64 << 20 // 64 MiB
)

// ErrFull means the spool is at its bound; the caller should answer 503
// (loudly) rather than drop the delivery silently.
var ErrFull = errors.New("spool is full")

// Entry is one parked delivery. Headers carry the original request headers
// (X-GitHub-Event and friends) so the replayed run is indistinguishable from
// the live one.
type Entry struct {
	ID       string              `json:"id"`
	HookID   string              `json:"hook_id"`
	Title    string              `json:"title,omitempty"`
	Headers  map[string][]string `json:"headers"`
	Body     []byte              `json:"body"`
	Received time.Time           `json:"received"`
}

// Store is a directory of parked deliveries, oldest-first by filename.
type Store struct {
	dir        string
	maxEntries int
	maxBytes   int64
	log        *slog.Logger
}

// Open prepares the spool directory. A nil logger is replaced with the
// default.
func Open(dir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("spool dir: %w", err)
	}
	return &Store{dir: dir, maxEntries: DefaultMaxEntries, maxBytes: DefaultMaxBytes, log: log}, nil
}

// SetBounds overrides the entry/byte caps. Values <= 0 keep the current
// bound, so a caller can raise one without knowing the other.
func (s *Store) SetBounds(maxEntries int, maxBytes int64) {
	if maxEntries > 0 {
		s.maxEntries = maxEntries
	}
	if maxBytes > 0 {
		s.maxBytes = maxBytes
	}
}

// entryName sorts lexically by receipt instant, so a directory listing is
// already oldest-first — replay preserves arrival order per hook.
func entryName(received time.Time, id string) string {
	return fmt.Sprintf("%020d-%s.json", received.UTC().UnixNano(), id)
}

// newID is the spool's own identifier space (the run-id generator is
// unexported, and a parked delivery is not a run). Same alphabet so the two
// read alike in logs.
func newID() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 32)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// Put parks a delivery, returning its spool id. Atomic temp+rename, so a
// crash mid-write can never leave a half-entry for the next process to
// replay.
func (s *Store) Put(e Entry) (string, error) {
	n, bytes, err := s.usage()
	if err != nil {
		return "", err
	}
	if n >= s.maxEntries || bytes+int64(len(e.Body)) > s.maxBytes {
		return "", ErrFull
	}
	if e.ID == "" {
		e.ID = newID()
	}
	if e.Received.IsZero() {
		e.Received = time.Now().UTC()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("marshal spool entry: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".spool-*.tmp")
	if err != nil {
		return "", fmt.Errorf("spool temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("write spool entry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("close spool entry: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, entryName(e.Received, e.ID))); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("commit spool entry: %w", err)
	}
	return e.ID, nil
}

func (s *Store) usage() (int, int64, error) {
	names, err := s.names()
	if err != nil {
		return 0, 0, err
	}
	var total int64
	for _, n := range names {
		if fi, err := os.Stat(filepath.Join(s.dir, n)); err == nil {
			total += fi.Size()
		}
	}
	return len(names), total, nil
}

func (s *Store) names() ([]string, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read spool dir: %w", err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		// Skip in-progress temps: only committed .json entries are replayable.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// Len reports how many deliveries are parked.
func (s *Store) Len() int {
	names, err := s.names()
	if err != nil {
		return 0
	}
	return len(names)
}

// Replay hands every parked delivery to fn, oldest first, deleting each one
// only after fn reports success. A failing entry is LEFT for the next boot
// rather than dropped — the whole point is that a delivery is never lost —
// and an unparseable one is quarantined by renaming it aside so a poison
// entry can't block the queue forever. Returns how many replayed.
func (s *Store) Replay(fn func(Entry) error) int {
	names, err := s.names()
	if err != nil {
		s.log.Error("spool replay: cannot read spool dir", "err", err)
		return 0
	}
	replayed := 0
	for _, name := range names {
		path := filepath.Join(s.dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			s.log.Error("spool replay: unreadable entry left in place", "entry", name, "err", err)
			continue
		}
		var e Entry
		if err := json.Unmarshal(data, &e); err != nil {
			// Quarantine, never silently drop: the operator can still see
			// what arrived, and the queue keeps moving.
			bad := path + ".corrupt"
			s.log.Error("spool replay: corrupt entry quarantined", "entry", name, "renamed_to", filepath.Base(bad), "err", err)
			if err := os.Rename(path, bad); err != nil {
				s.log.Error("spool replay: quarantine rename failed", "entry", name, "err", err)
			}
			continue
		}
		if err := fn(e); err != nil {
			s.log.Error("spool replay: dispatch failed, entry kept for the next boot",
				"entry", name, "hook", e.HookID, "err", err)
			continue
		}
		if err := os.Remove(path); err != nil {
			s.log.Error("spool replay: entry dispatched but not removed (it will replay again)",
				"entry", name, "err", err)
		}
		replayed++
	}
	return replayed
}
