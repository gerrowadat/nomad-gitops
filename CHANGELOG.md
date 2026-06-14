# Changelog

## Unreleased

### New features

- **Deregistration of jobs removed from the repo, and clear logging when a
  job leaves GitOps management.** A managed job leaves scope two ways, both
  logged: the `gitops_managed` tag is removed (job still in the repo — it is
  left running, never deregistered, logged via the meta-change tracker), or
  the job is removed from the repo entirely (file deleted or renamed). The
  latter is surfaced as `missing_from_hcl` and, by default, left running
  (`observation_only`). `--enable-deregister` / `ENABLE_DEREGISTER` (default
  off) deregisters it, but only when the live job carries
  `gitops_managed=true`, its effective policy is `full`, and it has been
  continuously orphaned for `--deregister-grace` / `DEREGISTER_GRACE`
  (default `5m`); live state is re-checked immediately before the call.
  Deregistration is a graceful stop by default; `--deregister-purge` /
  `DEREGISTER_PURGE` purges. New `apply_action` values `queued_deregister`
  and `deregister_pending_grace`; new counter
  `nomad_botherer_jobs_left_management_total{job,reason}`; `DEREGISTER`
  appears in `nomad_botherer_job_updates_total`.

## v0.6.0 — 2026-06-14

Refinements to the GitOps apply side introduced in v0.5.0. All defaults stay
conservative (detection-only).

### New features

- **Changes confined to nomad-botherer's own meta keys are not, on their
  own, drift.** When a commit adds or changes only a `gitops_*` key on a
  running job, that diff is neither applied nor counted as drift by default:
  re-registering a job purely to stamp our keys onto it is disruptive and
  needless, since the HCL is already authoritative for them. The keys
  converge opportunistically on the next real update (an image bump under
  `image-only` carries them along). Two new flags control this independently:
  `--apply-meta-only-changes` / `APPLY_META_ONLY_CHANGES` (default off) and
  `--count-meta-only-changes` / `COUNT_META_ONLY_CHANGES` (default off).
  Surfaced by the new `nomad_botherer_meta_only_diffs_total{job}` counter and
  the meta-change logs. A diff mixing a meta change with any other change is
  unaffected.
