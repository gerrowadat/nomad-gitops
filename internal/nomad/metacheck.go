package nomad

import (
	"log/slog"
	"strings"
)

// Meta-key validation: anything in a job's meta that starts with the managed
// prefix is addressed to nomad-botherer, so a key we don't recognise — or a
// recognised key with a value we can't act on — is almost certainly a typo
// that is silently changing behaviour (e.g. `gitops.managed` instead of
// `gitops_managed` drops the job out of scope without a trace).
//
// Each unique (job, key, value, issue) is logged once per process; the
// nomad_botherer_meta_key_issues_total counter keeps incrementing every
// cycle the issue persists, so dashboards can see it without log spam.

const (
	metaIssueUnknownKey   = "unknown_key"
	metaIssueInvalidValue = "invalid_value"
)

// validManagedValues are the accepted values for <prefix>_managed: "true"
// opts in, "false" is an explicit opt-out. Anything else ("True", "yes",
// "1") silently fails the opt-in check and is flagged.
func validManagedValue(v string) bool {
	return v == "true" || v == "false"
}

// validateManagedMeta scans meta for keys addressed to nomad-botherer and
// records an issue for any it cannot act on. source says where the meta was
// seen ("hcl:<file>" or "nomad") for the log line.
func (d *Differ) validateManagedMeta(jobID, source string, meta map[string]string) {
	if d.managedMetaPrefix == "" || len(meta) == 0 {
		return
	}
	underscored := d.managedMetaPrefix + "_"
	dotted := d.managedMetaPrefix + "."
	for k, v := range meta {
		if !strings.HasPrefix(k, underscored) && !strings.HasPrefix(k, dotted) {
			continue
		}
		switch k {
		case d.managedMetaPrefix + "_managed":
			if !validManagedValue(v) {
				d.recordMetaIssue(jobID, source, k, v, metaIssueInvalidValue,
					`accepted values are "true" and "false"`)
			}
		case d.managedMetaPrefix + "_update_policy":
			if !ValidUpdatePolicy(v) {
				d.recordMetaIssue(jobID, source, k, v, metaIssueInvalidValue,
					`accepted values are "full", "image-only" and "none"; treated as "none"`)
			}
		default:
			d.recordMetaIssue(jobID, source, k, v, metaIssueUnknownKey,
				"not a key nomad-botherer understands — possible typo")
		}
	}
}

// recordMetaIssue counts the issue and logs it the first time it is seen.
// Known keys with bad values log at ERROR — the author clearly intended to
// configure nomad-botherer and the value is being ignored or downgraded.
// Unknown keys under the prefix log at WARN.
func (d *Differ) recordMetaIssue(jobID, source, key, value, issue, hint string) {
	d.metaKeyIssues.WithLabelValues(jobID, issue).Inc()

	dedup := strings.Join([]string{jobID, key, value, issue}, "\x00")
	if _, seen := d.metaIssuesLogged.LoadOrStore(dedup, struct{}{}); seen {
		return
	}
	if issue == metaIssueInvalidValue {
		slog.Error("Job meta has a recognised nomad-botherer key with an invalid value",
			"job", jobID, "source", source, "key", key, "value", value, "hint", hint)
	} else {
		slog.Warn("Job meta has an unrecognised key under the managed prefix",
			"job", jobID, "source", source, "key", key, "value", value, "hint", hint)
	}
}
