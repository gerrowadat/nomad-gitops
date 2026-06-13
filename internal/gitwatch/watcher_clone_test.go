package gitwatch

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	xssh "golang.org/x/crypto/ssh"

	"github.com/gerrowadat/nomad-botherer/internal/config"
)

// makeDiskRepo creates a real git repository on disk with the given files
// committed on branch "main". The watcher can clone it via the local path,
// exercising the same Clone/Pull code paths as a remote URL.
func makeDiskRepo(t *testing.T, files map[string]string) (dir string, repo *git.Repository) {
	t.Helper()
	dir = t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	commitFiles(t, dir, repo, files, "initial commit")
	return dir, repo
}

// commitFiles writes files into the repo worktree and commits them.
func commitFiles(t *testing.T, dir string, repo *git.Repository, files map[string]string, msg string) {
	t.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("git add %s: %v", name, err)
		}
	}
	_, err = wt.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.invalid", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestClone_LocalRepo(t *testing.T) {
	dir, _ := makeDiskRepo(t, map[string]string{"app.hcl": `job "app" {}`})
	w := newTestWatcher(&config.Config{RepoURL: dir, Branch: "main"}, nil)

	if err := w.Clone(context.Background()); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if !w.Ready() {
		t.Error("Ready should be true after Clone")
	}
	commit, updated := w.Status()
	if commit == "" {
		t.Error("Status should report a commit after Clone")
	}
	if updated.IsZero() {
		t.Error("Status should report an update time after Clone")
	}

	files, err := w.ReadHCLFiles()
	if err != nil {
		t.Fatalf("ReadHCLFiles: %v", err)
	}
	if files["app.hcl"] != `job "app" {}` {
		t.Errorf("unexpected files after clone: %v", files)
	}
}

func TestClone_BadURL(t *testing.T) {
	w := newTestWatcher(&config.Config{RepoURL: filepath.Join(t.TempDir(), "nonexistent"), Branch: "main"}, nil)
	if err := w.Clone(context.Background()); err == nil {
		t.Error("Clone of a nonexistent path should fail")
	}
	if w.Ready() {
		t.Error("Ready should be false after failed Clone")
	}
}

func TestClone_BadBranch(t *testing.T) {
	dir, _ := makeDiskRepo(t, map[string]string{"app.hcl": `job "app" {}`})
	w := newTestWatcher(&config.Config{RepoURL: dir, Branch: "no-such-branch"}, nil)
	if err := w.Clone(context.Background()); err == nil {
		t.Error("Clone of a nonexistent branch should fail")
	}
}

func TestClone_AuthBuildError(t *testing.T) {
	dir, _ := makeDiskRepo(t, map[string]string{"app.hcl": `job "app" {}`})
	w := newTestWatcher(&config.Config{
		RepoURL:       dir,
		Branch:        "main",
		GitSSHKeyPath: filepath.Join(t.TempDir(), "no-such-key"),
	}, nil)
	if err := w.Clone(context.Background()); err == nil {
		t.Error("Clone with an unreadable SSH key should fail")
	}
}

func TestPull_NewCommitFiresOnChange(t *testing.T) {
	dir, repo := makeDiskRepo(t, map[string]string{"app.hcl": `job "app" {}`})

	changed := make(chan string, 1)
	w := newTestWatcher(&config.Config{RepoURL: dir, Branch: "main"}, func(commit string) {
		changed <- commit
	})
	if err := w.Clone(context.Background()); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	first := w.LastCommit()

	commitFiles(t, dir, repo, map[string]string{"app.hcl": `job "app" { type = "service" }`}, "update app")

	w.pull(context.Background())

	select {
	case commit := <-changed:
		if commit == first {
			t.Error("onChange fired with the old commit")
		}
		if commit != w.LastCommit() {
			t.Errorf("onChange commit %q != LastCommit %q", commit, w.LastCommit())
		}
	default:
		t.Fatal("onChange did not fire after pull of a new commit")
	}

	files, err := w.ReadHCLFiles()
	if err != nil {
		t.Fatalf("ReadHCLFiles: %v", err)
	}
	if files["app.hcl"] != `job "app" { type = "service" }` {
		t.Errorf("pull did not update file contents: %q", files["app.hcl"])
	}
}

