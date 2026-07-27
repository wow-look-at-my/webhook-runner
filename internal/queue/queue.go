// Package queue is the runner's durable WORK QUEUE primitive: a named,
// per-hook backlog of opaque item ids that survives the run that filled it.
//
// WHY IT IS HERE AND NOT IN A HOOK. A hook run is a container that lives for
// one delivery, so "work I did not get to" has nowhere to live inside it. Every
// hook that walks a large fleet therefore reinvents the same thing on top of
// the KV store — a cursor string, a rotation, a resume rule, and a set of
// off-by-one bugs — and reinvents it badly: a cursor is a position in a list
// the next run re-derives, so it silently means something different whenever
// that list changes. pr-minder shipped exactly that and stranded 119 of 169
// PRs behind a cap, hourly, forever. The backlog is the RUNNER's concern
// (operator ruling: "queue should exist in webhook-runner. This is completely
// out of scope for a webhook impl"), so it is a first-class primitive beside
// the KV store, the locks and the waits.
//
// THE CONTRACT, and why each half is shaped this way:
//
//   - PUSH IS A SET UNION, ORDER PRESERVED. Pushing an item already queued is
//     a no-op that keeps its ORIGINAL position. That is what lets a caller
//     re-push its whole candidate set on every tick — the natural, stateless
//     way to describe "this is the work that exists" — without the queue
//     growing without bound or an item at the back starving because a
//     re-push kept moving it.
//   - TAKE REMOVES, at-most-once, no leases or acks. Consumers of a backlog
//     like this are idempotent reconcilers whose next tick re-derives the same
//     work, so a run that dies mid-item loses nothing that the next push does
//     not restore — and in exchange there is no lease to expire, no visibility
//     timeout to tune, and no invisible in-flight state to leak.
//   - Depth is observable, so "the backlog is not draining" is a number a
//     hook can log and an operator can see, rather than an inference.
//
// The store is DISK-BACKED (unlike the lock table, which is deliberately
// memory-only): a queue outliving the process is the entire point — a runner
// restart in the middle of a fleet walk must not restart the walk.
package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Config bounds the store. Zero values fall back to the defaults applied in
// New.
type Config struct {
	Dir           string // directory holding one <namespace>.json per namespace
	MaxDepth      int    // items per queue (default 10000)
	MaxItemBytes  int    // per item (default 512)
	MaxQueues     int    // distinct queues per namespace (default 64)
	MaxNamespaces int    // distinct namespaces (default 256)
}

// Typed errors the HTTP layer maps onto status codes.
var (
	ErrBadNamespace  = errors.New("queue: invalid namespace")
	ErrBadName       = errors.New("queue: invalid queue name")
	ErrItemTooLarge  = errors.New("queue: item exceeds max size")
	ErrEmptyItem     = errors.New("queue: empty item")
	ErrTooManyQueues = errors.New("queue: queue limit reached for this namespace")
	ErrTooManyNS     = errors.New("queue: namespace limit reached")
)

// namePattern is the queue-name alphabet: lowercase kebab-case, like hook ids
// and KV namespaces. Names appear in URLs and in one JSON file per namespace,
// so they stay boring on purpose.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func validNamespace(ns string) bool {
	return ns != "" && len(ns) <= 128 && namePattern.MatchString(ns)
}

func validName(name string) bool {
	return name != "" && len(name) <= 128 && namePattern.MatchString(name)
}

// ValidName exposes the name rule to the HTTP layer, so a bad name is a 400
// from the router rather than an error only the mutating verbs can produce.
func ValidName(name string) bool { return validName(name) }

// Stat is one queue's observable state: what it is called and how much work
// is waiting. Never the items — a depth is an operational fact, the contents
// are the owner's business.
type Stat struct {
	Name  string `json:"name"`
	Depth int    `json:"depth"`
}

// PushResult reports what a push actually did. `Duplicates` are items already
// queued (kept at their original position, not re-queued); `Dropped` are items
// the depth cap refused — a full backlog means the consumer is not keeping up,
// which the caller must be able to SEE rather than infer from a silent gap.
type PushResult struct {
	Queued     int `json:"queued"`
	Duplicates int `json:"duplicates"`
	Dropped    int `json:"dropped"`
	Depth      int `json:"depth"`
}

type Store struct {
	mu  sync.Mutex
	ns  map[string]map[string][]string // namespace -> queue -> FIFO items
	cfg Config
	log *slog.Logger
}

// New constructs a Store, creating Dir if needed and loading every namespace
// file already present. A corrupt namespace file is logged and skipped rather
// than aborting startup (the same per-file tolerance kv.New and hooks.LoadDir
// apply).
func New(cfg Config, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Dir == "" {
		return nil, errors.New("queue: Config.Dir is required")
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = 10000
	}
	if cfg.MaxItemBytes <= 0 {
		cfg.MaxItemBytes = 512
	}
	if cfg.MaxQueues <= 0 {
		cfg.MaxQueues = 64
	}
	if cfg.MaxNamespaces <= 0 {
		cfg.MaxNamespaces = 256
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("queue: create dir: %w", err)
	}
	s := &Store{ns: make(map[string]map[string][]string), cfg: cfg, log: log}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	des, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		return fmt.Errorf("queue: read dir: %w", err)
	}
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		ns := strings.TrimSuffix(de.Name(), ".json")
		if !validNamespace(ns) {
			s.log.Warn("queue: skipping file with invalid namespace name", "file", de.Name())
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.cfg.Dir, de.Name()))
		if err != nil {
			s.log.Error("queue: read namespace file", "file", de.Name(), "err", err)
			continue
		}
		var m map[string][]string
		if err := json.Unmarshal(data, &m); err != nil {
			s.log.Error("queue: parse namespace file (skipping)", "file", de.Name(), "err", err)
			continue
		}
		s.ns[ns] = m
	}
	return nil
}

