//go:build regression

package regression

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gerrowadat/nomad-gitops/internal/nomad"
)

// ── Freezes ───────────────────────────────────────────────────────────────────
//
// A freeze lives in a real Nomad Variable, so these tests are the ones that
// prove the mechanism against the actual Variables API rather than a stub:
// that a record written with the Variables client is seen and honoured, that
// an expired record is not, and that the write API produces a Variable Nomad
// itself can read back.

const freezeTestPrefix = "nomad/jobs/gitops/freezes"

// freezeArgs are the common fast-cycle arguments for freeze tests, with
// applies enabled so a held apply is actually observable.
func freezeArgs(repoURL, branch, glob string, extra ...string) []string {
	args := []string{
		"--repo-url=" + repoURL,
		"--branch=" + branch,
		"--job-selector-glob=" + glob,
		"--apply-interval=1s",
		"--default-update-policy=full",
		"--freeze-variable-prefix=" + freezeTestPrefix,
	}
	return append(args, extra...)
}

// putFreeze writes a freeze record straight into Nomad Variables, the way an
// operator would with `nomad var put`. expiresAt may be empty (indefinite).
func putFreeze(t *testing.T, jobID, reason, expiresAt string) {
	t.Helper()
	v := nomadapi.NewVariable(freezeTestPrefix + "/" + jobID)
	v.Items["reason"] = reason
	if expiresAt != "" {
		v.Items["expires_at"] = expiresAt
	}
	if _, _, err := testNomadClient.Variables().Create(v, &nomadapi.WriteOptions{}); err != nil {
		t.Fatalf("writing freeze variable for %s: %v", jobID, err)
	}
	t.Cleanup(func() {
		_, _ = testNomadClient.Variables().Delete(freezeTestPrefix+"/"+jobID, &nomadapi.WriteOptions{})
	})
}

// purgeFreeze removes a freeze record, the way `nomad var purge` would.
func purgeFreeze(t *testing.T, jobID string) {
	t.Helper()
	if _, err := testNomadClient.Variables().Delete(freezeTestPrefix+"/"+jobID, &nomadapi.WriteOptions{}); err != nil {
		t.Fatalf("purging freeze variable for %s: %v", jobID, err)
	}
}

// readFreezeVar reads a freeze record's items back from Nomad, or nil if the
// variable does not exist.
func readFreezeVar(t *testing.T, jobID string) map[string]string {
	t.Helper()
	v, _, err := testNomadClient.Variables().Peek(freezeTestPrefix+"/"+jobID, &nomadapi.QueryOptions{})
	if err != nil {
		t.Fatalf("peeking freeze variable for %s: %v", jobID, err)
	}
	if v == nil {
		return nil
	}
	return v.Items
}

// fetchFreezes retrieves and decodes /api/v1/freezes.
func fetchFreezes(t *testing.T, baseURL, apiKey string) []nomad.FreezeRecord {
	t.Helper()
	var out struct {
		Freezes      []nomad.FreezeRecord `json:"freezes"`
		StoreHealthy bool                 `json:"store_healthy"`
	}
	apiGet(t, baseURL+"/api/v1/freezes", apiKey, &out)
	if !out.StoreHealthy {
		t.Error("store_healthy is false: nomad-gitops cannot read the freeze store")
	}
	return out.Freezes
}

// diffActionFor returns the apply_action for a job from /api/v1/diffs.
func diffActionFor(t *testing.T, baseURL, apiKey, jobID string) nomad.ApplyAction {
	t.Helper()
	var out struct {
		Diffs []nomad.JobDiff `json:"diffs"`
	}
	apiGet(t, baseURL+"/api/v1/diffs", apiKey, &out)
	for _, d := range out.Diffs {
		if d.JobID == jobID {
			return d.ApplyAction
		}
	}
	return ""
}

