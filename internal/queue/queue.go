// Package queue is the runner's durable per-hook WORK QUEUE: a hook records
// that it has work outstanding, and the runner starts a run to do it — now, or
// at a time the hook names.
//
// Why this is a runner primitive and not a hook's business. A stateful hook
// keeps records meaning "this subject needs another look" (required-builds'
// reconcile/settle/deferral records, pr-minder's owed re-checks). Something has
// to notice them. Every option available without this package is a bad one:
//
//   - Do it at the tail of every delivery. Then every unrelated event pays a
//     fleet scan — measured on required-builds: 78 pending commits turned a
//     3-second evaluation into a 90-second run, with the org's event volume
//     setting the rate.
//   - Do it on a fixed schedule. Cheap, but work that is ALREADY KNOWN sits
//     waiting out the interval — a failed publish blocking a merge gate for
//     minutes with nothing to show for the wait.
//   - Have the hook POST itself a trigger. Works, and required-builds shipped
//     exactly that: an HMAC self-call, a dedup marker in its own KV, a
//     suppression flag so a failing pass could not re-trigger itself in a loop,
//     and a floor tick for deadlines it could not express. That is a queue,
//     hand-rolled, per hook, non-durable — the marker and the pending work
//     disappear on restart precisely when a deploy dropped the deliveries.
//
// So the queue lives here. `POST /queue` on the state API takes {key, payload,
// delay_seconds}; the runner stores the entry, and a dispatcher starts a run of
// that hook when it comes due. The properties a hook would otherwise have to
// build itself:
//
//	DEDUP        — an entry is identified by (namespace, key). Enqueueing an
//	               existing key UPSERTS: the earliest due time wins (work that
//	               needs attention sooner is never pushed later) and the newest
//	               payload wins. A thousand enqueues are one run.
//	SCHEDULING   — delay_seconds > 0 is a wake-at-T, the thing no primitive here
//	               offered. A settle window or a grace period becomes an entry
//	               due at its deadline instead of a tick that polls for it.
//	NO SWALLOWING— the entry is deleted as its run STARTS. Work discovered while
//	               that run is in flight re-enqueues and gets exactly one
//	               follow-up run.
//	NO SPAMMING  — MinInterval bounds how often one key may fire. A hook that
//	               re-enqueues from inside its own queue run cannot hot-loop;
//	               the re-arm is simply scheduled at last-fire + MinInterval.
//	DURABILITY   — bbolt, like the run store. A queued wake survives the restart
//	               that a hook-side marker would not.
//
// The dispatcher is a PURE timing component in the internal/scheduler mould: it
// owns "what is due and when", and the actual run dispatch is a caller-supplied
// Fire callback, so this package needs no dependency on the runner, registry,
// or tracker and is testable with an injected clock.
package queue

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	// MaxKeyLen bounds a caller-supplied key. Keys are opaque to the runner
	// but they name entries on the dashboard and in logs.
	MaxKeyLen = 256
	// MaxPayloadBytes bounds one entry's payload. A queue entry says WHAT
	// needs doing, not the data to do it with — the hook's own state holds
	// that — so this is deliberately small.
	MaxPayloadBytes = 16 * 1024
	// MaxPerNamespace bounds how many distinct keys one hook may have
	// outstanding. Past it, enqueueing a NEW key is refused (existing keys
	// still upsert, so a hook can never be locked out of updating work it
	// already queued). A hook that needs thousands of distinct keys is
	// enqueueing subjects, not work — that is what its own KV is for.
	MaxPerNamespace = 1000
	// DefaultMinInterval is the floor between two fires of the SAME key.
	DefaultMinInterval = 5 * time.Second
	// DefaultMaxDelay caps delay_seconds. A queue entry is pending work, not
	// a calendar; anything wanting a longer horizon wants a schedule.
	DefaultMaxDelay = 24 * time.Hour
)

var (
	bucketEntries = []byte("entries") // key: namespace \x00 key -> Entry JSON
	bucketFired   = []byte("fired")   // key: namespace \x00 key -> last fire unix nanos
)