// Push appends items that are not already queued, in the given order, and
// returns what it did. Duplicates keep their original position (see the
// package comment): re-pushing the same backlog every tick is the intended
// usage, not an accident to defend against.
func (s *Store) Push(ns, name string, items []string) (PushResult, error) {
	if !validNamespace(ns) {
		return PushResult{}, ErrBadNamespace
	}
	if !validName(name) {
		return PushResult{}, ErrBadName
	}
	for _, it := range items {
		if it == "" {
			return PushResult{}, ErrEmptyItem
		}
		if len(it) > s.cfg.MaxItemBytes {
			return PushResult{}, ErrItemTooLarge
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	qs, nsExisted := s.ns[ns]
	if !nsExisted {
		if len(s.ns) >= s.cfg.MaxNamespaces {
			return PushResult{}, ErrTooManyNS
		}
		qs = make(map[string][]string)
		s.ns[ns] = qs
	}
	q, qExisted := qs[name]
	if !qExisted && len(qs) >= s.cfg.MaxQueues {
		if !nsExisted {
			delete(s.ns, ns)
		}
		return PushResult{}, ErrTooManyQueues
	}

	present := make(map[string]struct{}, len(q))
	for _, it := range q {
		present[it] = struct{}{}
	}
	res := PushResult{}
	for _, it := range items {
		if _, dup := present[it]; dup {
			res.Duplicates++
			continue
		}
		if len(q) >= s.cfg.MaxDepth {
			res.Dropped++
			continue
		}
		q = append(q, it)
		present[it] = struct{}{}
		res.Queued++
	}
	qs[name] = q
	res.Depth = len(q)

	if err := s.persist(ns); err != nil {
		// Roll back to what is on disk: an in-memory queue the file does not
		// know about would silently un-queue itself on the next restart.
		s.rollback(ns, name, q[:len(q)-res.Queued], qExisted, nsExisted)
		return PushResult{}, err
	}
	if res.Dropped > 0 {
		s.log.Warn("queue: depth cap reached, items dropped", "ns", ns, "queue", name, "dropped", res.Dropped, "depth", res.Depth)
	}
	return res, nil
}

// Take removes and returns up to count items from the head. A missing queue is
// not an error — it is an empty one, which is what a caller draining a backlog
// means by "nothing to do".
func (s *Store) Take(ns, name string, count int) ([]string, int, error) {
	if !validNamespace(ns) {
		return nil, 0, ErrBadNamespace
	}
	if !validName(name) {
		return nil, 0, ErrBadName
	}
	if count <= 0 {
		return []string{}, s.Depth(ns, name), nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	qs := s.ns[ns]
	if qs == nil {
		return []string{}, 0, nil
	}
	q := qs[name]
	if len(q) == 0 {
		return []string{}, 0, nil
	}
	if count > len(q) {
		count = len(q)
	}
	taken := append([]string(nil), q[:count]...)
	rest := append([]string(nil), q[count:]...)
	qs[name] = rest

	if err := s.persist(ns); err != nil {
		qs[name] = q // the take never happened
		return nil, 0, err
	}
	if len(rest) == 0 {
		delete(qs, name)
		if len(qs) == 0 {
			delete(s.ns, ns)
		}
		// Best-effort tidy: the file now describes an empty namespace. A
		// failure here costs nothing (the next push rewrites it), so it is
		// logged rather than surfaced — the take itself already succeeded.
		if err := s.persist(ns); err != nil {
			s.log.Error("queue: persist after drain", "ns", ns, "err", err)
		}
	}
	return taken, len(rest), nil
}

// Depth reports how much work is waiting on one queue.
func (s *Store) Depth(ns, name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ns[ns][name])
}

// List reports every non-empty queue in a namespace, name-sorted. Depths only:
// the items belong to the hook, and the listing is for operators.
func (s *Store) List(ns string) []Stat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Stat, 0, len(s.ns[ns]))
	for name, q := range s.ns[ns] {
		out = append(out, Stat{Name: name, Depth: len(q)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// rollback restores a queue (and a freshly created namespace) after a failed
// persist, keeping memory identical to disk.
func (s *Store) rollback(ns, name string, prev []string, qExisted, nsExisted bool) {
	qs := s.ns[ns]
	if qs == nil {
		return
	}
	if qExisted {
		qs[name] = prev
	} else {
		delete(qs, name)
	}
	if !nsExisted {
		delete(s.ns, ns)
	}
}

// persist atomically rewrites one namespace's file via temp+rename. Callers
// hold the mutex. A rename is atomic on the same filesystem, so a crash
// mid-write never leaves a torn file.
func (s *Store) persist(ns string) error {
	data, err := json.MarshalIndent(s.ns[ns], "", "  ")
	if err != nil {
		return fmt.Errorf("queue: marshal namespace %q: %w", ns, err)
	}
	tmp, err := os.CreateTemp(s.cfg.Dir, "."+ns+".json.tmp-*")
	if err != nil {
		return fmt.Errorf("queue: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("queue: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("queue: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("queue: close temp: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.cfg.Dir, ns+".json")); err != nil {
		return fmt.Errorf("queue: rename: %w", err)
	}
	return nil
}
