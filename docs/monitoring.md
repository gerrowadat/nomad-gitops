# Monitoring

nomad-botherer exposes its state three ways: a JSON `/healthz` summary, a
Prometheus `/metrics` endpoint, and the [JSON API](json-api.md).

## `/healthz`

Returns **HTTP 200** once the server has built its initial state (completed the
first git clone and the first diff check). Until then it returns **HTTP 503**
with `"status": "starting"`.

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

`"status"` is `"ok"` when there are no diffs, `"diffs_detected"` when drift is
detected, and `"starting"` (with HTTP 503) before the first diff check completes.

The `/diffs` endpoint and all `/api/v1/` endpoints that return state also return
HTTP 503 during startup.

By default (`--redact-secrets`), values that might be secrets are redacted from
plan diffs before they are stored, so `/diffs` shows them as `[REDACTED]` with a
`(value redacted)` annotation, and a banner in the output says so. This covers
all env vars, template bodies (`template` stanza contents), and any field whose
name contains a secret-like keyword (e.g. `Meta[db_password]`,
`Config[registry_token]`). The shape of the diff — field names, added/deleted/
edited markers, nesting — is unchanged.

## `/metrics`

Standard Prometheus exposition endpoint. All metric names are prefixed with
`nomad_botherer_`.

### Drift state

