// Package server provides the HTTP server exposing /healthz, /metrics, and
// the git webhook endpoint.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	webhookgithub "github.com/go-playground/webhooks/v6/github"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

// DiffSource is satisfied by *nomad.Differ.
type DiffSource interface {
	Diffs() ([]nomad.JobDiff, time.Time, string)
	SelectedJobs() ([]nomad.SelectedJob, time.Time, string)
	// Updates returns a snapshot of the GitOps update queue.
	Updates() []nomad.JobUpdate
	// Ready reports whether at least one diff check has completed.
	Ready() bool
}

// GitStatusSource is satisfied by *gitwatch.Watcher.
type GitStatusSource interface {
	Trigger()
	Status() (lastCommit string, lastUpdate time.Time)
	// Ready reports whether the initial git clone has completed.
	Ready() bool
}

// maxWebhookBodyBytes caps the webhook request body. GitHub limits webhook
// payloads to 25 MB; anything larger is not a legitimate delivery. Without a
// cap the webhook library reads the entire body into memory, which lets an
// attacker exhaust memory by streaming an arbitrarily large request.
const maxWebhookBodyBytes = 25 << 20

// Server holds the HTTP mux and all dependencies.
type Server struct {
	cfg       *config.Config
	diffs     DiffSource
	git       GitStatusSource
	buildInfo BuildInfo
	mux       *http.ServeMux
	handler   http.Handler // mux wrapped in securityHeaders

	webhookMu               sync.RWMutex
	lastWebhookSuccess      time.Time
	lastWebhookFailure      time.Time

	// Prometheus metrics
	webhookEvents           *prometheus.CounterVec
	lastWebhookSuccessGauge prometheus.Gauge
	lastWebhookFailureGauge prometheus.Gauge
}

// New creates a Server that registers Prometheus metrics into the default registry.
func New(cfg *config.Config, diffs DiffSource, git GitStatusSource, info BuildInfo) *Server {
	return NewWithRegistry(cfg, diffs, git, info, prometheus.DefaultRegisterer)
}

// NewWithRegistry creates a Server with a custom Prometheus Registerer.
// Useful in tests to avoid duplicate-registration panics when creating multiple servers.
func NewWithRegistry(cfg *config.Config, diffs DiffSource, git GitStatusSource, info BuildInfo, reg prometheus.Registerer) *Server {
	s := &Server{
		cfg:       cfg,
		diffs:     diffs,
		git:       git,
		buildInfo: info,

		webhookEvents: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_webhook_events_total",
			Help: "Total number of webhook events received, by event type.",
		}, []string{"event"}),
		lastWebhookSuccessGauge: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Name: "nomad_botherer_last_webhook_success_timestamp_seconds",
			Help: "Unix timestamp of the most recent successfully parsed webhook.",
		}),
		lastWebhookFailureGauge: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Name: "nomad_botherer_last_webhook_failure_timestamp_seconds",
			Help: "Unix timestamp of the most recent webhook that failed to parse.",
		}),
	}

	// Static info metric carrying the build version.
	promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
		Name: "nomad_botherer_info",
		Help: "Build information.",
	}, []string{"version"}).WithLabelValues(info.Version).Set(1)

	// Use the provided registry as the Prometheus gatherer if possible,
	// otherwise fall back to the global default.
	var metricsHandler http.Handler
	if g, ok := reg.(prometheus.Gatherer); ok {
		metricsHandler = promhttp.HandlerFor(g, promhttp.HandlerOpts{})
	} else {
		metricsHandler = promhttp.Handler()
	}

	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/{$}", s.handleIndex)
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/diffs", s.handleDiffs)
	s.mux.Handle("/metrics", metricsHandler)
	s.mux.HandleFunc(cfg.WebhookPath, s.handleWebhook())

	// Mount authenticated JSON API if a key is configured.
	if cfg.APIKey != "" {
		apiMux := http.NewServeMux()
		apiMux.HandleFunc("GET /api/v1/diffs", s.handleAPIDiffs)
		apiMux.HandleFunc("GET /api/v1/selected-jobs", s.handleAPISelectedJobs)
		apiMux.HandleFunc("GET /api/v1/updates", s.handleAPIUpdates)
		apiMux.HandleFunc("GET /api/v1/status", s.handleAPIStatus)
		apiMux.HandleFunc("GET /api/v1/version", s.handleAPIVersion)
		apiMux.HandleFunc("POST /api/v1/refresh", s.handleAPIRefresh)
		s.mux.Handle("/api/v1/", requireAPIKey(cfg.APIKey)(apiMux))
		// OpenAPI spec is public — no auth required.
		s.mux.HandleFunc("GET /api/openapi.json", s.handleAPISpec)
	} else {
		slog.Warn("API key not configured; /api/ endpoints are disabled. Set --api-key / API_KEY to enable.")
	}

	s.handler = securityHeaders{next: s.mux}

	return s
}

