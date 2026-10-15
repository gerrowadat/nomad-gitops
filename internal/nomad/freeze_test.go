package nomad_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gerrowadat/nomad-gitops/internal/config"
	"github.com/gerrowadat/nomad-gitops/internal/nomad"
)

// ── Fake freeze store ─────────────────────────────────────────────────────────

// fakeFreezeStore is an in-memory FreezeStore. It stands in for Nomad
// Variables everywhere except the round-trip test below, which exercises the
// real store against a stub Nomad API.
type fakeFreezeStore struct {
	mu   sync.Mutex
	recs map[string]nomad.FreezeRecord

	listErr   error
	putErr    error
	conflicts int // return ErrFreezeConflict on the next n Puts

	listCalls int
	puts      []nomad.FreezeRecord
	deletes   []string
}

func newFakeFreezeStore() *fakeFreezeStore {
	return &fakeFreezeStore{recs: map[string]nomad.FreezeRecord{}}
}

func (f *fakeFreezeStore) List(context.Context) ([]nomad.FreezeRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]nomad.FreezeRecord, 0, len(f.recs))
	for _, r := range f.recs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out, nil
}

func (f *fakeFreezeStore) Put(_ context.Context, rec nomad.FreezeRecord, modifyIndex uint64) (nomad.FreezeRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return nomad.FreezeRecord{}, f.putErr
	}
	if f.conflicts > 0 {
		f.conflicts--
		return nomad.FreezeRecord{}, fmt.Errorf("%w: record moved", nomad.ErrFreezeConflict)
	}
	f.puts = append(f.puts, rec)
	stored := nomad.NewFreezeRecord(rec.JobID, rec.Reason, rec.FrozenBy, rec.ExpiresAt, time.Now())
	stored.VariablePath = "nomad/jobs/gitops/freezes/" + nomad.FreezeSlug(rec.JobID)
	stored = nomad.WithFreezeModifyIndex(stored, modifyIndex+1)
	f.recs[rec.JobID] = stored
	return stored, nil
}

func (f *fakeFreezeStore) Delete(_ context.Context, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, jobID)
	delete(f.recs, jobID)
	return nil
}

// set stores a record directly, as if an operator had run `nomad var put`.
func (f *fakeFreezeStore) set(jobID, reason, expiresAt string, frozenAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := nomad.NewFreezeRecord(jobID, reason, "", expiresAt, frozenAt)
	rec.VariablePath = "nomad/jobs/gitops/freezes/" + nomad.FreezeSlug(jobID)
	f.recs[jobID] = rec
}

// freezeCfg is applyCfg plus a freeze prefix, which is what makes the Differ
// treat freeze support as configured.
func freezeCfg(defaultPolicy string) *config.Config {
	c := applyCfg(defaultPolicy, false)
	c.FreezeVariablePrefix = "nomad/jobs/gitops/freezes"
	return c
}

// differWithFreezes builds a Differ with a fake freeze store attached.
func differWithFreezes(t *testing.T, cfg *config.Config, mock *mockJobsClient) (*nomad.Differ, *fakeFreezeStore) {
	t.Helper()
	d := nomad.NewWithClient(cfg, mock)
	store := newFakeFreezeStore()
	d.SetFreezeStore(store)
	return d, store
}

// ── Record parsing and classification ─────────────────────────────────────────

