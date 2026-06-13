package server

import (
	"fmt"
	"sort"
	"strings"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

// renderDiffsText produces a nomad-job-plan-style plain-text representation
// of the current diff state. When redactionEnabled is true a banner states
// that potentially sensitive values have been replaced with [REDACTED].
func renderDiffsText(diffs []nomad.JobDiff, lastCheck time.Time, commit string, redactionEnabled bool) string {
	var b strings.Builder

	fmt.Fprintln(&b, "nomad-botherer diff report")
	if !lastCheck.IsZero() {
		fmt.Fprintf(&b, "Last check: %s | Commit: %s\n", lastCheck.Format(time.RFC3339), commit)
	}
	fmt.Fprintln(&b)

	if len(diffs) == 0 {
		fmt.Fprintln(&b, "No differences detected.")
		return b.String()
	}

	fmt.Fprintf(&b, "%d difference(s) detected:\n", len(diffs))
	if redactionEnabled {
		fmt.Fprintf(&b, "NOTE: potentially sensitive values (env vars, template bodies, secret-like keys) are shown as %s. Disable with --redact-secrets=false.\n", nomad.RedactedValue)
	}

	for _, d := range diffs {
		fmt.Fprintln(&b)
		switch d.DiffType {
		case nomad.DiffTypeMissingFromNomad:
			fmt.Fprintf(&b, "+ Job: %q\n", d.JobID)
			fmt.Fprintf(&b, "  Defined in %s but not registered in Nomad.\n", d.HCLFile)
		case nomad.DiffTypeMissingFromHCL:
			fmt.Fprintf(&b, "- Job: %q\n", d.JobID)
			fmt.Fprintf(&b, "  %s\n", d.Detail)
		case nomad.DiffTypeModified:
			if d.PlanDiff != nil {
				renderJobDiff(&b, d.PlanDiff, d.HCLFile)
			} else {
				fmt.Fprintf(&b, "+/- Job: %q\n", d.JobID)
				fmt.Fprintf(&b, "  %s\n", d.Detail)
			}
		}
		if d.ApplyAction != "" {
			fmt.Fprintf(&b, "  → %s\n", d.ApplyAction.Describe())
		}
	}

	return b.String()
}

func renderJobDiff(b *strings.Builder, jd *nomadapi.JobDiff, hclFile string) {
	if hclFile != "" {
		fmt.Fprintf(b, "%s Job: %q  (%s)\n", diffSymbol(jd.Type), jd.ID, hclFile)
	} else {
		fmt.Fprintf(b, "%s Job: %q\n", diffSymbol(jd.Type), jd.ID)
	}
	renderFields(b, jd.Fields, "  ")
	renderObjects(b, jd.Objects, "  ")
	for _, tg := range jd.TaskGroups {
		renderTaskGroupDiff(b, tg, "  ")
	}
}

func renderTaskGroupDiff(b *strings.Builder, tg *nomadapi.TaskGroupDiff, indent string) {
	var updates string
	if len(tg.Updates) > 0 {
		parts := make([]string, 0, len(tg.Updates))
		for k, v := range tg.Updates {
			if v > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", v, k))
			}
		}
		sort.Strings(parts)
		if len(parts) > 0 {
			updates = " (" + strings.Join(parts, ", ") + ")"
		}
	}
	fmt.Fprintf(b, "%s%s Task Group: %q%s\n", indent, diffSymbol(tg.Type), tg.Name, updates)
	renderFields(b, tg.Fields, indent+"  ")
	renderObjects(b, tg.Objects, indent+"  ")
	for _, t := range tg.Tasks {
		renderTaskDiff(b, t, indent+"  ")
	}
}

func renderTaskDiff(b *strings.Builder, t *nomadapi.TaskDiff, indent string) {
	ann := ""
	if len(t.Annotations) > 0 {
		ann = " (" + strings.Join(t.Annotations, ", ") + ")"
	}
	fmt.Fprintf(b, "%s%s Task: %q%s\n", indent, diffSymbol(t.Type), t.Name, ann)
	renderFields(b, t.Fields, indent+"  ")
	renderObjects(b, t.Objects, indent+"  ")
}

func renderFields(b *strings.Builder, fields []*nomadapi.FieldDiff, indent string) {
	for _, f := range fields {
		ann := ""
		if len(f.Annotations) > 0 {
			ann = " (" + strings.Join(f.Annotations, ", ") + ")"
		}
		switch f.Type {
		case "Added":
			fmt.Fprintf(b, "%s+ %s: %q%s\n", indent, f.Name, f.New, ann)
		case "Deleted":
			fmt.Fprintf(b, "%s- %s: %q%s\n", indent, f.Name, f.Old, ann)
		case "Edited":
			fmt.Fprintf(b, "%s~ %s: %q => %q%s\n", indent, f.Name, f.Old, f.New, ann)
		}
	}
}

func renderObjects(b *strings.Builder, objects []*nomadapi.ObjectDiff, indent string) {
	for _, o := range objects {
		fmt.Fprintf(b, "%s%s %s {\n", indent, diffSymbol(o.Type), o.Name)
		renderFields(b, o.Fields, indent+"  ")
		renderObjects(b, o.Objects, indent+"  ")
		fmt.Fprintf(b, "%s}\n", indent)
	}
}

func diffSymbol(t string) string {
	switch t {
	case "Added":
		return "+"
	case "Deleted":
		return "-"
	case "Edited":
		return "+/-"
	default:
		return "?"
	}
}