// Entry is one unit of outstanding work: a hook has said "run me for this key",
// optionally not before RunAt.
type Entry struct {
	Namespace  string    `json:"namespace"`
	Key        string    `json:"key"`
	RunAt      time.Time `json:"run_at"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	// Enqueues counts how many times this entry was (re-)enqueued before it
	// fired — the dedup made visible. 1 means one enqueue; 40 means the same
	// work was announced 40 times and still costs one run.
	Enqueues int             `json:"enqueues"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// Due reports whether the entry may fire at now.
func (e Entry) Due(now time.Time) bool { return !e.RunAt.After(now) }

// Config configures a Store.
type Config struct {
	// Path is the bbolt file. Required.
	Path string
	// MinInterval is the per-key fire floor (see DefaultMinInterval).
	MinInterval time.Duration
	// MaxDelay caps a caller's requested delay (see DefaultMaxDelay).
	MaxDelay time.Duration
}

// Store is the durable queue.
type Store struct {
	db  *bolt.DB
	cfg Config
	log *slog.Logger

	// Notifies the dispatcher that the earliest due time may have moved
	// closer. Buffered depth 1: a pending signal already means "re-read".
	wake chan struct{}

	mu     sync.Mutex
	closed bool
}

// Open creates or opens the queue store.
func Open(cfg Config, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Path == "" {
		return nil, errors.New("queue: Config.Path is required")
	}
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = DefaultMinInterval
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = DefaultMaxDelay
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, fmt.Errorf("queue: create dir: %w", err)
	}
	db, err := bolt.Open(cfg.Path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("queue: open %s: %w", cfg.Path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketEntries, bucketFired} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("queue: init buckets: %w", err)
	}
	return &Store{db: db, cfg: cfg, log: log, wake: make(chan struct{}, 1)}, nil
}

// Close releases the bbolt file.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.db.Close()
}

// MinInterval reports the configured per-key fire floor.
func (s *Store) MinInterval() time.Duration { return s.cfg.MinInterval }

// MaxDelay reports the configured delay ceiling.
func (s *Store) MaxDelay() time.Duration { return s.cfg.MaxDelay }

func entryKey(namespace, key string) []byte {
	return []byte(namespace + "\x00" + key)
}

func splitEntryKey(b []byte) (namespace, key string, ok bool) {
	ns, k, found := strings.Cut(string(b), "\x00")
	return ns, k, found
}

// ErrTooManyKeys is returned when a namespace is at MaxPerNamespace and the
// enqueue would add a NEW key.
var ErrTooManyKeys = errors.New("queue: too many outstanding keys for this namespace")

// Enqueue records outstanding work, returning the stored entry.
//
// UPSERT semantics, which are the whole point: re-enqueueing a key that is
// already outstanding does not add a second entry. The stored RunAt becomes the
// EARLIER of the two (work does not get postponed by a later announcement), the
// payload is replaced by the newest one, and the enqueue counter increments so
// the dashboard can show how much announcing one run absorbed.
//
// The MinInterval floor applies here rather than at fire time, so what is
// stored is the truth: an entry whose key fired moments ago is stored at
// lastFire+MinInterval, and the dashboard shows exactly when it will run.
func (s *Store) Enqueue(namespace, key string, delay time.Duration, payload json.RawMessage, now time.Time) (Entry, error) {
	if namespace == "" {
		return Entry{}, errors.New("queue: namespace is required")
	}
	if key == "" || len(key) > MaxKeyLen {
		return Entry{}, fmt.Errorf("queue: key must be 1..%d bytes", MaxKeyLen)
	}
	if strings.Contains(key, "\x00") {
		return Entry{}, errors.New("queue: key must not contain NUL")
	}
	if len(payload) > MaxPayloadBytes {
		return Entry{}, fmt.Errorf("queue: payload exceeds %d bytes", MaxPayloadBytes)
	}
	if delay < 0 {
		delay = 0
	}
	if delay > s.cfg.MaxDelay {
		return Entry{}, fmt.Errorf("queue: delay exceeds the %s maximum", s.cfg.MaxDelay)
	}

	want := now.Add(delay)
	var out Entry
	err := s.db.Update(func(tx *bolt.Tx) error {
		entries := tx.Bucket(bucketEntries)
		ek := entryKey(namespace, key)
		existing := entries.Get(ek)

		// The anti-hot-loop floor: never schedule a key sooner than
		// MinInterval after its own last fire. A hook that re-enqueues from
		// inside the run its enqueue caused gets a run — just not instantly,
		// forever.
		if last, ok := lastFire(tx, ek); ok {
			if earliest := last.Add(s.cfg.MinInterval); want.Before(earliest) {
				want = earliest
			}
		}

		var e Entry
		if existing != nil {
			if err := json.Unmarshal(existing, &e); err != nil {
				// A corrupt record must not wedge the queue: replace it.
				e = Entry{}
			}
		} else if entries.Stats().KeyN >= MaxPerNamespace && namespaceCount(tx, namespace) >= MaxPerNamespace {
			return ErrTooManyKeys
		}

		e.Namespace, e.Key = namespace, key
		if e.EnqueuedAt.IsZero() {
			e.EnqueuedAt = now
		}
		e.Enqueues++
		if payload != nil {
			e.Payload = payload
		}
		// Earliest-wins: an outstanding entry due sooner keeps its time.
		if existing == nil || e.RunAt.IsZero() || want.Before(e.RunAt) {
			e.RunAt = want
		}
		out = e
		blob, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return entries.Put(ek, blob)
	})
	if err != nil {
		return Entry{}, err
	}
	s.signal()
	return out, nil
}