func TestFreezeSlug(t *testing.T) {
	// Nomad job IDs are more permissive than Variable paths
	// (^[a-zA-Z0-9-_~/]{1,128}$), so anything else becomes '-'.
	tests := map[string]string{
		"api-server":   "api-server",
		"api_server":   "api_server",
		"api.server":   "api-server",
		"api/server":   "api-server",
		"team:api":     "team-api",
		"a~b":          "a~b",
		"MixedCase123": "MixedCase123",
	}
	for in, want := range tests {
		if got := nomad.FreezeSlug(in); got != want {
			t.Errorf("FreezeSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFreezeSlug_IsPathSafe(t *testing.T) {
	// Whatever the job ID, the slug must be writable as a Variable path.
	for _, id := range []string{"a.b", "x/y/z", "weird name!", "emoji-🙂", "semi;colon"} {
		slug := nomad.FreezeSlug(id)
		if strings.ContainsFunc(slug, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '~')
		}) {
			t.Errorf("FreezeSlug(%q) = %q, which Nomad would reject as a path segment", id, slug)
		}
	}
}

func TestFreezeRecord_Classify(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		reason    string
		expiresAt string
		want      nomad.FreezeState
		inForce   bool
	}{
		{"future expiry is active", "incident", now.Add(time.Hour).Format(time.RFC3339), nomad.FreezeStateActive, true},
		{"no expiry is indefinite", "incident", "", nomad.FreezeStateIndefinite, true},
		{"past expiry is expired", "incident", now.Add(-time.Hour).Format(time.RFC3339), nomad.FreezeStateExpired, false},
		{"no reason is malformed", "", now.Add(time.Hour).Format(time.RFC3339), nomad.FreezeStateMalformed, true},
		{"unparseable expiry is malformed", "incident", "next tuesday", nomad.FreezeStateMalformed, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := nomad.NewFreezeRecord("api", tc.reason, "", tc.expiresAt, now)
			rec.Classify(now)
			if rec.State != tc.want {
				t.Errorf("state = %q, want %q (issue: %q)", rec.State, tc.want, rec.Issue)
			}
			if rec.State.InForce() != tc.inForce {
				t.Errorf("InForce() = %v, want %v", rec.State.InForce(), tc.inForce)
			}
		})
	}
}

func TestFreezeRecord_ExpiryBoundaryIsNotActive(t *testing.T) {
	// Exactly at the expiry instant the freeze is over: holding a write a
	// moment longer than asked is the wrong way to round.
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	rec := nomad.NewFreezeRecord("api", "incident", "", now.Format(time.RFC3339), now)
	rec.Classify(now)
	if rec.State != nomad.FreezeStateExpired {
		t.Errorf("a freeze expiring exactly now should be expired, got %q", rec.State)
	}
}

func TestFreezeRecord_IndefiniteHasNoExpiry(t *testing.T) {
	rec := nomad.NewFreezeRecord("api", "incident", "", "", time.Now())
	rec.Classify(time.Now())
	if !rec.Expiry().IsZero() {
		t.Errorf("an indefinite freeze should have a zero expiry, got %v", rec.Expiry())
	}
	if rec.ExpiresAt != "" {
		t.Errorf("an indefinite freeze should expose no expires_at, got %q", rec.ExpiresAt)
	}
}

func TestValidateFreezePath(t *testing.T) {
	if err := nomad.ValidateFreezePath("nomad/jobs/gitops/freezes/api-server"); err != nil {
		t.Errorf("ordinary path rejected: %v", err)
	}
	long := "nomad/jobs/gitops/freezes/" + strings.Repeat("a", 120)
	if err := nomad.ValidateFreezePath(long); err == nil {
		t.Error("a path over Nomad's 128-byte limit should be rejected")
	}
}

func TestFreezeRecord_Describe(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	active := nomad.NewFreezeRecord("api", "INC-4412", "", now.Add(2*time.Hour).Format(time.RFC3339), now)
	active.Classify(now)
	if got := active.Describe(now); !strings.Contains(got, "INC-4412") || !strings.Contains(got, "2h0m0s from now") {
		t.Errorf("Describe() = %q, want the reason and the time remaining", got)
	}

	indef := nomad.NewFreezeRecord("api", "waiting on schema", "", "", now)
	indef.Classify(now)
	if got := indef.Describe(now); !strings.Contains(got, "indefinitely") {
		t.Errorf("Describe() = %q, want it to say indefinitely", got)
	}
}

// ── The register gate ─────────────────────────────────────────────────────────

func TestFreeze_HoldsRegister(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("full"), mock)
	store.set("test-job", "INC-4412: fixing the migration by hand", time.Now().Add(4*time.Hour).Format(time.RFC3339), time.Now())

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("a frozen job must not be registered, got %d register calls", len(calls))
	}
	if updates := d.Updates(); len(updates) != 0 {
		t.Errorf("a frozen job must not enqueue an update at all, got %+v", updates)
	}
	// Drift is still detected and reported: a freeze withholds writes, it does
	// not hide drift.
	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("drift should still be reported while frozen, got %d diffs", len(diffs))
	}
	if diffs[0].ApplyAction != nomad.ApplyActionFrozen {
		t.Errorf("apply_action = %q, want %q", diffs[0].ApplyAction, nomad.ApplyActionFrozen)
	}
	if !strings.Contains(diffs[0].ApplyDetail, "INC-4412") {
		t.Errorf("apply_detail should carry the freeze reason, got %q", diffs[0].ApplyDetail)
	}
	if got := testutil.ToFloat64(nomad.AppliesHeldByFreeze(d).WithLabelValues("REGISTER")); got != 1 {
		t.Errorf("applies_held_by_freeze_total{REGISTER} = %v, want 1", got)
	}
}

