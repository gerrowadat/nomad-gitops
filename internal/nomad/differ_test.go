package nomad_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

func strPtr(s string) *string { return &s }

// mockJobsClient lets individual test cases override only the methods they care about.
type mockJobsClient struct {
	parseHCLFn   func(jobHCL string, normalize bool) (*nomadapi.Job, error)
	planFn       func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error)
	infoFn       func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error)
	listFn       func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error)
	registerFn   func(job *nomadapi.Job, opts *nomadapi.RegisterOptions, q *nomadapi.WriteOptions) (*nomadapi.JobRegisterResponse, *nomadapi.WriteMeta, error)
	deregisterFn func(jobID string, purge bool, q *nomadapi.WriteOptions) (string, *nomadapi.WriteMeta, error)
}

func (m *mockJobsClient) ParseHCL(jobHCL string, normalize bool) (*nomadapi.Job, error) {
	return m.parseHCLFn(jobHCL, normalize)
}
func (m *mockJobsClient) Plan(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
	return m.planFn(job, diff, q)
}
func (m *mockJobsClient) Info(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
	return m.infoFn(jobID, q)
}
func (m *mockJobsClient) List(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
	return m.listFn(q)
}
func (m *mockJobsClient) RegisterOpts(job *nomadapi.Job, opts *nomadapi.RegisterOptions, q *nomadapi.WriteOptions) (*nomadapi.JobRegisterResponse, *nomadapi.WriteMeta, error) {
	if m.registerFn == nil {
		return &nomadapi.JobRegisterResponse{}, nil, nil
	}
	return m.registerFn(job, opts, q)
}
func (m *mockJobsClient) Deregister(jobID string, purge bool, q *nomadapi.WriteOptions) (string, *nomadapi.WriteMeta, error) {
	if m.deregisterFn == nil {
		return "", nil, nil
	}
	return m.deregisterFn(jobID, purge, q)
}

// defaultMock returns a client where everything succeeds with no diffs.
func defaultMock() *mockJobsClient {
	return &mockJobsClient{
		parseHCLFn: func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
			return &nomadapi.Job{ID: strPtr("test-job")}, nil
		},
		planFn: func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
			return &nomadapi.JobPlanResponse{Diff: &nomadapi.JobDiff{Type: "None"}}, nil, nil
		},
		infoFn: func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
			return &nomadapi.Job{ID: strPtr(jobID)}, nil, nil
		},
		listFn: func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
			return nil, nil, nil
		},
	}
}

func newTestDiffer(mock *mockJobsClient) *nomad.Differ {
	cfg := &config.Config{NomadAddr: "http://localhost:4646", NomadNamespace: "default", JobSelectorGlob: "*"}
	return nomad.NewWithClient(cfg, mock)
}

func newTestDifferWithDeadJobs(mock *mockJobsClient) *nomad.Differ {
	cfg := &config.Config{NomadAddr: "http://localhost:4646", NomadNamespace: "default", JobSelectorGlob: "*", IncludeDeadJobs: true}
	return nomad.NewWithClient(cfg, mock)
}

func newTestDifferWithSelection(mock *mockJobsClient, glob, metaPrefix string) *nomad.Differ {
	cfg := &config.Config{
		NomadAddr:         "http://localhost:4646",
		NomadNamespace:    "default",
		JobSelectorGlob:   glob,
		ManagedMetaPrefix: metaPrefix,
	}
	return nomad.NewWithClient(cfg, mock)
}

