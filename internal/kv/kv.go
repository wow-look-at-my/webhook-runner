// Package kv is a small disk-backed key/value store that gives each
// (opted-in) hook a persistent namespace it can read and write over HTTP —
// turning webhook-runner into "almost a simple serverless platform". A hook
// container is disposable and has no memory of its own; the store is where it
// keeps counters, dedupe sets, cached tokens, and anything else that must
// survive across runs (and across server restarts).
//
// The store is namespaced: every hook gets its own namespace (keyed by hook
// ID), persisted to <Dir>/<namespace>.json. It is bounded per item (value
// size, namespace count) and mutex-guarded — the same in-memory
// discipline as internal/runs and internal/events — but, unlike those, it
// writes through to disk so state is durable.
package kv

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config bounds the store. Zero values fall back to the defaults applied in
// New.
type Config struct {
	Dir           string        // directory holding one <namespace>.json per namespace
	MaxValueBytes int           // per-value ceiling (default 64 KiB)
	MaxNamespaces int           // distinct namespaces allowed (default 256)
	SweepInterval time.Duration // how often the TTL sweeper runs (default 1m)
}

// NamespaceStat is a read-only summary of one namespace, surfaced on the
// admin dashboard. It never includes values.
type NamespaceStat struct {
	Namespace string `json:"namespace"`
	Keys      int    `json:"keys"`
	Bytes     int    `json:"bytes"`
}