func TestFreeze_ExpiredRecordDoesNotHold(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("full"), mock)
	store.set("test-job", "yesterday's incident", time.Now().Add(-time.Hour).Format(time.RFC3339), time.Now().Add(-5*time.Hour))

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("an expired freeze suppresses nothing: want 1 register call, got %d", len(calls))
	}
}

func TestFreeze_IndefiniteHoldsRegister(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("full"), mock)
	store.set("test-job", "waiting on the new index schema", "", time.Now().Add(-48*time.Hour))

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("an indefinite freeze must hold writes, got %d register calls", len(calls))
	}
}

func TestFreeze_MalformedRecordHoldsRegister(t *testing.T) {
	// A record with no reason cannot be read as a well-formed freeze. It is
	// held conservatively rather than ignored: a safety control that cannot be
	// parsed must not silently fail open.
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("full"), mock)
	store.set("test-job", "", "", time.Now())

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("a malformed freeze record must hold writes, got %d register calls", len(calls))
	}
	if got := testutil.ToFloat64(nomad.FreezeRecordsGauge(d).WithLabelValues("malformed")); got != 1 {
		t.Errorf("freeze_records{state=malformed} = %v, want 1", got)
	}
}

func TestFreeze_OtherJobsUnaffected(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("full"), mock)
	store.set("some-other-job", "not this one", "", time.Now())

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("a freeze on another job must not hold this one: want 1 register call, got %d", len(calls))
	}
}

func TestFreeze_PolicyBlockedReportsPolicyNotFreeze(t *testing.T) {
	// The freeze gate sits after the gates that mean we were never going to
	// write anyway, so the reported reason is the freeze only when the freeze
	// is the actual reason — and the held-writes counter is not inflated by
	// jobs that would not have been written.
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "none"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("none"), mock)
	store.set("test-job", "frozen too", "", time.Now())

	runCheck(t, d, "aaaa111fffff")

	diffs, _, _ := d.Diffs()
	if len(diffs) != 1 {
		t.Fatalf("want 1 diff, got %d", len(diffs))
	}
	if diffs[0].ApplyAction != nomad.ApplyActionPolicyBlocked {
		t.Errorf("apply_action = %q, want %q: policy is the real reason here", diffs[0].ApplyAction, nomad.ApplyActionPolicyBlocked)
	}
	if got := testutil.ToFloat64(nomad.AppliesHeldByFreeze(d).WithLabelValues("REGISTER")); got != 0 {
		t.Errorf("applies_held_by_freeze_total{REGISTER} = %v, want 0: nothing was withheld by the freeze", got)
	}
}

// ── The apply-time gate (the enqueue/apply race) ──────────────────────────────

func TestFreeze_LandingAfterEnqueue_MarksUpdateHeld(t *testing.T) {
	// A freeze can land between detection and apply, and that window is
	// exactly when an operator reaching for a freeze most needs it to work.
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("full"), mock)

	runCheck(t, d, "aaaa111fffff")
	if updates := d.Updates(); len(updates) != 1 || updates[0].Status != nomad.JobUpdateStatusPending {
		t.Fatalf("expected one PENDING update before the freeze, got %+v", updates)
	}

	// Freeze lands now, after the update is queued but before it is applied.
	store.set("test-job", "stop, we are mid-incident", time.Now().Add(time.Hour).Format(time.RFC3339), time.Now())
	nomad.DrainUpdates(d)

	if len(calls) != 0 {
		t.Errorf("a freeze landing before the write must hold it, got %d register calls", len(calls))
	}
	updates := d.Updates()
	if len(updates) != 1 {
		t.Fatalf("want 1 update, got %d", len(updates))
	}
	if updates[0].Status != nomad.JobUpdateStatusHeld {
		t.Errorf("status = %q, want %q", updates[0].Status, nomad.JobUpdateStatusHeld)
	}
	if !strings.Contains(updates[0].Error, "mid-incident") {
		t.Errorf("the held update should record why, got %q", updates[0].Error)
	}
}

