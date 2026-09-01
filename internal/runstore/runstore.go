// Package runstore persists completed runs to a single bbolt file so run
// history survives server restarts. The in-memory tracker (internal/runs)
// remains the source of truth for active runs and the freshest window; this
// store is its durable, read-side complement: a run is written exactly ,
// when it reaches a terminal status, and the admin endpoints read it back
// merged behind the live tracker. Nothing is ever rehydrated into the
// tracker. A run still in flight when the server stops never completed, so
// it is never persisted — after a restart it exists nowhere.
//
// Retention is time-based (Config.Retention, the primary knob): expired runs
// are hidden from reads lazily and reclaimed by the background sweeper. The
// per-hook count cap (Config.MaxPerHook) is a coarse disk safety net behind
// it, enforced oldest- during the sweep.
package runstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// Defaults applied by Open for -valued Config fields.
const (
	DefaultRetention     = 48 * time.Hour
	DefaultMaxPerHook    = 200_000
	DefaultSweepInterval = 5 * time.Minute
)

// Config bounds the store. values fall back to the defaults above.
type Config struct {
	Path          string        // bbolt database file (required)
	Retention     time.Duration // completed runs older than this are dropped
	MaxPerHook    int           // persisted-run cap per hook (disk safety net; Retention is the primary knob)
	SweepInterval time.Duration // how often the GC sweeper runs
}

// Bucket layout. bytime and each per-hook bucket share the same "<start-unix-nanos>-<run-id>" key (-padded, so lexicographic order is chronological; the nanos are the run's QUEUED/accepted time, RunState.Started) — that shared time ordering is what makes range GC and.
var (
	bucketMeta   = []byte("meta")   // run ID -> RunState JSON (output stripped)
	bucketOutput = []byte("output") // run ID -> outputRecord JSON
	bucketByTime = []byte("bytime") // time key -> hook ID
	bucketByHook = []byte("byhook") // nested: hook ID -> bucket of time key -> summary value
)

// outputRecord keeps a run's captured output out of its metadata record so
// list and stats reads never touch output blobs.
type outputRecord struct {
	Output      []string    `json:"output,omitempty"`
	OutputTimes []time.Time `json:"output_times,omitempty"`
}

// Store is a bbolt-backed archive of terminal runs.
type Store struct {
	db  *bolt.DB
	cfg Config
	log *slog.Logger

	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// Open opens (creating as needed) the database file and its buckets.
func Open(cfg Config, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Path == "" {
		return nil, errors.New("runstore: Config.Path is required")
	}
	if cfg.Retention <= 0 {
		cfg.Retention = DefaultRetention
	}
	if cfg.MaxPerHook <= 0 {
		cfg.MaxPerHook = DefaultMaxPerHook
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = DefaultSweepInterval
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, fmt.Errorf("runstore: create dir: %w", err)
	}
	// The flock timeout makes a process holding the file fail fast instead of blocking startup forever.
	db, err := bolt.Open(cfg.Path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("runstore: open %s: %w", cfg.Path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketMeta, bucketOutput, bucketByTime, bucketByHook} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("runstore: init buckets: %w", err)
	}
	return &Store{db: db, cfg: cfg, log: log, stop: make(chan struct{})}, nil
}

// Retention returns the configured retention window.
func (s *Store) Retention() time.Duration { return s.cfg.Retention }

// Record persists terminal run — metadata, indexes, and output — in a
// single write transaction. Non-terminal states are rejected: active runs
// live only in the tracker.
func (s *Store) Record(st runs.RunState) error {
	if !st.Status.Terminal() {
		return fmt.Errorf("runstore: refusing to record non-terminal run %s (%s)", st.ID, st.Status)
	}
	key := timeKey(st.Started, st.ID)
	out := outputRecord{Output: st.Output, OutputTimes: st.OutputTimes}
	st.Output = nil
	st.OutputTimes = nil
	meta, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("runstore: marshal run %s: %w", st.ID, err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketMeta).Put([]byte(st.ID), meta); err != nil {
			return err
		}
		if err := tx.Bucket(bucketByTime).Put(key, []byte(st.HookID)); err != nil {
			return err
		}
		hb, err := tx.Bucket(bucketByHook).CreateBucketIfNotExists([]byte(st.HookID))
		if err != nil {
			return err
		}
		if err := hb.Put(key, summaryValue(st.Status, st.Finished, st.StartedAt)); err != nil {
			return err
		}
		if len(out.Output) == 0 {
			return nil
		}
		ob, err := json.Marshal(out)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketOutput).Put([]byte(st.ID), ob)
	})
}

