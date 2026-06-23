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
		"NOMAD_TOKEN", "NOMAD_TOKEN_FILE", "NOMAD_TOKEN_POLL_INTERVAL",
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
	if cfg.NomadToken != "" {
		t.Errorf("NomadToken default: want empty, got %q", cfg.NomadToken)
	}
	if cfg.NomadTokenFile != "" {
		t.Errorf("NomadTokenFile default: want empty, got %q", cfg.NomadTokenFile)
	}
	if cfg.NomadTokenPollInterval != 30*time.Second {
		t.Errorf("NomadTokenPollInterval default: want 30s, got %v", cfg.NomadTokenPollInterval)
	}
}

func TestLoadFromArgs_NomadTokenFlags(t *testing.T) {
	for _, k := range []string{"NOMAD_TOKEN", "NOMAD_TOKEN_FILE", "NOMAD_TOKEN_POLL_INTERVAL"} {
		os.Unsetenv(k)
	}
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--nomad-token", "static-abc",
		"--nomad-token-file", "/secrets/nomad_token",
		"--nomad-token-poll-interval", "15s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.NomadToken != "static-abc" {
		t.Errorf("NomadToken: want static-abc, got %q", cfg.NomadToken)
	}
	if cfg.NomadTokenFile != "/secrets/nomad_token" {
		t.Errorf("NomadTokenFile: want /secrets/nomad_token, got %q", cfg.NomadTokenFile)
	}
	if cfg.NomadTokenPollInterval != 15*time.Second {
		t.Errorf("NomadTokenPollInterval: want 15s, got %v", cfg.NomadTokenPollInterval)
	}
}

func TestLoadFromArgs_NomadTokenEnvVars(t *testing.T) {
	os.Setenv("NOMAD_TOKEN_FILE", "/run/nomad_token")
	os.Setenv("NOMAD_TOKEN_POLL_INTERVAL", "1m")
	t.Cleanup(func() {
		for _, k := range []string{"NOMAD_TOKEN_FILE", "NOMAD_TOKEN_POLL_INTERVAL"} {
			os.Unsetenv(k)
		}
	})
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.NomadTokenFile != "/run/nomad_token" || cfg.NomadTokenPollInterval != time.Minute {
		t.Errorf("nomad token env vars not honoured: file=%q poll=%v", cfg.NomadTokenFile, cfg.NomadTokenPollInterval)
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

func TestLoadFromArgs_APIKeyDefault(t *testing.T) {
	os.Unsetenv("API_KEY")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIKey != "" {
		t.Errorf("APIKey: want empty (API disabled by default), got %q", cfg.APIKey)
	}
}

func TestLoadFromArgs_APIKeyFlag(t *testing.T) {
	os.Unsetenv("API_KEY")
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--api-key", "mysecretkey",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIKey != "mysecretkey" {
		t.Errorf("APIKey: want mysecretkey, got %q", cfg.APIKey)
	}
}

func TestLoadFromArgs_APIKeyEnv(t *testing.T) {
	os.Setenv("API_KEY", "envkey")
	t.Cleanup(func() { os.Unsetenv("API_KEY") })
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIKey != "envkey" {
		t.Errorf("APIKey: want envkey, got %q", cfg.APIKey)
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

// TestLoadFromArgs_HCLCanonicalFlagRemoved pins the deliberate removal of
// --managed-meta-hcl-canonical: Git is always the source of truth for
// nomad-botherer's own meta keys, and there is no flag to invert that.
func TestLoadFromArgs_HCLCanonicalFlagRemoved(t *testing.T) {
	_, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--managed-meta-hcl-canonical",
	})
	if err == nil {
		t.Error("--managed-meta-hcl-canonical should no longer exist")
	}
}

func TestLoadFromArgs_GitTokenWithPlainHTTPRejected(t *testing.T) {
	os.Unsetenv("GIT_REPO_URL")
	os.Unsetenv("GIT_TOKEN")
	for _, url := range []string{"http://example.com/repo.git", "HTTP://example.com/repo.git"} {
		_, err := LoadFromArgs(newFS(), []string{"--repo-url", url, "--git-token", "secret-token"})
		if err == nil {
			t.Errorf("repo URL %q with git token: want error (cleartext token), got nil", url)
		}
	}
}

func TestLoadFromArgs_GitTokenWithHTTPSAccepted(t *testing.T) {
	os.Unsetenv("GIT_REPO_URL")
	os.Unsetenv("GIT_TOKEN")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/repo.git", "--git-token", "secret-token"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GitToken != "secret-token" {
		t.Errorf("unexpected GitToken: %q", cfg.GitToken)
	}
}

