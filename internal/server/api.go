package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gerrowadat/nomad-gitops/internal/nomad"
)

// BuildInfo holds version metadata injected at link time.
type BuildInfo struct {
	Version   string
	Commit    string
	BuildDate string
}

// API response types. These are the canonical JSON shapes for all /api/v1/ endpoints.

type diffsResponse struct {
	Diffs         []nomad.JobDiff `json:"diffs"`
	LastCheckTime string          `json:"last_check_time,omitempty"`
	LastCommit    string          `json:"last_commit,omitempty"`
}

type selectedJobsResponse struct {
	Jobs          []nomad.SelectedJob `json:"jobs"`
	LastCheckTime string              `json:"last_check_time,omitempty"`
	LastCommit    string              `json:"last_commit,omitempty"`
}

type updatesResponse struct {
	Updates []nomad.JobUpdate `json:"updates"`
}

type freezesResponse struct {
	Freezes []nomad.FreezeRecord `json:"freezes"`
	// StoreLastRead is the last successful read of the freeze store, and
	// StoreHealthy whether any read has succeeded. A false StoreHealthy means
	// nomad-gitops cannot tell which jobs are frozen and is withholding every
	// apply (unless --freeze-fail-open).
	StoreLastRead string `json:"store_last_read,omitempty"`
	StoreHealthy  bool   `json:"store_healthy"`
}

type freezeRequest struct {
	Reason   string `json:"reason"`
	FrozenBy string `json:"frozen_by,omitempty"`
	// ExpiresIn is a Go duration ("4h"); ExpiresAt an RFC3339 instant. At most
	// one may be given; neither means --freeze-default-ttl (0 = indefinite).
	ExpiresIn string `json:"expires_in,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type thawResponse struct {
	Message string `json:"message"`
}

type statusResponse struct {
	LastCommit  string `json:"last_commit,omitempty"`
	LastUpdated string `json:"last_updated,omitempty"`
}

type versionResponse struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

type refreshResponse struct {
	Message string `json:"message"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// requireAPIKey returns a middleware that enforces Bearer token authentication.
// If apiKey is empty every request is rejected with a clear 401.
// The expected "Bearer <key>" value is compared against the Authorization header
// in constant time. subtle.ConstantTimeCompare only runs in constant time for
// equal-length inputs, so a length check gates it; that leaks the expected
// length but never the key contents. expected and its length are computed once
// per middleware instance to avoid allocating on every request.
func requireAPIKey(apiKey string) func(http.Handler) http.Handler {
	expected := []byte("Bearer " + apiKey)
	expectedLen := len(expected)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("Authorization")
			if apiKey == "" || len(got) != expectedLen || subtle.ConstantTimeCompare([]byte(got), expected) != 1 {
				writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// fmtTime formats t as UTC RFC3339, returning "" for the zero value.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// apiNotReady writes a 503 JSON response for endpoints that need a completed check.
func apiNotReady(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "server is not ready: initial state not yet built"})
}

// ── API handlers ──────────────────────────────────────────────────────────────

func (s *Server) handleAPIDiffs(w http.ResponseWriter, r *http.Request) {
	if !s.git.Ready() || !s.diffs.Ready() {
		apiNotReady(w)
		return
	}
	diffs, lastCheck, lastCommit := s.diffs.Diffs()
	writeJSON(w, http.StatusOK, diffsResponse{
		Diffs:         diffs,
		LastCheckTime: fmtTime(lastCheck),
		LastCommit:    lastCommit,
	})
}

func (s *Server) handleAPISelectedJobs(w http.ResponseWriter, r *http.Request) {
	if !s.git.Ready() || !s.diffs.Ready() {
		apiNotReady(w)
		return
	}
	jobs, lastCheck, lastCommit := s.diffs.SelectedJobs()
	writeJSON(w, http.StatusOK, selectedJobsResponse{
		Jobs:          jobs,
		LastCheckTime: fmtTime(lastCheck),
		LastCommit:    lastCommit,
	})
}

func (s *Server) handleAPIUpdates(w http.ResponseWriter, r *http.Request) {
	updates := s.diffs.Updates()
	if updates == nil {
		updates = []nomad.JobUpdate{}
	}
	writeJSON(w, http.StatusOK, updatesResponse{Updates: updates})
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	if !s.git.Ready() {
		apiNotReady(w)
		return
	}
	lastCommit, lastUpdated := s.git.Status()
	writeJSON(w, http.StatusOK, statusResponse{
		LastCommit:  lastCommit,
		LastUpdated: fmtTime(lastUpdated),
	})
}

func (s *Server) handleAPIVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, versionResponse{
		Version:   s.buildInfo.Version,
		Commit:    s.buildInfo.Commit,
		BuildDate: s.buildInfo.BuildDate,
	})
}