func TestDiffer_NoChanges(t *testing.T) {
	d := newTestDiffer(defaultMock())

	if err := d.Check(map[string]string{"jobs/test-job.hcl": `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diffs, lastCheck, commit := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs, got %d: %+v", len(diffs), diffs)
	}
	if lastCheck.IsZero() {
		t.Error("lastCheck should not be zero after Check()")
	}
	if commit != "abc123" {
		t.Errorf("expected commit abc123, got %q", commit)
	}
}

func TestDiffer_MissingFromNomad(t *testing.T) {
	mock := defaultMock()
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("Unexpected response code: 404 (job not found)")
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{"jobs/test-job.hcl": `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}
	if diffs[0].DiffType != nomad.DiffTypeMissingFromNomad {
		t.Errorf("expected %s, got %s", nomad.DiffTypeMissingFromNomad, diffs[0].DiffType)
	}
	if diffs[0].HCLFile != "jobs/test-job.hcl" {
		t.Errorf("unexpected HCLFile: %q", diffs[0].HCLFile)
	}
}

func TestDiffer_Modified(t *testing.T) {
	mock := defaultMock()
	mock.planFn = func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
		return &nomadapi.JobPlanResponse{Diff: &nomadapi.JobDiff{Type: "Edited"}}, nil, nil
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{"jobs/test-job.hcl": `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}
	if diffs[0].DiffType != nomad.DiffTypeModified {
		t.Errorf("expected %s, got %s", nomad.DiffTypeModified, diffs[0].DiffType)
	}
}

func TestDiffer_MissingFromHCL(t *testing.T) {
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{{ID: "orphan-job", Status: "running"}}, nil, nil
	}
	d := newTestDiffer(mock)

	// No HCL files → every running Nomad job is orphaned.
	if err := d.Check(map[string]string{}, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}
	if diffs[0].DiffType != nomad.DiffTypeMissingFromHCL {
		t.Errorf("expected %s, got %s", nomad.DiffTypeMissingFromHCL, diffs[0].DiffType)
	}
	if diffs[0].JobID != "orphan-job" {
		t.Errorf("unexpected job ID: %q", diffs[0].JobID)
	}
}

func TestDiffer_HCLParseError_Skipped(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return nil, fmt.Errorf("HCL syntax error")
	}
	d := newTestDiffer(mock)

	// Content has a job stanza but the (mock) parser rejects it — should log,
	// increment the error counter, and move on without returning an error.
	if err := d.Check(map[string]string{`bad.hcl`: `job "broken" { INVALID }`}, "abc123"); err != nil {
		t.Fatalf("Check should not fail on parse errors: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs after parse error, got %d", len(diffs))
	}
}

func TestDiffer_NonJobHCL_Skipped(t *testing.T) {
	mock := defaultMock()
	parseCalled := false
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		parseCalled = true
		return nil, fmt.Errorf("should not be called")
	}
	d := newTestDiffer(mock)

	aclPolicy := `
name        = "my-policy"
description = "ACL policy for readers"
rules       = <<EOT
namespace "default" {
  policy = "read"
}
EOT`
	volume := `
id        = "database"
name      = "database"
type      = "csi"
plugin_id = "aws-ebs"
`
	if err := d.Check(map[string]string{
		"policies/readers.hcl": aclPolicy,
		"volumes/db.hcl":       volume,
	}, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if parseCalled {
		t.Error("ParseHCL should not be called for non-job HCL files")
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs for non-job files, got %d", len(diffs))
	}
}

func TestDiffer_MultipleDiffTypes(t *testing.T) {
	mock := defaultMock()

	// job-a: exists but modified
	// job-b: missing from Nomad
	// job-c: running in Nomad but not in HCL
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		if strings.Contains(jobHCL, "job-a") {
			return &nomadapi.Job{ID: strPtr("job-a")}, nil
		}
		return &nomadapi.Job{ID: strPtr("job-b")}, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		if jobID == "job-b" {
			return nil, nil, fmt.Errorf("404: not found")
		}
		return &nomadapi.Job{ID: strPtr(jobID)}, nil, nil
	}
	mock.planFn = func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
		return &nomadapi.JobPlanResponse{Diff: &nomadapi.JobDiff{Type: "Edited"}}, nil, nil
	}
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{
			{ID: "job-a", Status: "running"},
			{ID: "job-b", Status: "pending"},
			{ID: "job-c", Status: "running"},
		}, nil, nil
	}

	d := newTestDiffer(mock)
	if err := d.Check(map[string]string{`a.hcl`: `job "job-a" {}`, `b.hcl`: `job "job-b" {}`}, "xyz"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 3 {
		t.Errorf("expected 3 diffs, got %d: %+v", len(diffs), diffs)
	}
}

// TestDiffer_DeadJob_TreatedAsMissing verifies that a job found in Nomad with
// status "dead" is reported as missing_from_nomad by default.
func TestDiffer_DeadJob_TreatedAsMissing(t *testing.T) {
	mock := defaultMock()
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return &nomadapi.Job{ID: strPtr(jobID), Status: strPtr("dead")}, nil, nil
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{`jobs/test-job.hcl`: `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d: %+v", len(diffs), diffs)
	}
	if diffs[0].DiffType != nomad.DiffTypeMissingFromNomad {
		t.Errorf("expected %s for dead job, got %s", nomad.DiffTypeMissingFromNomad, diffs[0].DiffType)
	}
}

// TestDiffer_DeadJob_IncludeDeadJobs verifies that with IncludeDeadJobs=true a
// dead job is planned against normally (not treated as missing).
func TestDiffer_DeadJob_IncludeDeadJobs(t *testing.T) {
	mock := defaultMock()
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return &nomadapi.Job{ID: strPtr(jobID), Status: strPtr("dead")}, nil, nil
	}
	// Plan returns no diff — job is dead but config matches.
	mock.planFn = func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
		return &nomadapi.JobPlanResponse{Diff: &nomadapi.JobDiff{Type: "None"}}, nil, nil
	}
	d := newTestDifferWithDeadJobs(mock)

	if err := d.Check(map[string]string{`jobs/test-job.hcl`: `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs with IncludeDeadJobs=true and no plan diff, got %d: %+v", len(diffs), diffs)
	}
}

// TestDiffer_DeadJobInNomad_NoHCL_NotReported verifies that a dead job in
// Nomad without an HCL file is NOT reported as missing_from_hcl by default.
func TestDiffer_DeadJobInNomad_NoHCL_NotReported(t *testing.T) {
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{
			{ID: "stopped-job", Status: "dead"},
		}, nil, nil
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{}, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("dead job without HCL should not be reported by default, got %d diffs: %+v", len(diffs), diffs)
	}
}

func TestDiffer_NilJobID_Skipped(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: nil}, nil
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{`job.hcl`: `job "x" {}`}, "abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("nil job ID should be skipped, got %d diffs", len(diffs))
	}
}

func TestDiffer_InfoNonNotFoundError_Skipped(t *testing.T) {
	mock := defaultMock()
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("connection refused")
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{`job.hcl`: `job "test-job" {}`}, "abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("non-404 info error should be skipped, got %d diffs: %+v", len(diffs), diffs)
	}
}

func TestDiffer_PlanError_Skipped(t *testing.T) {
	mock := defaultMock()
	mock.planFn = func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
		return nil, nil, fmt.Errorf("server error")
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{`job.hcl`: `job "test-job" {}`}, "abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("plan error should be skipped, got %d diffs: %+v", len(diffs), diffs)
	}
}

func TestDiffer_ListError_Skipped(t *testing.T) {
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("list failed")
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{}, "abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("list error should not result in diffs, got %d", len(diffs))
	}
}

func newTestDifferWithRegistry(mock *mockJobsClient, reg prometheus.Registerer) *nomad.Differ {
	cfg := &config.Config{NomadAddr: "http://localhost:4646", NomadNamespace: "default", JobSelectorGlob: "*"}
	return nomad.NewWithClientAndRegistry(cfg, mock, reg)
}

// TestDiffer_DriftedJobsMetric verifies that drifted_jobs gauge is set per diff type.
func TestDiffer_DriftedJobsMetric(t *testing.T) {
	mock := defaultMock()
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{{ID: "orphan", Status: "running"}}, nil, nil
	}

	reg := prometheus.NewRegistry()
	d := newTestDifferWithRegistry(mock, reg)

	if err := d.Check(map[string]string{"a.hcl": `job "test-job" {}`}, "abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Gather all metrics and look for the two we care about.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	driftedJobs := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "nomad_botherer_drifted_jobs" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var dt string
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "diff_type" {
					dt = lp.GetValue()
				}
			}
			driftedJobs[dt] = m.GetGauge().GetValue()
		}
	}
	if driftedJobs["missing_from_nomad"] != 1 {
		t.Errorf("expected drifted_jobs{missing_from_nomad}=1, got %v", driftedJobs["missing_from_nomad"])
	}
	if driftedJobs["missing_from_hcl"] != 1 {
		t.Errorf("expected drifted_jobs{missing_from_hcl}=1, got %v", driftedJobs["missing_from_hcl"])
	}
	if _, ok := driftedJobs["modified"]; ok {
		t.Errorf("modified should not appear in drifted_jobs, got %v", driftedJobs["modified"])
	}
}

// TestDiffer_DriftFirstSeen_Persists verifies that first-seen timestamps are
// preserved across checks as long as drift continues.
func TestDiffer_DriftFirstSeen_Persists(t *testing.T) {
	mock := defaultMock()
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}

	reg := prometheus.NewRegistry()
	d := newTestDifferWithRegistry(mock, reg)

	if err := d.Check(map[string]string{"a.hcl": `job "test-job" {}`}, "abc"); err != nil {
		t.Fatalf("first check: %v", err)
	}
	firstTimestamp := gatherJobDriftSince(t, reg, "test-job", "missing_from_nomad")
	if firstTimestamp == 0 {
		t.Fatal("expected job_drift_first_seen_timestamp_seconds to be set after first check")
	}

	// Second check: drift still present — timestamp must not change.
	if err := d.Check(map[string]string{"a.hcl": `job "test-job" {}`}, "def"); err != nil {
		t.Fatalf("second check: %v", err)
	}
	secondTimestamp := gatherJobDriftSince(t, reg, "test-job", "missing_from_nomad")
	if secondTimestamp != firstTimestamp {
		t.Errorf("first-seen timestamp changed between checks: %v → %v", firstTimestamp, secondTimestamp)
	}
}

// TestDiffer_DriftFirstSeen_ClearedOnResolve verifies that the first-seen
// timestamp metric is removed once drift is resolved.
func TestDiffer_DriftFirstSeen_ClearedOnResolve(t *testing.T) {
	mock := defaultMock()
	// First check: job is missing.
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}

	reg := prometheus.NewRegistry()
	d := newTestDifferWithRegistry(mock, reg)

	if err := d.Check(map[string]string{"a.hcl": `job "test-job" {}`}, "abc"); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if ts := gatherJobDriftSince(t, reg, "test-job", "missing_from_nomad"); ts == 0 {
		t.Fatal("expected first-seen timestamp after first check")
	}

	// Second check: job now exists and matches — no drift.
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return &nomadapi.Job{ID: strPtr(jobID)}, nil, nil
	}
	if err := d.Check(map[string]string{"a.hcl": `job "test-job" {}`}, "def"); err != nil {
		t.Fatalf("second check: %v", err)
	}
	if ts := gatherJobDriftSince(t, reg, "test-job", "missing_from_nomad"); ts != 0 {
		t.Errorf("expected first-seen timestamp to be cleared after drift resolved, got %v", ts)
	}
}

// gatherJobDriftSince returns the value of
// nomad_botherer_job_drift_first_seen_timestamp_seconds for the given job and
// diff_type, or 0 if the metric is absent.
func gatherJobDriftSince(t *testing.T, reg prometheus.Gatherer, job, diffType string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "nomad_botherer_job_drift_first_seen_timestamp_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["job"] == job && labels["diff_type"] == diffType {
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

// TestDiffer_SkipOnUnchangedIndexAndCommit verifies that Check skips all
// per-job API calls when both the Nomad Raft index and the git commit are
// identical to the previous check, and still updates the last-check timestamp
// so the NomadBothererCheckStale alert does not fire on quiet but healthy cycles.
func TestDiffer_SkipOnUnchangedIndexAndCommit(t *testing.T) {
	mock := defaultMock()
	infoCalls := 0
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		infoCalls++
		return &nomadapi.Job{ID: strPtr(jobID)}, nil, nil
	}
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return nil, &nomadapi.QueryMeta{LastIndex: 42}, nil
	}
	reg := prometheus.NewRegistry()
	d := newTestDifferWithRegistry(mock, reg)

	if err := d.Check(map[string]string{"a.hcl": `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatal(err)
	}
	if infoCalls != 1 {
		t.Fatalf("expected 1 Info call after first check, got %d", infoCalls)
	}

	// Capture the timestamp written by the first (full) check.
	tsAfterFirst := gatherLastCheckTimestamp(t, reg)
	if tsAfterFirst == 0 {
		t.Fatal("expected last_check_timestamp_seconds to be set after first check")
	}

	// Wait until the wall clock has advanced past tsAfterFirst so that a fresh
	// time.Now().Unix() will produce a strictly larger value. This is at most
	// one second and avoids flakiness from both checks landing in the same second.
	deadline := time.Unix(int64(tsAfterFirst)+1, 0)
	time.Sleep(time.Until(deadline))

	// Second call: same commit, same Nomad index — must skip per-job work.
	if err := d.Check(map[string]string{"a.hcl": `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatal(err)
	}
	if infoCalls != 1 {
		t.Errorf("expected Info not called on skip, got %d total calls", infoCalls)
	}

	// The skip path must have advanced the timestamp, proving it updated the
	// metric rather than relying on the earlier commitResults() call.
	tsAfterSkip := gatherLastCheckTimestamp(t, reg)
	if tsAfterSkip <= tsAfterFirst {
		t.Errorf("last_check_timestamp_seconds not updated by skip: before=%v after=%v", tsAfterFirst, tsAfterSkip)
	}
}

func gatherLastCheckTimestamp(t *testing.T, reg prometheus.Gatherer) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "nomad_botherer_last_check_timestamp_seconds" {
			for _, m := range mf.GetMetric() {
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

// TestDiffer_NoSkipOnChangedCommit verifies that a new git commit forces a
// full check even when the Nomad index has not advanced.
func TestDiffer_NoSkipOnChangedCommit(t *testing.T) {
	mock := defaultMock()
	infoCalls := 0
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		infoCalls++
		return &nomadapi.Job{ID: strPtr(jobID)}, nil, nil
	}
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return nil, &nomadapi.QueryMeta{LastIndex: 42}, nil
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{"a.hcl": `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatal(err)
	}
	// Different commit, same Nomad index — must not skip.
	if err := d.Check(map[string]string{"a.hcl": `job "test-job" {}`}, "def456"); err != nil {
		t.Fatal(err)
	}
	if infoCalls != 2 {
		t.Errorf("expected 2 Info calls when commit changes, got %d", infoCalls)
	}
}

// TestDiffer_NoSkipOnChangedNomadIndex verifies that an advanced Nomad Raft
// index forces a full check even when the git commit is unchanged.
func TestDiffer_NoSkipOnChangedNomadIndex(t *testing.T) {
	mock := defaultMock()
	infoCalls := 0
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		infoCalls++
		return &nomadapi.Job{ID: strPtr(jobID)}, nil, nil
	}
	nomadIndex := uint64(42)
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return nil, &nomadapi.QueryMeta{LastIndex: nomadIndex}, nil
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{"a.hcl": `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatal(err)
	}
	// Advance the Nomad index, same commit — must not skip.
	nomadIndex = 99
	if err := d.Check(map[string]string{"a.hcl": `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatal(err)
	}
	if infoCalls != 2 {
		t.Errorf("expected 2 Info calls when Nomad index advances, got %d", infoCalls)
	}
}

