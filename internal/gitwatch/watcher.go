// Package gitwatch clones a remote git repo into memory and watches it for
// changes, triggering a callback whenever the watched branch advances.
package gitwatch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/gerrowadat/nomad-botherer/internal/config"
)

// Watcher holds a live in-memory clone of a git repo and keeps it up to date.
type Watcher struct {
	cfg *config.Config

	mu         sync.RWMutex
	repo       *git.Repository
	lastCommit string
	lastUpdate time.Time

	triggerCh chan struct{}
	onChange  func(commit string)

	gitFetches          prometheus.Counter
	gitFetchErrors      prometheus.Counter
	gitLastUpdate       prometheus.Gauge
	staleRefreshes      prometheus.Counter
}

// New creates a Watcher that registers metrics into the default Prometheus registry.
func New(cfg *config.Config, onChange func(commit string)) *Watcher {
	return NewWithRegistry(cfg, onChange, prometheus.DefaultRegisterer)
}

// NewWithRegistry creates a Watcher that registers metrics into reg.
// Use this in tests to avoid duplicate-registration panics.
func NewWithRegistry(cfg *config.Config, onChange func(commit string), reg prometheus.Registerer) *Watcher {
	f := promauto.With(reg)
	return &Watcher{
		cfg:       cfg,
		triggerCh: make(chan struct{}, 1),
		onChange:  onChange,
		gitFetches: f.NewCounter(prometheus.CounterOpts{
			Name: "nomad_botherer_git_fetches_total",
			Help: "Total number of remote git fetch/clone attempts.",
		}),
		gitFetchErrors: f.NewCounter(prometheus.CounterOpts{
			Name: "nomad_botherer_git_fetch_errors_total",
			Help: "Total number of remote git fetch/clone failures.",
		}),
		gitLastUpdate: f.NewGauge(prometheus.GaugeOpts{
			Name: "nomad_botherer_git_last_update_timestamp_seconds",
			Help: "Unix timestamp of the most recent successful git fetch.",
		}),
		staleRefreshes: f.NewCounter(prometheus.CounterOpts{
			Name: "nomad_botherer_git_staleness_refreshes_total",
			Help: "Total number of git fetches triggered by the staleness check.",
		}),
	}
}

// Clone performs the initial clone into memory. Must be called before Run.
func (w *Watcher) Clone(ctx context.Context) error {
	auth, err := w.buildAuth()
	if err != nil {
		return fmt.Errorf("building git auth: %w", err)
	}

	slog.Info("Cloning repository", "url", w.cfg.RepoURL, "branch", w.cfg.Branch)
	w.gitFetches.Inc()

	storer := memory.NewStorage()
	fs := memfs.New()

	repo, err := git.CloneContext(ctx, storer, fs, &git.CloneOptions{
		URL:           w.cfg.RepoURL,
		ReferenceName: plumbing.NewBranchReferenceName(w.cfg.Branch),
		SingleBranch:  true,
		Auth:          auth,
		Progress:      nil,
	})
	if err != nil {
		w.gitFetchErrors.Inc()
		return fmt.Errorf("cloning %s: %w", w.cfg.RepoURL, err)
	}

	commit, err := headCommit(repo)
	if err != nil {
		w.gitFetchErrors.Inc()
		return err
	}

	now := time.Now()
	w.mu.Lock()
	w.repo = repo
	w.lastCommit = commit
	w.lastUpdate = now
	w.mu.Unlock()

	w.gitLastUpdate.Set(float64(now.Unix()))
	slog.Info("Repository cloned", "commit", commit)
	return nil
}

// Run polls for updates on the configured interval and also reacts to Trigger
// calls. Blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pull(ctx)
		case <-w.triggerCh:
			slog.Info("Webhook trigger received, pulling")
			w.pull(ctx)
		}
	}
}

// Trigger schedules an immediate fetch, e.g. when a webhook fires.
// Non-blocking: if a trigger is already pending it is coalesced.
func (w *Watcher) Trigger() {
	select {
	case w.triggerCh <- struct{}{}:
	default:
	}
}

// TriggerStale schedules an immediate fetch because the repo has exceeded the
// configured maximum staleness. Increments the staleness counter and delegates
// to Trigger.
func (w *Watcher) TriggerStale() {
	w.staleRefreshes.Inc()
	w.Trigger()
}

// Ready reports whether the initial clone has completed successfully.
// Before Clone returns, Status and ReadHCLFiles return zero/nil values.
func (w *Watcher) Ready() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.repo != nil
}

// Status returns the last seen commit hash and the time it was seen.
func (w *Watcher) Status() (lastCommit string, lastUpdate time.Time) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.lastCommit, w.lastUpdate
}

// LastCommit returns just the last commit hash.
func (w *Watcher) LastCommit() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.lastCommit
}