// TestFreezeE2E_HoldsApplyUntilThawed is the headline test: a freeze written
// as a plain Nomad Variable stops a job that would otherwise converge, and
// purging it lets the next cycle converge with nothing remembered in between.
func TestFreezeE2E_HoldsApplyUntilThawed(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	jobID := uniqueJobID("freeze-hold")

	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 60*time.Second)

	// Git wants args ["999"] under policy full: without a freeze this
	// converges within a cycle or two.
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLWithPolicy(jobID, "full"),
	})

	putFreeze(t, jobID, "regression test: holding the apply",
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339))

	apiKey := "freeze-hold-key-" + randomSuffix()
	baseURL := startGitopsWithAPI(t, apiKey, freezeArgs(repoURL, branch, jobID)...)

	// Let several diff + apply cycles pass.
	time.Sleep(8 * time.Second)

	if args := jobTaskArgs(t, jobID); len(args) != 1 || args[0] != "600" {
		t.Fatalf("a frozen job must not be modified; args are now %v", args)
	}
	// The drift is still detected and reported — a freeze withholds writes,
	// it does not hide drift — and it explains itself.
	if got := diffActionFor(t, baseURL, apiKey, jobID); got != nomad.ApplyActionFrozen {
		t.Errorf("apply_action = %q, want %q", got, nomad.ApplyActionFrozen)
	}
	if updates := fetchUpdates(t, baseURL, apiKey); len(updates) != 0 {
		t.Errorf("a frozen job should enqueue no updates at all, got %+v", updates)
	}
	freezes := fetchFreezes(t, baseURL, apiKey)
	if len(freezes) != 1 || freezes[0].JobID != jobID || freezes[0].State != nomad.FreezeStateActive {
		t.Errorf("GET /api/v1/freezes = %+v, want one active record for %s", freezes, jobID)
	}

	// Thaw, and the next cycle re-derives the same intent and applies it.
	purgeFreeze(t, jobID)
	waitForTaskArgs(t, jobID, "999", 60*time.Second)
}

// TestFreezeE2E_ExpiredRecordDoesNotHold verifies that an expired record
// suppresses nothing — the job reconciles normally again with no intervention
// — while the record itself is left in place to be reported.
func TestFreezeE2E_ExpiredRecordDoesNotHold(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	jobID := uniqueJobID("freeze-expired")

	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 60*time.Second)
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLWithPolicy(jobID, "full"),
	})

	putFreeze(t, jobID, "regression test: already expired",
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))

	// Guard against a vacuous pass: the job must start out un-converged.
	if args := jobTaskArgs(t, jobID); len(args) != 1 || args[0] != "600" {
		t.Fatalf("precondition: job should start with args [600], got %v", args)
	}

	apiKey := "freeze-expired-key-" + randomSuffix()
	baseURL := startGitopsWithAPI(t, apiKey, freezeArgs(repoURL, branch, jobID)...)

	waitForTaskArgs(t, jobID, "999", 60*time.Second)

	// The record is kept, not deleted: it is the only record of why the job
	// was frozen, and it is reported until someone purges it.
	if items := readFreezeVar(t, jobID); items == nil {
		t.Error("an expired freeze record must not be deleted by default")
	}
	freezes := fetchFreezes(t, baseURL, apiKey)
	if len(freezes) != 1 || freezes[0].State != nomad.FreezeStateExpired {
		t.Errorf("GET /api/v1/freezes = %+v, want one expired record", freezes)
	}
}

// TestFreezeE2E_IndefiniteRecordHolds covers a record with no expires_at:
// permitted, and holds writes until purged.
func TestFreezeE2E_IndefiniteRecordHolds(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	jobID := uniqueJobID("freeze-indef")

	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 60*time.Second)
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLWithPolicy(jobID, "full"),
	})

	putFreeze(t, jobID, "regression test: no expiry", "")

	apiKey := "freeze-indef-key-" + randomSuffix()
	baseURL := startGitopsWithAPI(t, apiKey, freezeArgs(repoURL, branch, jobID)...)
	time.Sleep(8 * time.Second)

	if args := jobTaskArgs(t, jobID); len(args) != 1 || args[0] != "600" {
		t.Fatalf("an indefinite freeze must hold the apply; args are now %v", args)
	}
	freezes := fetchFreezes(t, baseURL, apiKey)
	if len(freezes) != 1 || freezes[0].State != nomad.FreezeStateIndefinite {
		t.Errorf("GET /api/v1/freezes = %+v, want one indefinite record", freezes)
	}
	if freezes[0].ExpiresAt != "" {
		t.Errorf("an indefinite record should report no expires_at, got %q", freezes[0].ExpiresAt)
	}
}

