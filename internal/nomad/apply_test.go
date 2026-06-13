package nomad_test

import (
	"fmt"
	"strings"
	"testing"

	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

func uint64Ptr(v uint64) *uint64 { return &v }

// registerCall records the arguments of a RegisterOpts invocation.
type registerCall struct {
	job  *nomadapi.Job
	opts *nomadapi.RegisterOptions
}

// editedImageDiff returns a plan diff confined to a Docker image change.
func editedImageDiff() *nomadapi.JobDiff {
	return wrapJobDiff(&nomadapi.TaskGroupDiff{
		Type: "Edited", Name: "web",
		Tasks: []*nomadapi.TaskDiff{taskDiffWithConfig(imageField())},
	})
}

// editedMixedDiff returns a plan diff with an image change plus an env change.
func editedMixedDiff() *nomadapi.JobDiff {
	return wrapJobDiff(&nomadapi.TaskGroupDiff{
		Type: "Edited", Name: "web",
		Tasks: []*nomadapi.TaskDiff{
			{
				Type: "Edited", Name: "app",
				Fields: []*nomadapi.FieldDiff{
					{Type: "Edited", Name: "Env[FOO]", Old: "a", New: "b"},
				},
				Objects: []*nomadapi.ObjectDiff{
					{Type: "Edited", Name: "Config", Fields: []*nomadapi.FieldDiff{imageField()}},
				},
			},
		},
	})
}

// applyMock builds a mock where test-job exists live (ModifyIndex liveIndex),
// the plan reports planDiff, the parsed HCL job carries meta, and register
// calls are recorded into calls.
func applyMock(meta map[string]string, planDiff *nomadapi.JobDiff, liveIndex uint64, calls *[]registerCall) *mockJobsClient {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("test-job"), Meta: meta}, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return &nomadapi.Job{ID: strPtr(jobID), Status: strPtr("running"), JobModifyIndex: uint64Ptr(liveIndex)}, nil, nil
	}
	mock.planFn = func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
		return &nomadapi.JobPlanResponse{Diff: planDiff}, nil, nil
	}
	mock.registerFn = func(job *nomadapi.Job, opts *nomadapi.RegisterOptions, q *nomadapi.WriteOptions) (*nomadapi.JobRegisterResponse, *nomadapi.WriteMeta, error) {
		*calls = append(*calls, registerCall{job: job, opts: opts})
		return &nomadapi.JobRegisterResponse{EvalID: "eval-1", JobModifyIndex: liveIndex + 1}, nil, nil
	}
	return mock
}

func applyCfg(defaultPolicy string, enableCreation bool) *config.Config {
	return &config.Config{
		NomadNamespace:      "default",
		JobSelectorGlob:     "*",
		ManagedMetaPrefix:   "gitops",
		DefaultUpdatePolicy: defaultPolicy,
		EnableJobCreation:   enableCreation,
	}
}

const testHCL = `job "test-job" {}`

func runCheck(t *testing.T, d *nomad.Differ, commit string) {
	t.Helper()
	if err := d.Check(map[string]string{"jobs/test-job.hcl": testHCL}, commit); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestApply_DefaultPolicyNone_NothingApplied(t *testing.T) {
	var calls []registerCall
	mock := applyMock(nil, editedMixedDiff(), 42, &calls)
	d := nomad.NewWithClient(applyCfg("none", false), mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("default policy none must never register, got %d calls", len(calls))
	}
	if updates := d.Updates(); len(updates) != 0 {
		t.Errorf("no updates should be enqueued, got %+v", updates)
	}
	// The drift itself is still observed.
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Errorf("diff should still be surfaced, got %d", len(diffs))
	}
}

func TestApply_MetaPolicyFull_RegistersWithCAS(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d := nomad.NewWithClient(applyCfg("none", false), mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Fatalf("expected 1 register call, got %d", len(calls))
	}
	if !calls[0].opts.EnforceIndex {
		t.Error("register must use EnforceIndex (CAS)")
	}
	if calls[0].opts.ModifyIndex != 42 {
		t.Errorf("CAS token should be the detection-time ModifyIndex 42, got %d", calls[0].opts.ModifyIndex)
	}

	updates := d.Updates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	u := updates[0]
	if u.Status != nomad.JobUpdateStatusSucceeded {
		t.Errorf("status: want SUCCEEDED, got %s (error %q)", u.Status, u.Error)
	}
	if u.AppliedAt == "" {
		t.Error("AppliedAt should be set")
	}
	if u.NomadJobModifyIndex != 43 {
		t.Errorf("update should record the post-apply ModifyIndex 43, got %d", u.NomadJobModifyIndex)
	}
	if u.UpdateID != "test-job/aaaa111" {
		t.Errorf("unexpected UpdateID %q", u.UpdateID)
	}
}