func TestFreeze_HeldIsTerminal_QueueDoesNotSpin(t *testing.T) {
	// HELD has to be terminal or drainUpdates would loop forever on an
	// update NextPending keeps handing back.
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("full"), mock)

	runCheck(t, d, "aaaa111fffff")
	store.set("test-job", "hold", "", time.Now())

	done := make(chan struct{})
	go func() {
		nomad.DrainUpdates(d)
		nomad.DrainUpdates(d)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drainUpdates did not return: a HELD update is being re-offered forever")
	}
}

func TestFreeze_ThawLetsTheNextCycleApply(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("full"), mock)
	store.set("test-job", "hold", "", time.Now())

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)
	if len(calls) != 0 {
		t.Fatalf("precondition: frozen job should not register, got %d calls", len(calls))
	}

	// Thaw, then let the next cycle re-derive the intent. Nothing has to be
	// remembered across the thaw.
	if err := d.ThawJob(context.Background(), "test-job"); err != nil {
		t.Fatalf("ThawJob: %v", err)
	}
	runCheck(t, d, "bbbb222fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("after a thaw the next cycle should apply: want 1 register call, got %d", len(calls))
	}
}

// ── Fail-closed behaviour ─────────────────────────────────────────────────────

func TestFreeze_NeverReadableStoreDoesNotHoldApplies(t *testing.T) {
	// Freeze support is on by default, so a deployment whose ACL policy
	// predates it (no variables:read) must not have every apply stopped by a
	// feature it is not using. Nobody can be relying on a freeze
	// nomad-gitops has never once been able to see.
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("full"), mock)
	store.listErr = errors.New("ACL token lacks variables:read")

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("a never-readable freeze store must not stop applies: want 1 register call, got %d", len(calls))
	}
	if hold, why := nomad.FreezeHold(d); hold {
		t.Errorf("freezeHold() should be false when the store has never been read, got %q", why)
	}
	// But the condition is visible rather than silent.
	_, _, healthy := d.Freezes()
	if healthy {
		t.Error("Freezes() should report the store as unhealthy")
	}
	// Detection is never degraded by a freeze-store problem.
	if diffs, _, _ := d.Diffs(); len(diffs) != 1 {
		t.Errorf("drift detection must be unaffected, got %d diffs", len(diffs))
	}
}

func TestFreeze_FailsClosedAfterAGoodReadThenFailure(t *testing.T) {
	// Once the store has been read successfully, freezes are evidently in use
	// here, so a later read failure holds applies rather than assuming
	// nothing is frozen.
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	cfg := freezeCfg("full")
	cfg.FreezeMaxStaleness = time.Minute
	d, store := differWithFreezes(t, cfg, mock)

	// One good read, then the ACL is revoked and the view goes stale.
	nomad.RefreshFreezes(d, context.Background())
	store.listErr = errors.New("ACL token lacks variables:read")
	nomad.RefreshFreezes(d, context.Background())
	nomad.SetFreezeLastRead(d, time.Now().Add(-10*time.Minute))

	hold, why := nomad.FreezeHold(d)
	if !hold {
		t.Fatal("a store that worked and then stopped must hold applies")
	}
	if !strings.Contains(why, "stale") {
		t.Errorf("hold reason = %q, want it to mention staleness", why)
	}

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)
	if len(calls) != 0 {
		t.Errorf("applies must be held while the freeze view cannot be trusted, got %d register calls", len(calls))
	}
}

func TestFreeze_FailsClosedOnStaleView(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	cfg := freezeCfg("full")
	cfg.FreezeMaxStaleness = 5 * time.Minute
	d, _ := differWithFreezes(t, cfg, mock)

	// A successful read, then backdate it past the staleness window.
	nomad.RefreshFreezes(d, context.Background())
	nomad.SetFreezeLastRead(d, time.Now().Add(-10*time.Minute))

	hold, why := nomad.FreezeHold(d)
	if !hold {
		t.Fatal("a freeze view older than --freeze-max-staleness should hold applies")
	}
	if !strings.Contains(why, "stale") {
		t.Errorf("hold reason = %q, want it to mention staleness", why)
	}
}

