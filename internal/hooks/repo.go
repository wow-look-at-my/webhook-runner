package hooks

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
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

	if err := r.clone(); err != nil {
		return nil, err
	}
	return r, nil
}

// OpenRepo opens the clone of url at dir WITHOUT moving an existing working tree: if dir already contains a git repository it is used exactly as-is (no fetch, no reset) — the reload CI gate decides when the tree moves. A missing dir is still cloned fresh (branch tip; the gate then flags it unverified until the first green).
func OpenRepo(url, branch, dir, sshKeyPath string, log *slog.Logger) (*Repo, error) {
	r := &Repo{
		url:        url,
		branch:     branch,
		dir:        dir,
		sshKeyPath: sshKeyPath,
		log:        log,
	}

	if isGitRepo(dir) {
		log.Info("hooks repo already cloned, leaving working tree untouched", "dir", dir)
		return r, nil
	}

	if err := r.clone(); err != nil {
		return nil, err
	}
	return r, nil
}

// clone performs the initial shallow, single-branch clone into r.dir.
func (r *Repo) clone() error {
	if err := os.MkdirAll(filepath.Dir(r.dir), 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	args := []string{"clone", "--depth=1", "--single-branch"}
	if r.branch != "" {
		args = append(args, "--branch", r.branch)
	}
	args = append(args, r.url, r.dir)

	out, err := r.gitCmd(args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone %s: %w\n%s", r.url, err, out)
	}
	r.log.Info("hooks repo cloned", "url", r.url, "branch", r.branch, "dir", r.dir)
	return nil
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

// Head returns the commit the working tree is currently checked out at.
func (r *Repo) Head() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out, err := r.gitCmd("-C", r.dir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w\n%s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// FetchBranch fetches the tracked branch from origin at the given history depth WITHOUT touching the working tree, and returns the fetched.
func (r *Repo) FetchBranch(depth int) (string, error) {
	return r.FetchBranchContext(context.Background(), depth)
}

// FetchBranchContext is FetchBranch with a KILL SWITCH. A `git fetch` against
// a degraded GitHub does not fail — it HANGS, and it hangs holding r.mu, so
// every later ResetTo/ResolveRef queues behind it. That is how one unreachable
// remote wedges the reload gate's own escape hatch. A caller that must stay
// answerable (the operator's manual switch) passes a deadline; the context
// kills the git process rather than waiting on it.
func (r *Repo) FetchBranchContext(ctx context.Context, depth int) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fetchArgs := []string{"-C", r.dir, "fetch", fmt.Sprintf("--depth=%d", depth), "origin"}
	if r.branch != "" {
		fetchArgs = append(fetchArgs, r.branch)
	}
	if out, err := r.gitCmdContext(ctx, fetchArgs...).CombinedOutput(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("git fetch: %w (killed after the deadline; the remote was not answering)\n%s", ctxErr, out)
		}
		return "", fmt.Errorf("git fetch: %w\n%s", err, out)
	}

	out, err := r.gitCmd("-C", r.dir, "rev-parse", "FETCH_HEAD").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse FETCH_HEAD: %w\n%s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// RecentCommits lists up to max commits reachable from the last fetch
// (FETCH_HEAD), newest first. Call FetchBranch first — a fresh clone has
// no FETCH_HEAD yet.
func (r *Repo) RecentCommits(max int) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out, err := r.gitCmd("-C", r.dir, "rev-list", fmt.Sprintf("--max-count=%d", max), "FETCH_HEAD").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git rev-list: %w\n%s", err, out)
	}
	return strings.Fields(string(out)), nil
}

// ResetTo hard-resets the working tree to sha, which must already be
// present locally (see FetchBranch / FetchSHA).
func (r *Repo) ResetTo(sha string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if out, err := r.gitCmd("-C", r.dir, "reset", "--hard", sha).CombinedOutput(); err != nil {
		return fmt.Errorf("git reset: %w\n%s", err, out)
	}
	r.log.Info("hooks repo reset", "sha", sha)
	return nil
}

