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

- [Design and prior art](#design-and-prior-art)
- [How it works](#how-it-works)
- [Job selection](#job-selection)
- [Installation](#installation)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Webhooks](#webhooks)
- [gRPC API](#grpc-api)
  - [Authentication](#authentication)
  - [Available RPCs](#available-rpcs)
  - [nbctl — operator CLI](#nbctl--operator-cli)
  - [grpcurl examples](#grpcurl-examples)
- [Monitoring](#monitoring)
  - [`/healthz`](#healthz)
  - [`/metrics`](#metrics)
  - [Sample Prometheus configuration](#sample-prometheus-configuration)
- [Docker](#docker)
- [Development](#development)

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
6. Results are stored in memory and exposed via `/healthz` (JSON), `/metrics` (Prometheus), and the gRPC API.
7. The repo is re-checked on every `--poll-interval` (git fetch), on every `--diff-interval` (Nomad-side drift), and immediately on a webhook push event or a `TriggerRefresh` gRPC call. When `--max-git-staleness` or `--max-nomad-staleness` is set, a dedicated background goroutine for each forces a refresh if the respective source has not been updated within the configured window — useful when webhooks are unreliable or paused. The two timers are independent and can be set or disabled individually.

---

## Job selection

nomad-botherer does not watch every job in a cluster by default. A job must match at least one of the configured selection criteria to be diffed:

| Criterion | Flag | Default |
|-----------|------|---------|
| Name glob | `--job-selector-glob` | *(empty — no glob selection)* |
| Meta prefix | `--managed-meta-prefix` | `gitops` |

The two criteria are a **union**: a job is selected if it matches the glob *or* has the `<prefix>.managed` meta key set to `"true"`. With the defaults (no glob, prefix `gitops`), only jobs declaring `gitops.managed = "true"` in their HCL meta stanza are watched.

The prefix is a namespace for all meta keys nomad-botherer reads or writes. Using `gitops` means the opt-in key is `gitops.managed`, and future attributes will follow the same `gitops.<attribute>` pattern. Use a different prefix if another team or tool already owns `gitops.*` on your cluster.

**Opting a job in via meta tag (default method):**

```hcl
job "my-service" {
  meta {
    "gitops.managed" = "true"
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
./nomad-botherer --managed-meta-prefix='myorg' ...
# opts in jobs with meta { "myorg.managed" = "true" }
```

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
./nbctl --help
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
| `--nomad-addr` | `NOMAD_ADDR` | `http://127.0.0.1:4646` | Nomad API address |
| `--nomad-token` | `NOMAD_TOKEN` | | Nomad ACL token |
| `--nomad-namespace` | `NOMAD_NAMESPACE` | `default` | Nomad namespace |
| `--listen-addr` | `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `--webhook-secret` | `WEBHOOK_SECRET` | | GitHub webhook HMAC secret |
| `--webhook-path` | `WEBHOOK_PATH` | `/webhook` | Webhook endpoint path |
| `--grpc-listen-addr` | `GRPC_LISTEN_ADDR` | `:9090` | gRPC listen address. Set to empty string (`""`) to disable the gRPC server |
| `--grpc-api-key` | `GRPC_API_KEY` | | Pre-shared API key for gRPC authentication. Required when `--grpc-listen-addr` is non-empty |
| `--diff-interval` | `DIFF_INTERVAL` | `1m` | Periodic Nomad-side drift check interval |
| `--include-dead-jobs` | `INCLUDE_DEAD_JOBS` | `false` | Treat dead Nomad jobs like running ones (by default dead jobs count as missing) |
| `--job-selector-glob` | `JOB_SELECTOR_GLOB` | *(empty — no glob)* | Glob pattern selecting jobs to watch by name (e.g. `myprefix-*`, `*` for all). Combined with `--managed-meta-prefix` as a union. |
| `--managed-meta-prefix` | `MANAGED_META_PREFIX` | `gitops` | Prefix for job meta keys used by nomad-botherer. With prefix `gitops`, the key `gitops.managed=true` opts a job in. Empty disables meta-based selection. |
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

## gRPC API

nomad-botherer exposes a gRPC server on `--grpc-listen-addr` (default `:9090`).
The server starts automatically unless the address is set to an empty string.
Setting a non-empty address without also setting an API key is a startup error.

The service is defined in [`proto/nomad_botherer.proto`](proto/nomad_botherer.proto).
Go bindings are pre-generated in `internal/grpcapi/` — no code generation is required
to build the binary.

### Authentication

All RPCs require a pre-shared API key in the `authorization` gRPC metadata header:

```
authorization: Bearer <your-api-key>
```

There is no TLS termination built in. In production, put the gRPC port behind a
TLS-terminating proxy (e.g. nginx, Envoy, or a load balancer), or restrict access
at the network level. The API key protects against unauthenticated reads on an
already-reachable port; it is not a substitute for transport security.

### Available RPCs

| RPC | Request | Response | Description |
|-----|---------|----------|-------------|
| `GetDiffs` | `GetDiffsRequest` | `GetDiffsResponse` | Returns all currently-detected job diffs, plus the last check time and git commit. Returns `codes.Unavailable` until startup is complete. |
| `GetStatus` | `GetStatusRequest` | `GetStatusResponse` | Returns git watcher status: last commit hash and last successful fetch time. Returns `codes.Unavailable` until the initial clone completes. |
| `TriggerRefresh` | `TriggerRefreshRequest` | `TriggerRefreshResponse` | Triggers an immediate git pull and diff check (same effect as a webhook push event) |
| `GetVersion` | `GetVersionRequest` | `GetVersionResponse` | Returns the server's version string, git commit hash, and build date (as set by `-ldflags` at build time; defaults to `dev` / `unknown`) |

### nbctl — operator CLI

`nbctl` is the purpose-built CLI for the gRPC API. It can be installed independently without cloning the repo:

```bash
go install github.com/gerrowadat/nomad-botherer/cmd/nbctl@latest
```

Or built locally alongside the server:

```bash
make build-ctl   # produces ./nbctl
make build       # produces both ./nomad-botherer and ./nbctl
```

#### Configuration

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--server` / `-s` | `NBCTL_SERVER` | `localhost:9090` | gRPC server address |
| `--api-key` / `-k` | `NBCTL_API_KEY` | | API key (required) |
| `--timeout` | | `10s` | Per-request timeout |
| `--output` / `-o` | | `text` | Output format: `text` or `json` |
| `--tls` | | `false` | Use TLS for the gRPC connection |

The API key is most conveniently set via the environment so it does not appear in shell history:

```bash
export NBCTL_API_KEY=your-api-key
export NBCTL_SERVER=nomad-botherer.internal:9090
```

#### Commands

**Show current job diffs:**

```bash
nbctl diffs
```

```
2 diff(s) detected
last check:  2026-05-08T12:00:00Z
last commit: abc1234def5678

[modified] api-server (jobs/api-server.hcl)
  Nomad plan shows diff type "Edited"

[missing_from_hcl] legacy-worker
```

**Show git watcher status:**

```bash
nbctl status
```

```
last commit:  abc1234def5678
last updated: 2026-05-08T11:59:50Z
```

**Trigger an immediate refresh:**

```bash
nbctl refresh
```

```
refresh triggered
```

**Show the server's build version:**

```bash
nbctl version
```

```
version:    v1.2.3
commit:     abc1234def5678
build date: 2026-05-08T10:00:00Z
```

**JSON output** (all commands support `--output json` / `-o json`):

```bash
nbctl diffs -o json
```

```json
{
  "diffs": [
    {
      "job_id": "api-server",
      "hcl_file": "jobs/api-server.hcl",
      "diff_type": "modified",
      "detail": "Nomad plan shows diff type \"Edited\""
    }
  ],
  "last_check_time": "2026-05-08T12:00:00Z",
  "last_commit": "abc1234def5678"
}
```

**Connecting to a remote server with TLS:**

```bash
nbctl --server nomad-botherer.internal:9090 --tls diffs
```

**`nbctl --version`** prints the CLI's own build version (set at link time via `-ldflags`; independent of the server version returned by `nbctl version`).

### grpcurl examples

The examples below use [`grpcurl`](https://github.com/fullstackio/grpcurl), which can be installed with:

```bash
go install github.com/fullstackio/grpcurl/cmd/grpcurl@latest
# or: brew install grpcurl
```

**List available RPCs** (requires the `.proto` file or a reflection-enabled server):

```bash
grpcurl -plaintext \
  -proto proto/nomad_botherer.proto \
  localhost:9090 list nomad_botherer.v1.NomadBotherer
```

**Get current diffs:**

```bash
grpcurl -plaintext \
  -proto proto/nomad_botherer.proto \
  -H 'authorization: Bearer your-api-key' \
  localhost:9090 nomad_botherer.v1.NomadBotherer/GetDiffs
```

Example output when drift is detected:

```json
{
  "diffs": [
    {
      "jobId": "api-server",
      "hclFile": "jobs/api-server.hcl",
      "diffType": "modified",
      "detail": "Nomad plan shows diff type \"Edited\""
    },
    {
      "jobId": "legacy-worker",
      "diffType": "missing_from_hcl"
    }
  ],
  "lastCheckTime": "2026-05-08T12:00:00Z",
  "lastCommit": "abc1234def5678"
}
```

**Get git watcher status:**

```bash
grpcurl -plaintext \
  -proto proto/nomad_botherer.proto \
  -H 'authorization: Bearer your-api-key' \
  localhost:9090 nomad_botherer.v1.NomadBotherer/GetStatus
```

```json
{
  "lastCommit": "abc1234def5678",
  "lastUpdateTime": "2026-05-08T11:59:50Z"
}
```

**Trigger an immediate refresh:**

```bash
grpcurl -plaintext \
  -proto proto/nomad_botherer.proto \
  -H 'authorization: Bearer your-api-key' \
  localhost:9090 nomad_botherer.v1.NomadBotherer/TriggerRefresh
```

```json
{
  "message": "refresh triggered"
}
```

**Get server version:**

```bash
grpcurl -plaintext \
  -proto proto/nomad_botherer.proto \
  -H 'authorization: Bearer your-api-key' \
  localhost:9090 nomad_botherer.v1.NomadBotherer/GetVersion
```

```json
{
  "version": "v1.2.3",
  "commit": "abc1234def5678",
  "buildDate": "2026-05-08T10:00:00Z"
}
```

Binaries built without `-ldflags` return `"version": "dev"`, `"commit": "unknown"`, `"buildDate": "unknown"`.

**Calling from a different host** (e.g. from a workstation against a remote deployment):

```bash
grpcurl -plaintext \
  -proto proto/nomad_botherer.proto \
  -H 'authorization: Bearer your-api-key' \
  nomad-botherer.internal:9090 nomad_botherer.v1.NomadBotherer/GetDiffs
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

The `/diffs` endpoint and all gRPC RPCs that return state also return HTTP 503 / `codes.Unavailable` during startup.

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

#### gRPC

These metrics describe requests to the gRPC server. They are only present when `--grpc-listen-addr` is configured.

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_grpc_requests_total` | Counter | `method`, `code` | Authenticated gRPC requests completed, by full method name and gRPC status code. |
| `nomad_botherer_grpc_auth_errors_total` | Counter | `method` | Requests rejected due to a missing or invalid API key, by method. |

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
  -e GRPC_API_KEY=your-api-key \
  -p 8080:8080 \
  -p 9090:9090 \
  ghcr.io/gerrowadat/nomad-botherer:latest
```

### Run with SSH key

```bash
docker run -d \
  -e GIT_REPO_URL=git@github.com:myorg/nomad-jobs.git \
  -e GIT_SSH_KEY=/run/secrets/ssh_key \
  -v /path/to/id_ed25519:/run/secrets/ssh_key:ro \
  -e NOMAD_ADDR=http://nomad.example.com:4646 \
  -e GRPC_API_KEY=your-api-key \
  -p 8080:8080 \
  -p 9090:9090 \
  ghcr.io/gerrowadat/nomad-botherer:latest
```

Omit `-e GRPC_API_KEY` and `-p 9090:9090` if you do not need the gRPC API.

Supported platforms: `linux/amd64`, `linux/arm64` (Raspberry Pi 4+).

### Available tags

| Tag | Description |
|-----|-------------|
| `latest` | Most recent release |
| `1`, `1.2`, `1.2.3` | Semver aliases, updated on each release |

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
make build        # compile both nomad-botherer and nbctl
make build-server # compile just the server
make build-ctl    # compile just nbctl
make install      # go install both binaries to $GOPATH/bin
make test         # go test -race ./...
make test-cover   # run tests and generate coverage.html
make lint         # go vet ./...
make clean        # remove build artefacts
```

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
