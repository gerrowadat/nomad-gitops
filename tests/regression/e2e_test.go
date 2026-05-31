//go:build regression

package regression

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/gerrowadat/nomad-botherer/internal/grpcapi"
	"github.com/gerrowadat/nomad-botherer/internal/server"
)

// TestE2E_StartupLifecycle verifies that:
//   - /healthz returns 200 once the initial git clone and diff check finish
//     (startBotherer blocks until this transition occurs)
//   - the response body is valid JSON with the expected fields
func TestE2E_StartupLifecycle(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)

	// Put a valid job HCL in the repo.
	jobID := uniqueJobID("lifecycle")
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCL(jobID),
	})

	// Start the binary. startBotherer waits for /healthz → 200.
	baseURL := startBotherer(t,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--job-selector-glob="+jobID,
	)

	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200, got %d", resp.StatusCode)
	}

	var health server.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode healthz JSON: %v", err)
	}
	if health.Status == "" {
		t.Error("health.Status should not be empty")
	}
	if health.GitCommit == "" {
		t.Error("health.GitCommit should be set after startup")
	}
}

// TestE2E_DriftDetectedViaHTTP registers a job in Nomad but provides a
// modified HCL in git, then verifies that /healthz reports the diff.
func TestE2E_DriftDetectedViaHTTP(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)

	jobID := uniqueJobID("e2e-drift")

	// Git has the modified HCL; Nomad has the original.
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLModified(jobID),
	})

	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	baseURL := startBotherer(t,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--job-selector-glob="+jobID,
	)

	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	var health server.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if health.DiffCount == 0 {
		t.Errorf("expected ≥1 diff (modified job), got diff_count=0")
	}

	// /diffs should also mention the job.
	diffsResp, err := http.Get(baseURL + "/diffs")
	if err != nil {
		t.Fatalf("GET /diffs: %v", err)
	}
	body, _ := io.ReadAll(diffsResp.Body)
	diffsResp.Body.Close()

	if !strings.Contains(string(body), jobID) {
		t.Errorf("/diffs output does not mention job %q:\n%s", jobID, body)
	}
}

// TestE2E_WebhookTriggersRefresh pushes a new commit to git and fires a
// webhook, then verifies that the server picks up the change within a few
// seconds without waiting for the next poll interval.
func TestE2E_WebhookTriggersRefresh(t *testing.T) {
	secret := "webhook-secret-" + randomSuffix()
	repoURL, workDir, branch := createGitRepo(t)

	jobID := uniqueJobID("e2e-webhook")

	// Start with matching HCL → no drift expected.
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCL(jobID),
	})

	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	// Very long poll interval so only webhooks drive refreshes.
	baseURL := startBotherer(t,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--job-selector-glob="+jobID,
		"--poll-interval=10m",
		"--diff-interval=10m",
		"--webhook-secret="+secret,
	)

	// Confirm initial state: no drift.
	if err := waitForHTTPStatus(baseURL+"/healthz", http.StatusOK, 30*time.Second); err != nil {
		t.Fatalf("initial health: %v", err)
	}

	// Push a modified HCL and fire a webhook.
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLModified(jobID),
	})
	fireWebhook(t, baseURL+"/webhook", secret, branch)

	// Wait up to 30s for the server to detect the drift.
	deadline := time.Now().Add(30 * time.Second)
	var lastCount int
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		var health server.HealthResponse
		json.NewDecoder(resp.Body).Decode(&health)
		resp.Body.Close()
		lastCount = health.DiffCount
		if health.DiffCount > 0 {
			return // drift detected — test passes
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("webhook did not trigger drift detection within 30s; last diff_count=%d", lastCount)
}

// TestE2E_GRPCGetDiffs starts the full binary with the gRPC server enabled,
// registers a drifting job, and verifies GetDiffs returns it over gRPC.
func TestE2E_GRPCGetDiffs(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	apiKey := "grpc-test-key-" + randomSuffix()

	jobID := uniqueJobID("e2e-grpc")

	// Git has modified HCL; Nomad has original → drift expected.
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLModified(jobID),
	})

	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	_, grpcAddr := startBothererWithGRPC(t, apiKey,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--job-selector-glob="+jobID,
	)

	conn, err := grpc.NewClient(grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	client := grpcapi.NewNomadBothererClient(conn)
	ctx := metadata.NewOutgoingContext(
		context.Background(),
		metadata.Pairs("authorization", "Bearer "+apiKey),
	)

	resp, err := client.GetDiffs(ctx, &grpcapi.GetDiffsRequest{})
	if err != nil {
		t.Fatalf("GetDiffs: %v", err)
	}
	if len(resp.Diffs) == 0 {
		t.Errorf("expected ≥1 diff over gRPC, got 0")
	}
	found := false
	for _, d := range resp.Diffs {
		if d.JobId == jobID {
			found = true
		}
	}
	if !found {
		t.Errorf("job %q not found in GetDiffs response: %v", jobID, resp.Diffs)
	}
}

