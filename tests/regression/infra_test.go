//go:build regression

package regression

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

// ── Nomad lifecycle ───────────────────────────────────────────────────────────

// startNomadDocker pulls hashicorp/nomad:<version>, starts it in dev mode, and
// waits for the HTTP API to become healthy. It returns the HTTP address, a
// cleanup function that stops the container, and any error.
//
// The container runs with --privileged so Nomad can manage cgroups and run
// raw_exec tasks (child processes inside the container).
func startNomadDocker(version string) (addr string, cleanup func(), err error) {
	return startNomadDockerWithConfig(version, "")
}

// startNomadDockerWithConfig is startNomadDocker with an extra HCL config
// fragment appended to the agent config (e.g. `acl { enabled = true }`). An
// empty fragment is equivalent to startNomadDocker.
func startNomadDockerWithConfig(version, extraConfig string) (addr string, cleanup func(), err error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", nil, fmt.Errorf("docker not in PATH: %w", err)
	}

	image := "hashicorp/nomad:" + version

	// Pull first so a missing/wrong version produces a clear error.
	var pullStderr bytes.Buffer
	pull := exec.Command("docker", "pull", image)
	pull.Stdout = io.Discard
	pull.Stderr = &pullStderr
	if err := pull.Run(); err != nil {
		return "", nil, fmt.Errorf("docker pull %s: %w\n%s", image, err, pullStderr.String())
	}

	// Three distinct ports: HTTP, RPC, and serf. With host networking
	// (Linux) all three must be pinned, or the agent's default RPC/serf
	// ports (4647/4648) collide with any Nomad already running on the host.
	ports := freePorts(3)
	addr = fmt.Sprintf("http://127.0.0.1:%d", ports[0])
	name := fmt.Sprintf("nomad-regression-%s", randomSuffix())

	runArgs, cfgPath, err := buildDockerRunArgs(name, image, ports[0], ports[1], ports[2], extraConfig)
	if err != nil {
		return "", nil, err
	}

	out, err := exec.Command("docker", runArgs...).Output()
	if err != nil {
		if cfgPath != "" {
			os.Remove(cfgPath)
		}
		return "", nil, fmt.Errorf("docker run: %w", err)
	}
	containerID := strings.TrimSpace(string(out))

	cleanup = func() {
		exec.Command("docker", "stop", "-t", "5", containerID).Run()
		exec.Command("docker", "rm", "-f", containerID).Run()
		if cfgPath != "" {
			os.Remove(cfgPath)
		}
	}

	if err := waitForNomadReady(addr, 90*time.Second); err != nil {
		// Container may have already exited; capture logs before cleanup removes it.
		logs, _ := exec.Command("docker", "logs", "--tail", "40", containerID).CombinedOutput()
		cleanup()
		return "", nil, fmt.Errorf("waiting for Nomad: %w\ncontainer logs:\n%s", err, logs)
	}

	return addr, cleanup, nil
}