// TestDiffer_NoSkipOnNilListMeta verifies that a nil QueryMeta (e.g. list
// error) never triggers a skip.
func TestDiffer_NoSkipOnNilListMeta(t *testing.T) {
	mock := defaultMock()
	infoCalls := 0
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		infoCalls++
		return &nomadapi.Job{ID: strPtr(jobID)}, nil, nil
	}
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return nil, nil, nil // nil meta, no error
	}
	d := newTestDiffer(mock)

	for i := 0; i < 3; i++ {
		if err := d.Check(map[string]string{"a.hcl": `job "test-job" {}`}, "abc123"); err != nil {
			t.Fatal(err)
		}
	}
	if infoCalls != 3 {
		t.Errorf("expected 3 Info calls with nil meta (no skip), got %d", infoCalls)
	}
}

// TestDiffer_DeadJobInNomad_NoHCL_IncludeDeadJobs verifies that with
// IncludeDeadJobs=true a dead Nomad job without HCL IS reported as missing_from_hcl.
func TestDiffer_DeadJobInNomad_NoHCL_IncludeDeadJobs(t *testing.T) {
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{
			{ID: "stopped-job", Status: "dead"},
		}, nil, nil
	}
	d := newTestDifferWithDeadJobs(mock)

	if err := d.Check(map[string]string{}, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff with IncludeDeadJobs=true, got %d: %+v", len(diffs), diffs)
	}
	if diffs[0].DiffType != nomad.DiffTypeMissingFromHCL {
		t.Errorf("expected %s, got %s", nomad.DiffTypeMissingFromHCL, diffs[0].DiffType)
	}
}

