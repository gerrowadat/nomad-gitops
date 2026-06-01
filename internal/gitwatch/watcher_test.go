package gitwatch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gerrowadat/nomad-botherer/internal/config"
)

// newTestWatcher creates a Watcher with a throwaway registry so tests don't
// collide on DefaultRegisterer.
func newTestWatcher(cfg *config.Config, onChange func(string)) *Watcher {
	return NewWithRegistry(cfg, onChange, prometheus.NewRegistry())
}

// makeTestRepo creates a fully in-memory git repo containing the given files
// and an initial commit, with no remote configured.
func makeTestRepo(t *testing.T, files map[string]string) *git.Repository {
	t.Helper()

	storer := memory.NewStorage()
	fs := memfs.New()

	repo, err := git.Init(storer, fs)
	if err != nil {
		t.Fatalf("git.Init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("repo.Worktree: %v", err)
	}

	for path, content := range files {
		dir := filepath.Dir(path)
		if dir != "." {
			if err := fs.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("MkdirAll(%s): %v", dir, err)
			}
		}
		f, err := fs.Create(path)
		if err != nil {
			t.Fatalf("Create(%s): %v", path, err)
		}
		if _, err := io.WriteString(f, content); err != nil {
			t.Fatalf("WriteString(%s): %v", path, err)
		}
		f.Close()
		if _, err := wt.Add(path); err != nil {
			t.Fatalf("wt.Add(%s): %v", path, err)
		}
	}

	_, err = wt.Commit("test commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("wt.Commit: %v", err)
	}

	return repo
}

// ── normalizeHCLDir ───────────────────────────────────────────────────────────

func TestNormalizeHCLDir(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{".", ""},
		{"/", ""},
		{"jobs", "jobs/"},
		{"/jobs", "jobs/"},
		{"jobs/", "jobs/"},
		{"/jobs/", "jobs/"},
		{"a/b/c", "a/b/c/"},
	}
	for _, tc := range cases {
		if got := normalizeHCLDir(tc.in); got != tc.want {
			t.Errorf("normalizeHCLDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── New ───────────────────────────────────────────────────────────────────────

func TestNew(t *testing.T) {
	cfg := &config.Config{PollInterval: 5 * time.Minute}
	called := false
	onChange := func(string) { called = true }

	w := newTestWatcher(cfg, onChange)

	if w.cfg != cfg {
		t.Error("cfg not stored")
	}
	if w.onChange == nil {
		t.Error("onChange should not be nil")
	}
	if w.triggerCh == nil {
		t.Error("triggerCh should be initialised")
	}
	// Verify the stored onChange is callable without panic.
	w.onChange("test")
	if !called {
		t.Error("onChange was not called")
	}
}

// ── Status / LastCommit ───────────────────────────────────────────────────────

func TestWatcher_Status_ZeroValue(t *testing.T) {
	w := newTestWatcher(&config.Config{}, nil)
	commit, updated := w.Status()
	if commit != "" {
		t.Errorf("want empty commit, got %q", commit)
	}
	if !updated.IsZero() {
		t.Errorf("want zero time, got %v", updated)
	}
}

func TestWatcher_LastCommit(t *testing.T) {
	w := &Watcher{lastCommit: "deadbeef"}
	if got := w.LastCommit(); got != "deadbeef" {
		t.Errorf("want deadbeef, got %q", got)
	}
}

func TestWatcher_Status_AfterSet(t *testing.T) {
	now := time.Now()
	w := &Watcher{lastCommit: "abc123", lastUpdate: now}
	commit, updated := w.Status()
	if commit != "abc123" {
		t.Errorf("want abc123, got %q", commit)
	}
	if !updated.Equal(now) {
		t.Errorf("unexpected update time: %v", updated)
	}
}

// ── Trigger ───────────────────────────────────────────────────────────────────

func TestWatcher_Trigger_NonBlocking(t *testing.T) {
	w := newTestWatcher(&config.Config{PollInterval: time.Minute}, nil)

	// Should never block even when called many times with no reader.
	for i := 0; i < 10; i++ {
		w.Trigger()
	}
}

func TestWatcher_Trigger_SendsSignal(t *testing.T) {
	w := newTestWatcher(&config.Config{PollInterval: time.Minute}, nil)
	w.Trigger()

	select {
	case <-w.triggerCh:
		// Good
	default:
		t.Error("Trigger() did not send to triggerCh")
	}
}

func TestWatcher_TriggerStale_SendsSignal(t *testing.T) {
	w := newTestWatcher(&config.Config{PollInterval: time.Minute}, nil)
	w.TriggerStale()

	select {
	case <-w.triggerCh:
		// Good
	default:
		t.Error("TriggerStale() did not send to triggerCh")
	}
}

func TestWatcher_TriggerStale_NonBlocking(t *testing.T) {
	w := newTestWatcher(&config.Config{PollInterval: time.Minute}, nil)
	// Should not block even when called repeatedly with no reader.
	for i := 0; i < 10; i++ {
		w.TriggerStale()
	}
}

// ── Run ───────────────────────────────────────────────────────────────────────

func TestWatcher_Run_ReturnsOnContextCancel(t *testing.T) {
	w := newTestWatcher(&config.Config{PollInterval: time.Minute}, nil)
	// Inject a repo so pull() has something to work with if the ticker fires
	// (it won't in this test since PollInterval is long, but be safe).
	w.repo = makeTestRepo(t, map[string]string{"dummy.hcl": `job "x" {}`})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Run did not return after context cancellation")
	}
}

// ── buildAuth ─────────────────────────────────────────────────────────────────

func TestBuildAuth_NoAuth(t *testing.T) {
	w := &Watcher{cfg: &config.Config{}}
	auth, err := w.buildAuth()
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if auth != nil {
		t.Errorf("expected nil auth, got %T", auth)
	}
}

func TestBuildAuth_Token(t *testing.T) {
	w := &Watcher{cfg: &config.Config{GitToken: "ghp_mytoken"}}
	auth, err := w.buildAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil auth")
	}
	basic, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("expected *githttp.BasicAuth, got %T", auth)
	}
	if basic.Password != "ghp_mytoken" {
		t.Errorf("expected password ghp_mytoken, got %q", basic.Password)
	}
}

