// Package nomad compares HCL job definitions against a live Nomad cluster and
// reports any diffs it finds.
package nomad

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"sort"
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

// SelectionReason describes why a job was included in the watched set.
type SelectionReason string

const (
	// SelectionReasonGlob means the job was selected by the job-selector-glob pattern.
	SelectionReasonGlob SelectionReason = "glob"
	// SelectionReasonMeta means the job was selected by the managed-meta-prefix key.
	SelectionReasonMeta SelectionReason = "meta"
	// SelectionReasonBoth means the job matched both the glob and the meta key.
	SelectionReasonBoth SelectionReason = "both"
)

// SelectedJob records a job that matched the configured selection criteria
// and the reason it was included.
type SelectedJob struct {
	JobID  string          `json:"job_id"`
	Reason SelectionReason `json:"selection_reason"`
}

// JobDiff describes a single divergence between the git repo and Nomad.
type JobDiff struct {
	JobID    string   `json:"job_id"`
	HCLFile  string   `json:"hcl_file,omitempty"` // empty for MissingFromHCL
	DiffType DiffType `json:"diff_type"`
	Detail   string   `json:"detail"`

	// ApplyAction records what nomad-botherer will do about this diff and,
	// when it will not apply it, why. Lets the API and web console explain
	// non-application without log scraping.
	ApplyAction ApplyAction `json:"apply_action,omitempty"`

	// PlanDiff holds the structured diff from the Nomad plan API.
	// Only populated for DiffTypeModified entries.
	PlanDiff *nomadapi.JobDiff `json:"-"`
}

// ApplyAction is the disposition of a detected diff: whether it will be
// applied and, if not, the reason.
type ApplyAction string

const (
	// ApplyActionQueued means an update was enqueued and will be applied.
	ApplyActionQueued ApplyAction = "queued"
	// ApplyActionPolicyBlocked means the effective update policy disallows it.
	ApplyActionPolicyBlocked ApplyAction = "blocked_by_policy"
	// ApplyActionPreExisting means the drift pre-dated the scope change that
	// brought it into scope — the job's opt-in, or a policy widening.
	ApplyActionPreExisting ApplyAction = "blocked_preexisting_drift"
	// ApplyActionCreationBlocked means first-time registration is disabled.
	ApplyActionCreationBlocked ApplyAction = "blocked_creation_disabled"
	// ApplyActionMetaOnly means the diff is confined to our own meta keys.
	ApplyActionMetaOnly ApplyAction = "skipped_meta_only"
	// ApplyActionObservationOnly means a job is running in Nomad with no HCL,
	// and deregistration is disabled (or the job is not deregister-eligible):
	// it is left running, observation-only.
	ApplyActionObservationOnly ApplyAction = "observation_only"
	// ApplyActionDeregisterQueued means an orphaned job will be deregistered.
	ApplyActionDeregisterQueued ApplyAction = "queued_deregister"
	// ApplyActionDeregisterGrace means an orphaned job is deregister-eligible
	// but its grace period has not yet elapsed.
	ApplyActionDeregisterGrace ApplyAction = "deregister_pending_grace"
	// ApplyActionNoChange means the only diff is autoscaler-owned churn.
	ApplyActionNoChange ApplyAction = "no_actionable_change"
	// ApplyActionKnownFailed means the flap-loop guard is holding the apply:
	// the HCL spec matches a recent Nomad job version whose deployment failed,
	// so re-applying it would re-enter a known failure. Released when Git moves
	// to a spec that has not failed.
	ApplyActionKnownFailed ApplyAction = "blocked_known_failed"
)

// Describe returns a human-readable explanation for display.
func (a ApplyAction) Describe() string {
	switch a {
	case ApplyActionQueued:
		return "queued for apply"
	case ApplyActionPolicyBlocked:
		return "not applied: blocked by update policy"
	case ApplyActionPreExisting:
		return "not applied: drift pre-dates the scope change that brought it in — opt-in or policy widening (set --apply-existing-drift)"
	case ApplyActionCreationBlocked:
		return "not applied: job creation disabled (set --enable-job-creation)"
	case ApplyActionMetaOnly:
		return "not applied: change is confined to managed meta keys"
	case ApplyActionObservationOnly:
		return "not applied: running but absent from the repo; left untouched (set --enable-deregister to remove)"
	case ApplyActionDeregisterQueued:
		return "queued for deregistration (removed from the repo)"
	case ApplyActionDeregisterGrace:
		return "will deregister after the grace period (removed from the repo)"
	case ApplyActionNoChange:
		return "no actionable change (autoscaler-owned)"
	case ApplyActionKnownFailed:
		return "not applied: this spec matches a recent failed deployment (flap-loop guard); waiting for a fix in Git"
	default:
		return string(a)
	}
}

// hclEntry is a parsed HCL job that has passed the preliminary selection filter.
// Final selection (using live Nomad meta) is applied in checkHCLCandidate.
type hclEntry struct {
	job     *nomadapi.Job
	file    string
	globSel bool // matched by job-selector-glob
	metaHCL bool // managed meta key present in parsed HCL
}

// HistorySource provides read-only access to prior git state, used to decide
// whether drift pre-dates a job entering management scope. *gitwatch.Watcher
// satisfies it. When nil, pre-existing-drift detection is disabled and drift
// reconciles normally.
type HistorySource interface {
	// FileAtParentOf returns the content of path at the first parent of the
	// named commit. ok is false when the commit is unknown, has no parent
	// (root commit), or the file is absent there. The lookup is keyed off the
	// commit being evaluated — not the repo's current HEAD — so the decision
	// stays consistent with the HCL snapshot passed to Check even if the
	// watcher pulls a newer commit concurrently.
	FileAtParentOf(commit, path string) (content string, ok bool)
}

// NomadJobsClient is the subset of the Nomad API jobs client we use.
// The concrete *nomadapi.Jobs satisfies this interface; tests inject a mock.
type NomadJobsClient interface {
	ParseHCL(jobHCL string, canonicalize bool) (*nomadapi.Job, error)
	Plan(job *nomadapi.Job, diff bool, q *nomadapi.WriteOptions) (*nomadapi.JobPlanResponse, *nomadapi.WriteMeta, error)
	Info(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Job, *nomadapi.QueryMeta, error)
	List(q *nomadapi.QueryOptions) ([]*nomadapi.JobListStub, *nomadapi.QueryMeta, error)
	RegisterOpts(job *nomadapi.Job, opts *nomadapi.RegisterOptions, q *nomadapi.WriteOptions) (*nomadapi.JobRegisterResponse, *nomadapi.WriteMeta, error)
	Deregister(jobID string, purge bool, q *nomadapi.WriteOptions) (string, *nomadapi.WriteMeta, error)
	// Versions returns a job's retained version history (most recent first).
	Versions(jobID string, diffs bool, q *nomadapi.QueryOptions) ([]*nomadapi.Job, []*nomadapi.JobDiff, *nomadapi.QueryMeta, error)
	// Deployments returns a job's deployments (most recent first).
	Deployments(jobID string, all bool, q *nomadapi.QueryOptions) ([]*nomadapi.Deployment, *nomadapi.QueryMeta, error)
	// LatestDeployment returns the job's most recent deployment, or nil.
	LatestDeployment(jobID string, q *nomadapi.QueryOptions) (*nomadapi.Deployment, *nomadapi.QueryMeta, error)
	// Revert rolls a job back to a prior version. enforcePriorVersion, when
	// non-nil, is a CAS guard: the revert only lands if the job is still at that
	// version.
	Revert(jobID string, version uint64, enforcePriorVersion *uint64, q *nomadapi.WriteOptions, consulToken, vaultToken string) (*nomadapi.JobRegisterResponse, *nomadapi.WriteMeta, error)
	// TagVersion attaches a durable name to a job version so it survives GC.
	TagVersion(jobID string, version uint64, name, description string, q *nomadapi.WriteOptions) (*nomadapi.WriteMeta, error)
}