// TestFreezeE2E_MalformedRecordHolds verifies the conservative reading: a
// record with no reason cannot be trusted as a well-formed freeze, and is
// held rather than ignored.
func TestFreezeE2E_MalformedRecordHolds(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	jobID := uniqueJobID("freeze-bad")

	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 60*time.Second)
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLWithPolicy(jobID, "full"),
	})

	// A record with an unparseable expiry and no reason.
	v := nomadapi.NewVariable(freezeTestPrefix + "/" + jobID)
	v.Items["expires_at"] = "next tuesday"
	if _, _, err := testNomadClient.Variables().Create(v, &nomadapi.WriteOptions{}); err != nil {
		t.Fatalf("writing malformed freeze variable: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testNomadClient.Variables().Delete(freezeTestPrefix+"/"+jobID, &nomadapi.WriteOptions{})
	})

	apiKey := "freeze-bad-key-" + randomSuffix()
	baseURL := startGitopsWithAPI(t, apiKey, freezeArgs(repoURL, branch, jobID)...)
	time.Sleep(8 * time.Second)

	if args := jobTaskArgs(t, jobID); len(args) != 1 || args[0] != "600" {
		t.Fatalf("a malformed freeze record must hold the apply; args are now %v", args)
	}
	freezes := fetchFreezes(t, baseURL, apiKey)
	if len(freezes) != 1 || freezes[0].State != nomad.FreezeStateMalformed {
		t.Errorf("GET /api/v1/freezes = %+v, want one malformed record", freezes)
	}
}

// TestFreezeE2E_WriteAPICreatesARealVariable drives the gated write endpoints
// and checks Nomad itself holds the resulting record.
func TestFreezeE2E_WriteAPICreatesARealVariable(t *testing.T) {
	repoURL, _, branch := createGitRepo(t)
	jobID := uniqueJobID("freeze-api")

	apiKey := "freeze-api-key-" + randomSuffix()
	baseURL := startGitopsWithAPI(t, apiKey,
		freezeArgs(repoURL, branch, jobID, "--enable-freeze-api")...)

	body, _ := json.Marshal(map[string]string{
		"reason":     "regression test: via the API",
		"frozen_by":  "regression",
		"expires_in": "1h",
	})
	rec := freezeAPIReq(t, http.MethodPost, baseURL+"/api/v1/freezes/"+jobID, apiKey, body)
	if rec != http.StatusOK {
		t.Fatalf("POST /api/v1/freezes/%s: status %d, want 200", jobID, rec)
	}
	t.Cleanup(func() {
		_, _ = testNomadClient.Variables().Delete(freezeTestPrefix+"/"+jobID, &nomadapi.WriteOptions{})
	})

	// Nomad holds the record, with the items the design specifies.
	items := readFreezeVar(t, jobID)
	if items == nil {
		t.Fatal("the write API should have created a Nomad Variable")
	}
	if items["reason"] != "regression test: via the API" {
		t.Errorf("reason item = %q", items["reason"])
	}
	if items["frozen_by"] != "regression" {
		t.Errorf("frozen_by item = %q", items["frozen_by"])
	}
	if _, err := time.Parse(time.RFC3339, items["expires_at"]); err != nil {
		t.Errorf("expires_at item %q should be RFC3339: %v", items["expires_at"], err)
	}
	// A path-safe job ID needs no job item.
	if _, ok := items["job"]; ok {
		t.Errorf("a path-safe job ID should not need a job item, got %q", items["job"])
	}

	// And the thaw removes it.
	if rec := freezeAPIReq(t, http.MethodDelete, baseURL+"/api/v1/freezes/"+jobID, apiKey, nil); rec != http.StatusOK {
		t.Fatalf("DELETE /api/v1/freezes/%s: status %d, want 200", jobID, rec)
	}
	if items := readFreezeVar(t, jobID); items != nil {
		t.Errorf("the thaw should have removed the Variable, still got %v", items)
	}
}

