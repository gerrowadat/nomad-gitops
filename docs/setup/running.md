# Running nomad-botherer

nomad-botherer needs two things: a git repo containing your Nomad HCL job
definitions, and a Nomad cluster to compare them against. Out of the box it
only *detects* drift and never writes — see
[Applying changes](../applying-changes.md) to turn on reconciliation.

There are three ways to run it. Pick one:

- [As a Nomad job](#as-a-nomad-job) — the common production deployment.
- [As a standalone binary](#as-a-standalone-binary) — for local use and testing.
- [As a Docker container](#as-a-docker-container) — outside Nomad.

Whichever you choose, you will also need to set up:

- [Git access](git-access.md) — for private repos (HTTPS token or SSH key).
- [Nomad access](nomad-access.md) — when the cluster has ACLs enabled.
- Optionally, [webhooks](webhooks.md) — to react to pushes immediately.

After it starts, [opt jobs in to monitoring](#opt-jobs-in-to-monitoring).

---

## As a Nomad job

The most common deployment is to run nomad-botherer as a Nomad job on the same
cluster it watches. [`examples/nomad-botherer.hcl`](../../examples/nomad-botherer.hcl)
is a ready-to-use job definition with every configuration option commented.

1. Copy `examples/nomad-botherer.hcl` into your job repo (or download it).

2. Set the required values in the `env {}` block:
   - `GIT_REPO_URL` — the URL of your HCL repo
   - `API_KEY` — a long random string (used to authenticate requests to the
     `/api/` endpoints)

3. For a private repo, also set `GIT_TOKEN` (HTTPS) or mount an SSH key and set
   `GIT_SSH_KEY`. The example file has commented instructions for both,
   including how to read secrets from a Nomad Variable rather than hardcoding
   them. See [Git access](git-access.md).

4. If your cluster has ACLs enabled, give the job access to the Nomad API. The
   recommended way is **workload identity** — no token to manage or rotate. The
   example job already includes the `identity { file = true }` block; you just
   bind an ACL policy to it. See [Nomad access](nomad-access.md) for the two
   commands. (A static `NOMAD_TOKEN` still works and is fine for testing.)

5. Submit the job:
   ```bash
   nomad job run nomad-botherer.hcl
   ```

6. Watch startup — nomad-botherer clones the repo and runs its first diff check
   before reporting healthy:
   ```bash
   nomad job status nomad-botherer
   curl http://<alloc-ip>:8080/healthz
   ```
   `/healthz` returns `HTTP 503` with `"status": "starting"` until the first
   check completes, then `HTTP 200` with a JSON drift summary.

A single allocation is correct; running more than one produces duplicate drift
reports (all git state is held in memory, nothing is written to disk).

---

## As a standalone binary

```bash
./nomad-botherer \
  --repo-url https://github.com/myorg/nomad-jobs.git \
  --nomad-addr http://nomad.example.com:4646
```

For a private HTTPS repo add `--git-token ghp_...`; for SSH add
`--git-ssh-key ~/.ssh/id_ed25519` (see [Git access](git-access.md)). On an
ACL-enabled cluster add `--nomad-token` (see [Nomad access](nomad-access.md)).
The full flag reference is in [Configuration](../configuration.md).

A few more starting points:

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

## As a Docker container

**With an HTTP (token) git remote:**

```bash
docker run -d \
  -e GIT_REPO_URL=https://github.com/myorg/nomad-jobs.git \
  -e GIT_TOKEN=ghp_... \
  -e NOMAD_ADDR=http://nomad.example.com:4646 \
  -e NOMAD_TOKEN=... \
  -p 8080:8080 \
  ghcr.io/gerrowadat/nomad-botherer:latest
```

**With an SSH git remote:**

```bash
docker run -d \
  -e GIT_REPO_URL=git@github.com:myorg/nomad-jobs.git \
  -e GIT_SSH_KEY=/run/secrets/ssh_key \
  -v /path/to/id_ed25519:/run/secrets/ssh_key:ro \
  -e NOMAD_ADDR=http://nomad.example.com:4646 \
  -p 8080:8080 \
  ghcr.io/gerrowadat/nomad-botherer:latest
```

To enable the JSON API, add `-e API_KEY=your-api-key`. Supported platforms:
`linux/amd64`, `linux/arm64` (Raspberry Pi 4+).

---

## Opt jobs in to monitoring

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
watched. See [Job selection](../job-selection.md) for the full details,
including how Git is always the source of truth for the `gitops_*` keys.