// Differ runs periodic diff checks and stores the latest results.
type Differ struct {
	jobs NomadJobsClient
	// nomadClient is the concrete client behind jobs, retained so the token can
	// be rotated via SetSecretID. nil in tests (which inject a mock jobs client).
	nomadClient *nomadapi.Client
	// tokenFilePath, when non-empty, is the Nomad token file the refresher
	// re-reads; tokenPollInterval is how often; initialToken is the value read
	// at startup, used as the refresher's baseline.
	tokenFilePath     string
	tokenPollInterval time.Duration
	initialToken      string
	// Workload-identity login mode: loginAuthMethod (non-empty enables it) is the
	// JWT auth method; loginJWTFile is the identity JWT to exchange. loginExpiry
	// is the startup token's expiry (nil if login not used or startup login
	// failed), loginFailed records a failed startup login so the refresher
	// retries promptly.
	loginAuthMethod   string
	loginJWTFile      string
	loginExpiry       *time.Time
	loginFailed       bool
	namespace         string
	includeDeadJobs   bool
	jobSelectorGlob   string
	managedMetaPrefix string
	// redactSecrets replaces potentially sensitive plan-diff values (env vars,
	// templates, secret-like keys) with RedactedValue before the diff is
	// stored, so no downstream consumer can expose them.
	redactSecrets bool

	// defaultPolicy is the update policy applied to managed jobs whose HCL
	// meta carries no <prefix>_update_policy key. Defaults to "none": detect
	// and surface drift, never apply it.
	defaultPolicy UpdatePolicy
	// enableJobCreation gates first-time registration of jobs that exist in
	// Git but not in Nomad. Off by default.
	enableJobCreation bool
	// applyMetaOnlyChanges allows a managed-meta-only diff to trigger an
	// update on its own. countMetaOnlyChanges allows it to count as drift.
	// Both off by default.
	applyMetaOnlyChanges bool
	countMetaOnlyChanges bool
	// applyExistingDrift allows drift that pre-existed a job's scope entry
	// (the managed tag added in the HEAD commit) to be applied. Off by default.
	applyExistingDrift bool
	// Deregistration of jobs removed from the repo. enableDeregister gates it;
	// deregisterPurge selects purge vs graceful stop; deregisterGrace is how
	// long a job must stay orphaned first. All off/conservative by default.
	enableDeregister bool
	deregisterPurge  bool
	deregisterGrace  time.Duration
	// flapGuard is the default flap-loop guard mode (history, tag, or off),
	// from --flap-guard. Per-job overridable via the <prefix>_flap_guard meta
	// key. allowRollback is the default for active rollback, from
	// --allow-rollback, overridable via <prefix>_rollback. Both only apply to
	// deployment-producing jobs.
	flapGuard     string
	allowRollback bool
	// history answers whether the managed tag was present before HEAD, used to
	// detect pre-existing drift. nil disables the check.
	history HistorySource
	// managedKeyRe matches the opt-in key set to "true" in raw HCL text, for
	// the cheap parent-content check. policyKeyRe captures the value of the
	// <prefix>_update_policy key in raw HCL text, used to read a job's policy at
	// the parent commit without re-parsing.
	managedKeyRe *regexp.Regexp
	policyKeyRe  *regexp.Regexp
	// applyInterval is the fallback cadence of the applier loop; enqueues
	// also wake it immediately via applyCh.
	applyInterval time.Duration
	updateQueue   *UpdateQueue
	applyCh       chan struct{}

	// metaIssuesLogged dedups meta-key issue log lines: each unique
	// (job, key, value, issue) is logged once per process. The counter
	// metric is not deduped.
	metaIssuesLogged sync.Map

	// rollbackLogged dedups the auto_revert-clash WARN: each job is logged at
	// most once per process when active rollback stands down in favour of
	// Nomad's own auto_revert. The metric is not deduped.
	rollbackLogged sync.Map

	// prevMeta holds the previous cycle's prefix-key snapshots per
	// (source, job), used to notice and log meta-key transitions. prevManaged
	// is the set of job IDs actively managed via HCL last cycle, used to log
	// a job leaving GitOps management exactly once. Both protected by metaMu.
	metaMu      sync.Mutex
	prevMeta    map[string]metaState
	prevManaged map[string]bool

	mu             sync.RWMutex
	diffs          []JobDiff
	selectedJobs   []SelectedJob
	lastCheckTime  time.Time
	lastCommit     string
	lastNomadIndex uint64               // Raft index from the last successful List(); protected by mu
	driftFirstSeen map[string]time.Time // key: driftKey(jobID, diffType); protected by mu

	hclParseErrors    prometheus.Counter
	hclFilesSkipped   prometheus.Counter
	diffChecks        prometheus.Counter
	diffChecksSkipped prometheus.Counter
	staleChecks       prometheus.Counter
	redactedFields    prometheus.Counter
	jobsSkippedBySel  *prometheus.CounterVec
	nomadAPIErrors    *prometheus.CounterVec
	lastCheck         prometheus.Gauge
	jobDiffs          *prometheus.GaugeVec
	driftedJobs       *prometheus.GaugeVec
	jobDriftSince     *prometheus.GaugeVec

	updatesBlockedByPolicy         *prometheus.CounterVec
	updatesBlockedCreationDisabled *prometheus.CounterVec
	jobUpdatesTotal                *prometheus.CounterVec
	pendingUpdates                 prometheus.Gauge
	metaKeyIssues                  *prometheus.CounterVec
	metaKeyChanges                 *prometheus.CounterVec
	metaOnlyDiffs                  *prometheus.CounterVec
	updatesBlockedExistingDrift    *prometheus.CounterVec
	jobsLeftManagement             *prometheus.CounterVec
	updatesBlockedKnownFailed      *prometheus.CounterVec
	rollbacks                      *prometheus.CounterVec
	failedVersionsTagged           *prometheus.CounterVec
	nomadTokenRefreshes            *prometheus.CounterVec
	nomadLogins                    *prometheus.CounterVec
}

