package nomad

import (
	"log/slog"
	"strings"

	nomadapi "github.com/hashicorp/nomad/api"
)

// RedactedValue replaces potentially sensitive values in plan diffs when
// secret redaction is enabled (--redact-secrets, on by default).
const RedactedValue = "[REDACTED]"

// redactedAnnotation is appended to each redacted field's annotations so the
// rendered diff states explicitly that the value was withheld.
const redactedAnnotation = "value redacted"

// secretKeywords are matched case-insensitively as substrings of field names.
// A field whose name contains any of these has its values redacted.
var secretKeywords = []string{
	"secret", "password", "passwd", "token", "credential",
	"api_key", "apikey", "private_key", "access_key",
}

// isSensitiveFieldName reports whether a plan-diff field's values should be
// redacted. All env vars are treated as potentially sensitive (Nomad renders
// them as fields named "Env[KEY]"), as are template bodies (EmbeddedTmpl) and
// any field whose name contains a secret-like keyword (e.g. Meta[db_password],
// driver Config[registry_token]).
func isSensitiveFieldName(name string) bool {
	n := strings.ToLower(name)
	if strings.HasPrefix(n, "env[") || n == "embeddedtmpl" {
		return true
	}
	for _, kw := range secretKeywords {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}

// RedactJobDiff replaces potentially sensitive field values throughout d with
// RedactedValue, in place, and annotates each redacted field. The diff
// structure (field names, added/deleted/edited types, nesting) is preserved so
// the rendered output still reads like a plan diff. Returns the number of
// fields redacted.
func RedactJobDiff(d *nomadapi.JobDiff) int {
	if d == nil {
		return 0
	}
	n := redactFields(d.Fields)
	n += redactObjects(d.Objects, 1)
	for _, tg := range d.TaskGroups {
		if tg == nil {
			continue
		}
		n += redactFields(tg.Fields)
		n += redactObjects(tg.Objects, 1)
		for _, t := range tg.Tasks {
			if t == nil {
				continue
			}
			n += redactFields(t.Fields)
			n += redactObjects(t.Objects, 1)
		}
	}
	return n
}

// redactObjects walks a diff's Objects tree redacting sensitive field values.
// depth is the nesting level (1 at the top); beyond MaxPlanDiffObjectDepth,
// recursion stops rather than continuing without bound — see
// diffdepth.go. A diff pathological enough to hit this cap is not a shape any
// legitimate job spec produces.
func redactObjects(objs []*nomadapi.ObjectDiff, depth int) int {
	if depth > MaxPlanDiffObjectDepth {
		slog.Warn("Plan diff exceeds maximum nesting depth; stopped redacting beyond this point",
			"depth", depth, "max_depth", MaxPlanDiffObjectDepth)
		return 0
	}
	n := 0
	for _, o := range objs {
		if o == nil {
			continue
		}
		n += redactFields(o.Fields)
		n += redactObjects(o.Objects, depth+1)
	}
	return n
}

// redactFields redacts the non-empty values of sensitive fields. Empty sides
// are left empty so Added fields still render as additions and Deleted fields
// as deletions.
func redactFields(fields []*nomadapi.FieldDiff) int {
	n := 0
	for _, f := range fields {
		if f == nil || !isSensitiveFieldName(f.Name) {
			continue
		}
		if f.Old == "" && f.New == "" {
			continue
		}
		if f.Old != "" {
			f.Old = RedactedValue
		}
		if f.New != "" {
			f.New = RedactedValue
		}
		f.Annotations = append(f.Annotations, redactedAnnotation)
		n++
	}
	return n
}
