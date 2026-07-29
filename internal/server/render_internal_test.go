package server

import (
	"strings"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gerrowadat/nomad-gitops/internal/nomad"
)

// TestRenderDiffsText_ZeroLastCheck verifies that a zero lastCheck time omits
// the "Last check:" line from the output.
func TestRenderDiffsText_ZeroLastCheck(t *testing.T) {
	out := renderDiffsText(nil, time.Time{}, "", false)
	if strings.Contains(out, "Last check:") {
		t.Error("zero lastCheck should not produce a 'Last check:' line")
	}
	if !strings.Contains(out, "No differences") {
		t.Error("expected 'No differences' message")
	}
}

// TestRenderObjects verifies that ObjectDiff entries are rendered with braces
// and their nested fields.
func TestRenderObjects(t *testing.T) {
	diffs := []nomad.JobDiff{
		{
			JobID:    "myapp",
			HCLFile:  "myapp.hcl",
			DiffType: nomad.DiffTypeModified,
			PlanDiff: &nomadapi.JobDiff{
				Type: "Edited",
				ID:   "myapp",
				Objects: []*nomadapi.ObjectDiff{
					{
						Type: "Edited",
						Name: "Meta",
						Fields: []*nomadapi.FieldDiff{
							{Type: "Added", Name: "env", New: "production"},
						},
					},
				},
			},
		},
	}
	out := renderDiffsText(diffs, time.Now(), "abc123", false)
	if !strings.Contains(out, "Meta") {
		t.Error("object name should appear in output")
	}
	if !strings.Contains(out, `+ env`) {
		t.Error("object field should appear with '+' prefix")
	}
	if !strings.Contains(out, "{") || !strings.Contains(out, "}") {
		t.Error("object diff should be wrapped in braces")
	}
}

// TestRenderObjects_Nested verifies that ObjectDiff entries nested inside
// other ObjectDiff entries are rendered recursively.
func TestRenderObjects_Nested(t *testing.T) {
	diffs := []nomad.JobDiff{
		{
			JobID:    "myapp",
			HCLFile:  "myapp.hcl",
			DiffType: nomad.DiffTypeModified,
			PlanDiff: &nomadapi.JobDiff{
				Type: "Edited",
				ID:   "myapp",
				Objects: []*nomadapi.ObjectDiff{
					{
						Type: "Edited",
						Name: "Outer",
						Objects: []*nomadapi.ObjectDiff{
							{
								Type: "Added",
								Name: "Inner",
								Fields: []*nomadapi.FieldDiff{
									{Type: "Added", Name: "key", New: "val"},
								},
							},
						},
					},
				},
			},
		},
	}
	out := renderDiffsText(diffs, time.Now(), "abc123", false)
	if !strings.Contains(out, "Outer") {
		t.Error("outer object name should appear in output")
	}
	if !strings.Contains(out, "Inner") {
		t.Error("inner object name should appear in output")
	}
	if !strings.Contains(out, `+ key`) {
		t.Error("inner object field should appear in output")
	}
}

// TestDiffSymbol_AllTypes exercises every branch of diffSymbol.
func TestDiffSymbol_AllTypes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Added", "+"},
		{"Deleted", "-"},
		{"Edited", "+/-"},
		{"None", "?"},
		{"", "?"},
		{"unknown", "?"},
	}
	for _, tc := range cases {
		if got := diffSymbol(tc.in); got != tc.want {
			t.Errorf("diffSymbol(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRenderFields_EditedType verifies that "Edited" fields render with the
// '~' prefix and both old and new values.
func TestRenderFields_EditedType(t *testing.T) {
	diffs := []nomad.JobDiff{
		{
			JobID:    "myapp",
			HCLFile:  "myapp.hcl",
			DiffType: nomad.DiffTypeModified,
			PlanDiff: &nomadapi.JobDiff{
				Type: "Edited",
				ID:   "myapp",
				Fields: []*nomadapi.FieldDiff{
					{Type: "Edited", Name: "count", Old: "1", New: "3"},
				},
			},
		},
	}
	out := renderDiffsText(diffs, time.Now(), "abc123", false)
	if !strings.Contains(out, `~ count: "1" => "3"`) {
		t.Errorf("edited field should render with '~' and old/new values, got:\n%s", out)
	}
}

// TestRenderDiffsText_ApplyDetailPreferred verifies that when a diff carries a
// specific ApplyDetail, the renderer shows it instead of the generic
// ApplyAction.Describe() text.
func TestRenderDiffsText_ApplyDetailPreferred(t *testing.T) {
	detail := `not applied: update policy is "none" (the --default-update-policy default), which never applies drift for this job. Set the policy to "image-only" or "full" to enable applies.`
	diffs := []nomad.JobDiff{
		{
			JobID:       "myapp",
			HCLFile:     "myapp.hcl",
			DiffType:    nomad.DiffTypeModified,
			Detail:      "changed",
			ApplyAction: nomad.ApplyActionPolicyBlocked,
			ApplyDetail: detail,
		},
	}
	out := renderDiffsText(diffs, time.Now(), "abc123", false)
	if !strings.Contains(out, detail) {
		t.Errorf("renderer should show the specific ApplyDetail, got:\n%s", out)
	}
	if strings.Contains(out, nomad.ApplyActionPolicyBlocked.Describe()) {
		t.Errorf("renderer should not fall back to the generic Describe() when ApplyDetail is set, got:\n%s", out)
	}
}

// TestRenderDiffsText_ApplyActionFallback verifies that without an ApplyDetail
// the renderer still shows the generic ApplyAction description.
func TestRenderDiffsText_ApplyActionFallback(t *testing.T) {
	diffs := []nomad.JobDiff{
		{
			JobID:       "myapp",
			HCLFile:     "myapp.hcl",
			DiffType:    nomad.DiffTypeModified,
			Detail:      "changed",
			ApplyAction: nomad.ApplyActionCreationBlocked,
		},
	}
	out := renderDiffsText(diffs, time.Now(), "abc123", false)
	if !strings.Contains(out, nomad.ApplyActionCreationBlocked.Describe()) {
		t.Errorf("renderer should fall back to Describe() when ApplyDetail is empty, got:\n%s", out)
	}
}