func TestLoadFromArgs_PlainHTTPWithoutTokenAccepted(t *testing.T) {
	os.Unsetenv("GIT_REPO_URL")
	os.Unsetenv("GIT_TOKEN")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "http://example.com/repo.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RepoURL != "http://example.com/repo.git" {
		t.Errorf("unexpected RepoURL: %q", cfg.RepoURL)
	}
}

func TestLoadFromArgs_RedactSecretsDefault(t *testing.T) {
	os.Unsetenv("REDACT_SECRETS")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.RedactSecrets {
		t.Error("RedactSecrets: want true by default, got false")
	}
}

func TestLoadFromArgs_RedactSecretsFlagOff(t *testing.T) {
	os.Unsetenv("REDACT_SECRETS")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git", "--redact-secrets=false"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RedactSecrets {
		t.Error("RedactSecrets: want false after --redact-secrets=false, got true")
	}
}

func TestLoadFromArgs_RedactSecretsEnvOff(t *testing.T) {
	os.Setenv("REDACT_SECRETS", "false")
	t.Cleanup(func() { os.Unsetenv("REDACT_SECRETS") })
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RedactSecrets {
		t.Error("RedactSecrets: want false from env var, got true")
	}
}

func TestLoad_UsesCommandLineAndArgs(t *testing.T) {
	os.Unsetenv("GIT_REPO_URL")
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine
	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
	})
	flag.CommandLine = flag.NewFlagSet("nomad-botherer", flag.ContinueOnError)
	os.Args = []string{"nomad-botherer", "--repo-url", "https://example.com/load.git"}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RepoURL != "https://example.com/load.git" {
		t.Errorf("unexpected RepoURL from Load: %q", cfg.RepoURL)
	}
}

func TestLoadFromArgs_UnknownFlagRejected(t *testing.T) {
	os.Unsetenv("GIT_REPO_URL")
	_, err := LoadFromArgs(newFS(), []string{"--no-such-flag"})
	if err == nil {
		t.Error("unknown flag should produce a parse error")
	}
}

func TestLoadFromArgs_ApplyFlagDefaults(t *testing.T) {
	for _, k := range []string{"DEFAULT_UPDATE_POLICY", "ENABLE_JOB_CREATION", "APPLY_INTERVAL"} {
		os.Unsetenv(k)
	}
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DefaultUpdatePolicy != "none" {
		t.Errorf("DefaultUpdatePolicy default: want none, got %q", cfg.DefaultUpdatePolicy)
	}
	if cfg.EnableJobCreation {
		t.Error("EnableJobCreation should default to false")
	}
	if cfg.ApplyInterval != 10*time.Second {
		t.Errorf("ApplyInterval default: want 10s, got %v", cfg.ApplyInterval)
	}
}

func TestLoadFromArgs_ApplyFlagsSet(t *testing.T) {
	for _, k := range []string{"DEFAULT_UPDATE_POLICY", "ENABLE_JOB_CREATION", "APPLY_INTERVAL"} {
		os.Unsetenv(k)
	}
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--default-update-policy", "image-only",
		"--enable-job-creation",
		"--apply-interval", "30s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DefaultUpdatePolicy != "image-only" {
		t.Errorf("DefaultUpdatePolicy: want image-only, got %q", cfg.DefaultUpdatePolicy)
	}
	if !cfg.EnableJobCreation {
		t.Error("EnableJobCreation: want true")
	}
	if cfg.ApplyInterval != 30*time.Second {
		t.Errorf("ApplyInterval: want 30s, got %v", cfg.ApplyInterval)
	}
}

