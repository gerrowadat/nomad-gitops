// export_test.go exposes unexported functions for testing from the nomad_test package.
package nomad

import (
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
