// Package nomad compares HCL job definitions against a live Nomad cluster and
// reports any diffs it finds.
package nomad

import (
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/gerrowadat/nomad-botherer/internal/config"
)

// jobBlockRe matches a top-level Nomad job stanza in HCL.
// Files without this pattern are silently skipped (e.g. ACL policies, volumes, namespaces).
var jobBlockRe = regexp.MustCompile(`(?m)^\s*job\s+"`)

// DiffType describes the relationship between a job in HCL and in Nomad.
type DiffType string

const (
	// DiffTypeModified means the job exists in both HCL and Nomad but the
	// definitions differ (Nomad plan shows changes).
	DiffTypeModified DiffType = "modified"

	// DiffTypeMissingFromNomad means the job is defined in HCL but not
	// currently registered in Nomad.
	DiffTypeMissingFromNomad DiffType = "missing_from_nomad"

	// DiffTypeMissingFromHCL means the job is running in Nomad but there is
	// no corresponding HCL file in the repo.
	DiffTypeMissingFromHCL DiffType = "missing_from_hcl"
)

// JobDiff describes a single divergence between the git repo and Nomad.
type JobDiff struct {
	JobID    string   `json:"job_id"`
	HCLFile  string   `json:"hcl_file,omitempty"` // empty for MissingFromHCL
	DiffType DiffType `json:"diff_type"`
	Detail   string   `json:"detail"`

	// PlanDiff holds the structured diff from the Nomad plan API.
	// Only populated for DiffTypeModified entries.
	PlanDiff *nomadapi.JobDiff `json:"-"`
}

// NomadJobsClient is the subset of the Nomad API jobs client we use.
// The concrete *nomadapi.Jobs satisfies this interface; tests inject a mock.
type NomadJobsClient interface {
	ParseHCL(jobHCL string, canonicalize bool) (*nomadapi.Job, error)
	Plan(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error)
	Info(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error)
	List(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error)
}

// Differ runs periodic diff checks and stores the latest results.
type Differ struct {
	jobs            NomadJobsClient
	namespace       string
	includeDeadJobs bool
	jobSelectorGlob   string
	managedMetaPrefix string

	mu              sync.RWMutex
	diffs           []JobDiff
	lastCheckTime   time.Time
	lastCommit      string
	lastNomadIndex  uint64            // Raft index from the last successful List(); protected by mu
	driftFirstSeen  map[string]time.Time // key: driftKey(jobID, diffType); protected by mu

	hclParseErrors      prometheus.Counter
	hclFilesSkipped     prometheus.Counter
	diffChecks          prometheus.Counter
	diffChecksSkipped   prometheus.Counter
	staleChecks         prometheus.Counter
	jobsSkippedBySel    *prometheus.CounterVec
	nomadAPIErrors      *prometheus.CounterVec
	lastCheck           prometheus.Gauge
	jobDiffs            *prometheus.GaugeVec
	driftedJobs         *prometheus.GaugeVec
	jobDriftSince       *prometheus.GaugeVec
}