// newDifferBase constructs a Differ from config with metrics registered into reg.
func newDifferBase(jobs NomadJobsClient, cfg *config.Config, reg prometheus.Registerer) *Differ {
	defaultPolicy := UpdatePolicy(cfg.DefaultUpdatePolicy)
	if !ValidUpdatePolicy(cfg.DefaultUpdatePolicy) {
		defaultPolicy = UpdatePolicyNone
	}
	applyInterval := cfg.ApplyInterval
	if applyInterval <= 0 {
		applyInterval = 10 * time.Second
	}
	flapGuard := cfg.FlapGuard
	if !validFlapGuardValue(flapGuard) {
		// Empty or invalid (e.g. a Config built directly in a test): fall back
		// to the documented default rather than silently disabling the guard.
		flapGuard = "history"
	}

	var managedKeyRe, policyKeyRe *regexp.Regexp
	if cfg.ManagedMetaPrefix != "" {
		// Matches <prefix>_managed = "true" in raw HCL, in both the block form
		// (bare key) and the object-expression form (quoted key).
		managedKeyRe = regexp.MustCompile(`(?m)"?` + regexp.QuoteMeta(cfg.ManagedMetaPrefix) + `_managed"?\s*=\s*"true"`)
		// Captures the value of <prefix>_update_policy in either HCL form.
		policyKeyRe = regexp.MustCompile(`(?m)"?` + regexp.QuoteMeta(cfg.ManagedMetaPrefix) + `_update_policy"?\s*=\s*"([^"]*)"`)
	}

	f := promauto.With(reg)
	d := &Differ{
		jobs:                 jobs,
		namespace:            cfg.NomadNamespace,
		includeDeadJobs:      cfg.IncludeDeadJobs,
		jobSelectorGlob:      cfg.JobSelectorGlob,
		managedMetaPrefix:    cfg.ManagedMetaPrefix,
		redactSecrets:        cfg.RedactSecrets,
		defaultPolicy:        defaultPolicy,
		enableJobCreation:    cfg.EnableJobCreation,
		applyMetaOnlyChanges: cfg.ApplyMetaOnlyChanges,
		countMetaOnlyChanges: cfg.CountMetaOnlyChanges,
		applyExistingDrift:   cfg.ApplyExistingDrift,
		enableDeregister:     cfg.EnableDeregister,
		deregisterPurge:      cfg.DeregisterPurge,
		deregisterGrace:      cfg.DeregisterGrace,
		managedKeyRe:         managedKeyRe,
		policyKeyRe:          policyKeyRe,
		flapGuard:            flapGuard,
		allowRollback:        cfg.AllowRollback,
		applyInterval:        applyInterval,
		updateQueue:          NewUpdateQueue(),
		applyCh:              make(chan struct{}, 1),
		driftFirstSeen:       make(map[string]time.Time),
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
		redactedFields: f.NewCounter(prometheus.CounterOpts{
			Name: "nomad_botherer_diff_fields_redacted_total",
			Help: "Total number of potentially sensitive plan-diff field values redacted before storage.",
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
		updatesBlockedByPolicy: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_updates_blocked_by_policy_total",
			Help: "Detected diffs that would have produced a JobUpdate but were filtered out by the effective update policy.",
		}, []string{"job", "policy"}),
		updatesBlockedCreationDisabled: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_updates_blocked_creation_disabled_total",
			Help: "First-time registrations blocked because --enable-job-creation is off.",
		}, []string{"job"}),
		jobUpdatesTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_job_updates_total",
			Help: "JobUpdates reaching a terminal state, by operation and status.",
		}, []string{"operation", "status"}),
		pendingUpdates: f.NewGauge(prometheus.GaugeOpts{
			Name: "nomad_botherer_job_updates_pending",
			Help: "Number of JobUpdates currently waiting to be applied.",
		}),
		metaKeyIssues: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_meta_key_issues_total",
			Help: "Job meta keys under the managed prefix that nomad-botherer cannot act on, by issue (unknown_key, invalid_value). Counted every check cycle the issue persists.",
		}, []string{"job", "issue"}),
		metaKeyChanges: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_meta_key_changes_total",
			Help: "Transitions of managed-prefix meta keys (added, removed, changed) noticed between check cycles, by source (hcl or nomad).",
		}, []string{"job", "source"}),
		metaOnlyDiffs: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_meta_only_diffs_total",
			Help: "Diffs confined to nomad-botherer's own meta keys, detected per check cycle. By default these are neither counted as drift nor applied (see --count-meta-only-changes, --apply-meta-only-changes); they converge on the next real update.",
		}, []string{"job"}),
		updatesBlockedExistingDrift: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_updates_blocked_preexisting_total",
			Help: "Updates not enqueued because the drift pre-dated a scope change that brought it in: the job's opt-in (managed tag added) or a policy widening (e.g. image-only to full). Enable with --apply-existing-drift.",
		}, []string{"job"}),
		jobsLeftManagement: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_jobs_left_management_total",
			Help: "Managed jobs that left GitOps management, by reason: tag_removed (gitops_managed dropped from HCL) or removed_from_repo (HCL file deleted or job renamed). Logged once per transition.",
		}, []string{"job", "reason"}),
		updatesBlockedKnownFailed: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_updates_blocked_known_failed_total",
			Help: "Registrations withheld by the flap-loop guard because the HCL spec matches a recent Nomad job version whose deployment failed. The signal that a job is stuck on a known-bad commit awaiting a fix in Git.",
		}, []string{"job"}),
		rollbacks: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_rollbacks_total",
			Help: "Active rollback outcomes for deployment-producing jobs without auto_revert, by result: queued (a revert was enqueued), deferred_auto_revert (stood down because the job's update stanza sets auto_revert), no_stable_version (no stable version to revert to).",
		}, []string{"job", "result"}),
		failedVersionsTagged: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_failed_versions_tagged_total",
			Help: "Failed job versions tagged in Nomad by the flap-guard tag mode (--flap-guard=tag) so the block survives version GC.",
		}, []string{"job"}),
		nomadTokenRefreshes: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_nomad_token_refreshes_total",
			Help: "Re-reads of the Nomad token file (--nomad-token-file), by result: rotated (the token changed and was applied), error (the file could not be read; previous token kept).",
		}, []string{"result"}),
		nomadLogins: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_nomad_logins_total",
			Help: "Workload-identity token exchanges via /v1/acl/login (--nomad-login-auth-method), by result: success (a fresh ACL token was obtained and applied) or error (the exchange failed; previous token kept).",
		}, []string{"result"}),
	}

	// Pre-populate Vec metrics for all finite label values so they appear in
	// Gather() output (with value 0) even before the first Check call.
	for _, src := range []string{"hcl", "nomad"} {
		d.jobsSkippedBySel.WithLabelValues(src)
	}
	for _, op := range []string{"list", "info", "plan", "register", "deregister", "versions", "deployments", "deployment", "revert", "tag"} {
		d.nomadAPIErrors.WithLabelValues(op)
	}
	for _, st := range []JobUpdateStatus{JobUpdateStatusSucceeded, JobUpdateStatusFailed, JobUpdateStatusSuperseded} {
		d.jobUpdatesTotal.WithLabelValues(string(JobUpdateOperationRegister), string(st))
		d.jobUpdatesTotal.WithLabelValues(string(JobUpdateOperationDeregister), string(st))
		d.jobUpdatesTotal.WithLabelValues(string(JobUpdateOperationRevert), string(st))
	}
	for _, dt := range []string{string(DiffTypeModified), string(DiffTypeMissingFromNomad), string(DiffTypeMissingFromHCL)} {
		d.driftedJobs.WithLabelValues(dt)
	}
	for _, r := range []string{"rotated", "error"} {
		d.nomadTokenRefreshes.WithLabelValues(r)
	}
	for _, r := range []string{"success", "error"} {
		d.nomadLogins.WithLabelValues(r)
	}

	return d
}

// NewDiffer creates a Differ backed by a real Nomad API client, registering
// metrics into the default Prometheus registry.
func NewDiffer(cfg *config.Config) (*Differ, error) {
	return NewDifferWithRegistry(cfg, prometheus.DefaultRegisterer)
}

