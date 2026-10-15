package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gerrowadat/nomad-gitops/internal/config"
	"github.com/gerrowadat/nomad-gitops/internal/nomad"
	"github.com/gerrowadat/nomad-gitops/internal/server"
)

const freezeTestKey = "test-key-0123456789"

// freezeServer builds a server whose diff source reports the given freeze
// records. writeAPI sets --enable-freeze-api.
func freezeServer(t *testing.T, writeAPI bool, recs ...nomad.FreezeRecord) (*server.Server, *mockDiffSource) {
	t.Helper()
	cfg := &config.Config{
		ListenAddr:         ":0",
		WebhookPath:        "/webhook",
		Branch:             "main",
		APIKey:             freezeTestKey,
		EnableFreezeAPI:    writeAPI,
		FreezeMaxStaleness: 5 * time.Minute,
	}
	diffSrc := &mockDiffSource{
		lastCheck:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		lastCommit:    "abc123",
		freezeEnabled: true,
		freezes:       recs,
		freezeRead:    time.Now(),
		freezeHealthy: true,
	}
	gitSrc := &mockGitSource{lastCommit: "abc123", lastUpdate: time.Now()}
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())
	return srv, diffSrc
}

// activeFreeze builds a classified record in force for the next hour.
func activeFreeze(jobID, reason string) nomad.FreezeRecord {
	rec := nomad.NewFreezeRecord(jobID, reason, "doc", time.Now().Add(time.Hour).UTC().Format(time.RFC3339), time.Now().Add(-30*time.Minute))
	rec.VariablePath = "nomad/jobs/gitops/freezes/" + jobID
	rec.Classify(time.Now())
	return rec
}

func indefiniteFreeze(jobID, reason string) nomad.FreezeRecord {
	rec := nomad.NewFreezeRecord(jobID, reason, "", "", time.Now().Add(-48*time.Hour))
	rec.VariablePath = "nomad/jobs/gitops/freezes/" + jobID
	rec.Classify(time.Now())
	return rec
}

func expiredFreeze(jobID, reason string) nomad.FreezeRecord {
	rec := nomad.NewFreezeRecord(jobID, reason, "", time.Now().Add(-6*24*time.Hour).UTC().Format(time.RFC3339), time.Now().Add(-7*24*time.Hour))
	rec.VariablePath = "nomad/jobs/gitops/freezes/" + jobID
	rec.Classify(time.Now())
	return rec
}