// newDifferBase constructs a Differ with metrics registered into reg.
func newDifferBase(jobs NomadJobsClient, namespace string, includeDeadJobs bool, jobSelectorGlob, managedMetaPrefix string, reg prometheus.Registerer) *Differ {
	f := promauto.With(reg)
	d := &Differ{
		jobs:              jobs,
		namespace:         namespace,
		includeDeadJobs:   includeDeadJobs,
		jobSelectorGlob:   jobSelectorGlob,
		managedMetaPrefix: managedMetaPrefix,
		driftFirstSeen:  make(map[string]time.Time),
		hclParseErrors: f.NewCounter(prometheus.CounterOpts{
			Name: "nomad_botherer_hcl_parse_errors_total",
			Help: "Total number of HCL files that failed to parse as Nomad job definitions.",
		}),
		hclFilesSkipped: f.NewCounter(prometheus.CounterOpts{
			Name: "nomad_botherer_hcl_non_job_files_skipped_total",
			Help: "Total number of HCL files skipped because they lack a top-level job stanza (e.g. ACL policies, volumes).",
		}),
		diffChecks: f.NewCounter(prometheus.CounterOpts{
			Name: "nomad_botherer_diff_checks_total",
			Help: "Total number of diff checks run against the Nomad cluster.",
		}),
		diffChecksSkipped: f.NewCounter(prometheus.CounterOpts{
			Name: "nomad_botherer_diff_checks_skipped_total",
			Help: "Total number of diff checks skipped because neither the Nomad index nor the git commit changed.",
		}),
		staleChecks: f.NewCounter(prometheus.CounterOpts{
			Name: "nomad_botherer_nomad_staleness_checks_total",
			Help: "Total number of Nomad diff checks triggered by the staleness check.",
		}),
		jobsSkippedBySel: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_jobs_skipped_by_selector_total",
			Help: "Total number of jobs skipped because they did not match the configured selection criteria, by source (hcl or nomad).",
		}, []string{"source"}),
		nomadAPIErrors: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_nomad_api_errors_total",
			Help: "Total number of Nomad API errors by operation.",
		}, []string{"op"}),
		lastCheck: f.NewGauge(prometheus.GaugeOpts{
			Name: "nomad_botherer_last_check_timestamp_seconds",
			Help: "Unix timestamp of the most recent diff check.",
		}),
		jobDiffs: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nomad_botherer_job_diffs",
			Help: "1 for each job/diff-type combination currently detected.",
		}, []string{"job", "diff_type"}),
		driftedJobs: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nomad_botherer_drifted_jobs",
			Help: "Number of jobs currently in each drift state.",
		}, []string{"diff_type"}),
		jobDriftSince: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nomad_botherer_job_drift_first_seen_timestamp_seconds",
			Help: "Unix timestamp when drift was first detected for each job. Cleared when drift resolves. Use time()-metric to get seconds in drift state.",
		}, []string{"job", "diff_type"}),
	}

	// Pre-populate Vec metrics for all finite label values so they appear in
	// Gather() output (with value 0) even before the first Check call.
	for _, src := range []string{"hcl", "nomad"} {
		d.jobsSkippedBySel.WithLabelValues(src)
	}
	for _, op := range []string{"list", "info", "plan"} {
		d.nomadAPIErrors.WithLabelValues(op)
	}
	for _, dt := range []string{string(DiffTypeModified), string(DiffTypeMissingFromNomad), string(DiffTypeMissingFromHCL)} {
		d.driftedJobs.WithLabelValues(dt)
	}

	return d
}

// NewDiffer creates a Differ backed by a real Nomad API client.
func NewDiffer(cfg *config.Config) (*Differ, error) {
	nomadCfg := nomadapi.DefaultConfig()
	nomadCfg.Address = cfg.NomadAddr
	if cfg.NomadToken != "" {
		nomadCfg.SecretID = cfg.NomadToken
	}

	client, err := nomadapi.NewClient(nomadCfg)
	if err != nil {
		return nil, fmt.Errorf("creating nomad client: %w", err)
	}

	return newDifferBase(client.Jobs(), cfg.NomadNamespace, cfg.IncludeDeadJobs, cfg.JobSelectorGlob, cfg.ManagedMetaPrefix, prometheus.DefaultRegisterer), nil
}

// NewWithClient creates a Differ with a custom jobs client, intended for tests.
func NewWithClient(cfg *config.Config, jobs NomadJobsClient) *Differ {
	return newDifferBase(jobs, cfg.NomadNamespace, cfg.IncludeDeadJobs, cfg.JobSelectorGlob, cfg.ManagedMetaPrefix, prometheus.NewRegistry())
}