// NewDifferWithRegistry is like NewDiffer but registers metrics into reg rather
// than the default registry, so more than one real-client Differ can be built
// in a single process (e.g. tests exercising token resolution, or embedding)
// without a duplicate-registration panic.
func NewDifferWithRegistry(cfg *config.Config, reg prometheus.Registerer) (*Differ, error) {
	nomadCfg := nomadapi.DefaultConfig()
	nomadCfg.Address = cfg.NomadAddr

	loginMode := cfg.NomadLoginAuthMethod != ""

	var token, watchPath string
	if !loginMode {
		var err error
		token, watchPath, err = resolveNomadToken(cfg)
		if err != nil {
			return nil, err
		}
	}
	// Set the token explicitly (possibly to "") so it always reflects our
	// resolution and overrides whatever DefaultConfig read from NOMAD_TOKEN.
	nomadCfg.SecretID = token

	client, err := nomadapi.NewClient(nomadCfg)
	if err != nil {
		return nil, fmt.Errorf("creating nomad client: %w", err)
	}

	d := newDifferBase(client.Jobs(), cfg, reg)
	d.nomadClient = client
	d.tokenPollInterval = cfg.NomadTokenPollInterval

	if loginMode {
		d.loginAuthMethod = cfg.NomadLoginAuthMethod
		d.loginJWTFile = loginJWTPath(cfg)
		if d.loginJWTFile == "" {
			return nil, fmt.Errorf("--nomad-login-auth-method is set but no JWT file is available: set --nomad-login-jwt-file or run where NOMAD_SECRETS_DIR is set")
		}
		slog.Info("Authenticating to Nomad with workload-identity login (JWT exchange)",
			"auth_method", d.loginAuthMethod, "jwt_file", d.loginJWTFile)
		// Exchange once at startup so the first diff check has a token. A failure
		// here is not fatal — the refresher retries — but is logged clearly.
		secretID, expiry, lerr := d.login()
		if lerr != nil {
			d.loginFailed = true
			d.nomadLogins.WithLabelValues("error").Inc()
			slog.Error("Initial Nomad workload-identity login failed; will retry. Check the auth method name, that the JWT's audience matches the method, and that a binding rule maps this job to a policy",
				"auth_method", d.loginAuthMethod, "jwt_file", d.loginJWTFile, "err", lerr)
		} else {
			client.SetSecretID(secretID)
			d.loginExpiry = expiry
			d.nomadLogins.WithLabelValues("success").Inc()
			slog.Info("Obtained a Nomad ACL token via workload-identity login", "expires", fmtExpiry(expiry))
		}
		return d, nil
	}

	d.tokenFilePath = watchPath
	d.initialToken = token
	switch {
	case watchPath != "":
		if looksLikeJWT(token) {
			slog.Warn("The token file looks like a workload-identity JWT, not an ACL SecretID; a raw WI JWT is rejected by Nomad's Job.Plan RPC. Use --nomad-login-auth-method to exchange it (see docs/setup/nomad-access.md)",
				"token_file", watchPath)
		}
		slog.Info("Authenticating to Nomad with a token file", "token_file", watchPath, "refresh_interval", cfg.NomadTokenPollInterval)
	case token != "":
		if looksLikeJWT(token) {
			slog.Warn("The static Nomad token looks like a workload-identity JWT, not an ACL SecretID; use --nomad-login-auth-method to exchange it (see docs/setup/nomad-access.md)")
		}
		slog.Info("Authenticating to Nomad with a static token")
	default:
		// Anonymous. If a workload-identity token file is sitting there unused,
		// the operator almost certainly meant to authenticate — point them at
		// login exchange rather than letting every plan fail silently.
		if p := defaultWorkloadTokenPath(); p != "" {
			slog.Warn("A workload-identity token file is present but no Nomad authentication is configured. Raw WI tokens are rejected by Nomad's Job.Plan RPC (issue #74); set --nomad-login-auth-method to exchange the identity JWT for an ACL token (see docs/setup/nomad-access.md)",
				"token_file", p)
		} else {
			slog.Info("No Nomad token configured; using anonymous access (works only when ACLs are disabled)")
		}
	}
	return d, nil
}

// login exchanges the workload-identity JWT for a real ACL token via
// /v1/acl/login, returning the SecretID and its expiry.
func (d *Differ) login() (secretID string, expiry *time.Time, err error) {
	jwt, err := readTokenFile(d.loginJWTFile)
	if err != nil {
		return "", nil, err
	}
	if jwt == "" {
		return "", nil, fmt.Errorf("workload-identity JWT file %q is empty", d.loginJWTFile)
	}
	tok, _, err := d.nomadClient.ACLAuth().Login(&nomadapi.ACLLoginRequest{
		AuthMethodName: d.loginAuthMethod,
		LoginToken:     jwt,
	}, &nomadapi.WriteOptions{Namespace: d.namespace})
	if err != nil {
		return "", nil, err
	}
	return tok.SecretID, tok.ExpirationTime, nil
}

// fmtExpiry renders a token expiry for logging.
func fmtExpiry(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// RunTokenRefresher keeps the Nomad token current: in login mode it re-exchanges
// the workload-identity JWT before the ACL token expires; in file mode it
// re-reads the token file. It is a no-op for a static token or no token, and
// blocks until ctx is cancelled.
func (d *Differ) RunTokenRefresher(ctx context.Context) {
	if d.nomadClient == nil {
		return
	}
	if d.loginAuthMethod != "" {
		firstDelay := nextLoginDelay(d.loginExpiry)
		if d.loginFailed {
			firstDelay = loginRetryBackoff
		}
		runLoginRefresher(ctx, firstDelay,
			d.login,
			func(secretID string) {
				d.nomadClient.SetSecretID(secretID)
				d.nomadLogins.WithLabelValues("success").Inc()
				slog.Info("Re-exchanged the Nomad workload-identity token via login")
			},
			func(err error) {
				d.nomadLogins.WithLabelValues("error").Inc()
				slog.Warn("Nomad workload-identity login failed; keeping the previous token and retrying", "auth_method", d.loginAuthMethod, "err", err)
			},
		)
		return
	}
	if d.tokenFilePath == "" {
		return
	}
	interval := d.tokenPollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	refreshTokenFile(ctx, d.tokenFilePath, interval, d.initialToken,
		func(tok string) {
			d.nomadClient.SetSecretID(tok)
			d.nomadTokenRefreshes.WithLabelValues("rotated").Inc()
			slog.Info("Applied a rotated Nomad token from the token file", "token_file", d.tokenFilePath)
		},
		func(err error) {
			d.nomadTokenRefreshes.WithLabelValues("error").Inc()
			slog.Warn("Could not re-read the Nomad token file; keeping the previous token", "token_file", d.tokenFilePath, "err", err)
		},
	)
}

// NewWithClient creates a Differ with a custom jobs client, intended for tests.
func NewWithClient(cfg *config.Config, jobs NomadJobsClient) *Differ {
	return newDifferBase(jobs, cfg, prometheus.NewRegistry())
}

// NewWithClientAndRegistry creates a Differ with a custom jobs client and Prometheus
// registry. Use this in tests that need to inspect metric values.
func NewWithClientAndRegistry(cfg *config.Config, jobs NomadJobsClient, reg prometheus.Registerer) *Differ {
	return newDifferBase(jobs, cfg, reg)
}

// metaKeyPresent reports whether the managed meta key is set in meta.
func (d *Differ) metaKeyPresent(meta map[string]string) bool {
	return d.managedMetaPrefix != "" && meta[d.managedMetaPrefix+"_managed"] == "true"
}

// selectionReasonFor maps a (glob match, meta match) pair to a SelectionReason.
// Returns (false, "") when neither criterion is met.
func selectionReasonFor(glob, meta bool) (bool, SelectionReason) {
	switch {
	case glob && meta:
		return true, SelectionReasonBoth
	case glob:
		return true, SelectionReasonGlob
	case meta:
		return true, SelectionReasonMeta
	default:
		return false, ""
	}
}

// jobSelectionReason reports whether a job should be watched and, if so, why.
func (d *Differ) jobSelectionReason(jobID string, meta map[string]string) (bool, SelectionReason) {
	glob := d.jobSelectorGlob != ""
	if glob {
		matched, _ := path.Match(d.jobSelectorGlob, jobID)
		glob = matched
	}
	return selectionReasonFor(glob, d.metaKeyPresent(meta))
}

// mergeSelectionReason returns the combined reason when a job is seen from
// multiple sources (e.g. HCL phase and Nomad phase). Any combination of two
// different non-empty reasons upgrades to SelectionReasonBoth.
func mergeSelectionReason(existing, incoming SelectionReason) SelectionReason {
	if existing == "" {
		return incoming
	}
	if existing == incoming {
		return existing
	}
	return SelectionReasonBoth
}

// SelectedJobs returns a snapshot of the jobs that matched the configured
// selection criteria during the last check, together with the reason each
// matched. The second and third return values are the same last-check time and
// commit as Diffs().
func (d *Differ) SelectedJobs() ([]SelectedJob, time.Time, string) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make([]SelectedJob, len(d.selectedJobs))
	copy(result, d.selectedJobs)
	return result, d.lastCheckTime, d.lastCommit
}