func TestPull_NoChange_NoCallback(t *testing.T) {
	dir, _ := makeDiskRepo(t, map[string]string{"app.hcl": `job "app" {}`})

	changed := make(chan string, 1)
	w := newTestWatcher(&config.Config{RepoURL: dir, Branch: "main"}, func(commit string) {
		changed <- commit
	})
	if err := w.Clone(context.Background()); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	w.pull(context.Background())

	select {
	case c := <-changed:
		t.Errorf("onChange fired with %q despite no new commit", c)
	default:
	}
	if _, updated := w.Status(); updated.IsZero() {
		t.Error("lastUpdate should still be set after a no-op pull")
	}
}

func TestPull_AuthBuildError_ReturnsEarly(t *testing.T) {
	dir, repo := makeDiskRepo(t, map[string]string{"app.hcl": `job "app" {}`})

	w := newTestWatcher(&config.Config{RepoURL: dir, Branch: "main"}, nil)
	if err := w.Clone(context.Background()); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	first := w.LastCommit()

	commitFiles(t, dir, repo, map[string]string{"new.hcl": `job "new" {}`}, "another commit")

	// Break auth config after the clone; pull must bail out before fetching.
	w.cfg.GitSSHKeyPath = filepath.Join(t.TempDir(), "no-such-key")
	w.pull(context.Background())

	if w.LastCommit() != first {
		t.Error("pull should not have advanced the commit when auth building fails")
	}
}

func TestPull_RemoteGone_RecloneFails(t *testing.T) {
	dir, _ := makeDiskRepo(t, map[string]string{"app.hcl": `job "app" {}`})

	w := newTestWatcher(&config.Config{RepoURL: dir, Branch: "main"}, nil)
	if err := w.Clone(context.Background()); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	first := w.LastCommit()

	// Delete the source repo: pull fails, and the fallback re-clone fails too.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("removing source repo: %v", err)
	}
	w.pull(context.Background())

	if w.LastCommit() != first {
		t.Error("commit should be unchanged after failed pull and re-clone")
	}
	if !w.Ready() {
		t.Error("watcher should keep serving the last good clone")
	}
}

func TestRun_TriggerCausesPull(t *testing.T) {
	dir, repo := makeDiskRepo(t, map[string]string{"app.hcl": `job "app" {}`})

	changed := make(chan string, 1)
	w := newTestWatcher(&config.Config{RepoURL: dir, Branch: "main", PollInterval: time.Hour}, func(commit string) {
		changed <- commit
	})
	if err := w.Clone(context.Background()); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	commitFiles(t, dir, repo, map[string]string{"app.hcl": `job "app" { type = "batch" }`}, "update")
	w.Trigger()

	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("onChange did not fire after Trigger")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestRun_PollIntervalCausesPull(t *testing.T) {
	dir, repo := makeDiskRepo(t, map[string]string{"app.hcl": `job "app" {}`})

	changed := make(chan string, 1)
	w := newTestWatcher(&config.Config{RepoURL: dir, Branch: "main", PollInterval: 20 * time.Millisecond}, func(commit string) {
		changed <- commit
	})
	if err := w.Clone(context.Background()); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	commitFiles(t, dir, repo, map[string]string{"app.hcl": `job "app" { type = "batch" }`}, "update")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("onChange did not fire from the poll ticker")
	}
}

func TestBuildAuth_SSHKey_Valid(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	block, err := xssh.MarshalPrivateKey(priv, "test key")
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}

	w := newTestWatcher(&config.Config{RepoURL: "git@example.com:x/y.git", Branch: "main", GitSSHKeyPath: keyPath}, nil)
	auth, err := w.buildAuth()
	if err != nil {
		t.Fatalf("buildAuth with valid SSH key: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil auth for SSH key config")
	}
	if _, ok := auth.(*githttp.BasicAuth); ok {
		t.Error("SSH key config should not produce HTTP basic auth")
	}
}

// TestNew_DefaultRegistry exercises the production constructor, which
// registers metrics into the default Prometheus registry. Called once per
// test binary to avoid duplicate-registration panics.
func TestNew_DefaultRegistry(t *testing.T) {
	w := New(&config.Config{PollInterval: time.Minute}, nil)
	if w == nil {
		t.Fatal("New returned nil")
	}
	if w.Ready() {
		t.Error("a fresh watcher should not be Ready")
	}
}

func TestReadHCLFiles_EmptyRepo_HeadError(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	w := newTestWatcher(&config.Config{}, nil)
	w.repo = repo // no commits: repo.Head() fails

	if _, err := w.ReadHCLFiles(); err == nil {
		t.Error("ReadHCLFiles on a repo with no commits should fail")
	}
}