// KeyInfo is one key's metadata — name, value size, and expiry — for the
// admin inspection endpoints. ExpiresAt/TTLSeconds are nil for keys without
// a TTL; TTLSeconds is the remaining lifetime, computed at read time. The
// entry model tracks nothing else (no created/updated stamps), so nothing
// else is reported.
type KeyInfo struct {
	Key        string     `json:"key"`
	Size       int        `json:"size"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	TTLSeconds *int64     `json:"ttl_seconds,omitempty"`
}

// Entry is KeyInfo plus the stored value — the admin single-key read.
type Entry struct {
	KeyInfo
	Value []byte
}

// Typed errors let the HTTP layer map failures onto status codes.
var (
	ErrValueTooLarge = errors.New("kv: value exceeds max size")
	ErrTooManyNS     = errors.New("kv: namespace limit reached")
	ErrNotInteger    = errors.New("kv: value is not a base-10 int64")
	ErrBadNamespace  = errors.New("kv: invalid namespace")
)

// nsPattern is exactly the hook-ID alphabet (lowercase kebab-case). A
// namespace maps 1:1 to a filename, so we constrain it tightly and reject
// anything that could escape the directory.
var nsPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func validNamespace(ns string) bool {
	return ns != "" && len(ns) <= 128 && nsPattern.MatchString(ns)
}

type entry struct {
	Value   []byte     `json:"v"`           // marshalled as base64 — binary safe
	Expires *time.Time `json:"e,omitempty"` // nil = no TTL
}

func (e entry) expired(now time.Time) bool {
	return e.Expires != nil && !e.Expires.After(now)
}

// info snapshots a (non-expired) entry's metadata. The expiry time is copied
// so the returned struct stays valid outside the store's lock.
func (e entry) info(key string, now time.Time) KeyInfo {
	ki := KeyInfo{Key: key, Size: len(e.Value)}
	if e.Expires != nil {
		exp := *e.Expires
		remaining := int64(exp.Sub(now) / time.Second)
		ki.ExpiresAt = &exp
		ki.TTLSeconds = &remaining
	}
	return ki
}

// Store is a disk-backed, bounded, concurrency-safe namespaced KV store.
type Store struct {
	mu  sync.RWMutex
	ns  map[string]map[string]entry
	cfg Config
	log *slog.Logger

	secret []byte // HMAC key for namespace tokens (see token.go)

	// Cooperative run-owned locks (see lock.go). Deliberately in-memory only
	// — a lock's lifecycle is bounded by its holding run, and no run survives
	// a restart — and under its own mutex, so lock verbs never contend with
	// entry persistence.
	lockMu sync.Mutex
	locks  map[string]map[string]lockEntry

	// onMutate, when set, is invoked after every successful ENTRY mutation
	// (Set, a Delete that deleted, Incr, a sweep that reclaimed something)
	// — synchronously on the mutating goroutine, under the store mutex, so
	// it must be fast, never block, and never call back into the Store.
	// It is the dashboard's "kv changed" push seam (locks are not entries
	// and never fire it). Set once at wiring time, before traffic.
	onMutate func()

	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// SetOnMutate registers fn to run after every successful entry mutation.
// A nil fn disables the callback. See the field comment for the contract.
func (s *Store) SetOnMutate(fn func()) {
	s.mu.Lock()
	s.onMutate = fn
	s.mu.Unlock()
}

// notifyMutate fires the onMutate seam. Callers hold s.mu (read the field
// under the same lock that guards it).
func (s *Store) notifyMutate() {
	if s.onMutate != nil {
		s.onMutate()
	}
}

// New constructs a Store, creating Dir if needed and loading every namespace
// file already present. A corrupt namespace file is logged and skipped rather
// than aborting startup (mirroring hooks.LoadDir's per-hook error handling).
func New(cfg Config, secret []byte, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Dir == "" {
		return nil, errors.New("kv: Config.Dir is required")
	}
	if cfg.MaxValueBytes <= 0 {
		cfg.MaxValueBytes = 64 * 1024
	}
	if cfg.MaxNamespaces <= 0 {
		cfg.MaxNamespaces = 256
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = time.Minute
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("kv: create dir: %w", err)
	}
	s := &Store{
		ns:     make(map[string]map[string]entry),
		locks:  make(map[string]map[string]lockEntry),
		cfg:    cfg,
		log:    log,
		secret: secret,
		stop:   make(chan struct{}),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	des, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		return fmt.Errorf("kv: read dir: %w", err)
	}
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		ns := strings.TrimSuffix(de.Name(), ".json")
		if !validNamespace(ns) {
			s.log.Warn("kv: skipping file with invalid namespace name", "file", de.Name())
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.cfg.Dir, de.Name()))
		if err != nil {
			s.log.Error("kv: read namespace file", "file", de.Name(), "err", err)
			continue
		}
		var m map[string]entry
		if err := json.Unmarshal(data, &m); err != nil {
			s.log.Error("kv: parse namespace file (skipping)", "file", de.Name(), "err", err)
			continue
		}
		s.ns[ns] = m
	}
	return nil
}

// MaxValueBytes is the configured per-value ceiling, exposed so the HTTP
// layer can cap an incoming request body before buffering it.
func (s *Store) MaxValueBytes() int { return s.cfg.MaxValueBytes }

// Get returns a copy of the value for key in ns, or ok=false if it is absent
// or expired. Expired entries are reclaimed by the sweeper, not here, so Get
// stays read-locked.
func (s *Store) Get(ns, key string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.ns[ns]
	if !ok {
		return nil, false
	}
	e, ok := m[key]
	if !ok || e.expired(time.Now()) {
		return nil, false
	}
	return append([]byte(nil), e.Value...), true
}

// Set stores value under key in ns. ttl<=0 means no expiry. The namespace
// file is rewritten before Set returns; a persist failure is reported and the
// in-memory mutation is rolled back so memory never diverges from disk.
func (s *Store) Set(ns, key string, value []byte, ttl time.Duration) error {
	if !validNamespace(ns) {
		return ErrBadNamespace
	}
	if len(value) > s.cfg.MaxValueBytes {
		return ErrValueTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	m, nsExisted := s.ns[ns]
	if !nsExisted {
		if len(s.ns) >= s.cfg.MaxNamespaces {
			return ErrTooManyNS
		}
		m = make(map[string]entry)
		s.ns[ns] = m
	}
	prev, keyExisted := m[key]

	e := entry{Value: append([]byte(nil), value...)}
	if ttl > 0 {
		exp := time.Now().Add(ttl)
		e.Expires = &exp
	}
	m[key] = e
	if err := s.persist(ns); err != nil {
		s.rollback(ns, key, prev, keyExisted, nsExisted)
		return err
	}
	s.notifyMutate()
	return nil
}

// Delete removes key from ns. It is idempotent: deleting an absent key is a
// no-op that returns nil. An error is only returned for a bad namespace or a
// persist failure (in which case the deletion is rolled back).
func (s *Store) Delete(ns, key string) error {
	if !validNamespace(ns) {
		return ErrBadNamespace
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.ns[ns]
	if !ok {
		return nil
	}
	prev, existed := m[key]
	if !existed {
		return nil
	}
	delete(m, key)
	if err := s.persist(ns); err != nil {
		m[key] = prev
		return err
	}
	s.notifyMutate()
	return nil
}

// List returns the sorted, non-expired keys in ns.
func (s *Store) List(ns string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.ns[ns]
	if !ok {
		return []string{}
	}
	now := time.Now()
	keys := make([]string, 0, len(m))
	for k, e := range m {
		if e.expired(now) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Keys returns metadata (never values) for the non-expired keys in ns whose
// names start with prefix ("" matches every key), sorted by key — the same
// lazy-expiry and ordering rules as List, plus per-key size and expiry for
// the admin inspection view.
func (s *Store) Keys(ns, prefix string) []KeyInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.ns[ns]
	if !ok {
		return []KeyInfo{}
	}
	now := time.Now()
	infos := make([]KeyInfo, 0, len(m))
	for k, e := range m {
		if e.expired(now) || !strings.HasPrefix(k, prefix) {
			continue
		}
		infos = append(infos, e.info(k, now))
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
	return infos
}

// GetEntry returns one key's metadata plus a copy of its value, or ok=false
// when it is absent or expired — the exact lazy-expiry rule Get uses, so the
// admin inspection endpoint can never serve a ghost the state API would 404.
func (s *Store) GetEntry(ns, key string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.ns[ns]
	if !ok {
		return Entry{}, false
	}
	e, ok := m[key]
	now := time.Now()
	if !ok || e.expired(now) {
		return Entry{}, false
	}
	return Entry{KeyInfo: e.info(key, now), Value: append([]byte(nil), e.Value...)}, true
}

// Incr atomically adds delta to the integer stored at key in ns and returns
// the new value. A missing or expired key starts from 0. An existing value
// that is not a base-10 int64 returns ErrNotInteger (it is never silently
// reset). ttl>0 sets a fresh expiry; ttl<=0 preserves any existing (non-
// expired) expiry, so a counter can be incremented repeatedly without its TTL
// being reset. The whole read-modify-write happens under one lock, which is
// the entire reason for a server-side increment over a racy client GET+PUT.
func (s *Store) Incr(ns, key string, delta int64, ttl time.Duration) (int64, error) {
	if !validNamespace(ns) {
		return 0, ErrBadNamespace
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	m, nsExisted := s.ns[ns]
	if !nsExisted {
		if len(s.ns) >= s.cfg.MaxNamespaces {
			return 0, ErrTooManyNS
		}
		m = make(map[string]entry)
		s.ns[ns] = m
	}

	now := time.Now()
	prev, keyExisted := m[key]
	var base int64
	keepExpiry := prev.Expires
	if keyExisted && !prev.expired(now) {
		n, err := strconv.ParseInt(string(prev.Value), 10, 64)
		if err != nil {
			if !nsExisted {
				delete(s.ns, ns)
			}
			return 0, ErrNotInteger
		}
		base = n
	} else {
		// Missing or expired: start from zero, and any stale expiry is gone.
		keepExpiry = nil
	}

	newVal := base + delta
	e := entry{Value: strconv.AppendInt(nil, newVal, 10)}
	if ttl > 0 {
		exp := now.Add(ttl)
		e.Expires = &exp
	} else {
		e.Expires = keepExpiry
	}
	m[key] = e
	if err := s.persist(ns); err != nil {
		s.rollback(ns, key, prev, keyExisted, nsExisted)
		return 0, err
	}
	s.notifyMutate()
	return newVal, nil
}

// Stats returns a read-only, value-free summary of every namespace, sorted by
// name, for the admin dashboard.
func (s *Store) Stats() []NamespaceStat {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	stats := make([]NamespaceStat, 0, len(s.ns))
	for ns, m := range s.ns {
		var keys, bytes int
		for _, e := range m {
			if e.expired(now) {
				continue
			}
			keys++
			bytes += len(e.Value)
		}
		stats = append(stats, NamespaceStat{Namespace: ns, Keys: keys, Bytes: bytes})
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Namespace < stats[j].Namespace })
	return stats
}

// StartSweeper launches the background TTL reaper. Lazy expiry already hides
// expired entries from reads; the sweeper is the backstop that reclaims their
// memory and disk.
func (s *Store) StartSweeper() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(s.cfg.SweepInterval)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				s.sweep()
			}
		}
	}()
}

func (s *Store) sweep() {
	s.reapExpiredLocks()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	reclaimed := false
	for ns, m := range s.ns {
		changed := false
		for k, e := range m {
			if e.expired(now) {
				delete(m, k)
				changed = true
			}
		}
		if changed {
			reclaimed = true
			if err := s.persist(ns); err != nil {
				s.log.Error("kv: persist during sweep failed", "ns", ns, "err", err)
			}
		}
	}
	// One signal per sweep that reclaimed anything: expired entries change
	// the admin /kv views (lazy expiry hides them from reads earlier, but
	// the sweep is when counts/bytes actually move).
	if reclaimed {
		s.notifyMutate()
	}
}

// Close stops the sweeper and waits for it to exit. Safe to call once.
func (s *Store) Close() {
	s.closeOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// rollback restores a key (and a freshly-created namespace) after a failed
// persist, keeping the in-memory state identical to what is on disk.
func (s *Store) rollback(ns, key string, prev entry, keyExisted, nsExisted bool) {
	m := s.ns[ns]
	if keyExisted {
		m[key] = prev
	} else if m != nil {
		delete(m, key)
	}
	if !nsExisted {
		delete(s.ns, ns)
	}
}

// persist atomically rewrites one namespace's file via temp+rename. Callers
// hold the write lock. A rename is atomic on the same filesystem, so a crash
// mid-write never leaves a torn file.
func (s *Store) persist(ns string) error {
	data, err := json.MarshalIndent(s.ns[ns], "", "  ")
	if err != nil {
		return fmt.Errorf("kv: marshal namespace %q: %w", ns, err)
	}
	tmp, err := os.CreateTemp(s.cfg.Dir, "."+ns+".json.tmp-*")
	if err != nil {
		return fmt.Errorf("kv: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("kv: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("kv: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("kv: close temp: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.cfg.Dir, ns+".json")); err != nil {
		return fmt.Errorf("kv: rename: %w", err)
	}
	return nil
}