// buildDockerRunArgs returns the argument slice for "docker run" to start a
// Nomad dev agent. On Linux it uses host networking (avoiding Docker bridge
// port-mapping issues in rootless and DinD environments) with a temporary
// config file that pins the HTTP, RPC, and serf ports — all three, because
// with host networking any port left at its default (4646/4647/4648) collides
// with a Nomad agent already running on the host. The agent binds to loopback
// only, so it is never reachable from the LAN. cfgPath is non-empty on Linux
// and must be removed by the caller after the container exits. On other
// platforms the original -p port-mapping approach is used; the container has
// its own network namespace there, so only the HTTP port needs mapping.
func buildDockerRunArgs(name, image string, httpPort, rpcPort, serfPort int, extraConfig string) (args []string, cfgPath string, err error) {
	if runtime.GOOS != "linux" {
		// The container has its own network namespace, so only the HTTP port
		// needs mapping and no ports config file is required. A config file is
		// still written when there is extra config to carry (e.g. ACLs).
		base := []string{
			"run", "-d",
			"--name", name,
			"--privileged",
			"-p", fmt.Sprintf("%d:4646", httpPort),
		}
		if extraConfig == "" {
			return append(base, image, "agent", "-dev", "-bind=0.0.0.0", "-log-level=error"), "", nil
		}
		path, ferr := writeNomadConfig(extraConfig)
		if ferr != nil {
			return nil, "", ferr
		}
		base = append(base, "-v", path+":/nomad-reg.hcl:ro")
		return append(base, image, "agent", "-dev", "-bind=0.0.0.0", "-config=/nomad-reg.hcl", "-log-level=error"), path, nil
	}

	// Nomad has no CLI flags for ports; they must be set via a config file.
	// Write the ports block (plus any extra config) and mount it read-only.
	ports := fmt.Sprintf("ports {\n  http = %d\n  rpc  = %d\n  serf = %d\n}\n", httpPort, rpcPort, serfPort)
	path, ferr := writeNomadConfig(ports + extraConfig)
	if ferr != nil {
		return nil, "", ferr
	}

	return []string{
		"run", "-d",
		"--name", name,
		"--privileged",
		"--network=host",
		// cgroupns=host is required on cgroupv2 systems: without it Docker
		// creates a private cgroup namespace and Nomad cannot write to
		// cgroup.subtree_control to set up its own process manager.
		"--cgroupns=host",
		"-v", path + ":/nomad-reg.hcl:ro",
		image,
		"agent", "-dev",
		"-bind=127.0.0.1",
		"-config=/nomad-reg.hcl",
		"-log-level=error",
	}, path, nil
}

// writeNomadConfig writes content to a temp HCL file and returns its path. The
// caller mounts it into the container and removes it after the container exits.
func writeNomadConfig(content string) (string, error) {
	f, err := os.CreateTemp("", "nomad-reg-*.hcl")
	if err != nil {
		return "", fmt.Errorf("create nomad config: %w", err)
	}
	if _, err = io.WriteString(f, content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write nomad config: %w", err)
	}
	if err = f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("close nomad config: %w", err)
	}
	return f.Name(), nil
}

// waitForNomadReady polls /v1/agent/self until the HTTP API is serving. A 200
// means the agent is up; a 403 means the API is up but ACLs are enabled and the
// (anonymous) probe is denied — which still indicates readiness for an
// ACL-enabled cluster, whose callers authenticate or bootstrap afterwards.
func waitForNomadReady(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(addr + "/v1/agent/self")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusForbidden {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("Nomad at %s did not respond within %v", addr, timeout)
}

// freePort returns an unused TCP port on loopback.
// There is an inherent TOCTOU race between closing the listener here and the
// caller binding to the port. This is unavoidable with the binary-startup
// protocol used by startBotherer; in practice it is benign on developer
// machines and dedicated CI hosts, but can cause rare port-conflict flakes in
// heavily loaded environments.
func freePort() int {
	return freePorts(1)[0]
}

// freePorts returns n distinct unused TCP ports on loopback. All listeners
// are held open until every port is allocated, so the same port cannot be
// handed out twice. The same TOCTOU caveat as freePort applies once they are
// closed.
func freePorts(n int) []int {
	ports := make([]int, n)
	listeners := make([]net.Listener, n)
	for i := range ports {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			panic(fmt.Sprintf("freePorts: %v", err))
		}
		listeners[i] = l
		ports[i] = l.Addr().(*net.TCPAddr).Port
	}
	for _, l := range listeners {
		l.Close()
	}
	return ports
}

// ── Binary build ──────────────────────────────────────────────────────────────

// buildBinary compiles cmd/nomad-botherer into a temp file.
func buildBinary() (string, error) {
	root, err := findRepoRoot()
	if err != nil {
		return "", err
	}
	out := filepath.Join(os.TempDir(), fmt.Sprintf("nomad-botherer-reg-%d", os.Getpid()))
	cmd := exec.Command("go", "build", "-o", out, "./cmd/nomad-botherer/")
	cmd.Dir = root
	if data, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w\n%s", err, data)
	}
	return out, nil
}

// findRepoRoot walks parent directories to find the one containing go.mod.
func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("go.mod not found above %s", wd)
		}
	}
}

// ── Git repo setup ────────────────────────────────────────────────────────────

