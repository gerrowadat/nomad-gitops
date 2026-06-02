package grpcserver_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/gerrowadat/nomad-botherer/internal/grpcapi"
	"github.com/gerrowadat/nomad-botherer/internal/grpcserver"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

const testAPIKey = "test-key-abc123"

// mockDiffSource implements grpcserver.DiffSource.
type mockDiffSource struct {
	diffs         []nomad.JobDiff
	selectedJobs  []nomad.SelectedJob
	lastCheck     time.Time
	lastCommit    string
}

func (m *mockDiffSource) Diffs() ([]nomad.JobDiff, time.Time, string) {
	return m.diffs, m.lastCheck, m.lastCommit
}

func (m *mockDiffSource) SelectedJobs() ([]nomad.SelectedJob, time.Time, string) {
	return m.selectedJobs, m.lastCheck, m.lastCommit
}

func (m *mockDiffSource) Ready() bool { return !m.lastCheck.IsZero() }

// mockGitSource implements grpcserver.GitStatusSource.
type mockGitSource struct {
	lastCommit string
	lastUpdate time.Time
	triggered  bool
}

func (m *mockGitSource) Trigger() {
	m.triggered = true
}

func (m *mockGitSource) Status() (string, time.Time) {
	return m.lastCommit, m.lastUpdate
}

func (m *mockGitSource) Ready() bool { return !m.lastUpdate.IsZero() }

var testBuildInfo = grpcserver.BuildInfo{
	Version:   "v1.2.3",
	Commit:    "abc123",
	BuildDate: "2026-05-08T00:00:00Z",
}

// startTestServer starts a real gRPC listener on a random port and returns
// a connected client and a cancel function that stops the server.
func startTestServer(t *testing.T, diffSrc grpcserver.DiffSource, gitSrc grpcserver.GitStatusSource) (grpcapi.NomadBothererClient, func()) {
	t.Helper()

	reg := prometheus.NewRegistry()
	srv := grpcserver.NewWithRegistry(testAPIKey, diffSrc, gitSrc, testBuildInfo, reg)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	grpcSrv := srv.GRPCServer()
	go func() { _ = grpcSrv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	return grpcapi.NewNomadBothererClient(conn), func() {
		conn.Close()
		grpcSrv.GracefulStop()
	}
}

func authCtx(key string) context.Context {
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+key))
}

// --- Auth tests ---

func TestAuth_MissingKey(t *testing.T) {
	diffSrc := &mockDiffSource{}
	gitSrc := &mockGitSource{}
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	_, err := client.GetDiffs(context.Background(), &grpcapi.GetDiffsRequest{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %s", s.Code())
	}
}

func TestAuth_WrongKey(t *testing.T) {
	diffSrc := &mockDiffSource{}
	gitSrc := &mockGitSource{}
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	_, err := client.GetDiffs(authCtx("wrong-key"), &grpcapi.GetDiffsRequest{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %s", s.Code())
	}
}

// --- GetDiffs tests ---

func TestGetDiffs_Empty(t *testing.T) {
	diffSrc := &mockDiffSource{
		lastCheck:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		lastCommit: "abc123",
	}
	gitSrc := &mockGitSource{lastUpdate: time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)}
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	resp, err := client.GetDiffs(authCtx(testAPIKey), &grpcapi.GetDiffsRequest{})
	if err != nil {
		t.Fatalf("GetDiffs: %v", err)
	}
	if len(resp.Diffs) != 0 {
		t.Fatalf("want 0 diffs, got %d", len(resp.Diffs))
	}
	if resp.LastCommit != "abc123" {
		t.Fatalf("want last_commit abc123, got %q", resp.LastCommit)
	}
	if resp.LastCheckTime != "2026-01-01T12:00:00Z" {
		t.Fatalf("unexpected last_check_time %q", resp.LastCheckTime)
	}
}

func TestGetDiffs_WithDiffs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	diffSrc := &mockDiffSource{
		diffs: []nomad.JobDiff{
			{
				JobID:    "my-job",
				HCLFile:  "jobs/my-job.hcl",
				DiffType: nomad.DiffTypeModified,
				Detail:   "+/- count: 1 => 2",
			},
			{
				JobID:    "missing-job",
				DiffType: nomad.DiffTypeMissingFromHCL,
			},
		},
		lastCheck:  now,
		lastCommit: "def456",
	}
	gitSrc := &mockGitSource{lastUpdate: now}
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	resp, err := client.GetDiffs(authCtx(testAPIKey), &grpcapi.GetDiffsRequest{})
	if err != nil {
		t.Fatalf("GetDiffs: %v", err)
	}
	if len(resp.Diffs) != 2 {
		t.Fatalf("want 2 diffs, got %d", len(resp.Diffs))
	}

	d0 := resp.Diffs[0]
	if d0.JobId != "my-job" {
		t.Errorf("want job_id my-job, got %q", d0.JobId)
	}
	if d0.HclFile != "jobs/my-job.hcl" {
		t.Errorf("want hcl_file jobs/my-job.hcl, got %q", d0.HclFile)
	}
	if d0.DiffType != "modified" {
		t.Errorf("want diff_type modified, got %q", d0.DiffType)
	}
	if d0.Detail != "+/- count: 1 => 2" {
		t.Errorf("unexpected detail %q", d0.Detail)
	}

	d1 := resp.Diffs[1]
	if d1.JobId != "missing-job" {
		t.Errorf("want job_id missing-job, got %q", d1.JobId)
	}
	if d1.DiffType != "missing_from_hcl" {
		t.Errorf("want diff_type missing_from_hcl, got %q", d1.DiffType)
	}
}

