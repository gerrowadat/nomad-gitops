// export_test.go exposes unexported functions for testing from the nomad_test package.
package nomad

import nomadapi "github.com/hashicorp/nomad/api"

// MergeSelectionReason wraps mergeSelectionReason for external test access.
func MergeSelectionReason(existing, incoming SelectionReason) SelectionReason {
	return mergeSelectionReason(existing, incoming)
}

// HasContentDiff wraps hasContentDiff for external test access.
func HasContentDiff(d *nomadapi.JobDiff) bool {
	return hasContentDiff(d)
}
