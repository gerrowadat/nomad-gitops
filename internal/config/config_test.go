package config

import (
	"flag"
	"os"
	"testing"
	"time"
)

func newFS() *flag.FlagSet {
	return flag.NewFlagSet("test", flag.ContinueOnError)
}

// ── envOrDefault / envDurationOrDefault ──────────────────────────────────────

func TestEnvOrDefault_Missing(t *testing.T) {
	const key = "TEST_NOMAD_BOTHERER_FOO"
	os.Unsetenv(key)
	if got := envOrDefault(key, "default"); got != "default" {
		t.Errorf("want default, got %q", got)
	}
}

func TestEnvOrDefault_Set(t *testing.T) {
	const key = "TEST_NOMAD_BOTHERER_FOO"
	os.Setenv(key, "fromenv")
	t.Cleanup(func() { os.Unsetenv(key) })
	if got := envOrDefault(key, "default"); got != "fromenv" {
		t.Errorf("want fromenv, got %q", got)
	}
}

func TestEnvDurationOrDefault_Missing(t *testing.T) {
	const key = "TEST_NOMAD_BOTHERER_DUR"
	os.Unsetenv(key)
	if got := envDurationOrDefault(key, time.Minute); got != time.Minute {
		t.Errorf("want 1m, got %v", got)
	}
}

func TestEnvDurationOrDefault_Valid(t *testing.T) {
	const key = "TEST_NOMAD_BOTHERER_DUR"
	os.Setenv(key, "30s")
	t.Cleanup(func() { os.Unsetenv(key) })
	if got := envDurationOrDefault(key, time.Minute); got != 30*time.Second {
		t.Errorf("want 30s, got %v", got)
	}
}

func TestEnvDurationOrDefault_Invalid(t *testing.T) {
	const key = "TEST_NOMAD_BOTHERER_DUR"
	os.Setenv(key, "not-a-duration")
	t.Cleanup(func() { os.Unsetenv(key) })
	if got := envDurationOrDefault(key, time.Minute); got != time.Minute {
		t.Errorf("invalid duration should fall back to default, got %v", got)
	}
}

// ── LoadFromArgs ─────────────────────────────────────────────────────────────

func TestLoadFromArgs_RequiresRepoURL(t *testing.T) {
	os.Unsetenv("GIT_REPO_URL")
	_, err := LoadFromArgs(newFS(), []string{})
	if err == nil {
		t.Error("expected error when repo URL is not set")
	}
}

func TestLoadFromArgs_FlagSetsRepoURL(t *testing.T) {
	os.Unsetenv("GIT_REPO_URL")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/repo.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RepoURL != "https://example.com/repo.git" {
		t.Errorf("unexpected RepoURL: %q", cfg.RepoURL)
	}
}