// parseHCLCandidates parses all HCL files and returns those that pass the
// selection filter (glob match or managed meta key in HCL), plus the set of
// every successfully parsed job ID — selected or not — so the Nomad-side
// phase knows which jobs Git has an opinion about.
// metaSeen collects each parsed job's prefix-key snapshot for change tracking.
func (d *Differ) parseHCLCandidates(hclFiles map[string]string, metaSeen map[string]metaState) (map[string]hclEntry, map[string]struct{}) {
	entries := make(map[string]hclEntry)
	parsedIDs := make(map[string]struct{})
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

		// Flag prefix-addressed meta keys we cannot act on before the
		// selection filter: a typo'd opt-in key is exactly what makes a job
		// silently unselected.
		d.validateManagedMeta(jobID, "hcl:"+filename, job.Meta)
		recordMetaSeen(metaSeen, d, "hcl", jobID, job.Meta)
		parsedIDs[jobID] = struct{}{}

		globSel := d.jobSelectorGlob != ""
		if globSel {
			globSel, _ = path.Match(d.jobSelectorGlob, jobID)
		}
		metaHCL := d.metaKeyPresent(job.Meta)

		if !globSel && !metaHCL {
			slog.Debug("Skipping job not matching selection criteria", "job", jobID, "file", filename)
			d.jobsSkippedBySel.WithLabelValues("hcl").Inc()
			continue
		}
		entries[jobID] = hclEntry{job: job, file: filename, globSel: globSel, metaHCL: metaHCL}
		slog.Debug("Parsed HCL file", "file", filename, "job_id", jobID)
	}
	return entries, parsedIDs
}

// updateCandidate carries the context needed to turn a detected diff into a
// JobUpdate: the parsed job, the CAS token, and the diff classification.
// Policy gating happens later, in maybeEnqueueUpdate.
type updateCandidate struct {
	jobID       string
	hclFile     string
	job         *nomadapi.Job
	modifyIndex uint64
	class       DiffClass
	isCreation  bool               // job absent (or dead) in Nomad: first-time registration
	globSel     bool               // selected by job-selector-glob (no opt-in moment)
	operation   JobUpdateOperation // REGISTER (default) or DEREGISTER
	policy      UpdatePolicy       // effective policy, for DEREGISTER candidates (from live meta)
	action      ApplyAction        // disposition, decided in decideApplyAction
}

// jobModifyIndex safely extracts a job's ModifyIndex.
func jobModifyIndex(j *nomadapi.Job) uint64 {
	if j == nil || j.JobModifyIndex == nil {
		return 0
	}
	return *j.JobModifyIndex
}

// checkHCLCandidate applies final selection against the live Nomad job, then
// runs Info + Plan to produce a diff. Returns (false, "", nil, nil) when the
// job should be skipped; (true, reason, nil, nil) when selected but no diff
// was found; (true, reason, &diff, cand) when a diff was detected. cand is
// non-nil only when the diff is actionable (a REGISTER could resolve it).
func (d *Differ) checkHCLCandidate(jobID string, entry hclEntry, q *nomadapi.QueryOptions, wq *nomadapi.WriteOptions) (bool, SelectionReason, *JobDiff, *updateCandidate) {
	nomadJob, _, infoErr := d.jobs.Info(jobID, q)
	notFound := infoErr != nil && isNotFound(infoErr)

	// Git is always the source of truth for nomad-botherer's own behaviour:
	// when a job has an HCL file, its keys alone decide selection. The opt-in
	// key in HCL selects the job even when the live copy does not carry it
	// yet — the key's absence on the live job is itself drift, and applying
	// it (policy permitting) is how the live meta converges. A live key with
	// no HCL counterpart never selects; it is surfaced as a meta-change
	// notice instead.
	_, reason := selectionReasonFor(entry.globSel, entry.metaHCL)

	if notFound {
		diff := &JobDiff{
			JobID:    jobID,
			HCLFile:  entry.file,
			DiffType: DiffTypeMissingFromNomad,
			Detail:   "job is defined in HCL but not registered in Nomad",
		}
		// ModifyIndex 0 with EnforceIndex means "job must not exist", which
		// is exactly the guard a first registration wants.
		cand := &updateCandidate{
			jobID: jobID, hclFile: entry.file, job: entry.job,
			modifyIndex: 0, class: DiffClassOther, isCreation: true, globSel: entry.globSel,
		}
		return true, reason, diff, cand
	}
	if infoErr != nil {
		d.nomadAPIErrors.WithLabelValues("info").Inc()
		slog.Warn("Failed to query job from Nomad", "job", jobID, "err", infoErr)
		return true, reason, nil, nil
	}

	// Unless the caller explicitly wants dead jobs included, treat a dead
	// job the same as a missing one.
	if !d.includeDeadJobs && nomadJob != nil && nomadJob.Status != nil && *nomadJob.Status == "dead" {
		slog.Debug("Job is dead in Nomad, treating as missing", "job", jobID)
		diff := &JobDiff{
			JobID:    jobID,
			HCLFile:  entry.file,
			DiffType: DiffTypeMissingFromNomad,
			Detail:   "job is defined in HCL but is in 'dead' state in Nomad",
		}
		// The dead job still exists in Nomad's state store, so the CAS
		// token is its current ModifyIndex, not 0.
		cand := &updateCandidate{
			jobID: jobID, hclFile: entry.file, job: entry.job,
			modifyIndex: jobModifyIndex(nomadJob), class: DiffClassOther, isCreation: true, globSel: entry.globSel,
		}
		return true, reason, diff, cand
	}

	// Job exists and is live — run a plan to detect config drift.
	// Nomad's deregister call sets Stop=true on the job record. Copy it
	// onto the HCL job so the plan does not report a Stop field diff.
	job := entry.job
	if nomadJob.Stop != nil && *nomadJob.Stop {
		stop := true
		job.Stop = &stop
	}
	plan, _, err := d.jobs.Plan(job, true, wq)
	if err != nil {
		d.nomadAPIErrors.WithLabelValues("plan").Inc()
		slog.Warn("Failed to plan job", "job", jobID, "err", err)
		return true, reason, nil, nil
	}

	if plan.Diff != nil && plan.Diff.Type != "" && plan.Diff.Type != "None" {
		// For dead jobs, Nomad may return Type="Edited" with only task-group
		// bookkeeping entries (Type="None" task groups). hasContentDiff filters
		// those out so we don't report spurious drift.
		isDead := nomadJob != nil && nomadJob.Status != nil && *nomadJob.Status == "dead"
		if isDead && !hasContentDiff(plan.Diff) {
			return true, reason, nil, nil
		}
		// Classify before redaction; classification reads only structure
		// (names and change types), but doing it first keeps the order
		// obviously safe.
		class := classifyDiff(plan.Diff, autoscaledGroups(entry.job), d.managedMetaPrefix)
		// Redact before the diff is stored so potentially sensitive values
		// never reach /diffs or any other consumer of the stored state.
		if d.redactSecrets {
			if n := RedactJobDiff(plan.Diff); n > 0 {
				d.redactedFields.Add(float64(n))
			}
		}
		diff := &JobDiff{
			JobID:    jobID,
			HCLFile:  entry.file,
			DiffType: DiffTypeModified,
			Detail:   fmt.Sprintf("Nomad plan shows diff type %q", plan.Diff.Type),
			PlanDiff: plan.Diff,
		}
		cand := &updateCandidate{
			jobID: jobID, hclFile: entry.file, job: entry.job,
			modifyIndex: jobModifyIndex(nomadJob), class: class, globSel: entry.globSel,
		}
		return true, reason, diff, cand
	}
	return true, reason, nil, nil
}

