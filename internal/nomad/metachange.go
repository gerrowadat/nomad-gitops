package nomad

import (
	"fmt"
	"log/slog"
	"path"
	"strings"
)

// Meta-key change tracking: the gitops_* keys are behavioural switches, so a
// job gaining, losing, or editing one changes what nomad-botherer does with
// it. Every transition is logged with the consequence — what the tool will
// do differently to honour it. Both sources are watched: the HCL side (a
// commit changed the keys) and the live side (someone re-registered the job
// manually, which is how the meta-drift problem manifests).
//
// Snapshots live in memory only: the first cycle after startup is a
// baseline and logs nothing.

// metaState holds a job's prefix-addressed meta keys from one source.
type metaState map[string]string

// prefixMetaKeys extracts the keys addressed to nomad-botherer (underscore
// and dotted forms) from meta.
func (d *Differ) prefixMetaKeys(meta map[string]string) metaState {
	out := metaState{}
	if d.managedMetaPrefix == "" {
		return out
	}
	underscored := d.managedMetaPrefix + "_"
	dotted := d.managedMetaPrefix + "."
	for k, v := range meta {
		if strings.HasPrefix(k, underscored) || strings.HasPrefix(k, dotted) {
			out[k] = v
		}
	}
	return out
}

// metaStateKey identifies one (source, job) snapshot.
func metaStateKey(source, jobID string) string {
	return source + "\x00" + jobID
}

// recordMetaSeen stores a job's current prefix keys for change detection at
// the end of the check cycle.
func recordMetaSeen(seen map[string]metaState, d *Differ, source, jobID string, meta map[string]string) {
	if d.managedMetaPrefix == "" {
		return
	}
	seen[metaStateKey(source, jobID)] = d.prefixMetaKeys(meta)
}

// logMetaChanges compares this cycle's snapshots against the previous
// cycle's and logs every key transition. Jobs seen for the first time are
// baselined silently; jobs that disappeared are dropped silently (their
// absence is drift, which is reported elsewhere).
func (d *Differ) logMetaChanges(seen map[string]metaState) {
	if d.managedMetaPrefix == "" {
		return
	}
	d.metaMu.Lock()
	defer d.metaMu.Unlock()

	if d.prevMeta != nil {
		for k, cur := range seen {
			prev, ok := d.prevMeta[k]
			if !ok {
				continue
			}
			parts := strings.SplitN(k, "\x00", 2)
			d.diffMetaState(parts[0], parts[1], prev, cur)
		}
	}
	d.prevMeta = seen
}

// diffMetaState logs every added, removed, or changed prefix key for one
// (source, job) pair.
func (d *Differ) diffMetaState(source, jobID string, prev, cur metaState) {
	keys := make(map[string]struct{}, len(prev)+len(cur))
	for k := range prev {
		keys[k] = struct{}{}
	}
	for k := range cur {
		keys[k] = struct{}{}
	}
	for k := range keys {
		oldV, hadOld := prev[k]
		newV, hasNew := cur[k]
		if hadOld && hasNew && oldV == newV {
			continue
		}
		change := "changed"
		switch {
		case !hadOld:
			change = "added"
		case !hasNew:
			change = "removed"
		}
		d.metaKeyChanges.WithLabelValues(jobID, source).Inc()
		slog.Info("Job meta key under the managed prefix changed",
			"job", jobID, "source", source, "key", k, "change", change,
			"old", oldV, "new", newV,
			"action", d.metaChangeAction(jobID, source, k, oldV, newV, hasNew))
	}
}

// metaChangeAction describes what nomad-botherer will do to honour a key
// transition.
func (d *Differ) metaChangeAction(jobID, source, key, oldV, newV string, hasNew bool) string {
	switch key {
	case d.managedMetaPrefix + "_managed":
		return d.managedTransitionAction(jobID, source, newV, hasNew)
	case d.managedMetaPrefix + "_update_policy":
		return d.policyTransitionAction(source, newV, hasNew)
	default:
		return "key is not one nomad-botherer understands; no behaviour change (see meta-key issue warnings)"
	}
}

// managedTransitionAction explains the consequence of an opt-in change.
func (d *Differ) managedTransitionAction(jobID, source, newV string, hasNew bool) string {
	if source == "nomad" {
		// Git is always the source of truth for our keys: a live-side change
		// only matters for jobs Git knows nothing about.
		return "noticed on the live job only; when the job has an HCL file in the repo, Git is the source of truth and the live value does not drive behaviour (for jobs without HCL, the live key controls missing_from_hcl detection)"
	}
	if hasNew && newV == "true" {
		return "job is now opted in to GitOps management: it will be diffed against its HCL and applied per its effective update policy; if the live job does not carry the key yet, that difference is itself drift and converges the same way"
	}
	// Removed, "false", or an invalid value: the opt-in check no longer passes.
	suffix := "nomad-botherer stops diffing it and will never register or deregister it"
	if hasNew && !validManagedValue(newV) {
		suffix = "the value is invalid, so the opt-in check fails; " + suffix
	}
	if d.jobSelectorGlob != "" {
		if matched, _ := path.Match(d.jobSelectorGlob, jobID); matched {
			return fmt.Sprintf("opt-in no longer effective, but the job still matches --job-selector-glob %q and remains watched", d.jobSelectorGlob)
		}
	}
	return "job is no longer managed: " + suffix
}

// policyTransitionAction explains the consequence of an update-policy change.
func (d *Differ) policyTransitionAction(source, newV string, hasNew bool) string {
	effective := d.defaultPolicy
	qualifier := ""
	switch {
	case !hasNew:
		qualifier = fmt.Sprintf("key removed; falling back to the default policy %q: ", d.defaultPolicy)
	case ValidUpdatePolicy(newV):
		effective = UpdatePolicy(newV)
	default:
		effective = UpdatePolicyNone
		qualifier = fmt.Sprintf("value %q is invalid; treating as %q: ", newV, UpdatePolicyNone)
	}

	var behaviour string
	switch effective {
	case UpdatePolicyFull:
		behaviour = "any detected drift will now be applied automatically"
	case UpdatePolicyImageOnly:
		behaviour = "only drift confined to Docker image changes will be applied; anything else is surfaced as a diff"
	default:
		behaviour = "drift will be surfaced but no longer applied"
	}

	note := ""
	if source == "nomad" {
		note = " (note: policy is read from the HCL side; the live job's value does not drive behaviour)"
	}
	return qualifier + behaviour + note
}
