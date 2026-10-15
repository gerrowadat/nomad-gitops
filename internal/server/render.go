package server

import (
	"fmt"
	"sort"
	"strings"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gerrowadat/nomad-gitops/internal/nomad"
)

// renderDiffsText produces a nomad-job-plan-style plain-text representation
// of the current diff state. When redactionEnabled is true a banner states
// that potentially sensitive values have been replaced with [REDACTED].
func renderDiffsText(diffs []nomad.JobDiff, lastCheck time.Time, commit string, redactionEnabled bool) string {
	var b strings.Builder

	fmt.Fprintln(&b, "nomad-gitops diff report")
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
		if d.ApplyDetail != "" {
			fmt.Fprintf(&b, "  → %s\n", d.ApplyDetail)
		} else if d.ApplyAction != "" {
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
	renderObjectsDepth(b, objects, indent, 1)
}

// renderObjectsDepth is renderObjects with an explicit nesting level. depth is
// 1 at the top; beyond nomad.MaxPlanDiffObjectDepth, rendering stops rather
// than recursing without bound, mirroring the cap classification and
// redaction apply to the same plan-diff tree (internal/nomad/diffdepth.go).
func renderObjectsDepth(b *strings.Builder, objects []*nomadapi.ObjectDiff, indent string, depth int) {
	if depth > nomad.MaxPlanDiffObjectDepth {
		fmt.Fprintf(b, "%s... (diff truncated: exceeds maximum nesting depth)\n", indent)
		return
	}
	for _, o := range objects {
		fmt.Fprintf(b, "%s%s %s {\n", indent, diffSymbol(o.Type), o.Name)
		renderFields(b, o.Fields, indent+"  ")
		renderObjectsDepth(b, o.Objects, indent+"  ", depth+1)
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

// freezeRow is one row of the console's frozen-jobs table.
type freezeRow struct {
	JobID  string
	When   string
	Reason string
	Path   string
	// Class is the CSS class for the When cell: indefinite, expired and
	// malformed records render as problems, a time-boxed freeze as a notice.
	Class string
}

// freezeRows splits the current freeze records into the two tables the console
// shows — freezes in force, and expired records left behind — and reports any
// problem with the freeze store itself. Anything hanging or left paused has to
// be visible here, so an indefinite, malformed or expired record is called out
// rather than just counted.
func (s *Server) freezeRows(now time.Time) (inForce, expired []freezeRow, problem string) {
	if !s.diffs.FreezeEnabled() {
		return nil, nil, ""
	}
	recs, lastRead, healthy := s.diffs.Freezes()

	held := false
	switch {
	case !healthy && lastRead.IsZero():
		// Never once readable: freezes are not being honoured at all. Applies
		// are deliberately not held here — see freezeHold in the nomad
		// package — so the thing to report is that freezes do not work.
		problem = "has never been readable, so freezes are NOT being honoured. Grant Nomad Variables read+list on the freeze prefix, or set --freeze-variable-prefix=\"\" to disable freeze support."
	case !healthy:
		problem = "unreadable since the last successful read. Applies are held once the view passes --freeze-max-staleness. Usually a Nomad Variables ACL problem."
		held = true
	case s.cfg.FreezeMaxStaleness > 0 && now.Sub(lastRead) > s.cfg.FreezeMaxStaleness:
		problem = fmt.Sprintf("last read %s ago, older than --freeze-max-staleness=%s — every apply is being held.",
			humanizeDuration(now.Sub(lastRead)), s.cfg.FreezeMaxStaleness)
		held = true
	}
	if held && s.cfg.FreezeFailOpen {
		problem += " --freeze-fail-open is set, so applies proceed as if nothing were frozen."
	}

	for _, r := range recs {
		row := freezeRow{JobID: r.JobID, Reason: r.Reason, Path: r.VariablePath}
		switch r.State {
		case nomad.FreezeStateExpired:
			row.When = fmt.Sprintf("expired %s ago", humanizeDuration(now.Sub(r.Expiry())))
			row.Class = "bad"
			expired = append(expired, row)
			continue
		case nomad.FreezeStateIndefinite:
			row.When = fmt.Sprintf("%s ago, no expiry — indefinite", humanizeDuration(now.Sub(r.Created())))
			row.Class = "bad"
		case nomad.FreezeStateMalformed:
			row.When = "malformed record — read as an indefinite freeze"
			row.Class = "bad"
			if r.Issue != "" {
				row.Reason = r.Issue
			}
		default:
			row.When = fmt.Sprintf("%s ago, expires in %s",
				humanizeDuration(now.Sub(r.Created())), humanizeDuration(r.Expiry().Sub(now)))
			row.Class = "starting"
		}
		inForce = append(inForce, row)
	}
	return inForce, expired, problem
}

// humanizeDuration renders d at the coarsest useful unit — "22m", "3h 10m",
// "42 days" — because an exact duration is noise in a console someone is
// reading to find out whether something has been stuck for minutes or weeks.
func humanizeDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) % 60; m != 0 {
			return fmt.Sprintf("%dh %dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	default:
		days := int(d.Hours()) / 24
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	}
}