// ReadHCLFiles returns a map of repo-relative path → file content for every
// .hcl file under the configured HCLDir.
func (w *Watcher) ReadHCLFiles() (map[string]string, error) {
	w.mu.RLock()
	repo := w.repo
	w.mu.RUnlock()

	if repo == nil {
		return nil, fmt.Errorf("repository not cloned yet")
	}

	ref, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("getting HEAD: %w", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("getting commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("getting commit tree: %w", err)
	}

	// Build prefix filter for HCLDir.
	hclPrefix := normalizeHCLDir(w.cfg.HCLDir)

	result := make(map[string]string)
	err = tree.Files().ForEach(func(f *object.File) error {
		if !strings.HasSuffix(f.Name, ".hcl") {
			return nil
		}
		if hclPrefix != "" && !strings.HasPrefix(f.Name, hclPrefix) {
			return nil
		}
		content, err := f.Contents()
		if err != nil {
			slog.Warn("Could not read HCL file from git tree", "file", f.Name, "err", err)
			return nil // skip bad files, don't abort the walk
		}
		result[f.Name] = content
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking commit tree: %w", err)
	}

	slog.Debug("Read HCL files from repo", "count", len(result), "hcl_dir", w.cfg.HCLDir)
	return result, nil
}

// FileAtParentOf returns the content of path as it was at the first parent of
// the named commit. ok is false when the repo is not cloned, the commit is
// unknown or has no parent (the root commit), or the file did not exist at the
// parent. Keying off an explicit commit — rather than the repo's current HEAD —
// keeps the answer consistent with the HCL snapshot being evaluated even if a
// concurrent pull has advanced HEAD. It is used to decide whether a change at
// that commit (such as adding the managed meta tag) is new relative to the
// previous commit.
func (w *Watcher) FileAtParentOf(commit, path string) (content string, ok bool) {
	w.mu.RLock()
	repo := w.repo
	w.mu.RUnlock()
	if repo == nil || commit == "" {
		return "", false
	}

	c, err := repo.CommitObject(plumbing.NewHash(commit))
	if err != nil || c.NumParents() == 0 {
		return "", false
	}
	parent, err := c.Parent(0)
	if err != nil {
		return "", false
	}
	f, err := parent.File(path)
	if err != nil {
		// Includes object.ErrFileNotFound: the file did not exist at the parent.
		return "", false
	}
	content, err = f.Contents()
	if err != nil {
		return "", false
	}
	return content, true
}

// pull fetches the latest changes and calls onChange if the HEAD moved.
func (w *Watcher) pull(ctx context.Context) {
	auth, err := w.buildAuth()
	if err != nil {
		slog.Error("Building git auth for pull", "err", err)
		return
	}

	w.mu.RLock()
	repo := w.repo
	w.mu.RUnlock()

	wt, err := repo.Worktree()
	if err != nil {
		slog.Error("Getting worktree", "err", err)
		return
	}

	w.gitFetches.Inc()
	err = wt.PullContext(ctx, &git.PullOptions{
		RemoteName:    "origin",
		ReferenceName: plumbing.NewBranchReferenceName(w.cfg.Branch),
		SingleBranch:  true,
		Force:         true,
		Auth:          auth,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		w.gitFetchErrors.Inc()
		slog.Warn("Pull failed, attempting re-clone", "err", err)
		if err2 := w.Clone(ctx); err2 != nil {
			slog.Error("Re-clone failed", "err", err2)
			return
		}
		// Clone already updated state and gauges; check if commit changed.
	}

	commit, err := headCommit(repo)
	if err != nil {
		slog.Error("Getting HEAD after pull", "err", err)
		return
	}

	now := time.Now()
	w.mu.Lock()
	prev := w.lastCommit
	w.lastCommit = commit
	w.lastUpdate = now
	w.mu.Unlock()

	w.gitLastUpdate.Set(float64(now.Unix()))

	if commit != prev {
		slog.Info("New commit on branch", "branch", w.cfg.Branch, "commit", commit, "prev", prev)
		if w.onChange != nil {
			w.onChange(commit)
		}
	}
}

func (w *Watcher) buildAuth() (transport.AuthMethod, error) {
	if w.cfg.GitSSHKeyPath != "" {
		auth, err := gitssh.NewPublicKeysFromFile("git", w.cfg.GitSSHKeyPath, w.cfg.GitSSHKeyPass)
		if err != nil {
			return nil, fmt.Errorf("loading SSH key from %s: %w", w.cfg.GitSSHKeyPath, err)
		}
		if err := w.setSSHHostKeyCallback(auth); err != nil {
			return nil, err
		}
		return auth, nil
	}
	if w.cfg.GitToken != "" {
		return &githttp.BasicAuth{
			Username: "x-token", // username is ignored by GitHub for token auth
			Password: w.cfg.GitToken,
		}, nil
	}
	return nil, nil // anonymous / SSH agent
}

// setSSHHostKeyCallback configures host key verification on auth.
// When --git-ssh-known-hosts is set the named file is required; if it cannot
// be opened, an error is returned. Without the flag go-git's default known_hosts
// locations (~/..ssh/known_hosts, /etc/ssh/ssh_known_hosts) are tried. If none
// are found, verification is skipped and a warning is logged.
func (w *Watcher) setSSHHostKeyCallback(auth *gitssh.PublicKeys) error {
	if w.cfg.GitSSHKnownHostsFile != "" {
		cb, err := gitssh.NewKnownHostsCallback(w.cfg.GitSSHKnownHostsFile)
		if err != nil {
			return fmt.Errorf("loading known_hosts from %s: %w", w.cfg.GitSSHKnownHostsFile, err)
		}
		auth.HostKeyCallback = cb
		return nil
	}
	cb, err := gitssh.NewKnownHostsCallback()
	if err != nil {
		slog.Warn("SSH host key verification disabled: no known_hosts file found; "+
			"set --git-ssh-known-hosts / GIT_SSH_KNOWN_HOSTS to enable verification", "err", err)
		return nil
	}
	auth.HostKeyCallback = cb
	return nil
}

func headCommit(repo *git.Repository) (string, error) {
	ref, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("getting HEAD: %w", err)
	}
	return ref.Hash().String(), nil
}

// normalizeHCLDir converts a user-supplied HCLDir value to the prefix used
// when filtering tree file paths (e.g. "jobs" → "jobs/", "" → "").
func normalizeHCLDir(dir string) string {
	dir = strings.Trim(dir, "/")
	if dir == "" || dir == "." {
		return ""
	}
	return dir + "/"
}
