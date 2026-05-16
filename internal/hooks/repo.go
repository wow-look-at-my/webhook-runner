package hooks

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Repo manages a shallow git clone of a hooks repository. It provides
// Pull to fetch the latest commit and hard-reset the working tree.
type Repo struct {
	url    string
	branch string
	dir    string
	token  string
	log    *slog.Logger
	mu     sync.Mutex

	askpassPath string
}

// CloneRepo clones url into dir (shallow, single-branch). If dir already
// contains a git repository, it pulls instead of cloning.
func CloneRepo(url, branch, dir, token string, log *slog.Logger) (*Repo, error) {
	r := &Repo{
		url:    url,
		branch: branch,
		dir:    dir,
		token:  token,
		log:    log,
	}

	if token != "" {
		path, err := writeAskpass(token)
		if err != nil {
			return nil, fmt.Errorf("setup git credentials: %w", err)
		}
		r.askpassPath = path
	}

	if isGitRepo(dir) {
		log.Info("hooks repo already cloned, updating", "dir", dir)
		if err := r.Pull(); err != nil {
			return nil, fmt.Errorf("update existing clone: %w", err)
		}
		return r, nil
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, fmt.Errorf("create parent directory: %w", err)
	}

	args := []string{"clone", "--depth=1", "--single-branch"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, url, dir)

	out, err := r.gitCmd(args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git clone %s: %w\n%s", url, err, sanitize(string(out), token))
	}
	log.Info("hooks repo cloned", "url", url, "branch", branch, "dir", dir)
	return r, nil
}

// Dir returns the local path to the cloned repository.
func (r *Repo) Dir() string { return r.dir }

// Pull fetches the latest commit from origin and hard-resets the working
// tree. The mutex serializes concurrent pull requests.
func (r *Repo) Pull() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	fetchArgs := []string{"-C", r.dir, "fetch", "--depth=1", "origin"}
	if r.branch != "" {
		fetchArgs = append(fetchArgs, r.branch)
	}
	if out, err := r.gitCmd(fetchArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch: %w\n%s", err, sanitize(string(out), r.token))
	}

	if out, err := r.gitCmd("-C", r.dir, "reset", "--hard", "FETCH_HEAD").CombinedOutput(); err != nil {
		return fmt.Errorf("git reset: %w\n%s", err, sanitize(string(out), r.token))
	}

	r.log.Info("hooks repo updated")
	return nil
}

func (r *Repo) gitCmd(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	if r.askpassPath != "" {
		cmd.Env = append(os.Environ(),
			"GIT_ASKPASS="+r.askpassPath,
			"GIT_TERMINAL_PROMPT=0",
		)
	}
	return cmd
}

func writeAskpass(token string) (string, error) {
	f, err := os.CreateTemp("", "webhook-runner-askpass-*")
	if err != nil {
		return "", err
	}
	script := "#!/bin/sh\ncase \"$1\" in\nUsername*|username*) echo x-access-token ;;\n*) echo '" + strings.ReplaceAll(token, "'", "'\\''") + "' ;;\nesac\n"
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func sanitize(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[REDACTED]")
}

func isGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}
