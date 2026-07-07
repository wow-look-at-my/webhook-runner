package kv

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("test-secret"), nil)
	require.NoError(t, err)
	return s
}

func TestCRUD(t *testing.T) {
	s := newStore(t)

	_, ok := s.Get("ns", "missing")
	require.False(t, ok)

	require.NoError(t, s.Set("ns", "k", []byte("v1"), 0))
	v, ok := s.Get("ns", "k")
	require.True(t, ok)
	require.Equal(t, "v1", string(v))

	// Overwrite.
	require.NoError(t, s.Set("ns", "k", []byte("v2"), 0))
	v, ok = s.Get("ns", "k")
	require.True(t, ok)
	require.Equal(t, "v2", string(v))

	// List is sorted.
	require.NoError(t, s.Set("ns", "a", []byte("x"), 0))
	require.Equal(t, []string{"a", "k"}, s.List("ns"))

	// Delete is idempotent.
	require.NoError(t, s.Delete("ns", "k"))
	require.NoError(t, s.Delete("ns", "k"))
	_, ok = s.Get("ns", "k")
	require.False(t, ok)
}

func TestGetReturnsCopy(t *testing.T) {
	s := newStore(t)
	require.NoError(t, s.Set("ns", "k", []byte("orig"), 0))

	v, _ := s.Get("ns", "k")
	v[0] = 'X' // must not mutate the stored value
	v2, _ := s.Get("ns", "k")
	require.Equal(t, "orig", string(v2))
}

func TestPersistenceReload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kv")
	s, err := New(Config{Dir: dir}, []byte("secret"), nil)
	require.NoError(t, err)
	require.NoError(t, s.Set("counter", "plain", []byte("42"), 0))
	require.NoError(t, s.Set("counter", "ttl", []byte("keep"), time.Hour))

	// A brand-new Store over the same dir must see the data — this is the
	// "survives a server restart" guarantee.
	s2, err := New(Config{Dir: dir}, []byte("secret"), nil)
	require.NoError(t, err)
	v, ok := s2.Get("counter", "plain")
	require.True(t, ok)
	require.Equal(t, "42", string(v))
	v, ok = s2.Get("counter", "ttl")
	require.True(t, ok)
	require.Equal(t, "keep", string(v))
}

func TestTTLExpiry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kv")
	s, err := New(Config{Dir: dir}, []byte("secret"), nil)
	require.NoError(t, err)
	require.NoError(t, s.Set("ns", "fast", []byte("v"), 10*time.Millisecond))
	time.Sleep(30 * time.Millisecond)

	_, ok := s.Get("ns", "fast")
	require.False(t, ok, "expired key still readable")
	require.Empty(t, s.List("ns"), "List included expired key")

	// The sweeper reclaims it from the file too.
	s.sweep()
	data, err := os.ReadFile(filepath.Join(dir, "ns.json"))
	require.NoError(t, err)
	require.Equal(t, "{}", string(data))
}