func TestDiffer_ForceCheck_RunsCheck(t *testing.T) {
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return nil, &nomadapi.QueryMeta{LastIndex: 42}, nil
	}
	d := newTestDiffer(mock)

	if err := d.ForceCheck(map[string]string{"job.hcl": `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diffs, lastCheck, commit := d.Diffs()
	if lastCheck.IsZero() {
		t.Error("lastCheck should not be zero after ForceCheck()")
	}
	if commit != "abc123" {
		t.Errorf("commit: want abc123, got %q", commit)
	}
	_ = diffs
}

// TestDiffer_Ready_BeforeCheck verifies that Ready() returns false before any
// Check has completed.
func TestDiffer_Ready_BeforeCheck(t *testing.T) {
	d := newTestDiffer(defaultMock())
	if d.Ready() {
		t.Error("Ready() should return false before any Check has completed")
	}
}

// TestDiffer_Ready_AfterCheck verifies that Ready() returns true once the first
// Check has run.
func TestDiffer_Ready_AfterCheck(t *testing.T) {
	d := newTestDiffer(defaultMock())
	if err := d.Check(map[string]string{"job.hcl": `job "test-job" {}`}, "abc"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !d.Ready() {
		t.Error("Ready() should return true after Check has completed")
	}
}

// TestDiffer_Diffs_SnapshotIsolation verifies that mutating the slice returned
// by Diffs() does not affect the Differ's internal state.
func TestDiffer_Diffs_SnapshotIsolation(t *testing.T) {
	mock := defaultMock()
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{"job.hcl": `job "test-job" {}`}, "abc"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	diffs1, _, _ := d.Diffs()
	if len(diffs1) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs1))
	}

	// Mutate the returned slice — the Differ's internal state must be unaffected.
	diffs1[0] = nomad.JobDiff{JobID: "mutated"}

	diffs2, _, _ := d.Diffs()
	if len(diffs2) != 1 {
		t.Fatalf("expected 1 diff after mutation, got %d", len(diffs2))
	}
	if diffs2[0].JobID != "test-job" {
		t.Errorf("Diffs() snapshot was affected by mutation; got job ID %q, want %q", diffs2[0].JobID, "test-job")
	}
}

// TestDiffer_EmptyJobID_Skipped verifies that a job returned by ParseHCL with
// an empty (non-nil) job ID is silently skipped.
func TestDiffer_EmptyJobID_Skipped(t *testing.T) {
	mock := defaultMock()
	emptyID := ""
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: &emptyID}, nil
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{`job.hcl`: `job "x" {}`}, "abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("empty string job ID should be skipped, got %d diffs", len(diffs))
	}
}

// TestDiffer_ForceCheck_IncrementsStaleCounter verifies that ForceCheck
// increments the staleness check counter metric.
func TestDiffer_ForceCheck_IncrementsStaleCounter(t *testing.T) {
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return nil, &nomadapi.QueryMeta{LastIndex: 42}, nil
	}
	reg := prometheus.NewRegistry()
	d := newTestDifferWithRegistry(mock, reg)

	if err := d.ForceCheck(map[string]string{"job.hcl": `job "test-job" {}`}, "abc"); err != nil {
		t.Fatalf("ForceCheck: %v", err)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var staleChecks float64
	for _, mf := range mfs {
		if mf.GetName() == "nomad_botherer_nomad_staleness_checks_total" {
			for _, m := range mf.GetMetric() {
				staleChecks += m.GetCounter().GetValue()
			}
		}
	}
	if staleChecks != 1 {
		t.Errorf("expected nomad_staleness_checks_total=1 after ForceCheck, got %v", staleChecks)
	}
}

func TestDiffer_ForceCheck_BypassesSkipOptimization(t *testing.T) {
	// Confirm that two identical ForceCheck calls both run (the second would
	// normally be skipped by the Raft-index optimisation, but ForceCheck must
	// still run — it calls Check() which respects the skip logic). This test
	// verifies ForceCheck delegates to Check rather than introducing new skip logic.
	mock := defaultMock()
	infoCalls := 0
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		infoCalls++
		return &nomadapi.Job{ID: strPtr(jobID)}, nil, nil
	}
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return nil, &nomadapi.QueryMeta{LastIndex: 99}, nil
	}
	d := newTestDiffer(mock)

	files := map[string]string{"job.hcl": `job "test-job" {}`}
	if err := d.ForceCheck(files, "sha1"); err != nil {
		t.Fatalf("first ForceCheck: %v", err)
	}
	firstCalls := infoCalls

	// Second call with same commit and same Raft index: Check() will skip, so
	// infoCalls should not increase.
	if err := d.ForceCheck(files, "sha1"); err != nil {
		t.Fatalf("second ForceCheck: %v", err)
	}
	if infoCalls != firstCalls {
		t.Errorf("second ForceCheck should have been skipped by Check(); infoCalls went from %d to %d", firstCalls, infoCalls)
	}
}

// ── Job selection tests ──────────────────────────────────────────────────────

// TestDiffer_GlobSelection_WildcardMatchesAll verifies that glob="*" selects all jobs.
func TestDiffer_GlobSelection_WildcardMatchesAll(t *testing.T) {
	mock := defaultMock()
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}
	d := newTestDifferWithSelection(mock, "*", "" /* no meta prefix */)

	if err := d.Check(map[string]string{"job.hcl": `job "any-job" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 || diffs[0].DiffType != nomad.DiffTypeMissingFromNomad {
		t.Errorf("expected 1 missing_from_nomad diff with wildcard glob, got %+v", diffs)
	}
}

// TestDiffer_GlobSelection_PrefixMatch verifies that a prefix glob selects matching jobs.
func TestDiffer_GlobSelection_PrefixMatch(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		if strings.Contains(jobHCL, "prod-web") {
			return &nomadapi.Job{ID: strPtr("prod-web")}, nil
		}
		return &nomadapi.Job{ID: strPtr("staging-web")}, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}
	d := newTestDifferWithSelection(mock, "prod-*", "" /* no meta prefix */)

	files := map[string]string{
		"prod-web.hcl":    `job "prod-web" {}`,
		"staging-web.hcl": `job "staging-web" {}`,
	}
	if err := d.Check(files, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff (only prod-web), got %d: %+v", len(diffs), diffs)
	}
	if diffs[0].JobID != "prod-web" {
		t.Errorf("expected diff for prod-web, got %q", diffs[0].JobID)
	}
}