func TestLoadFromArgs_EnvSetsRepoURL(t *testing.T) {
	os.Setenv("GIT_REPO_URL", "https://env.example.com/repo.git")
	t.Cleanup(func() { os.Unsetenv("GIT_REPO_URL") })

	cfg, err := LoadFromArgs(newFS(), []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RepoURL != "https://env.example.com/repo.git" {
		t.Errorf("unexpected RepoURL: %q", cfg.RepoURL)
	}
}

func TestLoadFromArgs_FlagOverridesEnv(t *testing.T) {
	os.Setenv("GIT_REPO_URL", "https://env.example.com/repo.git")
	t.Cleanup(func() { os.Unsetenv("GIT_REPO_URL") })

	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://flag.example.com/repo.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// flag takes priority: flags are registered with env default, then the flag
	// value overwrites it when explicitly passed.
	if cfg.RepoURL != "https://flag.example.com/repo.git" {
		t.Errorf("flag value should win over env default, got %q", cfg.RepoURL)
	}
}

func TestLoadFromArgs_Defaults(t *testing.T) {
	// Clear any env vars that could affect defaults.
	for _, k := range []string{
		"GIT_REPO_URL", "GIT_BRANCH", "NOMAD_ADDR", "NOMAD_NAMESPACE",
		"LISTEN_ADDR", "WEBHOOK_PATH", "LOG_LEVEL", "POLL_INTERVAL", "DIFF_INTERVAL",
		"JOB_SELECTOR_GLOB", "MANAGED_META_PREFIX",
	} {
		os.Unsetenv(k)
	}

	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := []struct {
		name string
		got  string
		want string
	}{
		{"Branch", cfg.Branch, "main"},
		{"NomadAddr", cfg.NomadAddr, "http://127.0.0.1:4646"},
		{"NomadNamespace", cfg.NomadNamespace, "default"},
		{"ListenAddr", cfg.ListenAddr, ":8080"},
		{"WebhookPath", cfg.WebhookPath, "/webhook"},
		{"LogLevel", cfg.LogLevel, "info"},
		{"JobSelectorGlob", cfg.JobSelectorGlob, ""},
		{"ManagedMetaPrefix", cfg.ManagedMetaPrefix, "gitops"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: want %q, got %q", c.name, c.want, c.got)
		}
	}

	if cfg.PollInterval != 5*time.Minute {
		t.Errorf("PollInterval: want 5m, got %v", cfg.PollInterval)
	}
	if cfg.DiffInterval != time.Minute {
		t.Errorf("DiffInterval: want 1m, got %v", cfg.DiffInterval)
	}
}

func TestLoadFromArgs_BranchFlag(t *testing.T) {
	os.Unsetenv("GIT_BRANCH")
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--branch", "develop",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Branch != "develop" {
		t.Errorf("want develop, got %q", cfg.Branch)
	}
}

func TestEnvBoolOrDefault_Missing(t *testing.T) {
	const key = "TEST_NOMAD_BOTHERER_BOOL"
	os.Unsetenv(key)
	if got := envBoolOrDefault(key, true); got != true {
		t.Error("missing env should return default")
	}
}

func TestEnvBoolOrDefault_True(t *testing.T) {
	const key = "TEST_NOMAD_BOTHERER_BOOL"
	for _, v := range []string{"true", "1", "yes", "TRUE", "YES"} {
		os.Setenv(key, v)
		t.Cleanup(func() { os.Unsetenv(key) })
		if !envBoolOrDefault(key, false) {
			t.Errorf("value %q should be truthy", v)
		}
	}
}

func TestEnvBoolOrDefault_False(t *testing.T) {
	const key = "TEST_NOMAD_BOTHERER_BOOL"
	for _, v := range []string{"false", "0", "no", "FALSE"} {
		os.Setenv(key, v)
		t.Cleanup(func() { os.Unsetenv(key) })
		if envBoolOrDefault(key, true) {
			t.Errorf("value %q should be falsy", v)
		}
	}
}

func TestLoadFromArgs_IncludeDeadJobsDefault(t *testing.T) {
	os.Unsetenv("INCLUDE_DEAD_JOBS")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.IncludeDeadJobs {
		t.Error("IncludeDeadJobs should default to false")
	}
}

func TestLoadFromArgs_IncludeDeadJobsFlag(t *testing.T) {
	os.Unsetenv("INCLUDE_DEAD_JOBS")
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--include-dead-jobs",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.IncludeDeadJobs {
		t.Error("IncludeDeadJobs should be true when flag is set")
	}
}

func TestLoadFromArgs_IncludeDeadJobsEnv(t *testing.T) {
	os.Setenv("INCLUDE_DEAD_JOBS", "true")
	t.Cleanup(func() { os.Unsetenv("INCLUDE_DEAD_JOBS") })
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.IncludeDeadJobs {
		t.Error("IncludeDeadJobs should be true when env var is set")
	}
}

func TestLoadFromArgs_PollIntervalEnv(t *testing.T) {
	os.Setenv("POLL_INTERVAL", "10s")
	t.Cleanup(func() { os.Unsetenv("POLL_INTERVAL") })

	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PollInterval != 10*time.Second {
		t.Errorf("want 10s, got %v", cfg.PollInterval)
	}
}

func TestLoadFromArgs_GRPCDefaults(t *testing.T) {
	for _, k := range []string{"GRPC_LISTEN_ADDR", "GRPC_API_KEY"} {
		os.Unsetenv(k)
	}
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GRPCListenAddr != "" {
		t.Errorf("GRPCListenAddr: want empty (disabled by default), got %q", cfg.GRPCListenAddr)
	}
	if cfg.GRPCAPIKey != "" {
		t.Errorf("GRPCAPIKey: want empty, got %q", cfg.GRPCAPIKey)
	}
}