// Get returns persisted run with its captured output, or ok=false when
// it is absent or past retention (expired entries are hidden here and
// reclaimed by the sweeper, mirroring the kv store's lazy-expiry split).
func (s *Store) Get(id string) (runs.RunState, bool) {
	var st runs.RunState
	found := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get([]byte(id))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			s.log.Error("runstore: corrupt run metadata", "run", id, "err", err)
			return nil
		}
		if s.expired(st.Started, time.Now()) {
			return nil
		}
		if oraw := tx.Bucket(bucketOutput).Get([]byte(id)); oraw != nil {
			var out outputRecord
			if err := json.Unmarshal(oraw, &out); err == nil {
				st.Output = out.Output
				st.OutputTimes = out.OutputTimes
			}
		}
		found = true
		return nil
	})
	return st, found
}

// ListAll returns persisted runs across all hooks, newest-, without
// output, capped at max (<= means no cap).
func (s *Store) ListAll(max int) []runs.RunState {
	return s.ListAllBefore(time.Time{}, max)
}

// ListAllBefore is ListAll paged into the past: only runs whose Started (the queued/accepted time the index keys carry) is STRICTLY before.
func (s *Store) ListAllBefore(before time.Time, max int) []runs.RunState {
	return s.ListAllBeforeFiltered(before, max, nil)
}

// ListAllBeforeFiltered is ListAllBefore with a status predicate applied DURING the walk, so max counts only the runs keep accepts.
func (s *Store) ListAllBeforeFiltered(before time.Time, max int, keep func(runs.Status) bool) []runs.RunState {
	var out []runs.RunState
	_ = s.db.View(func(tx *bolt.Tx) error {
		out = s.collectBefore(tx, tx.Bucket(bucketByTime), before, max, keep)
		return nil
	})
	return out
}

// ListByHook returns hook's persisted runs, newest-, without
// output, capped at max (<= means no cap).
func (s *Store) ListByHook(hookID string, max int) []runs.RunState {
	return s.ListByHookBefore(hookID, time.Time{}, max)
}

// ListByHookBefore is ListByHook paged into the past, with ListAllBefore's exact contract: runs Started strictly before the instant, newest-, capped at.
func (s *Store) ListByHookBefore(hookID string, before time.Time, max int) []runs.RunState {
	return s.ListByHookBeforeFiltered(hookID, before, max, nil)
}

// ListByHookBeforeFiltered is ListByHookBefore with ListAllBeforeFiltered's
// filter- contract. This is the cheap path: the per-hook index's VALUE
// carries the status, so a rejected row costs small parse instead of a
// metadata decode.
func (s *Store) ListByHookBeforeFiltered(hookID string, before time.Time, max int, keep func(runs.Status) bool) []runs.RunState {
	var out []runs.RunState
	_ = s.db.View(func(tx *bolt.Tx) error {
		hb := tx.Bucket(bucketByHook).Bucket([]byte(hookID))
		if hb == nil {
			return nil
		}
		out = s.collectBefore(tx, hb, before, max, keep)
		return nil
	})
	return out
}

