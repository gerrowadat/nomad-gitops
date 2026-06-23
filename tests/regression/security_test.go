//go:build regression

package regression

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
	"github.com/gerrowadat/nomad-botherer/internal/server"
)

// ── Webhook security ──────────────────────────────────────────────────────────

// TestSecurity_Webhook_ValidHMAC verifies that a correctly-signed webhook
// payload is accepted (HTTP 200).
func TestSecurity_Webhook_ValidHMAC(t *testing.T) {
	secret := "super-secret-webhook-" + randomSuffix()
	srv := newWebhookTestServer(t, secret)

	body := minimalPushPayload("main")
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", webhookSig(secret, body))
	req.Header.Set("X-GitHub-Delivery", "delivery-abc")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("valid HMAC: want 200, got %d; body: %s", w.Code, w.Body)
	}
}

// TestSecurity_Webhook_InvalidHMAC verifies that several forms of invalid or
// tampered signatures are rejected. The webhook library returns 400.
func TestSecurity_Webhook_InvalidHMAC(t *testing.T) {
	secret := "super-secret-webhook-" + randomSuffix()
	srv := newWebhookTestServer(t, secret)
	body := minimalPushPayload("main")

	// Compute a valid HMAC signed with a WRONG secret to use as a test value.
	wrongMac := hmac.New(sha256.New, []byte("wrong-secret"))
	wrongMac.Write(body)
	wrongSig := "sha256=" + hex.EncodeToString(wrongMac.Sum(nil))

	for _, badSig := range []string{
		"sha256=" + strings.Repeat("0", 64), // all-zero signature
		"sha256=",                           // empty hex after prefix
		wrongSig,                            // correct format, wrong secret
		"sha1=" + strings.Repeat("0", 40),   // wrong algorithm prefix
		"invalid-format",                    // no prefix at all
	} {
		t.Run(fmt.Sprintf("sig=%q", badSig[:min(len(badSig), 20)]), func(t *testing.T) {
			req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
			req.Header.Set("X-GitHub-Event", "push")
			req.Header.Set("X-Hub-Signature-256", badSig)
			req.Header.Set("X-GitHub-Delivery", "delivery-xyz")
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code == http.StatusOK {
				t.Errorf("sig %q: want non-200, got 200 (possible HMAC bypass)", badSig)
			}
		})
	}
}

// TestSecurity_Webhook_MissingSignature verifies that a request with no
// X-Hub-Signature-256 header is rejected when a secret is configured.
func TestSecurity_Webhook_MissingSignature(t *testing.T) {
	secret := "super-secret-webhook-" + randomSuffix()
	srv := newWebhookTestServer(t, secret)

	body := minimalPushPayload("main")
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("Content-Type", "application/json")
	// Intentionally omit X-Hub-Signature-256.

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("missing signature header: want non-200, got 200")
	}
}

// TestSecurity_Webhook_LargeBody verifies that a very large webhook body
// is handled gracefully (no crash, no hang) even when the signature is valid.
func TestSecurity_Webhook_LargeBody(t *testing.T) {
	secret := "large-body-test-" + randomSuffix()
	srv := newWebhookTestServer(t, secret)

	// 10 MB — valid signature but invalid JSON.
	body := bytes.Repeat([]byte("X"), 10*1024*1024)
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", webhookSig(secret, body))
	req.Header.Set("X-GitHub-Delivery", "large-body")
	req.Header.Set("Content-Type", "application/json")

	done := make(chan struct{})
	go func() {
		defer close(done)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req) // must not block indefinitely
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server hung on large body (>10s)")
	}
}

// TestSecurity_Webhook_ConcurrentRequests verifies that many simultaneous
// webhook requests do not cause races or panics.
func TestSecurity_Webhook_ConcurrentRequests(t *testing.T) {
	secret := "concurrent-webhook-" + randomSuffix()
	srv := newWebhookTestServer(t, secret)

	body := minimalPushPayload("main")
	sig := webhookSig(secret, body)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
			req.Header.Set("X-GitHub-Event", "push")
			req.Header.Set("X-Hub-Signature-256", sig)
			req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("concurrent-%d", i))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
		}(i)
	}
	wg.Wait()
	// The race detector will surface any data races.
}

// TestSecurity_Webhook_NoSecret verifies that when no secret is configured,
// unsigned requests are accepted. This is intentional permissive mode.
func TestSecurity_Webhook_NoSecret(t *testing.T) {
	srv := newWebhookTestServer(t, "")

	body := minimalPushPayload("main")
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "no-secret-test")
	req.Header.Set("Content-Type", "application/json")
	// No signature header.

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("no-secret mode: want 200 (unsigned accepted), got %d", w.Code)
	}
}

// TestSecurity_Webhook_HTMLEscaping verifies that a job ID containing an XSS
// payload is properly escaped in the HTML index page. Guards against stored
// XSS if diff results are displayed in a browser.
func TestSecurity_Webhook_HTMLEscaping(t *testing.T) {
	const xssJobID = `<script>alert(1)</script>`

	diffSrc := &mockDiffSource{
		ready: true,
		diffs: []nomad.JobDiff{
			{JobID: xssJobID, DiffType: "missing_from_hcl", Detail: "xss test"},
		},
	}
	gitSrc := &mockGitSource{ready: true}

	cfg := &config.Config{
		WebhookPath:       "/webhook",
		ManagedMetaPrefix: "gitops",
	}
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "<script>") {
		t.Errorf("HTML index contains unescaped <script> tag (XSS risk):\n%.500s", body)
	}
}

// ── JSON API authentication security ─────────────────────────────────────────

