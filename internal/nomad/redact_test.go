package nomad_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gerrowadat/nomad-gitops/internal/nomad"
)

// deepObjectDiffChain builds a chain of nested ObjectDiffs depth levels deep,
// returning the outermost node.
func deepObjectDiffChain(depth int) *nomadapi.ObjectDiff {
	cur := &nomadapi.ObjectDiff{Type: "Edited", Name: "leaf"}
	for i := 1; i < depth; i++ {
		cur = &nomadapi.ObjectDiff{
			Type:    "Edited",
			Name:    fmt.Sprintf("nested%d", i),
			Objects: []*nomadapi.ObjectDiff{cur},
		}
	}
	return cur
}

func TestRedactJobDiff_EnvValues(t *testing.T) {
	d := &nomadapi.JobDiff{
		Type: "Edited",
		ID:   "myapp",
		TaskGroups: []*nomadapi.TaskGroupDiff{
			{
				Type: "Edited",
				Name: "web",
				Tasks: []*nomadapi.TaskDiff{
					{
						Type: "Edited",
						Name: "app",
						Fields: []*nomadapi.FieldDiff{
							{Type: "Edited", Name: "Env[DATABASE_URL]", Old: "postgres://old", New: "postgres://new"},
							{Type: "Added", Name: "Env[NEW_VAR]", New: "added-value"},
							{Type: "Deleted", Name: "Env[OLD_VAR]", Old: "deleted-value"},
							{Type: "Edited", Name: "Image", Old: "app:1", New: "app:2"},
						},
					},
				},
			},
		},
	}

	n := nomad.RedactJobDiff(d)
	if n != 3 {
		t.Errorf("redacted count: want 3, got %d", n)
	}

	fields := d.TaskGroups[0].Tasks[0].Fields
	edited, added, deleted, image := fields[0], fields[1], fields[2], fields[3]

	if edited.Old != nomad.RedactedValue || edited.New != nomad.RedactedValue {
		t.Errorf("edited env var not redacted: old=%q new=%q", edited.Old, edited.New)
	}
	// Empty sides stay empty so Added still looks Added and Deleted Deleted.
	if added.Old != "" || added.New != nomad.RedactedValue {
		t.Errorf("added env var: want empty old and redacted new, got old=%q new=%q", added.Old, added.New)
	}
	if deleted.Old != nomad.RedactedValue || deleted.New != "" {
		t.Errorf("deleted env var: want redacted old and empty new, got old=%q new=%q", deleted.Old, deleted.New)
	}
	if image.Old != "app:1" || image.New != "app:2" {
		t.Errorf("non-sensitive field modified: old=%q new=%q", image.Old, image.New)
	}

	for _, f := range []*nomadapi.FieldDiff{edited, added, deleted} {
		if len(f.Annotations) == 0 || !strings.Contains(strings.Join(f.Annotations, ","), "redacted") {
			t.Errorf("field %q: missing redaction annotation, got %v", f.Name, f.Annotations)
		}
	}
	if len(image.Annotations) != 0 {
		t.Errorf("non-sensitive field annotated: %v", image.Annotations)
	}
}

func TestRedactJobDiff_TemplateAndKeywords(t *testing.T) {
	d := &nomadapi.JobDiff{
		Type: "Edited",
		ID:   "myapp",
		Objects: []*nomadapi.ObjectDiff{
			{
				Type: "Edited",
				Name: "Meta",
				Fields: []*nomadapi.FieldDiff{
					{Type: "Edited", Name: "Meta[db_password]", Old: "hunter2", New: "hunter3"},
					{Type: "Edited", Name: "Meta[team]", Old: "infra", New: "platform"},
				},
			},
		},
		TaskGroups: []*nomadapi.TaskGroupDiff{
			{
				Type: "Edited",
				Name: "web",
				Tasks: []*nomadapi.TaskDiff{
					{
						Type: "Edited",
						Name: "app",
						Objects: []*nomadapi.ObjectDiff{
							{
								Type: "Edited",
								Name: "Template",
								Fields: []*nomadapi.FieldDiff{
									{Type: "Edited", Name: "EmbeddedTmpl", Old: "SECRET={{key \"a\"}}", New: "SECRET={{key \"b\"}}"},
									{Type: "Edited", Name: "DestPath", Old: "local/a.env", New: "local/b.env"},
								},
							},
							{
								Type: "Edited",
								Name: "Config",
								Fields: []*nomadapi.FieldDiff{
									{Type: "Edited", Name: "Config[registry_token]", Old: "tok-old", New: "tok-new"},
								},
							},
						},
					},
				},
			},
		},
	}

	n := nomad.RedactJobDiff(d)
	if n != 3 {
		t.Errorf("redacted count: want 3 (password, template, token), got %d", n)
	}

	if got := d.Objects[0].Fields[0].New; got != nomad.RedactedValue {
		t.Errorf("Meta[db_password] not redacted: %q", got)
	}
	if got := d.Objects[0].Fields[1].New; got != "platform" {
		t.Errorf("Meta[team] should not be redacted: %q", got)
	}
	tmpl := d.TaskGroups[0].Tasks[0].Objects[0].Fields
	if tmpl[0].Old != nomad.RedactedValue || tmpl[0].New != nomad.RedactedValue {
		t.Errorf("EmbeddedTmpl not redacted: old=%q new=%q", tmpl[0].Old, tmpl[0].New)
	}
	if tmpl[1].New != "local/b.env" {
		t.Errorf("DestPath should not be redacted: %q", tmpl[1].New)
	}
	cfgField := d.TaskGroups[0].Tasks[0].Objects[1].Fields[0]
	if cfgField.Old != nomad.RedactedValue || cfgField.New != nomad.RedactedValue {
		t.Errorf("Config[registry_token] not redacted: old=%q new=%q", cfgField.Old, cfgField.New)
	}
}

