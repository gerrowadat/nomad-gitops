//go:build regression

package regression

import (
	"testing"
	"time"

	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

// TestDrift_MissingFromNomad verifies that a job defined in HCL but absent
// from Nomad is reported as missing_from_nomad.
func TestDrift_MissingFromNomad(t *testing.T) {
	jobID := uniqueJobID("missing-nomad")
	hcl := testJobHCL(jobID)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{jobID + ".hcl": hcl}, "commit-1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("want 1 diff, got %d: %v", len(diffs), diffs)
	}
	if diffs[0].DiffType != nomad.DiffTypeMissingFromNomad {
		t.Errorf("want %s, got %s", nomad.DiffTypeMissingFromNomad, diffs[0].DiffType)
	}
	if diffs[0].JobID != jobID {
		t.Errorf("want job_id %q, got %q", jobID, diffs[0].JobID)
	}
}

// TestDrift_MissingFromHCL verifies that a running Nomad job with no
// corresponding HCL file is reported as missing_from_hcl.
func TestDrift_MissingFromHCL(t *testing.T) {
	jobID := uniqueJobID("missing-hcl")
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{}, "commit-1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("want 1 diff, got %d: %v", len(diffs), diffs)
	}
	if diffs[0].DiffType != nomad.DiffTypeMissingFromHCL {
		t.Errorf("want %s, got %s", nomad.DiffTypeMissingFromHCL, diffs[0].DiffType)
	}
}

// TestDrift_NoChanges verifies that a running job whose HCL exactly matches
// the registered definition produces no diffs.
func TestDrift_NoChanges(t *testing.T) {
	jobID := uniqueJobID("no-changes")
	hcl := testJobHCL(jobID)
	registerJobHCL(t, hcl)
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{jobID + ".hcl": hcl}, "commit-1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("want 0 diffs, got %d: %v", len(diffs), diffs)
	}
}

// TestDrift_Modified verifies that a running job whose HCL differs from the
// registered definition is reported as modified.
func TestDrift_Modified(t *testing.T) {
	jobID := uniqueJobID("modified")
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{jobID + ".hcl": testJobHCLModified(jobID)}, "commit-2"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("want 1 diff, got %d: %v", len(diffs), diffs)
	}
	if diffs[0].DiffType != nomad.DiffTypeModified {
		t.Errorf("want %s, got %s", nomad.DiffTypeModified, diffs[0].DiffType)
	}
	if diffs[0].JobID != jobID {
		t.Errorf("want job_id %q, got %q", jobID, diffs[0].JobID)
	}
}

// TestDrift_DeadJob_TreatedAsMissing verifies that by default a stopped job
// (status=dead) is treated as missing_from_nomad, not modified.
func TestDrift_DeadJob_TreatedAsMissing(t *testing.T) {
	jobID := uniqueJobID("dead-default")
	hcl := testJobHCL(jobID)
	registerJobHCL(t, hcl)
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	stopJob(t, jobID)
	waitForJobStatus(t, jobID, "dead", 20*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	cfg.IncludeDeadJobs = false
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{jobID + ".hcl": hcl}, "commit-1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("want 1 diff, got %d: %v", len(diffs), diffs)
	}
	if diffs[0].DiffType != nomad.DiffTypeMissingFromNomad {
		t.Errorf("want %s, got %s (dead job should be treated as missing by default)",
			nomad.DiffTypeMissingFromNomad, diffs[0].DiffType)
	}
}

// TestDrift_DeadJob_IncludeDeadJobs verifies that with --include-dead-jobs,
// a stopped job is compared against its HCL rather than being treated as missing.
// When the HCL matches the stopped definition exactly, no diff is reported.
func TestDrift_DeadJob_IncludeDeadJobs(t *testing.T) {
	jobID := uniqueJobID("dead-included")
	hcl := testJobHCL(jobID)
	registerJobHCL(t, hcl)
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	stopJob(t, jobID)
	waitForJobStatus(t, jobID, "dead", 20*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	cfg.IncludeDeadJobs = true
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{jobID + ".hcl": hcl}, "commit-1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("want 0 diffs with include-dead-jobs and matching HCL, got %d: %v", len(diffs), diffs)
	}
}

// TestDrift_DeadJob_IncludeDeadJobs_Modified verifies that a dead job with
// a changed HCL is detected as modified when --include-dead-jobs is set.
func TestDrift_DeadJob_IncludeDeadJobs_Modified(t *testing.T) {
	jobID := uniqueJobID("dead-modified")
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	stopJob(t, jobID)
	waitForJobStatus(t, jobID, "dead", 20*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	cfg.IncludeDeadJobs = true
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{jobID + ".hcl": testJobHCLModified(jobID)}, "commit-2"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("want 1 diff, got %d: %v", len(diffs), diffs)
	}
	if diffs[0].DiffType != nomad.DiffTypeModified {
		t.Errorf("want %s, got %s", nomad.DiffTypeModified, diffs[0].DiffType)
	}
}