func TestLoadFromArgs_GRPCFlags(t *testing.T) {
	for _, k := range []string{"GRPC_LISTEN_ADDR", "GRPC_API_KEY"} {
		os.Unsetenv(k)
	}
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--grpc-listen-addr", ":19090",
		"--grpc-api-key", "mysecretkey",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GRPCListenAddr != ":19090" {
		t.Errorf("GRPCListenAddr: want :19090, got %q", cfg.GRPCListenAddr)
	}
	if cfg.GRPCAPIKey != "mysecretkey" {
		t.Errorf("GRPCAPIKey: want mysecretkey, got %q", cfg.GRPCAPIKey)
	}
}

func TestLoadFromArgs_GRPCEnvVars(t *testing.T) {
	os.Setenv("GRPC_LISTEN_ADDR", ":29090")
	os.Setenv("GRPC_API_KEY", "envkey")
	t.Cleanup(func() {
		os.Unsetenv("GRPC_LISTEN_ADDR")
		os.Unsetenv("GRPC_API_KEY")
	})
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GRPCListenAddr != ":29090" {
		t.Errorf("GRPCListenAddr: want :29090, got %q", cfg.GRPCListenAddr)
	}
	if cfg.GRPCAPIKey != "envkey" {
		t.Errorf("GRPCAPIKey: want envkey, got %q", cfg.GRPCAPIKey)
	}
}

func TestLoadFromArgs_MaxGitStalenessDefault(t *testing.T) {
	os.Unsetenv("MAX_GIT_STALENESS")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxGitStaleness != 0 {
		t.Errorf("MaxGitStaleness: want 0 (disabled), got %v", cfg.MaxGitStaleness)
	}
}

func TestLoadFromArgs_MaxGitStalenessFlag(t *testing.T) {
	os.Unsetenv("MAX_GIT_STALENESS")
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--max-git-staleness", "30m",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxGitStaleness != 30*time.Minute {
		t.Errorf("MaxGitStaleness: want 30m, got %v", cfg.MaxGitStaleness)
	}
}

func TestLoadFromArgs_MaxGitStalenessEnv(t *testing.T) {
	os.Setenv("MAX_GIT_STALENESS", "15m")
	t.Cleanup(func() { os.Unsetenv("MAX_GIT_STALENESS") })
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxGitStaleness != 15*time.Minute {
		t.Errorf("MaxGitStaleness: want 15m, got %v", cfg.MaxGitStaleness)
	}
}

func TestLoadFromArgs_MaxNomadStalenessDefault(t *testing.T) {
	os.Unsetenv("MAX_NOMAD_STALENESS")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxNomadStaleness != 0 {
		t.Errorf("MaxNomadStaleness: want 0 (disabled), got %v", cfg.MaxNomadStaleness)
	}
}

func TestLoadFromArgs_MaxNomadStalenessFlag(t *testing.T) {
	os.Unsetenv("MAX_NOMAD_STALENESS")
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--max-nomad-staleness", "10m",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxNomadStaleness != 10*time.Minute {
		t.Errorf("MaxNomadStaleness: want 10m, got %v", cfg.MaxNomadStaleness)
	}
}

func TestLoadFromArgs_MaxNomadStalenessEnv(t *testing.T) {
	os.Setenv("MAX_NOMAD_STALENESS", "5m")
	t.Cleanup(func() { os.Unsetenv("MAX_NOMAD_STALENESS") })
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxNomadStaleness != 5*time.Minute {
		t.Errorf("MaxNomadStaleness: want 5m, got %v", cfg.MaxNomadStaleness)
	}
}

func TestLoadFromArgs_StalenessIndependent(t *testing.T) {
	os.Unsetenv("MAX_GIT_STALENESS")
	os.Unsetenv("MAX_NOMAD_STALENESS")
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--max-git-staleness", "1h",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxGitStaleness != time.Hour {
		t.Errorf("MaxGitStaleness: want 1h, got %v", cfg.MaxGitStaleness)
	}
	if cfg.MaxNomadStaleness != 0 {
		t.Errorf("MaxNomadStaleness: want 0 (disabled), got %v", cfg.MaxNomadStaleness)
	}
}