// NewWithClientAndRegistry creates a Differ with a custom jobs client and Prometheus
// registry. Use this in tests that need to inspect metric values.
func NewWithClientAndRegistry(cfg *config.Config, jobs NomadJobsClient, reg prometheus.Registerer) *Differ {
	return newDifferBase(jobs, cfg.NomadNamespace, cfg.IncludeDeadJobs, cfg.JobSelectorGlob, cfg.ManagedMetaPrefix, reg)
}

// jobIsSelected reports whether a job should be watched. A job is selected when
// its ID matches the configured glob pattern, or when its meta map contains
// "<managedMetaPrefix>_managed" set to "true". If both are empty, no jobs
// are selected.
func (d *Differ) jobIsSelected(jobID string, meta map[string]string) bool {
	if d.jobSelectorGlob != "" {
		if matched, _ := path.Match(d.jobSelectorGlob, jobID); matched {
			return true
		}
	}
	if d.managedMetaPrefix != "" && meta[d.managedMetaPrefix+"_managed"] == "true" {
		return true
	}
	return false
}

// Check compares the given HCL files (path → content) against the live Nomad
// cluster and stores the results. commit is recorded for informational purposes.
func (d *Differ) Check(hclFiles map[string]string, commit string) error {
	// ?meta=true is documented in the Nomad HTTP API (GET /v1/jobs) and
	// causes the list response to include each job's Meta map. Without it,
	// Meta is omitted from the stub and meta-prefix selection cannot work.
	q := &nomadapi.QueryOptions{
		Namespace: d.namespace,
		Params:    map[string]string{"meta": "true"},
	}
	wq := &nomadapi.WriteOptions{Namespace: d.namespace}

	// List all Nomad jobs first. The returned Raft index lets us skip the
	// expensive per-job work when neither Nomad state nor the git commit has
	// changed since the last check.
	allJobs, listMeta, err := d.jobs.List(q)
	if err != nil {
		d.nomadAPIErrors.WithLabelValues("list").Inc()
		slog.Warn("Failed to list Nomad jobs", "err", err)
		allJobs = nil
		listMeta = nil
	}

	d.mu.RLock()
	prevCommit := d.lastCommit
	prevIndex := d.lastNomadIndex
	d.mu.RUnlock()

	if listMeta != nil && listMeta.LastIndex == prevIndex && commit == prevCommit {
		slog.Debug("Skipping diff: Nomad index and commit unchanged", "index", listMeta.LastIndex, "commit", commit)
		d.diffChecksSkipped.Inc()
		return nil
	}

	slog.Info("Running diff check", "commit", commit, "hcl_files", len(hclFiles))
	d.diffChecks.Inc()

	// Parse all HCL files via the Nomad API.
	hclJobs := make(map[string]*nomadapi.Job) // jobID → parsed job
	hclJobFile := make(map[string]string)      // jobID → source HCL file path

	for filename, content := range hclFiles {
		if !jobBlockRe.MatchString(content) {
			slog.Debug("Skipping HCL file with no job stanza", "file", filename)
			d.hclFilesSkipped.Inc()
			continue
		}

		job, err := d.jobs.ParseHCL(content, true)
		if err != nil {
			slog.Warn("Failed to parse HCL file, skipping", "file", filename, "err", err)
			d.hclParseErrors.Inc()
			continue
		}
		if job == nil || job.ID == nil || *job.ID == "" {
			slog.Warn("HCL file yielded no job ID, skipping", "file", filename)
			continue
		}
		jobID := *job.ID
		if !d.jobIsSelected(jobID, job.Meta) {
			slog.Debug("Skipping job not matching selection criteria", "job", jobID, "file", filename)
			d.jobsSkippedBySel.WithLabelValues("hcl").Inc()
			continue
		}
		hclJobs[jobID] = job
		hclJobFile[jobID] = filename
		slog.Debug("Parsed HCL file", "file", filename, "job_id", jobID)
	}

	var diffs []JobDiff

	// For each job defined in HCL, check Nomad.
	for jobID, job := range hclJobs {
		filename := hclJobFile[jobID]

		nomadJob, _, err := d.jobs.Info(jobID, q)
		if err != nil {
			if isNotFound(err) {
				diffs = append(diffs, JobDiff{
					JobID:    jobID,
					HCLFile:  filename,
					DiffType: DiffTypeMissingFromNomad,
					Detail:   "job is defined in HCL but not registered in Nomad",
				})
				continue
			}
			d.nomadAPIErrors.WithLabelValues("info").Inc()
			slog.Warn("Failed to query job from Nomad", "job", jobID, "err", err)
			continue
		}

		// Unless the caller explicitly wants dead jobs included, treat a dead
		// job the same as a missing one.
		if !d.includeDeadJobs && nomadJob != nil && nomadJob.Status != nil && *nomadJob.Status == "dead" {
			slog.Debug("Job is dead in Nomad, treating as missing", "job", jobID)
			diffs = append(diffs, JobDiff{
				JobID:    jobID,
				HCLFile:  filename,
				DiffType: DiffTypeMissingFromNomad,
				Detail:   "job is defined in HCL but is in 'dead' state in Nomad",
			})
			continue
		}

		// Job exists and is live — run a plan to detect config drift.
		// Nomad's deregister call sets Stop=true on the job record. Copy it
		// onto the HCL job so the plan does not report a Stop field diff.
		if nomadJob.Stop != nil && *nomadJob.Stop {
			stop := true
			job.Stop = &stop
		}
		plan, _, err := d.jobs.Plan(job, true, wq)
		if err != nil {
			d.nomadAPIErrors.WithLabelValues("plan").Inc()
			slog.Warn("Failed to plan job", "job", jobID, "err", err)
			continue
		}

		if plan.Diff != nil && plan.Diff.Type != "" && plan.Diff.Type != "None" {
			// For dead jobs, Nomad may return Type="Edited" with only task-group
			// bookkeeping entries (Type="None" task groups). hasContentDiff filters
			// those out so we don't report spurious drift.
			isDead := nomadJob != nil && nomadJob.Status != nil && *nomadJob.Status == "dead"
			if isDead && !hasContentDiff(plan.Diff) {
				continue
			}
			diffs = append(diffs, JobDiff{
				JobID:    jobID,
				HCLFile:  filename,
				DiffType: DiffTypeModified,
				Detail:   fmt.Sprintf("Nomad plan shows diff type %q", plan.Diff.Type),
				PlanDiff: plan.Diff,
			})
		}
	}

	// Find jobs in Nomad that have no corresponding HCL file.
	// Dead jobs are skipped unless --include-dead-jobs is set, since a dead
	// job without HCL is expected (it was stopped intentionally).
	// Only jobs that match the configured selection criteria are considered managed.
	for _, j := range allJobs {
		if !d.includeDeadJobs && j.Status == "dead" {
			continue
		}

		// Meta is populated because the List call includes ?meta=true.
		meta := j.Meta

		if !d.jobIsSelected(j.ID, meta) {
			d.jobsSkippedBySel.WithLabelValues("nomad").Inc()
			continue
		}
		if _, ok := hclJobs[j.ID]; !ok {
			diffs = append(diffs, JobDiff{
				JobID:    j.ID,
				DiffType: DiffTypeMissingFromHCL,
				Detail:   fmt.Sprintf("job is running in Nomad (status: %s) but has no HCL definition in the repo", j.Status),
			})
		}
	}

	now := time.Now()

	// Build the set of currently-drifting job+type keys.
	currentKeys := make(map[string]struct{}, len(diffs))
	for _, diff := range diffs {
		currentKeys[driftKey(diff.JobID, string(diff.DiffType))] = struct{}{}
	}

	d.mu.Lock()
	d.diffs = diffs
	d.lastCheckTime = now
	d.lastCommit = commit
	if listMeta != nil {
		d.lastNomadIndex = listMeta.LastIndex
	}

	// Remove entries that are no longer drifting.
	for k := range d.driftFirstSeen {
		if _, ok := currentKeys[k]; !ok {
			delete(d.driftFirstSeen, k)
		}
	}
	// Record the first time each new drift is observed.
	for k := range currentKeys {
		if _, ok := d.driftFirstSeen[k]; !ok {
			d.driftFirstSeen[k] = now
		}
	}
	// Snapshot first-seen times for metric updates below (outside the lock).
	firstSeenSnapshot := make(map[string]time.Time, len(d.driftFirstSeen))
	for k, v := range d.driftFirstSeen {
		firstSeenSnapshot[k] = v
	}
	d.mu.Unlock()

	d.lastCheck.Set(float64(now.Unix()))
	d.jobDiffs.Reset()
	d.driftedJobs.Reset()
	d.jobDriftSince.Reset()
	typeCounts := make(map[string]int)
	for _, diff := range diffs {
		d.jobDiffs.WithLabelValues(diff.JobID, string(diff.DiffType)).Set(1)
		typeCounts[string(diff.DiffType)]++
	}
	for typ, count := range typeCounts {
		d.driftedJobs.WithLabelValues(typ).Set(float64(count))
	}
	for _, diff := range diffs {
		k := driftKey(diff.JobID, string(diff.DiffType))
		if t, ok := firstSeenSnapshot[k]; ok {
			d.jobDriftSince.WithLabelValues(diff.JobID, string(diff.DiffType)).Set(float64(t.Unix()))
		}
	}

	slog.Info("Diff check complete", "diffs", len(diffs), "commit", commit)
	return nil
}