// TestFreezeE2E_WriteAPIDisabledByDefault verifies the second opt-in: the API
// key alone does not let a caller change cluster-visible state.
func TestFreezeE2E_WriteAPIDisabledByDefault(t *testing.T) {
	repoURL, _, branch := createGitRepo(t)
	jobID := uniqueJobID("freeze-nowrite")

	apiKey := "freeze-nowrite-key-" + randomSuffix()
	baseURL := startGitopsWithAPI(t, apiKey, freezeArgs(repoURL, branch, jobID)...)

	body := []byte(`{"reason":"should not be written"}`)
	if rec := freezeAPIReq(t, http.MethodPost, baseURL+"/api/v1/freezes/"+jobID, apiKey, body); rec != http.StatusNotImplemented {
		t.Errorf("POST without --enable-freeze-api: status %d, want 501", rec)
	}
	if items := readFreezeVar(t, jobID); items != nil {
		t.Errorf("nothing should have been written to Nomad, got %v", items)
	}

	// Reading is still available.
	if freezes := fetchFreezes(t, baseURL, apiKey); len(freezes) != 0 {
		t.Errorf("want no freeze records, got %+v", freezes)
	}
}

// TestFreezeE2E_UnslugabbleJobID covers a job ID that cannot be a Variable
// path segment: the path holds the slug and the exact ID travels in a "job"
// item, and the freeze still applies to the right job.
func TestFreezeE2E_UnslugabbleJobID(t *testing.T) {
	repoURL, _, branch := createGitRepo(t)
	// Nomad job IDs may contain '.'; Variable paths may not.
	jobID := uniqueJobID("freeze-dot") + ".v2"
	slug := nomad.FreezeSlug(jobID)

	apiKey := "freeze-dot-key-" + randomSuffix()
	baseURL := startGitopsWithAPI(t, apiKey,
		freezeArgs(repoURL, branch, "freeze-dot*", "--enable-freeze-api")...)

	body := []byte(`{"reason":"regression test: dotted job ID"}`)
	if rec := freezeAPIReq(t, http.MethodPost, baseURL+"/api/v1/freezes/"+jobID, apiKey, body); rec != http.StatusOK {
		t.Fatalf("POST /api/v1/freezes/%s: status %d, want 200", jobID, rec)
	}
	t.Cleanup(func() {
		_, _ = testNomadClient.Variables().Delete(freezeTestPrefix+"/"+slug, &nomadapi.WriteOptions{})
	})

	v, _, err := testNomadClient.Variables().Peek(freezeTestPrefix+"/"+slug, &nomadapi.QueryOptions{})
	if err != nil || v == nil {
		t.Fatalf("expected a Variable at the slugged path %s: %v", slug, err)
	}
	if v.Items["job"] != jobID {
		t.Errorf("job item = %q, want the exact job ID %q", v.Items["job"], jobID)
	}

	// nomad-gitops reads the exact ID back out of the job item.
	freezes := fetchFreezes(t, baseURL, apiKey)
	if len(freezes) != 1 || freezes[0].JobID != jobID {
		t.Errorf("GET /api/v1/freezes = %+v, want one record for %q", freezes, jobID)
	}
}

// freezeAPIReq makes an authenticated request to a freeze endpoint and
// returns the status code.
func freezeAPIReq(t *testing.T, method, url, apiKey string, body []byte) int {
	t.Helper()
	var req *http.Request
	var err error
	if body == nil {
		req, err = http.NewRequest(method, url, nil)
	} else {
		req, err = http.NewRequest(method, url, bytes.NewReader(body))
		if req != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		t.Fatalf("building %s %s: %v", method, url, err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