func TestLoadFromArgs_GRPCDisabled(t *testing.T) {
	os.Unsetenv("GRPC_LISTEN_ADDR")
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--grpc-listen-addr", "",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GRPCListenAddr != "" {
		t.Errorf("GRPCListenAddr: want empty (disabled), got %q", cfg.GRPCListenAddr)
	}
}

func TestLoadFromArgs_ManagedMetaPrefixDefault(t *testing.T) {
	os.Unsetenv("MANAGED_META_PREFIX")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ManagedMetaPrefix != "gitops" {
		t.Errorf("ManagedMetaPrefix: want gitops, got %q", cfg.ManagedMetaPrefix)
	}
}

func TestLoadFromArgs_ManagedMetaPrefixFlag(t *testing.T) {
	os.Unsetenv("MANAGED_META_PREFIX")
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--managed-meta-prefix", "myorg",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ManagedMetaPrefix != "myorg" {
		t.Errorf("ManagedMetaPrefix: want myorg, got %q", cfg.ManagedMetaPrefix)
	}
}

func TestLoadFromArgs_ManagedMetaPrefixEnv(t *testing.T) {
	os.Setenv("MANAGED_META_PREFIX", "acme")
	t.Cleanup(func() { os.Unsetenv("MANAGED_META_PREFIX") })
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ManagedMetaPrefix != "acme" {
		t.Errorf("ManagedMetaPrefix: want acme, got %q", cfg.ManagedMetaPrefix)
	}
}

func TestLoadFromArgs_ManagedMetaPrefixEmpty(t *testing.T) {
	os.Unsetenv("MANAGED_META_PREFIX")
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--managed-meta-prefix", "",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ManagedMetaPrefix != "" {
		t.Errorf("ManagedMetaPrefix: want empty (disabled), got %q", cfg.ManagedMetaPrefix)
	}
}

func TestLoadFromArgs_JobSelectorGlobDefault(t *testing.T) {
	os.Unsetenv("JOB_SELECTOR_GLOB")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.JobSelectorGlob != "" {
		t.Errorf("JobSelectorGlob: want empty (no glob), got %q", cfg.JobSelectorGlob)
	}
}

func TestLoadFromArgs_JobSelectorGlobFlag(t *testing.T) {
	os.Unsetenv("JOB_SELECTOR_GLOB")
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--job-selector-glob", "prod-*",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.JobSelectorGlob != "prod-*" {
		t.Errorf("JobSelectorGlob: want prod-*, got %q", cfg.JobSelectorGlob)
	}
}

func TestLoadFromArgs_JobSelectorGlobEnv(t *testing.T) {
	os.Setenv("JOB_SELECTOR_GLOB", "myapp-*")
	t.Cleanup(func() { os.Unsetenv("JOB_SELECTOR_GLOB") })
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.JobSelectorGlob != "myapp-*" {
		t.Errorf("JobSelectorGlob: want myapp-*, got %q", cfg.JobSelectorGlob)
	}
}

func TestLoadFromArgs_ManagedMetaHCLCanonicalDefault(t *testing.T) {
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ManagedMetaHCLCanonical {
		t.Error("ManagedMetaHCLCanonical: want false (Nomad-canonical by default), got true")
	}
}

func TestLoadFromArgs_ManagedMetaHCLCanonicalFlag(t *testing.T) {
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--managed-meta-hcl-canonical",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.ManagedMetaHCLCanonical {
		t.Error("ManagedMetaHCLCanonical: want true after --managed-meta-hcl-canonical flag, got false")
	}
}

func TestLoadFromArgs_ManagedMetaHCLCanonicalEnv(t *testing.T) {
	os.Setenv("MANAGED_META_HCL_CANONICAL", "true")
	t.Cleanup(func() { os.Unsetenv("MANAGED_META_HCL_CANONICAL") })
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.ManagedMetaHCLCanonical {
		t.Error("ManagedMetaHCLCanonical: want true from env var, got false")
	}
}
