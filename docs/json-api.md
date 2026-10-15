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
| GET | `/api/v1/freezes` | Current [freeze](freezing.md) records + freeze-store health | Always available |
| POST | `/api/v1/freezes/{job}` | Place or replace a freeze; returns the stored record | Requires `--enable-freeze-api`; **501** without it |
| DELETE | `/api/v1/freezes/{job}` | Thaw a job | Requires `--enable-freeze-api`; **501** without it |
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

# Current freezes (always readable)
curl -s -H "Authorization: Bearer $KEY" $BASE/api/v1/freezes | jq .

# Freeze a job for four hours (needs --enable-freeze-api)
curl -s -X POST -H "Authorization: Bearer $KEY" \
  -d '{"reason":"INC-4412: fixing the migration by hand","frozen_by":"doc","expires_in":"4h"}' \
  $BASE/api/v1/freezes/api-server | jq .

# Thaw it
curl -s -X DELETE -H "Authorization: Bearer $KEY" $BASE/api/v1/freezes/api-server | jq .
```

## Freezes

`GET /api/v1/freezes` returns every record, including expired and malformed
ones, plus whether the store is readable at all:

```json
{
  "freezes": [
    {
      "job_id": "api-server",
      "variable_path": "nomad/jobs/gitops/freezes/api-server",
      "reason": "INC-4412: fixing the migration by hand",
      "frozen_by": "doc",
      "frozen_at": "2026-10-15T09:12:04Z",
      "expires_at": "2026-10-15T18:00:00Z",
      "state": "active"
    },
    {
      "job_id": "batch-reindex",
      "variable_path": "nomad/jobs/gitops/freezes/batch-reindex",
      "reason": "waiting on the new index schema",
      "frozen_at": "2026-09-02T11:40:00Z",
      "state": "indefinite"
    }
  ],
  "store_last_read": "2026-10-15T09:40:02Z",
  "store_healthy": true
}
```

`state` is `active`, `indefinite`, `expired` or `malformed` (see
[Freezing a job](freezing.md#record-states)). A `store_healthy` of `false`
means nomad-gitops cannot read the freeze store and is withholding every apply
fail-closed.

The write endpoints are gated by `--enable-freeze-api` on top of the API key —
they are the only endpoints that change cluster-visible state. `POST` takes
`{reason, frozen_by?, expires_in?|expires_at?}`; `reason` is required (**400**
without one), `expires_in` is a Go duration and `expires_at` an RFC3339
instant, and giving neither means `--freeze-default-ttl` (indefinite by
default). A write that loses a check-and-set race is retried once, then returns
**409**.

Example `/api/v1/diffs` response when drift is detected:

```json
{
  "diffs": [
    {
      "job_id": "api-server",
      "hcl_file": "jobs/api-server.hcl",
      "diff_type": "modified",
      "detail": "Nomad plan shows diff type \"Edited\"",
      "apply_action": "blocked_by_policy",
      "apply_detail": "not applied: update policy is \"none\" (the --default-update-policy default), which never applies drift for this job. Set the policy to \"image-only\" or \"full\" to enable applies."
    },
    {
      "job_id": "legacy-worker",
      "diff_type": "missing_from_hcl",
      "detail": "job is running in Nomad (status: running) but has no HCL definition in the repo",
      "apply_action": "observation_only"
    },
    {
      "job_id": "batch-reindex",
      "hcl_file": "jobs/batch-reindex.hcl",
      "diff_type": "modified",
      "detail": "Nomad plan shows diff type \"Edited\"",
      "apply_action": "blocked_frozen",
      "apply_detail": "not applied: frozen until 2026-10-15T18:00:00Z (7h21m0s from now): INC-4412: fixing the migration by hand"
    }
  ],
  "last_check_time": "2026-05-08T12:00:00Z",
  "last_commit": "abc1234def5678"
}
```

The `apply_action` field on each diff is a stable machine-readable disposition
explaining whether and why it will (not) be applied — see
[Why a diff is or is not applied](applying-changes.md#why-a-diff-is-or-is-not-applied).
Some dispositions also carry an `apply_detail`: a human-readable string with the
specific values involved (for a policy block, the effective policy, where it came
from — a job's meta key or the `--default-update-policy` default — and what to
change to apply it). It is omitted when `apply_action` alone says everything. The
`/diffs` text view shows `apply_detail` in place of the generic description when
it is present.