func newAPITestServer(t *testing.T, apiKey string) *server.Server {
	t.Helper()
	cfg := &config.Config{
		WebhookPath:       "/webhook",
		Branch:            "main",
		ManagedMetaPrefix: "gitops",
		APIKey:            apiKey,
	}
	return server.NewWithRegistry(cfg, &mockDiffSource{ready: true}, &mockGitSource{ready: true},
		server.BuildInfo{Version: "regression-test"}, prometheus.NewRegistry())
}

// TestSecurity_API_MissingKey verifies that a request with no Authorization
// header returns 401.
func TestSecurity_API_MissingKey(t *testing.T) {
	srv := newAPITestServer(t, "correct-key-"+randomSuffix())

	req := httptest.NewRequest("GET", "/api/v1/version", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("no key: want 401, got %d", w.Code)
	}
}

// TestSecurity_API_WrongKey verifies that incorrect API keys all produce 401.
func TestSecurity_API_WrongKey(t *testing.T) {
	correctKey := "correct-key-" + randomSuffix()
	srv := newAPITestServer(t, correctKey)

	wrongKeys := []string{
		"",
		"wrong",
		correctKey[:len(correctKey)-1],
		correctKey + "x",
		strings.ToUpper(correctKey),
	}
	for _, k := range wrongKeys {
		t.Run(fmt.Sprintf("key=%q", k), func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/version", nil)
			if k != "" {
				req.Header.Set("Authorization", "Bearer "+k)
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("wrong key %q: want 401, got %d", k, w.Code)
			}
		})
	}
}

// TestSecurity_API_ValidKey verifies that the correct API key grants access.
func TestSecurity_API_ValidKey(t *testing.T) {
	key := "valid-key-" + randomSuffix()
	srv := newAPITestServer(t, key)

	req := httptest.NewRequest("GET", "/api/v1/version", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("valid key: want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "regression-test") {
		t.Errorf("unexpected version response: %s", w.Body.String())
	}
}

// TestSecurity_API_AuthUnderLoad fires 100 concurrent wrong-key requests and
// verifies they all return 401 without races or panics.
func TestSecurity_API_AuthUnderLoad(t *testing.T) {
	correctKey := strings.Repeat("a", 32)
	srv := newAPITestServer(t, correctKey)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/api/v1/version", nil)
			req.Header.Set("Authorization", fmt.Sprintf("Bearer wrong-%d", i))
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("goroutine %d: want 401, got %d", i, w.Code)
			}
		}(i)
	}
	wg.Wait()
}

// TestSecurity_HCL_PathTraversalInJobID verifies that job IDs containing
// path-traversal characters are handled without filesystem access or panics.
// Nomad's ParseHCL validates job IDs server-side; the differ should propagate
// rejection gracefully.
func TestSecurity_HCL_PathTraversalInJobID(t *testing.T) {
	for _, name := range []string{"../evil", "../../etc/passwd"} {
		t.Run(name, func(t *testing.T) {
			hcl := fmt.Sprintf(`job %q { datacenters = ["dc1"]; type = "batch"; group "g" { task "t" { driver = "raw_exec"; config { command = "/bin/true" } } } }`, name)

			cfg := baseDiffCfg()
			cfg.JobSelectorGlob = "*"
			d := newTestDiffer(cfg)

			// Must not panic. ParseHCL or the API call may error — that's fine.
			_ = d.Check(map[string]string{"evil.hcl": hcl}, "commit-evil")
		})
	}
}

// TestSecurity_HCL_VeryLargeFile verifies that a very large HCL file does not
// crash the differ or cause out-of-memory conditions.
func TestSecurity_HCL_VeryLargeFile(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`job "big-job" { datacenters = ["dc1"]; type = "batch"; meta {`)
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&sb, "\n  \"key%d\" = \"value%d\"", i, i)
	}
	sb.WriteString("\n}; group \"g\" { task \"t\" { driver = \"raw_exec\"; config { command = \"/bin/true\" } } } }")

	cfg := baseDiffCfg()
	cfg.JobSelectorGlob = "big-job"
	d := newTestDiffer(cfg)

	_ = d.Check(map[string]string{"big.hcl": sb.String()}, "commit-big")
	// No assertion — a panic would fail the test.
}

// ── helpers ───────────────────────────────────────────────────────────────────

func newWebhookTestServer(t *testing.T, secret string) *server.Server {
	t.Helper()
	cfg := &config.Config{
		WebhookSecret:     secret,
		WebhookPath:       "/webhook",
		Branch:            "main",
		ManagedMetaPrefix: "gitops",
	}
	return server.NewWithRegistry(cfg, &mockDiffSource{ready: true}, &mockGitSource{ready: true},
		server.BuildInfo{Version: "test"}, prometheus.NewRegistry())
}

// webhookSig computes "sha256=<hex>" for body signed with secret.
func webhookSig(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// minimalPushPayload returns a syntactically valid GitHub push event JSON body.
func minimalPushPayload(branch string) []byte {
	return []byte(fmt.Sprintf(`{
		"ref": "refs/heads/%s",
		"before": "aabbcc112233",
		"after":  "ddeeff445566",
		"commits": [],
		"compare": "https://github.com/test/repo/compare/aabbcc..ddeeff",
		"pusher": {"name": "tester", "email": "tester@test.invalid"},
		"repository": {
			"id": 1, "name": "repo", "full_name": "test/repo",
			"private": false,
			"html_url":  "https://github.com/test/repo",
			"clone_url": "https://github.com/test/repo.git"
		}
	}`, branch))
}
