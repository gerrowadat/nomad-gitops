package nomad_test

import (
	"fmt"
	"strings"
	"testing"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

// metaOnlyManagedDiff is a plan diff whose only change is one of our own
// meta keys being added to the live job.
func metaOnlyManagedDiff() *nomadapi.JobDiff {
	return &nomadapi.JobDiff{
		Type: "Edited", ID: "test-job",
		Objects: []*nomadapi.ObjectDiff{
			{Type: "Edited", Name: "Meta", Fields: []*nomadapi.FieldDiff{
				{Type: "Added", Name: "gitops_managed", New: "true"},
			}},
		},
	}
}

// editedImagePlusMetaDiff is a plan diff with a Docker image change plus one
// of our own meta keys.
func editedImagePlusMetaDiff() *nomadapi.JobDiff {
	return &nomadapi.JobDiff{
		Type: "Edited", ID: "test-job",
		Objects: []*nomadapi.ObjectDiff{
			{Type: "Edited", Name: "Meta", Fields: []*nomadapi.FieldDiff{
				{Type: "Added", Name: "gitops_managed", New: "true"},
			}},
		},
		TaskGroups: []*nomadapi.TaskGroupDiff{
			{Type: "Edited", Name: "web", Tasks: []*nomadapi.TaskDiff{taskDiffWithConfig(imageField())}},
		},
	}
}

func TestApply_MetaOnlyDiff_DefaultLeavesJobAlone(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, metaOnlyManagedDiff(), 42, &calls)
	d := nomad.NewWithClient(applyCfg("none", false), mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("a meta-only diff must not register by default even under policy full, got %d calls", len(calls))
	}
	if updates := d.Updates(); len(updates) != 0 {
		t.Errorf("a meta-only diff must not enqueue an update, got %+v", updates)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 0 {
		t.Errorf("a meta-only diff must not be counted as drift by default, got %+v", diffs)
	}
	if got := testutil.ToFloat64(nomad.MetaOnlyDiffs(d).WithLabelValues("test-job")); got != 1 {
		t.Errorf("meta_only_diffs_total: want 1, got %v", got)
	}
}

func TestApply_MetaOnlyDiff_AppliedWithFlag(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, metaOnlyManagedDiff(), 42, &calls)
	cfg := applyCfg("none", false)
	cfg.ApplyMetaOnlyChanges = true
	d := nomad.NewWithClient(cfg, mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("with --apply-meta-only-changes a meta-only diff under full should register, got %d calls", len(calls))
	}
}

func TestApply_MetaOnlyDiff_CountedWithFlag(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true"}
	mock := applyMock(meta, metaOnlyManagedDiff(), 42, &calls)
	cfg := applyCfg("none", false)
	cfg.CountMetaOnlyChanges = true
	d := nomad.NewWithClient(cfg, mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 || diffs[0].DiffType != nomad.DiffTypeModified {
		t.Errorf("with --count-meta-only-changes a meta-only diff should be counted, got %+v", diffs)
	}
	// Counting is independent of applying: still no register under policy none.
	if len(calls) != 0 {
		t.Errorf("counting must not imply applying, got %d calls", len(calls))
	}
}

func TestApply_ImagePlusMeta_ImageOnlyPolicy_ConvergesMetaOpportunistically(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "image-only"}
	mock := applyMock(meta, editedImagePlusMetaDiff(), 42, &calls)
	d := nomad.NewWithClient(applyCfg("none", false), mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Fatalf("image+meta under image-only policy should apply, got %d calls", len(calls))
	}
	// The registered job is the full HCL job, so the meta keys ride along.
	if calls[0].job.Meta["gitops_managed"] != "true" {
		t.Errorf("the applied job should carry the HCL meta keys, got %v", calls[0].job.Meta)
	}
}

func TestApply_MetaOnlyDiff_ImageOnlyPolicy_BlockedEvenWithApplyFlag(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_update_policy": "image-only"}
	mock := applyMock(meta, metaOnlyManagedDiff(), 42, &calls)
	cfg := applyCfg("none", false)
	cfg.ApplyMetaOnlyChanges = true
	d := nomad.NewWithClient(cfg, mock)

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("a pure meta change is not an image change; image-only policy must block it, got %d calls", len(calls))
	}
}

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

// metaSelectCfg selects jobs by meta only (no glob), so the pre-existing-drift
// gate (which exempts glob-selected jobs) applies.
func metaSelectCfg(applyExisting bool) *config.Config {
	return &config.Config{
		NomadNamespace:      "default",
		ManagedMetaPrefix:   "gitops",
		DefaultUpdatePolicy: "none",
		ApplyExistingDrift:  applyExisting,
	}
}

