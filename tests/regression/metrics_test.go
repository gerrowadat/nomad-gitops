//go:build regression

package regression

import (
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gerrowadat/nomad-gitops/internal/nomad"
)

// TestMetrics_AllExpectedMetricsPresent verifies that a fresh Differ registers
// all documented Prometheus metrics at construction time (before any Check call).
func TestMetrics_AllExpectedMetricsPresent(t *testing.T) {
	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = "regtest-metrics-*"
	_, reg := newTestDifferInspectable(cfg)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	present := make(map[string]struct{})
	for _, mf := range mfs {
		present[mf.GetName()] = struct{}{}
	}

	required := []string{
		"nomad_gitops_diff_checks_total",
		"nomad_gitops_diff_checks_skipped_total",
		"nomad_gitops_hcl_parse_errors_total",
		"nomad_gitops_hcl_non_job_files_skipped_total",
		"nomad_gitops_api_errors_total",
		"nomad_gitops_last_check_timestamp_seconds",
		// nomad_gitops_job_diffs and nomad_gitops_job_drift_first_seen_timestamp_seconds
		// have a dynamic "job" label and only appear after a Check call produces drift.
		// They are exercised by TestMetrics_DiffCountersReflectState and
		// TestMetrics_FirstSeenTimestamps.
		"nomad_gitops_drifted_jobs",
		"nomad_gitops_staleness_checks_total",
		"nomad_gitops_jobs_skipped_by_selector_total",
	}
	for _, name := range required {
		if _, ok := present[name]; !ok {
			t.Errorf("metric %q not registered", name)
		}
	}
}