// FetchSHA fetches one commit by sha from origin at the given depth —
// the startup last-good restore path (GitHub serves reachable-sha
// fetches).
func (r *Repo) FetchSHA(sha string, depth int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if out, err := r.gitCmd("-C", r.dir, "fetch", fmt.Sprintf("--depth=%d", depth), "origin", sha).CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch %s: %w\n%s", sha, err, out)
	}
	return nil
}

// CommitInfo returns the subject line and committer date of a commit that
// is already present locally (fetch it first — see FetchBranch/FetchSHA/
// ResolveRef). Read-only plumbing: it never touches the working tree.
func (r *Repo) CommitInfo(sha string) (subject string, date time.Time, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out, cerr := r.gitCmd("-C", r.dir, "show", "-s", "--format=%cI%x1f%s", sha).CombinedOutput()
	if cerr != nil {
		return "", time.Time{}, fmt.Errorf("git show %s: %w\n%s", sha, cerr, out)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\x1f", 2)
	if len(parts) != 2 {
		return "", time.Time{}, fmt.Errorf("git show %s: unexpected output %q", sha, out)
	}
	when, perr := time.Parse(time.RFC3339, parts[0])
	if perr != nil {
		// The subject is still useful without a parseable date.
		return parts[1], time.Time{}, nil
	}
	return parts[1], when, nil
}

// TreeHasDir reports whether the commit's TREE contains the given path (`git cat-file -e <sha>:<path>`) — pure object inspection, never a checkout.
func (r *Repo) TreeHasDir(sha, path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.gitCmd("-C", r.dir, "cat-file", "-e", sha+":"+path).Run() == nil
}

// resolveRefDepth bounds how much history a ResolveRef by-name fetch pulls — the same order of magnitude as the reload gate's ordering.
const resolveRefDepth = 100

// validManualRef guards ResolveRef's user-supplied ref before it becomes a git argument: plausible ref characters only, and never.
var validManualRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@-]{0,250}$`)

// ResolveRef resolves ref — a full or abbreviated commit sha, or a
// branch/tag name — to a full commit sha. Branch/tag names are fetched
// from origin by name first so they resolve to origin's CURRENT commit
// (the stale local checkout ref must never win); full shas verify locally
// and fall back to a reachable-sha fetch; abbreviated shas resolve against
// local history only (origin cannot serve them by name). The working tree
// is never touched.
func (r *Repo) ResolveRef(ref string) (string, error) {
	if !validManualRef.MatchString(ref) {
		return "", fmt.Errorf("invalid ref %q", ref)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	revParse := func(rev string) (string, bool) {
		out, err := r.gitCmd("-C", r.dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}").Output()
		sha := strings.TrimSpace(string(out))
		return sha, err == nil && sha != ""
	}

	if isFullSHA(ref) {
		if sha, ok := revParse(ref); ok {
			return sha, nil
		}
		// Not local yet: GitHub serves reachable-sha fetches.
		if out, err := r.gitCmd("-C", r.dir, "fetch", fmt.Sprintf("--depth=%d", resolveRefDepth), "origin", ref).CombinedOutput(); err != nil {
			return "", fmt.Errorf("fetch %s: %w\n%s", ref, err, out)
		}
		if sha, ok := revParse(ref); ok {
			return sha, nil
		}
		return "", fmt.Errorf("commit %s not found on origin", ref)
	}

	// A branch/tag name: fetch it by name so origin's current commit wins
	// over any stale local ref of the same name.
	if _, err := r.gitCmd("-C", r.dir, "fetch", fmt.Sprintf("--depth=%d", resolveRefDepth), "origin", ref).CombinedOutput(); err == nil {
		if sha, ok := revParse("FETCH_HEAD"); ok {
			return sha, nil
		}
	}
	// Not fetchable by name (e.g. an abbreviated sha): local resolution.
	if sha, ok := revParse(ref); ok {
		return sha, nil
	}
	return "", fmt.Errorf("cannot resolve ref %q (not a branch, tag, or known commit)", ref)
}

// isFullSHA reports whether s looks like a full hex object name.
func isFullSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func (r *Repo) gitCmd(args ...string) *exec.Cmd {
	return r.gitCmdContext(context.Background(), args...)
}

func (r *Repo) gitCmdContext(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
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
