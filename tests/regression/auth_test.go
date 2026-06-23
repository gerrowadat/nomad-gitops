//go:build regression

package regression

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

// Authentication regression tests. These exercise nomad-botherer's Nomad-auth
// code paths (internal/nomad/token.go: resolveNomadToken, the file refresher,
// and the live SetSecretID rotation) against a real ACL-enabled Nomad cluster,
// so token handling is verified against actual ACL enforcement and across the
// Nomad versions in the compatibility matrix.
//
// The cluster here is a dedicated, ACL-enabled agent, separate from the shared
// (ACL-disabled) cluster the other regression tests use — enabling ACLs on the
// shared one would break every other test. It is torn down when the test ends.
//
// "Manual token" is a static --nomad-token. "Workload identity" is the token
// file path: under Nomad, `identity { file = true }` makes Nomad write the
// task's identity token to ${NOMAD_SECRETS_DIR}/nomad_token and rotate it; from
// nomad-botherer's side that is a real ACL token in a file it re-reads. These
// tests provide real ACL tokens through that same file mechanism (and the
// auto-detected path), and verify a rotated token is applied to the live client.

// wiTokenFileName is the filename Nomad writes the workload-identity token to
// under the task secrets dir (mirrors the unexported constant in the nomad
// package, which is not importable from this external test package).
const wiTokenFileName = "nomad_token"

// startACLNomad starts a dedicated ACL-enabled Nomad (same version as the rest
// of the suite), bootstraps ACLs, and returns the address and a management
// token. It skips when Docker is unavailable (e.g. running against an external
// cluster) and fails when Docker is present but the cluster will not start.
func startACLNomad(t *testing.T) (addr, mgmtToken string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; ACL auth test needs to start its own ACL-enabled cluster")
	}
	ver := testNomadVersion
	if ver == "" {
		ver = defaultNomadVersion
	}
	addr, cleanup, err := startNomadDockerWithConfig(ver, "acl {\n  enabled = true\n}\n")
	if err != nil {
		t.Fatalf("starting ACL-enabled Nomad %s: %v", ver, err)
	}
	t.Cleanup(cleanup)

	mgmtToken = aclBootstrap(t, addr)
	return addr, mgmtToken
}