// commitResults stores the computed diffs and updates drift tracking and metrics.
// It is called at the end of each Check to atomically publish new state.
func (d *Differ) commitResults(diffs []JobDiff, selReasons map[string]SelectionReason, commit string, listMeta *nomadapi.QueryMeta, now time.Time) {
	currentKeys := make(map[string]struct{}, len(diffs))
	for _, diff := range diffs {
		currentKeys[driftKey(diff.JobID, string(diff.DiffType))] = struct{}{}
	}

	selectedJobs := make([]SelectedJob, 0, len(selReasons))
	for jobID, reason := range selReasons {
		selectedJobs = append(selectedJobs, SelectedJob{JobID: jobID, Reason: reason})
	}
	sort.Slice(selectedJobs, func(i, j int) bool { return selectedJobs[i].JobID < selectedJobs[j].JobID })

	d.mu.Lock()
	d.diffs = diffs
	d.selectedJobs = selectedJobs
	d.lastCheckTime = now
	d.lastCommit = commit
	if listMeta != nil {
		d.lastNomadIndex = listMeta.LastIndex
	}
	for k := range d.driftFirstSeen {
		if _, ok := currentKeys[k]; !ok {
			delete(d.driftFirstSeen, k)
		}
	}
	for k := range currentKeys {
		if _, ok := d.driftFirstSeen[k]; !ok {
			d.driftFirstSeen[k] = now
		}
	}
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
		d.lastCheck.Set(float64(time.Now().Unix()))
		return nil
	}

	slog.Info("Running diff check", "commit", commit, "hcl_files", len(hclFiles))
	d.diffChecks.Inc()

	metaSeen := make(map[string]metaState)
	hclEntries, parsedIDs := d.parseHCLCandidates(hclFiles, metaSeen)
	// hclJobSet tracks jobs that passed HCL-phase selection; used below to
	// detect jobs running in Nomad that have no corresponding HCL file.
	hclJobSet := make(map[string]struct{}, len(hclEntries))
	selReasons := make(map[string]SelectionReason)
	// metaByJob holds the HCL meta of each managed job that has an HCL file in
	// the repo, used by the rollback poll. Active rollback is deliberately
	// scoped to HCL-defined jobs: a job running in Nomad with no HCL is either
	// unmanaged or an orphan leaving management, not something nomad-botherer
	// drives applies for, so it is not a rollback candidate.
	metaByJob := make(map[string]map[string]string)
	var diffs []JobDiff
	var candidates []*updateCandidate

	for jobID, entry := range hclEntries {
		selected, reason, diff, cand := d.checkHCLCandidate(jobID, entry, q, wq)
		if !selected {
			continue
		}
		selReasons[jobID] = mergeSelectionReason(selReasons[jobID], reason)
		hclJobSet[jobID] = struct{}{}
		metaByJob[jobID] = entry.job.Meta
		if cand != nil {
			// Decide the disposition once; it is recorded on the diff (for the
			// API/console) and drives whether the update is enqueued below.
			cand.action = d.decideApplyAction(cand, commit, q)
		}
		if diff != nil {
			metaOnly := cand != nil && cand.class == DiffClassManagedMetaOnly
			if metaOnly {
				d.metaOnlyDiffs.WithLabelValues(jobID).Inc()
			}
			if cand != nil {
				diff.ApplyAction = cand.action
			}
			// A diff confined to our own meta keys is an expected,
			// non-disruptive difference. By default it is not counted as
			// drift (so it does not trigger alerts) — it is surfaced via its
			// own counter and the meta-change logs instead, and converges on
			// the next real update.
			if !metaOnly || d.countMetaOnlyChanges {
				diffs = append(diffs, *diff)
			}
		}
		if cand != nil {
			candidates = append(candidates, cand)
		}
	}

	// Find jobs in Nomad that have no corresponding HCL file.
	// Dead jobs are skipped unless --include-dead-jobs is set, since a dead
	// job without HCL is expected (it was stopped intentionally).
	// Only jobs that match the configured selection criteria are considered managed.
	for _, j := range allJobs {
		// Validate before any skip: a live job with a malformed opt-in key
		// is silently out of scope, which is the failure worth surfacing.
		if _, inHCL := hclJobSet[j.ID]; !inHCL {
			d.validateManagedMeta(j.ID, "nomad", j.Meta)
		}
		// Track the live side for every job, including managed ones: a
		// manual `nomad job run` that drops the keys is exactly the
		// meta-drift event worth noticing.
		recordMetaSeen(metaSeen, d, "nomad", j.ID, j.Meta)
		if !d.includeDeadJobs && j.Status == "dead" {
			continue
		}
		// Git is always the source of truth for our own keys: when the job
		// has an HCL file, that file alone already decided selection in the
		// HCL phase. A live key on a job whose HCL does not opt in never
		// overrides Git; it is only surfaced via meta validation and
		// change notices.
		if _, parsed := parsedIDs[j.ID]; parsed {
			if _, ok := hclJobSet[j.ID]; !ok {
				d.jobsSkippedBySel.WithLabelValues("nomad").Inc()
			}
			continue
		}
		// Meta is populated because the List call includes ?meta=true.
		selected, reason := d.jobSelectionReason(j.ID, j.Meta)
		if !selected {
			d.jobsSkippedBySel.WithLabelValues("nomad").Inc()
			continue
		}
		selReasons[j.ID] = mergeSelectionReason(selReasons[j.ID], reason)
		if _, ok := hclJobSet[j.ID]; !ok {
			diff := JobDiff{
				JobID:       j.ID,
				DiffType:    DiffTypeMissingFromHCL,
				Detail:      fmt.Sprintf("job is running in Nomad (status: %s) but has no HCL definition in the repo", j.Status),
				ApplyAction: ApplyActionObservationOnly,
			}
			// A job carrying our tag in its live meta with no HCL declaring it
			// is an orphan — removed from the repo (file deleted or renamed),
			// since tag-removal-with-file-present is excluded by the parsedIDs
			// skip above. Such a job is a deregister candidate. A glob-only
			// orphan (no tag) is never deregistered; it stays observation-only.
			if d.metaKeyPresent(j.Meta) {
				cand := &updateCandidate{
					jobID:     j.ID,
					operation: JobUpdateOperationDeregister,
					policy:    d.effectivePolicy(j.Meta),
				}
				cand.action = d.decideDeregisterAction(cand)
				diff.ApplyAction = cand.action
				candidates = append(candidates, cand)
			}
			diffs = append(diffs, diff)
		}
	}

	d.commitResults(diffs, selReasons, commit, listMeta, time.Now())
	d.logMetaChanges(metaSeen)
	liveJobs := make(map[string]string, len(allJobs))
	for _, j := range allJobs {
		liveJobs[j.ID] = j.Status
	}
	d.logScopeExits(hclJobSet, parsedIDs, liveJobs)

	var raftIndex uint64
	if listMeta != nil {
		raftIndex = listMeta.LastIndex
	}
	enqueued := 0
	for _, cand := range candidates {
		if cand.action == ApplyActionQueued || cand.action == ApplyActionDeregisterQueued {
			d.enqueueUpdate(cand, commit, raftIndex)
			enqueued++
		}
	}
	if enqueued > 0 {
		d.notifyApplier()
	}

	// Active rollback poll: for managed deployment-producing jobs that have
	// rollback enabled, revert a failed deployment to the last stable version
	// (unless the job uses auto_revert, where Nomad wins). Cheap and skipped
	// entirely when no managed job opts in.
	d.checkRollbacks(metaByJob, q, raftIndex)

	slog.Info("Diff check complete", "diffs", len(diffs), "updates_enqueued", enqueued, "commit", commit)
	return nil
}