// collectBefore is the shared list walk: from the position seekBefore
// selects, it steps the chronological bucket newest-, decoding each
// run's metadata record, until max runs are collected (<= = no cap) or the
// retention-expired key ends the walk. A non-nil keep rejects runs by
// status BEFORE they count against max (see ListAllBeforeFiltered).
func (s *Store) collectBefore(tx *bolt.Tx, b *bolt.Bucket, before time.Time, max int, keep func(runs.Status) bool) []runs.RunState {
	var out []runs.RunState
	now := time.Now()
	meta := tx.Bucket(bucketMeta)
	c := b.Cursor()
	for k, v := seekBefore(c, before); k != nil; k, v = c.Prev() {
		if max > 0 && len(out) >= max {
			break
		}
		started, id, ok := splitKey(k)
		if !ok {
			continue
		}
		// Keys are chronological, so the expired ends the walk.
		if s.expired(started, now) {
			break
		}
		// Cheap rejection: the per-hook index's value is a summary carrying the status, so a filtered walk skips the metadata decode entirely.
		if keep != nil {
			if status, _, _, ok := splitSummary(v); ok && !keep(status) {
				continue
			}
		}
		raw := meta.Get([]byte(id))
		if raw == nil {
			continue
		}
		var st runs.RunState
		if err := json.Unmarshal(raw, &st); err != nil {
			s.log.Error("runstore: corrupt run metadata", "run", id, "err", err)
			continue
		}
		if keep != nil && !keep(st.Status) {
			continue
		}
		out = append(out, st)
	}
	return out
}

// seekBefore positions the cursor at the newest key STRICTLY older than the given instant and returns it with its value (nil key = nothing older); a before starts at the newest key overall.
func seekBefore(c *bolt.Cursor, before time.Time) ([]byte, []byte) {
	if before.IsZero() {
		return c.Last()
	}
	if k, _ := c.Seek(fmt.Appendf(nil, "%019d", before.UnixNano())); k == nil {
		return c.Last()
	}
	return c.Prev()
}

// SummariesByHook returns skeleton states (ID, HookID, Status, Started,
// StartedAt, Finished — nothing else) for every retained run of hook,
// newest-. It reads only the per-hook index (key + summary value),
// never metadata blobs, so aggregating stats over a full retention window
// stays a single cheap cursor walk even at the count cap. StartedAt is
// for runs that never started AND for legacy rows persisted before it was
// indexed — stats treat both as "processing start unknown" (see
// runs.ComputeStats).
func (s *Store) SummariesByHook(hookID string) []runs.RunState {
	var out []runs.RunState
	now := time.Now()
	_ = s.db.View(func(tx *bolt.Tx) error {
		hb := tx.Bucket(bucketByHook).Bucket([]byte(hookID))
		if hb == nil {
			return nil
		}
		c := hb.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			started, id, ok := splitKey(k)
			if !ok {
				continue
			}
			if s.expired(started, now) {
				break
			}
			status, finished, startedAt, ok := splitSummary(v)
			if !ok {
				continue
			}
			out = append(out, runs.RunState{
				ID:        id,
				HookID:    hookID,
				Status:    status,
				Started:   started,
				StartedAt: startedAt,
				Finished:  finished,
			})
		}
		return nil
	})
	return out
}

// StartSweeper launches the background GC. Lazy expiry already hides
// out-of-retention runs from reads; the sweeper reclaims their disk and
// enforces the per-hook count cap.
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
				n, err := s.sweep(time.Now())
				if err != nil {
					s.log.Error("runstore: sweep failed", "err", err)
				} else if n > 0 {
					s.log.Info("runstore: swept persisted runs", "removed", n)
				}
			}
		}
	}()
}