// TestE2E_GRPCGetStatus verifies GetStatus returns a non-empty commit hash
// and a plausible last-update timestamp.
func TestE2E_GRPCGetStatus(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	apiKey := "status-key-" + randomSuffix()

	commitToGit(t, workDir, map[string]string{
		"placeholder.hcl": `# no jobs`,
	})

	_, grpcAddr := startBothererWithGRPC(t, apiKey,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--job-selector-glob=does-not-exist",
	)

	conn, err := grpc.NewClient(grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	client := grpcapi.NewNomadBothererClient(conn)
	ctx := metadata.NewOutgoingContext(
		context.Background(),
		metadata.Pairs("authorization", "Bearer "+apiKey),
	)

	resp, err := client.GetStatus(ctx, &grpcapi.GetStatusRequest{})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if len(resp.LastCommit) < 7 {
		t.Errorf("expected a non-empty commit hash, got %q", resp.LastCommit)
	}
	if resp.LastUpdateTime == "" {
		t.Error("LastUpdateTime should be set after a successful fetch")
	}
}

// TestE2E_GRPCTriggerRefresh verifies that TriggerRefresh causes the git
// watcher to pull (observable via a commit change reaching GetStatus).
func TestE2E_GRPCTriggerRefresh(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	apiKey := "trigger-key-" + randomSuffix()

	// Start with minimal content.
	commitToGit(t, workDir, map[string]string{"placeholder.hcl": "# initial"})

	_, grpcAddr := startBothererWithGRPC(t, apiKey,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--poll-interval=10m", // long interval so only TriggerRefresh drives fetches
		"--diff-interval=10m",
		"--job-selector-glob=does-not-exist",
	)

	conn, err := grpc.NewClient(grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	client := grpcapi.NewNomadBothererClient(conn)
	ctx := metadata.NewOutgoingContext(
		context.Background(),
		metadata.Pairs("authorization", "Bearer "+apiKey),
	)

	// Record the initial commit.
	s1, err := client.GetStatus(ctx, &grpcapi.GetStatusRequest{})
	if err != nil {
		t.Fatalf("GetStatus (initial): %v", err)
	}
	initialCommit := s1.LastCommit

	// Push a new commit to the repo.
	commitToGit(t, workDir, map[string]string{"placeholder.hcl": "# updated"})

	// Trigger a refresh.
	if _, err := client.TriggerRefresh(ctx, &grpcapi.TriggerRefreshRequest{}); err != nil {
		t.Fatalf("TriggerRefresh: %v", err)
	}

	// Wait for the commit to change.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s2, err := client.GetStatus(ctx, &grpcapi.GetStatusRequest{})
		if err == nil && s2.LastCommit != initialCommit {
			return // commit updated after trigger — test passes
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Error("TriggerRefresh did not cause the git commit to advance within 20s")
}

// TestE2E_MetricsEndpoint verifies that /metrics returns a valid Prometheus
// text exposition containing at least the core metric names.
func TestE2E_MetricsEndpoint(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	commitToGit(t, workDir, map[string]string{"placeholder.hcl": "# no jobs"})

	baseURL := startBotherer(t,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--job-selector-glob=does-not-exist",
	)

	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, metric := range []string{
		"nomad_botherer_diff_checks_total",
		"nomad_botherer_git_fetches_total",
		"nomad_botherer_info",
	} {
		if !strings.Contains(text, metric) {
			t.Errorf("metric %q not found in /metrics output", metric)
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// fireWebhook sends a synthetic push webhook to url with the given secret and branch.
func fireWebhook(t *testing.T, url, secret, branch string) {
	t.Helper()
	body := minimalPushPayload(branch)
	sig := webhookSig(secret, body)

	req, err := http.NewRequest("POST", url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new webhook request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("e2e-%s", randomSuffix()))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fire webhook: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook returned %d", resp.StatusCode)
	}
}