func TestRedactJobDiff_NilAndEmpty(t *testing.T) {
	if n := nomad.RedactJobDiff(nil); n != 0 {
		t.Errorf("nil diff: want 0, got %d", n)
	}
	if n := nomad.RedactJobDiff(&nomadapi.JobDiff{Type: "Edited"}); n != 0 {
		t.Errorf("empty diff: want 0, got %d", n)
	}
	// A sensitive field with both sides empty is not counted or annotated.
	d := &nomadapi.JobDiff{
		Fields: []*nomadapi.FieldDiff{{Type: "Edited", Name: "Env[EMPTY]"}},
	}
	if n := nomad.RedactJobDiff(d); n != 0 {
		t.Errorf("empty-valued env field: want 0, got %d", n)
	}
	if len(d.Fields[0].Annotations) != 0 {
		t.Errorf("empty-valued env field should not be annotated: %v", d.Fields[0].Annotations)
	}
}

func TestRedactJobDiff_NilEntriesSkipped(t *testing.T) {
	d := &nomadapi.JobDiff{
		Type:   "Edited",
		Fields: []*nomadapi.FieldDiff{nil, {Type: "Edited", Name: "Env[X]", Old: "a", New: "b"}},
		Objects: []*nomadapi.ObjectDiff{
			nil,
			{Type: "Edited", Name: "Meta", Fields: []*nomadapi.FieldDiff{nil}},
		},
		TaskGroups: []*nomadapi.TaskGroupDiff{
			nil,
			{Type: "Edited", Name: "web", Tasks: []*nomadapi.TaskDiff{nil}},
		},
	}
	if n := nomad.RedactJobDiff(d); n != 1 {
		t.Errorf("nil entries should be skipped without panic; want 1 redaction, got %d", n)
	}
	if d.Fields[1].New != nomad.RedactedValue {
		t.Errorf("non-nil env field should still be redacted: %q", d.Fields[1].New)
	}
}

// TestRedactJobDiff_DepthCapPreventsUnboundedRecursion verifies that a plan
// diff nested far deeper than any real Nomad job spec produces (e.g. a
// deliberately crafted HCL file) does not recurse without bound. A shallow
// sensitive field alongside the deep chain still gets redacted normally.
func TestRedactJobDiff_DepthCapPreventsUnboundedRecursion(t *testing.T) {
	deep := deepObjectDiffChain(nomad.MaxPlanDiffObjectDepth + 50)
	d := &nomadapi.JobDiff{
		Type: "Edited",
		Objects: []*nomadapi.ObjectDiff{
			deep,
			{
				Type:   "Edited",
				Name:   "Config",
				Fields: []*nomadapi.FieldDiff{{Type: "Edited", Name: "registry_password", Old: "a", New: "b"}},
			},
		},
	}

	done := make(chan int, 1)
	go func() { done <- nomad.RedactJobDiff(d) }()
	select {
	case n := <-done:
		if n != 1 {
			t.Errorf("want 1 redaction (the shallow sensitive field), got %d", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RedactJobDiff did not return; recursion depth cap did not stop it")
	}

	cfgField := d.Objects[1].Fields[0]
	if cfgField.New != nomad.RedactedValue {
		t.Errorf("shallow sensitive field alongside a deep chain should still be redacted: %q", cfgField.New)
	}
}
