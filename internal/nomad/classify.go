package nomad

import (
	"strings"

	nomadapi "github.com/hashicorp/nomad/api"
)

// DiffClass categorises a plan diff for update-policy decisions.
type DiffClass int

const (
	// DiffClassNone means the diff contains no changes that Git owns —
	// either it is empty, or everything in it is autoscaler-owned
	// Count/Scaling churn. Nothing to apply.
	DiffClassNone DiffClass = iota
	// DiffClassImageOnly means every Git-owned change is a Docker image
	// reference (the "image" field inside a task's Config object).
	DiffClassImageOnly
	// DiffClassOther means the diff contains at least one Git-owned change
	// that is not an image reference.
	DiffClassOther
)

func (c DiffClass) String() string {
	switch c {
	case DiffClassNone:
		return "none"
	case DiffClassImageOnly:
		return "image-only"
	default:
		return "other"
	}
}

// classifyDiff walks a plan diff and classifies it. autoscaledGroups names
// the task groups that carry a scaling policy: changes to their Count field
// and Scaling object are owned by the autoscaler, not by Git, and are
// ignored for classification (per the "do not fight the autoscaler" design
// rule). Container nodes (job/group/task marked Edited because a child
// changed) are not leaves and do not affect the result.
func classifyDiff(d *nomadapi.JobDiff, autoscaledGroups map[string]bool) DiffClass {
	if d == nil || d.Type == "" || d.Type == "None" {
		return DiffClassNone
	}

	imageChanges := 0
	otherChanges := 0

	// Job-level fields and objects are never image references.
	otherChanges += countChangedFields(d.Fields, nil)
	otherChanges += countChangedObjectLeaves(d.Objects)

	for _, tg := range d.TaskGroups {
		if tg == nil {
			continue
		}
		autoscaled := autoscaledGroups[tg.Name]

		// Whole-group additions or removals are structural changes.
		if tg.Type == "Added" || tg.Type == "Deleted" {
			otherChanges++
		}

		skipFields := map[string]bool{}
		if autoscaled {
			skipFields["Count"] = true
		}
		otherChanges += countChangedFields(tg.Fields, skipFields)
		for _, o := range tg.Objects {
			if o == nil {
				continue
			}
			if autoscaled && o.Name == "Scaling" {
				continue
			}
			otherChanges += countChangedObjectLeaves([]*nomadapi.ObjectDiff{o})
		}

		for _, task := range tg.Tasks {
			if task == nil {
				continue
			}
			if task.Type == "Added" || task.Type == "Deleted" {
				otherChanges++
			}
			otherChanges += countChangedFields(task.Fields, nil)
			for _, o := range task.Objects {
				if o == nil {
					continue
				}
				if o.Name == "Config" {
					img, other := splitConfigChanges(o)
					imageChanges += img
					otherChanges += other
					continue
				}
				otherChanges += countChangedObjectLeaves([]*nomadapi.ObjectDiff{o})
			}
		}
	}

	switch {
	case otherChanges > 0:
		return DiffClassOther
	case imageChanges > 0:
		return DiffClassImageOnly
	default:
		return DiffClassNone
	}
}

// splitConfigChanges counts changed fields in a task's Config object,
// separating the "image" field from everything else. Nested objects inside
// Config (e.g. mounts, port maps) are never image references.
func splitConfigChanges(cfg *nomadapi.ObjectDiff) (image, other int) {
	for _, f := range cfg.Fields {
		if f == nil || !fieldChanged(f.Type) {
			continue
		}
		if strings.EqualFold(f.Name, "image") {
			image++
		} else {
			other++
		}
	}
	other += countChangedObjectLeaves(cfg.Objects)
	return image, other
}

// countChangedFields counts fields with a changed type, skipping any whose
// name is in skip.
func countChangedFields(fields []*nomadapi.FieldDiff, skip map[string]bool) int {
	n := 0
	for _, f := range fields {
		if f == nil || !fieldChanged(f.Type) {
			continue
		}
		if skip != nil && skip[f.Name] {
			continue
		}
		n++
	}
	return n
}

// countChangedObjectLeaves counts changed field leaves anywhere under the
// given objects.
func countChangedObjectLeaves(objs []*nomadapi.ObjectDiff) int {
	n := 0
	for _, o := range objs {
		if o == nil {
			continue
		}
		n += countChangedFields(o.Fields, nil)
		n += countChangedObjectLeaves(o.Objects)
	}
	return n
}

func fieldChanged(t string) bool {
	return t == "Added" || t == "Deleted" || t == "Edited"
}

// autoscaledGroups returns the names of task groups in the parsed job that
// carry an enabled scaling policy.
func autoscaledGroups(job *nomadapi.Job) map[string]bool {
	if job == nil {
		return nil
	}
	var out map[string]bool
	for _, tg := range job.TaskGroups {
		if tg == nil || tg.Name == nil || tg.Scaling == nil {
			continue
		}
		if tg.Scaling.Enabled != nil && !*tg.Scaling.Enabled {
			continue
		}
		if out == nil {
			out = make(map[string]bool)
		}
		out[*tg.Name] = true
	}
	return out
}
