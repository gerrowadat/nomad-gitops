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

	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

// ── GitOps apply (job mutation) ───────────────────────────────────────────────
//
// These tests run the real binary against the real cluster and verify the
// apply side: drift is corrected only when the policy and flags allow it,
// and never otherwise. Fast intervals keep convergence under a few seconds.

// applyArgs are the common fast-cycle arguments for apply tests.
func applyArgs(repoURL, branch, glob string, extra ...string) []string {
	args := []string{
		"--repo-url=" + repoURL,
		"--branch=" + branch,
		"--job-selector-glob=" + glob,
		"--apply-interval=1s",
	}
	return append(args, extra...)
}

// testJobHCLWithPolicy is testJobHCLModified plus an update-policy meta key.
func testJobHCLWithPolicy(jobID, policy string) string {
	return fmt.Sprintf(`
job %q {
  datacenters = ["dc1"]
  type        = "service"

  meta {
    gitops_managed       = "true"
    gitops_update_policy = %q
  }

  group "main" {
    count = 1

    task "sleep" {
      driver = "raw_exec"
      config {
        command = "/bin/sleep"
        args    = ["999"]
      }
      resources {
        cpu    = 10
        memory = 16
      }
    }
  }
}
`, jobID, policy)
}

// jobTaskArgs returns the first task's config args for jobID, or nil if the
// job does not exist.
func jobTaskArgs(t *testing.T, jobID string) []interface{} {
	t.Helper()
	job, _, err := testNomadClient.Jobs().Info(jobID, &nomadapi.QueryOptions{})
	if err != nil || job == nil {
		return nil
	}
	if len(job.TaskGroups) == 0 || len(job.TaskGroups[0].Tasks) == 0 {
		return nil
	}
	args, _ := job.TaskGroups[0].Tasks[0].Config["args"].([]interface{})
	return args
}

// waitForTaskArgs polls until the job's first task args equal want.
func waitForTaskArgs(t *testing.T, jobID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		args := jobTaskArgs(t, jobID)
		if len(args) == 1 && args[0] == want {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("job %s task args did not become [%s] within %v (last seen: %v)",
		jobID, want, timeout, jobTaskArgs(t, jobID))
}

// TestApplyE2E_FullPolicyViaMeta_Converges registers a job, drifts Git ahead
// of it, and verifies the binary re-registers the job to match HCL when the
// job's meta declares gitops_update_policy = "full" — even though the
// deployment default remains "none".
func TestApplyE2E_FullPolicyViaMeta_Converges(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	jobID := uniqueJobID("apply-full")

	// Nomad runs the job with args ["600"] (no policy meta needed live —
	// policy is read from the HCL side).
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 60*time.Second)

	// Git wants args ["999"] and declares policy full.
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLWithPolicy(jobID, "full"),
	})

	startBotherer(t, applyArgs(repoURL, branch, jobID)...)

	waitForTaskArgs(t, jobID, "999", 60*time.Second)
}

// TestApplyE2E_DefaultPolicyNone_NeverWrites is the critical negative test:
// with everything at defaults, drift is reported but the cluster is never
// touched.
func TestApplyE2E_DefaultPolicyNone_NeverWrites(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	jobID := uniqueJobID("apply-none")

	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 60*time.Second)

	// Git drifts ahead, but no policy meta and the default policy is none.
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLModified(jobID),
	})

	apiKey := "apply-none-key-" + randomSuffix()
	baseURL := startBothererWithAPI(t, apiKey, applyArgs(repoURL, branch, jobID)...)

	// Let several diff + apply cycles pass.
	time.Sleep(8 * time.Second)

	if args := jobTaskArgs(t, jobID); len(args) != 1 || args[0] != "600" {
		t.Fatalf("default policy none must not modify the job; args are now %v", args)
	}

	// The drift is still surfaced, and the update queue is empty.
	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "diffs_detected") {
		t.Errorf("drift should still be reported, got %s", body)
	}

	updates := fetchUpdates(t, baseURL, apiKey)
	if len(updates) != 0 {
		t.Errorf("no updates should exist under policy none, got %+v", updates)
	}
}