// effectivePolicy resolves the update policy for a job: the HCL meta key
// <prefix>_update_policy wins (Git is intent); otherwise the configured
// default applies. An unrecognised meta value is treated as "none" — the
// conservative reading — and logged.
func (d *Differ) effectivePolicy(meta map[string]string) UpdatePolicy {
	if d.managedMetaPrefix != "" {
		if v, ok := meta[d.managedMetaPrefix+"_update_policy"]; ok {
			if ValidUpdatePolicy(v) {
				return UpdatePolicy(v)
			}
			// Already logged at ERROR by validateManagedMeta during parsing.
			return UpdatePolicyNone
		}
	}
	return d.defaultPolicy
}

// SetHistorySource wires the git-history accessor used to detect pre-existing
// drift. Called once at startup after the watcher exists.
func (d *Differ) SetHistorySource(h HistorySource) {
	d.history = h
}

// isPreExistingDrift reports whether a candidate's drift pre-dates the change,
// at the commit being evaluated, that brought it into scope to apply. Two such
// scope-widening changes are treated the same way (issue #69):
//
//   - Enablement: the managed opt-in tag (<prefix>_managed) was added at this
//     commit (absent in the parent version of the file). The whole job entered
//     management; its existing drift pre-dates the opt-in.
//   - Policy promotion: the job was managed at the parent, but its effective
//     update policy there would not have applied this diff's class, while the
//     policy at this commit does (e.g. image-only → full applying a memory
//     change that image-only had been deferring). The drift was live the whole
//     time, merely held back by the stricter policy.
//
// In both cases the conservative default is not to retroactively apply the
// accumulated drift: changing scope expresses intent about future
// reconciliation, not "deploy the backlog now". --apply-existing-drift opts in
// to applying it.
//
// Derived from git history, so it holds identically whether the change landed
// while the process was running or before it started. Glob-selected jobs are
// always in scope and have no opt-in moment, so they are never pre-existing.
// Creations have no live job to pre-date. When history is unavailable the check
// is skipped (not pre-existing) so reconciliation is not broken.
func (d *Differ) isPreExistingDrift(c *updateCandidate, commit string) bool {
	if c.isCreation || c.globSel || d.history == nil || d.managedKeyRe == nil || c.hclFile == "" {
		return false
	}
	parent, ok := d.history.FileAtParentOf(commit, c.hclFile)
	if !ok {
		// No parent version: the file was created at this commit (or it is the
		// root commit). The tag, policy, and spec were introduced together, so
		// nothing was deferred under an earlier scope — not retroactive.
		return false
	}
	if !d.managedKeyRe.MatchString(parent) {
		// The opt-in tag was added at this commit: the job entered management
		// here, so its drift pre-dates the opt-in.
		return true
	}
	// Managed at the parent too. A managed-meta-only diff is governed by the
	// meta-only gate, not this one.
	if c.class == DiffClassManagedMetaOnly {
		return false
	}
	// Pre-existing iff the parent's effective policy would not have applied this
	// diff's class but the current one does — a scope-widening policy change at
	// this commit.
	return !policyPermits(d.effectivePolicyFromText(parent), c.class) &&
		policyPermits(d.effectivePolicy(c.job.Meta), c.class)
}

// policyPermits reports whether an update policy would apply a diff of the given
// class. Mirrors the policy gate in decideApplyAction (creation, which is
// full-only, is handled separately and never reaches the pre-existing check).
func policyPermits(policy UpdatePolicy, class DiffClass) bool {
	switch policy {
	case UpdatePolicyFull:
		return true
	case UpdatePolicyImageOnly:
		return class == DiffClassImageOnly
	default: // none, or an unrecognised value treated as none
		return false
	}
}

// effectivePolicyFromText resolves a job's update policy from a raw HCL snapshot
// (e.g. a parent commit's version of the file), without re-parsing: the
// <prefix>_update_policy value wins, an unrecognised value is treated as none
// (matching effectivePolicy), and an absent key falls back to the default.
func (d *Differ) effectivePolicyFromText(hclText string) UpdatePolicy {
	if d.policyKeyRe != nil {
		if m := d.policyKeyRe.FindStringSubmatch(hclText); m != nil {
			if ValidUpdatePolicy(m[1]) {
				return UpdatePolicy(m[1])
			}
			return UpdatePolicyNone
		}
	}
	return d.defaultPolicy
}

