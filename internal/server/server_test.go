package server_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
	"github.com/gerrowadat/nomad-botherer/internal/server"
)

// mockDiffSource implements server.DiffSource.
type mockDiffSource struct {
	diffs        []nomad.JobDiff
	selectedJobs []nomad.SelectedJob
	updates      []nomad.JobUpdate
	lastCheck    time.Time
	lastCommit   string
}

func (m *mockDiffSource) Diffs() ([]nomad.JobDiff, time.Time, string) {
	return m.diffs, m.lastCheck, m.lastCommit
}

func (m *mockDiffSource) SelectedJobs() ([]nomad.SelectedJob, time.Time, string) {
	return m.selectedJobs, m.lastCheck, m.lastCommit
}

func (m *mockDiffSource) Updates() []nomad.JobUpdate { return m.updates }

func (m *mockDiffSource) Ready() bool { return !m.lastCheck.IsZero() }

// mockGitSource implements server.GitStatusSource.
type mockGitSource struct {
	lastCommit string
	lastUpdate time.Time
	triggered  bool
}

func (m *mockGitSource) Trigger()                    { m.triggered = true }
func (m *mockGitSource) Status() (string, time.Time) { return m.lastCommit, m.lastUpdate }
func (m *mockGitSource) Ready() bool                 { return !m.lastUpdate.IsZero() }

// newTestServer builds a Server with fresh per-test Prometheus registry.
func newTestServer(t *testing.T, diffs []nomad.JobDiff) (*server.Server, *mockGitSource) {
	t.Helper()
	return newTestServerWithConfig(t, diffs, "", "main")
}

func newTestServerWithConfig(t *testing.T, diffs []nomad.JobDiff, webhookSecret, branch string) (*server.Server, *mockGitSource) {
	t.Helper()
	srv, gitSrc, _ := newTestServerWithRegistry(t, diffs, webhookSecret, branch)
	return srv, gitSrc
}

// newTestServerWithRegistry is like newTestServerWithConfig but also returns
// the Prometheus registry so tests can gather metric values.
func newTestServerWithRegistry(t *testing.T, diffs []nomad.JobDiff, webhookSecret, branch string) (*server.Server, *mockGitSource, *prometheus.Registry) {
	t.Helper()
	cfg := &config.Config{
		ListenAddr:    ":0",
		WebhookPath:   "/webhook",
		WebhookSecret: webhookSecret,
		Branch:        branch,
	}
	diffSrc := &mockDiffSource{
		diffs:      diffs,
		lastCheck:  time.Now(),
		lastCommit: "deadbeef",
	}
	gitSrc := &mockGitSource{
		lastCommit: "deadbeef",
		lastUpdate: time.Now(),
	}
	reg := prometheus.NewRegistry()
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, reg)
	return srv, gitSrc, reg
}

// githubPushRequest builds a minimal GitHub push webhook request.
// If secret is non-empty, the correct HMAC-SHA256 signature is added.
func githubPushRequest(t *testing.T, secret, branch, commitSHA string) *http.Request {
	t.Helper()
	body := []byte(fmt.Sprintf(
		`{"ref":"refs/heads/%s","before":"0000000000000000000000000000000000000000","after":"%s","commits":[]}`,
		branch, commitSHA,
	))
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "test-delivery-id")
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Hub-Signature-256", fmt.Sprintf("sha256=%x", mac.Sum(nil)))
	}
	return req
}

