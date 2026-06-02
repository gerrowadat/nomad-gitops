//go:build regression

package regression

import (
	"testing"
	"time"

	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

// TestSelection_GlobExactMatch verifies that a job matching an exact glob is
// selected and detected as missing_from_hcl when only in Nomad.
func TestSelection_GlobExactMatch(t *testing.T) {
	jobID := uniqueJobID("glob-exact")
	otherID := uniqueJobID("glob-other")

	registerJobHCL(t, testJobHCL(jobID))
	registerJobHCL(t, testJobHCL(otherID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)
	waitForJobStatus(t, otherID, "running", 30*time.Second)

	// Glob selects only jobID, not otherID.
	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{}, "c1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("want 1 diff (jobID only), got %d: %v", len(diffs), diffs)
	}
	if diffs[0].JobID != jobID {
		t.Errorf("want job_id %q, got %q", jobID, diffs[0].JobID)
	}
	if diffs[0].DiffType != nomad.DiffTypeMissingFromHCL {
		t.Errorf("want missing_from_hcl, got %s", diffs[0].DiffType)
	}
}

// TestSelection_GlobWildcard verifies that a wildcard glob selects multiple
// jobs by prefix.
func TestSelection_GlobWildcard(t *testing.T) {
	suffix := randomSuffix()
	id1 := "regtest-gwild-a-" + suffix
	id2 := "regtest-gwild-b-" + suffix
	otherID := uniqueJobID("gwild-other")

	registerJobHCL(t, testJobHCL(id1))
	registerJobHCL(t, testJobHCL(id2))
	registerJobHCL(t, testJobHCL(otherID))
	waitForJobStatus(t, id1, "running", 30*time.Second)
	waitForJobStatus(t, id2, "running", 30*time.Second)
	waitForJobStatus(t, otherID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = "regtest-gwild-*-" + suffix
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{}, "c1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 2 {
		t.Fatalf("want 2 diffs (id1 and id2), got %d: %v", len(diffs), diffs)
	}
	for _, df := range diffs {
		if df.JobID != id1 && df.JobID != id2 {
			t.Errorf("unexpected job in diffs: %q", df.JobID)
		}
	}
}

// TestSelection_GlobNoMatch verifies that a glob matching nothing produces
// no diffs even when Nomad has running jobs.
func TestSelection_GlobNoMatch(t *testing.T) {
	jobID := uniqueJobID("glob-no-match")
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = "does-not-exist-ever-*"
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{}, "c1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("want 0 diffs for non-matching glob, got %d: %v", len(diffs), diffs)
	}
}

// TestSelection_MetaKey verifies that a job with the gitops_managed=true meta
// key is selected by the meta-prefix selector.
func TestSelection_MetaKey(t *testing.T) {
	jobID := uniqueJobID("meta-sel")
	prefix := "gitops"
	registerJobHCL(t, testJobHCLWithMeta(jobID, prefix))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.ManagedMetaPrefix = prefix
	// No glob — only meta selection.
	d := newTestDiffer(cfg)

	// HCL matches: should see no diff.
	if err := d.Check(map[string]string{jobID + ".hcl": testJobHCLWithMeta(jobID, prefix)}, "c1"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("want 0 diffs for matching meta-key job, got %d: %v", len(diffs), diffs)
	}
}

// TestSelection_MetaKeyMissing verifies that a job WITHOUT the managed meta
// key is NOT selected by the meta-prefix selector.
func TestSelection_MetaKeyMissing(t *testing.T) {
	jobID := uniqueJobID("meta-absent")
	// Register a job without the meta key.
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.ManagedMetaPrefix = "gitops"
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{}, "c1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	// The job should be invisible to the differ.
	for _, df := range diffs {
		if df.JobID == jobID {
			t.Errorf("job without managed meta should not appear in diffs, but got: %v", df)
		}
	}
}

// TestSelection_Union_GlobOnly verifies that with both glob and meta-prefix
// configured, a job matched only by glob is still selected.
func TestSelection_Union_GlobOnly(t *testing.T) {
	jobID := uniqueJobID("union-glob-only")
	// Register without meta — matches glob only.
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	cfg.ManagedMetaPrefix = "gitops"
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{}, "c1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	found := false
	for _, df := range diffs {
		if df.JobID == jobID {
			found = true
			if df.DiffType != nomad.DiffTypeMissingFromHCL {
				t.Errorf("want missing_from_hcl, got %s", df.DiffType)
			}
		}
	}
	if !found {
		t.Errorf("job %q selected by glob should appear in diffs", jobID)
	}
}

// TestSelection_Union_MetaOnly verifies that with both glob and meta-prefix
// configured, a job matched only by meta is still selected.
func TestSelection_Union_MetaOnly(t *testing.T) {
	jobID := uniqueJobID("union-meta-only")
	prefix := "gitops"
	// Register with meta — matches meta only (not the glob pattern).
	registerJobHCL(t, testJobHCLWithMeta(jobID, prefix))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = "does-not-match-*"
	cfg.ManagedMetaPrefix = prefix
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{}, "c1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	found := false
	for _, df := range diffs {
		if df.JobID == jobID {
			found = true
		}
	}
	if !found {
		t.Errorf("job %q selected by meta should appear in diffs", jobID)
	}
}

// TestSelection_NoSelectionCriteria verifies that with neither glob nor
// meta-prefix set, no jobs are selected (nothing is reported).
func TestSelection_NoSelectionCriteria(t *testing.T) {
	jobID := uniqueJobID("no-sel")
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	// Both are empty — nothing should be selected.
	cfg.JobSelectorGlob = ""
	cfg.ManagedMetaPrefix = ""
	d := newTestDiffer(cfg)

	if err := d.Check(map[string]string{jobID + ".hcl": testJobHCL(jobID)}, "c1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("want 0 diffs with no selection criteria, got %d: %v", len(diffs), diffs)
	}
}