func TestLoadFromArgs_ApplyFlagEnvVars(t *testing.T) {
	os.Setenv("DEFAULT_UPDATE_POLICY", "full")
	os.Setenv("ENABLE_JOB_CREATION", "true")
	os.Setenv("APPLY_INTERVAL", "1m")
	t.Cleanup(func() {
		for _, k := range []string{"DEFAULT_UPDATE_POLICY", "ENABLE_JOB_CREATION", "APPLY_INTERVAL"} {
			os.Unsetenv(k)
		}
	})
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DefaultUpdatePolicy != "full" || !cfg.EnableJobCreation || cfg.ApplyInterval != time.Minute {
		t.Errorf("env vars not honoured: %+v", cfg)
	}
}

func TestLoadFromArgs_InvalidUpdatePolicyRejected(t *testing.T) {
	os.Unsetenv("DEFAULT_UPDATE_POLICY")
	_, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git", "--default-update-policy", "everything"})
	if err == nil {
		t.Error("invalid update policy should be rejected at config load")
	}
}

func TestLoadFromArgs_RollbackFlagDefaults(t *testing.T) {
	for _, k := range []string{"FLAP_GUARD", "ALLOW_ROLLBACK"} {
		os.Unsetenv(k)
	}
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FlapGuard != "history" {
		t.Errorf("FlapGuard default: want history, got %q", cfg.FlapGuard)
	}
	if cfg.AllowRollback {
		t.Error("AllowRollback should default to false")
	}
}

func TestLoadFromArgs_RollbackFlagsSet(t *testing.T) {
	for _, k := range []string{"FLAP_GUARD", "ALLOW_ROLLBACK"} {
		os.Unsetenv(k)
	}
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--flap-guard", "tag",
		"--allow-rollback",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FlapGuard != "tag" {
		t.Errorf("FlapGuard: want tag, got %q", cfg.FlapGuard)
	}
	if !cfg.AllowRollback {
		t.Error("AllowRollback: want true")
	}
}

func TestLoadFromArgs_RollbackFlagEnvVars(t *testing.T) {
	os.Setenv("FLAP_GUARD", "off")
	os.Setenv("ALLOW_ROLLBACK", "true")
	t.Cleanup(func() {
		for _, k := range []string{"FLAP_GUARD", "ALLOW_ROLLBACK"} {
			os.Unsetenv(k)
		}
	})
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FlapGuard != "off" || !cfg.AllowRollback {
		t.Errorf("rollback env vars not honoured: %+v", cfg)
	}
}

func TestLoadFromArgs_InvalidFlapGuardRejected(t *testing.T) {
	os.Unsetenv("FLAP_GUARD")
	_, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git", "--flap-guard", "sometimes"})
	if err == nil {
		t.Error("invalid flap-guard value should be rejected at config load")
	}
}

func TestLoadFromArgs_TagModeRequiresMetaPrefix(t *testing.T) {
	for _, k := range []string{"FLAP_GUARD", "MANAGED_META_PREFIX"} {
		os.Unsetenv(k)
	}
	_, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--flap-guard", "tag",
		"--managed-meta-prefix", "",
	})
	if err == nil {
		t.Error("--flap-guard=tag with an empty managed-meta-prefix should be rejected at config load")
	}

	// tag mode with the default (non-empty) prefix is fine.
	if _, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git", "--flap-guard", "tag"}); err != nil {
		t.Errorf("tag mode with the default prefix should be accepted, got %v", err)
	}
}

func TestLoadFromArgs_MetaOnlyFlagDefaults(t *testing.T) {
	for _, k := range []string{"APPLY_META_ONLY_CHANGES", "COUNT_META_ONLY_CHANGES"} {
		os.Unsetenv(k)
	}
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ApplyMetaOnlyChanges {
		t.Error("ApplyMetaOnlyChanges should default to false")
	}
	if cfg.CountMetaOnlyChanges {
		t.Error("CountMetaOnlyChanges should default to false")
	}
}