// createGitRepo creates a local bare git repository and a working copy in
// t.TempDir(). It returns:
//   - repoURL: file:// URL to the bare repo (use as --repo-url)
//   - workDir: the working copy directory to commit HCL files into
//   - branch:  the branch name (typically "main")
func createGitRepo(t *testing.T) (repoURL, workDir, branch string) {
	t.Helper()

	dir := t.TempDir()
	bareDir := filepath.Join(dir, "repo.git")
	workDir = filepath.Join(dir, "work")

	// Init bare repo.
	gitRun(t, "", "git", "init", "--bare", bareDir)

	// Create a working copy.
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", workDir, err)
	}
	gitRun(t, workDir, "git", "init")
	gitRun(t, workDir, "git", "config", "user.email", "regression@test.invalid")
	gitRun(t, workDir, "git", "config", "user.name", "Regression Test")
	gitRun(t, workDir, "git", "remote", "add", "origin", bareDir)

	// Ensure the branch is named "main" regardless of git's init.default defaulting.
	gitRun(t, workDir, "git", "checkout", "-b", "main")
	branch = "main"

	// Create the initial commit so the branch exists on the remote.
	if err := os.WriteFile(filepath.Join(workDir, ".gitkeep"), []byte(""), 0o644); err != nil {
		t.Fatalf("write .gitkeep: %v", err)
	}
	gitRun(t, workDir, "git", "add", ".")
	gitRun(t, workDir, "git", "commit", "-m", "init")
	gitRun(t, workDir, "git", "push", "-u", "origin", "main")

	return "file://" + bareDir, workDir, branch
}