func githubPingRequest(t *testing.T) *http.Request {
	t.Helper()
	body := []byte(`{"hook_id":42,"hook":{"type":"Repository","id":42,"name":"web","active":true,"events":["push"]}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-GitHub-Delivery", "test-ping-id")
	return req
}

// ── / (index) ─────────────────────────────────────────────────────────────────

func TestIndex_NoDiffs(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected text/html content-type, got %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/diffs") {
		t.Error("index page should link to /diffs")
	}
	if !strings.Contains(body, "/healthz") {
		t.Error("index page should link to /healthz")
	}
	if !strings.Contains(body, "no differences") {
		t.Error("index page should indicate no differences when there are none")
	}
}

func TestIndex_WithDiffs(t *testing.T) {
	diffs := []nomad.JobDiff{
		{JobID: "api", DiffType: nomad.DiffTypeModified, Detail: "Edited"},
	}
	srv, _ := newTestServer(t, diffs)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "1 difference") {
		t.Error("index page should report the diff count")
	}
}

// ── /diffs ────────────────────────────────────────────────────────────────────

func TestDiffs_NoDiffs(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/diffs", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("expected text/plain content-type, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "No differences") {
		t.Error("expected 'No differences' message")
	}
}

func TestDiffs_MissingFromNomad(t *testing.T) {
	diffs := []nomad.JobDiff{
		{JobID: "new-job", HCLFile: "jobs/new-job.hcl", DiffType: nomad.DiffTypeMissingFromNomad},
	}
	srv, _ := newTestServer(t, diffs)
	req := httptest.NewRequest(http.MethodGet, "/diffs", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `+ Job:`) {
		t.Error("missing-from-nomad job should render with '+' prefix")
	}
	if !strings.Contains(body, "new-job") {
		t.Error("job ID should appear in output")
	}
}

func TestDiffs_MissingFromHCL(t *testing.T) {
	diffs := []nomad.JobDiff{
		{JobID: "orphan", DiffType: nomad.DiffTypeMissingFromHCL, Detail: "running but no HCL"},
	}
	srv, _ := newTestServer(t, diffs)
	req := httptest.NewRequest(http.MethodGet, "/diffs", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `- Job:`) {
		t.Error("missing-from-hcl job should render with '-' prefix")
	}
}

func TestDiffs_Modified_WithPlanDiff(t *testing.T) {
	diffs := []nomad.JobDiff{
		{
			JobID:    "api",
			HCLFile:  "jobs/api.hcl",
			DiffType: nomad.DiffTypeModified,
			PlanDiff: &nomadapi.JobDiff{
				Type: "Edited",
				ID:   "api",
				TaskGroups: []*nomadapi.TaskGroupDiff{
					{
						Type: "Edited",
						Name: "web",
						Tasks: []*nomadapi.TaskDiff{
							{
								Type: "Edited",
								Name: "server",
								Objects: []*nomadapi.ObjectDiff{
									{
										Type: "Edited",
										Name: "Config",
										Fields: []*nomadapi.FieldDiff{
											{Type: "Edited", Name: "image", Old: "nginx:1.19", New: "nginx:1.21"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	srv, _ := newTestServer(t, diffs)
	req := httptest.NewRequest(http.MethodGet, "/diffs", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "+/- Job:") {
		t.Error("modified job should render with '+/-' prefix")
	}
	if !strings.Contains(body, "Task Group:") {
		t.Error("task group diff should appear in output")
	}
	if !strings.Contains(body, `"nginx:1.19" => "nginx:1.21"`) {
		t.Error("field diff old/new values should appear in output")
	}
}

// ── /healthz ──────────────────────────────────────────────────────────────────

func TestHealthz_NoDiffs(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp server.HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("want status ok, got %q", resp.Status)
	}
	if resp.DiffCount != 0 {
		t.Errorf("want 0 diffs, got %d", resp.DiffCount)
	}
}

func TestHealthz_WithDiffs(t *testing.T) {
	diffs := []nomad.JobDiff{
		{JobID: "api", HCLFile: "jobs/api.hcl", DiffType: nomad.DiffTypeModified, Detail: "Edited"},
		{JobID: "old", DiffType: nomad.DiffTypeMissingFromHCL, Detail: "running but no HCL"},
	}
	srv, _ := newTestServer(t, diffs)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp server.HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "diffs_detected" {
		t.Errorf("want diffs_detected, got %q", resp.Status)
	}
	if resp.DiffCount != 2 {
		t.Errorf("want 2, got %d", resp.DiffCount)
	}
}

func TestHealthz_ContentType(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("want application/json, got %q", ct)
	}
}

func TestHealthz_IncludesGitInfo(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	var resp server.HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.GitCommit == "" {
		t.Error("GitCommit should be populated")
	}
	if resp.LastCheck == "" {
		t.Error("LastCheck should be populated")
	}
}

// ── /webhook ──────────────────────────────────────────────────────────────────

func TestWebhook_PushToWatchedBranch_Triggers(t *testing.T) {
	srv, gitSrc := newTestServer(t, nil)

	req := githubPushRequest(t, "", "main", "abc123")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !gitSrc.triggered {
		t.Error("Trigger() should have been called for a push to the watched branch")
	}
}

func TestWebhook_PushToOtherBranch_NoTrigger(t *testing.T) {
	srv, gitSrc := newTestServer(t, nil)

	req := githubPushRequest(t, "", "feature/foo", "abc123")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if gitSrc.triggered {
		t.Error("Trigger() should NOT have been called for a push to a different branch")
	}
}

func TestWebhook_UnknownEvent_Returns200(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "issues") // registered: push + ping only
	req.Header.Set("X-GitHub-Delivery", "test-id")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for unknown event, got %d", rec.Code)
	}
}

func TestWebhook_PingEvent_Returns200(t *testing.T) {
	srv, gitSrc := newTestServer(t, nil)

	req := githubPingRequest(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for ping, got %d", rec.Code)
	}
	if gitSrc.triggered {
		t.Error("ping should not trigger a fetch")
	}
}

func TestWebhook_WithSecret_ValidSignature_Triggers(t *testing.T) {
	const secret = "super-secret-webhook-key"
	srv, gitSrc := newTestServerWithConfig(t, nil, secret, "main")

	req := githubPushRequest(t, secret, "main", "def456")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !gitSrc.triggered {
		t.Error("Trigger() should have been called with a valid signature")
	}
}

// ── webhook event counters ────────────────────────────────────────────────────

func TestWebhook_PushCounter(t *testing.T) {
	srv, _, reg := newTestServerWithRegistry(t, nil, "", "main")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), githubPushRequest(t, "", "main", "abc"))
	if count := testutil.CollectAndCount(reg, "nomad_botherer_webhook_events_total"); count == 0 {
		t.Error("expected webhook_events_total to be registered")
	}
}

func TestWebhook_CounterByEvent(t *testing.T) {
	srv, _, reg := newTestServerWithRegistry(t, nil, "", "main")

	// push
	srv.Handler().ServeHTTP(httptest.NewRecorder(), githubPushRequest(t, "", "main", "abc"))
	// ping
	srv.Handler().ServeHTTP(httptest.NewRecorder(), githubPingRequest(t))
	// unknown event
	unknownReq := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader([]byte(`{}`)))
	unknownReq.Header.Set("Content-Type", "application/json")
	unknownReq.Header.Set("X-GitHub-Event", "issues")
	unknownReq.Header.Set("X-GitHub-Delivery", "x")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), unknownReq)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	counts := map[string]float64{}
	for _, mf := range families {
		if mf.GetName() != "nomad_botherer_webhook_events_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "event" {
					counts[lp.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
	}
	if counts["push"] != 1 {
		t.Errorf("push counter: want 1, got %v", counts["push"])
	}
	if counts["ping"] != 1 {
		t.Errorf("ping counter: want 1, got %v", counts["ping"])
	}
	if counts["unknown"] != 1 {
		t.Errorf("unknown counter: want 1, got %v", counts["unknown"])
	}
}

// ── webhook timestamp tracking ────────────────────────────────────────────────

func TestWebhook_SuccessTimestampOnIndex(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	// Before any webhook the index should have no webhook timestamp.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "Last webhook") {
		t.Error("index should not show webhook line before any webhook is received")
	}

	// Fire a push webhook.
	srv.Handler().ServeHTTP(httptest.NewRecorder(), githubPushRequest(t, "", "main", "abc123"))

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Last webhook") {
		t.Error("index should show webhook line after a successful webhook")
	}
	if !strings.Contains(body, "ok") {
		t.Error("index should show 'ok' timestamp after successful webhook")
	}
}

func TestWebhook_FailureTimestampOnIndex(t *testing.T) {
	const secret = "mykey"
	srv, _ := newTestServerWithConfig(t, nil, secret, "main")

	// Bad signature → failure.
	body := []byte(`{"ref":"refs/heads/main","before":"000","after":"abc","commits":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "fail-test")
	req.Header.Set("X-Hub-Signature-256", "sha256=invalidsignature")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	indexBody := rec.Body.String()
	if !strings.Contains(indexBody, "Last webhook") {
		t.Error("index should show webhook line after a failed webhook")
	}
	if !strings.Contains(indexBody, "failed") {
		t.Error("index should show 'failed' timestamp after a webhook error")
	}
}

func TestWebhook_SuccessGauge(t *testing.T) {
	srv, _, reg := newTestServerWithRegistry(t, nil, "", "main")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), githubPushRequest(t, "", "main", "abc"))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == "nomad_botherer_last_webhook_success_timestamp_seconds" {
			if v := mf.GetMetric()[0].GetGauge().GetValue(); v == 0 {
				t.Error("last_webhook_success gauge should be non-zero after a successful webhook")
			}
			return
		}
	}
	t.Error("nomad_botherer_last_webhook_success_timestamp_seconds not found in registry")
}

