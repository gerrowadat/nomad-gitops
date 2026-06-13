package nomad_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

// metaCheckDiffer builds a Differ whose parsed HCL job carries meta.
func metaCheckDiffer(meta map[string]string) *nomad.Differ {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("test-job"), Meta: meta}, nil
	}
	cfg := &config.Config{NomadNamespace: "default", JobSelectorGlob: "*", ManagedMetaPrefix: "gitops"}
	return nomad.NewWithClientAndRegistry(cfg, mock, prometheus.NewRegistry())
}

func issueCount(t *testing.T, d *nomad.Differ, job, issue string) float64 {
	t.Helper()
	return testutil.ToFloat64(nomad.MetaKeyIssues(d).WithLabelValues(job, issue))
}

func TestMetaCheck_UnknownPrefixedKey_Flagged(t *testing.T) {
	d := metaCheckDiffer(map[string]string{"gitops_managd": "true"}) // typo'd key
	runCheck(t, d, "aaaa111fffff")

	if got := issueCount(t, d, "test-job", "unknown_key"); got != 1 {
		t.Errorf("unknown_key count: want 1, got %v", got)
	}
	if got := issueCount(t, d, "test-job", "invalid_value"); got != 0 {
		t.Errorf("invalid_value count: want 0, got %v", got)
	}
}

func TestMetaCheck_DottedSeparator_Flagged(t *testing.T) {
	// HCL object-form meta allows dotted keys; gitops.managed is the typo'd
	// twin of gitops_managed and silently fails the opt-in check.
	d := metaCheckDiffer(map[string]string{"gitops.managed": "true"})
	runCheck(t, d, "aaaa111fffff")

	if got := issueCount(t, d, "test-job", "unknown_key"); got != 1 {
		t.Errorf("unknown_key count: want 1, got %v", got)
	}
}

func TestMetaCheck_KnownKeyBadValue_Flagged(t *testing.T) {
	cases := map[string]map[string]string{
		"managed wrong case":   {"gitops_managed": "True"},
		"managed wrong word":   {"gitops_managed": "yes"},
		"policy unknown value": {"gitops_update_policy": "everything"},
	}
	for name, meta := range cases {
		t.Run(name, func(t *testing.T) {
			d := metaCheckDiffer(meta)
			runCheck(t, d, "aaaa111fffff")
			if got := issueCount(t, d, "test-job", "invalid_value"); got != 1 {
				t.Errorf("invalid_value count: want 1, got %v", got)
			}
		})
	}
}

func TestMetaCheck_ValidMeta_NotFlagged(t *testing.T) {
	cases := map[string]map[string]string{
		"opt in":           {"gitops_managed": "true", "gitops_update_policy": "image-only"},
		"explicit opt out": {"gitops_managed": "false"},
		"policy none":      {"gitops_update_policy": "none"},
		"policy full":      {"gitops_update_policy": "full"},
		"unrelated keys":   {"team": "infra", "gitopsish_thing": "x", "mygitops_managed": "true"},
		"no meta":          nil,
	}
	for name, meta := range cases {
		t.Run(name, func(t *testing.T) {
			d := metaCheckDiffer(meta)
			runCheck(t, d, "aaaa111fffff")
			for _, issue := range []string{"unknown_key", "invalid_value"} {
				if got := issueCount(t, d, "test-job", issue); got != 0 {
					t.Errorf("%s count: want 0, got %v", issue, got)
				}
			}
		})
	}
}

func TestMetaCheck_EmptyPrefix_NoValidation(t *testing.T) {
	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("test-job"), Meta: map[string]string{"gitops_bogus": "x"}}, nil
	}
	cfg := &config.Config{NomadNamespace: "default", JobSelectorGlob: "*", ManagedMetaPrefix: ""}
	d := nomad.NewWithClientAndRegistry(cfg, mock, prometheus.NewRegistry())
	runCheck(t, d, "aaaa111fffff")

	if got := issueCount(t, d, "test-job", "unknown_key"); got != 0 {
		t.Errorf("no validation expected with an empty prefix, got %v", got)
	}
}

func TestMetaCheck_LiveJobMeta_Flagged(t *testing.T) {
	// The job exists only in Nomad (no HCL); its malformed opt-in key means
	// it is silently out of scope — exactly the case worth flagging.
	mock := defaultMock()
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		return []*nomadapi.JobListStub{
			{ID: "live-only", Status: "running", Meta: map[string]string{"gitops_managed": "True"}},
		}, &nomadapi.QueryMeta{LastIndex: 1}, nil
	}
	cfg := &config.Config{NomadNamespace: "default", ManagedMetaPrefix: "gitops"}
	d := nomad.NewWithClientAndRegistry(cfg, mock, prometheus.NewRegistry())

	if err := d.Check(map[string]string{}, "aaaa111fffff"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got := issueCount(t, d, "live-only", "invalid_value"); got != 1 {
		t.Errorf("live-side invalid_value count: want 1, got %v", got)
	}
}

func TestMetaCheck_LoggedOncePerIssue_CountedEveryCycle(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	d := metaCheckDiffer(map[string]string{
		"gitops_managd":  "true", // unknown key → WARN
		"gitops_managed": "yes",  // bad value → ERROR
	})
	runCheck(t, d, "aaaa111fffff")
	runCheck(t, d, "bbbb222fffff") // second cycle: counted again, not re-logged

	logs := buf.String()
	if got := strings.Count(logs, "unrecognised key under the managed prefix"); got != 1 {
		t.Errorf("unknown-key warning should be logged exactly once, got %d:\n%s", got, logs)
	}
	if got := strings.Count(logs, "recognised nomad-botherer key with an invalid value"); got != 1 {
		t.Errorf("invalid-value error should be logged exactly once, got %d:\n%s", got, logs)
	}
	if !strings.Contains(logs, "level=ERROR msg=\"Job meta has a recognised nomad-botherer key") {
		t.Errorf("known key with bad value should log at ERROR:\n%s", logs)
	}
	if !strings.Contains(logs, "level=WARN msg=\"Job meta has an unrecognised key") {
		t.Errorf("unknown key should log at WARN:\n%s", logs)
	}

	if got := issueCount(t, d, "test-job", "unknown_key"); got != 2 {
		t.Errorf("unknown_key should count every cycle: want 2, got %v", got)
	}
	if got := issueCount(t, d, "test-job", "invalid_value"); got != 2 {
		t.Errorf("invalid_value should count every cycle: want 2, got %v", got)
	}
}