func (s *Server) handleAPIRefresh(w http.ResponseWriter, r *http.Request) {
	s.git.Trigger()
	writeJSON(w, http.StatusOK, refreshResponse{Message: "refresh triggered"})
}

// maxFreezeBodyBytes caps a freeze request body. A record is four short
// strings; anything larger is not a legitimate request.
const maxFreezeBodyBytes = 16 << 10

// maxFreezeReasonBytes caps the reason. It is stored in a Nomad Variable and
// rendered in the console, logs and alerts, so it is a sentence, not a report.
const maxFreezeReasonBytes = 1024

func (s *Server) handleAPIFreezes(w http.ResponseWriter, r *http.Request) {
	if !s.diffs.FreezeEnabled() {
		writeJSON(w, http.StatusOK, freezesResponse{Freezes: []nomad.FreezeRecord{}, StoreHealthy: true})
		return
	}
	recs, lastRead, healthy := s.diffs.Freezes()
	if recs == nil {
		recs = []nomad.FreezeRecord{}
	}
	writeJSON(w, http.StatusOK, freezesResponse{
		Freezes:       recs,
		StoreLastRead: fmtTime(lastRead),
		StoreHealthy:  healthy,
	})
}

// freezeAPIDisabled writes the 501 used when --enable-freeze-api is off. The
// response names the flag: reading freezes works without it, so a caller that
// gets this has the key and only lacks the opt-in.
func freezeAPIDisabled(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotImplemented, errorResponse{
		Error: "the freeze write API is disabled; start nomad-gitops with --enable-freeze-api to enable it, or set the freeze by hand with `nomad var put`",
	})
}

func (s *Server) handleAPIFreezeJob(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.EnableFreezeAPI {
		freezeAPIDisabled(w)
		return
	}
	jobID := r.PathValue("job")
	if jobID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "a job ID is required"})
		return
	}

	var req freezeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFreezeBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "could not parse request body as JSON"})
		return
	}
	// A reason is required, deliberately: an unexplained freeze is the thing
	// this design is most trying to avoid.
	if strings.TrimSpace(req.Reason) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "a freeze needs a reason: say why this job is frozen so whoever finds it later knows"})
		return
	}
	if len(req.Reason) > maxFreezeReasonBytes {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "reason is too long"})
		return
	}
	if req.ExpiresIn != "" && req.ExpiresAt != "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "give expires_in or expires_at, not both"})
		return
	}

	expiresAt, err := resolveFreezeExpiry(req, s.diffs.FreezeDefaultTTL(), time.Now())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	rec, err := s.diffs.FreezeJob(r.Context(), jobID, req.Reason, req.FrozenBy, expiresAt)
	if err != nil {
		if errors.Is(err, nomad.ErrFreezeConflict) {
			writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// resolveFreezeExpiry turns a request's expires_in / expires_at into the
// RFC3339 expiry to store, applying defaultTTL when neither is given. An empty
// result means an indefinite freeze.
func resolveFreezeExpiry(req freezeRequest, defaultTTL time.Duration, now time.Time) (string, error) {
	switch {
	case req.ExpiresAt != "":
		t, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			return "", fmt.Errorf("expires_at %q is not an RFC3339 timestamp", req.ExpiresAt)
		}
		if !t.After(now) {
			return "", fmt.Errorf("expires_at %q is in the past; a freeze that is already expired would suppress nothing", req.ExpiresAt)
		}
		return t.UTC().Format(time.RFC3339), nil
	case req.ExpiresIn != "":
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil {
			return "", fmt.Errorf("expires_in %q is not a duration (e.g. \"4h\", \"30m\")", req.ExpiresIn)
		}
		if d <= 0 {
			return "", fmt.Errorf("expires_in %q must be positive", req.ExpiresIn)
		}
		return now.Add(d).UTC().Format(time.RFC3339), nil
	case defaultTTL > 0:
		return now.Add(defaultTTL).UTC().Format(time.RFC3339), nil
	default:
		// Indefinite: permitted, and alerted on.
		return "", nil
	}
}

func (s *Server) handleAPIThawJob(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.EnableFreezeAPI {
		freezeAPIDisabled(w)
		return
	}
	jobID := r.PathValue("job")
	if jobID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "a job ID is required"})
		return
	}
	if err := s.diffs.ThawJob(r.Context(), jobID); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, thawResponse{Message: "job thawed: " + jobID})
}

