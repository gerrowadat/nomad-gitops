package nomad_test

import (
	"testing"

	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

// taskDiffWithConfig builds a task diff whose Config object contains the
// given changed fields.
func taskDiffWithConfig(fields ...*nomadapi.FieldDiff) *nomadapi.TaskDiff {
	return &nomadapi.TaskDiff{
		Type: "Edited",
		Name: "app",
		Objects: []*nomadapi.ObjectDiff{
			{Type: "Edited", Name: "Config", Fields: fields},
		},
	}
}

func imageField() *nomadapi.FieldDiff {
	return &nomadapi.FieldDiff{Type: "Edited", Name: "image", Old: "app:1", New: "app:2"}
}

func wrapJobDiff(tg *nomadapi.TaskGroupDiff) *nomadapi.JobDiff {
	return &nomadapi.JobDiff{Type: "Edited", ID: "myapp", TaskGroups: []*nomadapi.TaskGroupDiff{tg}}
}

func TestClassifyDiff(t *testing.T) {
	cases := []struct {
		name       string
		diff       *nomadapi.JobDiff
		autoscaled map[string]bool
		want       nomad.DiffClass
	}{
		{
			name: "nil diff",
			diff: nil,
			want: nomad.DiffClassNone,
		},
		{
			name: "type none",
			diff: &nomadapi.JobDiff{Type: "None"},
			want: nomad.DiffClassNone,
		},
		{
			name: "edited but no changed leaves",
			diff: wrapJobDiff(&nomadapi.TaskGroupDiff{Type: "Edited", Name: "web"}),
			want: nomad.DiffClassNone,
		},
		{
			name: "image only",
			diff: wrapJobDiff(&nomadapi.TaskGroupDiff{
				Type: "Edited", Name: "web",
				Tasks: []*nomadapi.TaskDiff{taskDiffWithConfig(imageField())},
			}),
			want: nomad.DiffClassImageOnly,
		},
		{
			name: "image plus env",
			diff: wrapJobDiff(&nomadapi.TaskGroupDiff{
				Type: "Edited", Name: "web",
				Tasks: []*nomadapi.TaskDiff{
					{
						Type: "Edited", Name: "app",
						Fields: []*nomadapi.FieldDiff{
							{Type: "Edited", Name: "Env[FOO]", Old: "a", New: "b"},
						},
						Objects: []*nomadapi.ObjectDiff{
							{Type: "Edited", Name: "Config", Fields: []*nomadapi.FieldDiff{imageField()}},
						},
					},
				},
			}),
			want: nomad.DiffClassOther,
		},
		{
			name: "image plus count with scaling policy",
			diff: wrapJobDiff(&nomadapi.TaskGroupDiff{
				Type: "Edited", Name: "web",
				Fields: []*nomadapi.FieldDiff{
					{Type: "Edited", Name: "Count", Old: "2", New: "5"},
				},
				Tasks: []*nomadapi.TaskDiff{taskDiffWithConfig(imageField())},
			}),
			autoscaled: map[string]bool{"web": true},
			want:       nomad.DiffClassImageOnly,
		},
		{
			name: "image plus count without scaling policy",
			diff: wrapJobDiff(&nomadapi.TaskGroupDiff{
				Type: "Edited", Name: "web",
				Fields: []*nomadapi.FieldDiff{
					{Type: "Edited", Name: "Count", Old: "2", New: "5"},
				},
				Tasks: []*nomadapi.TaskDiff{taskDiffWithConfig(imageField())},
			}),
			want: nomad.DiffClassOther,
		},
		{
			name: "count only with scaling policy",
			diff: wrapJobDiff(&nomadapi.TaskGroupDiff{
				Type: "Edited", Name: "web",
				Fields: []*nomadapi.FieldDiff{
					{Type: "Edited", Name: "Count", Old: "2", New: "5"},
				},
			}),
			autoscaled: map[string]bool{"web": true},
			want:       nomad.DiffClassNone,
		},
		{
			name: "scaling object change with scaling policy",
			diff: wrapJobDiff(&nomadapi.TaskGroupDiff{
				Type: "Edited", Name: "web",
				Objects: []*nomadapi.ObjectDiff{
					{Type: "Edited", Name: "Scaling", Fields: []*nomadapi.FieldDiff{
						{Type: "Edited", Name: "Max", Old: "5", New: "10"},
					}},
				},
			}),
			autoscaled: map[string]bool{"web": true},
			want:       nomad.DiffClassNone,
		},
		{
			name: "added task",
			diff: wrapJobDiff(&nomadapi.TaskGroupDiff{
				Type: "Edited", Name: "web",
				Tasks: []*nomadapi.TaskDiff{
					{Type: "Added", Name: "sidecar"},
					taskDiffWithConfig(imageField()),
				},
			}),
			want: nomad.DiffClassOther,
		},
		{
			name: "removed group",
			diff: &nomadapi.JobDiff{
				Type: "Edited", ID: "myapp",
				TaskGroups: []*nomadapi.TaskGroupDiff{
					{Type: "Deleted", Name: "old-group"},
				},
			},
			want: nomad.DiffClassOther,
		},
		{
			name: "job-level field change",
			diff: &nomadapi.JobDiff{
				Type: "Edited", ID: "myapp",
				Fields: []*nomadapi.FieldDiff{
					{Type: "Edited", Name: "Priority", Old: "50", New: "80"},
				},
			},
			want: nomad.DiffClassOther,
		},
		{
			name: "non-image config field",
			diff: wrapJobDiff(&nomadapi.TaskGroupDiff{
				Type: "Edited", Name: "web",
				Tasks: []*nomadapi.TaskDiff{taskDiffWithConfig(
					&nomadapi.FieldDiff{Type: "Edited", Name: "network_mode", Old: "host", New: "bridge"},
				)},
			}),
			want: nomad.DiffClassOther,
		},
		{
			name: "template change outside config",
			diff: wrapJobDiff(&nomadapi.TaskGroupDiff{
				Type: "Edited", Name: "web",
				Tasks: []*nomadapi.TaskDiff{
					{
						Type: "Edited", Name: "app",
						Objects: []*nomadapi.ObjectDiff{
							{Type: "Edited", Name: "Template", Fields: []*nomadapi.FieldDiff{
								{Type: "Edited", Name: "EmbeddedTmpl", Old: "a", New: "b"},
							}},
						},
					},
				},
			}),
			want: nomad.DiffClassOther,
		},
		{
			name: "nested object change inside config",
			diff: wrapJobDiff(&nomadapi.TaskGroupDiff{
				Type: "Edited", Name: "web",
				Tasks: []*nomadapi.TaskDiff{
					{
						Type: "Edited", Name: "app",
						Objects: []*nomadapi.ObjectDiff{
							{Type: "Edited", Name: "Config",
								Fields: []*nomadapi.FieldDiff{imageField()},
								Objects: []*nomadapi.ObjectDiff{
									{Type: "Edited", Name: "mounts", Fields: []*nomadapi.FieldDiff{
										{Type: "Added", Name: "target", New: "/data"},
									}},
								},
							},
						},
					},
				},
			}),
			want: nomad.DiffClassOther,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nomad.ClassifyDiff(tc.diff, tc.autoscaled); got != tc.want {
				t.Errorf("classifyDiff = %v, want %v", got, tc.want)
			}
		})
	}
}
