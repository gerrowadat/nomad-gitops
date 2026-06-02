package main

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gerrowadat/nomad-botherer/internal/grpcserver"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

const testKey = "test-secret"

// mockDiffSource implements grpcserver.DiffSource for tests.
type mockDiffSource struct {
	diffs        []nomad.JobDiff
	selectedJobs []nomad.SelectedJob
	lastCheck    time.Time
	lastCommit   string
}

func (m *mockDiffSource) Diffs() ([]nomad.JobDiff, time.Time, string) {
	return m.diffs, m.lastCheck, m.lastCommit
}

func (m *mockDiffSource) SelectedJobs() ([]nomad.SelectedJob, time.Time, string) {
	return m.selectedJobs, m.lastCheck, m.lastCommit
}

func (m *mockDiffSource) Ready() bool { return !m.lastCheck.IsZero() }

// mockGitSource implements grpcserver.GitStatusSource for tests.
type mockGitSource struct {
	lastCommit string
	lastUpdate time.Time
}

func (m *mockGitSource) Trigger()                    {}
func (m *mockGitSource) Status() (string, time.Time) { return m.lastCommit, m.lastUpdate }
func (m *mockGitSource) Ready() bool                 { return !m.lastUpdate.IsZero() }

// startServer starts a throwaway gRPC server and returns its address and a stop function.
func startServer(t *testing.T, diffs grpcserver.DiffSource, git grpcserver.GitStatusSource, info grpcserver.BuildInfo) (addr string, stop func()) {
	t.Helper()
	reg := prometheus.NewRegistry()
	srv := grpcserver.NewWithRegistry(testKey, diffs, git, info, reg)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := srv.GRPCServer()
	go func() { _ = grpcSrv.Serve(lis) }()
	return lis.Addr().String(), grpcSrv.GracefulStop
}

// runCmd builds a fresh root command, wires the given args, and returns
// captured stdout plus any execution error.
func runCmd(t *testing.T, addr, key string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(io.Discard)
	all := append([]string{"--server", addr, "--api-key", key}, args...)
	root.SetArgs(all)
	err := root.Execute()
	return out.String(), err
}

// defaultInfo is the build info injected into every test server.
var defaultInfo = grpcserver.BuildInfo{
	Version:   "v9.9.9",
	Commit:    "deadbeef",
	BuildDate: "2026-01-01T00:00:00Z",
}

// ── diffs ────────────────────────────────────────────────────────────────────

func TestDiffsCmd_NoDiffs(t *testing.T) {
	now := time.Now()
	addr, stop := startServer(t, &mockDiffSource{lastCheck: now}, &mockGitSource{lastUpdate: now}, defaultInfo)
	defer stop()

	out, err := runCmd(t, addr, testKey, "diffs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "no diffs detected") {
		t.Errorf("want 'no diffs detected', got: %q", out)
	}
}

func TestDiffsCmd_WithDiffs(t *testing.T) {
	src := &mockDiffSource{
		diffs: []nomad.JobDiff{
			{
				JobID:    "api-server",
				HCLFile:  "jobs/api-server.hcl",
				DiffType: nomad.DiffTypeModified,
				Detail:   "+/- count: 1 => 2",
			},
			{
				JobID:    "ghost-worker",
				DiffType: nomad.DiffTypeMissingFromHCL,
			},
		},
		lastCheck:  time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC),
		lastCommit: "abc123",
	}
	git := &mockGitSource{lastUpdate: time.Date(2026, 5, 8, 11, 0, 0, 0, time.UTC)}
	addr, stop := startServer(t, src, git, defaultInfo)
	defer stop()

	out, err := runCmd(t, addr, testKey, "diffs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "2 diff(s)") {
		t.Errorf("want diff count, got: %q", out)
	}
	if !strings.Contains(out, "[modified] api-server (jobs/api-server.hcl)") {
		t.Errorf("want modified diff line, got: %q", out)
	}
	if !strings.Contains(out, "[missing_from_hcl] ghost-worker") {
		t.Errorf("want missing_from_hcl line, got: %q", out)
	}
	if !strings.Contains(out, "+/- count: 1 => 2") {
		t.Errorf("want detail, got: %q", out)
	}
}

func TestDiffsCmd_JSON(t *testing.T) {
	now := time.Now()
	src := &mockDiffSource{lastCheck: now, lastCommit: "abc123"}
	addr, stop := startServer(t, src, &mockGitSource{lastUpdate: now}, defaultInfo)
	defer stop()

	out, err := runCmd(t, addr, testKey, "diffs", "--output", "json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `"last_commit"`) {
		t.Errorf("want json field last_commit, got: %q", out)
	}
}

// ── status ───────────────────────────────────────────────────────────────────

