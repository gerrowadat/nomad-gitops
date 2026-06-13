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

// metaChangeHarness drives a Differ whose HCL meta and live meta can be
// swapped between Check cycles, capturing INFO logs.
type metaChangeHarness struct {
	d        *nomad.Differ
	logs     *bytes.Buffer
	hclMeta  map[string]string
	liveMeta map[string]string
	commitN  int
	t        *testing.T
}

func newMetaChangeHarness(t *testing.T, cfg *config.Config) *metaChangeHarness {
	t.Helper()
	h := &metaChangeHarness{logs: &bytes.Buffer{}, t: t}

	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	mock := defaultMock()
	mock.parseHCLFn = func(jobHCL string, normalize bool) (*nomadapi.Job, error) {
		return &nomadapi.Job{ID: strPtr("test-job"), Meta: h.hclMeta}, nil
	}
	mock.listFn = func(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error) {
		if h.liveMeta == nil {
			return nil, nil, nil
		}
		return []*nomadapi.JobListStub{
			{ID: "test-job", Status: "running", Meta: h.liveMeta},
		}, nil, nil
	}
	h.d = nomad.NewWithClientAndRegistry(cfg, mock, prometheus.NewRegistry())
	return h
}

// check runs one diff cycle with a fresh commit so the skip optimisation
// never engages.
func (h *metaChangeHarness) check() {
	h.t.Helper()
	h.commitN++
	commit := strings.Repeat("a", 7) + string(rune('0'+h.commitN))
	if err := h.d.Check(map[string]string{"jobs/test-job.hcl": `job "test-job" {}`}, commit); err != nil {
		h.t.Fatalf("Check: %v", err)
	}
}

func metaChangeCfg() *config.Config {
	return &config.Config{NomadNamespace: "default", JobSelectorGlob: "*", ManagedMetaPrefix: "gitops"}
}

const changeMsg = "Job meta key under the managed prefix changed"

func TestMetaChange_FirstCycleIsSilentBaseline(t *testing.T) {
	h := newMetaChangeHarness(t, metaChangeCfg())
	h.hclMeta = map[string]string{"gitops_managed": "true"}
	h.check()

	if strings.Contains(h.logs.String(), changeMsg) {
		t.Errorf("first cycle should baseline silently, got:\n%s", h.logs.String())
	}
}

func TestMetaChange_OptIn_Logged(t *testing.T) {
	h := newMetaChangeHarness(t, metaChangeCfg())
	h.hclMeta = map[string]string{}
	h.check()
	h.hclMeta = map[string]string{"gitops_managed": "true"}
	h.check()

	logs := h.logs.String()
	if !strings.Contains(logs, changeMsg) {
		t.Fatalf("opt-in transition should be logged, got:\n%s", logs)
	}
	if !strings.Contains(logs, "change=added") || !strings.Contains(logs, "key=gitops_managed") {
		t.Errorf("log should name the added key, got:\n%s", logs)
	}
	if !strings.Contains(logs, "now opted in to GitOps management") {
		t.Errorf("log should state the consequence, got:\n%s", logs)
	}
	got := testutil.ToFloat64(nomad.MetaKeyChanges(h.d).WithLabelValues("test-job", "hcl"))
	if got != 1 {
		t.Errorf("meta_key_changes_total{hcl}: want 1, got %v", got)
	}
}

func TestMetaChange_OptOut_GlobStillWatches(t *testing.T) {
	h := newMetaChangeHarness(t, metaChangeCfg()) // glob "*" matches everything
	h.hclMeta = map[string]string{"gitops_managed": "true"}
	h.check()
	h.hclMeta = map[string]string{}
	h.check()

	logs := h.logs.String()
	if !strings.Contains(logs, "change=removed") {
		t.Fatalf("opt-out should be logged as removed, got:\n%s", logs)
	}
	if !strings.Contains(logs, "still matches --job-selector-glob") {
		t.Errorf("with a matching glob the action should say the job remains watched, got:\n%s", logs)
	}
}

func TestMetaChange_OptOut_NoGlob_StopsManaging(t *testing.T) {
	cfg := metaChangeCfg()
	cfg.JobSelectorGlob = ""
	h := newMetaChangeHarness(t, cfg)
	h.hclMeta = map[string]string{"gitops_managed": "true"}
	h.check()
	h.hclMeta = map[string]string{"gitops_managed": "false"}
	h.check()

	logs := h.logs.String()
	if !strings.Contains(logs, "no longer managed") || !strings.Contains(logs, "stops diffing") {
		t.Errorf("opt-out without glob should state management stops, got:\n%s", logs)
	}
}

