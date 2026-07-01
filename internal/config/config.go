package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	// Git
	RepoURL              string
	Branch               string
	PollInterval         time.Duration
	HCLDir               string
	GitToken             string
	GitSSHKeyPath        string
	GitSSHKeyPass        string
	GitSSHKnownHostsFile string

	// Nomad
	NomadAddr      string
	NomadToken     string
	NomadNamespace string

	// NomadTokenFile is a path to a file containing a Nomad ACL token SecretID,
	// re-read periodically so a rotating token stays current. Use it for a real
	// SecretID written to a file (e.g. by a sidecar). Note: this must be a
	// 36-char ACL SecretID, not a workload-identity JWT — a raw WI JWT is
	// rejected by Nomad's Job.Plan RPC (see NomadLoginAuthMethod).
	NomadTokenFile string
	// NomadTokenPollInterval is how often the token file is re-read for changes.
	NomadTokenPollInterval time.Duration

	// NomadLoginAuthMethod, when set, enables Nomad workload-identity login: the
	// identity JWT (NomadLoginJWTFile) is exchanged for a real ACL token via
	// POST /v1/acl/login against this JWT auth method, and re-exchanged before
	// it expires. This is the working way to use workload identity — a raw WI
	// JWT authenticates read RPCs but is rejected by Job.Plan, which
	// nomad-botherer needs for every drift check (issue #74).
	NomadLoginAuthMethod string
	// NomadLoginJWTFile is the path to the workload-identity JWT to exchange.
	// Defaults to ${NOMAD_SECRETS_DIR}/nomad_token; point it at a named
	// identity's file (nomad_<name>.jwt) when the auth method's audience does
	// not match the default identity.
	NomadLoginJWTFile string

	// Server
	ListenAddr    string
	WebhookSecret string
	WebhookPath   string
	APIKey        string // PSK for /api/ endpoints; empty disables the API

	// Diff
	DiffInterval    time.Duration
	IncludeDeadJobs bool
	RedactSecrets   bool

	// Apply (GitOps mutation)
	DefaultUpdatePolicy string
	EnableJobCreation   bool
	ApplyInterval       time.Duration

	// Managed-meta-only changes: a diff confined to nomad-botherer's own
	// meta keys (e.g. gitops_managed). By default these neither trigger an
	// update nor count as drift; the keys converge opportunistically on the
	// next real update.
	ApplyMetaOnlyChanges bool
	CountMetaOnlyChanges bool

	// ApplyExistingDrift controls whether drift that already existed when a
	// change widened a job's scope is applied. Scope widens two ways, treated
	// the same: a job gains the managed meta tag (enablement), or its update
	// policy is widened to cover drift it was deferring (e.g. image-only → full).
	// Off by default: a scope change does not retroactively mutate the job; only
	// changes committed after it apply.
	ApplyExistingDrift bool

	// Deregistration of jobs removed from the repo (file deleted or job
	// renamed). Off by default; the one destructive write nomad-botherer can
	// make, so heavily gated.
	EnableDeregister bool
	DeregisterPurge  bool
	DeregisterGrace  time.Duration

	// FlapGuard controls how nomad-botherer avoids re-applying a job spec that
	// a recent Nomad job version already failed to deploy (the
	// apply→fail→revert→re-apply loop). One of: history (Approach A: compare
	// spec fingerprints against Nomad's in-cluster version history, ephemeral
	// and GC-bounded), tag (Approach B: additionally tag the failed version so
	// the block survives version GC), or off (disabled). Per-job overridable
	// via the <prefix>_flap_guard meta key. Only applies to deployment-producing
	// jobs (service jobs with an update stanza and health checks).
	FlapGuard string

	// AllowRollback enables active rollback: for managed deployment-producing
	// jobs whose update stanza does not set auto_revert, nomad-botherer reverts
	// the job to its last stable version when a deployment fails. Off by
	// default. Per-job overridable via the <prefix>_rollback meta key. Where a
	// job's update stanza sets auto_revert=true, Nomad's own rollback always
	// wins and nomad-botherer stands down.
	AllowRollback bool

	// Job selection. Git is always the source of truth for nomad-botherer's
	// own meta keys: when a job has an HCL file in the repo, that file alone
	// decides selection and policy. There is deliberately no flag to invert
	// this.
	JobSelectorGlob   string
	ManagedMetaPrefix string

	// Staleness
	MaxGitStaleness   time.Duration
	MaxNomadStaleness time.Duration

	// Logging
	LogLevel string
}

