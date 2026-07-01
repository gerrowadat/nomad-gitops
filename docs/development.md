# Development

How to build, test, and release nomad-botherer. For using it, start at the
[documentation index](README.md).

## Local development with .env

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

## Build and test

```bash
make build        # compile nomad-botherer
make install      # go install to $GOPATH/bin
make test         # go test -race ./...
make test-cover   # run tests and generate coverage.html
make fmt          # gofmt -w .
make lint         # gofmt check + go vet ./...
make clean        # remove build artefacts
```

`make lint` fails if any file is not gofmt-clean, then runs `go vet`. CI runs the
same `make lint` on every push and PR, so keep the tree formatted (`make fmt`).

## Simulating a webhook

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
HMAC-SHA256 signature (using `openssl`). If no secret is set, the request is sent
unsigned.

## Regression tests

The regression suite lives in `tests/regression/` and is excluded from normal
`go test ./...` runs by the `//go:build regression` build tag. It starts a real
Nomad cluster (via Docker or a pre-existing address) and exercises the full
request path: drift detection, job selection, Prometheus metrics, HTTP and JSON
API endpoints, webhook HMAC verification, the GitOps apply side, Nomad auth, and
the compiled binary's startup lifecycle.

Run it before cutting a release to verify that the build behaves correctly
against a real cluster.

### Prerequisites

- **Docker** — the suite pulls and starts `hashicorp/nomad:<version>`
  automatically. The container runs with `--privileged` to allow Nomad's
  `raw_exec` driver (used by test jobs) to manage cgroups.
- **Go 1.25+**

### Running against a Docker-managed Nomad

```bash
make test-regression
```

This pulls the default Nomad image, starts a dev-mode cluster, runs all tests,
and stops the container on exit. The full suite takes roughly 5–10 minutes.

On Linux the container uses host networking with the agent's HTTP, RPC, and serf
ports pinned to randomly chosen free ports and bound to loopback, so the suite
runs cleanly alongside a real Nomad agent on the same host (no clash with
4646/4647/4648) and is never reachable from the LAN.

### Targeting a specific Nomad version

```bash
NOMAD_VERSION=1.11.3 make test-regression
```

Or directly:

```bash
NOMAD_VERSION=1.11.3 go test -tags=regression -timeout 15m -v -count=1 ./tests/regression/...
```

`NOMAD_VERSION` must match a tag on the official
[`hashicorp/nomad`](https://hub.docker.com/r/hashicorp/nomad/tags) Docker image.

### Testing against multiple versions

```bash
make test-regression-versions NOMAD_VERSIONS="1.9.6 1.10.5 1.11.3 2.0.2"
```

This iterates over the list and runs the full suite against each version in
sequence, stopping on the first failure.

### Using an existing cluster

If you already have a Nomad cluster running, point the suite at it instead of
starting Docker:

```bash
NOMAD_ADDR=http://my-nomad.internal:4646 make test-regression
```

When `NOMAD_ADDR` is set, Docker is not used at all. `NOMAD_TOKEN` is also
honoured if the cluster requires ACL authentication. (The Nomad-auth regression
test always starts its own dedicated ACL-enabled Docker cluster and is skipped
when Docker is unavailable.)

The suite clears all Nomad SDK environment variables (`NOMAD_ADDR`,
`NOMAD_TOKEN`, `NOMAD_NAMESPACE`, `NOMAD_REGION`, and the TLS set) from the
process environment before any tests run, then restores them on exit. This
prevents env vars from a developer's shell from leaking into subprocesses spawned
by the E2E tests (the compiled binary, Docker commands). The captured values are
still used to configure the test cluster connection.

Note that Raft-index skip tests (`TestDrift_RaftIndexSkip`,
`TestMetrics_SkipOptimizationCounter`) can be flaky against a shared cluster
because unrelated job or eval activity advances the global `LastIndex` between
calls. They are reliable against the isolated Docker-managed cluster.

### What the suite covers

| File | What is tested |
|------|----------------|
| `drift_test.go` | All three DiffTypes (`modified`, `missing_from_nomad`, `missing_from_hcl`); dead-job handling (stop-only and purge modes); Raft-index skip optimisation; commit-change bypass; multi-job checks; `ForceCheck` staleness counter |
| `selection_test.go` | Exact glob; wildcard glob; no-match glob; meta-key presence/absence; union selection (both criteria); no criteria configured |
| `metrics_test.go` | All expected metric names registered at construction; gauge values match observed drift; skip counter; first-seen timestamps (set, stable, cleared); parse-error and non-job-skip counters |
| `security_test.go` | Webhook HMAC-SHA256 (valid, invalid, missing, wrong algorithm, large body, concurrent flood, no-secret mode); JSON API auth (missing, wrong, correct key; 100-concurrent load); path-traversal job IDs; very large HCL files; HTML XSS escaping in the index page |
| `e2e_test.go` | Binary lifecycle (503→200 on startup); drift detected over HTTP and `/diffs`; webhook triggers refresh without waiting for next poll interval; JSON API (`/api/v1/diffs`, `/api/v1/status`, `/api/v1/selected-jobs`, `/api/v1/version`, `POST /api/v1/refresh`, `/api/openapi.json`); `/metrics` endpoint content |
| `apply_test.go` | GitOps apply, end to end with the real binary: drifted job converges under policy `full` (meta and flag); **never writes under the default policy** (the critical negative test); opting a running job in via a single commit; job creation gated by `--enable-job-creation`; `image-only` blocks a non-image change; meta-only changes left alone by default; pre-existing drift deferred on opt-in and on `image-only` → `full` policy widening (and applied with `--apply-existing-drift`); `/api/v1/updates` records |
| `auth_test.go` | Nomad authentication against a dedicated **ACL-enabled** cluster: a static `--nomad-token` authenticates and an anonymous client is denied (403, counted); token-file auth via `--nomad-token-file` with live **token rotation** (a rotated SecretID is applied and the prior token revoked); and **workload-identity login exchange** — a minted RS256 JWT is exchanged for an ACL token via `/v1/acl/login` (with a JWT auth method, static validation key, and binding rule) and used to authenticate a diff check (issue #74) |

### Nomad version compatibility

[`nomad-versions.md`](nomad-versions.md) documents which Nomad versions have been
verified against each nomad-botherer release by running the full regression
suite. The table is updated manually when a release is cut.
`tests/regression/compat.go` holds a `TestedVersions` slice that mirrors the
table in code.

## Release process

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

Then go to GitHub, find the tag under **Releases**, and **publish** it.
Publishing triggers the Docker workflow, which builds and pushes
`ghcr.io/gerrowadat/nomad-botherer:<tag>` for both `amd64` and `arm64`.

### Docker builds

```bash
make docker        # build multi-platform image locally (requires docker buildx)
make docker-push   # build and push to ghcr.io
```