func TestSelectedJobsCmd_NoJobs(t *testing.T) {
	now := time.Now()
	addr, stop := startServer(t, &mockDiffSource{lastCheck: now}, &mockGitSource{lastUpdate: now}, defaultInfo)
	defer stop()

	out, err := runCmd(t, addr, testKey, "selected-jobs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "no jobs currently selected") {
		t.Errorf("want 'no jobs currently selected', got: %q", out)
	}
}

func TestSelectedJobsCmd_WithJobs(t *testing.T) {
	src := &mockDiffSource{
		selectedJobs: []nomad.SelectedJob{
			{JobID: "api", Reason: nomad.SelectionReasonMeta},
			{JobID: "worker", Reason: nomad.SelectionReasonGlob},
		},
		lastCheck:  time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC),
		lastCommit: "abc123",
	}
	git := &mockGitSource{lastUpdate: time.Now()}
	addr, stop := startServer(t, src, git, defaultInfo)
	defer stop()

	out, err := runCmd(t, addr, testKey, "selected-jobs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "2 job(s) selected") {
		t.Errorf("want job count, got: %q", out)
	}
	if !strings.Contains(out, "api") || !strings.Contains(out, "meta") {
		t.Errorf("want api/meta line, got: %q", out)
	}
	if !strings.Contains(out, "worker") || !strings.Contains(out, "glob") {
		t.Errorf("want worker/glob line, got: %q", out)
	}
}

func TestStatusCmd(t *testing.T) {
	git := &mockGitSource{
		lastCommit: "feedbeef",
		lastUpdate: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
	}
	addr, stop := startServer(t, &mockDiffSource{}, git, defaultInfo)
	defer stop()

	out, err := runCmd(t, addr, testKey, "status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "feedbeef") {
		t.Errorf("want commit hash, got: %q", out)
	}
	if !strings.Contains(out, "2026-03-01T09:00:00Z") {
		t.Errorf("want update time, got: %q", out)
	}
}

func TestStatusCmd_NotReady(t *testing.T) {
	addr, stop := startServer(t, &mockDiffSource{}, &mockGitSource{}, defaultInfo)
	defer stop()

	_, err := runCmd(t, addr, testKey, "status")
	if err == nil {
		t.Fatal("expected error when server not ready, got nil")
	}
}

// ── refresh ──────────────────────────────────────────────────────────────────

func TestRefreshCmd(t *testing.T) {
	addr, stop := startServer(t, &mockDiffSource{}, &mockGitSource{}, defaultInfo)
	defer stop()

	out, err := runCmd(t, addr, testKey, "refresh")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "refresh triggered") {
		t.Errorf("want 'refresh triggered', got: %q", out)
	}
}

// ── version ──────────────────────────────────────────────────────────────────

func TestVersionCmd(t *testing.T) {
	addr, stop := startServer(t, &mockDiffSource{}, &mockGitSource{}, defaultInfo)
	defer stop()

	out, err := runCmd(t, addr, testKey, "version")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "v9.9.9") {
		t.Errorf("want version v9.9.9, got: %q", out)
	}
	if !strings.Contains(out, "deadbeef") {
		t.Errorf("want commit deadbeef, got: %q", out)
	}
	if !strings.Contains(out, "2026-01-01T00:00:00Z") {
		t.Errorf("want build date, got: %q", out)
	}
}

func TestVersionCmd_JSON(t *testing.T) {
	addr, stop := startServer(t, &mockDiffSource{}, &mockGitSource{}, defaultInfo)
	defer stop()

	out, err := runCmd(t, addr, testKey, "version", "--output", "json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `"version"`) || !strings.Contains(out, `"commit"`) {
		t.Errorf("want json fields, got: %q", out)
	}
}

// ── auth errors ──────────────────────────────────────────────────────────────

func TestMissingAPIKey(t *testing.T) {
	addr, stop := startServer(t, &mockDiffSource{}, &mockGitSource{}, defaultInfo)
	defer stop()

	// no --api-key and no NBCTL_API_KEY env var
	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--server", addr, "diffs"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
}

func TestWrongAPIKey(t *testing.T) {
	addr, stop := startServer(t, &mockDiffSource{}, &mockGitSource{}, defaultInfo)
	defer stop()

	_, err := runCmd(t, addr, "wrong-key", "diffs")
	if err == nil {
		t.Fatal("expected error for wrong API key")
	}
}

// ── env var fallback ──────────────────────────────────────────────────────────

func TestAPIKeyFromEnv(t *testing.T) {
	now := time.Now()
	addr, stop := startServer(t, &mockDiffSource{lastCheck: now}, &mockGitSource{lastUpdate: now}, defaultInfo)
	defer stop()

	t.Setenv("NBCTL_API_KEY", testKey)

	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--server", addr, "diffs"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServerFromEnv(t *testing.T) {
	now := time.Now()
	addr, stop := startServer(t, &mockDiffSource{lastCheck: now}, &mockGitSource{lastUpdate: now}, defaultInfo)
	defer stop()

	t.Setenv("NBCTL_SERVER", addr)

	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--api-key", testKey, "diffs"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

