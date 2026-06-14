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
