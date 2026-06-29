# JSON API

The JSON API is served on the same HTTP port as the web console
(`--listen-addr`, default `:8080`). It is disabled by default; set `--api-key` /
`API_KEY` to enable it.

## Authentication

All `/api/v1/` endpoints require a pre-shared key as a Bearer token:

```
Authorization: Bearer <your-api-key>
```

There is no TLS built in. In production, front the server with a
TLS-terminating proxy (nginx, Envoy, a load balancer). The API key protects
against unauthenticated reads on an already-reachable port; it is not a
substitute for transport security.

The OpenAPI 3.0 specification is served at `GET /api/openapi.json` without
authentication.

## Endpoints

| Method | Path | Returns | Notes |
|--------|------|---------|-------|
| GET | `/api/v1/diffs` | Current job diffs + last check time + last commit | 503 until startup completes |
| GET | `/api/v1/selected-jobs` | Jobs matched by selection criteria + reason each matched | 503 until startup completes |
| GET | `/api/v1/updates` | GitOps update queue: pending, in-progress, and recent updates | Always available |
| GET | `/api/v1/status` | Git watcher status (last commit, last fetch time) | 503 until git clone completes |
| GET | `/api/v1/version` | Build version, commit hash, build date | Always available |
| POST | `/api/v1/refresh` | `{"message":"refresh triggered"}` | Triggers immediate git pull |
| GET | `/api/openapi.json` | OpenAPI 3.0 spec (JSON) | No authentication required |

## curl examples

```bash
BASE=http://localhost:8080
KEY=your-api-key

# Current diffs
curl -s -H "Authorization: Bearer $KEY" $BASE/api/v1/diffs | jq .

# Jobs being watched and why
curl -s -H "Authorization: Bearer $KEY" $BASE/api/v1/selected-jobs | jq .

# Git watcher status
curl -s -H "Authorization: Bearer $KEY" $BASE/api/v1/status | jq .

# Build version
curl -s -H "Authorization: Bearer $KEY" $BASE/api/v1/version | jq .

# Trigger an immediate refresh
curl -s -X POST -H "Authorization: Bearer $KEY" $BASE/api/v1/refresh | jq .
```

Example `/api/v1/diffs` response when drift is detected:

```json
{
  "diffs": [
    {
      "job_id": "api-server",
      "hcl_file": "jobs/api-server.hcl",
      "diff_type": "modified",
      "detail": "Nomad plan shows diff type \"Edited\"",
      "apply_action": "blocked_by_policy"
    },
    {
      "job_id": "legacy-worker",
      "diff_type": "missing_from_hcl",
      "detail": "job is running in Nomad (status: running) but has no HCL definition in the repo",
      "apply_action": "observation_only"
    }
  ],
  "last_check_time": "2026-05-08T12:00:00Z",
  "last_commit": "abc1234def5678"
}
```

The `apply_action` field on each diff explains whether and why it will (not) be
applied — see
[Why a diff is or is not applied](applying-changes.md#why-a-diff-is-or-is-not-applied).