func TestWebhook_FailureGauge(t *testing.T) {
	const secret = "mykey"
	srv, _, reg := newTestServerWithRegistry(t, nil, secret, "main")

	body := []byte(`{"ref":"refs/heads/main","before":"000","after":"abc","commits":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "fail-gauge-test")
	req.Header.Set("X-Hub-Signature-256", "sha256=invalidsignature")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == "nomad_botherer_last_webhook_failure_timestamp_seconds" {
			if v := mf.GetMetric()[0].GetGauge().GetValue(); v == 0 {
				t.Error("last_webhook_failure gauge should be non-zero after a failed webhook")
			}
			return
		}
	}
	t.Error("nomad_botherer_last_webhook_failure_timestamp_seconds not found in registry")
}

func TestWebhook_WithSecret_InvalidSignature_Rejected(t *testing.T) {
	const secret = "super-secret-webhook-key"
	srv, gitSrc := newTestServerWithConfig(t, nil, secret, "main")

	// Build the request with the correct payload structure but a wrong HMAC.
	body := []byte(`{"ref":"refs/heads/main","before":"000","after":"abc","commits":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "test-id")
	req.Header.Set("X-Hub-Signature-256", "sha256=invalidsignature")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid signature, got %d", rec.Code)
	}
	if gitSrc.triggered {
		t.Error("Trigger() should NOT be called when signature is invalid")
	}
}