func TestIncr(t *testing.T) {
	s := newStore(t)

	n, err := s.Incr("ns", "c", 1, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	n, err = s.Incr("ns", "c", 5, 0)
	require.NoError(t, err)
	require.Equal(t, int64(6), n)

	n, err = s.Incr("ns", "c", -2, 0)
	require.NoError(t, err)
	require.Equal(t, int64(4), n)

	// A non-integer value is never silently reset.
	require.NoError(t, s.Set("ns", "word", []byte("hello"), 0))
	_, err = s.Incr("ns", "word", 1, 0)
	require.Equal(t, ErrNotInteger, err)
	v, _ := s.Get("ns", "word")
	require.Equal(t, "hello", string(v))
}

func TestIncrExpiredStartsFromZero(t *testing.T) {
	s := newStore(t)
	_, err := s.Incr("ns", "c", 7, 10*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(30 * time.Millisecond)
	n, err := s.Incr("ns", "c", 1, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

func TestIncrPreservesTTL(t *testing.T) {
	s := newStore(t)
	_, err := s.Incr("ns", "c", 1, time.Hour)
	require.NoError(t, err)
	// ttl<=0 must not drop the existing expiry.
	_, err = s.Incr("ns", "c", 1, 0)
	require.NoError(t, err)

	s.mu.RLock()
	e := s.ns["ns"]["c"]
	s.mu.RUnlock()
	require.NotNil(t, e.Expires, "Incr with ttl<=0 dropped the existing expiry")
}

func TestIncrRace(t *testing.T) {
	s := newStore(t)
	const goroutines, iterations = 16, 100
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, err := s.Incr("ns", "c", 1, 0)
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()
	v, _ := s.Get("ns", "c")
	n, _ := strconv.ParseInt(string(v), 10, 64)
	require.Equal(t, int64(goroutines*iterations), n)
}

func TestCaps(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kv")
	s, err := New(Config{Dir: dir, MaxValueBytes: 8, MaxKeysPerNS: 2, MaxNamespaces: 2}, []byte("secret"), nil)
	require.NoError(t, err)

	require.Equal(t, ErrValueTooLarge, s.Set("ns", "k", []byte("123456789"), 0))
	require.NoError(t, s.Set("ns", "a", []byte("x"), 0))
	require.NoError(t, s.Set("ns", "b", []byte("x"), 0))
	require.Equal(t, ErrTooManyKeys, s.Set("ns", "c", []byte("x"), 0))
	// Overwriting an existing key is always allowed.
	require.NoError(t, s.Set("ns", "a", []byte("y"), 0))
	// ns is the first namespace; one more is allowed, a third is not.
	require.NoError(t, s.Set("ns2", "k", []byte("x"), 0))
	require.Equal(t, ErrTooManyNS, s.Set("ns3", "k", []byte("x"), 0))
}

func TestDefaultLimits(t *testing.T) {
	// Zero-valued limits fall back to the built-in defaults.
	s := newStore(t)
	require.Equal(t, 64*1024, s.cfg.MaxValueBytes)
	require.Equal(t, 5000, s.cfg.MaxKeysPerNS)
	require.Equal(t, 256, s.cfg.MaxNamespaces)
}

func TestBadNamespace(t *testing.T) {
	s := newStore(t)
	for _, ns := range []string{"", "../etc", "UPPER", "has/slash", ".hidden", "a.b"} {
		assert.Equalf(t, ErrBadNamespace, s.Set(ns, "k", []byte("v"), 0), "ns=%q", ns)
	}
	// A normal hook ID is accepted.
	require.NoError(t, s.Set("pr-describe", "k", []byte("v"), 0))
}

func TestToken(t *testing.T) {
	s := newStore(t)
	tok := s.Token("my-hook")
	ns, ok := s.VerifyToken(tok)
	require.True(t, ok)
	require.Equal(t, "my-hook", ns)

	// Tampered MAC.
	_, ok = s.VerifyToken(tok + "x")
	require.False(t, ok)

	// Cross-namespace forgery: keep a valid MAC but swap the namespace.
	_, ok = s.VerifyToken("other" + tok[len("my-hook"):])
	require.False(t, ok)

	// Different secret.
	other, err := New(Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("other-secret"), nil)
	require.NoError(t, err)
	_, ok = other.VerifyToken(tok)
	require.False(t, ok)

	// Garbage.
	for _, bad := range []string{"", "no-dot", "ns.", ".mac", "ns.not-base64!!"} {
		_, ok = s.VerifyToken(bad)
		assert.Falsef(t, ok, "bad=%q", bad)
	}
}

func TestStats(t *testing.T) {
	s := newStore(t)
	require.NoError(t, s.Set("b", "k1", []byte("xx"), 0))
	require.NoError(t, s.Set("a", "k1", []byte("y"), 0))
	require.NoError(t, s.Set("a", "k2", []byte("zz"), 0))

	stats := s.Stats()
	require.Len(t, stats, 2)
	// Sorted by namespace.
	require.Equal(t, "a", stats[0].Namespace)
	require.Equal(t, 2, stats[0].Keys)
	require.Equal(t, 3, stats[0].Bytes)
	require.Equal(t, "b", stats[1].Namespace)
	require.Equal(t, 1, stats[1].Keys)
}

func TestKeys(t *testing.T) {
	s := newStore(t)

	// Unknown namespace: ok=false (the admin endpoint's 404).
	_, ok := s.Keys("nope")
	require.False(t, ok)

	require.NoError(t, s.Set("ns", "b", []byte("xx"), 0))
	require.NoError(t, s.Set("ns", "a", []byte("y"), time.Hour))
	require.NoError(t, s.Set("ns", "gone", []byte("zzz"), 10*time.Millisecond))
	time.Sleep(30 * time.Millisecond)

	keys, ok := s.Keys("ns")
	require.True(t, ok)
	// Sorted by key, TTL-expired hidden.
	require.Len(t, keys, 2)
	require.Equal(t, "a", keys[0].Key)
	require.Equal(t, 1, keys[0].Bytes)
	require.NotNil(t, keys[0].ExpiresAt, "TTL'd key must carry its expiry")
	require.True(t, keys[0].ExpiresAt.After(time.Now()))
	require.Equal(t, "b", keys[1].Key)
	require.Equal(t, 2, keys[1].Bytes)
	require.Nil(t, keys[1].ExpiresAt, "no-TTL key must omit expiry")

	// An existing namespace whose keys all expired is still ok=true, empty.
	require.NoError(t, s.Set("empty", "k", []byte("v"), 10*time.Millisecond))
	time.Sleep(30 * time.Millisecond)
	keys, ok = s.Keys("empty")
	require.True(t, ok)
	require.Empty(t, keys)
}

func TestEnsureSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state-secret")
	a, err := EnsureSecret(path)
	require.NoError(t, err)
	require.Len(t, a, 32)

	// A second call returns the same persisted secret.
	b, err := EnsureSecret(path)
	require.NoError(t, err)
	require.Equal(t, a, b)

	// Persisted with owner-only permissions.
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestNoLingeringTempFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kv")
	s, err := New(Config{Dir: dir}, []byte("secret"), nil)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		require.NoError(t, s.Set("ns", "k"+strconv.Itoa(i), []byte("v"), 0))
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".*tmp*"))
	require.Empty(t, matches)
}