func TestFreeze_FailOpenProceeds(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	cfg := freezeCfg("full")
	cfg.FreezeFailOpen = true
	cfg.FreezeMaxStaleness = time.Minute
	d, store := differWithFreezes(t, cfg, mock)

	// A good read first, so this is the post-first-read case that would
	// otherwise fail closed.
	nomad.RefreshFreezes(d, context.Background())
	store.listErr = errors.New("nomad is down")
	nomad.SetFreezeLastRead(d, time.Now().Add(-10*time.Minute))

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(calls) != 1 {
		t.Errorf("--freeze-fail-open should proceed as if nothing were frozen: want 1 register call, got %d", len(calls))
	}
}

func TestFreeze_FailedReadKeepsPreviousView(t *testing.T) {
	// Stale but safe beats fresh and empty: a read failure must not be read
	// as "nothing is frozen".
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("full"), mock)
	store.set("test-job", "frozen before the outage", "", time.Now())

	nomad.RefreshFreezes(d, context.Background())
	store.listErr = errors.New("nomad is unreachable")
	nomad.RefreshFreezes(d, context.Background())

	blocked, _ := nomad.FreezeBlocks(d, "test-job")
	if !blocked {
		t.Error("the previously-read freeze must stay in force while reads are failing")
	}
}

func TestFreeze_DisabledWhenNoStoreConfigured(t *testing.T) {
	// With no freeze prefix there is no store, no hold, and no reason for the
	// gate to do anything at all.
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d := nomad.NewWithClient(applyCfg("full", false), mock)

	if d.FreezeEnabled() {
		t.Error("freeze support should be off without --freeze-variable-prefix")
	}
	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)
	if len(calls) != 1 {
		t.Errorf("with freeze support off, applies proceed: want 1 register call, got %d", len(calls))
	}
	if hold, _ := nomad.FreezeHold(d); hold {
		t.Error("freeze support off must never hold applies")
	}
}

// ── Deregister and revert ─────────────────────────────────────────────────────

// managedFullMeta is the live meta of a job that opted in and allows applies.
var managedFullMeta = map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}

// freezeDeregCfg is deregCfg plus a freeze prefix.
func freezeDeregCfg(grace time.Duration) *config.Config {
	c := deregCfg(true, grace)
	c.FreezeVariablePrefix = "nomad/jobs/gitops/freezes"
	return c
}

func TestFreeze_HoldsDeregister(t *testing.T) {
	var deregisters []string
	d, store := differWithFreezes(t, freezeDeregCfg(0), orphanMock(managedFullMeta, &deregisters))
	store.set("orphan", "mid-rename, do not delete it", time.Now().Add(time.Hour).Format(time.RFC3339), time.Now())

	// Two cycles: the first records the orphan, the second would deregister it
	// once the (zero) grace period has elapsed.
	checkNoHCL(t, d, "aaaa111")
	checkNoHCL(t, d, "bbbb222")
	nomad.DrainUpdates(d)

	if len(deregisters) != 0 {
		t.Errorf("a frozen job must not be deregistered, got %v", deregisters)
	}
	if got := orphanDiffAction(t, d); got != nomad.ApplyActionFrozen {
		t.Errorf("apply_action = %q, want %q", got, nomad.ApplyActionFrozen)
	}
	if got := testutil.ToFloat64(nomad.AppliesHeldByFreeze(d).WithLabelValues("DEREGISTER")); got == 0 {
		t.Error("applies_held_by_freeze_total{DEREGISTER} should have been incremented")
	}
}

func TestFreeze_DeregisterProceedsWhenNotFrozen(t *testing.T) {
	var deregisters []string
	d, _ := differWithFreezes(t, freezeDeregCfg(0), orphanMock(managedFullMeta, &deregisters))

	checkNoHCL(t, d, "aaaa111")
	checkNoHCL(t, d, "bbbb222")
	nomad.DrainUpdates(d)

	if len(deregisters) != 1 {
		t.Errorf("an unfrozen orphan should still be deregistered: want 1, got %v", deregisters)
	}
}

func TestFreeze_HoldsOrphanGraceRatherThanSpendingIt(t *testing.T) {
	// Freezing an orphaned job buys time without spending it: the time spent
	// frozen does not count towards --deregister-grace.
	var deregisters []string
	d, store := differWithFreezes(t, freezeDeregCfg(time.Hour), orphanMock(managedFullMeta, &deregisters))
	store.set("orphan", "mid-rename", "", time.Now())

	checkNoHCL(t, d, "aaaa111")
	// Two more refreshes with the job frozen accrue frozen time.
	nomad.RefreshFreezes(d, context.Background())
	nomad.RefreshFreezes(d, context.Background())

	if off := nomad.GraceOffset(d, "orphan"); off <= 0 {
		t.Errorf("a frozen orphan should accrue a grace offset, got %v", off)
	}
	// A job that was never frozen accrues nothing.
	if off := nomad.GraceOffset(d, "some-other-job"); off != 0 {
		t.Errorf("an unfrozen job should accrue no grace offset, got %v", off)
	}
}