// ── readiness gate ────────────────────────────────────────────────────────────

// newNotReadyServer builds a server where neither git nor diffs are ready.
func newNotReadyServer(t *testing.T) *server.Server {
	t.Helper()
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main"}
	diffSrc := &mockDiffSource{} // zero lastCheck → not ready
	gitSrc := &mockGitSource{}   // zero lastUpdate → not ready
	return server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())
}

func TestHealthz_NotReady_Returns503(t *testing.T) {
	srv := newNotReadyServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	var resp server.HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "starting" {
		t.Errorf("want status starting, got %q", resp.Status)
	}
	if resp.Message == "" {
		t.Error("want non-empty message when not ready")
	}
}

func TestHealthz_GitNotReady_Returns503(t *testing.T) {
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main"}
	diffSrc := &mockDiffSource{lastCheck: time.Now(), lastCommit: "abc"} // diffs ready
	gitSrc := &mockGitSource{}                                            // git not ready
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when git not ready, got %d", rec.Code)
	}
}

func TestHealthz_DiffsNotReady_Returns503(t *testing.T) {
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main"}
	diffSrc := &mockDiffSource{}                                                // diffs not ready
	gitSrc := &mockGitSource{lastCommit: "abc", lastUpdate: time.Now()} // git ready
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when diffs not ready, got %d", rec.Code)
	}
}

func TestDiffs_NotReady_Returns503(t *testing.T) {
	srv := newNotReadyServer(t)
	req := httptest.NewRequest(http.MethodGet, "/diffs", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
}

func TestIndex_NotReady_Returns503(t *testing.T) {
	srv := newNotReadyServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "starting") {
		t.Error("index page should show starting state when not ready")
	}
}