// logScopeExits logs, once per transition, when a job leaves active GitOps
// management. managed is this cycle's HCL-managed set, parsedIDs the jobs
// present in the repo at all, and liveJobs maps job ID to Nomad status. A job
// that was managed last cycle and is not now either had its tag removed (still
// in the repo — already reported by the meta-change log) or was removed from
// the repo entirely (file deleted or job renamed). prevManaged is nil on the
// first cycle, so a restart logs nothing.
func (d *Differ) logScopeExits(managed, parsedIDs map[string]struct{}, liveJobs map[string]string) {
	d.metaMu.Lock()
	defer d.metaMu.Unlock()

	if d.prevManaged != nil {
		for jobID := range d.prevManaged {
			if _, still := managed[jobID]; still {
				continue
			}
			if _, inRepo := parsedIDs[jobID]; inRepo {
				// Tag removed but the job is still in the repo: the meta-change
				// log already carries the human message; just count it.
				d.jobsLeftManagement.WithLabelValues(jobID, "tag_removed").Inc()
				continue
			}
			d.jobsLeftManagement.WithLabelValues(jobID, "removed_from_repo").Inc()
			if status, running := liveJobs[jobID]; running {
				slog.Info("Job left GitOps management: removed from the repo (file deleted or job renamed); the running job is left untouched",
					"job", jobID, "nomad_status", status, "deregister_enabled", d.enableDeregister)
			} else {
				slog.Info("Job left GitOps management: removed from the repo and no longer present in Nomad",
					"job", jobID)
			}
		}
	}

	next := make(map[string]bool, len(managed))
	for jobID := range managed {
		next[jobID] = true
	}
	d.prevManaged = next
}

// decideDeregisterAction decides what to do with an orphaned managed job (a
// tagged live job removed from the repo). Gated, most-conservative first:
// deregistration must be enabled, the job's effective policy must be full, and
// it must have been orphaned for the grace period. Anything short of that
// leaves the job running (observation, policy-blocked, or grace-pending).
func (d *Differ) decideDeregisterAction(c *updateCandidate) ApplyAction {
	if !d.enableDeregister {
		return ApplyActionObservationOnly
	}
	if c.policy != UpdatePolicyFull {
		d.updatesBlockedByPolicy.WithLabelValues(c.jobID, string(c.policy)).Inc()
		return ApplyActionPolicyBlocked
	}
	if !d.orphanGraceElapsed(c.jobID) {
		return ApplyActionDeregisterGrace
	}
	return ApplyActionDeregisterQueued
}

// orphanGraceElapsed reports whether a job has been continuously orphaned
// (missing_from_hcl) for at least the configured grace period. It reads the
// first-seen time recorded by the previous cycle's commitResults; a job
// orphaned for the first time this cycle has no entry yet and is not eligible.
func (d *Differ) orphanGraceElapsed(jobID string) bool {
	d.mu.RLock()
	t, ok := d.driftFirstSeen[driftKey(jobID, string(DiffTypeMissingFromHCL))]
	d.mu.RUnlock()
	return ok && time.Since(t) >= d.deregisterGrace
}

// decideApplyAction determines a candidate's disposition and records the
// reason via metrics/logs. It does not enqueue anything; the caller enqueues
// when the action is ApplyActionQueued. The gates are ordered most-conservative
// first so the surfaced reason is the primary one.
func (d *Differ) decideApplyAction(c *updateCandidate, commit string, q *nomadapi.QueryOptions) ApplyAction {
	if c.class == DiffClassNone && !c.isCreation {
		// Everything in the diff is autoscaler-owned Count/Scaling churn;
		// Git has nothing to apply. The diff stays visible as an observation.
		return ApplyActionNoChange
	}

	if !c.isCreation && d.isPreExistingDrift(c, commit) && !d.applyExistingDrift {
		// The drift was already there when the change that brought it into scope
		// landed at the HEAD commit — the job's opt-in tag was added, or its
		// policy was widened to cover this diff. Conservative default: a scope
		// change does not retroactively mutate the job; only changes committed
		// after it apply. Enable with --apply-existing-drift.
		slog.Info("Pre-existing drift not applied on scope change (opt-in or policy widening); set --apply-existing-drift to apply", "job", c.jobID)
		d.updatesBlockedExistingDrift.WithLabelValues(c.jobID).Inc()
		return ApplyActionPreExisting
	}

	if c.class == DiffClassManagedMetaOnly && !c.isCreation && !d.applyMetaOnlyChanges {
		// A change confined to our own meta keys: leave the running job
		// alone. Re-registering just to push gitops_* keys is disruptive and
		// unnecessary — the HCL is already authoritative for them, and they
		// converge on the next real update (registering from HCL carries them
		// along, even under an image-only policy). Enable with
		// --apply-meta-only-changes.
		return ApplyActionMetaOnly
	}

	policy := d.effectivePolicy(c.job.Meta)
	switch policy {
	case UpdatePolicyNone:
		d.updatesBlockedByPolicy.WithLabelValues(c.jobID, string(policy)).Inc()
		return ApplyActionPolicyBlocked
	case UpdatePolicyImageOnly:
		// Initial registration is full-only by design: registering a job
		// for the first time is not an image-only change.
		if c.isCreation || c.class != DiffClassImageOnly {
			d.updatesBlockedByPolicy.WithLabelValues(c.jobID, string(policy)).Inc()
			return ApplyActionPolicyBlocked
		}
	}

	if c.isCreation && !d.enableJobCreation {
		slog.Info("Job creation blocked: --enable-job-creation is off", "job", c.jobID)
		d.updatesBlockedCreationDisabled.WithLabelValues(c.jobID).Inc()
		return ApplyActionCreationBlocked
	}

	// Flap-loop guard: hold a re-apply of a spec that a recent deployment
	// already failed (apply→fail→revert→re-apply). Only meaningful for an
	// existing job; a first registration has no prior failed version to match.
	// Released automatically when Git moves to a spec that has not failed.
	if !c.isCreation {
		if mode := d.effectiveFlapGuard(c.job.Meta); mode != "off" && d.flapGuardBlocks(c, mode, q) {
			slog.Info("Flap-guard: holding re-apply of a spec a recent deployment already failed; waiting for a fix in Git", "job", c.jobID)
			d.updatesBlockedKnownFailed.WithLabelValues(c.jobID).Inc()
			return ApplyActionKnownFailed
		}
	}

	return ApplyActionQueued
}

// enqueueUpdate places an approved candidate's update on the queue. A
// DEREGISTER candidate carries no parsed job (the job is removed by ID); a
// REGISTER candidate carries the HCL job to write.
func (d *Differ) enqueueUpdate(c *updateCandidate, commit string, raftIndex uint64) {
	op := c.operation
	if op == "" {
		op = JobUpdateOperationRegister
	}
	u := JobUpdate{
		UpdateID:       updateID(c.jobID, commit),
		JobID:          c.jobID,
		HCLFile:        c.hclFile,
		GitCommit:      commit,
		Operation:      op,
		Status:         JobUpdateStatusPending,
		NomadRaftIndex: raftIndex,
		DetectedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if op == JobUpdateOperationRegister {
		u.Policy = d.effectivePolicy(c.job.Meta)
		u.NomadJobModifyIndex = c.modifyIndex
		u.job = c.job
		u.preserveCounts = len(autoscaledGroups(c.job)) > 0
	} else {
		u.Policy = c.policy
	}
	superseded := d.updateQueue.Enqueue(u)
	if superseded > 0 {
		d.jobUpdatesTotal.WithLabelValues(string(op), string(JobUpdateStatusSuperseded)).Add(float64(superseded))
	}
	d.pendingUpdates.Set(float64(d.updateQueue.PendingCount()))
	slog.Info("Enqueued job update", "job", c.jobID, "update_id", u.UpdateID, "operation", op, "policy", u.Policy)
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
