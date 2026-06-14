package nomad_test

import (
	"fmt"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

// orphanMock returns a client where one job ("orphan") runs in Nomad with the
// given live meta and has no HCL file. deregisterFn records each call.
func orphanMock(meta map[string]string, deregistered *[]string) *mockJobsClient {
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{{ID: "orphan", Status: "running", Meta: meta}}, nil, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return &nomadapi.Job{ID: strPtr(jobID), Status: strPtr("running"), Meta: meta}, nil, nil
	}
	mock.deregisterFn = func(jobID string, purge bool, q *nomadapi.WriteOptions) (string, *nomadapi.WriteMeta, error) {
		*deregistered = append(*deregistered, jobID)
		return "eval-dereg", nil, nil
	}
	return mock
}

// deregCfg builds a config that selects via meta only, with deregister knobs.
func deregCfg(enable bool, grace time.Duration) *config.Config {
	return &config.Config{
		NomadNamespace:      "default",
		ManagedMetaPrefix:   "gitops",
		DefaultUpdatePolicy: "none",
		EnableDeregister:    enable,
		DeregisterGrace:     grace,
	}
}

// checkNoHCL runs a check with no HCL files (everything is orphaned).
func checkNoHCL(t *testing.T, d *nomad.Differ, commit string) {
	t.Helper()
	if err := d.Check(map[string]string{}, commit); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func orphanDiffAction(t *testing.T, d *nomad.Differ) nomad.ApplyAction {
	t.Helper()
	diffs, _, _ := d.Diffs()
	for _, df := range diffs {
		if df.JobID == "orphan" && df.DiffType == nomad.DiffTypeMissingFromHCL {
			return df.ApplyAction
		}
	}
	t.Fatalf("no missing_from_hcl diff for orphan, got %+v", diffs)
	return ""
}

func TestDeregister_Disabled_ObservationOnly(t *testing.T) {
	var deregistered []string
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	d := nomad.NewWithClient(deregCfg(false, 0), orphanMock(meta, &deregistered))

	checkNoHCL(t, d, "c1")
	nomad.DrainUpdates(d)

	if got := orphanDiffAction(t, d); got != nomad.ApplyActionObservationOnly {
		t.Errorf("deregister disabled: want observation_only, got %q", got)
	}
	if len(deregistered) != 0 {
		t.Errorf("deregister disabled must not deregister, got %v", deregistered)
	}
}

func TestDeregister_PolicyNotFull_Blocked(t *testing.T) {
	var deregistered []string
	// gitops_managed but no policy → effective default "none".
	meta := map[string]string{"gitops_managed": "true"}
	d := nomad.NewWithClient(deregCfg(true, 0), orphanMock(meta, &deregistered))

	checkNoHCL(t, d, "c1")
	checkNoHCL(t, d, "c2")
	nomad.DrainUpdates(d)

	if got := orphanDiffAction(t, d); got != nomad.ApplyActionPolicyBlocked {
		t.Errorf("policy none: want blocked_by_policy, got %q", got)
	}
	if len(deregistered) != 0 {
		t.Errorf("non-full policy must not deregister, got %v", deregistered)
	}
}

func TestDeregister_WithinGrace_Pending(t *testing.T) {
	var deregistered []string
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	// Long grace so the first sighting is never eligible.
	d := nomad.NewWithClient(deregCfg(true, time.Hour), orphanMock(meta, &deregistered))

	checkNoHCL(t, d, "c1")
	checkNoHCL(t, d, "c2")
	nomad.DrainUpdates(d)

	if got := orphanDiffAction(t, d); got != nomad.ApplyActionDeregisterGrace {
		t.Errorf("within grace: want deregister_pending_grace, got %q", got)
	}
	if len(deregistered) != 0 {
		t.Errorf("within grace must not deregister, got %v", deregistered)
	}
}

func TestDeregister_GraceElapsed_Deregisters(t *testing.T) {
	var deregistered []string
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	d := nomad.NewWithClient(deregCfg(true, 0), orphanMock(meta, &deregistered))

	// First cycle records the orphan's first-seen time (grace not yet elapsed).
	checkNoHCL(t, d, "c1")
	nomad.DrainUpdates(d)
	if len(deregistered) != 0 {
		t.Fatalf("first cycle must not deregister, got %v", deregistered)
	}
	// Second cycle: grace (0) has elapsed since first sighting.
	checkNoHCL(t, d, "c2")
	if got := orphanDiffAction(t, d); got != nomad.ApplyActionDeregisterQueued {
		t.Errorf("grace elapsed: want queued_deregister, got %q", got)
	}
	nomad.DrainUpdates(d)
	if len(deregistered) != 1 || deregistered[0] != "orphan" {
		t.Errorf("grace elapsed must deregister the orphan, got %v", deregistered)
	}
}

func TestDeregister_NoTag_NeverDeregisters(t *testing.T) {
	var deregistered []string
	// Orphan selected by glob, with no managed tag → never a deregister candidate.
	mock := orphanMock(nil, &deregistered)
	cfg := deregCfg(true, 0)
	cfg.JobSelectorGlob = "*"
	d := nomad.NewWithClient(cfg, mock)

	checkNoHCL(t, d, "c1")
	checkNoHCL(t, d, "c2")
	nomad.DrainUpdates(d)

	if got := orphanDiffAction(t, d); got != nomad.ApplyActionObservationOnly {
		t.Errorf("untagged orphan: want observation_only, got %q", got)
	}
	if len(deregistered) != 0 {
		t.Errorf("untagged orphan must never deregister, got %v", deregistered)
	}
}

func TestDeregister_Purge_PassedThrough(t *testing.T) {
	var purges []bool
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := orphanMock(meta, &[]string{})
	mock.deregisterFn = func(jobID string, purge bool, q *nomadapi.WriteOptions) (string, *nomadapi.WriteMeta, error) {
		purges = append(purges, purge)
		return "eval", nil, nil
	}
	cfg := deregCfg(true, 0)
	cfg.DeregisterPurge = true
	d := nomad.NewWithClient(cfg, mock)

	checkNoHCL(t, d, "c1")
	checkNoHCL(t, d, "c2")
	nomad.DrainUpdates(d)

	if len(purges) != 1 || !purges[0] {
		t.Errorf("--deregister-purge should pass purge=true, got %v", purges)
	}
}

// ── applier recheck ─────────────────────────────────────────────────────────

func TestDeregister_Recheck_AlreadyGone_NoopSucceeds(t *testing.T) {
	var deregistered []string
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := orphanMock(meta, &deregistered)
	infoCalls := 0
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		infoCalls++
		// Detection-time Info is not used for orphans; the recheck (apply time)
		// finds the job already gone.
		return nil, nil, fmt.Errorf("Unexpected response code: 404 (job not found)")
	}
	d := nomad.NewWithClient(deregCfg(true, 0), mock)

	checkNoHCL(t, d, "c1")
	checkNoHCL(t, d, "c2")
	nomad.DrainUpdates(d)

	if len(deregistered) != 0 {
		t.Errorf("an already-gone job should not be deregistered, got %v", deregistered)
	}
	updates := d.Updates()
	if len(updates) != 1 || updates[0].Status != nomad.JobUpdateStatusSucceeded {
		t.Errorf("already-gone deregister should complete SUCCEEDED, got %+v", updates)
	}
}