// ── /metrics ──────────────────────────────────────────────────────────────────

func TestMetrics_Endpoint_Returns200(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from /metrics, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("expected text/plain content-type from /metrics, got %q", ct)
	}
}

func TestMetrics_Endpoint_ContainsBuildInfo(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "nomad_botherer_info") {
		t.Error("expected nomad_botherer_info metric in /metrics output")
	}
}

// ── Handler consistency ───────────────────────────────────────────────────────

func TestHandler_ConsistentReturn(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	h1 := srv.Handler()
	h2 := srv.Handler()
	if h1 != h2 {
		t.Error("Handler() should return the same http.Handler on repeated calls")
	}
}

// ── / selected jobs ───────────────────────────────────────────────────────────

func newTestServerWithSelectedJobs(t *testing.T, jobs []nomad.SelectedJob) *server.Server {
	t.Helper()
	cfg := &config.Config{
		ListenAddr:  ":0",
		WebhookPath: "/webhook",
		Branch:      "main",
	}
	diffSrc := &mockDiffSource{
		selectedJobs: jobs,
		lastCheck:    time.Now(),
		lastCommit:   "deadbeef",
	}
	gitSrc := &mockGitSource{lastCommit: "deadbeef", lastUpdate: time.Now()}
	return server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())
}

func TestIndex_SelectedJobs_Shown(t *testing.T) {
	jobs := []nomad.SelectedJob{
		{JobID: "api", Reason: nomad.SelectionReasonMeta},
		{JobID: "worker", Reason: nomad.SelectionReasonGlob},
	}
	srv := newTestServerWithSelectedJobs(t, jobs)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "api") {
		t.Error("index page should list job 'api'")
	}
	if !strings.Contains(body, "worker") {
		t.Error("index page should list job 'worker'")
	}
	if !strings.Contains(body, "meta") {
		t.Error("index page should show selection reason 'meta'")
	}
	if !strings.Contains(body, "glob") {
		t.Error("index page should show selection reason 'glob'")
	}
	if !strings.Contains(body, "Selected jobs (2)") {
		t.Error("index page should show selected jobs count")
	}
}

func TestIndex_NoSelectedJobs_SectionAbsent(t *testing.T) {
	srv := newTestServerWithSelectedJobs(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "Selected jobs") {
		t.Error("index page should not show selected-jobs section when there are no selected jobs")
	}
}

// ── /api/ ─────────────────────────────────────────────────────────────────────

func newTestServerWithAPI(t *testing.T, apiKey string) *server.Server {
	t.Helper()
	cfg := &config.Config{
		ListenAddr:  ":0",
		WebhookPath: "/webhook",
		Branch:      "main",
		APIKey:      apiKey,
	}
	diffSrc := &mockDiffSource{
		diffs: []nomad.JobDiff{
			{JobID: "api-job", DiffType: nomad.DiffTypeModified, Detail: "changed"},
		},
		selectedJobs: []nomad.SelectedJob{
			{JobID: "api-job", Reason: nomad.SelectionReasonMeta},
		},
		lastCheck:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		lastCommit: "abc123",
	}
	gitSrc := &mockGitSource{lastCommit: "abc123", lastUpdate: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "v1.0.0", Commit: "abc", BuildDate: "2026-01-01"}, prometheus.NewRegistry())
}

func apiReq(t *testing.T, method, path, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	rec := httptest.NewRecorder()
	return rec
}

func TestAPI_Disabled_Returns404(t *testing.T) {
	// No APIKey → routes not registered → 404
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/diffs", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404 when API disabled, got %d", rec.Code)
	}
}

func TestAPI_NoKey_Returns401(t *testing.T) {
	srv := newTestServerWithAPI(t, "secret")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/diffs", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401 without key, got %d", rec.Code)
	}
}

func TestAPI_WrongKey_Returns401(t *testing.T) {
	srv := newTestServerWithAPI(t, "correct")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/diffs", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401 with wrong key, got %d", rec.Code)
	}
}