// aclBootstrap bootstraps the ACL system and returns the initial management
// token's secret. It retries: immediately after the agent is up the ACL
// subsystem can briefly be unready.
func aclBootstrap(t *testing.T, addr string) string {
	t.Helper()
	c := newACLClient(t, addr, "")
	deadline := time.Now().Add(30 * time.Second)
	for {
		tok, _, err := c.ACLTokens().Bootstrap(nil)
		if err == nil {
			return tok.SecretID
		}
		if time.Now().After(deadline) {
			t.Fatalf("ACL bootstrap did not succeed within 30s: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// newACLClient builds a Nomad API client for addr authenticating with token
// (empty for anonymous).
func newACLClient(t *testing.T, addr, token string) *nomadapi.Client {
	t.Helper()
	cfg := nomadapi.DefaultConfig()
	cfg.Address = addr
	cfg.SecretID = token
	c, err := nomadapi.NewClient(cfg)
	if err != nil {
		t.Fatalf("nomad client: %v", err)
	}
	return c
}

// createReadToken creates a client token whose policy grants the read
// capabilities nomad-botherer needs for detection, and returns the token.
func createReadToken(t *testing.T, mgmt *nomadapi.Client, policyName string) *nomadapi.ACLToken {
	t.Helper()
	policy := &nomadapi.ACLPolicy{
		Name:        policyName,
		Description: "nomad-botherer auth regression test (read-only)",
		Rules: `namespace "default" {
  capabilities = ["list-jobs", "read-job"]
}`,
	}
	if _, err := mgmt.ACLPolicies().Upsert(policy, nil); err != nil {
		t.Fatalf("upsert ACL policy %s: %v", policyName, err)
	}
	t.Cleanup(func() { mgmt.ACLPolicies().Delete(policyName, nil) })

	tok, _, err := mgmt.ACLTokens().Create(&nomadapi.ACLToken{
		Name:     policyName,
		Type:     "client",
		Policies: []string{policyName},
	}, nil)
	if err != nil {
		t.Fatalf("create ACL token for %s: %v", policyName, err)
	}
	t.Cleanup(func() { mgmt.ACLTokens().Delete(tok.AccessorID, nil) })
	return tok
}

// registerManagedJobWith registers a gitops-managed job using the given client,
// returning its ID. The job is only registered (not waited on); it is visible
// to List immediately, which is all the detection path needs.
func registerManagedJobWith(t *testing.T, client *nomadapi.Client, jobID string) {
	t.Helper()
	job, err := client.Jobs().ParseHCL(testJobHCLWithMeta(jobID, "gitops"), true)
	if err != nil {
		t.Fatalf("ParseHCL: %v", err)
	}
	if _, _, err := client.Jobs().Register(job, &nomadapi.WriteOptions{}); err != nil {
		t.Fatalf("register job %s: %v", jobID, err)
	}
	t.Cleanup(func() { client.Jobs().Deregister(jobID, true, &nomadapi.WriteOptions{}) })
}

// aclDiffCfg is a Config pointing at the ACL cluster with meta selection on and
// a short token-file poll so rotation is observable quickly.
func aclDiffCfg(addr string) *config.Config {
	return &config.Config{
		NomadAddr:              addr,
		NomadNamespace:         "default",
		ManagedMetaPrefix:      "gitops",
		NomadTokenPollInterval: 200 * time.Millisecond,
	}
}

// differSeesJob runs one diff check and reports whether jobID was detected as
// missing_from_hcl (the diff produced for a managed job that is running in
// Nomad with no HCL in the repo). Reaching that requires a successful,
// authorized Jobs.List, so it is a proxy for "the token authenticated".
func differSeesJob(t *testing.T, d *nomad.Differ, jobID, commit string) bool {
	t.Helper()
	if err := d.Check(map[string]string{}, commit); err != nil {
		t.Fatalf("differ.Check: %v", err)
	}
	diffs, _, _ := d.Diffs()
	for _, df := range diffs {
		if df.JobID == jobID && df.DiffType == nomad.DiffTypeMissingFromHCL {
			return true
		}
	}
	return false
}

// TestAuth_WithACLs starts one ACL-enabled Nomad and runs every auth scenario
// against it as a subtest, so the cluster (the slow part) is started once.
func TestAuth_WithACLs(t *testing.T) {
	addr, mgmtToken := startACLNomad(t)
	mgmt := newACLClient(t, addr, mgmtToken)

	t.Run("manual token: valid authenticates", func(t *testing.T) {
		jobID := uniqueJobID("auth-manual-ok")
		registerManagedJobWith(t, mgmt, jobID)
		readTok := createReadToken(t, mgmt, "botherer-manual-"+randomSuffix())

		cfg := aclDiffCfg(addr)
		cfg.NomadToken = readTok.SecretID
		d, err := nomad.NewDifferWithRegistry(cfg, prometheus.NewRegistry())
		if err != nil {
			t.Fatalf("NewDifferWithRegistry: %v", err)
		}
		if !differSeesJob(t, d, jobID, "manual-valid") {
			t.Error("a differ with a valid read token should detect the managed job")
		}
	})

	t.Run("manual token: anonymous is denied", func(t *testing.T) {
		jobID := uniqueJobID("auth-manual-anon")
		registerManagedJobWith(t, mgmt, jobID)

		cfg := aclDiffCfg(addr) // no NomadToken, no file: anonymous
		reg := prometheus.NewRegistry()
		d, err := nomad.NewDifferWithRegistry(cfg, reg)
		if err != nil {
			t.Fatalf("NewDifferWithRegistry: %v", err)
		}
		if differSeesJob(t, d, jobID, "manual-anon") {
			t.Error("anonymous access must not see jobs on an ACL-enabled cluster")
		}
		if got := gatherCounter(t, reg, "nomad_botherer_nomad_api_errors_total"); got == 0 {
			t.Error("anonymous List should be rejected by ACLs and counted in nomad_api_errors_total")
		}
	})

	t.Run("workload identity: explicit token file", func(t *testing.T) {
		jobID := uniqueJobID("auth-wi-file")
		registerManagedJobWith(t, mgmt, jobID)
		readTok := createReadToken(t, mgmt, "botherer-wi-file-"+randomSuffix())

		// Write the token to a file, the way `identity { file = true }` makes
		// Nomad expose the workload's token; nomad-botherer reads it via
		// --nomad-token-file.
		tokenPath := filepath.Join(t.TempDir(), wiTokenFileName)
		writeTokenFile(t, tokenPath, readTok.SecretID)

		cfg := aclDiffCfg(addr)
		cfg.NomadTokenFile = tokenPath
		d, err := nomad.NewDifferWithRegistry(cfg, prometheus.NewRegistry())
		if err != nil {
			t.Fatalf("NewDifferWithRegistry: %v", err)
		}
		if !differSeesJob(t, d, jobID, "wi-file") {
			t.Error("a differ authenticating from a token file should detect the managed job")
		}
	})

	t.Run("workload identity: auto-detected secrets file", func(t *testing.T) {
		jobID := uniqueJobID("auth-wi-auto")
		registerManagedJobWith(t, mgmt, jobID)
		readTok := createReadToken(t, mgmt, "botherer-wi-auto-"+randomSuffix())

		// Simulate the Nomad task secrets dir: ${NOMAD_SECRETS_DIR}/nomad_token.
		// With no token configured, nomad-botherer should auto-detect it.
		secretsDir := t.TempDir()
		writeTokenFile(t, filepath.Join(secretsDir, wiTokenFileName), readTok.SecretID)
		t.Setenv("NOMAD_SECRETS_DIR", secretsDir)

		cfg := aclDiffCfg(addr) // no token, no explicit file
		d, err := nomad.NewDifferWithRegistry(cfg, prometheus.NewRegistry())
		if err != nil {
			t.Fatalf("NewDifferWithRegistry: %v", err)
		}
		if !differSeesJob(t, d, jobID, "wi-auto") {
			t.Error("a differ should auto-detect ${NOMAD_SECRETS_DIR}/nomad_token and authenticate with it")
		}
	})

	t.Run("workload identity: token rotation is applied", func(t *testing.T) {
		jobID := uniqueJobID("auth-wi-rotate")
		registerManagedJobWith(t, mgmt, jobID)

		tokenA := createReadToken(t, mgmt, "botherer-rotate-a-"+randomSuffix())
		tokenB := createReadToken(t, mgmt, "botherer-rotate-b-"+randomSuffix())

		tokenPath := filepath.Join(t.TempDir(), wiTokenFileName)
		writeTokenFile(t, tokenPath, tokenA.SecretID)

		cfg := aclDiffCfg(addr)
		cfg.NomadTokenFile = tokenPath
		reg := prometheus.NewRegistry()
		d, err := nomad.NewDifferWithRegistry(cfg, reg)
		if err != nil {
			t.Fatalf("NewDifferWithRegistry: %v", err)
		}

		// Token A works initially.
		if !differSeesJob(t, d, jobID, "rotate-A") {
			t.Fatal("token A should authenticate before rotation")
		}

		// Start the refresher, then rotate the file to token B and revoke A.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go d.RunTokenRefresher(ctx)

		writeTokenFile(t, tokenPath, tokenB.SecretID)
		if _, err := mgmt.ACLTokens().Delete(tokenA.AccessorID, nil); err != nil {
			t.Fatalf("revoke token A: %v", err)
		}

		// Wait until the refresher has applied a rotated token.
		deadline := time.Now().Add(5 * time.Second)
		for gatherCounter(t, reg, "nomad_botherer_nomad_token_refreshes_total") == 0 {
			if time.Now().After(deadline) {
				t.Fatal("token refresher did not apply the rotated token within 5s")
			}
			time.Sleep(50 * time.Millisecond)
		}

		// Token A is now revoked; the differ must still work, proving token B
		// (from the rotated file) is the one in use.
		if !differSeesJob(t, d, jobID, "rotate-B") {
			t.Error("after rotation the differ should authenticate with token B (token A is revoked)")
		}

		// Sanity: token A really is revoked, so the rotation was meaningful.
		revoked := newACLClient(t, addr, tokenA.SecretID)
		if _, _, err := revoked.Jobs().List(&nomadapi.QueryOptions{}); err == nil {
			t.Error("token A should be rejected after revocation")
		}
	})
}

// writeTokenFile writes a token to path (0600), failing the test on error.
func writeTokenFile(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write token file %s: %v", path, err)
	}
}