These metrics describe the current relationship between the git repo and the
live Nomad cluster. They are reset and recomputed on every diff check.

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_drifted_jobs` | Gauge | `diff_type` | Number of jobs currently in each drift state. The simplest signal for "is anything wrong?" — alert on `sum(nomad_botherer_drifted_jobs) > 0`. |
| `nomad_botherer_job_diffs` | Gauge | `job`, `diff_type` | 1 for every (job, diff_type) pair currently detected. Useful for per-job dashboards or filtering by job name. |
| `nomad_botherer_job_drift_first_seen_timestamp_seconds` | Gauge | `job`, `diff_type` | Unix timestamp of when drift was first detected for this job. Absent when no drift is present. `time() - metric` gives how long the job has been drifting — use this to distinguish a deploy in progress from a job that's been stuck for hours. |

### Diff checks

These counters and timestamps describe the diff check loop itself — how often it
runs and whether it is working correctly.

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_diff_checks_total` | Counter | — | Total diff checks run since startup. Use `rate()` to confirm the loop is running at the expected frequency. |
| `nomad_botherer_diff_checks_skipped_total` | Counter | — | Checks skipped because neither the Nomad Raft index nor the git commit changed since the last run. A high skip rate is normal and indicates the optimisation is working. |
| `nomad_botherer_last_check_timestamp_seconds` | Gauge | — | Unix timestamp of the most recent completed diff check. Alert when `time() - metric` exceeds 2× `--diff-interval` to catch a stuck check loop. |
| `nomad_botherer_nomad_api_errors_total` | Counter | `op` (`info`, `plan`, `list`, `register`, `deregister`, `versions`, `deployments`, `deployment`, `revert`, `tag`) | Nomad API call failures by operation. `info` = job lookup, `plan` = drift plan, `list` = listing all jobs, `register`/`deregister`/`revert` = apply-side writes, `versions`/`deployments`/`deployment` = rollback and flap-guard reads, `tag` = flap-guard version tagging. A rising count means results may be incomplete for that operation. |
| `nomad_botherer_hcl_parse_errors_total` | Counter | — | HCL files that failed to parse via the Nomad API. These files are skipped; the rest of the check continues. |
| `nomad_botherer_hcl_non_job_files_skipped_total` | Counter | — | HCL files that were skipped because they contain no `job` stanza (e.g. ACL policies, volumes). Expected and normal; a rising rate may indicate `--hcl-dir` is set too broadly. |
| `nomad_botherer_jobs_skipped_by_selector_total` | Counter | `source` (`hcl`, `nomad`) | Jobs skipped because they did not match the selection criteria (glob or managed meta key), by where they were seen. Expected on a shared cluster with unmanaged jobs. |
| `nomad_botherer_diff_fields_redacted_total` | Counter | — | Plan-diff field values replaced with `[REDACTED]` before storage (only when `--redact-secrets` is on). A rising count means drifted jobs have changes in env vars, templates, or secret-like fields. |
| `nomad_botherer_updates_blocked_by_policy_total` | Counter | `job`, `policy` | Diffs that would have produced a JobUpdate but were filtered out by the effective update policy. Watch this to find jobs accumulating unapplied drift. |
| `nomad_botherer_updates_blocked_creation_disabled_total` | Counter | `job` | First-time registrations blocked because `--enable-job-creation` is off. |
| `nomad_botherer_job_updates_total` | Counter | `operation`, `status` | JobUpdates reaching a terminal state (`SUCCEEDED`, `FAILED`, `SUPERSEDED`). Operation is `REGISTER`, `DEREGISTER`, or `REVERT`. |
| `nomad_botherer_job_updates_pending` | Gauge | — | Updates currently waiting to be applied. |
| `nomad_botherer_meta_key_issues_total` | Counter | `job`, `issue` | Job meta keys under the managed prefix that nomad-botherer cannot act on: `unknown_key` (e.g. a typo like `gitops_managd` or `gitops.managed`) or `invalid_value` (a recognised key with an unusable value, e.g. `gitops_managed = "True"`). Counted every cycle the issue persists; logged once per unique issue (WARN for unknown keys, ERROR for bad values). |
| `nomad_botherer_meta_key_changes_total` | Counter | `job`, `source` | Managed-prefix meta keys added, removed, or changed between check cycles, on the HCL side (a commit changed them) or the live side (someone re-registered the job manually). Each transition is also logged at INFO with the behavioural consequence. |
| `nomad_botherer_meta_only_diffs_total` | Counter | `job` | Diffs confined to nomad-botherer's own meta keys, detected per check cycle. By default these are neither counted as drift nor applied (`--count-meta-only-changes`, `--apply-meta-only-changes`); they converge on the next real update. A non-zero rate is normal after opting a running job in via a commit. |
| `nomad_botherer_updates_blocked_preexisting_total` | Counter | `job` | Updates not enqueued because the drift pre-dated a scope change that brought it in — the job's opt-in (managed tag added) or a policy widening (e.g. `image-only` → `full`). Enable applying it with `--apply-existing-drift`. |
| `nomad_botherer_jobs_left_management_total` | Counter | `job`, `reason` | Managed jobs that left GitOps management, by reason: `tag_removed` (the managed tag was dropped from HCL) or `removed_from_repo` (HCL file deleted or job renamed). Counted once per transition. See [Deregistration](applying-changes.md#deregistration-jobs-removed-from-the-repo). |
| `nomad_botherer_updates_blocked_known_failed_total` | Counter | `job` | Registrations withheld by the flap-loop guard because the spec matches a recent failed deployment. A persistent non-zero value means a job is stuck on a known-bad commit awaiting a fix in Git. See [Rollback](rollback.md). |
| `nomad_botherer_rollbacks_total` | Counter | `job`, `result` | Active-rollback outcomes: `queued` (a revert was enqueued), `deferred_auto_revert` (stood down because the job sets `auto_revert`), `no_stable_version` (no earlier stable version to revert to). See [Rollback](rollback.md). |
| `nomad_botherer_failed_versions_tagged_total` | Counter | `job` | Failed versions tagged by `--flap-guard=tag` so the block survives Nomad's version GC. |
| `nomad_botherer_nomad_token_refreshes_total` | Counter | `result` | Re-reads of the Nomad token file (workload identity): `rotated` (the token changed and was applied) or `error` (the file could not be read; previous token kept). Only moves when a token file is in use — an explicit `--nomad-token-file` or the auto-detected `${NOMAD_SECRETS_DIR}/nomad_token`. See [Nomad access](setup/nomad-access.md). |

### Git tracking

These metrics describe the in-memory git clone and polling loop.

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_git_fetches_total` | Counter | — | Total remote fetch/clone attempts. Each poll interval triggers one. |
| `nomad_botherer_git_fetch_errors_total` | Counter | — | Fetch/clone attempts that failed. A rising count means new commits are not being picked up; diff checks continue against the last known commit. |
| `nomad_botherer_git_last_update_timestamp_seconds` | Gauge | — | Unix timestamp of the last successful fetch. Alert when `time() - metric` is significantly larger than `--poll-interval` to catch a stuck git loop. |

### Webhooks

These metrics describe incoming webhook events from GitHub.

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_webhook_events_total` | Counter | `event` (`push`, `ping`, `unknown`, `error`) | Webhook events received by type. `push` events trigger an immediate fetch. `error` events indicate a failed delivery (bad signature, parse error, etc.). |
| `nomad_botherer_last_webhook_success_timestamp_seconds` | Gauge | — | Unix timestamp of the last successfully processed webhook. Zero if no webhook has been received yet. |
| `nomad_botherer_last_webhook_failure_timestamp_seconds` | Gauge | — | Unix timestamp of the last failed webhook delivery. Zero if no failure has occurred. |

### Staleness checking

These counters are only non-zero when `--max-git-staleness` or
`--max-nomad-staleness` is configured.

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_git_staleness_refreshes_total` | Counter | — | Git fetches triggered because `time() - nomad_botherer_git_last_update_timestamp_seconds` exceeded `--max-git-staleness`. A rising count means the normal polling or webhook path is not keeping the repo current. |
| `nomad_botherer_nomad_staleness_checks_total` | Counter | — | Nomad diff checks triggered because `time() - nomad_botherer_last_check_timestamp_seconds` exceeded `--max-nomad-staleness`. A rising count means the normal diff loop is falling behind. |

### Service info

| Metric | Type | Labels | What it tells you |
|--------|------|--------|-------------------|
| `nomad_botherer_info` | Gauge | `version` | Always 1. The `version` label holds the build version string. Useful for tracking rollouts: `count by(version)(nomad_botherer_info)`. |

## Sample Prometheus configuration

The [`monitoring/`](../monitoring/) directory contains ready-to-use
configuration files:

| File | Contents |
|------|----------|
| [`monitoring/prometheus.yml`](../monitoring/prometheus.yml) | Scrape configuration for nomad-botherer |
| [`monitoring/recording_rules.yml`](../monitoring/recording_rules.yml) | Pre-aggregated series for dashboards and alerts |
| [`monitoring/alerts.yml`](../monitoring/alerts.yml) | Alerting rules covering drift, service health, git, and webhooks |

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