func TestAPI_Diffs(t *testing.T) {
	const key = "testkey"
	srv := newTestServerWithAPI(t, key)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/diffs", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("want application/json, got %q", ct)
	}
	var resp struct {
		Diffs []struct {
			JobID string `json:"job_id"`
		} `json:"diffs"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Diffs) != 1 || resp.Diffs[0].JobID != "api-job" {
		t.Errorf("unexpected diffs: %+v", resp.Diffs)
	}
}

func TestAPI_SelectedJobs(t *testing.T) {
	const key = "testkey"
	srv := newTestServerWithAPI(t, key)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/selected-jobs", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var resp struct {
		Jobs []struct {
			JobID  string `json:"job_id"`
			Reason string `json:"selection_reason"`
		} `json:"jobs"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Jobs) != 1 || resp.Jobs[0].JobID != "api-job" || resp.Jobs[0].Reason != "meta" {
		t.Errorf("unexpected jobs: %+v", resp.Jobs)
	}
}

func TestAPI_Status(t *testing.T) {
	const key = "testkey"
	srv := newTestServerWithAPI(t, key)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var resp struct {
		LastCommit string `json:"last_commit"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LastCommit != "abc123" {
		t.Errorf("want commit abc123, got %q", resp.LastCommit)
	}
}

func TestAPI_Version(t *testing.T) {
	const key = "testkey"
	srv := newTestServerWithAPI(t, key)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var resp struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version != "v1.0.0" || resp.Commit != "abc" {
		t.Errorf("unexpected version response: %+v", resp)
	}
}

func TestAPI_Refresh(t *testing.T) {
	const key = "testkey"
	srv := newTestServerWithAPI(t, key)
	_, gitSrc := newTestServer(t, nil) // unused, just checking trigger
	_ = gitSrc
	req := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("want 200 on refresh, got %d", rec.Code)
	}
}

func TestAPI_Spec_Public(t *testing.T) {
	srv := newTestServerWithAPI(t, "secret")
	// No Authorization header — spec should be public
	req := httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for spec, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"openapi"`) {
		t.Error("spec response should contain openapi key")
	}
}

func notReadySrv(t *testing.T) *server.Server {
	t.Helper()
	const key = "testkey"
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main", APIKey: key}
	diffSrc := &mockDiffSource{} // zero lastCheck → not ready
	gitSrc := &mockGitSource{lastCommit: "x", lastUpdate: time.Now()}
	return server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())
}

func TestAPI_NotReady_Diffs_Returns503(t *testing.T) {
	srv := notReadySrv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/diffs", nil)
	req.Header.Set("Authorization", "Bearer testkey")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when not ready, got %d", rec.Code)
	}
}

func TestAPI_NotReady_SelectedJobs_Returns503(t *testing.T) {
	srv := notReadySrv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/selected-jobs", nil)
	req.Header.Set("Authorization", "Bearer testkey")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 for selected-jobs when not ready, got %d", rec.Code)
	}
}

func TestAPI_NotReady_Status_Returns503(t *testing.T) {
	const key = "testkey"
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main", APIKey: key}
	diffSrc := &mockDiffSource{lastCheck: time.Now()} // diffs ready
	gitSrc := &mockGitSource{}                         // git NOT ready (zero lastUpdate)
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 for status when git not ready, got %d", rec.Code)
	}
}

func TestFmtTime_ZeroReturnsEmpty(t *testing.T) {
	if got := server.FmtTime(time.Time{}); got != "" {
		t.Errorf("zero time: want empty string, got %q", got)
	}
}

func TestFmtTime_NonZeroReturnsRFC3339(t *testing.T) {
	ts := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	got := server.FmtTime(ts)
	if got != "2026-01-15T12:00:00Z" {
		t.Errorf("want RFC3339 UTC, got %q", got)
	}
}

// ── /api/v1/updates ───────────────────────────────────────────────────────────

func TestAPIUpdates_RequiresAuth(t *testing.T) {
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main", APIKey: "k-" + strings.Repeat("x", 16)}
	srv := server.NewWithRegistry(cfg, &mockDiffSource{lastCheck: time.Now()}, &mockGitSource{lastUpdate: time.Now()},
		server.BuildInfo{Version: "test"}, prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/updates", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: want 401, got %d", rec.Code)
	}
}