func TestGetDiffs_NotReady(t *testing.T) {
	diffSrc := &mockDiffSource{} // zero lastCheck → not ready
	gitSrc := &mockGitSource{}   // zero lastUpdate → not ready
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	_, err := client.GetDiffs(authCtx(testAPIKey), &grpcapi.GetDiffsRequest{})
	if err == nil {
		t.Fatal("expected error when server not ready, got nil")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unavailable {
		t.Fatalf("want Unavailable, got %s", s.Code())
	}
}

func TestGetDiffs_GitNotReady(t *testing.T) {
	diffSrc := &mockDiffSource{lastCheck: time.Now(), lastCommit: "abc"} // diffs ready
	gitSrc := &mockGitSource{}                                            // git not ready
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	_, err := client.GetDiffs(authCtx(testAPIKey), &grpcapi.GetDiffsRequest{})
	if err == nil {
		t.Fatal("expected error when git not ready, got nil")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unavailable {
		t.Fatalf("want Unavailable, got %s", s.Code())
	}
}

// --- GetStatus tests ---

func TestGetStatus(t *testing.T) {
	now := time.Date(2026, 3, 15, 9, 30, 0, 0, time.UTC)
	diffSrc := &mockDiffSource{}
	gitSrc := &mockGitSource{
		lastCommit: "feedbeef",
		lastUpdate: now,
	}
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	resp, err := client.GetStatus(authCtx(testAPIKey), &grpcapi.GetStatusRequest{})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if resp.LastCommit != "feedbeef" {
		t.Fatalf("want last_commit feedbeef, got %q", resp.LastCommit)
	}
	if resp.LastUpdateTime != "2026-03-15T09:30:00Z" {
		t.Fatalf("unexpected last_update_time %q", resp.LastUpdateTime)
	}
}

func TestGetStatus_GitNotReady(t *testing.T) {
	diffSrc := &mockDiffSource{}
	gitSrc := &mockGitSource{} // zero lastUpdate → git not ready
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	_, err := client.GetStatus(authCtx(testAPIKey), &grpcapi.GetStatusRequest{})
	if err == nil {
		t.Fatal("expected error when git not ready, got nil")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unavailable {
		t.Fatalf("want Unavailable, got %s", s.Code())
	}
}

// --- TriggerRefresh tests ---

func TestTriggerRefresh(t *testing.T) {
	diffSrc := &mockDiffSource{}
	gitSrc := &mockGitSource{}
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	resp, err := client.TriggerRefresh(authCtx(testAPIKey), &grpcapi.TriggerRefreshRequest{})
	if err != nil {
		t.Fatalf("TriggerRefresh: %v", err)
	}
	if resp.Message != "refresh triggered" {
		t.Fatalf("unexpected message %q", resp.Message)
	}
	if !gitSrc.triggered {
		t.Fatal("expected git.Trigger() to have been called")
	}
}

// --- GetVersion tests ---

func TestGetVersion(t *testing.T) {
	diffSrc := &mockDiffSource{}
	gitSrc := &mockGitSource{}
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	resp, err := client.GetVersion(authCtx(testAPIKey), &grpcapi.GetVersionRequest{})
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if resp.Version != testBuildInfo.Version {
		t.Errorf("want version %q, got %q", testBuildInfo.Version, resp.Version)
	}
	if resp.Commit != testBuildInfo.Commit {
		t.Errorf("want commit %q, got %q", testBuildInfo.Commit, resp.Commit)
	}
	if resp.BuildDate != testBuildInfo.BuildDate {
		t.Errorf("want build_date %q, got %q", testBuildInfo.BuildDate, resp.BuildDate)
	}
}

func TestGetVersion_DevBuild(t *testing.T) {
	diffSrc := &mockDiffSource{}
	gitSrc := &mockGitSource{}
	reg := prometheus.NewRegistry()
	srv := grpcserver.NewWithRegistry(testAPIKey, diffSrc, gitSrc, grpcserver.BuildInfo{
		Version:   "dev",
		Commit:    "unknown",
		BuildDate: "unknown",
	}, reg)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := srv.GRPCServer()
	go func() { _ = grpcSrv.Serve(lis) }()
	defer grpcSrv.GracefulStop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := grpcapi.NewNomadBothererClient(conn)

	resp, err := client.GetVersion(authCtx(testAPIKey), &grpcapi.GetVersionRequest{})
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if resp.Version != "dev" {
		t.Errorf("want version dev, got %q", resp.Version)
	}
	if resp.Commit != "unknown" {
		t.Errorf("want commit unknown, got %q", resp.Commit)
	}
}

// --- Metrics tests ---

func TestMetrics_AuthErrorCounted(t *testing.T) {
	diffSrc := &mockDiffSource{}
	gitSrc := &mockGitSource{}
	reg := prometheus.NewRegistry()
	srv := grpcserver.NewWithRegistry(testAPIKey, diffSrc, gitSrc, testBuildInfo, reg)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := srv.GRPCServer()
	go func() { _ = grpcSrv.Serve(lis) }()
	defer grpcSrv.GracefulStop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := grpcapi.NewNomadBothererClient(conn)

	// Two requests with bad auth.
	for range 2 {
		_, _ = client.GetDiffs(authCtx("bad"), &grpcapi.GetDiffsRequest{})
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var authErrors float64
	for _, mf := range mfs {
		if mf.GetName() == "nomad_botherer_grpc_auth_errors_total" {
			for _, m := range mf.GetMetric() {
				authErrors += m.GetCounter().GetValue()
			}
		}
	}
	if authErrors != 2 {
		t.Fatalf("want 2 auth errors, got %v", authErrors)
	}
}

// TestListen_BindsSuccessfully verifies that Listen() returns a usable
// net.Listener bound to a free port when given address ":0".
func TestListen_BindsSuccessfully(t *testing.T) {
	diffSrc := &mockDiffSource{}
	gitSrc := &mockGitSource{}
	reg := prometheus.NewRegistry()
	srv := grpcserver.NewWithRegistry(testAPIKey, diffSrc, gitSrc, testBuildInfo, reg)

	lis, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}
	defer lis.Close()

	if lis.Addr() == nil {
		t.Error("expected non-nil listener address")
	}
}

// TestServe_ExitsOnContextCancel verifies that Serve() stops accepting new
// connections and returns nil once its context is cancelled.
func TestServe_ExitsOnContextCancel(t *testing.T) {
	diffSrc := &mockDiffSource{}
	gitSrc := &mockGitSource{}
	reg := prometheus.NewRegistry()
	srv := grpcserver.NewWithRegistry(testAPIKey, diffSrc, gitSrc, testBuildInfo, reg)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ctx, lis)
	}()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Serve did not return after context cancellation")
	}
}

