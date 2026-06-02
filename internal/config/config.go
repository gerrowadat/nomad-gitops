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
	RepoURL       string
	Branch        string
	PollInterval  time.Duration
	HCLDir        string
	GitToken             string
	GitSSHKeyPath        string
	GitSSHKeyPass        string
	GitSSHKnownHostsFile string

	// Nomad
	NomadAddr      string
	NomadToken     string
	NomadNamespace string

	// Server
	ListenAddr    string
	WebhookSecret string
	WebhookPath   string

	// gRPC
	GRPCListenAddr string
	GRPCAPIKey     string

	// Diff
	DiffInterval    time.Duration
	IncludeDeadJobs bool

	// Job selection
	JobSelectorGlob          string
	ManagedMetaPrefix        string
	ManagedMetaHCLCanonical  bool

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
	fs.StringVar(&c.NomadToken, "nomad-token", envOrDefault("NOMAD_TOKEN", ""), "Nomad ACL token")
	fs.StringVar(&c.NomadNamespace, "nomad-namespace", envOrDefault("NOMAD_NAMESPACE", "default"), "Nomad namespace")

	fs.StringVar(&c.ListenAddr, "listen-addr", envOrDefault("LISTEN_ADDR", ":8080"), "HTTP listen address")
	fs.StringVar(&c.WebhookSecret, "webhook-secret", envOrDefault("WEBHOOK_SECRET", ""), "GitHub webhook HMAC secret")
	fs.StringVar(&c.WebhookPath, "webhook-path", envOrDefault("WEBHOOK_PATH", "/webhook"), "HTTP path for webhook endpoint")

	fs.StringVar(&c.GRPCListenAddr, "grpc-listen-addr", envOrDefault("GRPC_LISTEN_ADDR", ""), "gRPC listen address (e.g. :9090). Empty (the default) disables the gRPC server. Requires --grpc-api-key when set.")
	fs.StringVar(&c.GRPCAPIKey, "grpc-api-key", envOrDefault("GRPC_API_KEY", ""), "Pre-shared API key for gRPC authentication (required when --grpc-listen-addr is set)")

	fs.DurationVar(&c.DiffInterval, "diff-interval", envDurationOrDefault("DIFF_INTERVAL", time.Minute), "How often to run a diff check regardless of git changes")
	fs.BoolVar(&c.IncludeDeadJobs, "include-dead-jobs", envBoolOrDefault("INCLUDE_DEAD_JOBS", false), "Treat dead Nomad jobs like running ones (by default dead jobs are treated as missing)")
	fs.StringVar(&c.JobSelectorGlob, "job-selector-glob", envOrDefault("JOB_SELECTOR_GLOB", ""), "Glob pattern selecting jobs by name (e.g. 'myprefix-*', '*' for all). Jobs matching either this or --managed-meta-prefix are watched. Empty means no glob selection.")
	fs.StringVar(&c.ManagedMetaPrefix, "managed-meta-prefix", envOrDefault("MANAGED_META_PREFIX", "gitops"), "Prefix for job meta keys used by nomad-botherer (e.g. 'gitops' means 'gitops_managed = true' opts a job in). Empty disables meta-based selection.")
	fs.BoolVar(&c.ManagedMetaHCLCanonical, "managed-meta-hcl-canonical", envBoolOrDefault("MANAGED_META_HCL_CANONICAL", false), "Use the HCL file as the source of truth for managed-meta-prefix selection. By default the live Nomad job's meta is checked; enable this to select jobs based on the meta key in HCL even if the running job does not carry it.")
	fs.DurationVar(&c.MaxGitStaleness, "max-git-staleness", envDurationOrDefault("MAX_GIT_STALENESS", 0), "Maximum time since last successful git fetch before forcing a refresh (0 disables)")
	fs.DurationVar(&c.MaxNomadStaleness, "max-nomad-staleness", envDurationOrDefault("MAX_NOMAD_STALENESS", 0), "Maximum time since last successful Nomad diff check before forcing a refresh (0 disables)")

	fs.StringVar(&c.LogLevel, "log-level", envOrDefault("LOG_LEVEL", "info"), "Log level: debug, info, warn, error")

	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("parsing flags: %w", err)
	}

	if c.RepoURL == "" {
		return nil, fmt.Errorf("--repo-url / GIT_REPO_URL is required")
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
