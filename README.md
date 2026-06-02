# nomad-botherer

[![Tests](https://github.com/gerrowadat/nomad-botherer/actions/workflows/test.yml/badge.svg)](https://github.com/gerrowadat/nomad-botherer/actions/workflows/test.yml)
[![Coverage](https://raw.githubusercontent.com/wiki/gerrowadat/nomad-botherer/coverage.svg)](https://raw.githack.com/wiki/gerrowadat/nomad-botherer/coverage.html)

Watches a remote git repo for Nomad job HCL definitions and continuously compares them against a live Nomad cluster. When drift is detected it logs, exposes Prometheus metrics, and reports details on `/healthz`.

Three kinds of drift are tracked:

| Diff type | Meaning |
|-----------|---------|
| `modified` | Job exists in both HCL and Nomad but the definitions differ (detected via `nomad job plan`) |
| `missing_from_nomad` | Job defined in HCL but not currently registered in Nomad (dead jobs count as missing by default) |
| `missing_from_hcl` | Job registered and running in Nomad but has no HCL file in the repo (dead jobs are excluded by default) |

---

## Contents

- [Getting started](#getting-started)
  - [Run as a Nomad job](#run-as-a-nomad-job)
  - [Run the binary directly](#run-the-binary-directly)
  - [Opt jobs in to monitoring](#opt-jobs-in-to-monitoring)
- [Design and prior art](#design-and-prior-art)
- [How it works](#how-it-works)
- [Job selection](#job-selection)
- [Installation](#installation)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Webhooks](#webhooks)
- [JSON API](#json-api)
  - [Authentication](#authentication)
  - [Endpoints](#endpoints)
  - [curl examples](#curl-examples)
- [Monitoring](#monitoring)
  - [`/healthz`](#healthz)
  - [`/metrics`](#metrics)
  - [Sample Prometheus configuration](#sample-prometheus-configuration)
- [Docker](#docker)
- [Testing](#testing)
  - [Unit tests](#unit-tests)
  - [Regression tests](#regression-tests)
    - [Prerequisites](#prerequisites)
    - [Running against a Docker-managed Nomad](#running-against-a-docker-managed-nomad)
    - [Targeting a specific Nomad version](#targeting-a-specific-nomad-version)
    - [Testing against multiple versions](#testing-against-multiple-versions)
    - [Using an existing cluster](#using-an-existing-cluster)
    - [What the suite covers](#what-the-suite-covers)
    - [Nomad version compatibility](#nomad-version-compatibility)
- [Development](#development)

---

## Getting started

nomad-botherer needs two things: a git repo containing your Nomad HCL job
definitions, and a Nomad cluster to compare them against.

### Run as a Nomad job

The most common deployment is to run nomad-botherer as a Nomad job on the
same cluster it watches. [`examples/nomad-botherer.hcl`](examples/nomad-botherer.hcl)
is a ready-to-use job definition with every configuration option commented.

1. Copy `examples/nomad-botherer.hcl` into your job repo (or download it).

2. Set the required values in the `env {}` block:
   - `GIT_REPO_URL` — the URL of your HCL repo
   - `API_KEY` — a long random string (used to authenticate requests to the `/api/` endpoints)

3. For a private repo, also set `GIT_TOKEN` (HTTPS) or mount an SSH key and
   set `GIT_SSH_KEY`. The example file has commented instructions for both,
   including how to read secrets from a Nomad Variable rather than hardcoding
   them.

4. If your cluster has ACLs enabled, set `NOMAD_TOKEN` to a token with
   `list-jobs` and `read-job` capabilities on the target namespace.

5. Submit the job:
   ```bash
   nomad job run nomad-botherer.hcl
   ```

6. Watch startup — nomad-botherer clones the repo and runs its first diff
   check before reporting healthy:
   ```bash
   nomad job status nomad-botherer
   curl http://<alloc-ip>:8080/healthz
   ```
   `/healthz` returns `HTTP 503` with `"status": "starting"` until the first
   check completes, then `HTTP 200` with a JSON drift summary.

### Run the binary directly

```bash
./nomad-botherer \
  --repo-url https://github.com/myorg/nomad-jobs.git \
  --nomad-addr http://nomad.example.com:4646
```

For a private HTTPS repo add `--git-token ghp_...`; for SSH add
`--git-ssh-key ~/.ssh/id_ed25519`. See [Quick start](#quick-start) for
more examples and [Configuration](#configuration) for the full flag reference.

### Opt jobs in to monitoring

By default, nomad-botherer only watches jobs that declare a `gitops_managed`
meta key. Add this to any job you want monitored:

```hcl
job "my-service" {
  meta {
    gitops_managed = "true"
  }
  # ...
}
```

Jobs without this key are silently ignored, even if they are running on the
cluster. This is intentional — it prevents nomad-botherer from reporting drift
on manually-managed or third-party jobs that are not in your HCL repo.

To instead watch jobs by name pattern (or watch everything), use
`--job-selector-glob`. Both criteria are a union: a job matching either is
watched. See [Job selection](#job-selection) for details.

---

## Design and prior art

nomad-botherer is currently a **drift detector only** — it observes and reports
differences between Git and a live Nomad cluster but does not apply changes.
Job application (GitOps-style reconciliation) is planned but not yet implemented.

[`docs/prior-art.md`](docs/prior-art.md) surveys the existing Nomad GitOps
tooling (nomad-gitops-operator, nomad-ops, Levant, Waypoint), explains what each
does and where each falls short, and describes the design decisions that will
guide the apply side of nomad-botherer when it is built.

The design proposals for job application and change checkpointing are in
[`docs/proposals/`](docs/proposals/).

---

## How it works

1. On startup, the repo is cloned entirely into memory using [go-git](https://github.com/go-git/go-git).
2. All `.hcl` files under `--hcl-dir` (default: repo root) are sent to Nomad's `/v1/jobs/parse` endpoint to produce canonical `Job` structs.
3. Each parsed job is checked against the configured **job selection criteria** (see [Job selection](#job-selection)). Jobs that match neither the glob nor the `<prefix>.managed` meta key are ignored.
4. For each selected job:
   - If the job is **not registered** in Nomad, or is registered but in **`dead` state** → `missing_from_nomad`
   - If the job **is registered and live**, `nomad job plan` is run → if the plan shows changes → `modified`
5. All jobs **currently running in Nomad** (non-dead) that match the selection criteria but have no corresponding HCL file → `missing_from_hcl`

   Dead jobs are excluded from both checks by default because a stopped job is expected state — it was intentionally halted. Pass `--include-dead-jobs` to treat dead jobs like running ones.
6. Results are stored in memory and exposed via `/healthz` (JSON), `/metrics` (Prometheus), and the authenticated JSON API (`/api/v1/`).
7. The repo is re-checked on every `--poll-interval` (git fetch), on every `--diff-interval` (Nomad-side drift), and immediately on a webhook push event or a `POST /api/v1/refresh` call. When `--max-git-staleness` or `--max-nomad-staleness` is set, a dedicated background goroutine for each forces a refresh if the respective source has not been updated within the configured window — useful when webhooks are unreliable or paused. The two timers are independent and can be set or disabled individually.

---

## Job selection

nomad-botherer does not watch every job in a cluster by default. A job must match at least one of the configured selection criteria to be diffed:

| Criterion | Flag | Default |
|-----------|------|---------|
| Name glob | `--job-selector-glob` | *(empty — no glob selection)* |
| Meta prefix | `--managed-meta-prefix` | `gitops` |

The two criteria are a **union**: a job is selected if it matches the glob *or* has the `<prefix>_managed` meta key set to `"true"`. With the defaults (no glob, prefix `gitops`), only jobs declaring `gitops_managed = "true"` in their registered Nomad meta are watched.

The prefix is a namespace for all meta keys nomad-botherer reads or writes. Using `gitops` means the opt-in key is `gitops_managed`, and future attributes will follow the same `gitops_<attribute>` pattern.

If you need to change the prefix — for example because another team already owns `gitops_*` on the cluster — keep `gitops` as a root and append your qualifier: `gitops_myteam`, `gitops_platform`, etc. This keeps all nomad-botherer keys visually grouped across teams and avoids conflicts with unrelated meta keys.

**Source of truth for the meta key**

By default, the live Nomad job is the source of truth: if `gitops_managed = "true"` is present in the HCL file but not in the running job's meta, the job is not selected. This prevents nomad-botherer from silently picking up jobs that were never explicitly opted in to management at the Nomad level. The HCL meta is used as a fallback only when the job does not yet exist in Nomad, so new jobs declared in HCL are still detected as `missing_from_nomad`.

To opt the other way and treat the HCL as canonical for selection (the behaviour prior to v0.3.0), pass `--managed-meta-hcl-canonical`:

```bash
./nomad-botherer --managed-meta-hcl-canonical ...
# job is selected if HCL carries gitops_managed = "true", regardless of live Nomad meta
```

**Opting a job in via meta tag (default method):**

```hcl
job "my-service" {
  meta {
    gitops_managed = "true"
  }
  # ...rest of job definition
}
```

**Watching all jobs in a directory:**

```bash
./nomad-botherer --job-selector-glob='*' ...
```

**Watching a named prefix:**

```bash
./nomad-botherer --job-selector-glob='production-*' ...
```

**Changing the meta prefix** (useful when sharing a cluster with multiple teams or tools):

```bash
./nomad-botherer --managed-meta-prefix='gitops_myteam' ...
# opts in jobs with meta { gitops_myteam_managed = "true" }
```

Keeping `gitops` as the root of a custom prefix makes all nomad-botherer keys easy to identify across a shared cluster.

**Disabling meta-based selection entirely** (glob only):

```bash
./nomad-botherer --managed-meta-prefix='' --job-selector-glob='myprefix-*' ...
```

If both `--job-selector-glob` and `--managed-meta-prefix` are empty, no jobs are selected and no diffs will be reported. The current selection criteria are shown on the `/` status page.

---

## Installation

### From source

Requires Go 1.25+.

```bash
git clone https://github.com/gerrowadat/nomad-botherer.git
cd nomad-botherer
make build
./nomad-botherer --help
```

### Docker

```bash
docker pull ghcr.io/gerrowadat/nomad-botherer:latest
```

Pre-built images are available for `linux/amd64` and `linux/arm64` (Raspberry Pi 4+).

---

## Quick start

**Public repo, Nomad without ACLs:**

```bash
./nomad-botherer \
  --repo-url https://github.com/myorg/nomad-jobs.git \
  --nomad-addr http://nomad.example.com:4646
```

**Private repo via GitHub PAT, Nomad with ACL token:**

```bash
export GIT_TOKEN=ghp_...
export NOMAD_TOKEN=...
./nomad-botherer \
  --repo-url https://github.com/myorg/nomad-jobs.git \
  --nomad-addr http://nomad.example.com:4646 \
  --hcl-dir jobs
```

**Private repo via SSH key:**

```bash
./nomad-botherer \
  --repo-url git@github.com:myorg/nomad-jobs.git \
  --git-ssh-key ~/.ssh/id_ed25519 \
  --nomad-addr http://nomad.example.com:4646
```

---

## Configuration

Every flag has a corresponding environment variable. Environment variables are read at startup; flags override them when explicitly passed.

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--repo-url` | `GIT_REPO_URL` | *(required)* | Remote git repo URL |
| `--branch` | `GIT_BRANCH` | `main` | Branch to watch |
| `--poll-interval` | `POLL_INTERVAL` | `5m` | How often to poll git for changes |
| `--hcl-dir` | `HCL_DIR` | *(repo root)* | Subdirectory containing HCL job files |
| `--git-token` | `GIT_TOKEN` | | HTTP token for private repos (GitHub PAT etc.) |
| `--git-ssh-key` | `GIT_SSH_KEY` | | Path to SSH private key |
| `--git-ssh-key-password` | `GIT_SSH_KEY_PASSWORD` | | SSH key passphrase |
| `--git-ssh-known-hosts` | `GIT_SSH_KNOWN_HOSTS` | `~/.ssh/known_hosts` | Path to known_hosts file for SSH host key verification; required when using SSH auth. Defaults to the system known_hosts locations. Omit to allow the default search, or set explicitly to a specific file. |
| `--nomad-addr` | `NOMAD_ADDR` | `http://127.0.0.1:4646` | Nomad API address |
| `--nomad-token` | `NOMAD_TOKEN` | | Nomad ACL token |
| `--nomad-namespace` | `NOMAD_NAMESPACE` | `default` | Nomad namespace |
| `--listen-addr` | `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `--webhook-secret` | `WEBHOOK_SECRET` | | GitHub webhook HMAC secret |
| `--webhook-path` | `WEBHOOK_PATH` | `/webhook` | Webhook endpoint path |
| `--api-key` | `API_KEY` | *(empty — disabled)* | Pre-shared key for `/api/` endpoints (Bearer token). Empty disables the JSON API. |
| `--diff-interval` | `DIFF_INTERVAL` | `1m` | Periodic Nomad-side drift check interval |
| `--include-dead-jobs` | `INCLUDE_DEAD_JOBS` | `false` | Treat dead Nomad jobs like running ones (by default dead jobs count as missing) |
| `--job-selector-glob` | `JOB_SELECTOR_GLOB` | *(empty — no glob)* | Glob pattern selecting jobs to watch by name (e.g. `myprefix-*`, `*` for all). Combined with `--managed-meta-prefix` as a union. |
| `--managed-meta-prefix` | `MANAGED_META_PREFIX` | `gitops` | Prefix for job meta keys used by nomad-botherer. With prefix `gitops`, the key `gitops_managed = "true"` opts a job in. Empty disables meta-based selection. |
| `--managed-meta-hcl-canonical` | `MANAGED_META_HCL_CANONICAL` | `false` | When false (default), the live Nomad job's meta is the source of truth for managed-meta-prefix selection. When true, the HCL file is sufficient to opt a job in even if the running job does not carry the key. |
| `--max-git-staleness` | `MAX_GIT_STALENESS` | `0` (disabled) | If the git repo has not been successfully fetched within this window, force an immediate fetch. Set to `0` to disable. E.g. `--max-git-staleness=30m` |
| `--max-nomad-staleness` | `MAX_NOMAD_STALENESS` | `0` (disabled) | If the Nomad diff check has not run within this window, force an immediate check. Set to `0` to disable. E.g. `--max-nomad-staleness=10m` |
| `--log-level` | `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |

Logs are written to stderr as JSON (structured via `log/slog`).

---

## Webhooks

Configuring a webhook removes the latency between a push to the repo and the next drift check — instead of waiting for `--poll-interval`, nomad-botherer fetches immediately on push.

### GitHub setup

1. Go to your repo → **Settings** → **Webhooks** → **Add webhook**
2. Set **Payload URL** to `https://your-host:8080/webhook`
3. Set **Content type** to `application/json`
4. Set **Secret** to the same value as `--webhook-secret` / `WEBHOOK_SECRET`
5. Under **Which events would you like to trigger this webhook?** choose **Just the push event**
6. Click **Add webhook**

The service handles `push` events (triggers a fetch + diff) and `ping` events (acknowledged, no action). All other event types are silently ignored with a `200 OK`.

If `--webhook-secret` is empty, signature verification is skipped. In production, always set a secret.

---

## JSON API

The JSON API is served on the same HTTP port as the web console (`--listen-addr`, default `:8080`). It is disabled by default; set `--api-key` / `API_KEY` to enable it.

### Authentication

All `/api/v1/` endpoints require a pre-shared key as a Bearer token:

```
Authorization: Bearer <your-api-key>
```

There is no TLS built in. In production, front the server with a TLS-terminating proxy (nginx, Envoy, a load balancer). The API key protects against unauthenticated reads on an already-reachable port; it is not a substitute for transport security.

The OpenAPI 3.0 specification is served at `GET /api/openapi.json` without authentication.

### Endpoints

| Method | Path | Returns | Notes |
|--------|------|---------|-------|
| GET | `/api/v1/diffs` | Current job diffs + last check time + last commit | 503 until startup completes |
| GET | `/api/v1/selected-jobs` | Jobs matched by selection criteria + reason each matched | 503 until startup completes |
| GET | `/api/v1/status` | Git watcher status (last commit, last fetch time) | 503 until git clone completes |
| GET | `/api/v1/version` | Build version, commit hash, build date | Always available |
| POST | `/api/v1/refresh` | `{"message":"refresh triggered"}` | Triggers immediate git pull |
| GET | `/api/openapi.json` | OpenAPI 3.0 spec (JSON) | No authentication required |

### curl examples

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
      "detail": "Nomad plan shows diff type \"Edited\""
    },
    {
      "job_id": "legacy-worker",
      "diff_type": "missing_from_hcl",
      "detail": "job is running in Nomad (status: running) but has no HCL definition in the repo"
    }
  ],
  "last_check_time": "2026-05-08T12:00:00Z",
  "last_commit": "abc1234def5678"
}
```

---

## Monitoring

### `/healthz`

Returns **HTTP 200** once the server has built its initial state (completed the first git clone and the first diff check). Until then it returns **HTTP 503** with `"status": "starting"`.

```json
{
  "status": "diffs_detected",
  "diff_count": 2,
  "diffs": [
    {
      "job_id": "api-server",
      "hcl_file": "jobs/api-server.hcl",
      "diff_type": "modified",
      "detail": "Nomad plan shows diff type \"Edited\""
    },
    {
      "job_id": "legacy-worker",
      "diff_type": "missing_from_hcl",
      "detail": "job is running in Nomad (status: running) but has no HCL definition in the repo"
    }
  ],
  "last_check": "2024-01-15T12:00:00Z",
  "git_commit": "abc1234def5678",
  "git_updated": "2024-01-15T11:59:50Z"
}
```

`"status"` is `"ok"` when there are no diffs, `"diffs_detected"` when drift is detected, and `"starting"` (with HTTP 503) before the first diff check completes.

The `/diffs` endpoint and all `/api/v1/` endpoints that return state also return HTTP 503 during startup.

### `/metrics`

Standard Prometheus exposition endpoint. All metric names are prefixed with `nomad_botherer_`.

#### Drift state

These metrics describe the current relationship between the git repo and the live Nomad cluster. They are reset and recomputed on every diff check.

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_drifted_jobs` | Gauge | `diff_type` | Number of jobs currently in each drift state. The simplest signal for "is anything wrong?" — alert on `sum(nomad_botherer_drifted_jobs) > 0`. |
| `nomad_botherer_job_diffs` | Gauge | `job`, `diff_type` | 1 for every (job, diff_type) pair currently detected. Useful for per-job dashboards or filtering by job name. |
| `nomad_botherer_job_drift_first_seen_timestamp_seconds` | Gauge | `job`, `diff_type` | Unix timestamp of when drift was first detected for this job. Absent when no drift is present. `time() - metric` gives how long the job has been drifting — use this to distinguish a deploy in progress from a job that's been stuck for hours. |

#### Diff checks

These counters and timestamps describe the diff check loop itself — how often it runs and whether it is working correctly.

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_diff_checks_total` | Counter | — | Total diff checks run since startup. Use `rate()` to confirm the loop is running at the expected frequency. |
| `nomad_botherer_diff_checks_skipped_total` | Counter | — | Checks skipped because neither the Nomad Raft index nor the git commit changed since the last run. A high skip rate is normal and indicates the optimisation is working. |
| `nomad_botherer_last_check_timestamp_seconds` | Gauge | — | Unix timestamp of the most recent completed diff check. Alert when `time() - metric` exceeds 2× `--diff-interval` to catch a stuck check loop. |
| `nomad_botherer_nomad_api_errors_total` | Counter | `op` (`info`, `plan`, `list`) | Nomad API call failures by operation. `info` = job lookup, `plan` = drift plan, `list` = listing all jobs. A rising count means drift results may be incomplete for that operation. |
| `nomad_botherer_hcl_parse_errors_total` | Counter | — | HCL files that failed to parse via the Nomad API. These files are skipped; the rest of the check continues. |
| `nomad_botherer_hcl_non_job_files_skipped_total` | Counter | — | HCL files that were skipped because they contain no `job` stanza (e.g. ACL policies, volumes). Expected and normal; a rising rate may indicate `--hcl-dir` is set too broadly. |

#### Git tracking

These metrics describe the in-memory git clone and polling loop.

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_git_fetches_total` | Counter | — | Total remote fetch/clone attempts. Each poll interval triggers one. |
| `nomad_botherer_git_fetch_errors_total` | Counter | — | Fetch/clone attempts that failed. A rising count means new commits are not being picked up; diff checks continue against the last known commit. |
| `nomad_botherer_git_last_update_timestamp_seconds` | Gauge | — | Unix timestamp of the last successful fetch. Alert when `time() - metric` is significantly larger than `--poll-interval` to catch a stuck git loop. |

#### Webhooks

These metrics describe incoming webhook events from GitHub.

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_webhook_events_total` | Counter | `event` (`push`, `ping`, `unknown`, `error`) | Webhook events received by type. `push` events trigger an immediate fetch. `error` events indicate a failed delivery (bad signature, parse error, etc.). |
| `nomad_botherer_last_webhook_success_timestamp_seconds` | Gauge | — | Unix timestamp of the last successfully processed webhook. Zero if no webhook has been received yet. |
| `nomad_botherer_last_webhook_failure_timestamp_seconds` | Gauge | — | Unix timestamp of the last failed webhook delivery. Zero if no failure has occurred. |

#### Staleness checking

These counters are only non-zero when `--max-git-staleness` or `--max-nomad-staleness` is configured.

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_git_staleness_refreshes_total` | Counter | — | Git fetches triggered because `time() - nomad_botherer_git_last_update_timestamp_seconds` exceeded `--max-git-staleness`. A rising count means the normal polling or webhook path is not keeping the repo current. |
| `nomad_botherer_nomad_staleness_checks_total` | Counter | — | Nomad diff checks triggered because `time() - nomad_botherer_last_check_timestamp_seconds` exceeded `--max-nomad-staleness`. A rising count means the normal diff loop is falling behind. |

#### Service info

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_info` | Gauge | `version` | Always 1. The `version` label holds the build version string. Useful for tracking rollouts: `count by(version)(nomad_botherer_info)`. |

### Sample Prometheus configuration

The [`monitoring/`](monitoring/) directory contains ready-to-use configuration files:

| File | Contents |
|------|----------|
| [`monitoring/prometheus.yml`](monitoring/prometheus.yml) | Scrape configuration for nomad-botherer |
| [`monitoring/recording_rules.yml`](monitoring/recording_rules.yml) | Pre-aggregated series for dashboards and alerts |
| [`monitoring/alerts.yml`](monitoring/alerts.yml) | Alerting rules covering drift, service health, git, and webhooks |

The alerts cover:

- **NomadJobDrift** — any drift detected for more than 5 minutes
- **NomadJobModifiedPersistent** — a job's config has diverged from git for over 1 hour
- **NomadJobMissingFromNomad** — a git-defined job has been absent from Nomad for over 15 minutes
- **NomadJobMissingFromHCL** — a running Nomad job has no HCL file in the repo for over 1 hour
- **NomadBothererCheckStale** — no diff check has completed in over 5 minutes
- **NomadBothererGitFetchFailing** — git fetches have been failing for 10 minutes
- **NomadBothererGitStale** — the in-memory git clone has not refreshed in over 30 minutes
- **NomadBothererAPIErrors** — Nomad API calls are failing
- **NomadBothererDown** — Prometheus cannot reach the `/metrics` endpoint
- **NomadBothererWebhookErrors** — webhook deliveries are consistently failing

---

## Docker

### Run with HTTP token

```bash
docker run -d \
  -e GIT_REPO_URL=https://github.com/myorg/nomad-jobs.git \
  -e GIT_TOKEN=ghp_... \
  -e NOMAD_ADDR=http://nomad.example.com:4646 \
  -e NOMAD_TOKEN=... \
  -p 8080:8080 \
  ghcr.io/gerrowadat/nomad-botherer:latest
```

### Run with SSH key

```bash
docker run -d \
  -e GIT_REPO_URL=git@github.com:myorg/nomad-jobs.git \
  -e GIT_SSH_KEY=/run/secrets/ssh_key \
  -v /path/to/id_ed25519:/run/secrets/ssh_key:ro \
  -e NOMAD_ADDR=http://nomad.example.com:4646 \
  -p 8080:8080 \
  ghcr.io/gerrowadat/nomad-botherer:latest
```

To enable the JSON API, add `-e API_KEY=your-api-key`.

Supported platforms: `linux/amd64`, `linux/arm64` (Raspberry Pi 4+).

### Available tags

| Tag | Description |
|-----|-------------|
| `latest` | Most recent release |
| `1`, `1.2`, `1.2.3` | Semver aliases, updated on each release |

---

## Testing

### Unit tests

The unit test suite runs against mocked interfaces and requires no external
infrastructure. It runs automatically in CI on every push.

```bash
make test         # go test -race ./...
make test-cover   # run tests and generate coverage.html
```

### Regression tests

The regression suite lives in `tests/regression/` and is excluded from normal
`go test ./...` runs by the `//go:build regression` build tag. It starts a real
Nomad cluster (via Docker or a pre-existing address) and exercises the full
request path: drift detection, job selection, Prometheus metrics, HTTP and JSON
API endpoints, webhook HMAC verification, and the compiled binary's startup
lifecycle.

Run it before cutting a release to verify that the build behaves correctly
against a real cluster.

#### Prerequisites

- **Docker** — the suite pulls and starts `hashicorp/nomad:<version>` automatically. The container runs with `--privileged` to allow Nomad's `raw_exec` driver (used by test jobs) to manage cgroups.
- **Go 1.25+**

#### Running against a Docker-managed Nomad

```bash
make test-regression
```

This pulls the default Nomad image (`1.9.3`), starts a dev-mode cluster, runs
all tests, and stops the container on exit. The full suite takes roughly 5–10
minutes.

#### Targeting a specific Nomad version

```bash
NOMAD_VERSION=1.11.3 make test-regression
```

Or directly:

```bash
NOMAD_VERSION=1.11.3 go test -tags=regression -timeout 15m -v -count=1 ./tests/regression/...
```

`NOMAD_VERSION` must match a tag on the official
[`hashicorp/nomad`](https://hub.docker.com/r/hashicorp/nomad/tags) Docker image.

#### Testing against multiple versions

```bash
make test-regression-versions NOMAD_VERSIONS="1.9.6 1.10.5 1.11.3 2.0.2"
```

This iterates over the list and runs the full suite against each version in
sequence, stopping on the first failure.

#### Using an existing cluster

If you already have a Nomad cluster running, point the suite at it instead of
starting Docker:

```bash
NOMAD_ADDR=http://my-nomad.internal:4646 make test-regression
```

When `NOMAD_ADDR` is set, Docker is not used at all. `NOMAD_TOKEN` is also
honoured if the cluster requires ACL authentication.

The suite clears all Nomad SDK environment variables (`NOMAD_ADDR`,
`NOMAD_TOKEN`, `NOMAD_NAMESPACE`, `NOMAD_REGION`, and the TLS set) from the
process environment before any tests run, then restores them on exit. This
prevents env vars from a developer's shell from leaking into subprocesses
spawned by the E2E tests (the compiled binary, Docker commands). The captured
values are still used to configure the test cluster connection.

Note that Raft-index skip tests (`TestDrift_RaftIndexSkip`,
`TestMetrics_SkipOptimizationCounter`) can be flaky against a shared cluster
because unrelated job or eval activity advances the global `LastIndex` between
calls. They are reliable against the isolated Docker-managed cluster.

#### What the suite covers

| File | What is tested |
|------|----------------|
| `drift_test.go` | All three DiffTypes (`modified`, `missing_from_nomad`, `missing_from_hcl`); dead-job handling (stop-only and purge modes); Raft-index skip optimisation; commit-change bypass; multi-job checks; `ForceCheck` staleness counter |
| `selection_test.go` | Exact glob; wildcard glob; no-match glob; meta-key presence/absence; union selection (both criteria); no criteria configured |
| `metrics_test.go` | All expected metric names registered at construction; gauge values match observed drift; skip counter; first-seen timestamps (set, stable, cleared); parse-error and non-job-skip counters |
| `security_test.go` | Webhook HMAC-SHA256 (valid, invalid, missing, wrong algorithm, large body, concurrent flood, no-secret mode); JSON API auth (missing, wrong, correct key; 100-concurrent load); path-traversal job IDs; very large HCL files; HTML XSS escaping in the index page |
| `e2e_test.go` | Binary lifecycle (503→200 on startup); drift detected over HTTP and `/diffs`; webhook triggers refresh without waiting for next poll interval; JSON API (`/api/v1/diffs`, `/api/v1/status`, `/api/v1/selected-jobs`, `/api/v1/version`, `POST /api/v1/refresh`, `/api/openapi.json`); `/metrics` endpoint content |

#### Nomad version compatibility

[`docs/nomad-versions.md`](docs/nomad-versions.md) documents which Nomad
versions have been verified against each nomad-botherer release by running the
full regression suite. The table is updated manually when a release is cut.
`tests/regression/compat.go` holds a `TestedVersions` slice that mirrors the
table in code.

---

## Development

### Local development with .env

Copy `.env.example` to `.env` and fill in your values. The binary loads `.env`
automatically on startup when the file is present, so you can iterate without
setting environment variables by hand each time.

```bash
cp .env.example .env
$EDITOR .env
make build
./nomad-botherer
```

`.env` is listed in `.gitignore` and will never be committed.

### Build and test

```bash
make build        # compile nomad-botherer
make install      # go install to $GOPATH/bin
make test         # go test -race ./...
make test-cover   # run tests and generate coverage.html
make test-regression                              # regression suite against Nomad 1.9.3 (Docker)
make test-regression NOMAD_VERSION=1.10.2        # regression suite against a specific version
make test-regression-versions NOMAD_VERSIONS="1.9.3 1.10.2"  # test against multiple versions
make lint         # go vet ./...
make clean        # remove build artefacts
```

See [Testing](#testing) for the full regression suite documentation.

### Simulating a webhook

`scripts/send-webhook.sh` constructs a minimal GitHub push event payload and
POSTs it to a locally running instance. It reads defaults from `.env` (URL,
branch, secret) and accepts flags to override any of them.

```bash
# Push to whatever branch GIT_BRANCH is set to in .env (default: main)
scripts/send-webhook.sh

# Override branch and commit SHA
scripts/send-webhook.sh -b develop -c abc1234def5678

# Target a different host or port
scripts/send-webhook.sh -u http://nomad-botherer.internal/webhook

# See all options
scripts/send-webhook.sh -h
```

If `WEBHOOK_SECRET` is set in `.env`, the script signs the request with an
HMAC-SHA256 signature (using `openssl`). If no secret is set, the request is
sent unsigned.

### Release process

Releases use semver git tags. The Makefile handles tag creation:

```bash
make release-patch   # 1.2.3 → 1.2.4
make release-minor   # 1.2.3 → 1.3.0
make release-major   # 1.2.3 → 2.0.0
```

Each `make release-*` creates an annotated tag locally. Push it with:

```bash
git push origin <tag>   # e.g. git push origin v1.2.4
```

Then go to GitHub, find the tag under **Releases**, and **publish** it. Publishing triggers the Docker workflow, which builds and pushes `ghcr.io/gerrowadat/nomad-botherer:<tag>` for both `amd64` and `arm64`.

### Docker builds

```bash
make docker        # build multi-platform image locally (requires docker buildx)
make docker-push   # build and push to ghcr.io
```