func TestMetaChange_PolicyTransitions(t *testing.T) {
	t.Run("policy added full", func(t *testing.T) {
		h := newMetaChangeHarness(t, metaChangeCfg())
		h.hclMeta = map[string]string{"gitops_managed": "true"}
		h.check()
		h.hclMeta = map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
		h.check()

		if !strings.Contains(h.logs.String(), "any detected drift will now be applied automatically") {
			t.Errorf("policy full action missing, got:\n%s", h.logs.String())
		}
	})

	t.Run("policy changed to none", func(t *testing.T) {
		h := newMetaChangeHarness(t, metaChangeCfg())
		h.hclMeta = map[string]string{"gitops_update_policy": "full"}
		h.check()
		h.hclMeta = map[string]string{"gitops_update_policy": "none"}
		h.check()

		logs := h.logs.String()
		if !strings.Contains(logs, "old=full") || !strings.Contains(logs, "new=none") {
			t.Errorf("old and new values should be logged, got:\n%s", logs)
		}
		if !strings.Contains(logs, "surfaced but no longer applied") {
			t.Errorf("policy none action missing, got:\n%s", logs)
		}
	})

	t.Run("policy removed falls back to default", func(t *testing.T) {
		h := newMetaChangeHarness(t, metaChangeCfg())
		h.hclMeta = map[string]string{"gitops_update_policy": "full"}
		h.check()
		h.hclMeta = map[string]string{}
		h.check()

		logs := h.logs.String()
		if !strings.Contains(logs, "falling back to the default policy") || !strings.Contains(logs, "surfaced but no longer applied") {
			t.Errorf("fallback action missing, got:\n%s", logs)
		}
	})

	t.Run("policy changed to invalid value", func(t *testing.T) {
		h := newMetaChangeHarness(t, metaChangeCfg())
		h.hclMeta = map[string]string{"gitops_update_policy": "full"}
		h.check()
		h.hclMeta = map[string]string{"gitops_update_policy": "yolo"}
		h.check()

		logs := h.logs.String()
		if !strings.Contains(logs, "is invalid; treating as") || !strings.Contains(logs, "surfaced but no longer applied") {
			t.Errorf("invalid-value action missing, got:\n%s", logs)
		}
	})
}

func TestMetaChange_LiveSide_ManualRegisterLosesKeys(t *testing.T) {
	h := newMetaChangeHarness(t, metaChangeCfg())
	h.hclMeta = map[string]string{"gitops_managed": "true"}
	h.liveMeta = map[string]string{"gitops_managed": "true", "gitops_update_policy": "full"}
	h.check()
	// Someone ran `nomad job run` from plain HCL: the live job lost the keys.
	h.liveMeta = map[string]string{}
	h.check()

	logs := h.logs.String()
	if !strings.Contains(logs, "source=nomad") {
		t.Fatalf("live-side change should be logged with source=nomad, got:\n%s", logs)
	}
	if !strings.Contains(logs, "policy is read from the HCL side") {
		t.Errorf("live policy change should note HCL is authoritative, got:\n%s", logs)
	}
	if !strings.Contains(logs, "Git is the source of truth and the live value does not drive behaviour") {
		t.Errorf("live managed-key change should note Git primacy, got:\n%s", logs)
	}
	got := testutil.ToFloat64(nomad.MetaKeyChanges(h.d).WithLabelValues("test-job", "nomad"))
	if got != 2 { // both keys removed
		t.Errorf("meta_key_changes_total{nomad}: want 2, got %v", got)
	}
}

func TestMetaChange_NoChange_NoLogs(t *testing.T) {
	h := newMetaChangeHarness(t, metaChangeCfg())
	h.hclMeta = map[string]string{"gitops_managed": "true", "gitops_update_policy": "image-only"}
	h.check()
	h.logs.Reset()
	h.check()
	h.check()

	if strings.Contains(h.logs.String(), changeMsg) {
		t.Errorf("unchanged meta should not be logged, got:\n%s", h.logs.String())
	}
}

func TestMetaChange_UnknownKeyChange_Logged(t *testing.T) {
	h := newMetaChangeHarness(t, metaChangeCfg())
	h.hclMeta = map[string]string{"gitops_mystery": "a"}
	h.check()
	h.hclMeta = map[string]string{"gitops_mystery": "b"}
	h.check()

	logs := h.logs.String()
	if !strings.Contains(logs, "change=changed") || !strings.Contains(logs, "key=gitops_mystery") {
		t.Fatalf("unknown-key change should be logged, got:\n%s", logs)
	}
	if !strings.Contains(logs, "no behaviour change") {
		t.Errorf("unknown-key action should say no behaviour change, got:\n%s", logs)
	}
}