// TestDrift_RaftIndexSkip verifies that a second Check with an unchanged
// Nomad Raft index and the same commit is skipped (returns immediately).
//
// Note: when run against a shared cluster via NOMAD_ADDR, unrelated job or
// eval churn can advance the global LastIndex between calls and prevent the
// skip from triggering, making this test flaky. It is reliable against the
// Docker-managed cluster started by TestMain.
func TestDrift_RaftIndexSkip(t *testing.T) {
	jobID := uniqueJobID("raft-skip")
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	d, reg := newTestDifferInspectable(cfg)

	hclFiles := map[string]string{jobID + ".hcl": testJobHCL(jobID)}
	commit := "commit-stable"

	// Background cluster activity (evals and GC from earlier tests) can
	// advance the global Raft index between two Check calls, defeating the
	// skip legitimately. Retry the pair until a quiet window is hit.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := d.Check(hclFiles, commit); err != nil {
			t.Fatalf("first Check: %v", err)
		}
		if err := d.Check(hclFiles, commit); err != nil {
			t.Fatalf("second Check: %v", err)
		}
		if gatherCounter(t, reg, "nomad_botherer_diff_checks_skipped_total") >= 1 {
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("no skipped check observed after identical commit+index within 30s; got %v",
		gatherCounter(t, reg, "nomad_botherer_diff_checks_skipped_total"))
}

// TestDrift_CommitChange verifies that changing the commit hash (with the same
// Nomad state) forces a new diff check and bypasses the skip optimisation.
func TestDrift_CommitChange(t *testing.T) {
	jobID := uniqueJobID("commit-change")
	hcl := testJobHCL(jobID)
	registerJobHCL(t, hcl)
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	d, reg := newTestDifferInspectable(cfg)

	hclFiles := map[string]string{jobID + ".hcl": hcl}
	if err := d.Check(hclFiles, "commit-A"); err != nil {
		t.Fatalf("first Check: %v", err)
	}
	if err := d.Check(hclFiles, "commit-B"); err != nil {
		t.Fatalf("second Check: %v", err)
	}

	skipped := gatherCounter(t, reg, "nomad_botherer_diff_checks_skipped_total")
	if skipped > 0 {
		t.Errorf("commit change must not be skipped; skipped=%v", skipped)
	}
	checks := gatherCounter(t, reg, "nomad_botherer_diff_checks_total")
	if checks < 2 {
		t.Errorf("want ≥2 checks (one per distinct commit), got %v", checks)
	}
}

// TestDrift_MultipleJobs verifies correct diff detection across multiple jobs
// in a single Check call.
func TestDrift_MultipleJobs(t *testing.T) {
	suffix := randomSuffix()
	runningID := "regtest-multi-run-" + suffix
	missingID := "regtest-multi-mis-" + suffix

	registerJobHCL(t, testJobHCL(runningID))
	waitForJobStatus(t, runningID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = "regtest-multi-*-" + suffix
	d := newTestDiffer(cfg)

	hclFiles := map[string]string{
		runningID + ".hcl": testJobHCL(runningID),
		missingID + ".hcl": testJobHCL(missingID),
	}
	if err := d.Check(hclFiles, "commit-1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	diffsByJob := make(map[string]nomad.DiffType)
	for _, df := range diffs {
		diffsByJob[df.JobID] = df.DiffType
	}

	if dt, ok := diffsByJob[runningID]; ok {
		t.Errorf("running job %q should have no diff, got %s", runningID, dt)
	}
	if dt, ok := diffsByJob[missingID]; !ok || dt != nomad.DiffTypeMissingFromNomad {
		t.Errorf("missing job %q: want missing_from_nomad, got %q", missingID, dt)
	}
}

// TestDrift_ForceCheck verifies that ForceCheck increments the staleness
// counter and runs a check. It also documents the current behaviour when
// ForceCheck is called with an unchanged commit and Nomad index: the
// underlying Check still applies the Raft-index skip optimisation, so the
// check may be skipped. Whether that is desirable is a design question; this
// test records the observed behaviour so regressions are visible.
func TestDrift_ForceCheck(t *testing.T) {
	jobID := uniqueJobID("force-check")
	hcl := testJobHCL(jobID)
	registerJobHCL(t, hcl)
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	d, reg := newTestDifferInspectable(cfg)

	hclFiles := map[string]string{jobID + ".hcl": hcl}
	const commit = "force-commit"

	// First call establishes the Raft-index baseline.
	if err := d.Check(hclFiles, commit); err != nil {
		t.Fatalf("Check: %v", err)
	}

	// ForceCheck with the same commit and unchanged Nomad state.
	if err := d.ForceCheck(hclFiles, commit); err != nil {
		t.Fatalf("ForceCheck: %v", err)
	}

	// The staleness counter must always be incremented by ForceCheck.
	stale := gatherCounter(t, reg, "nomad_botherer_nomad_staleness_checks_total")
	if stale < 1 {
		t.Errorf("want ≥1 staleness check counted, got %v", stale)
	}

	// Total checks must be ≥1 (the initial Check call).
	checks := gatherCounter(t, reg, "nomad_botherer_diff_checks_total")
	if checks < 1 {
		t.Errorf("want ≥1 diff check total, got %v", checks)
	}
}
