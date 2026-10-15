// export_test.go exposes unexported functions for testing from the nomad_test package.
package nomad

import (
	"context"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus"
)

// MergeSelectionReason wraps mergeSelectionReason for external test access.
func MergeSelectionReason(existing, incoming SelectionReason) SelectionReason {
	return mergeSelectionReason(existing, incoming)
}

// HasContentDiff wraps hasContentDiff for external test access.
func HasContentDiff(d *nomadapi.JobDiff) bool {
	return hasContentDiff(d)
}

// RedactedFieldsCounter exposes the redaction counter for metric assertions.
func RedactedFieldsCounter(d *Differ) prometheus.Counter {
	return d.redactedFields
}

// DrainUpdates runs the applier synchronously over all pending updates.
func DrainUpdates(d *Differ) { d.drainUpdates() }

// ClassifyDiff exposes the diff classifier for table-driven tests.
func ClassifyDiff(d *nomadapi.JobDiff, autoscaled map[string]bool, metaPrefix string) DiffClass {
	return classifyDiff(d, autoscaled, metaPrefix)
}

// MetaOnlyDiffs exposes the meta-only diff counter for metric assertions.
func MetaOnlyDiffs(d *Differ) *prometheus.CounterVec {
	return d.metaOnlyDiffs
}

// UpdatesBlockedExistingDrift exposes the pre-existing-drift block counter.
func UpdatesBlockedExistingDrift(d *Differ) *prometheus.CounterVec {
	return d.updatesBlockedExistingDrift
}

// JobsLeftManagement exposes the scope-exit counter for metric assertions.
func JobsLeftManagement(d *Differ) *prometheus.CounterVec {
	return d.jobsLeftManagement
}

// LastNomadIndex exposes the cached Raft index for skip-invalidation tests.
func LastNomadIndex(d *Differ) uint64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.lastNomadIndex
}

// MetaKeyIssues exposes the meta-key issue counter for metric assertions.
func MetaKeyIssues(d *Differ) *prometheus.CounterVec {
	return d.metaKeyIssues
}

// MetaKeyChanges exposes the meta-key change counter for metric assertions.
func MetaKeyChanges(d *Differ) *prometheus.CounterVec {
	return d.metaKeyChanges
}

// UpdatesBlockedKnownFailed exposes the flap-guard block counter.
func UpdatesBlockedKnownFailed(d *Differ) *prometheus.CounterVec {
	return d.updatesBlockedKnownFailed
}

// Rollbacks exposes the active-rollback outcome counter.
func Rollbacks(d *Differ) *prometheus.CounterVec {
	return d.rollbacks
}

// FailedVersionsTagged exposes the flap-guard tag counter.
func FailedVersionsTagged(d *Differ) *prometheus.CounterVec {
	return d.failedVersionsTagged
}

// NomadTokenRefreshes exposes the token-refresh counter.
func NomadTokenRefreshes(d *Differ) *prometheus.CounterVec {
	return d.nomadTokenRefreshes
}

// SpecFingerprint exposes the spec fingerprint helper for unit tests.
func SpecFingerprint(job *nomadapi.Job, metaPrefix string) (string, error) {
	return specFingerprint(job, metaPrefix)
}

// LastStableVersion exposes the stable-version selection helper.
func LastStableVersion(versions []*nomadapi.Job, failed uint64) (uint64, bool) {
	return lastStableVersion(versions, failed)
}

// JobHasAutoRevert exposes the auto_revert detection helper.
func JobHasAutoRevert(job *nomadapi.Job) bool {
	return jobHasAutoRevert(job)
}

// ── Freeze test hooks ─────────────────────────────────────────────────────────

// ValidateFreezePath exposes the Variable path length check.
func ValidateFreezePath(path string) error { return validateFreezePath(path) }

// FreezeHold exposes the fail-closed decision: whether every apply is being
// held because the freeze view cannot be trusted, and why.
func FreezeHold(d *Differ) (bool, string) { return d.freezeHold() }

// FreezeBlocks exposes the per-job gate used by the deciders and the applier.
func FreezeBlocks(d *Differ, jobID string) (bool, string) { return d.freezeBlocks(jobID) }

// RefreshFreezes forces a read of the freeze store.
func RefreshFreezes(d *Differ, ctx context.Context) { d.refreshFreezes(ctx) }

// PurgeExpiredFreezes runs the expired-record purge.
func PurgeExpiredFreezes(d *Differ, ctx context.Context) { d.purgeExpiredFreezes(ctx) }

// GraceOffset exposes the time a job has spent frozen, which is subtracted
// from its orphan deregister grace period.
func GraceOffset(d *Differ, jobID string) time.Duration { return d.graceOffset(jobID) }

// SetFreezeStaleness overrides --freeze-max-staleness and --freeze-fail-open.
func SetFreezeStaleness(d *Differ, max time.Duration, failOpen bool) {
	d.freezeMu.Lock()
	d.freezeMaxStaleness = max
	d.freezeFailOpen = failOpen
	d.freezeMu.Unlock()
}

// SetFreezeLastRead backdates the cached view's last-read time, so staleness
// can be tested without sleeping.
func SetFreezeLastRead(d *Differ, t time.Time) {
	d.freezeMu.Lock()
	d.freezes.lastRead = t
	d.freezeMu.Unlock()
}

// SetFreezeExpiredRetention overrides --freeze-expired-retention.
func SetFreezeExpiredRetention(d *Differ, retention time.Duration) {
	d.freezeExpiredRetention = retention
}

// FreezeRecordModifyIndex exposes a record's CAS token.
func FreezeRecordModifyIndex(r FreezeRecord) uint64 { return r.modifyIndex }

// WithFreezeModifyIndex returns a copy of r carrying the given CAS token, for
// fake stores that need to hand one back.
func WithFreezeModifyIndex(r FreezeRecord, idx uint64) FreezeRecord {
	r.modifyIndex = idx
	return r
}

// AppliesHeldByFreeze exposes the withheld-writes counter.
func AppliesHeldByFreeze(d *Differ) *prometheus.CounterVec { return d.appliesHeldByFreeze }

// FreezeStoreErrors exposes the freeze-store error counter.
func FreezeStoreErrors(d *Differ) *prometheus.CounterVec { return d.freezeStoreErrors }

// JobFrozenGauge exposes the per-job frozen gauge.
func JobFrozenGauge(d *Differ) *prometheus.GaugeVec { return d.jobFrozen }

// FreezeRecordsGauge exposes the per-state record-count gauge.
func FreezeRecordsGauge(d *Differ) *prometheus.GaugeVec { return d.freezeRecords }

// FrozenJobsGauge exposes the total frozen-jobs gauge.
func FrozenJobsGauge(d *Differ) prometheus.Gauge { return d.frozenJobs }

// JobFreezeExpiresGauge exposes the per-job expiry gauge, which is absent for
// an indefinite freeze.
func JobFreezeExpiresGauge(d *Differ) *prometheus.GaugeVec { return d.jobFreezeExpires }

// OldestPendingAgeGauge exposes the stuck-queue gauge.
func OldestPendingAgeGauge(d *Differ) prometheus.Gauge { return d.oldestPendingAge }

// NewNomadVariableFreezeStore builds the Nomad-Variables-backed store against
// a client pointed at a test server.
func NewNomadVariableFreezeStore(c *nomadapi.Client, prefix, namespace string, errFn func(op string)) FreezeStore {
	return newNomadVariableFreezeStore(c, prefix, namespace, errFn)
}