- **Drift that pre-existed a job entering scope is not applied by default.**
  When the managed tag is added to a job that already differs from its HCL
  (e.g. an image bumped in Git before the tag), that drift is not
  retroactively applied — only changes committed after opt-in are.
  `--apply-existing-drift` / `APPLY_EXISTING_DRIFT` (default off) applies it
  instead. The decision is derived from git history (was the tag present in
  the commit before HEAD for the job's file?), so it holds identically
  whether the tag was added while running or before startup — a restart never
  freezes an already-managed cluster. A file created with the tag in one
  commit is not a retroactive opt-in and applies. Glob-selected jobs have no
  opt-in moment and are unaffected. Counted in
  `nomad_botherer_updates_blocked_preexisting_total{job}`.
- **Every diff carries an `apply_action` explaining whether and why it will
  (not) be applied** — `queued`, `blocked_by_policy`,
  `blocked_preexisting_drift`, `blocked_creation_disabled`,
  `skipped_meta_only`, `observation_only`, or `no_actionable_change`. Shown on
  `/diffs`, in the `/api/v1/diffs` and `/healthz` JSON, and in the OpenAPI
  spec.

### Fixed

- **Update-queue race on re-enqueue of an in-flight update.** Re-enqueuing an
  update with the same ID while it was `IN_PROGRESS` mutated the in-flight
  update's fields, which the applier reads without holding the queue lock.
  In-progress updates are now left strictly untouched.

## v0.5.0 — 2026-06-13

### Breaking changes

- **Git is always the source of truth for nomad-botherer's own behaviour,
  and `--managed-meta-hcl-canonical` / `MANAGED_META_HCL_CANONICAL` is
  removed** (passing the flag is now a startup error). When a job has an
  HCL file in the repo, that file alone decides selection and update
  policy, in both directions:
  - `gitops_managed = "true"` in HCL selects the job even when the running
    job's meta does not carry it; the missing live key is itself drift and
    converges through the normal apply path (policy permitting). Opting a
    running job in is a single commit — previously (v0.3.0 behaviour,
    where live meta was the source of truth) adding the key in Git did
    nothing until someone manually re-registered the job.
  - A stale `gitops_managed` key on a live job whose HCL does *not* carry
    it never selects the job. Previously the live key kept such jobs
    selected and they were misreported as `missing_from_hcl`.

  Live meta only drives behaviour for jobs Git knows nothing about
  (`missing_from_hcl` detection). Live-side key changes on jobs with HCL
  are still noticed, logged, and counted — they just never change
  behaviour.

### New features

- **GitOps apply: nomad-botherer can now mutate jobs.** When drift is
  detected for a managed job, it can re-register the job from its HCL —
  implementing the async-queue design from
  `docs/proposals/gitops-job-updates.md` and the policy model from
  `docs/proposals/update-policies.md`. Everything defaults to
  detection-only:
  - Per-job update policies: `none` (default), `image-only` (apply only
    when the entire plan diff is Docker image fields), `full`. Set the
    default with `--default-update-policy` / `DEFAULT_UPDATE_POLICY`;
    override per job with the `gitops_update_policy` meta key in HCL.
  - First-time registration of jobs missing from Nomad is additionally
    gated on `--enable-job-creation` / `ENABLE_JOB_CREATION` (default off)
    and requires policy `full`.
  - Every write is plan-first and CAS-protected (`EnforceIndex` with the
    `JobModifyIndex` captured at detection); conflicts mark the update
    `FAILED` and the next cycle re-detects. Autoscaled groups register
    with `PreserveCounts`, and autoscaler-owned Count/Scaling churn never
    triggers an update.
  - Updates flow through an in-memory queue drained by a separate apply
    loop (`--apply-interval` fallback cadence); newer commits supersede
    pending updates for the same job. The queue is visible at
    `GET /api/v1/updates` and in four new metrics
    (`nomad_botherer_job_updates_total`, `..._job_updates_pending`,
    `..._updates_blocked_by_policy_total`,
    `..._updates_blocked_creation_disabled_total`).
  - Deregistration (`missing_from_hcl`) remains observation-only.
  - The web console index shows the apply mode (default policy, job
    creation flag, pending update count), and the regression suite gains
    end-to-end apply scenarios against a real cluster, including the
    negative test that the defaults never write.
  - Meta keys under the managed prefix that nomad-botherer cannot act on
    are flagged: unknown keys (typos like `gitops_managd` or
    `gitops.managed`) log at WARN, recognised keys with unusable values
    (`gitops_managed = "True"`) log at ERROR. Logged once per unique
    issue; counted every cycle in
    `nomad_botherer_meta_key_issues_total{job,issue}`.
  - Changes to managed-prefix meta keys are noticed and logged with their
    behavioural consequence: opting a job in or out of management,
    switching update policy (including falling back to the default when
    the key is removed), and live jobs losing keys to a manual
    `nomad job run`. Logged at INFO with old/new values and what the tool
    does to honour the change; counted in
    `nomad_botherer_meta_key_changes_total{job,source}`. The first check
    after startup is a silent baseline.

### Changed

- **Test coverage raised from 78% to 88% of statements.** `internal/config`
  and `internal/nomad` are at 100%, `internal/server` at 99%, and
  `internal/gitwatch` went from 66% to 92% — Clone, pull, and Run are now
  exercised against real on-disk git repositories instead of only mocks.
  No production code changed.

### Fixed

- **Regression suite runs alongside a real Nomad agent.** The Docker-managed
  test Nomad uses host networking on Linux but only pinned the HTTP port,
  leaving RPC (4647) and serf (4648) at their defaults — so the suite failed
  to start on any host already running Nomad. All three ports are now pinned
  to free ports, and the test agent binds to loopback only so it is not
  exposed on the LAN.

## v0.4.0 — 2026-06-11

### Security

- **Dependency updates for known vulnerabilities.** Go toolchain 1.25.6 →
  1.25.11 and `golang.org/x/crypto` v0.50.0 → v0.52.0. `govulncheck` reported
  21 vulnerabilities reachable from this codebase at the old versions
  (stdlib `net/http`, `crypto/tls`, `crypto/x509`, `html/template`, and the
  x/crypto SSH code used for git auth); it reports none after the upgrade.
- **Webhook request bodies are capped at 25 MB** (GitHub's own payload limit).
  Previously the body was read into memory without limit, allowing memory
  exhaustion via a single large request.
- **`--git-token` is now refused with a plain `http://` repo URL**, which
  would send the token in cleartext. Use `https://` or SSH.
- **API key comparison no longer leaks the key length.** Both sides of the
  bearer-token check are SHA-256 hashed before the constant-time compare.
- **Hardening headers on all HTTP responses**: `X-Content-Type-Options:
  nosniff`, `X-Frame-Options: DENY`, a restrictive `Content-Security-Policy`,
  and `Referrer-Policy: no-referrer`.
- **Plan diffs redact potentially sensitive values by default.** Env vars,
  template bodies, and fields with secret-like names (`password`, `token`,
  `secret`, ...) are replaced with `[REDACTED]` and annotated
  `(value redacted)` before the diff is stored, so `/diffs` never shows them.
  The diff structure and field names are preserved, and the `/diffs` output
  carries a banner saying redaction is active. Controlled by
  `--redact-secrets` / `REDACT_SECRETS` (default `true`); redactions are
  counted in `nomad_botherer_diff_fields_redacted_total`.

### Breaking changes

- **gRPC API and `nbctl` CLI removed.** The gRPC server, proto definitions,
  generated bindings, and the `nbctl` CLI are gone. The `--grpc-listen-addr`
  and `--grpc-api-key` flags are removed.

### New features

- **JSON API** (`/api/v1/`): a plain HTTP/JSON API replaces the gRPC server.
  Enable it by setting `--api-key` / `API_KEY`. All endpoints require
  `Authorization: Bearer <key>`. Available endpoints:
  - `GET /api/v1/diffs` — current job diffs
  - `GET /api/v1/selected-jobs` — jobs selected for monitoring and why
  - `GET /api/v1/status` — git watcher status
  - `GET /api/v1/version` — build version / commit / date
  - `POST /api/v1/refresh` — trigger immediate git pull
  - `GET /api/openapi.json` — OpenAPI 3.0 spec (public, no auth required)

## v0.3.0 — 2026-06-02

### Breaking changes

- **Nomad job meta is now the source of truth for managed-meta-prefix selection.**
  Previously, if the HCL file for a job declared `gitops_managed = "true"` but
  the running Nomad job did not carry that key, the job was still selected and
  diffed. Now the live Nomad job's meta is checked instead. A job is only
  selected if the running job carries `gitops_managed = "true"` (or whichever
  key the configured prefix produces).

  The HCL meta is used as a fallback for jobs that do not yet exist in Nomad
  (so new jobs are still detected as `missing_from_nomad`).

  To restore the previous behaviour, set `--managed-meta-hcl-canonical`
  (`MANAGED_META_HCL_CANONICAL=true`).

## v0.2.0 — 2026-06-02

### Breaking changes

- **Meta key separator changed from `.` to `_`.** The opt-in meta key is now
  `gitops_managed = "true"` (previously `"gitops.managed" = "true"`). This
  makes the key a valid HCL2 identifier, allowing the cleaner block form:

  ```hcl
  meta {
    gitops_managed = "true"
  }
  ```

  instead of the object-expression form with quoted keys. Any existing job HCL
  using the old dotted key must be updated before upgrading. Jobs using the
  previous key format will no longer be selected after this change.

  The custom prefix configured via `--managed-meta-prefix` / `MANAGED_META_PREFIX`
  works the same way: a prefix of `myorg` now produces `myorg_managed`. When
  choosing a custom prefix, keeping `gitops` as a root (e.g. `gitops_myteam`)
  is recommended so all nomad-botherer keys remain visually grouped.

## v0.1.2 — 2026-06-02

### Security fixes

- Fixed three security bugs introduced in earlier releases:
  - SSH host key verification was disabled by default, allowing MITM attacks on git clones over SSH. Host key checking is now on by default; the `--git-ssh-known-hosts` flag lets you point at a custom known_hosts file.
  - The HTTP server had no read/write timeouts, leaving it open to slowloris-style connection exhaustion. Timeouts are now applied.
  - Webhook signatures were not verified when no `--webhook-secret` was configured, accepting any POST as a valid push event. Webhooks are now rejected if no secret is configured.

### New features

- **Job selection** (`--job-selector-glob`, `--managed-meta-prefix`): Two independent mechanisms for scoping which jobs nomad-botherer watches. `--job-selector-glob` selects by name pattern (e.g. `myteam-*`); `--managed-meta-prefix` selects jobs that carry a meta key with the given prefix (default `gitops`, meaning `gitops.managed = "true"` opts a job in — note: renamed to `gitops_managed` in v0.2.0). Jobs matching either selector are watched; jobs matching neither are ignored.
- **gRPC server disabled by default**: The gRPC server no longer binds to `:9090` on startup. Set `--grpc-listen-addr` (or `GRPC_LISTEN_ADDR`) to enable it. This avoids unexpected port conflicts and makes the `--grpc-api-key` requirement easier to enforce.

### Correctness

- Replaced empirical Nomad API workarounds with behaviour documented in the Nomad source and HTTP API:
  - Job list calls now use `?meta=true` (documented query parameter) to retrieve job meta in the list response, removing a redundant per-job `Info()` call.
  - Jobs stopped via Nomad's deregister-without-purge set `Stop=true` on the job record. The differ now copies this field onto the parsed HCL job before planning, preventing a spurious `Stop` field diff.
  - `hasContentDiff` now recognises `Type="None"` (defined as `DiffTypeNone` in `nomad/structs/diff.go`) as a no-op task group result, avoiding false positives on plan responses for unchanged jobs.

### Testing

- Added a full regression test suite (`tests/regression/`) covering drift detection, end-to-end HTTP and gRPC flows, Prometheus metrics, webhook handling, security behaviours, and job selection. The suite runs against a real Nomad instance and is tagged `//go:build regression` so it does not run in CI by default. See the Testing section of the README for how to run it.
- Verified against Nomad 1.9.6, 1.10.5, 1.11.3, and 2.0.2. All tests pass on all four versions.

### Documentation

- Added a Getting Started section to the README with a minimal working example.
- Added `examples/nomad-botherer.hcl`: a commented Nomad job definition for running nomad-botherer on a Nomad cluster, covering all configuration options.
- Added design intent documentation: `docs/proposals/` covers the planned apply side and change checkpointing; `docs/prior-art.md` surveys existing tooling and the problems nomad-botherer is designed to avoid.

### Dependencies and tooling

- Updated Go to 1.25.6.
- Switched protobuf code generation from `protoc` + `arduino/setup-protoc` to `buf`. Added a CI check for proto drift.
- Bumped `github.com/go-git/go-git/v5` to 5.19.1.
- Bumped `google.golang.org/grpc` to 1.81.1.
- Updated GitHub Actions: `actions/checkout` v6, `actions/setup-go` v6, `docker/build-push-action` v7, `docker/login-action` v4, `docker/metadata-action` v6, `docker/setup-buildx-action` v4, `docker/setup-qemu-action` v4.

---

## v0.1.1 — 2026-05-11

- Added gRPC API (`GetDiffs`, `GetStatus`, `TriggerRefresh`, `GetVersion`) with API key authentication.
- Added `nbctl` CLI for interacting with the gRPC API.
- Added Prometheus metrics endpoint (`/metrics`).
- Added webhook endpoint for GitHub push events (`/webhook`).
- Added staleness checks (`--max-git-staleness`, `--max-nomad-staleness`).
- Documentation and Docker publishing improvements.

## v0.1.0 — 2026-05-10

- Initial release. HTTP server with `/healthz`, `/diffs`, and `/` endpoints. Git polling and Nomad diff detection.