func TestFreeze_GraceAccrualClearedWhenNoLongerOrphaned(t *testing.T) {
	// Once a job is back in the repo, a later orphaning starts its grace
	// period clean rather than inheriting frozen time from months ago.
	var deregisters []string
	d, store := differWithFreezes(t, freezeDeregCfg(time.Hour), orphanMock(managedFullMeta, &deregisters))
	store.set("orphan", "mid-rename", "", time.Now())

	checkNoHCL(t, d, "aaaa111")
	nomad.RefreshFreezes(d, context.Background())
	if off := nomad.GraceOffset(d, "orphan"); off <= 0 {
		t.Fatalf("precondition: want an accrued offset, got %v", off)
	}

	// The job reappears in the repo, so it is no longer orphaned. Thaw it too,
	// so the live offset is only what was accrued.
	if err := d.ThawJob(context.Background(), "orphan"); err != nil {
		t.Fatalf("ThawJob: %v", err)
	}
	if err := d.Check(map[string]string{"jobs/orphan.hcl": `job "orphan" {}`}, "bbbb222"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if off := nomad.GraceOffset(d, "orphan"); off != 0 {
		t.Errorf("accrued frozen time should be cleared once a job is no longer orphaned, got %v", off)
	}
}

func TestFreeze_HoldsRevert(t *testing.T) {
	var reverts []revertCall
	cfg := rollbackCfg(true)
	cfg.FreezeVariablePrefix = "nomad/jobs/gitops/freezes"
	meta := map[string]string{"gitops_managed": "true", "gitops_rollback": "true"}
	d, store := differWithFreezes(t, cfg, rollbackMock(meta, nil, &reverts))
	store.set("test-job", "do not roll back, we are debugging", "", time.Now())

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(reverts) != 0 {
		t.Errorf("a frozen job must not be reverted, got %+v", reverts)
	}
	if got := testutil.ToFloat64(nomad.Rollbacks(d).WithLabelValues("test-job", "held_by_freeze")); got != 1 {
		t.Errorf("rollbacks_total{result=held_by_freeze} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(nomad.AppliesHeldByFreeze(d).WithLabelValues("REVERT")); got != 1 {
		t.Errorf("applies_held_by_freeze_total{REVERT} = %v, want 1", got)
	}
}

func TestFreeze_RevertProceedsWhenNotFrozen(t *testing.T) {
	var reverts []revertCall
	cfg := rollbackCfg(true)
	cfg.FreezeVariablePrefix = "nomad/jobs/gitops/freezes"
	meta := map[string]string{"gitops_managed": "true", "gitops_rollback": "true"}
	d, _ := differWithFreezes(t, cfg, rollbackMock(meta, nil, &reverts))

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)

	if len(reverts) != 1 {
		t.Errorf("an unfrozen job should still roll back: want 1 revert, got %d", len(reverts))
	}
}

// ── Metrics ───────────────────────────────────────────────────────────────────

func TestFreeze_Metrics(t *testing.T) {
	mock := defaultMock()
	d, store := differWithFreezes(t, freezeCfg("none"), mock)
	store.set("active-job", "incident", time.Now().Add(time.Hour).Format(time.RFC3339), time.Now())
	store.set("indefinite-job", "no idea how long", "", time.Now())
	store.set("expired-job", "last week", time.Now().Add(-time.Hour).Format(time.RFC3339), time.Now().Add(-5*time.Hour))

	nomad.RefreshFreezes(d, context.Background())

	if got := testutil.ToFloat64(nomad.FrozenJobsGauge(d)); got != 2 {
		t.Errorf("frozen_jobs = %v, want 2 (the expired record holds nothing)", got)
	}
	for _, jobID := range []string{"active-job", "indefinite-job"} {
		if got := testutil.ToFloat64(nomad.JobFrozenGauge(d).WithLabelValues(jobID)); got != 1 {
			t.Errorf("job_frozen{%s} = %v, want 1", jobID, got)
		}
	}
	for state, want := range map[string]float64{"active": 1, "indefinite": 1, "expired": 1, "malformed": 0} {
		if got := testutil.ToFloat64(nomad.FreezeRecordsGauge(d).WithLabelValues(state)); got != want {
			t.Errorf("freeze_records{state=%s} = %v, want %v", state, got, want)
		}
	}
	// The expiry series must be absent for an indefinite freeze: that absence
	// is what the alert rules query with `unless on(nomad_job)`.
	if n := testutil.CollectAndCount(nomad.JobFreezeExpiresGauge(d)); n != 1 {
		t.Errorf("job_freeze_expires series count = %d, want 1 (only the active freeze has an expiry)", n)
	}
}

func TestFreeze_ThawClearsPerJobMetrics(t *testing.T) {
	mock := defaultMock()
	d, store := differWithFreezes(t, freezeCfg("none"), mock)
	store.set("api", "incident", "", time.Now())
	nomad.RefreshFreezes(d, context.Background())
	if n := testutil.CollectAndCount(nomad.JobFrozenGauge(d)); n != 1 {
		t.Fatalf("precondition: want 1 job_frozen series, got %d", n)
	}

	if err := d.ThawJob(context.Background(), "api"); err != nil {
		t.Fatalf("ThawJob: %v", err)
	}
	if n := testutil.CollectAndCount(nomad.JobFrozenGauge(d)); n != 0 {
		t.Errorf("a thawed job should leave no job_frozen series behind, got %d", n)
	}
	if got := testutil.ToFloat64(nomad.FrozenJobsGauge(d)); got != 0 {
		t.Errorf("frozen_jobs = %v, want 0 after the thaw", got)
	}
}

func TestOldestPendingUpdateAgeMetric(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("full"), mock)
	store.set("test-job", "hold it", "", time.Now())

	// Frozen, so nothing is enqueued and the queue stays empty.
	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)
	if got := testutil.ToFloat64(nomad.OldestPendingAgeGauge(d)); got != 0 {
		t.Errorf("oldest_pending_update_age_seconds = %v, want 0 with an empty queue", got)
	}
}