func TestDeregister_Recheck_TagGone_Abandoned(t *testing.T) {
	var deregistered []string
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := orphanMock(meta, &deregistered)
	// The List (detection) shows the tag; the Info (recheck) shows it removed.
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return &nomadapi.Job{ID: strPtr(jobID), Status: strPtr("running"), Meta: nil}, nil, nil
	}
	d := nomad.NewWithClient(deregCfg(true, 0), mock)

	checkNoHCL(t, d, "c1")
	checkNoHCL(t, d, "c2")
	nomad.DrainUpdates(d)

	if len(deregistered) != 0 {
		t.Errorf("recheck found the tag gone; must not deregister, got %v", deregistered)
	}
	updates := d.Updates()
	if len(updates) != 1 || updates[0].Status != nomad.JobUpdateStatusFailed {
		t.Errorf("abandoned deregister should be FAILED, got %+v", updates)
	}
}

func TestDeregister_Error_Failed(t *testing.T) {
	var deregistered []string
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := orphanMock(meta, &deregistered)
	mock.deregisterFn = func(jobID string, purge bool, q *nomadapi.WriteOptions) (string, *nomadapi.WriteMeta, error) {
		return "", nil, fmt.Errorf("nomad exploded")
	}
	d := nomad.NewWithClient(deregCfg(true, 0), mock)

	checkNoHCL(t, d, "c1")
	checkNoHCL(t, d, "c2")
	nomad.DrainUpdates(d)

	updates := d.Updates()
	if len(updates) != 1 || updates[0].Status != nomad.JobUpdateStatusFailed {
		t.Fatalf("deregister error should be FAILED, got %+v", updates)
	}
}

// ── scope-exit logging metric ────────────────────────────────────────────────

func TestScopeExit_RemovedFromRepo_Counted(t *testing.T) {
	// Cycle 1: "moving" is actively managed via HCL. Cycle 2: its HCL is gone
	// (renamed/deleted) but it still runs → left management, removed_from_repo.
	live := map[string]string{"gitops_managed": "true"}
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("moving"), Meta: live}, nil
	}
	mock.infoFn = func(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error) {
		return &nomadapi.Job{ID: strPtr(jobID), Status: strPtr("running"), Meta: live}, nil, nil
	}
	mock.planFn = func(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error) {
		return &nomadapi.JobPlanResponse{Diff: &nomadapi.JobDiff{Type: "None"}}, nil, nil
	}
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{{ID: "moving", Status: "running", Meta: live}}, nil, nil
	}
	cfg := &config.Config{NomadNamespace: "default", ManagedMetaPrefix: "gitops", DefaultUpdatePolicy: "none"}
	d := nomad.NewWithClientAndRegistry(cfg, mock, prometheus.NewRegistry())

	// Cycle 1: HCL declares "moving".
	if err := d.Check(map[string]string{"moving.hcl": `job "moving" {}`}, "c1"); err != nil {
		t.Fatal(err)
	}
	// Cycle 2: HCL no longer declares it.
	if err := d.Check(map[string]string{}, "c2"); err != nil {
		t.Fatal(err)
	}

	if got := testutil.ToFloat64(nomad.JobsLeftManagement(d).WithLabelValues("moving", "removed_from_repo")); got != 1 {
		t.Errorf("removed_from_repo counter: want 1, got %v", got)
	}
}
