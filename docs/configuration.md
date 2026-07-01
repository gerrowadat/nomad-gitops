# Configuration

Every flag has a corresponding environment variable. Environment variables are
read at startup; flags override them when explicitly passed.

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--repo-url` | `GIT_REPO_URL` | *(required)* | Remote git repo URL |
| `--branch` | `GIT_BRANCH` | `main` | Branch to watch |
| `--poll-interval` | `POLL_INTERVAL` | `5m` | How often to poll git for changes |
| `--hcl-dir` | `HCL_DIR` | *(repo root)* | Subdirectory containing HCL job files |
| `--git-token` | `GIT_TOKEN` | | HTTP token for private repos (GitHub PAT etc.). Requires an `https://` repo URL; refused for plain `http://` URLs, which would send the token in cleartext. |
| `--git-ssh-key` | `GIT_SSH_KEY` | | Path to SSH private key |
| `--git-ssh-key-password` | `GIT_SSH_KEY_PASSWORD` | | SSH key passphrase |
| `--git-ssh-known-hosts` | `GIT_SSH_KNOWN_HOSTS` | `~/.ssh/known_hosts` | Path to known_hosts file for SSH host key verification; required when using SSH auth. Defaults to the system known_hosts locations. Omit to allow the default search, or set explicitly to a specific file. |
| `--nomad-addr` | `NOMAD_ADDR` | `http://127.0.0.1:4646` | Nomad API address |
| `--nomad-token` | `NOMAD_TOKEN` | | Nomad ACL token (static SecretID). For manual running and testing. Does not refresh; under Nomad use workload identity. See [Nomad access](setup/nomad-access.md). |
| `--nomad-token-file` | `NOMAD_TOKEN_FILE` | | Path to a file holding a Nomad ACL token **SecretID** (not a WI JWT), re-read periodically so a rotating token stays current. Takes precedence over `--nomad-token`. See [Nomad access](setup/nomad-access.md). |
| `--nomad-token-poll-interval` | `NOMAD_TOKEN_POLL_INTERVAL` | `30s` | How often to re-read `--nomad-token-file` for a rotated token. |
| `--nomad-login-auth-method` | `NOMAD_LOGIN_AUTH_METHOD` | | Enable Nomad **workload-identity login**: name of the JWT auth method to exchange the identity JWT for an ACL token via `/v1/acl/login`, re-exchanged before expiry. The working way to use workload identity — a raw WI JWT is rejected by `Job.Plan`. See [Nomad access](setup/nomad-access.md#workload-identity-recommended-under-nomad). |
| `--nomad-login-jwt-file` | `NOMAD_LOGIN_JWT_FILE` | `${NOMAD_SECRETS_DIR}/nomad_token` | Path to the workload-identity JWT to exchange (login mode). Point it at a named identity's file (`nomad_<name>.jwt`) when the auth method audience does not match the default identity. |
| `--nomad-namespace` | `NOMAD_NAMESPACE` | `default` | Nomad namespace |
| `--listen-addr` | `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `--webhook-secret` | `WEBHOOK_SECRET` | | GitHub webhook HMAC secret |
| `--webhook-path` | `WEBHOOK_PATH` | `/webhook` | Webhook endpoint path |
| `--api-key` | `API_KEY` | *(empty — disabled)* | Pre-shared key for `/api/` endpoints (Bearer token). Empty disables the JSON API. |
| `--diff-interval` | `DIFF_INTERVAL` | `1m` | Periodic Nomad-side drift check interval |
| `--include-dead-jobs` | `INCLUDE_DEAD_JOBS` | `false` | Treat dead Nomad jobs like running ones (by default dead jobs count as missing) |
| `--redact-secrets` | `REDACT_SECRETS` | `true` | Redact potentially sensitive plan-diff values before they are stored or rendered. Env vars, template bodies, and fields with secret-like names (`password`, `token`, `secret`, ...) are shown as `[REDACTED]`; the diff structure and field names are kept. Set to `false` to show real values. |
| `--default-update-policy` | `DEFAULT_UPDATE_POLICY` | `none` | Update policy for managed jobs without an explicit `<prefix>_update_policy` meta key: `none` (detect only), `image-only`, or `full`. See [Applying changes](applying-changes.md). |
| `--enable-job-creation` | `ENABLE_JOB_CREATION` | `false` | Allow first-time registration of jobs that exist in Git but not in Nomad. Requires an effective policy of `full` for the job. |
| `--apply-interval` | `APPLY_INTERVAL` | `10s` | Fallback cadence of the apply loop; enqueued updates are also applied immediately. |
| `--apply-meta-only-changes` | `APPLY_META_ONLY_CHANGES` | `false` | Apply a diff whose only change is to nomad-botherer's own meta keys (e.g. `gitops_managed`). Off by default — re-registering a running job just to push these keys is disruptive and unnecessary; they ride along the next real update. |
| `--count-meta-only-changes` | `COUNT_META_ONLY_CHANGES` | `false` | Count a managed-meta-only diff as drift (surface it on `/diffs`, `/healthz`, and the drift metrics). Off by default so these expected differences do not trigger alerts. |
| `--apply-existing-drift` | `APPLY_EXISTING_DRIFT` | `false` | When a change widens a job's scope, apply drift that already existed at that moment. Scope widens two ways, treated identically: a job gains the managed meta tag (enablement), or its update policy is widened to cover drift it was deferring (e.g. `image-only` → `full`). Off by default — a scope change does not retroactively mutate the job; only changes committed after it apply. See [Drift that pre-dates a scope change](applying-changes.md#drift-that-pre-dates-a-scope-change-opt-in-or-policy-widening). |
| `--enable-deregister` | `ENABLE_DEREGISTER` | `false` | Deregister jobs removed from the repo entirely (HCL file deleted or job renamed) while still running. Off by default. Only acts on a live job carrying `gitops_managed=true` whose effective policy is `full`, and only after it has been orphaned for `--deregister-grace`. Removing only the tag (job still in the repo) never deregisters. See [Deregistration](applying-changes.md#deregistration-jobs-removed-from-the-repo). |
| `--deregister-purge` | `DEREGISTER_PURGE` | `false` | Purge the job from Nomad's state immediately instead of a graceful stop (queryable, GC'd later). |
| `--deregister-grace` | `DEREGISTER_GRACE` | `5m` | How long a job must be continuously orphaned before being deregistered. Absorbs transient renames and mid-edit commits. |
| `--flap-guard` | `FLAP_GUARD` | `history` | How to avoid re-applying a spec a recent deployment already failed (the apply→fail→revert→re-apply loop): `history` (compare spec fingerprints against Nomad's version history; ephemeral, lost when Nomad GCs old versions), `tag` (additionally tag the failed version so the block survives GC; requires a non-empty `--managed-meta-prefix`, since tag names derive from it), or `off`. Per-job overridable via `<prefix>_flap_guard`. Only applies to deployment-producing jobs. See [Rollback](rollback.md). |
| `--allow-rollback` | `ALLOW_ROLLBACK` | `false` | Enable active rollback: for managed deployment-producing jobs whose `update` stanza does not set `auto_revert`, revert to the last stable version when a deployment fails. Off by default. Per-job overridable via `<prefix>_rollback`. Where the job sets `auto_revert`, Nomad's own rollback wins. See [Rollback](rollback.md). |
| `--job-selector-glob` | `JOB_SELECTOR_GLOB` | *(empty — no glob)* | Glob pattern selecting jobs to watch by name (e.g. `myprefix-*`, `*` for all). Combined with `--managed-meta-prefix` as a union. |
| `--managed-meta-prefix` | `MANAGED_META_PREFIX` | `gitops` | Prefix for job meta keys used by nomad-botherer. With prefix `gitops`, the key `gitops_managed = "true"` opts a job in. Empty disables meta-based selection. |
| `--max-git-staleness` | `MAX_GIT_STALENESS` | `0` (disabled) | If the git repo has not been successfully fetched within this window, force an immediate fetch. Set to `0` to disable. E.g. `--max-git-staleness=30m` |
| `--max-nomad-staleness` | `MAX_NOMAD_STALENESS` | `0` (disabled) | If the Nomad diff check has not run within this window, force an immediate check. Set to `0` to disable. E.g. `--max-nomad-staleness=10m` |
| `--log-level` | `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |

Logs are written to stderr as JSON (structured via `log/slog`).

## Per-job meta keys

Some behaviour is set per job in its HCL `meta {}` block rather than by a flag —
`gitops_managed`, `gitops_update_policy`, `gitops_flap_guard`, and
`gitops_rollback`. The **[Meta-key reference](meta-keys.md)** is the canonical
list of every key, its valid values, and what it overrides.