// ── Mutation ──────────────────────────────────────────────────────────────────

func TestFreezeJob_WritesAndTakesEffect(t *testing.T) {
	var calls []registerCall
	meta := map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	mock := applyMock(meta, editedMixedDiff(), 42, &calls)
	d, store := differWithFreezes(t, freezeCfg("full"), mock)

	expiry := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	rec, err := d.FreezeJob(context.Background(), "test-job", "INC-1: by hand", "doc", expiry)
	if err != nil {
		t.Fatalf("FreezeJob: %v", err)
	}
	if rec.State != nomad.FreezeStateActive {
		t.Errorf("stored record state = %q, want active", rec.State)
	}
	if len(store.puts) != 1 {
		t.Fatalf("want 1 store write, got %d", len(store.puts))
	}

	runCheck(t, d, "aaaa111fffff")
	nomad.DrainUpdates(d)
	if len(calls) != 0 {
		t.Errorf("the freeze should take effect immediately, got %d register calls", len(calls))
	}
}

func TestFreezeJob_RequiresAReason(t *testing.T) {
	d, store := differWithFreezes(t, freezeCfg("none"), defaultMock())
	if _, err := d.FreezeJob(context.Background(), "api", "   ", "", ""); err == nil {
		t.Error("a freeze with no reason should be refused: an unexplained freeze is the thing this design most avoids")
	}
	if len(store.puts) != 0 {
		t.Errorf("nothing should have been written, got %d writes", len(store.puts))
	}
}

func TestFreezeJob_RejectsBadExpiry(t *testing.T) {
	d, _ := differWithFreezes(t, freezeCfg("none"), defaultMock())
	if _, err := d.FreezeJob(context.Background(), "api", "incident", "", "tomorrow-ish"); err == nil {
		t.Error("a non-RFC3339 expiry should be refused")
	}
}

func TestFreezeJob_RetriesOnceOnCASConflict(t *testing.T) {
	// A racing write — another replica, or a human `nomad var put` — makes the
	// first Put lose. The retry re-reads and wins.
	d, store := differWithFreezes(t, freezeCfg("none"), defaultMock())
	store.conflicts = 1

	if _, err := d.FreezeJob(context.Background(), "api", "incident", "", ""); err != nil {
		t.Fatalf("FreezeJob should have retried after the conflict: %v", err)
	}
	if len(store.puts) != 1 {
		t.Errorf("want 1 successful write after the retry, got %d", len(store.puts))
	}
}