// handleAPISpec serves the OpenAPI 3.0 specification for the /api/v1/ endpoints.
// This endpoint does not require authentication.
func (s *Server) handleAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(openAPISpec))
}

// openAPISpec is the OpenAPI 3.0 JSON specification for the /api/v1/ endpoints.
const openAPISpec = `{
  "openapi": "3.0.3",
  "info": {
    "title": "nomad-gitops",
    "description": "Query drift state between a Git repo and a Nomad cluster.",
    "version": "v1"
  },
  "servers": [{"url": "/api/v1"}],
  "security": [{"bearerAuth": []}],
  "components": {
    "securitySchemes": {
      "bearerAuth": {"type": "http", "scheme": "bearer"}
    },
    "schemas": {
      "JobDiff": {
        "type": "object",
        "properties": {
          "job_id":    {"type": "string"},
          "hcl_file":  {"type": "string"},
          "diff_type": {"type": "string", "enum": ["modified", "missing_from_nomad", "missing_from_hcl"]},
          "detail":    {"type": "string"},
          "apply_action": {"type": "string", "description": "Disposition of this diff: whether it will be applied and, if not, why.", "enum": ["queued", "blocked_by_policy", "blocked_preexisting_drift", "blocked_creation_disabled", "skipped_meta_only", "observation_only", "queued_deregister", "deregister_pending_grace", "no_actionable_change", "blocked_known_failed", "blocked_frozen"]},
          "apply_detail": {"type": "string", "description": "Optional human-readable explanation refining apply_action with the specific values involved (e.g. the effective update policy, its source, and what to change to apply the diff). Omitted when apply_action alone is self-explanatory."}
        }
      },
      "SelectedJob": {
        "type": "object",
        "properties": {
          "job_id":           {"type": "string"},
          "selection_reason": {"type": "string", "enum": ["glob", "meta", "both"]}
        }
      },
      "DiffsResponse": {
        "type": "object",
        "properties": {
          "diffs":           {"type": "array", "items": {"$ref": "#/components/schemas/JobDiff"}},
          "last_check_time": {"type": "string", "format": "date-time"},
          "last_commit":     {"type": "string"}
        }
      },
      "SelectedJobsResponse": {
        "type": "object",
        "properties": {
          "jobs":            {"type": "array", "items": {"$ref": "#/components/schemas/SelectedJob"}},
          "last_check_time": {"type": "string", "format": "date-time"},
          "last_commit":     {"type": "string"}
        }
      },
      "JobUpdate": {
        "type": "object",
        "properties": {
          "update_id":              {"type": "string", "description": "Stable across restarts. REGISTER/DEREGISTER: <job_id>/<git_commit_short>. REVERT: <job_id>/revert-<failed_version>."},
          "job_id":                 {"type": "string"},
          "hcl_file":               {"type": "string"},
          "git_commit":             {"type": "string"},
          "operation":              {"type": "string", "enum": ["REGISTER", "DEREGISTER", "REVERT"]},
          "status":                 {"type": "string", "description": "HELD means a freeze was in force when the update reached the front of the queue; it is terminal and the next diff cycle after the thaw re-derives the intent.", "enum": ["PENDING", "IN_PROGRESS", "SUCCEEDED", "FAILED", "SUPERSEDED", "HELD"]},
          "policy":                 {"type": "string", "enum": ["full", "image-only", "none"]},
          "nomad_job_modify_index": {"type": "integer", "description": "CAS token captured at detection time; 0 = job did not exist"},
          "nomad_raft_index":       {"type": "integer"},
          "detected_at":            {"type": "string", "format": "date-time"},
          "applied_at":             {"type": "string", "format": "date-time"},
          "error":                  {"type": "string"},
          "revert_to_version":      {"type": "integer", "description": "REVERT only: the stable job version rolled back to"},
          "revert_from_version":    {"type": "integer", "description": "REVERT only: the failed version used as the CAS guard"}
        }
      },
      "UpdatesResponse": {
        "type": "object",
        "properties": {
          "updates": {"type": "array", "items": {"$ref": "#/components/schemas/JobUpdate"}}
        }
      },
      "FreezeRecord": {
        "type": "object",
        "description": "A temporary, operator-set hold on nomad-gitops's authority to write to one job. Stored as a Nomad Variable, so the source of truth is outside nomad-gitops. A freeze holds REGISTER, DEREGISTER and active rollback; drift detection, reporting and alerting are unaffected.",
        "properties": {
          "job_id":        {"type": "string"},
          "variable_path": {"type": "string", "description": "Nomad Variables path holding this record."},
          "reason":        {"type": "string", "description": "Why the job is frozen. Required on every record."},
          "frozen_by":     {"type": "string", "description": "Self-reported and advisory: Nomad does not record who wrote a Variable."},
          "frozen_at":     {"type": "string", "format": "date-time", "description": "The Variable's CreateTime."},
          "expires_at":    {"type": "string", "format": "date-time", "description": "Omitted for an indefinite freeze."},
          "state":         {"type": "string", "description": "active = expiry in the future; indefinite = no expiry set; expired = past its expiry, suppressing nothing, left in place as the record of why; malformed = unparseable or unreadable, read conservatively as an indefinite freeze.", "enum": ["active", "indefinite", "expired", "malformed"]},
          "issue":         {"type": "string", "description": "Why a malformed record could not be read."}
        }
      },
      "FreezesResponse": {
        "type": "object",
        "properties": {
          "freezes":         {"type": "array", "items": {"$ref": "#/components/schemas/FreezeRecord"}},
          "store_last_read": {"type": "string", "format": "date-time"},
          "store_healthy":   {"type": "boolean", "description": "False means nomad-gitops cannot read the freeze store and is withholding every apply (unless --freeze-fail-open)."}
        }
      },
      "FreezeRequest": {
        "type": "object",
        "required": ["reason"],
        "properties": {
          "reason":     {"type": "string", "description": "Required. Why this job is being frozen."},
          "frozen_by":  {"type": "string"},
          "expires_in": {"type": "string", "description": "Go duration, e.g. \"4h\". Mutually exclusive with expires_at."},
          "expires_at": {"type": "string", "format": "date-time", "description": "Mutually exclusive with expires_in. Neither one means --freeze-default-ttl, which is indefinite by default."}
        }
      },
      "ThawResponse": {
        "type": "object",
        "properties": {
          "message": {"type": "string"}
        }
      },
      "StatusResponse": {
        "type": "object",
        "properties": {
          "last_commit":  {"type": "string"},
          "last_updated": {"type": "string", "format": "date-time"}
        }
      },
      "VersionResponse": {
        "type": "object",
        "properties": {
          "version":    {"type": "string"},
          "commit":     {"type": "string"},
          "build_date": {"type": "string"}
        }
      },
      "RefreshResponse": {
        "type": "object",
        "properties": {
          "message": {"type": "string"}
        }
      },
      "ErrorResponse": {
        "type": "object",
        "properties": {
          "error": {"type": "string"}
        }
      }
    },
    "responses": {
      "Unauthorized": {
        "description": "Missing or invalid API key",
        "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}}
      },
      "ServiceUnavailable": {
        "description": "Server has not completed its initial check",
        "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}}
      }
    }
  },
  "paths": {
    "/diffs": {
      "get": {
        "summary": "Current job diffs",
        "description": "Returns all jobs where drift was detected between Git and Nomad.",
        "responses": {
          "200": {"description": "Diff results", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/DiffsResponse"}}}},
          "401": {"$ref": "#/components/responses/Unauthorized"},
          "503": {"$ref": "#/components/responses/ServiceUnavailable"}
        }
      }
    },
    "/selected-jobs": {
      "get": {
        "summary": "Jobs currently selected for monitoring",
        "description": "Returns all jobs that matched the configured selection criteria during the last check, with the reason each was included.",
        "responses": {
          "200": {"description": "Selected jobs", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/SelectedJobsResponse"}}}},
          "401": {"$ref": "#/components/responses/Unauthorized"},
          "503": {"$ref": "#/components/responses/ServiceUnavailable"}
        }
      }
    },
    "/updates": {
      "get": {
        "summary": "GitOps update queue",
        "description": "Returns the queue of intended job changes derived from detected drift: pending, in-progress, and recently completed updates.",
        "responses": {
          "200": {"description": "Update queue", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/UpdatesResponse"}}}},
          "401": {"$ref": "#/components/responses/Unauthorized"}
        }
      }
    },
    "/status": {
      "get": {
        "summary": "Git watcher status",
        "description": "Returns the last known git commit and fetch time.",
        "responses": {
          "200": {"description": "Git status", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/StatusResponse"}}}},
          "401": {"$ref": "#/components/responses/Unauthorized"},
          "503": {"$ref": "#/components/responses/ServiceUnavailable"}
        }
      }
    },
    "/version": {
      "get": {
        "summary": "Build version",
        "description": "Returns the version, commit hash, and build date of the running binary.",
        "responses": {
          "200": {"description": "Version info", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/VersionResponse"}}}},
          "401": {"$ref": "#/components/responses/Unauthorized"}
        }
      }
    },
    "/refresh": {
      "post": {
        "summary": "Trigger an immediate git pull and diff check",
        "responses": {
          "200": {"description": "Refresh triggered", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/RefreshResponse"}}}},
          "401": {"$ref": "#/components/responses/Unauthorized"}
        }
      }
    },
    "/freezes": {
      "get": {
        "summary": "Current freezes",
        "description": "Returns every freeze record, including expired and malformed ones, plus the health of the freeze store. Always available; the write endpoints below additionally need --enable-freeze-api.",
        "responses": {
          "200": {"description": "Freeze records", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/FreezesResponse"}}}},
          "401": {"$ref": "#/components/responses/Unauthorized"}
        }
      }
    },
    "/freezes/{job}": {
      "parameters": [
        {"name": "job", "in": "path", "required": true, "schema": {"type": "string"}, "description": "Nomad job ID."}
      ],
      "post": {
        "summary": "Freeze a job",
        "description": "Places or replaces a freeze, CAS-protected against the record's last-read ModifyIndex. Requires --enable-freeze-api; returns 501 otherwise.",
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/FreezeRequest"}}}
        },
        "responses": {
          "200": {"description": "The stored freeze record", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/FreezeRecord"}}}},
          "400": {"description": "Missing reason, or an unparseable or past expiry", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}}},
          "401": {"$ref": "#/components/responses/Unauthorized"},
          "409": {"description": "The record changed between being read and written; re-read and retry", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}}},
          "501": {"description": "The freeze write API is disabled (--enable-freeze-api)", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}}}
        }
      },
      "delete": {
        "summary": "Thaw a job",
        "description": "Removes a job's freeze. Thawing a job that is not frozen is not an error. Requires --enable-freeze-api; returns 501 otherwise.",
        "responses": {
          "200": {"description": "Job thawed", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ThawResponse"}}}},
          "401": {"$ref": "#/components/responses/Unauthorized"},
          "501": {"description": "The freeze write API is disabled (--enable-freeze-api)", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}}}
        }
      }
    }
  }
}
`