func TestAPIUpdates_ReturnsQueue(t *testing.T) {
	key := "k-" + strings.Repeat("x", 16)
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main", APIKey: key}
	diffSrc := &mockDiffSource{
		lastCheck: time.Now(),
		updates: []nomad.JobUpdate{
			{
				UpdateID:            "myapp/abc1234",
				JobID:               "myapp",
				HCLFile:             "myapp.hcl",
				GitCommit:           "abc1234def",
				Operation:           nomad.JobUpdateOperationRegister,
				Status:              nomad.JobUpdateStatusSucceeded,
				Policy:              nomad.UpdatePolicyFull,
				NomadJobModifyIndex: 43,
			},
		},
	}
	srv := server.NewWithRegistry(cfg, diffSrc, &mockGitSource{lastUpdate: time.Now()},
		server.BuildInfo{Version: "test"}, prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/updates", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Updates []nomad.JobUpdate `json:"updates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Updates) != 1 {
		t.Fatalf("want 1 update, got %d", len(resp.Updates))
	}
	u := resp.Updates[0]
	if u.UpdateID != "myapp/abc1234" || u.Operation != nomad.JobUpdateOperationRegister ||
		u.Status != nomad.JobUpdateStatusSucceeded || u.Policy != nomad.UpdatePolicyFull {
		t.Errorf("unexpected update payload: %+v", u)
	}
}

func TestAPIUpdates_EmptyQueueIsEmptyArray(t *testing.T) {
	key := "k-" + strings.Repeat("x", 16)
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main", APIKey: key}
	srv := server.NewWithRegistry(cfg, &mockDiffSource{lastCheck: time.Now()}, &mockGitSource{lastUpdate: time.Now()},
		server.BuildInfo{Version: "test"}, prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/updates", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"updates":[]`) {
		t.Errorf("empty queue should serialise as [], got %s", rec.Body.String())
	}
}

func TestAPISpec_IncludesUpdates(t *testing.T) {
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main", APIKey: "k-" + strings.Repeat("x", 16)}
	srv := server.NewWithRegistry(cfg, &mockDiffSource{lastCheck: time.Now()}, &mockGitSource{lastUpdate: time.Now()},
		server.BuildInfo{Version: "test"}, prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	for _, want := range []string{`"/updates"`, `"JobUpdate"`, "SUPERSEDED"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("OpenAPI spec missing %s", want)
		}
	}
}

// ── / (index) apply mode ──────────────────────────────────────────────────────

func TestIndex_ApplyMode_Defaults(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Apply mode: default policy <code>none</code>") {
		t.Errorf("index should show the default policy, got:\n%.500s", body)
	}
	if !strings.Contains(body, "job creation disabled") {
		t.Error("index should show job creation disabled by default")
	}
	if strings.Contains(body, "pending") {
		t.Error("no pending-updates note expected with an empty queue")
	}
}

func TestIndex_ApplyMode_EnabledWithPending(t *testing.T) {
	cfg := &config.Config{
		ListenAddr:          ":0",
		WebhookPath:         "/webhook",
		Branch:              "main",
		DefaultUpdatePolicy: "full",
		EnableJobCreation:   true,
	}
	diffSrc := &mockDiffSource{
		lastCheck: time.Now(),
		updates: []nomad.JobUpdate{
			{UpdateID: "a/1234567", JobID: "a", Status: nomad.JobUpdateStatusPending},
			{UpdateID: "b/1234567", JobID: "b", Status: nomad.JobUpdateStatusSucceeded},
		},
	}
	srv := server.NewWithRegistry(cfg, diffSrc, &mockGitSource{lastUpdate: time.Now()},
		server.BuildInfo{Version: "test"}, prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "default policy <code>full</code>") {
		t.Errorf("index should show policy full, got:\n%.500s", body)
	}
	if !strings.Contains(body, "job creation") || !strings.Contains(body, "enabled") {
		t.Error("index should show job creation enabled")
	}
	if !strings.Contains(body, "1 update(s) pending") {
		t.Errorf("index should count only non-terminal updates as pending, got:\n%.500s", body)
	}
}