// TestApplyE2E_DefaultPolicyFullFlag_Converges drives the same convergence
// through the --default-update-policy flag instead of job meta, and checks
// the update queue records the SUCCEEDED apply.
func TestApplyE2E_DefaultPolicyFullFlag_Converges(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	jobID := uniqueJobID("apply-flag")

	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 60*time.Second)

	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLModified(jobID),
	})

	apiKey := "apply-flag-key-" + randomSuffix()
	baseURL := startBothererWithAPI(t, apiKey,
		applyArgs(repoURL, branch, jobID, "--default-update-policy=full")...)

	waitForTaskArgs(t, jobID, "999", 60*time.Second)

	// The queue should show the update as SUCCEEDED with a CAS token.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		updates := fetchUpdates(t, baseURL, apiKey)
		for _, u := range updates {
			if u.JobID == jobID && u.Status == nomad.JobUpdateStatusSucceeded {
				if u.Operation != nomad.JobUpdateOperationRegister {
					t.Errorf("operation: want REGISTER, got %s", u.Operation)
				}
				if u.NomadJobModifyIndex == 0 {
					t.Error("successful update should record the post-apply ModifyIndex")
				}
				if u.AppliedAt == "" {
					t.Error("AppliedAt should be set")
				}
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("no SUCCEEDED update appeared in /api/v1/updates")
}

// TestApplyE2E_JobCreation_GatedByFlag verifies that a job present in Git but
// absent from Nomad is only registered when --enable-job-creation is set.
func TestApplyE2E_JobCreation_GatedByFlag(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		repoURL, workDir, branch := createGitRepo(t)
		jobID := uniqueJobID("create-off")

		commitToGit(t, workDir, map[string]string{
			jobID + ".hcl": testJobHCL(jobID),
		})

		startBotherer(t, applyArgs(repoURL, branch, jobID, "--default-update-policy=full")...)

		time.Sleep(8 * time.Second)

		if _, _, err := testNomadClient.Jobs().Info(jobID, &nomadapi.QueryOptions{}); err == nil {
			t.Cleanup(func() { deregisterJob(t, jobID, true) })
			t.Fatal("job must not be created while --enable-job-creation is off")
		}
	})

	t.Run("enabled", func(t *testing.T) {
		repoURL, workDir, branch := createGitRepo(t)
		jobID := uniqueJobID("create-on")
		t.Cleanup(func() { deregisterJob(t, jobID, true) })

		commitToGit(t, workDir, map[string]string{
			jobID + ".hcl": testJobHCL(jobID),
		})

		startBotherer(t, applyArgs(repoURL, branch, jobID,
			"--default-update-policy=full", "--enable-job-creation")...)

		waitForJobStatus(t, jobID, "running", 60*time.Second)
	})
}

// TestApplyE2E_ImageOnlyPolicy_BlocksNonImageChange verifies that a job with
// gitops_update_policy = "image-only" does not have a non-image change
// (different task args) applied; raw_exec jobs have no image field, so any
// drift is by definition not image-only.
func TestApplyE2E_ImageOnlyPolicy_BlocksNonImageChange(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	jobID := uniqueJobID("apply-imgonly")

	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 60*time.Second)

	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLWithPolicy(jobID, "image-only"),
	})

	startBotherer(t, applyArgs(repoURL, branch, jobID)...)

	time.Sleep(8 * time.Second)

	if args := jobTaskArgs(t, jobID); len(args) != 1 || args[0] != "600" {
		t.Fatalf("image-only policy must block a non-image change; args are now %v", args)
	}
}