// commitToGit writes files to the working copy, commits, and pushes.
func commitToGit(t *testing.T, workDir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(workDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	gitRun(t, workDir, "git", "add", ".")
	gitRun(t, workDir, "git", "commit", "-m", "update hcl")
	gitRun(t, workDir, "git", "push")
}

// gitRun runs a git command, failing the test on error.
func gitRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// ── Nomad job helpers ─────────────────────────────────────────────────────────

// registerJobHCL parses hcl using the Nomad API, registers the resulting job,
// and returns its ID. t.Cleanup purges the job when the test ends.
func registerJobHCL(t *testing.T, hcl string) string {
	t.Helper()
	job, err := testNomadClient.Jobs().ParseHCL(hcl, true)
	if err != nil {
		t.Fatalf("ParseHCL: %v", err)
	}
	if _, _, err = testNomadClient.Jobs().Register(job, &nomadapi.WriteOptions{}); err != nil {
		t.Fatalf("Register job %s: %v", *job.ID, err)
	}
	jobID := *job.ID
	t.Cleanup(func() { deregisterJob(t, jobID, true) })
	return jobID
}

// deregisterJob stops and optionally purges a job. Errors are logged, not fatal.
func deregisterJob(t *testing.T, jobID string, purge bool) {
	t.Helper()
	_, _, err := testNomadClient.Jobs().Deregister(jobID, purge, &nomadapi.WriteOptions{})
	if err != nil && !strings.Contains(err.Error(), "404") {
		t.Logf("deregister %s: %v", jobID, err)
	}
}

// stopJob deregisters a job without purging, leaving it in "dead" state.
func stopJob(t *testing.T, jobID string) {
	t.Helper()
	if _, _, err := testNomadClient.Jobs().Deregister(jobID, false, &nomadapi.WriteOptions{}); err != nil {
		t.Fatalf("stop job %s: %v", jobID, err)
	}
}

// waitForJobStatus polls until jobID reaches wantStatus or timeout expires.
func waitForJobStatus(t *testing.T, jobID, wantStatus string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		j, _, err := testNomadClient.Jobs().Info(jobID, &nomadapi.QueryOptions{})
		if err == nil && j.Status != nil && *j.Status == wantStatus {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	j, _, _ := testNomadClient.Jobs().Info(jobID, &nomadapi.QueryOptions{})
	cur := "unknown"
	if j != nil && j.Status != nil {
		cur = *j.Status
	}
	t.Fatalf("job %q: want status %q, got %q after %v", jobID, wantStatus, cur, timeout)
}

// ── HCL templates ────────────────────────────────────────────────────────────

// randomSuffix returns an 8-char random hex string.
func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// uniqueJobID returns a unique, test-safe Nomad job ID.
func uniqueJobID(label string) string {
	return fmt.Sprintf("regtest-%s-%s", label, randomSuffix())
}

// testJobHCL is a minimal service job that stays alive by sleeping.
func testJobHCL(jobID string) string {
	return fmt.Sprintf(`
job %q {
  datacenters = ["dc1"]
  type        = "service"

  group "main" {
    count = 1

    task "sleep" {
      driver = "raw_exec"
      config {
        command = "/bin/sleep"
        args    = ["600"]
      }
      resources {
        cpu    = 10
        memory = 16
      }
    }
  }
}
`, jobID)
}

// testJobHCLModified is the same job with different task args, producing a
// "modified" diff when planned against a job registered with testJobHCL.
func testJobHCLModified(jobID string) string {
	return fmt.Sprintf(`
job %q {
  datacenters = ["dc1"]
  type        = "service"

  group "main" {
    count = 1

    task "sleep" {
      driver = "raw_exec"
      config {
        command = "/bin/sleep"
        args    = ["999"]
      }
      resources {
        cpu    = 10
        memory = 16
      }
    }
  }
}
`, jobID)
}

// testJobHCLWithMeta produces HCL that opts a job in to nomad-botherer via the
// meta block. The key name is "<prefix>_managed" — a valid HCL2 identifier, so
// the block form (meta { ... }) can be used without quoting.
func testJobHCLWithMeta(jobID, metaPrefix string) string {
	return fmt.Sprintf(`
job %q {
  datacenters = ["dc1"]
  type        = "service"

  meta {
    %s = "true"
  }

  group "main" {
    count = 1

    task "sleep" {
      driver = "raw_exec"
      config {
        command = "/bin/sleep"
        args    = ["600"]
      }
      resources {
        cpu    = 10
        memory = 16
      }
    }
  }
}
`, jobID, metaPrefix+"_managed")
}

// ── Differ helpers ────────────────────────────────────────────────────────────

// newTestDiffer creates a Differ backed by the real Nomad client under test.
func newTestDiffer(cfg *config.Config) *nomad.Differ {
	return nomad.NewWithClientAndRegistry(cfg, testNomadClient.Jobs(), prometheus.NewRegistry())
}

// newTestDifferInspectable creates a Differ with an inspectable Prometheus
// registry so tests can check counter values.
func newTestDifferInspectable(cfg *config.Config) (*nomad.Differ, *prometheus.Registry) {
	reg := prometheus.NewRegistry()
	return nomad.NewWithClientAndRegistry(cfg, testNomadClient.Jobs(), reg), reg
}

// baseDiffCfg returns a Config pointing at the test Nomad cluster with no
// job selection configured. Callers should set JobSelectorGlob or
// ManagedMetaPrefix.
func baseDiffCfg() *config.Config {
	return &config.Config{
		NomadAddr:      testNomadAddr,
		NomadNamespace: "default",
	}
}

// gatherCounter sums all metric samples matching name from reg.
func gatherCounter(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if c := m.GetCounter(); c != nil {
				total += c.GetValue()
			}
			if g := m.GetGauge(); g != nil {
				total += g.GetValue()
			}
		}
	}
	return total
}

// ── Mock sources (for server unit-style security tests) ──────────────────────

type mockDiffSource struct {
	ready bool
	diffs []nomad.JobDiff
}

func (m *mockDiffSource) Diffs() ([]nomad.JobDiff, time.Time, string) {
	return m.diffs, time.Now(), "deadbeef"
}
func (m *mockDiffSource) SelectedJobs() ([]nomad.SelectedJob, time.Time, string) {
	return nil, time.Now(), "deadbeef"
}
func (m *mockDiffSource) Updates() []nomad.JobUpdate { return nil }
func (m *mockDiffSource) Ready() bool                { return m.ready }

type mockGitSource struct {
	ready     bool
	triggered bool
}

func (m *mockGitSource) Trigger()                           { m.triggered = true }
func (m *mockGitSource) Status() (string, time.Time)        { return "deadbeef", time.Now() }
func (m *mockGitSource) Ready() bool                        { return m.ready }

// ── Full binary helpers ───────────────────────────────────────────────────────

