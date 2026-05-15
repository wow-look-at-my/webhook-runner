package hooks

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Repo manages a shallow git clone of a hooks repository. It provides
// Pull to fetch the latest commit and hard-reset the working tree.
type Repo struct {
	url    string
	branch string
	dir    string
	log    *slog.Logger
	mu     sync.Mutex
}

// CloneRepo clones url into dir (shallow, single-branch). If dir already
// contains a git repository, it pulls instead of cloning.
func CloneRepo(url, branch, dir string, log *slog.Logger) (*Repo, error) {
	r := &Repo{
		url:    url,
		branch: branch,
		dir:    dir,
		log:    log,
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

	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git clone %s: %w\n%s", url, err, out)
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
	if out, err := exec.Command("git", fetchArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch: %w\n%s", err, out)
	}

	if out, err := exec.Command("git", "-C", r.dir, "reset", "--hard", "FETCH_HEAD").CombinedOutput(); err != nil {
		return fmt.Errorf("git reset: %w\n%s", err, out)
	}

	r.log.Info("hooks repo updated")
	return nil
}

func isGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}