// fetchUpdates retrieves and decodes /api/v1/updates.
func fetchUpdates(t *testing.T, baseURL, apiKey string) []nomad.JobUpdate {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/updates", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/updates: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/updates: status %d", resp.StatusCode)
	}
	var out struct {
		Updates []nomad.JobUpdate `json:"updates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding updates: %v", err)
	}
	return out.Updates
}

// TestApplyE2E_OptInViaGitCommit_Converges is the git-is-intent scenario: a
// job is already running with no gitops keys, and a commit adds
// gitops_managed + an update policy to its HCL. No glob selection is
// configured — the HCL meta key alone must select the job, the missing key
// on the live job is drift, and applying converges the live meta too.
func TestApplyE2E_OptInViaGitCommit_Converges(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	jobID := uniqueJobID("optin-commit")

	// Live job: no gitops keys at all.
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 60*time.Second)

	// The commit opts the job in and declares policy full.
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLWithPolicy(jobID, "full"),
	})

	// Deliberately no --job-selector-glob: meta-only selection.
	startBotherer(t,
		"--repo-url="+repoURL,
		"--branch="+branch,
		"--apply-interval=1s",
	)

	waitForTaskArgs(t, jobID, "999", 60*time.Second)

	// The live job's meta must have converged to carry the keys.
	job, _, err := testNomadClient.Jobs().Info(jobID, &nomadapi.QueryOptions{})
	if err != nil {
		t.Fatalf("Info after convergence: %v", err)
	}
	if job.Meta["gitops_managed"] != "true" {
		t.Errorf("live job should now carry gitops_managed=true, got %v", job.Meta)
	}
	if job.Meta["gitops_update_policy"] != "full" {
		t.Errorf("live job should now carry the policy key, got %v", job.Meta)
	}
}

// TestApplyE2E_MetaOnlyChange_LeavesJobAloneByDefault verifies that a commit
// adding only nomad-botherer's own meta keys to a running job is not applied
// and not counted as drift by default — the disruptive re-register is
// avoided and no alert fires. This also exercises the real Nomad plan-diff
// format for meta against the classifier.
func TestApplyE2E_MetaOnlyChange_LeavesJobAloneByDefault(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	jobID := uniqueJobID("meta-only")

	// Running job: args ["999"], no gitops meta.
	registerJobHCL(t, testJobHCLModified(jobID))
	waitForJobStatus(t, jobID, "running", 60*time.Second)

	// Commit adds only the gitops meta keys; the task args stay ["999"], so
	// the only difference is the meta.
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLWithPolicy(jobID, "full"),
	})

	apiKey := "meta-only-key-" + randomSuffix()
	baseURL := startBothererWithAPI(t, apiKey, applyArgs(repoURL, branch, jobID)...)

	time.Sleep(8 * time.Second)

	job, _, err := testNomadClient.Jobs().Info(jobID, &nomadapi.QueryOptions{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if job.Meta["gitops_managed"] == "true" {
		t.Error("a meta-only change must not be applied by default; the live meta was converged")
	}

	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "diffs_detected") {
		t.Errorf("a meta-only change must not be counted as drift, got %s", body)
	}
	if updates := fetchUpdates(t, baseURL, apiKey); len(updates) != 0 {
		t.Errorf("a meta-only change must not enqueue updates, got %+v", updates)
	}
}

// TestApplyE2E_MetaOnlyChange_AppliedWithFlag verifies the opt-in: with
// --apply-meta-only-changes the live meta converges.
func TestApplyE2E_MetaOnlyChange_AppliedWithFlag(t *testing.T) {
	repoURL, workDir, branch := createGitRepo(t)
	jobID := uniqueJobID("meta-only-apply")

	registerJobHCL(t, testJobHCLModified(jobID))
	waitForJobStatus(t, jobID, "running", 60*time.Second)
	commitToGit(t, workDir, map[string]string{
		jobID + ".hcl": testJobHCLWithPolicy(jobID, "full"),
	})

	startBotherer(t, applyArgs(repoURL, branch, jobID, "--apply-meta-only-changes")...)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		job, _, err := testNomadClient.Jobs().Info(jobID, &nomadapi.QueryOptions{})
		if err == nil && job.Meta["gitops_managed"] == "true" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Error("with --apply-meta-only-changes the live meta should converge to carry gitops_managed")
}

// TestApplyE2E_ExistingDrift reproduces the "opt a drifted job in" scenario:
// a running job already differs from its HCL (args), then the gitops meta tag
// is added by a later commit while nomad-botherer is watching. By default the
// pre-existing drift is not applied; with --apply-existing-drift it is.
func TestApplyE2E_ExistingDrift(t *testing.T) {
	run := func(t *testing.T, applyExisting bool) (jobID, baseURL string) {
		repoURL, workDir, branch := createGitRepo(t)
		jobID = uniqueJobID("existing-drift")

		// Running job: args ["600"], no gitops keys.
		registerJobHCL(t, testJobHCL(jobID))
		waitForJobStatus(t, jobID, "running", 60*time.Second)

		// Commit 1: HCL drifts the args to ["999"], still no gitops keys, so
		// the job is not yet in scope (no glob configured).
		commitToGit(t, workDir, map[string]string{jobID + ".hcl": testJobHCLModified(jobID)})

		extra := []string{"--repo-url=" + repoURL, "--branch=" + branch, "--apply-interval=1s", "--poll-interval=1s"}
		if applyExisting {
			extra = append(extra, "--apply-existing-drift")
		}
		baseURL = startBotherer(t, extra...)

		// Let it observe the out-of-scope job for a couple of cycles.
		time.Sleep(3 * time.Second)

		// Commit 2: add the gitops tag while running — the job enters scope
		// with the args drift already present.
		commitToGit(t, workDir, map[string]string{jobID + ".hcl": testJobHCLWithPolicy(jobID, "full")})
		return jobID, baseURL
	}

	t.Run("default leaves pre-existing drift alone", func(t *testing.T) {
		jobID, _ := run(t, false)
		time.Sleep(8 * time.Second)
		if args := jobTaskArgs(t, jobID); len(args) != 1 || args[0] != "600" {
			t.Errorf("pre-existing drift must not be applied on opt-in by default; args are now %v", args)
		}
	})

	t.Run("flag applies pre-existing drift", func(t *testing.T) {
		jobID, _ := run(t, true)
		waitForTaskArgs(t, jobID, "999", 60*time.Second)
	})
}

// TestApplyE2E_ExistingDrift_AtStartup verifies the git-history-based gate
// works when nomad-botherer starts up already pointing at the opt-in commit
// (it did not witness the tag being added). The job ran drifted, the drift and
// then the tag were committed, and only then is the binary started.
func TestApplyE2E_ExistingDrift_AtStartup(t *testing.T) {
	run := func(t *testing.T, applyExisting bool) string {
		repoURL, workDir, branch := createGitRepo(t)
		jobID := uniqueJobID("existing-startup")

		// Running job: args ["600"], no gitops keys.
		registerJobHCL(t, testJobHCL(jobID))
		waitForJobStatus(t, jobID, "running", 60*time.Second)

		// Commit 1: drift the args, still no tag.
		commitToGit(t, workDir, map[string]string{jobID + ".hcl": testJobHCLModified(jobID)})
		// Commit 2: add the tag (opt-in). HEAD's parent (commit 1) lacks the tag.
		commitToGit(t, workDir, map[string]string{jobID + ".hcl": testJobHCLWithPolicy(jobID, "full")})

		// Only now start the binary — it never witnessed the opt-in.
		extra := []string{"--repo-url=" + repoURL, "--branch=" + branch, "--apply-interval=1s"}
		if applyExisting {
			extra = append(extra, "--apply-existing-drift")
		}
		startBotherer(t, extra...)
		return jobID
	}

	t.Run("default leaves pre-existing drift alone on startup", func(t *testing.T) {
		jobID := run(t, false)
		time.Sleep(8 * time.Second)
		if args := jobTaskArgs(t, jobID); len(args) != 1 || args[0] != "600" {
			t.Errorf("startup pre-existing drift must not apply by default; args are now %v", args)
		}
	})

	t.Run("flag applies pre-existing drift on startup", func(t *testing.T) {
		jobID := run(t, true)
		waitForTaskArgs(t, jobID, "999", 60*time.Second)
	})
}