// TestMetrics_DiffCountersReflectState runs a Check that produces both
// missing_from_nomad and missing_from_hcl diffs and verifies the gauge values.
func TestMetrics_DiffCountersReflectState(t *testing.T) {
	suffix := randomSuffix()
	// mhcl: in Nomad, not in HCL → missing_from_hcl
	mhcl := "regtest-mhcl-" + suffix
	// mnomad: in HCL, not in Nomad → missing_from_nomad
	mnomad := "regtest-mnomad-" + suffix

	registerJobHCL(t, testJobHCL(mhcl))
	waitForJobStatus(t, mhcl, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = "regtest-m*-" + suffix
	d, reg := newTestDifferInspectable(cfg)

	hclFiles := map[string]string{
		mnomad + ".hcl": testJobHCL(mnomad),
		// mhcl has no HCL entry → triggers missing_from_hcl
	}
	if err := d.Check(hclFiles, "c1"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs, _, _ := d.Diffs()
	diffsByJob := make(map[string]nomad.DiffType)
	for _, df := range diffs {
		diffsByJob[df.JobID] = df.DiffType
	}
	if dt, ok := diffsByJob[mnomad]; !ok || dt != nomad.DiffTypeMissingFromNomad {
		t.Errorf("job %q: want missing_from_nomad, got %q", mnomad, dt)
	}
	if dt, ok := diffsByJob[mhcl]; !ok || dt != nomad.DiffTypeMissingFromHCL {
		t.Errorf("job %q: want missing_from_hcl, got %q", mhcl, dt)
	}

	// The drifted_jobs gauge should reflect the two diffs.
	drifted := gatherCounter(t, reg, "nomad_gitops_drifted_jobs")
	if drifted == 0 {
		t.Error("nomad_gitops_drifted_jobs should be nonzero after diffs detected")
	}

	// The job_diffs gauge should have one entry per (job, diff_type) pair.
	jobDiff := gatherCounter(t, reg, "nomad_gitops_job_diffs")
	if jobDiff == 0 {
		t.Error("nomad_gitops_job_diffs should be nonzero")
	}

	// last_check_timestamp should be a plausible recent Unix timestamp.
	lastCheck := gatherCounter(t, reg, "nomad_gitops_last_check_timestamp_seconds")
	if lastCheck < 1_000_000_000 { // anything before 2001 is wrong
		t.Errorf("nomad_gitops_last_check_timestamp_seconds not set (got %v)", lastCheck)
	}
}

// TestMetrics_APIErrorCounter verifies that a Nomad API error (e.g. connecting
// to an unreachable address) increments the diff_checks_total counter and does
// not panic.
func TestMetrics_APIErrorCounter(t *testing.T) {
	cfg := baseDiffCfg()
	cfg.NomadAddr = "http://127.0.0.1:1" // unreachable port
	cfg.JobSelectorGlob = "anything"

	// Build a client that actually targets the unreachable address so that
	// List() fails. newTestDifferInspectable always uses the global test client,
	// so we construct the Differ manually here.
	badAPICfg := nomadapi.DefaultConfig()
	badAPICfg.Address = cfg.NomadAddr
	badClient, err := nomadapi.NewClient(badAPICfg)
	if err != nil {
		t.Fatalf("nomadapi.NewClient: %v", err)
	}
	reg := prometheus.NewRegistry()
	d := nomad.NewWithClientAndRegistry(cfg, badClient.Jobs(), reg)

	_ = d.Check(map[string]string{}, "c1")

	// The List() call fails; the differ logs the error and continues.
	// diff_checks_total still increments (we started a check, even if List failed).
	// API errors may or may not be counted depending on how the SDK reports the failure.
	checks := gatherCounter(t, reg, "nomad_gitops_diff_checks_total")
	if checks == 0 {
		t.Error("diff_checks_total should be ≥1 even when the API is unreachable")
	}
}

// TestMetrics_SkipOptimizationCounter verifies that repeated Check calls with
// the same commit and unchanged Nomad index increment diff_checks_skipped_total.
//
// Note: when run against a shared cluster via NOMAD_ADDR, unrelated activity
// can advance the global LastIndex between calls and prevent skips from
// triggering. This test is reliable against the Docker-managed cluster started
// by TestMain.
func TestMetrics_SkipOptimizationCounter(t *testing.T) {
	jobID := uniqueJobID("skip-counter")
	registerJobHCL(t, testJobHCL(jobID))
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	d, reg := newTestDifferInspectable(cfg)

	hclFiles := map[string]string{jobID + ".hcl": testJobHCL(jobID)}
	const commit = "stable-commit"
	const rounds = 4

	for i := 0; i < rounds; i++ {
		if err := d.Check(hclFiles, commit); err != nil {
			t.Fatalf("Check %d: %v", i, err)
		}
	}

	checks := gatherCounter(t, reg, "nomad_gitops_diff_checks_total")
	skipped := gatherCounter(t, reg, "nomad_gitops_diff_checks_skipped_total")

	if checks < 1 {
		t.Errorf("want ≥1 real check, got %v", checks)
	}
	// After the first check captures the Raft index, subsequent identical calls
	// should be skipped.
	if skipped < float64(rounds-1) {
		t.Errorf("want ≥%d skipped checks, got %v (checks=%v)", rounds-1, skipped, checks)
	}
}

// TestMetrics_FirstSeenTimestamps verifies that the first-seen timestamp for a
// drifting job is set, remains stable on repeated checks, and is cleared once
// the drift resolves.
func TestMetrics_FirstSeenTimestamps(t *testing.T) {
	jobID := uniqueJobID("first-seen")
	hcl := testJobHCL(jobID)

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = jobID
	d, reg := newTestDifferInspectable(cfg)

	// First check: job not in Nomad → drift detected, first-seen set.
	if err := d.Check(map[string]string{jobID + ".hcl": hcl}, "c1"); err != nil {
		t.Fatalf("Check c1: %v", err)
	}
	ts1 := gatherCounter(t, reg, "nomad_gitops_job_drift_first_seen_timestamp_seconds")
	if ts1 == 0 {
		t.Fatal("first-seen timestamp should be set after initial drift detection")
	}

	// Second check (different commit to bypass skip): drift persists, timestamp unchanged.
	// gatherCounter sums all samples in the family; this works correctly here because
	// there is exactly one drifting job in this registry. A future test that adds a
	// second drift entry to the same registry would need label-filtered lookup instead.
	if err := d.Check(map[string]string{jobID + ".hcl": hcl}, "c2"); err != nil {
		t.Fatalf("Check c2: %v", err)
	}
	ts2 := gatherCounter(t, reg, "nomad_gitops_job_drift_first_seen_timestamp_seconds")
	if ts2 != ts1 {
		t.Errorf("first-seen should be stable across checks: c1=%v c2=%v", ts1, ts2)
	}

	// Register the job — on the next check the drift resolves.
	registerJobHCL(t, hcl)
	waitForJobStatus(t, jobID, "running", 30*time.Second)

	if err := d.Check(map[string]string{jobID + ".hcl": hcl}, "c3"); err != nil {
		t.Fatalf("Check c3: %v", err)
	}
	ts3 := gatherCounter(t, reg, "nomad_gitops_job_drift_first_seen_timestamp_seconds")
	if ts3 != 0 {
		t.Errorf("first-seen timestamp should be cleared after drift resolves, got %v", ts3)
	}
}

// TestMetrics_HCLParseErrorCounter verifies that malformed HCL increments
// the hcl_parse_errors_total counter.
func TestMetrics_HCLParseErrorCounter(t *testing.T) {
	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = "*"
	d, reg := newTestDifferInspectable(cfg)

	// The file contains "job " (so jobBlockRe matches) but has invalid syntax.
	badHCL := map[string]string{
		"broken.hcl": `job "broken" { this is not valid hcl syntax !!!`,
	}
	if err := d.Check(badHCL, "c1"); err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}

	parseErrors := gatherCounter(t, reg, "nomad_gitops_hcl_parse_errors_total")
	if parseErrors < 1 {
		t.Errorf("want ≥1 HCL parse error counted, got %v", parseErrors)
	}
}

// TestMetrics_NonJobHCLSkipped verifies that HCL files without a top-level
// job stanza (e.g. ACL policies, volume definitions) are counted as skipped.
func TestMetrics_NonJobHCLSkipped(t *testing.T) {
	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = "*"
	d, reg := newTestDifferInspectable(cfg)

	// ACL policy HCL — no "job" stanza.
	nonJobHCL := map[string]string{
		"policy.hcl": `
namespace "default" {
  policy = "read"
}
`,
	}
	if err := d.Check(nonJobHCL, "c1"); err != nil {
		t.Fatalf("Check returned error: %v", err)
	}

	skipped := gatherCounter(t, reg, "nomad_gitops_hcl_non_job_files_skipped_total")
	if skipped < 1 {
		t.Errorf("want ≥1 non-job file skipped, got %v", skipped)
	}
}