// TestDiffer_GlobSelection_NoMatch_JobSkipped verifies that a non-matching job is
// silently excluded and produces no diff.
func TestDiffer_GlobSelection_NoMatch_JobSkipped(t *testing.T) {
	mock := defaultMock()
	d := newTestDifferWithSelection(mock, "prod-*", "" /* no meta prefix */)

	if err := d.Check(map[string]string{"job.hcl": `job "staging-job" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs for non-matching glob, got %d: %+v", len(diffs), diffs)
	}
}

// TestDiffer_MetaSelection_Matches verifies that a job with the managed meta key
// is selected even when no glob is configured.
func TestDiffer_MetaSelection_Matches(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{
			ID:   strPtr("managed-job"),
			Meta: map[string]string{"gitops_managed": "true"},
		}, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}
	d := newTestDifferWithSelection(mock, "", "gitops")

	if err := d.Check(map[string]string{"job.hcl": `job "managed-job" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 || diffs[0].DiffType != nomad.DiffTypeMissingFromNomad {
		t.Errorf("expected 1 missing_from_nomad diff for managed job, got %+v", diffs)
	}
}

// TestDiffer_MetaSelection_NoTag_JobSkipped verifies that a job without the managed
// meta key is excluded when no glob is configured.
func TestDiffer_MetaSelection_NoTag_JobSkipped(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("unmanaged-job"), Meta: nil}, nil
	}
	d := newTestDifferWithSelection(mock, "", "gitops")

	if err := d.Check(map[string]string{"job.hcl": `job "unmanaged-job" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs for job without meta tag, got %d: %+v", len(diffs), diffs)
	}
}

// TestDiffer_MetaSelection_CustomPrefix verifies that a custom meta prefix works,
// deriving the full key as "<prefix>_managed".
func TestDiffer_MetaSelection_CustomPrefix(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{
			ID:   strPtr("my-job"),
			Meta: map[string]string{"myorg_managed": "true"},
		}, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}
	d := newTestDifferWithSelection(mock, "", "myorg")

	if err := d.Check(map[string]string{"job.hcl": `job "my-job" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Errorf("expected 1 diff with custom meta key, got %d: %+v", len(diffs), diffs)
	}
}

// TestDiffer_GlobOrMeta_EitherSelects verifies that a job matching either the
// glob or the meta key is included (union, not intersection).
func TestDiffer_GlobOrMeta_EitherSelects(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		if strings.Contains(jobHCL, "by-glob") {
			return &nomadapi.Job{ID: strPtr("by-glob"), Meta: nil}, nil
		}
		return &nomadapi.Job{
			ID:   strPtr("by-meta"),
			Meta: map[string]string{"gitops_managed": "true"},
		}, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}
	d := newTestDifferWithSelection(mock, "by-glob", "gitops")

	files := map[string]string{
		"by-glob.hcl": `job "by-glob" {}`,
		"by-meta.hcl": `job "by-meta" {}`,
	}
	if err := d.Check(files, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 2 {
		t.Errorf("expected 2 diffs (one per selector), got %d: %+v", len(diffs), diffs)
	}
}

// TestDiffer_MissingFromHCL_ManagedByMeta_Reported verifies that a Nomad job
// with the managed meta key and no HCL file is reported as missing_from_hcl.
func TestDiffer_MissingFromHCL_ManagedByMeta_Reported(t *testing.T) {
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{
			{ID: "managed-orphan", Status: "running", Meta: map[string]string{"gitops_managed": "true"}},
		}, nil, nil
	}
	d := newTestDifferWithSelection(mock, "", "gitops")

	if err := d.Check(map[string]string{}, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 || diffs[0].DiffType != nomad.DiffTypeMissingFromHCL {
		t.Errorf("expected 1 missing_from_hcl diff for managed orphan, got %+v", diffs)
	}
}

// TestDiffer_MissingFromHCL_UnmanagedByMeta_NotReported verifies that a Nomad
// job without the managed meta key is not reported as missing_from_hcl.
func TestDiffer_MissingFromHCL_UnmanagedByMeta_NotReported(t *testing.T) {
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{
			{ID: "unmanaged-job", Status: "running", Meta: nil},
		}, nil, nil
	}
	d := newTestDifferWithSelection(mock, "", "gitops")

	if err := d.Check(map[string]string{}, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs for unmanaged Nomad job, got %d: %+v", len(diffs), diffs)
	}
}