// securityHeaders sets standard hardening headers on every response. The web
// console serves no scripts and is never meant to be framed or sniffed.
// It is a comparable struct (not an http.HandlerFunc) so values returned by
// Server.Handler can be compared with ==.
type securityHeaders struct {
	next http.Handler
}

func (s securityHeaders) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	h.Set("Referrer-Policy", "no-referrer")
	s.next.ServeHTTP(w, r)
}

// newHTTPServer constructs the http.Server with timeouts to prevent slowloris
// and other connection-exhaustion attacks.
func (s *Server) newHTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
}

// Run starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	srv := s.newHTTPServer()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	slog.Info("HTTP server listening", "addr", s.cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// Handler returns the underlying http.Handler, useful for testing without a
// real listener.
func (s *Server) Handler() http.Handler {
	return s.handler
}

var indexTmpl = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>nomad-botherer</title>
  <style>
    body { font-family: sans-serif; max-width: 640px; margin: 2em auto; color: #222; }
    h1   { margin-bottom: 0.2em; }
    .ok  { color: #2a7a2a; font-weight: bold; }
    .bad { color: #b94040; font-weight: bold; }
    .starting { color: #7a6a00; font-weight: bold; }
    code { background: #f4f4f4; padding: 0.1em 0.3em; border-radius: 3px; }
    ul   { line-height: 1.8; }
  </style>
</head>
<body>
  <h1>nomad-botherer <small>{{.Version}}</small></h1>
  <p>Status:
    {{- if .Starting}}
    <span class="starting">starting — initial state not yet built</span>
    {{- else if .DiffCount}}
    <span class="bad">{{.DiffCount}} difference(s) detected</span>
    {{- else}}
    <span class="ok">OK — no differences</span>
    {{- end}}
  </p>
  <p>Watching:
    {{- if .SelectionGlob}} jobs matching <code>{{.SelectionGlob}}</code>{{end}}
    {{- if and .SelectionGlob .ManagedMetaKey}}, or{{end}}
    {{- if .ManagedMetaKey}} jobs with <code>{{.ManagedMetaKey}}=true</code> in job meta{{end}}
    {{- if not (or .SelectionGlob .ManagedMetaKey)}} <em>no jobs — no selection criteria configured</em>{{end}}
  </p>
  {{- if .ManagedMetaKey}}
  <p><small>To include a job, add <code>meta { &#34;{{.ManagedMetaKey}}&#34; = &#34;true&#34; }</code> to its HCL definition.</small></p>
  {{- end}}
  <p>Apply mode: default policy <code>{{.DefaultPolicy}}</code>
    {{- if eq .DefaultPolicy "none"}} (detection only unless a job&#39;s meta opts in){{end}},
    job creation {{if .JobCreationEnabled}}<span class="bad">enabled</span>{{else}}disabled{{end}}
    {{- if .PendingUpdates}}, <span class="starting">{{.PendingUpdates}} update(s) pending</span>{{end}}
  </p>
  {{- if .LastCheck}}
  <p>Last diff check: {{.LastCheck}}{{if .Commit}} (commit <code>{{.Commit}}</code>){{end}}</p>
  {{- end}}
  {{- if (or .LastWebhookOK .LastWebhookFail)}}
  <p>Last webhook:
    {{- if .LastWebhookOK}} ok <code>{{.LastWebhookOK}}</code>{{end}}
    {{- if .LastWebhookFail}} &nbsp; failed <code>{{.LastWebhookFail}}</code>{{end}}
  </p>
  {{- end}}
  {{- if .SelectedJobs}}
  <h2>Selected jobs ({{len .SelectedJobs}})</h2>
  <table style="border-collapse:collapse;width:100%">
    <thead><tr style="text-align:left;border-bottom:1px solid #ccc">
      <th style="padding:0.3em 1em 0.3em 0">Job</th>
      <th style="padding:0.3em 0">Selected by</th>
    </tr></thead>
    <tbody>
    {{- range .SelectedJobs}}
    <tr>
      <td style="padding:0.25em 1em 0.25em 0"><code>{{.JobID}}</code></td>
      <td style="padding:0.25em 0">{{.Reason}}</td>
    </tr>
    {{- end}}
    </tbody>
  </table>
  {{- end}}
  <ul>
    <li><a href="/diffs">/diffs</a> — current job diffs (plan-style)</li>
    <li><a href="/healthz">/healthz</a> — JSON health check</li>
    <li><a href="/metrics">/metrics</a> — Prometheus metrics</li>
  </ul>
</body>
</html>
`))

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	starting := !s.git.Ready() || !s.diffs.Ready()

	var diffs []nomad.JobDiff
	var selectedJobs []nomad.SelectedJob
	var lastCheck time.Time
	var commit string
	if !starting {
		diffs, lastCheck, commit = s.diffs.Diffs()
		selectedJobs, _, _ = s.diffs.SelectedJobs()
	}

	s.webhookMu.RLock()
	lastOK := s.lastWebhookSuccess
	lastFail := s.lastWebhookFailure
	s.webhookMu.RUnlock()

	managedMetaKey := ""
	if s.cfg.ManagedMetaPrefix != "" {
		managedMetaKey = s.cfg.ManagedMetaPrefix + "_managed"
	}

	pendingUpdates := 0
	for _, u := range s.diffs.Updates() {
		if u.Status == nomad.JobUpdateStatusPending || u.Status == nomad.JobUpdateStatusInProgress {
			pendingUpdates++
		}
	}
	defaultPolicy := s.cfg.DefaultUpdatePolicy
	if defaultPolicy == "" {
		defaultPolicy = "none"
	}

	data := struct {
		Version            string
		Starting           bool
		DiffCount          int
		SelectedJobs       []nomad.SelectedJob
		LastCheck          string
		Commit             string
		LastWebhookOK      string
		LastWebhookFail    string
		SelectionGlob      string
		ManagedMetaKey     string
		DefaultPolicy      string
		JobCreationEnabled bool
		PendingUpdates     int
	}{
		Version:            s.buildInfo.Version,
		Starting:           starting,
		DiffCount:          len(diffs),
		SelectedJobs:       selectedJobs,
		Commit:             commit,
		SelectionGlob:      s.cfg.JobSelectorGlob,
		ManagedMetaKey:     managedMetaKey,
		DefaultPolicy:      defaultPolicy,
		JobCreationEnabled: s.cfg.EnableJobCreation,
		PendingUpdates:     pendingUpdates,
	}
	if !lastCheck.IsZero() {
		data.LastCheck = lastCheck.Format(time.RFC3339)
	}
	if !lastOK.IsZero() {
		data.LastWebhookOK = lastOK.Format(time.RFC3339)
	}
	if !lastFail.IsZero() {
		data.LastWebhookFail = lastFail.Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if starting {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = indexTmpl.Execute(w, data)
}

func (s *Server) handleDiffs(w http.ResponseWriter, r *http.Request) {
	if !s.git.Ready() || !s.diffs.Ready() {
		http.Error(w, "not ready: initial state not yet built", http.StatusServiceUnavailable)
		return
	}
	diffs, lastCheck, commit := s.diffs.Diffs()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, renderDiffsText(diffs, lastCheck, commit, s.cfg.RedactSecrets))
}

// HealthResponse is the JSON body returned by /healthz.
type HealthResponse struct {
	Status     string      `json:"status"`
	Message    string      `json:"message,omitempty"`
	DiffCount  int         `json:"diff_count"`
	Diffs      []DiffEntry `json:"diffs"`
	LastCheck  string      `json:"last_check,omitempty"`
	GitCommit  string      `json:"git_commit,omitempty"`
	GitUpdated string      `json:"git_updated,omitempty"`
}

// DiffEntry is one element of HealthResponse.Diffs.
type DiffEntry struct {
	JobID    string `json:"job_id"`
	HCLFile  string `json:"hcl_file,omitempty"`
	DiffType string `json:"diff_type"`
	Detail   string `json:"detail"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if !s.git.Ready() || !s.diffs.Ready() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(HealthResponse{
			Status:  "starting",
			Message: "initial state not yet built",
		})
		return
	}

	diffs, lastCheck, gitCommit := s.diffs.Diffs()
	_, gitUpdated := s.git.Status()

	status := "ok"
	if len(diffs) > 0 {
		status = "diffs_detected"
	}

	entries := make([]DiffEntry, 0, len(diffs))
	for _, d := range diffs {
		entries = append(entries, DiffEntry{
			JobID:    d.JobID,
			HCLFile:  d.HCLFile,
			DiffType: string(d.DiffType),
			Detail:   d.Detail,
		})
	}

	resp := HealthResponse{
		Status:    status,
		DiffCount: len(diffs),
		Diffs:     entries,
	}
	if !lastCheck.IsZero() {
		resp.LastCheck = lastCheck.Format(time.RFC3339)
	}
	if gitCommit != "" {
		resp.GitCommit = gitCommit
	}
	if !gitUpdated.IsZero() {
		resp.GitUpdated = gitUpdated.Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// recordWebhookOutcome records the current time into field under the webhook
// mutex and updates the corresponding Prometheus gauge.
func (s *Server) recordWebhookOutcome(field *time.Time, gauge prometheus.Gauge) {
	now := time.Now()
	s.webhookMu.Lock()
	*field = now
	s.webhookMu.Unlock()
	gauge.Set(float64(now.Unix()))
}

func (s *Server) handleWebhook() http.HandlerFunc {
	if s.cfg.WebhookSecret == "" {
		slog.Warn("Webhook secret is empty: webhook endpoint accepts unsigned requests. " +
			"Set --webhook-secret / WEBHOOK_SECRET to require HMAC-SHA256 signatures.")
	}
	hook, err := webhookgithub.New(webhookgithub.Options.Secret(s.cfg.WebhookSecret))
	if err != nil {
		// This only errors with an invalid secret; log and serve a stub.
		slog.Error("Failed to initialise GitHub webhook handler", "err", err)
		return func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "webhook handler misconfigured", http.StatusInternalServerError)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		eventType := r.Header.Get("X-GitHub-Event")
		deliveryID := r.Header.Get("X-GitHub-Delivery")

		// The webhook library reads the whole body into memory; cap it so an
		// oversized request fails instead of exhausting memory.
		r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)

		payload, err := hook.Parse(r, webhookgithub.PushEvent, webhookgithub.PingEvent)
		if err != nil {
			if err == webhookgithub.ErrEventNotFound {
				s.webhookEvents.WithLabelValues("unknown").Inc()
				slog.Debug("Ignoring unhandled webhook event", "event", eventType, "delivery", deliveryID)
				w.WriteHeader(http.StatusOK)
				return
			}
			s.webhookEvents.WithLabelValues("error").Inc()
			slog.Warn("Webhook rejected", "event", eventType, "delivery", deliveryID, "err", err)
			s.recordWebhookOutcome(&s.lastWebhookFailure, s.lastWebhookFailureGauge)
			http.Error(w, "bad webhook payload", http.StatusBadRequest)
			return
		}

		switch p := payload.(type) {
		case webhookgithub.PushPayload:
			s.webhookEvents.WithLabelValues("push").Inc()
			branch := strings.TrimPrefix(p.Ref, "refs/heads/")
			slog.Info("Received push webhook",
				"delivery", deliveryID,
				"repo", p.Repository.FullName,
				"branch", branch,
				"before", p.Before,
				"after", p.After,
				"commits", len(p.Commits),
				"pusher", p.Pusher.Name,
				"compare", p.Compare,
			)
			if branch == s.cfg.Branch {
				s.git.Trigger()
			}
		case webhookgithub.PingPayload:
			s.webhookEvents.WithLabelValues("ping").Inc()
			slog.Info("Received ping webhook",
				"delivery", deliveryID,
				"hook_id", p.HookID,
				"repo", p.Repository.FullName,
				"events", p.Hook.Events,
			)
		}

		s.recordWebhookOutcome(&s.lastWebhookSuccess, s.lastWebhookSuccessGauge)

		w.WriteHeader(http.StatusOK)
	}
}