func TestFreezeJob_SecondConflictIsReturned(t *testing.T) {
	// One retry, not a loop.
	d, store := differWithFreezes(t, freezeCfg("none"), defaultMock())
	store.conflicts = 2

	_, err := d.FreezeJob(context.Background(), "api", "incident", "", "")
	if !errors.Is(err, nomad.ErrFreezeConflict) {
		t.Errorf("want ErrFreezeConflict after a second conflict, got %v", err)
	}
}

func TestFreezeJob_DisabledWithoutAStore(t *testing.T) {
	d := nomad.NewWithClient(applyCfg("none", false), defaultMock())
	if _, err := d.FreezeJob(context.Background(), "api", "incident", "", ""); err == nil {
		t.Error("freezing should fail when freeze support is disabled")
	}
	if err := d.ThawJob(context.Background(), "api"); err == nil {
		t.Error("thawing should fail when freeze support is disabled")
	}
}

func TestThawJob_UnfrozenJobIsNotAnError(t *testing.T) {
	// The caller asked for the job not to be frozen, and it is not.
	d, _ := differWithFreezes(t, freezeCfg("none"), defaultMock())
	if err := d.ThawJob(context.Background(), "never-frozen"); err != nil {
		t.Errorf("thawing a job that is not frozen should succeed, got %v", err)
	}
}

func TestPurgeExpiredFreezes(t *testing.T) {
	d, store := differWithFreezes(t, freezeCfg("none"), defaultMock())
	nomad.SetFreezeExpiredRetention(d, time.Hour)
	// Expired two hours ago: past the one-hour retention.
	store.set("old-job", "last week", time.Now().Add(-2*time.Hour).Format(time.RFC3339), time.Now().Add(-5*time.Hour))
	// Expired a minute ago: still within retention.
	store.set("recent-job", "just now", time.Now().Add(-time.Minute).Format(time.RFC3339), time.Now().Add(-time.Hour))
	// Still in force: never a purge candidate.
	store.set("live-job", "ongoing", "", time.Now())

	nomad.RefreshFreezes(d, context.Background())
	nomad.PurgeExpiredFreezes(d, context.Background())

	if len(store.deletes) != 1 || store.deletes[0] != "old-job" {
		t.Errorf("want only old-job purged, got %v", store.deletes)
	}
}

func TestPurgeExpiredFreezes_OffByDefault(t *testing.T) {
	d, store := differWithFreezes(t, freezeCfg("none"), defaultMock())
	store.set("old-job", "last week", time.Now().Add(-100*time.Hour).Format(time.RFC3339), time.Now().Add(-200*time.Hour))

	nomad.RefreshFreezes(d, context.Background())
	nomad.PurgeExpiredFreezes(d, context.Background())

	if len(store.deletes) != 0 {
		t.Errorf("expired records are kept unless --freeze-expired-retention is set, got %v", store.deletes)
	}
}

// ── Freeze view refresh ───────────────────────────────────────────────────────

func TestFreeze_ViewRefreshedEvenWhenTheDiffIsSkipped(t *testing.T) {
	// A Variable write does not move the jobs table's Raft index, so if the
	// freeze read happened after the skip optimisation, a freeze set during a
	// quiet period would go unreported until something else changed.
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return nil, &nomadapi.QueryMeta{LastIndex: 99}, nil
	}
	d, store := differWithFreezes(t, freezeCfg("none"), mock)

	runCheck(t, d, "samecommit")
	before := store.listCalls

	// Same commit, same Raft index: this cycle takes the skip shortcut.
	store.set("test-job", "frozen during a quiet period", "", time.Now())
	runCheck(t, d, "samecommit")

	if store.listCalls <= before {
		t.Error("the freeze store should be read even on a skipped diff cycle")
	}
	if blocked, _ := nomad.FreezeBlocks(d, "test-job"); !blocked {
		t.Error("a freeze set during a quiet period should be in force after the next cycle")
	}
	frozen, _, healthy := d.Freezes()
	if !healthy || len(frozen) != 1 {
		t.Errorf("Freezes() = %d records, healthy=%v; want 1 record and a healthy store", len(frozen), healthy)
	}
}