func TestMetrics_SuccessfulRequestCounted(t *testing.T) {
	now := time.Now()
	diffSrc := &mockDiffSource{lastCheck: now, lastCommit: "abc"}
	gitSrc := &mockGitSource{lastUpdate: now}
	reg := prometheus.NewRegistry()
	srv := grpcserver.NewWithRegistry(testAPIKey, diffSrc, gitSrc, testBuildInfo, reg)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := srv.GRPCServer()
	go func() { _ = grpcSrv.Serve(lis) }()
	defer grpcSrv.GracefulStop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := grpcapi.NewNomadBothererClient(conn)

	_, err = client.GetDiffs(authCtx(testAPIKey), &grpcapi.GetDiffsRequest{})
	if err != nil {
		t.Fatalf("GetDiffs: %v", err)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() == "nomad_botherer_grpc_requests_total" {
			for _, m := range mf.GetMetric() {
				total += m.GetCounter().GetValue()
			}
		}
	}
	if total != 1 {
		t.Fatalf("want 1 request total, got %v", total)
	}
}

func TestGetSelectedJobs_Empty(t *testing.T) {
	diffSrc := &mockDiffSource{
		lastCheck:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		lastCommit: "abc123",
	}
	gitSrc := &mockGitSource{lastCommit: "abc123", lastUpdate: time.Now()}
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	resp, err := client.GetSelectedJobs(authCtx(testAPIKey), &grpcapi.GetSelectedJobsRequest{})
	if err != nil {
		t.Fatalf("GetSelectedJobs: %v", err)
	}
	if len(resp.Jobs) != 0 {
		t.Errorf("want 0 jobs, got %d", len(resp.Jobs))
	}
	if resp.LastCheckTime == "" {
		t.Errorf("want non-empty last_check_time")
	}
	if resp.LastCommit != "abc123" {
		t.Errorf("want last_commit abc123, got %q", resp.LastCommit)
	}
}