func TestApply_DefaultPolicyFull_AppliesWithoutMeta(t *testing.T) {
	var calls []registerCall
	mock := applyMock(map[string]string{"gitops_managed": "true"}, editedMixedDiff(), 42, &calls)
	d := nomad.NewWithClient(applyCfg("full", false), mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("default policy full should apply, got %d register calls", len(calls))
	}
}

func TestApply_MetaOverridesDefault_NoneBlocksFullDefault(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "none"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d := nomad.NewWithClient(applyCfg("full", false), mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("meta policy none must override default full, got %d register calls", len(calls))
	}
}

func TestApply_ImageOnlyPolicy(t *testing.T) {
	t.Run("image-only diff applies", func(t *testing.T) {
		var calls []registerCall
		meta := map[string]string{"gitops_update_policy": "image-only"}
		mock := applyMock(meta, editedImageDiff(), 42, &calls)
		d := nomad.NewWithClient(applyCfg("none", false), mock)

		runCheck(t, d, "aaaa111fffff")
		nomad.DrainUpdates(d)

		if len(calls) != 1 {
			t.Errorf("image-only diff should apply under image-only policy, got %d calls", len(calls))
		}
	})

	t.Run("mixed diff blocked", func(t *testing.T) {
		var calls []registerCall
		meta := map[string]string{"gitops_update_policy": "image-only"}
		mock := applyMock(meta, editedMixedDiff(), 42, &calls)
		d := nomad.NewWithClient(applyCfg("none", false), mock)

		runCheck(t, d, "aaaa111fffff")
		nomad.DrainUpdates(d)

		if len(calls) != 0 {
			t.Errorf("mixed diff must be blocked under image-only policy, got %d calls", len(calls))
		}
		if updates := d.Updates(); len(updates) != 0 {
			t.Errorf("blocked diff must not enqueue an update, got %+v", updates)
		}
	})
}

func TestApply_InvalidMetaPolicy_TreatedAsNone(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_update_policy": "yolo"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d := nomad.NewWithClient(applyCfg("full", false), mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("unrecognised meta policy must be conservative (none), got %d register calls", len(calls))
	}
}

func TestApply_JobCreation_GatedByFlag(t *testing.T) {
	newMock := func(calls *[]registerCall) *mockJobsClient {
		mock := applyMock(map[string]string{"gitops_update_policy": "full"}, nil, 0, calls)
		mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
			return nil, nil, fmt.Errorf("Unexpected response code: 404 (job not found)")
		}
		mock.planFn = func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
			return &nomadapi.JobPlanResponse{Diff: &nomadapi.JobDiff{
				Type:       "Added",
				TaskGroups: []*nomadapi.TaskGroupDiff{{Type: "Added", Name: "web"}},
			}}, nil, nil
		}
		return mock
	}

	t.Run("disabled", func(t *testing.T) {
		var calls []registerCall
		d := nomad.NewWithClient(applyCfg("none", false), newMock(&calls))
		runCheck(t, d, "aaaa111fffff")
		nomad.DrainUpdates(d)
		if len(calls) != 0 {
			t.Errorf("creation disabled: no register expected, got %d", len(calls))
		}
	})

	t.Run("enabled", func(t *testing.T) {
		var calls []registerCall
		d := nomad.NewWithClient(applyCfg("none", true), newMock(&calls))
		runCheck(t, d, "aaaa111fffff")
		nomad.DrainUpdates(d)
		if len(calls) != 1 {
			t.Fatalf("creation enabled: expected 1 register, got %d", len(calls))
		}
		if !calls[0].opts.EnforceIndex || calls[0].opts.ModifyIndex != 0 {
			t.Errorf("first registration must enforce ModifyIndex 0 (job must not exist), got %+v", calls[0].opts)
		}
	})

	t.Run("enabled but policy image-only", func(t *testing.T) {
		var calls []registerCall
		mock := newMock(&calls)
		mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
			return &nomadapi.Job{ID: strPtr("test-job"), Meta: map[string]string{"gitops_update_policy": "image-only"}}, nil
		}
		d := nomad.NewWithClient(applyCfg("none", true), mock)
		runCheck(t, d, "aaaa111fffff")
		nomad.DrainUpdates(d)
		if len(calls) != 0 {
			t.Errorf("initial registration is full-only; image-only must not register, got %d", len(calls))
		}
	})
}

func TestApply_DeadJob_CASTokenFromDeadJob(t *testing.T) {
	var calls []registerCall
	mock := applyMock(map[string]string{"gitops_update_policy": "full"}, editedImageDiff(), 0, &calls)
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return &nomadapi.Job{ID: strPtr(jobID), Status: strPtr("dead"), JobModifyIndex: uint64Ptr(77)}, nil, nil
	}
	d := nomad.NewWithClient(applyCfg("none", true), mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Fatalf("expected 1 register, got %d", len(calls))
	}
	if calls[0].opts.ModifyIndex != 77 {
		t.Errorf("dead job still exists in state: CAS token must be its ModifyIndex 77, got %d", calls[0].opts.ModifyIndex)
	}
}

