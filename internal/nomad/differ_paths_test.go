package nomad_test

import (
	"testing"

	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

func boolPtr(b bool) *bool { return &b }

// TestNewDiffer_RealClient verifies the production constructor builds a Differ
// from config without contacting the cluster. Called once per test binary:
// it registers metrics into the default Prometheus registry.
func TestNewDiffer_RealClient(t *testing.T) {
	d, err := nomad.NewDiffer(&config.Config{
		NomadAddr:      "http://127.0.0.1:4646",
		NomadToken:     "test-token",
		NomadNamespace: "default",
	})
	if err != nil {
		t.Fatalf("NewDiffer: %v", err)
	}
	if d == nil {
		t.Fatal("NewDiffer returned nil Differ")
	}
	if d.Ready() {
		t.Error("a fresh Differ should not be Ready before any check")
	}
}

func TestNewDiffer_BadAddress(t *testing.T) {
	_, err := nomad.NewDiffer(&config.Config{NomadAddr: "://not-a-url"})
	if err == nil {
		t.Error("NewDiffer with an unparseable address should fail")
	}
}

// TestDiffer_StoppedJob_StopCopiedToPlan verifies that when the live job has
// Stop=true (deregistered), the flag is copied onto the HCL job before
// planning so the plan does not report a spurious Stop field diff.
func TestDiffer_StoppedJob_StopCopiedToPlan(t *testing.T) {
	var plannedStop *bool
	mock := defaultMock()
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return &nomadapi.Job{ID: strPtr(jobID), Stop: boolPtr(true), Status: strPtr("running")}, nil, nil
	}
	mock.planFn = func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
		plannedStop = job.Stop
		return &nomadapi.JobPlanResponse{Diff: &nomadapi.JobDiff{Type: "None"}}, nil, nil
	}
	d := newTestDiffer(mock)

	if err := d.Check(map[string]string{"jobs/test-job.hcl": `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plannedStop == nil || !*plannedStop {
		t.Error("Stop=true on the live job should be copied onto the planned job")
	}
}

// TestDiffer_DeadJob_BookkeepingOnlyDiff_Suppressed verifies that with
// --include-dead-jobs, a plan against a dead job whose diff contains only
// allocation bookkeeping (Type="None" task groups) is not reported as drift.
func TestDiffer_DeadJob_BookkeepingOnlyDiff_Suppressed(t *testing.T) {
	mock := defaultMock()
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return &nomadapi.Job{ID: strPtr(jobID), Status: strPtr("dead")}, nil, nil
	}
	mock.planFn = func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
		return &nomadapi.JobPlanResponse{Diff: &nomadapi.JobDiff{
			Type: "Edited",
			TaskGroups: []*nomadapi.TaskGroupDiff{
				{Type: "None", Name: "web"},
			},
		}}, nil, nil
	}
	d := newTestDifferWithDeadJobs(mock)

	if err := d.Check(map[string]string{"jobs/test-job.hcl": `job "test-job" {}`}, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("bookkeeping-only diff on a dead job should be suppressed, got %+v", diffs)
	}
}