// fakeHistory implements nomad.HistorySource. A path present in parent returns
// that content (ok=true); an absent path returns ok=false (file new at the
// commit). The commit argument is ignored: these tests use a single commit per
// job, so keying off the path is sufficient.
type fakeHistory struct {
	parent map[string]string
}

func (f fakeHistory) FileAtParentOf(_, path string) (string, bool) {
	c, ok := f.parent[path]
	return c, ok
}

const testJobFile = "jobs/test-job.hcl"

// optInMock returns a mock whose HCL job carries the gitops keys (opted in at
// HEAD); the live job exists with a real image diff (pre-existing drift).
func optInMock(calls *[]registerCall) *mockJobsClient {
	mock := applyMock(map[string]string{
		"gitops_managed": "true", "gitops_update_policy": "full",
	}, editedImagePlusMetaDiff(), 42, calls)
	return mock
}

func TestApply_ExistingDrift_TagAddedAtHead_NotApplied(t *testing.T) {
	var calls []registerCall
	d := nomad.NewWithClient(metaSelectCfg(false), optInMock(&calls))
	// The tag was added at HEAD: the parent version of the file lacks it.
	d.SetHistorySource(fakeHistory{parent: map[string]string{testJobFile: `job "test-job" {}`}})

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("drift that pre-dates opt-in must not apply by default, got %d calls", len(calls))
	}
	if got := testutil.ToFloat64(nomad.UpdatesBlockedExistingDrift(d).WithLabelValues("test-job")); got != 1 {
		t.Errorf("blocked-preexisting metric: want 1, got %v", got)
	}
	// The diff is still surfaced, annotated with the reason.
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 || diffs[0].ApplyAction != nomad.ApplyActionPreExisting {
		t.Errorf("diff should be surfaced with the pre-existing reason, got %+v", diffs)
	}
}

func TestApply_ExistingDrift_TagAddedAtHead_AppliedWithFlag(t *testing.T) {
	var calls []registerCall
	d := nomad.NewWithClient(metaSelectCfg(true), optInMock(&calls))
	d.SetHistorySource(fakeHistory{parent: map[string]string{testJobFile: `job "test-job" {}`}})

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("with --apply-existing-drift the pre-existing drift should apply, got %d calls", len(calls))
	}
}

// TestApply_ExistingDrift_TagPresentAtParent_Applied is the established /
// reconcile-on-start case: the tag and the same effective policy were already
// present before HEAD, so the job is not freshly opted in, its scope did not
// widen, and its drift reconciles normally.
func TestApply_ExistingDrift_TagPresentAtParent_Applied(t *testing.T) {
	var calls []registerCall
	d := nomad.NewWithClient(metaSelectCfg(false), optInMock(&calls))
	d.SetHistorySource(fakeHistory{parent: map[string]string{
		testJobFile: `job "test-job" { meta { gitops_managed = "true" gitops_update_policy = "full" } }`,
	}})

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("an established job (tag and policy present before HEAD) should reconcile, got %d calls", len(calls))
	}
}

// TestApply_ExistingDrift_NoHistory_Applied verifies that without a history
// source the gate is skipped so reconciliation is not broken.
func TestApply_ExistingDrift_NoHistory_Applied(t *testing.T) {
	var calls []registerCall
	d := nomad.NewWithClient(metaSelectCfg(false), optInMock(&calls))
	// No SetHistorySource call.

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("without history the pre-existing gate must not block, got %d calls", len(calls))
	}
}

// TestApply_ExistingDrift_GlobSelectedExempt verifies that glob-selected jobs
// (no opt-in moment) are never frozen, even when the tag was added at HEAD.
func TestApply_ExistingDrift_GlobSelectedExempt(t *testing.T) {
	var calls []registerCall
	cfg := metaSelectCfg(false)
	cfg.JobSelectorGlob = "*"
	d := nomad.NewWithClient(cfg, optInMock(&calls))
	d.SetHistorySource(fakeHistory{parent: map[string]string{testJobFile: `job "test-job" {}`}})

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("glob-selected jobs have no opt-in moment and must not be frozen, got %d calls", len(calls))
	}
}

// TestApply_ExistingDrift_FileNewAtHead_Applied verifies that a file created
// at HEAD (no parent version) is not treated as a retroactive opt-in: the tag
// and spec arrived together, so the drift is applied (subject to policy).
func TestApply_ExistingDrift_FileNewAtHead_Applied(t *testing.T) {
	var calls []registerCall
	d := nomad.NewWithClient(metaSelectCfg(false), optInMock(&calls))
	d.SetHistorySource(fakeHistory{parent: map[string]string{}}) // file absent at parent

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("a file created with the tag at HEAD should apply (tag present when spec introduced), got %d calls", len(calls))
	}
}

