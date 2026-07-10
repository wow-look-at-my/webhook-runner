package hooks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// SecretsFileName is the per-hook encrypted secrets file: a sops-encrypted
// dotenv file sitting next to hook.json. Its decrypted KEY=VALUE entries are
// injected into the hook's container environment and are resolvable by
// ${NAME} references in hook.json (env values and api_key), so secrets can
// live in the hooks repo instead of the runner host's environment.
const SecretsFileName = "secrets.sops.env"

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SecretsLoader decrypts per-hook sops secrets files by shelling out to the
// sops binary (same convention as docker: no SDK). Decryption happens at
// run/request time, never at load/validate time, so `validate` in CI needs
// neither sops nor any keys. Results are cached per file and invalidated by
// mtime+size, so a `git pull` of the hooks repo picks up new values without
// a process restart.
type SecretsLoader struct {
	sops string

	mu    sync.Mutex
	cache map[string]*cachedSecrets
}

type cachedSecrets struct {
	modTime time.Time
	size    int64
	values  map[string]string
}

// NewSecretsLoader returns a loader using the given sops binary ("" means
// "sops" from PATH).
func NewSecretsLoader(sopsBin string) *SecretsLoader {
	if sopsBin == "" {
		sopsBin = "sops"
	}
	return &SecretsLoader{sops: sopsBin, cache: make(map[string]*cachedSecrets)}
}

// Load returns the hook's decrypted secrets, or nil when the hook has no
// secrets file (the common case — not an error). Decrypt and parse failures
// are errors: a hook that ships a secrets file expects them, so callers must
// fail loud (the runner fails the run, auth fails closed) rather than run
// without them. The returned map is shared with the cache — callers must not
// mutate it.
func (l *SecretsLoader) Load(hook *Hook) (map[string]string, error) {
	dir := hook.Dir()
	if dir == "" {
		return nil, nil
	}
	path := filepath.Join(dir, SecretsFileName)
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		l.mu.Lock()
		delete(l.cache, path)
		l.mu.Unlock()
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	l.mu.Lock()
	if c, ok := l.cache[path]; ok && c.modTime.Equal(fi.ModTime()) && c.size == fi.Size() {
		values := c.values
		l.mu.Unlock()
		return values, nil
	}
	l.mu.Unlock()

	out, err := exec.Command(l.sops, "--decrypt", "--input-type", "dotenv", "--output-type", "dotenv", path).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("sops decrypt %s: %s", path, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("sops decrypt %s: %w", path, err)
	}
	values, err := parseDotenv(out)
	if err != nil {
		return nil, fmt.Errorf("decrypted %s: %w", path, err)
	}

	l.mu.Lock()
	l.cache[path] = &cachedSecrets{modTime: fi.ModTime(), size: fi.Size(), values: values}
	l.mu.Unlock()
	return values, nil
}

// SecretsFirstLookup returns an ExpandEnvRefs lookup that resolves names from
// the given secrets first and falls back to the host environment. A nil map
// degrades to plain host-env lookup.
func SecretsFirstLookup(secrets map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if v, ok := secrets[name]; ok {
			return v, true
		}
		return os.LookupEnv(name)
	}
}

// parseDotenv parses sops' decrypted dotenv output: KEY=VALUE per line,
// blank lines and #-comments ignored, values taken verbatim (no quote
// stripping). Anything else is an error — silently dropping a malformed
// secret would be worse than failing the run.
func parseDotenv(data []byte) (map[string]string, error) {
	values := make(map[string]string)
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: not KEY=VALUE", i+1)
		}
		if !envNamePattern.MatchString(key) {
			return nil, fmt.Errorf("line %d: invalid variable name %q", i+1, key)
		}
		values[key] = value
	}
	return values, nil
}
