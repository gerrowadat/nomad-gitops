package nomad

import (
	"log/slog"
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
	// reference (the "image" field inside a task's Config object), possibly
	// alongside nomad-gitops's own managed-meta keys.
	DiffClassImageOnly
	// DiffClassManagedMetaOnly means every Git-owned change is to one of
	// nomad-gitops's own managed-prefix meta keys (e.g. gitops_managed,
	// gitops_update_policy). These are not applied on their own by default:
	// re-registering a running job purely to push our keys onto it is
	// disruptive and unnecessary, since the HCL is already the source of
	// truth for them. They ride along the next real update.
	DiffClassManagedMetaOnly
	// DiffClassOther means the diff contains at least one Git-owned change
	// that is not an image reference or a managed-meta key.
	DiffClassOther
)

func (c DiffClass) String() string {
	switch c {
	case DiffClassNone:
		return "none"
	case DiffClassImageOnly:
		return "image-only"
	case DiffClassManagedMetaOnly:
		return "managed-meta-only"
	default:
		return "other"
	}
}

// changeCounts tallies the changed leaves in a plan diff by kind.
type changeCounts struct {
	image       int
	managedMeta int
	other       int
}

// classifyDiff walks a plan diff and classifies it. autoscaled names the task
// groups that carry a scaling policy: changes to their Count field and
// Scaling object are owned by the autoscaler, not by Git, and are ignored
// (per the "do not fight the autoscaler" design rule). metaPrefix is the
// managed meta prefix (e.g. "gitops"); changes to keys under it are bucketed
// separately. Container nodes (job/group/task marked Edited because a child
// changed) are not leaves and do not affect the result.
func classifyDiff(d *nomadapi.JobDiff, autoscaled map[string]bool, metaPrefix string) DiffClass {
	if d == nil || d.Type == "" || d.Type == "None" {
		return DiffClassNone
	}

	var c changeCounts
	c.addFields(d.Fields, nil, false, false, metaPrefix)
	c.addObjects(d.Objects, metaPrefix, 1)

	for _, tg := range d.TaskGroups {
		if tg == nil {
			continue
		}
		isAuto := autoscaled[tg.Name]

		// Whole-group additions or removals are structural changes.
		if tg.Type == "Added" || tg.Type == "Deleted" {
			c.other++
		}

		var skip map[string]bool
		if isAuto {
			skip = map[string]bool{"Count": true}
		}
		c.addFields(tg.Fields, skip, false, false, metaPrefix)
		for _, o := range tg.Objects {
			if o == nil {
				continue
			}
			if isAuto && o.Name == "Scaling" {
				continue
			}
			c.addObject(o, metaPrefix, 1)
		}

		for _, task := range tg.Tasks {
			if task == nil {
				continue
			}
			if task.Type == "Added" || task.Type == "Deleted" {
				c.other++
			}
			c.addFields(task.Fields, nil, false, false, metaPrefix)
			for _, o := range task.Objects {
				if o == nil {
					continue
				}
				c.addObject(o, metaPrefix, 1)
			}
		}
	}

	switch {
	case c.other > 0:
		return DiffClassOther
	case c.image > 0:
		return DiffClassImageOnly
	case c.managedMeta > 0:
		return DiffClassManagedMetaOnly
	default:
		return DiffClassNone
	}
}

// addObject counts the changed leaves under one object, aware of two special
// objects: "Config" (whose "image" field is a Docker image reference) and
// "Meta" (whose fields are meta keys, so managed-prefix ones are tracked
// separately). Image/meta semantics do not propagate into nested sub-objects.
// depth is the object's nesting level (1 at the top); beyond
// MaxPlanDiffObjectDepth, recursion stops and the unexamined subtree counts as
// a non-image, non-meta change — the conservative reading, since an
// update-policy of image-only or none must not wave through a change nobody
// actually looked at.
func (c *changeCounts) addObject(o *nomadapi.ObjectDiff, metaPrefix string, depth int) {
	if depth > MaxPlanDiffObjectDepth {
		slog.Warn("Plan diff exceeds maximum nesting depth; treating unexamined nesting as a non-trivial change",
			"depth", depth, "max_depth", MaxPlanDiffObjectDepth)
		c.other++
		return
	}
	c.addFields(o.Fields, nil, o.Name == "Meta", o.Name == "Config", metaPrefix)
	c.addObjects(o.Objects, metaPrefix, depth+1)
}

func (c *changeCounts) addObjects(objs []*nomadapi.ObjectDiff, metaPrefix string, depth int) {
	for _, o := range objs {
		if o == nil {
			continue
		}
		c.addObject(o, metaPrefix, depth)
	}
}

// addFields counts changed fields into the right bucket. inMeta marks fields
// inside a Meta object (their names are bare meta keys); inConfig marks
// fields inside a Config object (where "image" is the Docker image). skip
// names fields to ignore entirely (autoscaler-owned Count).
func (c *changeCounts) addFields(fields []*nomadapi.FieldDiff, skip map[string]bool, inMeta, inConfig bool, metaPrefix string) {
	for _, f := range fields {
		if f == nil || !fieldChanged(f.Type) {
			continue
		}
		if skip != nil && skip[f.Name] {
			continue
		}
		switch {
		case inConfig && strings.EqualFold(f.Name, "image"):
			c.image++
		case isManagedMetaName(f.Name, inMeta, metaPrefix):
			c.managedMeta++
		default:
			c.other++
		}
	}
}

// isManagedMetaName reports whether a changed field refers to one of
// nomad-gitops's own meta keys. It accepts both a bare key name when the
// field is inside a Meta object (inMeta) and the wrapped "Meta[key]" form
// anywhere, so it is robust to how different Nomad versions render meta diffs.
func isManagedMetaName(name string, inMeta bool, metaPrefix string) bool {
	if metaPrefix == "" {
		return false
	}
	key := name
	if strings.HasPrefix(name, "Meta[") && strings.HasSuffix(name, "]") {
		key = name[len("Meta[") : len(name)-1]
	} else if !inMeta {
		return false
	}
	return strings.HasPrefix(key, metaPrefix+"_") || strings.HasPrefix(key, metaPrefix+".")
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
