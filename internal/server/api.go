package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gerrowadat/nomad-botherer/internal/nomad"
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
// Both sides are hashed before the constant-time compare: ConstantTimeCompare
// returns immediately on unequal lengths, which would otherwise leak the key
// length through response timing.
func requireAPIKey(apiKey string) func(http.Handler) http.Handler {
	expected := sha256.Sum256([]byte("Bearer " + apiKey))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
			if apiKey == "" || subtle.ConstantTimeCompare(got[:], expected[:]) != 1 {
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
    "title": "nomad-botherer",
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
          "detail":    {"type": "string"}
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
          "update_id":              {"type": "string", "description": "<job_id>/<git_commit_short>; stable across restarts"},
          "job_id":                 {"type": "string"},
          "hcl_file":               {"type": "string"},
          "git_commit":             {"type": "string"},
          "operation":              {"type": "string", "enum": ["REGISTER", "DEREGISTER"]},
          "status":                 {"type": "string", "enum": ["PENDING", "IN_PROGRESS", "SUCCEEDED", "FAILED", "SUPERSEDED"]},
          "policy":                 {"type": "string", "enum": ["full", "image-only", "none"]},
          "nomad_job_modify_index": {"type": "integer", "description": "CAS token captured at detection time; 0 = job did not exist"},
          "nomad_raft_index":       {"type": "integer"},
          "detected_at":            {"type": "string", "format": "date-time"},
          "applied_at":             {"type": "string", "format": "date-time"},
          "error":                  {"type": "string"}
        }
      },
      "UpdatesResponse": {
        "type": "object",
        "properties": {
          "updates": {"type": "array", "items": {"$ref": "#/components/schemas/JobUpdate"}}
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
    }
  }
}
`