func TestLoadFromArgs_MetaOnlyFlagsSet(t *testing.T) {
	for _, k := range []string{"APPLY_META_ONLY_CHANGES", "COUNT_META_ONLY_CHANGES"} {
		os.Unsetenv(k)
	}
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--apply-meta-only-changes",
		"--count-meta-only-changes",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.ApplyMetaOnlyChanges {
		t.Error("ApplyMetaOnlyChanges: want true after flag")
	}
	if !cfg.CountMetaOnlyChanges {
		t.Error("CountMetaOnlyChanges: want true after flag")
	}
}

func TestLoadFromArgs_MetaOnlyFlagsEnv(t *testing.T) {
	os.Setenv("APPLY_META_ONLY_CHANGES", "true")
	os.Setenv("COUNT_META_ONLY_CHANGES", "1")
	t.Cleanup(func() {
		os.Unsetenv("APPLY_META_ONLY_CHANGES")
		os.Unsetenv("COUNT_META_ONLY_CHANGES")
	})
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.ApplyMetaOnlyChanges || !cfg.CountMetaOnlyChanges {
		t.Errorf("env vars not honoured: %+v", cfg)
	}
}

func TestLoadFromArgs_ApplyExistingDriftDefault(t *testing.T) {
	os.Unsetenv("APPLY_EXISTING_DRIFT")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ApplyExistingDrift {
		t.Error("ApplyExistingDrift should default to false")
	}
}

func TestLoadFromArgs_ApplyExistingDriftSet(t *testing.T) {
	os.Unsetenv("APPLY_EXISTING_DRIFT")
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git", "--apply-existing-drift"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.ApplyExistingDrift {
		t.Error("ApplyExistingDrift: want true after flag")
	}
}

func TestLoadFromArgs_ApplyExistingDriftEnv(t *testing.T) {
	os.Setenv("APPLY_EXISTING_DRIFT", "true")
	t.Cleanup(func() { os.Unsetenv("APPLY_EXISTING_DRIFT") })
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.ApplyExistingDrift {
		t.Error("ApplyExistingDrift: want true from env")
	}
}

func TestLoadFromArgs_DeregisterDefaults(t *testing.T) {
	for _, k := range []string{"ENABLE_DEREGISTER", "DEREGISTER_PURGE", "DEREGISTER_GRACE"} {
		os.Unsetenv(k)
	}
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.EnableDeregister {
		t.Error("EnableDeregister should default to false")
	}
	if cfg.DeregisterPurge {
		t.Error("DeregisterPurge should default to false")
	}
	if cfg.DeregisterGrace != 5*time.Minute {
		t.Errorf("DeregisterGrace default: want 5m, got %v", cfg.DeregisterGrace)
	}
}

func TestLoadFromArgs_DeregisterFlagsSet(t *testing.T) {
	for _, k := range []string{"ENABLE_DEREGISTER", "DEREGISTER_PURGE", "DEREGISTER_GRACE"} {
		os.Unsetenv(k)
	}
	cfg, err := LoadFromArgs(newFS(), []string{
		"--repo-url", "https://example.com/r.git",
		"--enable-deregister", "--deregister-purge", "--deregister-grace", "30s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.EnableDeregister || !cfg.DeregisterPurge || cfg.DeregisterGrace != 30*time.Second {
		t.Errorf("deregister flags not set: %+v", cfg)
	}
}

func TestLoadFromArgs_DeregisterEnv(t *testing.T) {
	os.Setenv("ENABLE_DEREGISTER", "true")
	os.Setenv("DEREGISTER_GRACE", "1h")
	t.Cleanup(func() {
		os.Unsetenv("ENABLE_DEREGISTER")
		os.Unsetenv("DEREGISTER_GRACE")
	})
	cfg, err := LoadFromArgs(newFS(), []string{"--repo-url", "https://example.com/r.git"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.EnableDeregister || cfg.DeregisterGrace != time.Hour {
		t.Errorf("deregister env not honoured: %+v", cfg)
	}
}