func TestApply_RegisterError_FailedAndSkipInvalidated(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	mock.registerFn = func(job *nomadapi.Job, opts *nomadapi.RegisterOptions, q *nomadapi.WriteOptions) (*nomadapi.JobRegisterResponse, *nomadapi.WriteMeta, error) {
		return nil, nil, fmt.Errorf("Enforcing job modify index 42: job exists with conflicting job modify index: 50")
	}
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return nil, &nomadapi.QueryMeta{LastIndex: 100}, nil
	}
	d := nomad.NewWithClient(applyCfg("none", false), mock)

	runCheck(t, d, "aaaa111fffff")
	if nomad.LastNomadIndex(d) != 100 {
		t.Fatalf("precondition: Check should cache the Raft index, got %d", nomad.LastNomadIndex(d))
	}
	nomad.DrainUpdates(d)

	updates := d.Updates()
	if len(updates) != 1 || updates[0].Status != nomad.JobUpdateStatusFailed {
		t.Fatalf("expected 1 FAILED update, got %+v", updates)
	}
	if !strings.Contains(updates[0].Error, "conflicting job modify index") {
		t.Errorf("update should carry the register error, got %q", updates[0].Error)
	}
	if nomad.LastNomadIndex(d) != 0 {
		t.Error("failed apply must invalidate the skip optimisation so the next cycle re-detects")
	}
}

func TestApply_PlanShowsNoChange_NoopSuccess(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	planCalls := 0
	mock.planFn = func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
		planCalls++
		if planCalls == 1 {
			// Detection-time plan: drift present.
			return &nomadapi.JobPlanResponse{Diff: editedMixedDiff()}, nil, nil
		}
		// Apply-time plan: drift resolved in the meantime.
		return &nomadapi.JobPlanResponse{Diff: &nomadapi.JobDiff{Type: "None"}}, nil, nil
	}
	d := nomad.NewWithClient(applyCfg("none", false), mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("no-op apply must not register, got %d calls", len(calls))
	}
	updates := d.Updates()
	if len(updates) != 1 || updates[0].Status != nomad.JobUpdateStatusSucceeded {
		t.Errorf("no-op apply should complete SUCCEEDED, got %+v", updates)
	}
}

func TestApply_AutoscaledJob_PreservesCounts(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_update_policy": "full"}
	mock := applyMock(meta, editedImageDiff(), 42, &calls)
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{
			ID:   strPtr("test-job"),
			Meta: meta,
			TaskGroups: []*nomadapi.TaskGroup{
				{Name: strPtr("web"), Scaling: &nomadapi.ScalingPolicy{}},
			},
		}, nil
	}
	d := nomad.NewWithClient(applyCfg("none", false), mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Fatalf("expected 1 register, got %d", len(calls))
	}
	if !calls[0].opts.PreserveCounts {
		t.Error("jobs with scaling policies must register with PreserveCounts")
	}
}

func TestApply_NewCommitSupersedesPending(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d := nomad.NewWithClient(applyCfg("none", false), mock)

	// Two commits detected before any apply runs.
	runCheck(t, d, "aaaa111fffff")
	runCheck(t, d, "bbbb222fffff")

	updates := d.Updates()
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(updates))
	}
	if updates[0].Status != nomad.JobUpdateStatusSuperseded {
		t.Errorf("older update should be SUPERSEDED, got %s", updates[0].Status)
	}
	if updates[1].Status != nomad.JobUpdateStatusPending || updates[1].GitCommit != "bbbb222fffff" {
		t.Errorf("newest commit should be the PENDING one, got %+v", updates[1])
	}

	nomad.DrainUpdates(d)
	if len(calls) != 1 {
		t.Errorf("only the newest update should be applied, got %d register calls", len(calls))
	}
}

func TestApply_AutoscalerOnlyChurn_NotEnqueued(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_update_policy": "full"}
	countOnlyDiff := wrapJobDiff(&nomadapi.TaskGroupDiff{
		Type: "Edited", Name: "web",
		Fields: []*nomadapi.FieldDiff{{Type: "Edited", Name: "Count", Old: "2", New: "5"}},
	})
	mock := applyMock(meta, countOnlyDiff, 42, &calls)
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{
			ID:   strPtr("test-job"),
			Meta: meta,
			TaskGroups: []*nomadapi.TaskGroup{
				{Name: strPtr("web"), Scaling: &nomadapi.ScalingPolicy{}},
			},
		}, nil
	}
	d := nomad.NewWithClient(applyCfg("none", false), mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("autoscaler-owned count churn must not be applied, got %d register calls", len(calls))
	}
	if updates := d.Updates(); len(updates) != 0 {
		t.Errorf("autoscaler churn must not enqueue updates, got %+v", updates)
	}
	// The drift remains visible as an observation.
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Errorf("count drift should still be surfaced as a diff, got %d", len(diffs))
	}
}