// ForceCheck runs a diff check unconditionally because the Nomad state has
// exceeded the configured maximum staleness. Increments the staleness counter
// and delegates to Check.
func (d *Differ) ForceCheck(hclFiles map[string]string, commit string) error {
	d.staleChecks.Inc()
	return d.Check(hclFiles, commit)
}

// driftKey returns a map key for a (jobID, diffType) pair.
func driftKey(jobID, diffType string) string {
	return jobID + "\x00" + diffType
}

// Ready reports whether at least one diff check has completed successfully.
// Before the first check finishes, callers cannot distinguish "no drift" from
// "haven't checked yet", so they should treat the Differ as unavailable.
func (d *Differ) Ready() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return !d.lastCheckTime.IsZero()
}

// Diffs returns a snapshot of the latest diffs, the time they were computed,
// and the git commit they were computed against.
func (d *Differ) Diffs() ([]JobDiff, time.Time, string) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make([]JobDiff, len(d.diffs))
	copy(result, d.diffs)
	return result, d.lastCheckTime, d.lastCommit
}

// hasContentDiff reports whether d contains spec changes beyond allocation
// bookkeeping entries. Used to suppress spurious diffs when planning HCL
// against a dead job where task groups appear with Type="None" (no spec
// change, only allocation count bookkeeping).
//
// Type="None" is defined in Nomad source (nomad/structs/diff.go, DiffTypeNone)
// and is returned in plan responses when a task group has no spec changes.
func hasContentDiff(d *nomadapi.JobDiff) bool {
	if d == nil || d.Type == "" || d.Type == "None" {
		return false
	}
	if len(d.Fields) > 0 || len(d.Objects) > 0 {
		return true
	}
	for _, tg := range d.TaskGroups {
		if tg.Type == "Added" || tg.Type == "Deleted" {
			return true
		}
		if len(tg.Fields) > 0 || len(tg.Objects) > 0 {
			return true
		}
		for _, task := range tg.Tasks {
			if task.Type != "None" || len(task.Fields) > 0 || len(task.Objects) > 0 {
				return true
			}
		}
	}
	return false
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "404") || strings.Contains(strings.ToLower(s), "not found")
}