// Load parses flags from os.Args and falls back to environment variables.
func Load() (*Config, error) {
	return LoadFromArgs(flag.CommandLine, os.Args[1:])
}

// LoadFromArgs registers flags on fs and parses args.
// Tests pass a fresh flag.NewFlagSet to avoid touching flag.CommandLine.
func LoadFromArgs(fs *flag.FlagSet, args []string) (*Config, error) {
	c := &Config{}

	fs.StringVar(&c.RepoURL, "repo-url", envOrDefault("GIT_REPO_URL", ""), "Remote git repo URL (required)")
	fs.StringVar(&c.Branch, "branch", envOrDefault("GIT_BRANCH", "main"), "Branch to watch")
	fs.DurationVar(&c.PollInterval, "poll-interval", envDurationOrDefault("POLL_INTERVAL", 5*time.Minute), "Git poll interval (e.g. 5m, 30s)")
	fs.StringVar(&c.HCLDir, "hcl-dir", envOrDefault("HCL_DIR", ""), "Directory within repo containing HCL job files (empty = repo root)")
	fs.StringVar(&c.GitToken, "git-token", envOrDefault("GIT_TOKEN", ""), "Git HTTP token for private repos (e.g. GitHub PAT)")
	fs.StringVar(&c.GitSSHKeyPath, "git-ssh-key", envOrDefault("GIT_SSH_KEY", ""), "Path to SSH private key for git auth")
	fs.StringVar(&c.GitSSHKeyPass, "git-ssh-key-password", envOrDefault("GIT_SSH_KEY_PASSWORD", ""), "SSH private key passphrase")
	fs.StringVar(&c.GitSSHKnownHostsFile, "git-ssh-known-hosts", envOrDefault("GIT_SSH_KNOWN_HOSTS", ""), "Path to known_hosts file for SSH host key verification (defaults to ~/.ssh/known_hosts; set to empty string to use system defaults)")

	fs.StringVar(&c.NomadAddr, "nomad-addr", envOrDefault("NOMAD_ADDR", "http://127.0.0.1:4646"), "Nomad API address")
	fs.StringVar(&c.NomadToken, "nomad-token", envOrDefault("NOMAD_TOKEN", ""), "Nomad ACL token (static SecretID). Intended for manual running and testing; for a deployment under Nomad, use workload identity (see --nomad-login-auth-method).")
	fs.StringVar(&c.NomadTokenFile, "nomad-token-file", envOrDefault("NOMAD_TOKEN_FILE", ""), "Path to a file containing a Nomad ACL token SecretID, re-read periodically so a rotating token stays current. Must be a 36-char SecretID, not a workload-identity JWT. Takes precedence over --nomad-token.")
	fs.DurationVar(&c.NomadTokenPollInterval, "nomad-token-poll-interval", envDurationOrDefault("NOMAD_TOKEN_POLL_INTERVAL", 30*time.Second), "How often to re-read the Nomad token file (--nomad-token-file) for a rotated token.")
	fs.StringVar(&c.NomadLoginAuthMethod, "nomad-login-auth-method", envOrDefault("NOMAD_LOGIN_AUTH_METHOD", ""), "Enable Nomad workload-identity login: name of the JWT ACL auth method to exchange the identity JWT (--nomad-login-jwt-file) for an ACL token via /v1/acl/login, re-exchanged before it expires. This is the working way to use workload identity — a raw WI JWT is rejected by Nomad's Job.Plan RPC.")
	fs.StringVar(&c.NomadLoginJWTFile, "nomad-login-jwt-file", envOrDefault("NOMAD_LOGIN_JWT_FILE", ""), "Path to the workload-identity JWT to exchange (login mode). Defaults to ${NOMAD_SECRETS_DIR}/nomad_token; point it at a named identity's file (nomad_<name>.jwt) when the auth method audience does not match the default identity.")
	fs.StringVar(&c.NomadNamespace, "nomad-namespace", envOrDefault("NOMAD_NAMESPACE", "default"), "Nomad namespace")

	fs.StringVar(&c.ListenAddr, "listen-addr", envOrDefault("LISTEN_ADDR", ":8080"), "HTTP listen address")
	fs.StringVar(&c.WebhookSecret, "webhook-secret", envOrDefault("WEBHOOK_SECRET", ""), "GitHub webhook HMAC secret")
	fs.StringVar(&c.WebhookPath, "webhook-path", envOrDefault("WEBHOOK_PATH", "/webhook"), "HTTP path for webhook endpoint")
	fs.StringVar(&c.APIKey, "api-key", envOrDefault("API_KEY", ""), "Pre-shared key for /api/ endpoints (Bearer token). Empty disables the JSON API.")

	fs.DurationVar(&c.DiffInterval, "diff-interval", envDurationOrDefault("DIFF_INTERVAL", time.Minute), "How often to run a diff check regardless of git changes")
	fs.BoolVar(&c.IncludeDeadJobs, "include-dead-jobs", envBoolOrDefault("INCLUDE_DEAD_JOBS", false), "Treat dead Nomad jobs like running ones (by default dead jobs are treated as missing)")
	fs.BoolVar(&c.RedactSecrets, "redact-secrets", envBoolOrDefault("REDACT_SECRETS", true), "Redact potentially sensitive plan-diff values (env vars, template bodies, fields with secret-like names) before storing and rendering diffs")
	fs.StringVar(&c.DefaultUpdatePolicy, "default-update-policy", envOrDefault("DEFAULT_UPDATE_POLICY", "none"), "Update policy for managed jobs without an explicit <prefix>_update_policy meta key: none (detect only), image-only (apply drift confined to Docker image fields), full (apply any drift)")
	fs.BoolVar(&c.EnableJobCreation, "enable-job-creation", envBoolOrDefault("ENABLE_JOB_CREATION", false), "Allow registering jobs that exist in Git but not in Nomad (first-time registration). Off by default; requires an effective update policy of full for the job.")
	fs.DurationVar(&c.ApplyInterval, "apply-interval", envDurationOrDefault("APPLY_INTERVAL", 10*time.Second), "Fallback cadence of the apply loop; enqueued updates are also applied immediately")
	fs.BoolVar(&c.ApplyMetaOnlyChanges, "apply-meta-only-changes", envBoolOrDefault("APPLY_META_ONLY_CHANGES", false), "Apply a diff whose only change is to nomad-botherer's own meta keys (e.g. gitops_managed). Off by default: re-registering a running job just to push these keys is disruptive and unnecessary (the HCL is already authoritative), so they ride along the next real update instead.")
	fs.BoolVar(&c.CountMetaOnlyChanges, "count-meta-only-changes", envBoolOrDefault("COUNT_META_ONLY_CHANGES", false), "Count a managed-meta-only diff as drift (surface it on /diffs, /healthz, and the drift metrics). Off by default so these expected differences do not trigger drift alerts.")
	fs.BoolVar(&c.ApplyExistingDrift, "apply-existing-drift", envBoolOrDefault("APPLY_EXISTING_DRIFT", false), "When a change widens a job's scope, apply drift that already existed at that moment. Scope widens two ways, treated the same: a job gains the managed meta tag (enablement), or its update policy is widened to cover drift it was deferring (e.g. image-only → full applying a non-image change committed earlier). Off by default (conservative): a scope change does not retroactively mutate the job; only changes committed after it apply. Drift reconciles normally when scope is unchanged.")
	fs.BoolVar(&c.EnableDeregister, "enable-deregister", envBoolOrDefault("ENABLE_DEREGISTER", false), "Deregister jobs that were removed from the repo entirely (HCL file deleted or job renamed) while still running in Nomad. Off by default. Only ever acts on a job carrying gitops_managed=true in its live meta whose effective update policy is full, and only after it has been continuously orphaned for --deregister-grace. Removing only the gitops_managed tag (with the job still in the repo) never deregisters — it just stops management.")
	fs.BoolVar(&c.DeregisterPurge, "deregister-purge", envBoolOrDefault("DEREGISTER_PURGE", false), "When deregistering, purge the job from Nomad's state immediately instead of a graceful stop (which leaves it queryable and garbage-collected later). Off by default.")
	fs.DurationVar(&c.DeregisterGrace, "deregister-grace", envDurationOrDefault("DEREGISTER_GRACE", 5*time.Minute), "How long a job must be continuously orphaned (running in Nomad, removed from the repo) before it is deregistered. Absorbs transient renames and mid-edit commits.")
	fs.StringVar(&c.FlapGuard, "flap-guard", envOrDefault("FLAP_GUARD", "history"), "How to avoid re-applying a spec a recent Nomad job version already failed to deploy (the apply/fail/revert/re-apply loop): history (compare spec fingerprints against Nomad's version history; ephemeral, lost when Nomad GCs old versions), tag (additionally tag the failed version so the block survives GC), or off (disabled). Per-job overridable via the <prefix>_flap_guard meta key. Only applies to deployment-producing jobs.")
	fs.BoolVar(&c.AllowRollback, "allow-rollback", envBoolOrDefault("ALLOW_ROLLBACK", false), "Enable active rollback: for managed deployment-producing jobs whose update stanza does not set auto_revert, revert to the last stable version when a deployment fails. Off by default. Per-job overridable via the <prefix>_rollback meta key. Where the job's update stanza sets auto_revert=true, Nomad's own rollback wins and nomad-botherer stands down.")
	fs.StringVar(&c.JobSelectorGlob, "job-selector-glob", envOrDefault("JOB_SELECTOR_GLOB", ""), "Glob pattern selecting jobs by name (e.g. 'myprefix-*', '*' for all). Jobs matching either this or --managed-meta-prefix are watched. Empty means no glob selection.")
	fs.StringVar(&c.ManagedMetaPrefix, "managed-meta-prefix", envOrDefault("MANAGED_META_PREFIX", "gitops"), "Prefix for job meta keys used by nomad-botherer (e.g. 'gitops' means 'gitops_managed = true' in a job's HCL opts it in). Git is always the source of truth for these keys: when a job has an HCL file, the live job's keys are ignored for selection. Empty disables meta-based selection.")
	fs.DurationVar(&c.MaxGitStaleness, "max-git-staleness", envDurationOrDefault("MAX_GIT_STALENESS", 0), "Maximum time since last successful git fetch before forcing a refresh (0 disables)")
	fs.DurationVar(&c.MaxNomadStaleness, "max-nomad-staleness", envDurationOrDefault("MAX_NOMAD_STALENESS", 0), "Maximum time since last successful Nomad diff check before forcing a refresh (0 disables)")

	fs.StringVar(&c.LogLevel, "log-level", envOrDefault("LOG_LEVEL", "info"), "Log level: debug, info, warn, error")

	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("parsing flags: %w", err)
	}

	if c.RepoURL == "" {
		return nil, fmt.Errorf("--repo-url / GIT_REPO_URL is required")
	}

	// A git token over plain HTTP is sent in cleartext to the remote.
	if c.GitToken != "" && strings.HasPrefix(strings.ToLower(c.RepoURL), "http://") {
		return nil, fmt.Errorf("--git-token / GIT_TOKEN cannot be used with a plain http:// repo URL: the token would be sent in cleartext; use https:// or SSH instead")
	}

	switch c.DefaultUpdatePolicy {
	case "none", "image-only", "full":
	default:
		return nil, fmt.Errorf("--default-update-policy / DEFAULT_UPDATE_POLICY must be one of none, image-only, full; got %q", c.DefaultUpdatePolicy)
	}

	switch c.FlapGuard {
	case "history", "tag", "off":
	default:
		return nil, fmt.Errorf("--flap-guard / FLAP_GUARD must be one of history, tag, off; got %q", c.FlapGuard)
	}

	// Tag mode builds failed-version tag names from the managed-meta prefix
	// (<prefix>-failed-<fingerprint>) and recognises them by that prefix. With
	// an empty prefix the tag name would start with "-failed-" and could never
	// be recognised again, so durable blocking would silently not work.
	if c.FlapGuard == "tag" && c.ManagedMetaPrefix == "" {
		return nil, fmt.Errorf("--flap-guard=tag requires --managed-meta-prefix / MANAGED_META_PREFIX to be non-empty: failed-version tag names are derived from the prefix")
	}

	return c, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDurationOrDefault(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return def
}

func envBoolOrDefault(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(v) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	return def
}
