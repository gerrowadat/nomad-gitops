package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gerrowadat/nomad-gitops/internal/config"
	"github.com/gerrowadat/nomad-gitops/internal/server"
)

// TestServer_Run_ShutsDownOnContextCancel starts the real HTTP server on an
// ephemeral port and verifies that cancelling the context shuts it down
// cleanly (nil error).
func TestServer_Run_ShutsDownOnContextCancel(t *testing.T) {
	cfg := &config.Config{ListenAddr: "127.0.0.1:0", WebhookPath: "/webhook", Branch: "main"}
	diffSrc := &mockDiffSource{lastCheck: time.Now()}
	gitSrc := &mockGitSource{lastUpdate: time.Now()}
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	// Give ListenAndServe a moment to bind before cancelling.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run should return nil on graceful shutdown, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// TestServer_Run_BadListenAddr verifies that an unbindable address surfaces
// as an error from Run.
func TestServer_Run_BadListenAddr(t *testing.T) {
	cfg := &config.Config{ListenAddr: "256.256.256.256:0", WebhookPath: "/webhook", Branch: "main"}
	diffSrc := &mockDiffSource{lastCheck: time.Now()}
	gitSrc := &mockGitSource{lastUpdate: time.Now()}
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Run(ctx); err == nil {
		t.Error("Run with an unbindable address should return an error")
	}
}

// TestServer_Run_BadListenAddr_NoGoroutineLeak verifies that an early
// ListenAndServe failure does not leave a goroutine blocked waiting on the
// context. Run is called with an unbindable address (so it returns before the
// context is ever cancelled) and the goroutine count is expected to settle
// back to its pre-Run baseline.
func TestServer_Run_BadListenAddr_NoGoroutineLeak(t *testing.T) {
	cfg := &config.Config{ListenAddr: "256.256.256.256:0", WebhookPath: "/webhook", Branch: "main"}
	diffSrc := &mockDiffSource{lastCheck: time.Now()}
	gitSrc := &mockGitSource{lastUpdate: time.Now()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Let any goroutines from earlier tests settle before sampling.
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())
		if err := srv.Run(ctx); err == nil {
			t.Fatal("Run with an unbindable address should return an error")
		}
	}

	// The old implementation leaked one goroutine per failed Run (blocked on
	// ctx.Done()); 20 iterations would show a clear jump. Poll to let the
	// runtime reclaim finished goroutines before asserting.
	var got int
	for i := 0; i < 20; i++ {
		got = runtime.NumGoroutine()
		if got <= baseline+2 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Errorf("goroutine count grew after 20 failed Run calls: baseline %d, got %d", baseline, got)
}

// TestNew_DefaultRegistry exercises the production constructor, which
// registers into the default Prometheus registry. Called once per test
// binary to avoid duplicate-registration panics.
func TestNew_DefaultRegistry(t *testing.T) {
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main"}
	diffSrc := &mockDiffSource{lastCheck: time.Now()}
	gitSrc := &mockGitSource{lastUpdate: time.Now()}
	srv := server.New(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"})
	if srv == nil {
		t.Fatal("New returned nil")
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("healthz on default-registry server: want 200, got %d", rec.Code)
	}
}

// nonGathererRegisterer wraps a Registry so it satisfies only
// prometheus.Registerer, forcing NewWithRegistry's fallback to the global
// /metrics handler.
type nonGathererRegisterer struct{ inner prometheus.Registerer }

func (r nonGathererRegisterer) Register(c prometheus.Collector) error  { return r.inner.Register(c) }
func (r nonGathererRegisterer) MustRegister(c ...prometheus.Collector) { r.inner.MustRegister(c...) }
func (r nonGathererRegisterer) Unregister(c prometheus.Collector) bool { return r.inner.Unregister(c) }

func TestNewWithRegistry_NonGathererFallback(t *testing.T) {
	cfg := &config.Config{ListenAddr: ":0", WebhookPath: "/webhook", Branch: "main"}
	diffSrc := &mockDiffSource{lastCheck: time.Now()}
	gitSrc := &mockGitSource{lastUpdate: time.Now()}
	reg := nonGathererRegisterer{inner: prometheus.NewRegistry()}
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, reg)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/metrics with non-Gatherer registerer: want 200, got %d", rec.Code)
	}
}

// TestIndex_ManagedMetaKeyShown verifies the index page renders the opt-in
// meta key hint when a managed-meta prefix is configured.
func TestIndex_ManagedMetaKeyShown(t *testing.T) {
	cfg := &config.Config{
		ListenAddr:        ":0",
		WebhookPath:       "/webhook",
		Branch:            "main",
		ManagedMetaPrefix: "gitops",
	}
	diffSrc := &mockDiffSource{lastCheck: time.Now()}
	gitSrc := &mockGitSource{lastUpdate: time.Now()}
	srv := server.NewWithRegistry(cfg, diffSrc, gitSrc, server.BuildInfo{Version: "test"}, prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("index: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "gitops_managed") {
		t.Error("index page should mention the gitops_managed opt-in key")
	}
}