func TestPull_BareRepo_WorktreeError(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, true) // bare: no worktree
	if err != nil {
		t.Fatalf("init bare: %v", err)
	}

	w := newTestWatcher(&config.Config{RepoURL: dir, Branch: "main"}, nil)
	w.repo = repo
	w.lastCommit = "before"

	w.pull(context.Background())

	if w.lastCommit != "before" {
		t.Error("pull on a bare repo should leave state unchanged")
	}
}

func TestHeadCommit_EmptyRepo(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := headCommit(repo); err == nil {
		t.Error("headCommit on a repo with no commits should fail")
	}
}

func TestBuildAuth_SSHKey_BadKnownHosts(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	block, err := xssh.MarshalPrivateKey(priv, "test key")
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}

	w := newTestWatcher(&config.Config{
		RepoURL:              "git@example.com:x/y.git",
		Branch:               "main",
		GitSSHKeyPath:        keyPath,
		GitSSHKnownHostsFile: filepath.Join(t.TempDir(), "no-such-known-hosts"),
	}, nil)
	if _, err := w.buildAuth(); err == nil {
		t.Error("valid key with an unreadable known_hosts file should fail")
	}
}

func TestFileAtParentOf(t *testing.T) {
	dir, repo := makeDiskRepo(t, map[string]string{"app.hcl": `job "app" { v = 1 }`})
	commitFiles(t, dir, repo, map[string]string{"app.hcl": `job "app" { v = 2 }`}, "bump")

	w := newTestWatcher(&config.Config{RepoURL: dir, Branch: "main"}, nil)
	if err := w.Clone(context.Background()); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	// HEAD (v=2) has a parent holding v=1.
	head := w.LastCommit()
	content, ok := w.FileAtParentOf(head, "app.hcl")
	if !ok {
		t.Fatal("FileAtParentOf should find app.hcl at the parent commit")
	}
	if content != `job "app" { v = 1 }` {
		t.Errorf("unexpected parent content: %q", content)
	}

	// An advancing HEAD must not change the answer for an earlier commit: the
	// lookup is keyed off the named commit, not current HEAD.
	commitFiles(t, dir, repo, map[string]string{"app.hcl": `job "app" { v = 3 }`}, "bump again")
	w2 := newTestWatcher(&config.Config{RepoURL: dir, Branch: "main"}, nil)
	if err := w2.Clone(context.Background()); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	content, ok = w2.FileAtParentOf(head, "app.hcl") // same earlier commit
	if !ok || content != `job "app" { v = 1 }` {
		t.Errorf("parent of an earlier commit should be stable across HEAD advances, got %q ok=%v", content, ok)
	}

	// Unknown commit hash.
	if _, ok := w2.FileAtParentOf("0000000000000000000000000000000000000000", "app.hcl"); ok {
		t.Error("FileAtParentOf should report ok=false for an unknown commit")
	}
}

func TestFileAtParentOf_FileNew(t *testing.T) {
	dir, repo := makeDiskRepo(t, map[string]string{"app.hcl": `job "app" {}`})
	commitFiles(t, dir, repo, map[string]string{"new.hcl": `job "new" {}`}, "add new")

	w := newTestWatcher(&config.Config{RepoURL: dir, Branch: "main"}, nil)
	if err := w.Clone(context.Background()); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	// new.hcl was created at HEAD; it did not exist at the parent.
	if _, ok := w.FileAtParentOf(w.LastCommit(), "new.hcl"); ok {
		t.Error("FileAtParentOf should report ok=false for a file new at the commit")
	}
}

func TestFileAtParentOf_RootCommit(t *testing.T) {
	dir, _ := makeDiskRepo(t, map[string]string{"app.hcl": `job "app" {}`})
	w := newTestWatcher(&config.Config{RepoURL: dir, Branch: "main"}, nil)
	if err := w.Clone(context.Background()); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	// The single commit is the root: no parent.
	if _, ok := w.FileAtParentOf(w.LastCommit(), "app.hcl"); ok {
		t.Error("FileAtParentOf should report ok=false at the root commit")
	}
}

func TestFileAtParentOf_NoRepo(t *testing.T) {
	w := newTestWatcher(&config.Config{}, nil)
	if _, ok := w.FileAtParentOf("deadbeef", "app.hcl"); ok {
		t.Error("FileAtParentOf should report ok=false before clone")
	}
}