// TestDiffer_MissingFromHCL_ManagedByGlob_Reported verifies that a Nomad job
// matching the glob but with no HCL file is reported as missing_from_hcl.
func TestDiffer_MissingFromHCL_ManagedByGlob_Reported(t *testing.T) {
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{
			{ID: "prod-web", Status: "running"},
		}, nil, nil
	}
	d := newTestDifferWithSelection(mock, "prod-*", "")

	if err := d.Check(map[string]string{}, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 || diffs[0].DiffType != nomad.DiffTypeMissingFromHCL {
		t.Errorf("expected 1 missing_from_hcl for glob-matched job, got %+v", diffs)
	}
}

// TestDiffer_NoSelection_NoJobsWatched verifies that with both glob and meta key
// empty, no jobs are selected and no diffs are reported.
func TestDiffer_NoSelection_NoJobsWatched(t *testing.T) {
	mock := defaultMock()
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{{ID: "some-job", Status: "running"}}, nil, nil
	}
	d := newTestDifferWithSelection(mock, "", "" /* both empty — nothing selected */)

	if err := d.Check(map[string]string{"job.hcl": `job "some-job" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs with no selection criteria, got %d: %+v", len(diffs), diffs)
	}
}

// TestDiffer_SelectedJobs_MetaReason verifies that a job selected via the meta
// key is returned by SelectedJobs with reason "meta".
func TestDiffer_SelectedJobs_MetaReason(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("meta-job"), Meta: map[string]string{"gitops_managed": "true"}}, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}
	d := newTestDifferWithSelection(mock, "", "gitops")

	if err := d.Check(map[string]string{"meta-job.hcl": `job "meta-job" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	jobs, _, _ := d.SelectedJobs()
	if len(jobs) != 1 || jobs[0].JobID != "meta-job" || jobs[0].Reason != nomad.SelectionReasonMeta {
		t.Errorf("want 1 job with reason meta, got %+v", jobs)
	}
}

// TestDiffer_SelectedJobs_GlobReason verifies that a job selected via the glob
// is returned by SelectedJobs with reason "glob".
func TestDiffer_SelectedJobs_GlobReason(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("prod-api"), Meta: nil}, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}
	d := newTestDifferWithSelection(mock, "prod-*", "")

	if err := d.Check(map[string]string{"prod-api.hcl": `job "prod-api" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	jobs, _, _ := d.SelectedJobs()
	if len(jobs) != 1 || jobs[0].JobID != "prod-api" || jobs[0].Reason != nomad.SelectionReasonGlob {
		t.Errorf("want 1 job with reason glob, got %+v", jobs)
	}
}

// TestDiffer_SelectedJobs_BothReason verifies that a job matching both the glob
// and the meta key is returned with reason "both".
func TestDiffer_SelectedJobs_BothReason(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("prod-api"), Meta: map[string]string{"gitops_managed": "true"}}, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}
	d := newTestDifferWithSelection(mock, "prod-*", "gitops")

	if err := d.Check(map[string]string{"prod-api.hcl": `job "prod-api" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	jobs, _, _ := d.SelectedJobs()
	if len(jobs) != 1 || jobs[0].JobID != "prod-api" || jobs[0].Reason != nomad.SelectionReasonBoth {
		t.Errorf("want 1 job with reason both, got %+v", jobs)
	}
}

// TestDiffer_SelectedJobs_NoDuplicates verifies that a job appearing in both the
// HCL phase and the Nomad list phase is returned only once. The live Nomad job
// carries the meta key so it passes Nomad-canonical selection.
func TestDiffer_SelectedJobs_NoDuplicates(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("shared-job"), Meta: map[string]string{"gitops_managed": "true"}}, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		s := "running"
		return &nomadapi.Job{ID: strPtr("shared-job"), Status: &s, Meta: map[string]string{"gitops_managed": "true"}}, nil, nil
	}
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{
			{ID: "shared-job", Status: "running", Meta: map[string]string{"gitops_managed": "true"}},
		}, nil, nil
	}
	mock.planFn = func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
		return &nomadapi.JobPlanResponse{}, nil, nil
	}
	d := newTestDifferWithSelection(mock, "", "gitops")

	if err := d.Check(map[string]string{"shared-job.hcl": `job "shared-job" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	jobs, _, _ := d.SelectedJobs()
	if len(jobs) != 1 {
		t.Errorf("want 1 unique selected job, got %d: %+v", len(jobs), jobs)
	}
}

// TestDiffer_GitIsIntent_MetaInHCLNotNomad verifies the default behaviour:
// a job whose HCL carries the managed meta key is selected even when the
// live Nomad instance does NOT carry it (Git is intent). The only difference
// — the missing meta key — is a managed-meta-only diff, so by default it is
// NOT counted as drift and NOT applied; it is surfaced via its own counter
// and converges on the next real update.
func TestDiffer_GitIsIntent_MetaInHCLNotNomad(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("not-yet-managed"), Meta: map[string]string{"gitops_managed": "true"}}, nil
	}
	// Live job exists but has no meta key.
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		s := "running"
		return &nomadapi.Job{ID: strPtr("not-yet-managed"), Status: &s, Meta: nil}, nil, nil
	}
	// The plan reports only the meta addition as drift.
	mock.planFn = func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
		return &nomadapi.JobPlanResponse{Diff: &nomadapi.JobDiff{
			Type: "Edited",
			Objects: []*nomadapi.ObjectDiff{
				{Type: "Edited", Name: "Meta", Fields: []*nomadapi.FieldDiff{
					{Type: "Added", Name: "gitops_managed", New: "true"},
				}},
			},
		}}, nil, nil
	}
	d := newTestDifferWithSelection(mock, "", "gitops")

	if err := d.Check(map[string]string{"not-yet-managed.hcl": `job "not-yet-managed" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	jobs, _, _ := d.SelectedJobs()
	if len(jobs) != 1 || jobs[0].JobID != "not-yet-managed" {
		t.Fatalf("HCL opt-in key should select the job even without the live key, got %+v", jobs)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("a managed-meta-only difference must not be counted as drift by default, got %+v", diffs)
	}
	if got := testutil.ToFloat64(nomad.MetaOnlyDiffs(d).WithLabelValues("not-yet-managed")); got != 1 {
		t.Errorf("meta_only_diffs_total: want 1, got %v", got)
	}
}

// TestDiffer_GitIsIntent_LiveKeyNeverOverridesHCL verifies that when a job's
// HCL exists in the repo without the managed key, a stale key on the live
// job does not select it: Git is always the source of truth for
// nomad-botherer's own keys. The job is neither diffed nor reported as
// missing_from_hcl.
func TestDiffer_GitIsIntent_LiveKeyNeverOverridesHCL(t *testing.T) {
	mock := defaultMock()
	// HCL job has no meta key: Git says "not managed".
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("formerly-managed"), Meta: nil}, nil
	}
	// Live Nomad list shows the job with a stale meta key.
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{
			{ID: "formerly-managed", Status: "running", Meta: map[string]string{"gitops_managed": "true"}},
		}, nil, nil
	}
	d := newTestDifferWithSelection(mock, "", "gitops")

	if err := d.Check(map[string]string{"formerly-managed.hcl": `job "formerly-managed" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("a stale live key must not produce diffs when the HCL opts out, got %+v", diffs)
	}
	jobs, _, _ := d.SelectedJobs()
	if len(jobs) != 0 {
		t.Errorf("a stale live key must not select the job, got %+v", jobs)
	}
}

// TestDiffer_LiveKeySelects_WhenNoHCLExists verifies that for a job Git
// knows nothing about (no HCL file at all), the live key still selects it —
// that is how missing_from_hcl is detected.
func TestDiffer_LiveKeySelects_WhenNoHCLExists(t *testing.T) {
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{
			{ID: "orphan-job", Status: "running", Meta: map[string]string{"gitops_managed": "true"}},
		}, nil, nil
	}
	d := newTestDifferWithSelection(mock, "", "gitops")

	if err := d.Check(map[string]string{}, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 || diffs[0].DiffType != nomad.DiffTypeMissingFromHCL {
		t.Errorf("want 1 missing_from_hcl diff for a live-only managed job, got %+v", diffs)
	}
}

// TestDiffer_NomadCanonical_MissingFromNomad_HCLFallback verifies that an HCL
// job with the managed meta key that does not yet exist in Nomad is still
// selected (HCL is used as fallback when there is no live job to consult).
func TestDiffer_NomadCanonical_MissingFromNomad_HCLFallback(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("new-job"), Meta: map[string]string{"gitops_managed": "true"}}, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return nil, nil, fmt.Errorf("404: not found")
	}
	d := newTestDifferWithSelection(mock, "", "gitops")

	if err := d.Check(map[string]string{"new-job.hcl": `job "new-job" {}`}, "abc"); err != nil {
		t.Fatal(err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 || diffs[0].DiffType != nomad.DiffTypeMissingFromNomad {
		t.Errorf("want 1 missing_from_nomad diff (HCL fallback for new job), got %+v", diffs)
	}
}

// ── mergeSelectionReason ──────────────────────────────────────────────────────

func TestMergeSelectionReason_EmptyExisting(t *testing.T) {
	got := nomad.MergeSelectionReason("", nomad.SelectionReasonGlob)
	if got != nomad.SelectionReasonGlob {
		t.Errorf("want glob, got %v", got)
	}
}

func TestMergeSelectionReason_SameReason(t *testing.T) {
	got := nomad.MergeSelectionReason(nomad.SelectionReasonMeta, nomad.SelectionReasonMeta)
	if got != nomad.SelectionReasonMeta {
		t.Errorf("want meta, got %v", got)
	}
}

func TestMergeSelectionReason_DifferentReasons(t *testing.T) {
	got := nomad.MergeSelectionReason(nomad.SelectionReasonGlob, nomad.SelectionReasonMeta)
	if got != nomad.SelectionReasonBoth {
		t.Errorf("want both, got %v", got)
	}
}

// ── hasContentDiff ────────────────────────────────────────────────────────────

func TestHasContentDiff_Nil(t *testing.T) {
	if nomad.HasContentDiff(nil) {
		t.Error("nil diff should not be a content diff")
	}
}

func TestHasContentDiff_TypeNone(t *testing.T) {
	d := &nomadapi.JobDiff{Type: "None"}
	if nomad.HasContentDiff(d) {
		t.Error("Type=None should not be a content diff")
	}
}

func TestHasContentDiff_TopLevelField(t *testing.T) {
	d := &nomadapi.JobDiff{
		Type:   "Edited",
		Fields: []*nomadapi.FieldDiff{{Name: "Priority", Type: "Edited"}},
	}
	if !nomad.HasContentDiff(d) {
		t.Error("top-level field diff should be a content diff")
	}
}

func TestHasContentDiff_TopLevelObject(t *testing.T) {
	d := &nomadapi.JobDiff{
		Type:    "Edited",
		Objects: []*nomadapi.ObjectDiff{{Type: "Edited"}},
	}
	if !nomad.HasContentDiff(d) {
		t.Error("top-level object diff should be a content diff")
	}
}

func TestHasContentDiff_TaskGroupAdded(t *testing.T) {
	d := &nomadapi.JobDiff{
		Type: "Edited",
		TaskGroups: []*nomadapi.TaskGroupDiff{
			{Type: "Added"},
		},
	}
	if !nomad.HasContentDiff(d) {
		t.Error("Added task group should be a content diff")
	}
}

func TestHasContentDiff_TaskGroupFieldOnly(t *testing.T) {
	d := &nomadapi.JobDiff{
		Type: "Edited",
		TaskGroups: []*nomadapi.TaskGroupDiff{
			{
				Type:   "Edited",
				Fields: []*nomadapi.FieldDiff{{Name: "Count", Type: "Edited"}},
			},
		},
	}
	if !nomad.HasContentDiff(d) {
		t.Error("task group field diff should be a content diff")
	}
}

func TestHasContentDiff_TaskGroupObjectOnly(t *testing.T) {
	d := &nomadapi.JobDiff{
		Type: "Edited",
		TaskGroups: []*nomadapi.TaskGroupDiff{
			{
				Type:    "Edited",
				Objects: []*nomadapi.ObjectDiff{{Type: "Edited"}},
			},
		},
	}
	if !nomad.HasContentDiff(d) {
		t.Error("task group object diff should be a content diff")
	}
}

func TestHasContentDiff_TaskEdited(t *testing.T) {
	d := &nomadapi.JobDiff{
		Type: "Edited",
		TaskGroups: []*nomadapi.TaskGroupDiff{
			{
				Type: "Edited",
				Tasks: []*nomadapi.TaskDiff{
					{Type: "Edited", Fields: []*nomadapi.FieldDiff{{Name: "Driver"}}},
				},
			},
		},
	}
	if !nomad.HasContentDiff(d) {
		t.Error("edited task should be a content diff")
	}
}

func TestHasContentDiff_AllTaskGroupsNone(t *testing.T) {
	d := &nomadapi.JobDiff{
		Type: "Edited",
		TaskGroups: []*nomadapi.TaskGroupDiff{
			{
				Type: "None",
				Tasks: []*nomadapi.TaskDiff{
					{Type: "None"},
				},
			},
		},
	}
	if nomad.HasContentDiff(d) {
		t.Error("all-None task groups should not be a content diff")
	}
}
