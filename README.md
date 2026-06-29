# nomad-botherer

[![Tests](https://github.com/gerrowadat/nomad-botherer/actions/workflows/test.yml/badge.svg)](https://github.com/gerrowadat/nomad-botherer/actions/workflows/test.yml)
[![Coverage](https://raw.githubusercontent.com/wiki/gerrowadat/nomad-botherer/coverage.svg)](https://raw.githack.com/wiki/gerrowadat/nomad-botherer/coverage.html)

Watches a remote git repo for Nomad job HCL definitions and continuously
compares them against a live Nomad cluster. When drift is detected it logs,
exposes Prometheus metrics, and reports details on `/healthz` — and, when
explicitly enabled per job or per deployment, applies the change by
re-registering the job from its HCL. **Out of the box it never writes anything.**

Three kinds of drift are tracked:

| Diff type | Meaning |
|-----------|---------|
| `modified` | Job exists in both HCL and Nomad but the definitions differ (detected via `nomad job plan`) |
| `missing_from_nomad` | Job defined in HCL but not currently registered in Nomad (dead jobs count as missing by default) |
| `missing_from_hcl` | Job registered and running in Nomad but has no HCL file in the repo (dead jobs are excluded by default) |

## Quick start

Run it against a repo of HCL job files and a cluster:

```bash
docker run -d -p 8080:8080 \
  -e GIT_REPO_URL=https://github.com/myorg/nomad-jobs.git \
  -e NOMAD_ADDR=http://nomad.example.com:4646 \
  ghcr.io/gerrowadat/nomad-botherer:latest
```

Then opt a job in to monitoring by adding one line to its HCL and committing:

```hcl
job "my-service" {
  meta {
    gitops_managed = "true"
  }
  # ...
}
```

`curl http://localhost:8080/healthz` shows the drift summary; `/metrics` exposes
it to Prometheus. That is the whole default behaviour — detect and report, never
write. See [Running nomad-botherer](docs/setup/running.md) for the binary and
in-cluster (Nomad job) deployments, and [Common use cases](docs/use-cases.md) to
go further.

## How it works

1. On startup the repo is cloned entirely into memory using
   [go-git](https://github.com/go-git/go-git).
2. All `.hcl` files under `--hcl-dir` (default: repo root) are sent to Nomad's
   `/v1/jobs/parse` endpoint to produce canonical `Job` structs.
3. Each parsed job is checked against the configured
   [selection criteria](docs/job-selection.md). Jobs matching neither the glob
   nor the managed meta key are ignored.
4. For each selected job: if it is not registered (or is `dead`) →
   `missing_from_nomad`; if it is live, `nomad job plan` runs → changes →
   `modified`.
5. Running Nomad jobs (non-dead) that match selection but have no HCL file →
   `missing_from_hcl`. Dead jobs are excluded by default (`--include-dead-jobs`
   to include them).
6. Results are exposed via `/healthz` (JSON), `/metrics` (Prometheus), and the
   authenticated [JSON API](docs/json-api.md).
7. Each actionable diff is checked against the job's effective
   [update policy](docs/applying-changes.md) (HCL meta key, falling back to
   `--default-update-policy`, default `none`). Diffs the policy allows are
   applied by a separate loop — plan-first and CAS-protected. With the defaults
   nothing is applied.
8. The repo is re-checked on every `--poll-interval` (git fetch), every
   `--diff-interval` (Nomad-side drift), and immediately on a
   [webhook](docs/setup/webhooks.md) push or `POST /api/v1/refresh`.

nomad-botherer is a **drift detector first and a GitOps operator second**, and
deliberately conservative: every write is opt-in, Git is always the source of
truth, and it holds no persistent state of its own. See
[Design philosophy](docs/philosophy.md).

## Documentation

The full docs live in [`docs/`](docs/README.md):

- **Setup** — [installation](docs/setup/installation.md),
  [running](docs/setup/running.md) (Nomad job / binary / Docker),
  [git access](docs/setup/git-access.md),
  [Nomad access](docs/setup/nomad-access.md) (workload identity / token),
  [webhooks](docs/setup/webhooks.md).
- **Using it** — [common use cases](docs/use-cases.md),
  [configuration](docs/configuration.md),
  [meta-key reference](docs/meta-keys.md), [job selection](docs/job-selection.md),
  [applying changes](docs/applying-changes.md), [rollback](docs/rollback.md),
  [monitoring](docs/monitoring.md), [JSON API](docs/json-api.md).
- **Understanding it** — [FAQ & gotchas](docs/faq.md),
  [design philosophy](docs/philosophy.md), [prior art](docs/prior-art.md),
  [design records](docs/design/).
- **Contributing** — [development & testing](docs/development.md).

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
