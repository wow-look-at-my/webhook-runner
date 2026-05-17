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
	log    *slog.Logger
	mu     sync.Mutex

	sshKeyPath string
}

// CloneRepo clones url into dir (shallow, single-branch). If dir already
// contains a git repository, it pulls instead of cloning.
func CloneRepo(url, branch, dir, sshKeyPath string, log *slog.Logger) (*Repo, error) {
	r := &Repo{
		url:        url,
		branch:     branch,
		dir:        dir,
		sshKeyPath: sshKeyPath,
		log:        log,
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
	if out, err := r.gitCmd(fetchArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch: %w\n%s", err, out)
	}

	if out, err := r.gitCmd("-C", r.dir, "reset", "--hard", "FETCH_HEAD").CombinedOutput(); err != nil {
		return fmt.Errorf("git reset: %w\n%s", err, out)
	}

	r.log.Info("hooks repo updated")
	return nil
}

func (r *Repo) gitCmd(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	if r.sshKeyPath != "" {
		cmd.Env = append(os.Environ(),
			"GIT_SSH_COMMAND=ssh -i "+r.sshKeyPath+" -o StrictHostKeyChecking=accept-new",
		)
	}
	return cmd
}

// EnsureSSHKey checks for an Ed25519 keypair at keyPath. If none exists,
// it generates one. Returns the path to the private key.
func EnsureSSHKey(keyPath string, log *slog.Logger) (string, error) {
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return "", fmt.Errorf("create ssh key directory: %w", err)
	}

	if _, err := os.Stat(keyPath); err == nil {
		pub, err := os.ReadFile(keyPath + ".pub")
		if err != nil {
			return "", fmt.Errorf("read public key: %w", err)
		}
		printDeployKey(strings.TrimSpace(string(pub)))
		return keyPath, nil
	}

	out, err := exec.Command(
		"ssh-keygen", "-t", "ed25519", "-f", keyPath, "-N", "", "-C", "webhook-runner",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh-keygen: %w\n%s", err, out)
	}

	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return "", fmt.Errorf("read public key: %w", err)
	}
	printDeployKey(strings.TrimSpace(string(pub)))
	return keyPath, nil
}

func printDeployKey(pub string) {
	fmt.Fprintf(os.Stderr, "\n"+
		"Add this deploy key to your repository:\n\n"+
		"  %s\n\n", pub)
}

func isGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}