// startBotherer starts the nomad-botherer binary and waits for /healthz to
// return 200 (meaning startup — git clone + first diff — completed). It
// returns the HTTP base URL and registers a cleanup function.
//
// All extra args are passed directly to the binary. Callers MUST supply
// --repo-url for startup to succeed.
func startBotherer(t *testing.T, extraArgs ...string) string {
	t.Helper()
	if testBinaryPath == "" {
		t.Skip("nomad-botherer binary unavailable (build failed at test startup)")
	}

	httpPort := freePort()
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	args := append([]string{
		fmt.Sprintf("--listen-addr=127.0.0.1:%d", httpPort),
		"--nomad-addr=" + testNomadAddr,
		"--log-level=error",
		"--diff-interval=2s",
		"--poll-interval=2s",
	}, extraArgs...)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, testBinaryPath, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start botherer: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		cmd.Wait()
	})

	if err := waitForHTTPStatus(baseURL+"/healthz", http.StatusOK, 60*time.Second); err != nil {
		t.Fatalf("botherer not ready: %v", err)
	}
	return baseURL
}

// startBothererWithAPI is like startBotherer but also enables the JSON API
// by passing --api-key. Returns the HTTP base URL.
func startBothererWithAPI(t *testing.T, apiKey string, extraArgs ...string) string {
	t.Helper()
	if testBinaryPath == "" {
		t.Skip("nomad-botherer binary unavailable (build failed at test startup)")
	}

	httpPort := freePort()
	httpURL := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	args := append([]string{
		fmt.Sprintf("--listen-addr=127.0.0.1:%d", httpPort),
		"--api-key=" + apiKey,
		"--nomad-addr=" + testNomadAddr,
		"--log-level=error",
		"--diff-interval=2s",
		"--poll-interval=2s",
	}, extraArgs...)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, testBinaryPath, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start botherer (with API): %v", err)
	}
	t.Cleanup(func() {
		cancel()
		cmd.Wait()
	})

	if err := waitForHTTPStatus(httpURL+"/healthz", http.StatusOK, 60*time.Second); err != nil {
		t.Fatalf("botherer not ready: %v", err)
	}
	return httpURL
}

// waitForHTTPStatus polls url until it returns wantCode or timeout expires.
func waitForHTTPStatus(url string, wantCode int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == wantCode {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s did not return %d within %v", url, wantCode, timeout)
}

// ── Infra self-tests ──────────────────────────────────────────────────────────

// TestInfra_FreePorts_Distinct verifies that freePorts never hands out the
// same port twice within one call.
func TestInfra_FreePorts_Distinct(t *testing.T) {
	ports := freePorts(10)
	seen := make(map[int]struct{}, len(ports))
	for _, p := range ports {
		if p == 0 {
			t.Error("freePorts returned port 0")
		}
		if _, dup := seen[p]; dup {
			t.Errorf("freePorts returned duplicate port %d in %v", p, ports)
		}
		seen[p] = struct{}{}
	}
}

// TestInfra_BuildDockerRunArgs_PinsAllPorts verifies that on Linux the
// generated Nomad config pins HTTP, RPC, and serf ports and the agent binds
// to loopback. Leaving RPC or serf at their defaults collides with a Nomad
// agent already running on the host, because the container uses host
// networking.
func TestInfra_BuildDockerRunArgs_PinsAllPorts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("host-networking config path is Linux-only")
	}

	args, cfgPath, err := buildDockerRunArgs("test-name", "test-image", 10001, 10002, 10003, "")
	if err != nil {
		t.Fatalf("buildDockerRunArgs: %v", err)
	}
	if cfgPath == "" {
		t.Fatal("expected a config file path on Linux")
	}
	defer os.Remove(cfgPath)

	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading generated config: %v", err)
	}
	for _, want := range []string{"http = 10001", "rpc  = 10002", "serf = 10003"} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("config missing %q:\n%s", want, cfg)
		}
	}

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--network=host") {
		t.Errorf("expected host networking in args: %v", args)
	}
	if !strings.Contains(joined, "-bind=127.0.0.1") {
		t.Errorf("expected loopback bind in args: %v", args)
	}
}