func TestGetSelectedJobs_WithJobs(t *testing.T) {
	diffSrc := &mockDiffSource{
		lastCheck:  time.Now(),
		lastCommit: "def456",
		selectedJobs: []nomad.SelectedJob{
			{JobID: "api", Reason: nomad.SelectionReasonMeta},
			{JobID: "worker", Reason: nomad.SelectionReasonGlob},
			{JobID: "both-job", Reason: nomad.SelectionReasonBoth},
		},
	}
	gitSrc := &mockGitSource{lastCommit: "def456", lastUpdate: time.Now()}
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	resp, err := client.GetSelectedJobs(authCtx(testAPIKey), &grpcapi.GetSelectedJobsRequest{})
	if err != nil {
		t.Fatalf("GetSelectedJobs: %v", err)
	}
	if len(resp.Jobs) != 3 {
		t.Fatalf("want 3 jobs, got %d: %+v", len(resp.Jobs), resp.Jobs)
	}
	byID := make(map[string]string)
	for _, j := range resp.Jobs {
		byID[j.JobId] = j.SelectionReason
	}
	if byID["api"] != "meta" {
		t.Errorf("api: want reason meta, got %q", byID["api"])
	}
	if byID["worker"] != "glob" {
		t.Errorf("worker: want reason glob, got %q", byID["worker"])
	}
	if byID["both-job"] != "both" {
		t.Errorf("both-job: want reason both, got %q", byID["both-job"])
	}
}

func TestGetSelectedJobs_NotReady(t *testing.T) {
	diffSrc := &mockDiffSource{} // zero lastCheck → not ready
	gitSrc := &mockGitSource{lastCommit: "abc", lastUpdate: time.Now()}
	client, stop := startTestServer(t, diffSrc, gitSrc)
	defer stop()

	_, err := client.GetSelectedJobs(authCtx(testAPIKey), &grpcapi.GetSelectedJobsRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("want Unavailable, got %v", err)
	}
}