// promotionMock builds a mock for a job managed at HEAD with the given policy,
// drifting against the live job per planDiff.
func promotionMock(policy string, planDiff *nomadapi.JobDiff, calls *[]registerCall) *mockJobsClient {
	return applyMock(map[string]string{
		"gitops_managed": "true", "gitops_update_policy": policy,
	}, planDiff, 42, calls)
}

// parentWithPolicy is a history source whose parent version of the job file is
// managed with the given policy.
func parentWithPolicy(policy string) fakeHistory {
	return fakeHistory{parent: map[string]string{
		testJobFile: fmt.Sprintf(`job "test-job" { meta { gitops_managed = "true" gitops_update_policy = %q } }`, policy),
	}}
}

// Issue #69: a policy change that widens what is applied (e.g. image-only → full)
// must treat drift that accumulated under the stricter policy the same way an
// opt-in treats pre-existing drift — deferred by default, applied with the flag.

func TestApply_PolicyPromotion_ImageOnlyToFull_DefersAccumulatedDrift(t *testing.T) {
	var calls []registerCall
	d := nomad.NewWithClient(metaSelectCfg(false), promotionMock("full", editedMixedDiff(), &calls))
	d.SetHistorySource(parentWithPolicy("image-only"))

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("non-image drift accumulated under image-only must not apply on promotion to full by default, got %d calls", len(calls))
	}
	if got := testutil.ToFloat64(nomad.UpdatesBlockedExistingDrift(d).WithLabelValues("test-job")); got != 1 {
		t.Errorf("blocked-preexisting metric: want 1, got %v", got)
	}
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 || diffs[0].ApplyAction != nomad.ApplyActionPreExisting {
		t.Errorf("diff should surface the pre-existing reason, got %+v", diffs)
	}
}

func TestApply_PolicyPromotion_ImageOnlyToFull_AppliedWithFlag(t *testing.T) {
	var calls []registerCall
	d := nomad.NewWithClient(metaSelectCfg(true), promotionMock("full", editedMixedDiff(), &calls))
	d.SetHistorySource(parentWithPolicy("image-only"))

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("with --apply-existing-drift the accumulated drift should apply on promotion, got %d calls", len(calls))
	}
}

func TestApply_PolicyPromotion_NoneToFull_DefersAccumulatedDrift(t *testing.T) {
	// none → full: all drift was out of scope under none, so it is pre-existing.
	var calls []registerCall
	d := nomad.NewWithClient(metaSelectCfg(false), promotionMock("full", editedMixedDiff(), &calls))
	d.SetHistorySource(parentWithPolicy("none"))

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("drift accumulated under none must not apply on promotion to full by default, got %d calls", len(calls))
	}
}

func TestApply_PolicyPromotion_ImageDriftRemainsInScope_Applies(t *testing.T) {
	// image-only → full where the pending drift is image-class: it was already in
	// scope under image-only, so promotion does not defer it.
	var calls []registerCall
	d := nomad.NewWithClient(metaSelectCfg(false), promotionMock("full", editedImageDiff(), &calls))
	d.SetHistorySource(parentWithPolicy("image-only"))

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("image drift already in scope under image-only should apply on promotion to full, got %d calls", len(calls))
	}
}

func TestApply_PolicyStable_NoWidening_Applies(t *testing.T) {
	// Same policy (full) at parent and HEAD: no widening, so drift reconciles
	// normally even though it pre-dates the commit being evaluated.
	var calls []registerCall
	d := nomad.NewWithClient(metaSelectCfg(false), promotionMock("full", editedMixedDiff(), &calls))
	d.SetHistorySource(parentWithPolicy("full"))

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("a stable full policy should reconcile drift normally, got %d calls", len(calls))
	}
}

func TestApply_PolicyPromotion_MetaOnlyChange_NotPreExisting(t *testing.T) {
	// A commit that only changes gitops_* meta (no other drift) is governed by
	// the meta-only gate, not the pre-existing gate.
	var calls []registerCall
	d := nomad.NewWithClient(metaSelectCfg(false), promotionMock("full", metaOnlyManagedDiff(), &calls))
	d.SetHistorySource(parentWithPolicy("image-only"))

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if got := testutil.ToFloat64(nomad.UpdatesBlockedExistingDrift(d).WithLabelValues("test-job")); got != 0 {
		t.Errorf("a meta-only change must not be flagged pre-existing, got %v", got)
	}
	if len(calls) != 0 {
		t.Errorf("a meta-only change should not register by default, got %d calls", len(calls))
	}
}