func TestBuildAuth_SSHKey_NotFound(t *testing.T) {
	w := &Watcher{cfg: &config.Config{GitSSHKeyPath: "/nonexistent/path/to/key"}}
	_, err := w.buildAuth()
	if err == nil {
		t.Error("expected error for non-existent SSH key path")
	}
}

// ── setSSHHostKeyCallback ─────────────────────────────────────────────────────

func TestSetSSHHostKeyCallback_ExplicitFile_Good(t *testing.T) {
	f, err := os.CreateTemp("", "known_hosts_*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(f.Name())
	f.Close()

	w := &Watcher{cfg: &config.Config{GitSSHKnownHostsFile: f.Name()}}
	auth := &gitssh.PublicKeys{}
	if err := w.setSSHHostKeyCallback(auth); err != nil {
		t.Fatalf("unexpected error with valid known_hosts file: %v", err)
	}
	if auth.HostKeyCallback == nil {
		t.Error("HostKeyCallback should be set after loading a valid known_hosts file")
	}
}

func TestSetSSHHostKeyCallback_ExplicitFile_Missing(t *testing.T) {
	w := &Watcher{cfg: &config.Config{GitSSHKnownHostsFile: "/nonexistent/known_hosts"}}
	auth := &gitssh.PublicKeys{}
	if err := w.setSSHHostKeyCallback(auth); err == nil {
		t.Error("expected error when --git-ssh-known-hosts points to a missing file")
	}
}

func TestSetSSHHostKeyCallback_NoConfig_DoesNotError(t *testing.T) {
	w := &Watcher{cfg: &config.Config{}}
	auth := &gitssh.PublicKeys{}
	if err := w.setSSHHostKeyCallback(auth); err != nil {
		t.Errorf("setSSHHostKeyCallback should not error when no known_hosts config is set: %v", err)
	}
}

// ── ReadHCLFiles ─────────────────────────────────────────────────────────────

func TestWatcher_ReadHCLFiles_NoRepo(t *testing.T) {
	w := &Watcher{cfg: &config.Config{}}
	_, err := w.ReadHCLFiles()
	if err == nil {
		t.Error("expected error when repo is nil")
	}
}

func TestWatcher_ReadHCLFiles_RepoRoot(t *testing.T) {
	repo := makeTestRepo(t, map[string]string{
		"job1.hcl":  `job "job1" {}`,
		"job2.hcl":  `job "job2" {}`,
		"notes.txt": "not an hcl file",
	})
	w := &Watcher{cfg: &config.Config{HCLDir: ""}, repo: repo}

	files, err := w.ReadHCLFiles()
	if err != nil {
		t.Fatalf("ReadHCLFiles: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("expected 2 HCL files, got %d: %v", len(files), files)
	}
	for _, name := range []string{"job1.hcl", "job2.hcl"} {
		if _, ok := files[name]; !ok {
			t.Errorf("expected file %q in result", name)
		}
	}
}

func TestWatcher_ReadHCLFiles_Subdir(t *testing.T) {
	repo := makeTestRepo(t, map[string]string{
		"jobs/api.hcl":     `job "api" {}`,
		"other/worker.hcl": `job "worker" {}`,
		"root.hcl":         `job "root" {}`,
	})
	w := &Watcher{cfg: &config.Config{HCLDir: "jobs"}, repo: repo}

	files, err := w.ReadHCLFiles()
	if err != nil {
		t.Fatalf("ReadHCLFiles: %v", err)
	}
	if len(files) != 1 {
		t.Errorf("expected 1 HCL file, got %d: %v", len(files), files)
	}
	if _, ok := files["jobs/api.hcl"]; !ok {
		t.Errorf("expected jobs/api.hcl, got: %v", files)
	}
}

func TestWatcher_ReadHCLFiles_NonHCLIgnored(t *testing.T) {
	repo := makeTestRepo(t, map[string]string{
		"job.hcl":    `job "x" {}`,
		"job.json":   `{}`,
		"job.yml":    ``,
		"README.md":  `# docs`,
		"nested/x.hcl": `job "nested" {}`,
	})
	w := &Watcher{cfg: &config.Config{HCLDir: ""}, repo: repo}

	files, err := w.ReadHCLFiles()
	if err != nil {
		t.Fatalf("ReadHCLFiles: %v", err)
	}
	for name := range files {
		if filepath.Ext(name) != ".hcl" {
			t.Errorf("non-.hcl file included: %q", name)
		}
	}
	if len(files) != 2 {
		t.Errorf("expected 2 HCL files (job.hcl + nested/x.hcl), got %d: %v", len(files), files)
	}
}

func TestWatcher_ReadHCLFiles_FileContents(t *testing.T) {
	const content = `job "my-app" { type = "service" }`
	repo := makeTestRepo(t, map[string]string{"my-app.hcl": content})
	w := &Watcher{cfg: &config.Config{HCLDir: ""}, repo: repo}

	files, err := w.ReadHCLFiles()
	if err != nil {
		t.Fatalf("ReadHCLFiles: %v", err)
	}
	if got := files["my-app.hcl"]; got != content {
		t.Errorf("content mismatch:\n got: %q\nwant: %q", got, content)
	}
}

// ── headCommit ────────────────────────────────────────────────────────────────

func TestHeadCommit(t *testing.T) {
	repo := makeTestRepo(t, map[string]string{"job.hcl": `job "x" {}`})
	commit, err := headCommit(repo)
	if err != nil {
		t.Fatalf("headCommit: %v", err)
	}
	if len(commit) != 40 {
		t.Errorf("expected 40-char SHA, got %d chars: %q", len(commit), commit)
	}
}

// ── pull ──────────────────────────────────────────────────────────────────────

// TestWatcher_Pull_NoRemote calls pull() directly on a repo with no remote.
// pull should fail gracefully: increment error counters, attempt re-clone (which
// also fails), log errors, and return without panicking.
func TestWatcher_Pull_NoRemote(t *testing.T) {
	w := newTestWatcher(&config.Config{RepoURL: "", Branch: "main"}, nil)
	w.repo = makeTestRepo(t, map[string]string{"job.hcl": `job "x" {}`})
	w.pull(context.Background()) // must not panic or block
}

func TestWatcher_ReadHCLFiles_EmptyDir(t *testing.T) {
	repo := makeTestRepo(t, map[string]string{
		"jobs/api.hcl": `job "api" {}`,
	})
	// Filter for a directory that exists but has no HCL files.
	w := &Watcher{cfg: &config.Config{HCLDir: "other"}, repo: repo}

	files, err := w.ReadHCLFiles()
	if err != nil {
		t.Fatalf("ReadHCLFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files for empty dir, got %d", len(files))
	}
}

// ── Ready ─────────────────────────────────────────────────────────────────────

func TestWatcher_Ready_BeforeClone(t *testing.T) {
	w := &Watcher{cfg: &config.Config{}}
	if w.Ready() {
		t.Error("Ready() should return false before repo is cloned")
	}
}

func TestWatcher_Ready_AfterClone(t *testing.T) {
	repo := makeTestRepo(t, map[string]string{"job.hcl": `job "x" {}`})
	w := &Watcher{cfg: &config.Config{}, repo: repo}
	if !w.Ready() {
		t.Error("Ready() should return true once repo is set")
	}
}

// ── TriggerStale metric ───────────────────────────────────────────────────────

func TestWatcher_TriggerStale_IncrementsMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	w := NewWithRegistry(&config.Config{PollInterval: time.Minute}, nil, reg)

	w.TriggerStale()

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var count float64
	for _, mf := range mfs {
		if mf.GetName() == "nomad_botherer_git_staleness_refreshes_total" {
			for _, m := range mf.GetMetric() {
				count += m.GetCounter().GetValue()
			}
		}
	}
	if count != 1 {
		t.Errorf("expected stale_refreshes_total=1 after TriggerStale, got %v", count)
	}
}