func doReq(t *testing.T, srv *server.Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.Header.Set("Authorization", "Bearer "+freezeTestKey)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// ── GET /api/v1/freezes ───────────────────────────────────────────────────────

func TestAPIFreezes_ListsRecords(t *testing.T) {
	srv, _ := freezeServer(t, false, activeFreeze("api-server", "INC-4412"), expiredFreeze("old-worker", "last week"))
	rec := doReq(t, srv, http.MethodGet, "/api/v1/freezes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		Freezes []struct {
			JobID        string `json:"job_id"`
			State        string `json:"state"`
			Reason       string `json:"reason"`
			VariablePath string `json:"variable_path"`
		} `json:"freezes"`
		StoreHealthy  bool   `json:"store_healthy"`
		StoreLastRead string `json:"store_last_read"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Freezes) != 2 {
		t.Fatalf("want 2 records (expired ones are reported too), got %d", len(resp.Freezes))
	}
	if resp.Freezes[0].State != "active" || resp.Freezes[1].State != "expired" {
		t.Errorf("states = %q/%q, want active/expired", resp.Freezes[0].State, resp.Freezes[1].State)
	}
	if resp.Freezes[0].Reason != "INC-4412" {
		t.Errorf("reason = %q", resp.Freezes[0].Reason)
	}
	if !resp.StoreHealthy || resp.StoreLastRead == "" {
		t.Errorf("want a healthy store with a last-read time, got %+v", resp)
	}
}

func TestAPIFreezes_ReadableWithoutTheWriteAPI(t *testing.T) {
	// Reading freezes is always available; only writing needs the flag.
	srv, _ := freezeServer(t, false, activeFreeze("api-server", "INC-4412"))
	if rec := doReq(t, srv, http.MethodGet, "/api/v1/freezes", ""); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 even with --enable-freeze-api off", rec.Code)
	}
}

func TestAPIFreezes_RequiresAPIKey(t *testing.T) {
	srv, _ := freezeServer(t, false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/freezes", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a key", rec.Code)
	}
}

func TestAPIFreezes_UnhealthyStoreIsReported(t *testing.T) {
	srv, diffSrc := freezeServer(t, false)
	diffSrc.freezeHealthy = false

	rec := doReq(t, srv, http.MethodGet, "/api/v1/freezes", "")
	var resp struct {
		StoreHealthy bool `json:"store_healthy"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.StoreHealthy {
		t.Error("store_healthy should be false when the freeze store cannot be read")
	}
}

func TestAPIFreezes_EmptyWhenDisabled(t *testing.T) {
	srv, diffSrc := freezeServer(t, false)
	diffSrc.freezeEnabled = false

	rec := doReq(t, srv, http.MethodGet, "/api/v1/freezes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"freezes":[]`) {
		t.Errorf("want an empty array when freeze support is off, got %s", rec.Body.String())
	}
}

// ── POST /api/v1/freezes/{job} ────────────────────────────────────────────────

func TestAPIFreezeJob_Writes(t *testing.T) {
	srv, diffSrc := freezeServer(t, true)
	rec := doReq(t, srv, http.MethodPost, "/api/v1/freezes/api-server",
		`{"reason":"INC-4412: by hand","frozen_by":"doc","expires_in":"4h"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(diffSrc.frozeJob) != 1 || diffSrc.frozeJob[0] != "api-server" {
		t.Errorf("frozen jobs = %v, want [api-server]", diffSrc.frozeJob)
	}
	if diffSrc.frozeReason != "INC-4412: by hand" || diffSrc.frozeBy != "doc" {
		t.Errorf("reason/frozen_by = %q/%q", diffSrc.frozeReason, diffSrc.frozeBy)
	}
	// expires_in is resolved to an absolute instant before storage.
	exp, err := time.Parse(time.RFC3339, diffSrc.frozeExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", diffSrc.frozeExpiresAt, err)
	}
	if d := time.Until(exp); d < 3*time.Hour || d > 5*time.Hour {
		t.Errorf("expires_at is %v away, want about 4h", d)
	}
}

func TestAPIFreezeJob_DisabledReturns501(t *testing.T) {
	srv, diffSrc := freezeServer(t, false)
	rec := doReq(t, srv, http.MethodPost, "/api/v1/freezes/api-server", `{"reason":"x"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 when --enable-freeze-api is off", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "enable-freeze-api") {
		t.Errorf("the response should name the flag, got %s", rec.Body.String())
	}
	if len(diffSrc.frozeJob) != 0 {
		t.Errorf("nothing should have been written, got %v", diffSrc.frozeJob)
	}
}

func TestAPIFreezeJob_RequiresAReason(t *testing.T) {
	for _, body := range []string{`{}`, `{"reason":"  "}`, `{"expires_in":"1h"}`} {
		srv, diffSrc := freezeServer(t, true)
		rec := doReq(t, srv, http.MethodPost, "/api/v1/freezes/api-server", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
		if len(diffSrc.frozeJob) != 0 {
			t.Errorf("body %s: nothing should have been written", body)
		}
	}
}

func TestAPIFreezeJob_RejectsBadAndPastExpiries(t *testing.T) {
	tests := map[string]string{
		"unparseable expires_at": `{"reason":"x","expires_at":"next tuesday"}`,
		"past expires_at":        `{"reason":"x","expires_at":"2020-01-01T00:00:00Z"}`,
		"unparseable expires_in": `{"reason":"x","expires_in":"soon"}`,
		"negative expires_in":    `{"reason":"x","expires_in":"-1h"}`,
		"both given":             `{"reason":"x","expires_in":"1h","expires_at":"2099-01-01T00:00:00Z"}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			srv, diffSrc := freezeServer(t, true)
			rec := doReq(t, srv, http.MethodPost, "/api/v1/freezes/api-server", body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if len(diffSrc.frozeJob) != 0 {
				t.Error("nothing should have been written")
			}
		})
	}
}

func TestAPIFreezeJob_NoExpiryIsIndefinite(t *testing.T) {
	srv, diffSrc := freezeServer(t, true)
	rec := doReq(t, srv, http.MethodPost, "/api/v1/freezes/api-server", `{"reason":"waiting on a decision"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if diffSrc.frozeExpiresAt != "" {
		t.Errorf("want no expiry (indefinite), got %q", diffSrc.frozeExpiresAt)
	}
}

func TestAPIFreezeJob_DefaultTTLApplied(t *testing.T) {
	srv, diffSrc := freezeServer(t, true)
	diffSrc.freezeTTL = 2 * time.Hour

	rec := doReq(t, srv, http.MethodPost, "/api/v1/freezes/api-server", `{"reason":"short hold"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	exp, err := time.Parse(time.RFC3339, diffSrc.frozeExpiresAt)
	if err != nil {
		t.Fatalf("want --freeze-default-ttl applied, got %q", diffSrc.frozeExpiresAt)
	}
	if d := time.Until(exp); d < time.Hour || d > 3*time.Hour {
		t.Errorf("expires_at is %v away, want about 2h", d)
	}
}

func TestAPIFreezeJob_ExplicitExpiryBeatsDefaultTTL(t *testing.T) {
	srv, diffSrc := freezeServer(t, true)
	diffSrc.freezeTTL = 100 * time.Hour

	if rec := doReq(t, srv, http.MethodPost, "/api/v1/freezes/api-server", `{"reason":"x","expires_in":"30m"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	exp, _ := time.Parse(time.RFC3339, diffSrc.frozeExpiresAt)
	if d := time.Until(exp); d > 2*time.Hour {
		t.Errorf("expires_at is %v away, want the explicit 30m not the default TTL", d)
	}
}

func TestAPIFreezeJob_ConflictReturns409(t *testing.T) {
	srv, diffSrc := freezeServer(t, true)
	diffSrc.freezeErr = nomad.ErrFreezeConflict

	rec := doReq(t, srv, http.MethodPost, "/api/v1/freezes/api-server", `{"reason":"x"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 so the caller re-reads and retries", rec.Code)
	}
}

func TestAPIFreezeJob_RejectsOversizedReason(t *testing.T) {
	srv, _ := freezeServer(t, true)
	body := `{"reason":"` + strings.Repeat("x", 2000) + `"}`
	if rec := doReq(t, srv, http.MethodPost, "/api/v1/freezes/api-server", body); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an oversized reason", rec.Code)
	}
}

func TestAPIFreezeJob_RejectsGarbageBody(t *testing.T) {
	srv, _ := freezeServer(t, true)
	if rec := doReq(t, srv, http.MethodPost, "/api/v1/freezes/api-server", `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ── DELETE /api/v1/freezes/{job} ──────────────────────────────────────────────

func TestAPIThawJob_Thaws(t *testing.T) {
	srv, diffSrc := freezeServer(t, true, activeFreeze("api-server", "INC-4412"))
	rec := doReq(t, srv, http.MethodDelete, "/api/v1/freezes/api-server", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(diffSrc.thawedJob) != 1 || diffSrc.thawedJob[0] != "api-server" {
		t.Errorf("thawed = %v, want [api-server]", diffSrc.thawedJob)
	}
}

func TestAPIThawJob_DisabledReturns501(t *testing.T) {
	srv, diffSrc := freezeServer(t, false, activeFreeze("api-server", "x"))
	if rec := doReq(t, srv, http.MethodDelete, "/api/v1/freezes/api-server", ""); rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
	if len(diffSrc.thawedJob) != 0 {
		t.Error("nothing should have been thawed")
	}
}

func TestAPIThawJob_JobWithADotInItsName(t *testing.T) {
	// The path segment is the job ID as given; slugging happens in the store.
	srv, diffSrc := freezeServer(t, true)
	if rec := doReq(t, srv, http.MethodDelete, "/api/v1/freezes/api.server", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(diffSrc.thawedJob) != 1 || diffSrc.thawedJob[0] != "api.server" {
		t.Errorf("thawed = %v, want the exact job ID", diffSrc.thawedJob)
	}
}

// ── /healthz ──────────────────────────────────────────────────────────────────

func TestHealthz_ReportsFrozenJobs(t *testing.T) {
	srv, _ := freezeServer(t, false,
		activeFreeze("api-server", "INC-4412"),
		indefiniteFreeze("batch-reindex", "waiting on the schema"),
		expiredFreeze("old-worker", "last week"),
	)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp server.HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Only freezes in force count: an expired record suppresses nothing.
	if resp.FrozenCount != 2 {
		t.Errorf("frozen_count = %d, want 2", resp.FrozenCount)
	}
	states := map[string]string{}
	for _, f := range resp.Frozen {
		states[f.JobID] = f.State
	}
	if states["api-server"] != "active" || states["batch-reindex"] != "indefinite" {
		t.Errorf("states = %v", states)
	}
	if _, ok := states["old-worker"]; ok {
		t.Error("an expired record must not be listed as frozen")
	}
}

func TestHealthz_FreezeDoesNotHideDrift(t *testing.T) {
	// A freeze withholds writes; it does not make drift disappear, and the
	// status must still say so.
	srv, diffSrc := freezeServer(t, false, activeFreeze("api-server", "INC-4412"))
	diffSrc.diffs = []nomad.JobDiff{{
		JobID:       "api-server",
		DiffType:    nomad.DiffTypeModified,
		Detail:      "changed",
		ApplyAction: nomad.ApplyActionFrozen,
	}}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	var resp server.HealthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status != "diffs_detected" {
		t.Errorf("status = %q, want diffs_detected: a freeze does not hide drift", resp.Status)
	}
	if resp.DiffCount != 1 {
		t.Errorf("diff_count = %d, want 1", resp.DiffCount)
	}
	if len(resp.Diffs) != 1 || resp.Diffs[0].ApplyAction != "blocked_frozen" {
		t.Errorf("want the diff to carry apply_action=blocked_frozen, got %+v", resp.Diffs)
	}
}

func TestHealthz_NoFrozenFieldsWhenNothingIsFrozen(t *testing.T) {
	srv, _ := freezeServer(t, false)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "frozen") {
		t.Errorf("a clean cluster should not mention freezes, got %s", rec.Body.String())
	}
}

// ── The console ───────────────────────────────────────────────────────────────

func getIndex(t *testing.T, srv *server.Server) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestIndex_ShowsFrozenJobs(t *testing.T) {
	srv, _ := freezeServer(t, false, activeFreeze("api-server", "INC-4412: migration by hand"))
	body := getIndex(t, srv)

	for _, want := range []string{"Frozen: 1 job(s)", "api-server", "INC-4412: migration by hand", "expires in"} {
		if !strings.Contains(body, want) {
			t.Errorf("console should contain %q", want)
		}
	}
}

func TestIndex_CallsOutIndefiniteFreezes(t *testing.T) {
	srv, _ := freezeServer(t, false, indefiniteFreeze("batch-reindex", "waiting on the schema"))
	body := getIndex(t, srv)

	if !strings.Contains(body, "no expiry — indefinite") {
		t.Error("an indefinite freeze should be called out as such")
	}
	if !strings.Contains(body, `class="bad"`) {
		t.Error("an indefinite freeze should render as a problem, not a notice")
	}
}

func TestIndex_ShowsExpiredRecordsSeparately(t *testing.T) {
	srv, _ := freezeServer(t, false, expiredFreeze("old-worker", "last week"))
	body := getIndex(t, srv)

	if !strings.Contains(body, "Expired freeze records") {
		t.Error("expired records should get their own section: they are left behind, not holding anything")
	}
	if strings.Contains(body, "Frozen: 1 job(s)") {
		t.Error("an expired record must not be counted as frozen")
	}
	// The console tells the operator how to clear it.
	if !strings.Contains(body, "nomad var purge") {
		t.Error("the console should say how to purge an expired record")
	}
}

func TestIndex_ReportsAFreezeStoreThatWasNeverReadable(t *testing.T) {
	// Never once readable: freezes are not honoured and applies are NOT
	// held, so the console has to say that rather than claim a hold.
	srv, diffSrc := freezeServer(t, false)
	diffSrc.freezeHealthy = false
	diffSrc.freezeRead = time.Time{}
	body := getIndex(t, srv)

	if !strings.Contains(body, "Freeze store:") || !strings.Contains(body, "NOT being honoured") {
		t.Errorf("a never-readable freeze store should say freezes do not work, got %s", body)
	}
	if strings.Contains(body, "every apply is being held") {
		t.Error("applies are not held when the store was never readable; the console must not claim they are")
	}
}

func TestIndex_ReportsAFreezeStoreThatStoppedWorking(t *testing.T) {
	srv, diffSrc := freezeServer(t, false)
	diffSrc.freezeHealthy = false
	diffSrc.freezeRead = time.Now().Add(-time.Minute)
	body := getIndex(t, srv)

	if !strings.Contains(body, "Freeze store:") || !strings.Contains(body, "Applies are held") {
		t.Errorf("a store that stopped working should say applies are held, got %s", body)
	}
}

func TestIndex_ReportsAStaleFreezeView(t *testing.T) {
	srv, diffSrc := freezeServer(t, false)
	diffSrc.freezeRead = time.Now().Add(-30 * time.Minute)
	body := getIndex(t, srv)

	if !strings.Contains(body, "freeze-max-staleness") {
		t.Errorf("a stale freeze view should name the flag, got %s", body)
	}
}

func TestIndex_StaleViewWithFailOpenSaysAppliesProceed(t *testing.T) {
	cfg := &config.Config{
		ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main",
		FreezeMaxStaleness: time.Minute, FreezeFailOpen: true,
	}
	diffSrc := &mockDiffSource{
		lastCheck: time.Now(), lastCommit: "abc",
		freezeEnabled: true, freezeHealthy: false,
		// Read once, then stopped working: the case that actually holds
		// applies, and so the case --freeze-fail-open applies to.
		freezeRead: time.Now().Add(-time.Minute),
	}
	gitSrc := &mockGitSource{lastCommit: "abc", lastUpdate: time.Now()}
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "t"}, prometheus.NewRegistry())

	if body := getIndex(t, srv); !strings.Contains(body, "freeze-fail-open") {
		t.Errorf("with --freeze-fail-open the console should say applies proceed, got %s", body)
	}
}

func TestIndex_CleanWhenNothingIsFrozen(t *testing.T) {
	srv, _ := freezeServer(t, false)
	body := getIndex(t, srv)

	if strings.Contains(body, "Frozen:") || strings.Contains(body, "Expired freeze records") {
		t.Error("a clean cluster should show no freeze sections at all")
	}
	if strings.Contains(body, "Freeze store:") {
		t.Error("a healthy freeze store should not be mentioned")
	}
}

func TestIndex_ShowsOldestPendingUpdateAge(t *testing.T) {
	// A count of pending updates says nothing on its own; the age of the
	// oldest is what distinguishes a busy applier from a stuck one.
	srv, diffSrc := freezeServer(t, false)
	diffSrc.updates = []nomad.JobUpdate{{
		UpdateID:   "api-server/abc1234",
		JobID:      "api-server",
		Operation:  nomad.JobUpdateOperationRegister,
		Status:     nomad.JobUpdateStatusPending,
		DetectedAt: time.Now().Add(-22 * time.Minute).UTC().Format(time.RFC3339),
	}}

	body := getIndex(t, srv)
	if !strings.Contains(body, "1 update(s) pending") {
		t.Error("the pending count should still be shown")
	}
	if !strings.Contains(body, "oldest 22m old") {
		t.Errorf("the age of the oldest pending update should be shown, got %s", body)
	}
}

func TestIndex_NoFreezeSectionsWhenSupportDisabled(t *testing.T) {
	srv, diffSrc := freezeServer(t, false, activeFreeze("api-server", "x"))
	diffSrc.freezeEnabled = false

	if body := getIndex(t, srv); strings.Contains(body, "Frozen:") {
		t.Error("with freeze support off the console should show no freeze section")
	}
}

// ── OpenAPI ───────────────────────────────────────────────────────────────────

func TestOpenAPISpec_DocumentsFreezeEndpoints(t *testing.T) {
	srv, _ := freezeServer(t, true)
	req := httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("the spec must be valid JSON: %v", err)
	}
	if _, ok := spec.Paths["/freezes"]; !ok {
		t.Error("/freezes should be documented")
	}
	byJob, ok := spec.Paths["/freezes/{job}"]
	if !ok {
		t.Fatal("/freezes/{job} should be documented")
	}
	for _, method := range []string{"post", "delete"} {
		if _, ok := byJob[method]; !ok {
			t.Errorf("/freezes/{job} should document %s", method)
		}
	}
	// The new apply_action and update status have to be in the enums, or
	// clients validating against the spec will reject real responses.
	body := rec.Body.String()
	for _, want := range []string{"blocked_frozen", "HELD"} {
		if !strings.Contains(body, want) {
			t.Errorf("the spec should mention %q", want)
		}
	}
}