// sweep deletes runs older than Retention and, per hook, the oldest runs
// beyond MaxPerHook, removing each victim from all buckets in
// transaction. Victims are collected and deleted after, so no bucket
// is mutated mid-iteration.
func (s *Store) sweep(now time.Time) (int, error) {
	type victim struct {
		key  []byte
		hook []byte
		id   string
	}
	removed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		output := tx.Bucket(bucketOutput)
		bytime := tx.Bucket(bucketByTime)
		byhook := tx.Bucket(bucketByHook)

		var victims []victim
		// Time pass: bytime is chronological, so expired keys are a prefix.
		c := bytime.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			started, id, ok := splitKey(k)
			if !ok {
				continue
			}
			if !s.expired(started, now) {
				break
			}
			victims = append(victims, victim{
				key:  append([]byte(nil), k...),
				hook: append([]byte(nil), v...),
				id:   id,
			})
		}
		// Count-cap pass: per hook, everything below the newest MaxPerHook.
		hc := byhook.Cursor()
		for name, val := hc.First(); name != nil; name, val = hc.Next() {
			if val != nil {
				continue // only sub-buckets live here
			}
			hb := byhook.Bucket(name)
			excess := hb.Stats().KeyN - s.cfg.MaxPerHook
			if excess <= 0 {
				continue
			}
			hook := append([]byte(nil), name...)
			cc := hb.Cursor()
			for k, _ := cc.First(); k != nil && excess > 0; k, _ = cc.Next() {
				_, id, ok := splitKey(k)
				if !ok {
					continue
				}
				victims = append(victims, victim{
					key:  append([]byte(nil), k...),
					hook: hook,
					id:   id,
				})
				excess--
			}
		}

		for _, v := range victims {
			if err := meta.Delete([]byte(v.id)); err != nil {
				return err
			}
			if err := output.Delete([]byte(v.id)); err != nil {
				return err
			}
			if err := bytime.Delete(v.key); err != nil {
				return err
			}
			if hb := byhook.Bucket(v.hook); hb != nil {
				if err := hb.Delete(v.key); err != nil {
					return err
				}
			}
			removed++
		}
		return nil
	})
	return removed, err
}

// Close stops the sweeper and closes the database. Safe to call more than
// ; only the call does the work.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		s.wg.Wait()
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// expired reports whether a run that started at the given time has fallen out of the retention window (the boundary itself counts as expired.
func (s *Store) expired(started, now time.Time) bool {
	return !started.After(now.Add(-s.cfg.Retention))
}

// timeKey builds the shared chronological index key.
func timeKey(started time.Time, id string) []byte {
	return []byte(fmt.Sprintf("%019d-%s", started.UnixNano(), id))
}

func splitKey(k []byte) (started time.Time, id string, ok bool) {
	i := bytes.IndexByte(k, '-')
	if i < 0 {
		return time.Time{}, "", false
	}
	nanos, err := strconv.ParseInt(string(k[:i]), 10, 64)
	if err != nil {
		return time.Time{}, "", false
	}
	return time.Unix(0, nanos).UTC(), string(k[i+1:]), true
}

// summaryValue encodes the per-hook index value ("<status> <finished-nanos> <startedat-nanos>"). The field is the processing start (RunState.StartedAt), when the run never started. Status tokens never contain spaces, so plain cuts decode it.
func summaryValue(status runs.Status, finished, startedAt time.Time) []byte {
	startedNanos := int64(0)
	if !startedAt.IsZero() {
		startedNanos = startedAt.UnixNano()
	}
	return []byte(fmt.Sprintf("%s %d %d", status, finished.UnixNano(), startedNanos))
}

func splitSummary(v []byte) (status runs.Status, finished, startedAt time.Time, ok bool) {
	st, rest, found := strings.Cut(string(v), " ")
	if !found {
		return "", time.Time{}, time.Time{}, false
	}
	finRaw, startRaw, hasStart := strings.Cut(rest, " ")
	nanos, err := strconv.ParseInt(finRaw, 10, 64)
	if err != nil {
		return "", time.Time{}, time.Time{}, false
	}
	// Legacy -field values ("<status> <finished-nanos>", persisted before the queue-wait/processing split) have no StartedAt: it stays , so their duration falls back to.
	if hasStart {
		startNanos, err := strconv.ParseInt(startRaw, 10, 64)
		if err != nil {
			return "", time.Time{}, time.Time{}, false
		}
		if startNanos != 0 {
			startedAt = time.Unix(0, startNanos).UTC()
		}
	}
	return runs.Status(st), time.Unix(0, nanos).UTC(), startedAt, true
}
