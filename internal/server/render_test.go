package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
	"github.com/gerrowadat/nomad-botherer/internal/server"
)

// diffsResponse builds a /diffs response with full control over the diff source.
// Both sources are "ready" (non-zero times) so the handler serves the render output.
func diffsResponse(t *testing.T, diffs []nomad.JobDiff, lastCheck time.Time) string {
	t.Helper()
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main"}
	diffSrc := &mockDiffSource{diffs: diffs, lastCheck: lastCheck, lastCommit: "abc"}
	gitSrc := &mockGitSource{lastUpdate: time.Now()}
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())
	req := httptest.NewRequest(http.MethodGet, "/diffs", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Body.String()
}

// ── renderDiffsText ───────────────────────────────────────────────────────────

func TestDiffs_NoLastCheckLine(t *testing.T) {
	// When lastCheck is provided as a non-zero time, the "Last check:" line should appear.
	body := diffsResponse(t, nil, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	if !strings.Contains(body, "Last check:") {
		t.Error("non-zero lastCheck should produce a 'Last check:' line")
	}
	if !strings.Contains(body, "No differences") {
		t.Error("expected 'No differences' message")
	}
}

func TestDiffs_Modified_NoPlanDiff(t *testing.T) {
	diffs := []nomad.JobDiff{
		{JobID: "myapp", DiffType: nomad.DiffTypeModified, Detail: "plan shows Edited"},
	}
	body := diffsResponse(t, diffs, time.Now())
	if !strings.Contains(body, "+/- Job:") {
		t.Error("modified without PlanDiff should render with '+/-' prefix")
	}
	if !strings.Contains(body, "plan shows Edited") {
		t.Error("detail should appear when there is no PlanDiff")
	}
}

// ── renderJobDiff ─────────────────────────────────────────────────────────────

func TestDiffs_Modified_WithJobNoHCLFile(t *testing.T) {
	// hclFile == "" takes the else branch in renderJobDiff.
	diffs := []nomad.JobDiff{
		{
			JobID:    "myapp",
			HCLFile:  "",
			DiffType: nomad.DiffTypeModified,
			PlanDiff: &nomadapi.JobDiff{Type: "Edited", ID: "myapp"},
		},
	}
	body := diffsResponse(t, diffs, time.Now())
	if !strings.Contains(body, `"myapp"`) {
		t.Error("job ID should appear even with empty HCLFile")
	}
}

// ── renderFields ──────────────────────────────────────────────────────────────

func TestDiffs_Modified_WithAddedDeletedFields(t *testing.T) {
	diffs := []nomad.JobDiff{
		{
			JobID:    "myapp",
			HCLFile:  "myapp.hcl",
			DiffType: nomad.DiffTypeModified,
			PlanDiff: &nomadapi.JobDiff{
				Type: "Edited",
				ID:   "myapp",
				Fields: []*nomadapi.FieldDiff{
					{Type: "Added", Name: "new-field", New: "newval"},
					{Type: "Deleted", Name: "old-field", Old: "oldval"},
				},
			},
		},
	}
	body := diffsResponse(t, diffs, time.Now())
	if !strings.Contains(body, `+ new-field`) {
		t.Error("Added field should appear with '+' prefix")
	}
	if !strings.Contains(body, `- old-field`) {
		t.Error("Deleted field should appear with '-' prefix")
	}
}

func TestDiffs_Modified_WithFieldAnnotations(t *testing.T) {
	diffs := []nomad.JobDiff{
		{
			JobID:    "myapp",
			HCLFile:  "myapp.hcl",
			DiffType: nomad.DiffTypeModified,
			PlanDiff: &nomadapi.JobDiff{
				Type: "Edited",
				ID:   "myapp",
				Fields: []*nomadapi.FieldDiff{
					{
						Type:        "Edited",
						Name:        "count",
						Old:         "1",
						New:         "3",
						Annotations: []string{"forces create/destroy update"},
					},
				},
			},
		},
	}
	body := diffsResponse(t, diffs, time.Now())
	if !strings.Contains(body, "forces create/destroy update") {
		t.Error("field annotation should appear in output")
	}
}

// ── renderTaskGroupDiff ───────────────────────────────────────────────────────

func TestDiffs_Modified_WithTaskGroupUpdates(t *testing.T) {
	diffs := []nomad.JobDiff{
		{
			JobID:    "myapp",
			DiffType: nomad.DiffTypeModified,
			PlanDiff: &nomadapi.JobDiff{
				Type: "Edited",
				ID:   "myapp",
				TaskGroups: []*nomadapi.TaskGroupDiff{
					{
						Type: "Edited",
						Name: "web",
						// "stop" is zero and should be filtered out.
						Updates: map[string]uint64{"create": 2, "stop": 0},
					},
				},
			},
		},
	}
	body := diffsResponse(t, diffs, time.Now())
	if !strings.Contains(body, "create") {
		t.Error("non-zero update count should appear in task group output")
	}
	if strings.Contains(body, "stop") {
		t.Error("zero update count should be filtered from task group output")
	}
}

func TestDiffs_Modified_WithAddedTaskGroup(t *testing.T) {
	// Task group type "Added" exercises diffSymbol("Added") → "+".
	diffs := []nomad.JobDiff{
		{
			JobID:    "myapp",
			DiffType: nomad.DiffTypeModified,
			PlanDiff: &nomadapi.JobDiff{
				Type: "Edited",
				ID:   "myapp",
				TaskGroups: []*nomadapi.TaskGroupDiff{
					{Type: "Added", Name: "new-group"},
				},
			},
		},
	}
	body := diffsResponse(t, diffs, time.Now())
	if !strings.Contains(body, `+ Task Group: "new-group"`) {
		t.Errorf("added task group should render with '+' prefix, got:\n%s", body)
	}
}

func TestDiffs_Modified_WithDeletedTaskGroup(t *testing.T) {
	// Task group type "Deleted" exercises diffSymbol("Deleted") → "-".
	diffs := []nomad.JobDiff{
		{
			JobID:    "myapp",
			DiffType: nomad.DiffTypeModified,
			PlanDiff: &nomadapi.JobDiff{
				Type: "Edited",
				ID:   "myapp",
				TaskGroups: []*nomadapi.TaskGroupDiff{
					{Type: "Deleted", Name: "old-group"},
				},
			},
		},
	}
	body := diffsResponse(t, diffs, time.Now())
	if !strings.Contains(body, `- Task Group: "old-group"`) {
		t.Errorf("deleted task group should render with '-' prefix, got:\n%s", body)
	}
}

// ── renderTaskDiff ────────────────────────────────────────────────────────────

func TestDiffs_Modified_WithTaskAnnotations(t *testing.T) {
	diffs := []nomad.JobDiff{
		{
			JobID:    "myapp",
			DiffType: nomad.DiffTypeModified,
			PlanDiff: &nomadapi.JobDiff{
				Type: "Edited",
				ID:   "myapp",
				TaskGroups: []*nomadapi.TaskGroupDiff{
					{
						Type: "Edited",
						Name: "web",
						Tasks: []*nomadapi.TaskDiff{
							{
								Type:        "Edited",
								Name:        "server",
								Annotations: []string{"in-place update"},
							},
						},
					},
				},
			},
		},
	}
	body := diffsResponse(t, diffs, time.Now())
	if !strings.Contains(body, "in-place update") {
		t.Error("task annotation should appear in output")
	}
}

// ── secret redaction ──────────────────────────────────────────────────────────

// redactedEnvDiff is a JobDiff as the differ stores it when redaction is on:
// values already replaced and the field annotated.
func redactedEnvDiff() []nomad.JobDiff {
	return []nomad.JobDiff{
		{
			JobID:    "myapp",
			HCLFile:  "myapp.hcl",
			DiffType: nomad.DiffTypeModified,
			PlanDiff: &nomadapi.JobDiff{
				Type: "Edited",
				ID:   "myapp",
				Fields: []*nomadapi.FieldDiff{
					{Type: "Edited", Name: "Env[API_TOKEN]", Old: nomad.RedactedValue, New: nomad.RedactedValue, Annotations: []string{"value redacted"}},
				},
			},
		},
	}
}

func diffsResponseWithRedaction(t *testing.T, diffs []nomad.JobDiff, redact bool) string {
	t.Helper()
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main", RedactSecrets: redact}
	diffSrc := &mockDiffSource{diffs: diffs, lastCheck: time.Now(), lastCommit: "abc"}
	gitSrc := &mockGitSource{lastUpdate: time.Now()}
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())
	req := httptest.NewRequest(http.MethodGet, "/diffs", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Body.String()
}

func TestDiffs_RedactionBannerAndMarkers(t *testing.T) {
	body := diffsResponseWithRedaction(t, redactedEnvDiff(), true)
	if !strings.Contains(body, "shown as [REDACTED]") {
		t.Errorf("redaction banner missing from /diffs output:\n%s", body)
	}
	if !strings.Contains(body, `~ Env[API_TOKEN]: "[REDACTED]" => "[REDACTED]" (value redacted)`) {
		t.Errorf("redacted field should keep diff appearance with markers and annotation:\n%s", body)
	}
}

func TestDiffs_RedactionDisabled_NoBanner(t *testing.T) {
	body := diffsResponseWithRedaction(t, redactedEnvDiff(), false)
	if strings.Contains(body, "shown as [REDACTED]") {
		t.Errorf("banner should be absent when redaction is disabled:\n%s", body)
	}
}

func TestDiffs_RedactionEnabled_NoDiffs_NoBanner(t *testing.T) {
	body := diffsResponseWithRedaction(t, nil, true)
	if strings.Contains(body, "[REDACTED]") {
		t.Errorf("banner should be absent when there are no diffs:\n%s", body)
	}
	if !strings.Contains(body, "No differences") {
		t.Error("expected 'No differences' message")
	}
}

// ── apply action / reason ──────────────────────────────────────────────────────

func TestDiffs_ShowsApplyActionReason(t *testing.T) {
	diffs := []nomad.JobDiff{
		{
			JobID:       "hass",
			HCLFile:     "hass.hcl",
			DiffType:    nomad.DiffTypeModified,
			Detail:      "plan shows Edited",
			ApplyAction: nomad.ApplyActionPreExisting,
		},
	}
	body := diffsResponse(t, diffs, time.Now())
	if !strings.Contains(body, "drift pre-dates the scope change") {
		t.Errorf("/diffs should explain why the diff is not applied, got:\n%s", body)
	}
}

func TestDiffs_NoApplyActionWhenEmpty(t *testing.T) {
	diffs := []nomad.JobDiff{
		{JobID: "api", DiffType: nomad.DiffTypeModified, Detail: "x"},
	}
	body := diffsResponse(t, diffs, time.Now())
	if strings.Contains(body, "→") {
		t.Errorf("no reason arrow expected when ApplyAction is empty, got:\n%s", body)
	}
}
