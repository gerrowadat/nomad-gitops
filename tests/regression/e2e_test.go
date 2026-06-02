//go:build regression

package regression

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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

// TestE2E_APIDisabledByDefault verifies that /api/v1/ endpoints return 404
// when --api-key is not set (default).
func TestE2E_APIDisabledByDefault(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	commitToGit(t, workDir, map[string]string{"placeholder.hcl": "# no jobs"})

	baseURL := startBotherer(t,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--job-selector-glob=does-not-exist",
	)

	req, _ := http.NewRequest("GET", baseURL+"/api/v1/diffs", nil)
	req.Header.Set("Authorization", "Bearer anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/diffs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404 when --api-key not set, got %d", resp.StatusCode)
	}
}

// TestE2E_APIGetDiffs starts the binary with --api-key, registers a drifting
// job, and verifies GET /api/v1/diffs returns it.
func TestE2E_APIGetDiffs(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	apiKey := "api-test-key-" + randomSuffix()

	jobID := uniqueJobID("e2e-api")

	// Git has modified HCL; Nomad has original → drift expected.
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLModified(jobID),
	})
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	baseURL := startBothererWithAPI(t, apiKey,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--job-selector-glob="+jobID,
	)

	var result struct {
		Diffs []struct {
			JobID string `json:"job_id"`
		} `json:"diffs"`
	}
	apiGet(t, baseURL+"/api/v1/diffs", apiKey, &result)

	if len(result.Diffs) == 0 {
		t.Fatalf("expected ≥1 diff, got 0")
	}
	found := false
	for _, d := range result.Diffs {
		if d.JobID == jobID {
			found = true
		}
	}
	if !found {
		t.Errorf("job %q not found in /api/v1/diffs response", jobID)
	}
}

// TestE2E_APIGetStatus verifies GET /api/v1/status returns a non-empty commit.
func TestE2E_APIGetStatus(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	apiKey := "status-key-" + randomSuffix()

	commitToGit(t, workDir, map[string]string{"placeholder.hcl": "# no jobs"})

	baseURL := startBothererWithAPI(t, apiKey,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--job-selector-glob=does-not-exist",
	)

	var result struct {
		LastCommit  string `json:"last_commit"`
		LastUpdated string `json:"last_updated"`
	}
	apiGet(t, baseURL+"/api/v1/status", apiKey, &result)

	if len(result.LastCommit) < 7 {
		t.Errorf("expected a non-empty commit hash, got %q", result.LastCommit)
	}
	if result.LastUpdated == "" {
		t.Error("last_updated should be set after a successful fetch")
	}
}

// TestE2E_APIRefresh verifies that POST /api/v1/refresh causes the git watcher
// to pull (observable via a commit change on the status endpoint).
func TestE2E_APIRefresh(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	apiKey := "refresh-key-" + randomSuffix()

	commitToGit(t, workDir, map[string]string{"placeholder.hcl": "# initial"})

	baseURL := startBothererWithAPI(t, apiKey,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--poll-interval=10m",
		"--diff-interval=10m",
		"--job-selector-glob=does-not-exist",
	)

	var s1 struct {
		LastCommit string `json:"last_commit"`
	}
	apiGet(t, baseURL+"/api/v1/status", apiKey, &s1)
	initialCommit := s1.LastCommit

	// Push a new commit to the repo.
	commitToGit(t, workDir, map[string]string{"placeholder.hcl": "# updated"})

	// Trigger a refresh via the API.
	req, _ := http.NewRequest("POST", baseURL+"/api/v1/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/refresh: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/refresh returned %d", resp.StatusCode)
	}

	// Wait for the commit to advance.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var s2 struct {
			LastCommit string `json:"last_commit"`
		}
		if err := apiGetErr(baseURL+"/api/v1/status", apiKey, &s2); err == nil && s2.LastCommit != initialCommit {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Error("POST /api/v1/refresh did not cause the git commit to advance within 20s")
}

// TestE2E_APISelectedJobs verifies GET /api/v1/selected-jobs returns the list
// of watched jobs when at least one job is selected by glob.
func TestE2E_APISelectedJobs(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	apiKey := "selj-key-" + randomSuffix()

	jobID := uniqueJobID("e2e-selj")
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCL(jobID),
	})
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	baseURL := startBothererWithAPI(t, apiKey,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--job-selector-glob="+jobID,
	)

	var result struct {
		Jobs []struct {
			JobID  string `json:"job_id"`
			Reason string `json:"selection_reason"`
		} `json:"jobs"`
	}
	apiGet(t, baseURL+"/api/v1/selected-jobs", apiKey, &result)

	found := false
	for _, j := range result.Jobs {
		if j.JobID == jobID {
			found = true
			if j.Reason != "glob" {
				t.Errorf("job %q: want reason=glob, got %q", jobID, j.Reason)
			}
		}
	}
	if !found {
		t.Errorf("job %q not found in /api/v1/selected-jobs: %+v", jobID, result.Jobs)
	}
}

// TestE2E_APIVersion verifies GET /api/v1/version returns build metadata.
func TestE2E_APIVersion(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	apiKey := "ver-key-" + randomSuffix()
	commitToGit(t, workDir, map[string]string{"placeholder.hcl": "# no jobs"})

	baseURL := startBothererWithAPI(t, apiKey,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--job-selector-glob=does-not-exist",
	)

	var result struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	apiGet(t, baseURL+"/api/v1/version", apiKey, &result)

	if result.Version == "" {
		t.Error("version should not be empty")
	}
	if result.Commit == "" {
		t.Error("commit should not be empty")
	}
}

// TestE2E_APISpec verifies GET /api/openapi.json is public and returns a valid
// OpenAPI document.
func TestE2E_APISpec(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	apiKey := "spec-key-" + randomSuffix()
	commitToGit(t, workDir, map[string]string{"placeholder.hcl": "# no jobs"})

	baseURL := startBothererWithAPI(t, apiKey,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--job-selector-glob=does-not-exist",
	)

	// No Authorization header — spec is public.
	resp, err := http.Get(baseURL + "/api/openapi.json")
	if err != nil {
		t.Fatalf("GET /api/openapi.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"openapi"`) {
		t.Error("spec response should contain openapi field")
	}
}

// ── API helpers ───────────────────────────────────────────────────────────────

// apiGet makes an authenticated GET request and decodes the JSON response into v.
func apiGet(t *testing.T, url, apiKey string, v any) {
	t.Helper()
	if err := apiGetErr(url, apiKey, v); err != nil {
		t.Fatalf("apiGet %s: %v", url, err)
	}
}

func apiGetErr(url, apiKey string, v any) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(v)
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