func namespaceCount(tx *bolt.Tx, namespace string) int {
	n := 0
	prefix := []byte(namespace + "\x00")
	c := tx.Bucket(bucketEntries).Cursor()
	for k, _ := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, _ = c.Next() {
		n++
	}
	return n
}

func lastFire(tx *bolt.Tx, ek []byte) (time.Time, bool) {
	v := tx.Bucket(bucketFired).Get(ek)
	if len(v) != 8 {
		return time.Time{}, false
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(v))), true
}

// Delete drops an entry (a hook cancelling work it queued). Missing is fine.
func (s *Store) Delete(namespace, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketEntries).Delete(entryKey(namespace, key))
	})
}

// List returns a namespace's outstanding entries, soonest first. An empty
// namespace lists everything (the admin view).
func (s *Store) List(namespace string) []Entry {
	var out []Entry
	_ = s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketEntries).ForEach(func(k, v []byte) error {
			ns, _, ok := splitEntryKey(k)
			if !ok || (namespace != "" && ns != namespace) {
				return nil
			}
			var e Entry
			if err := json.Unmarshal(v, &e); err == nil {
				out = append(out, e)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].RunAt.Equal(out[j].RunAt) {
			return out[i].Namespace+out[i].Key < out[j].Namespace+out[j].Key
		}
		return out[i].RunAt.Before(out[j].RunAt)
	})
	return out
}

// Get returns one entry.
func (s *Store) Get(namespace, key string) (Entry, bool) {
	var e Entry
	found := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketEntries).Get(entryKey(namespace, key))
		if v == nil {
			return nil
		}
		found = json.Unmarshal(v, &e) == nil
		return nil
	})
	return e, found
}

// Claim removes and returns every entry due at now, stamping each key's fire
// time. Removal happens as the run STARTS (this call is what precedes the
// dispatch), so work discovered while that run is in flight re-enqueues cleanly
// and earns exactly one follow-up run instead of being swallowed.
func (s *Store) Claim(now time.Time) []Entry {
	var claimed []Entry
	err := s.db.Update(func(tx *bolt.Tx) error {
		entries := tx.Bucket(bucketEntries)
		fired := tx.Bucket(bucketFired)
		var keys [][]byte
		if err := entries.ForEach(func(k, v []byte) error {
			var e Entry
			if err := json.Unmarshal(v, &e); err != nil {
				keys = append(keys, append([]byte(nil), k...)) // drop the corrupt record
				return nil
			}
			if e.Due(now) {
				claimed = append(claimed, e)
				keys = append(keys, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		var stamp [8]byte
		binary.BigEndian.PutUint64(stamp[:], uint64(now.UnixNano()))
		for _, k := range keys {
			if err := entries.Delete(k); err != nil {
				return err
			}
			if err := fired.Put(k, stamp[:]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.log.Error("queue: claiming due entries failed", "err", err)
		return nil
	}
	sort.Slice(claimed, func(i, j int) bool { return claimed[i].RunAt.Before(claimed[j].RunAt) })
	return claimed
}

// NextDue reports when the soonest entry becomes due.
func (s *Store) NextDue() (time.Time, bool) {
	var next time.Time
	_ = s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketEntries).ForEach(func(_, v []byte) error {
			var e Entry
			if err := json.Unmarshal(v, &e); err != nil {
				return nil
			}
			if next.IsZero() || e.RunAt.Before(next) {
				next = e.RunAt
			}
			return nil
		})
	})
	return next, !next.IsZero()
}

// PruneNamespaces drops entries (and fire stamps) for namespaces that no longer
// exist — a hook removed or renamed. Called on reload with the live hook ids;
// a nil/empty set is ignored rather than treated as "delete everything", so a
// failed load can never wipe the queue.
func (s *Store) PruneNamespaces(live map[string]bool) int {
	if len(live) == 0 {
		return 0
	}
	dropped := 0
	_ = s.db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{bucketEntries, bucketFired} {
			b := tx.Bucket(bucket)
			var keys [][]byte
			_ = b.ForEach(func(k, _ []byte) error {
				if ns, _, ok := splitEntryKey(k); ok && !live[ns] {
					keys = append(keys, append([]byte(nil), k...))
				}
				return nil
			})
			for _, k := range keys {
				if err := b.Delete(k); err == nil && bucket[0] == 'e' {
					dropped++
				}
			}
		}
		return nil
	})
	return dropped
}

// signal nudges the dispatcher without blocking.
func (s *Store) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
