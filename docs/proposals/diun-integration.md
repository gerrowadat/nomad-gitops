# Proposal: image update tracking with Diun

**Status**: draft  
**Date**: 2026-06-11

## Background

GitOps pins image tags in HCL. That is the point — the cluster runs what Git
says — but it means nobody is watching the other direction: upstream
publishes `api-server:1.44.0` and Git happily pins `1.43.0` forever. The
missing piece is *update availability*: knowing that a newer image exists for
something Git manages, and making it easy to act on.

[Diun](https://github.com/crazy-max/diun) (Docker Image Update Notifier,
crazy-max/diun) is the established tool for this: it watches container
registries for new or changed tags and sends notifications. Per the
no-reimplementation rule, nomad-botherer should not grow its own registry
polling; it should integrate with Diun.

Two hard constraints frame the design:

1. **Git is the source of truth for what to track.** The set of images worth
   watching is exactly the set referenced by managed jobs in the repo. That
   set should drive Diun, not be maintained by hand in a second place.
2. **nomad-botherer never mutates GitHub.** Available updates are surfaced,
   and a helper produces a ready-to-use diff, but committing and PR-ing the
   bump is the human's (or external automation's) job.

The result is a full circle:

```
Git HCL (source of truth: images + tag constraints)
  → nomad-botherer derives the image watch list
  → Diun checks registries on its own schedule
  → Diun webhook → nomad-botherer records an available update
  → surfaced via API + metrics; patch helper emits a diff
  → human turns the diff into a PR; merge updates Git
  → normal GitOps apply path registers the change (policy-gated)
  → HCL now references the new tag → available-update record pruned
```

---

## What Diun provides (and does not)

Facts that constrain the design, from Diun's documentation:

- **Providers** define what Diun watches: Docker, Swarm, Kubernetes,
  **Nomad**, File, and Dockerfile. The Nomad provider connects to a cluster
  and watches images from running Docker-driver tasks, with opt-in via
  `diun.enable = "true"` in job/group/task meta (or service tags), plus a
  `watchByDefault` mode. The File provider reads a YAML list of image
  entries from the **local filesystem only** (a file or a directory; no
  HTTP fetch).
- **Per-image options** are the same vocabulary everywhere: `watch_repo`,
  `include_tags` / `exclude_tags` (regexps), `max_tags`, `sort_tags`
  (including `semver`), `notify_on` (`new`/`update`), `platform`, and a
  free-form `metadata` map that is echoed back in notifications.
- **Notifications are push-only.** Diun has no HTTP query API; you cannot ask
  it "what updates are available?". Its webhook notifier sends a JSON
  payload per event with configurable endpoint, method, and headers.
- **Diun owns its own seen-state** (an embedded key-value store). Each
  new tag or changed digest is notified **once**. A consumer that loses a
  notification does not get it again.

The webhook payload (fields as documented):

```json
{
  "diun_version": "...",
  "hostname": "diun-host",
  "status": "new",
  "provider": "file",
  "image": "ghcr.io/example/api-server:1.44.0",
  "hub_link": "https://github.com/example/api-server/pkgs/container/api-server",
  "mime_type": "application/vnd.docker.distribution.manifest.v2+json",
  "digest": "sha256:…",
  "created": "2026-06-11T10:26:58Z",
  "platform": "linux/amd64",
  "metadata": { "…": "echoed from the provider entry" }
}
```

`status` is `new` (a tag not seen before — how new releases of a
`watch_repo` image arrive) or `update` (a watched tag's digest changed — how
a mutable tag like `:1.43` or `:latest` being re-pushed arrives). Both are
interesting to a GitOps setup: the first is "there is something newer", the
second is "what you have pinned no longer means what it meant".

---

## Who owns the watch list

Diun wants a list it owns and polls; it does not answer ad-hoc queries. The
question is where that list comes from.

### Alternative A: Diun's Nomad provider, nomad-botherer passive

Point Diun's Nomad provider at the cluster. Jobs opt in by carrying
`diun.enable = "true"` in their meta — which, for managed jobs, lives in the
HCL in Git, so the opt-in is still version-controlled. nomad-botherer's only
involvement is receiving webhooks.

**Pros**

- Zero list-maintenance code in nomad-botherer. Diun's Nomad provider
  already exists and is maintained.
- The `diun.*` meta keys in HCL are human-written and round-trip through
  registration untouched — no meta-drift.
- Diun's default Nomad metadata (job ID, namespace, task group) comes back
  in the webhook, making it trivial to match a notification to a job.

**Cons**

- **The watch list is the *running cluster*, not Git.** During drift windows
  the tracked tag is whatever is running, not what Git declares; a job
  committed to Git but not yet registered is not watched at all; a job
  stopped for maintenance silently drops out of tracking.
- Diun needs its own Nomad token with job-read access, a second credential
  to manage.
- Only Docker-driver tasks of *running* jobs are seen.

### Alternative B: nomad-botherer generates the File provider list

nomad-botherer already parses every managed job's HCL each cycle. From the
parsed jobs it derives the image watch list: every Docker task image in a
job with `"diun.enable" = "true"` in meta, with the job's other `diun.*`
meta keys mapped onto the corresponding File provider entry options
(`include_tags`, `sort_tags`, `watch_repo`, …). Each entry's `metadata` map
carries the owning job IDs and HCL file paths, so the webhook payload comes
back pre-correlated.

The list is exposed two ways, both stateless and regenerated from HEAD:

- `GET /api/v1/diun/images.yml` — the rendered File provider YAML, always
  available. Useful for inspection and for any external mechanism that
  delivers it to Diun.
- `--diun-image-list-path` / `DIUN_IMAGE_LIST_PATH` (optional): a filesystem
  path the list is (atomically: write temp + rename) rewritten to whenever
  HEAD changes. Intended for co-scheduling Diun and nomad-botherer in the
  same Nomad task group with a shared `alloc/` directory; Diun's File
  provider points at the shared path. This is a deliberate, narrow exception
  to the no-disk-writes posture: the file is derived state, owned by this
  feature, regenerable from Git at any time, and written outside the in-memory
  git clone (which stays `memory.NewStorage()`).

**Pros**

- **Git is the source of truth**, exactly as required. Images are tracked
  from the moment they are committed, before first registration, and
  independently of cluster state.
- The same `diun.*` meta vocabulary as Alternative A — switching between
  alternatives needs no HCL changes.
- The `metadata` round-trip makes webhook matching exact rather than
  inferred.
- Diun needs no Nomad credentials.

**Cons**

- The File provider reads local files only, so list delivery requires either
  co-scheduling with a shared alloc dir or an external fetch-to-disk step.
- More code in nomad-botherer: list rendering, the endpoint, the file
  writer, and their tests.

### Alternative C: query Diun directly — rejected

Diun has a gRPC API used by its own CLI (`diun image list`), bound to
localhost by default. It is an internal interface, not a stable integration
surface, and polling it would still leave Diun's watch list to be maintained
somewhere. Webhooks are the supported integration point; this alternative is
noted only because "nomad-botherer asks Diun" sounds plausible until the
shape of Diun's API is examined.

### Verdict

Alternative B, with Alternative A as a zero-code fallback that the webhook
intake should be compatible with from day one (the intake matches
notifications by image repository regardless of provider, so a deployment
can start on A and move to B without changes on the receiving side).

---

## Webhook intake

The existing webhook server gains a second endpoint:

| Flag | Env var | Default |
|---|---|---|
| `--diun-webhook-path` | `DIUN_WEBHOOK_PATH` | `/webhook/diun` |
| `--diun-webhook-token` | `DIUN_WEBHOOK_TOKEN` | (empty — endpoint disabled) |

Diun's webhook notifier supports custom headers; the Diun config sets
`Authorization: Bearer <token>` (or an `X-Diun-Token` header) to the same
value, and nomad-botherer rejects non-matching requests. Unlike the GitHub
webhook there is no HMAC signature scheme; a shared bearer token is what the
mechanism supports. An empty token leaves the endpoint unregistered rather
than unauthenticated.

On receipt, nomad-botherer:

1. Parses the payload; rejects anything without an `image`.
2. Splits the image reference into repository and tag, and matches the
   repository against images referenced by managed jobs' HCL at current
   HEAD (plus the `metadata` job hints when present). A repository may match
   several jobs; the update fans out to all of them.
3. Records an `AvailableImageUpdate` per (job, repository):

```go
type AvailableImageUpdate struct {
    JobID      string `json:"job_id"`
    HCLFile    string `json:"hcl_file"`
    Repository string `json:"repository"`          // e.g. ghcr.io/example/api-server
    CurrentRef string `json:"current_ref"`         // image reference pinned in HCL
    NewTag     string `json:"new_tag"`
    Digest     string `json:"digest"`
    Status     string `json:"status"`              // diun's "new" or "update"
    Created    string `json:"created"`             // image creation time, from diun
    ReceivedAt string `json:"received_at"`         // RFC3339
}
```

4. Surfaces the set at `GET /api/v1/image-updates`.

A notification whose repository matches no managed job is counted (metric
below) and dropped — likely a stale watch list or an Alternative A
deployment with `watchByDefault` on.

---

## Restart safety

This is the one place in the design where in-memory state is *not*
recoverable from Git and Nomad alone: Diun notifies each event once, so if
nomad-botherer restarts after receiving a webhook but the human has not yet
acted on it, the knowledge is gone and Diun will not resend it.

Per the [checkpointing proposal](change-checkpointing.md), the store for
small operational state is Nomad Variables. Each received update is written
to:

```
nomad/jobs/gitops/image-updates/<sanitised repository>
```

(Variable paths allow a restricted character set; the repository string is
sanitised by replacing disallowed characters, with the original kept in the
JSON value.) The value is the JSON set of outstanding `AvailableImageUpdate`
records for that repository — small, far under the 64 KiB Variable limit,
CAS-protected against concurrent writers. On startup, nomad-botherer lists
the prefix and rehydrates.

**Pruning** closes the circle and needs no tag-ordering heuristics: an
`AvailableImageUpdate` is dropped when the HCL at current HEAD pins exactly
the notified tag for that job (the human merged the bump — it is no longer
"available", it is current), or when the job no longer references the
repository at all. Records the human chose to skip (e.g. `1.44.0` arrived,
then `1.45.0`, and only the latter was merged) are pruned by a retention
flag rather than guesswork:

| Flag | Env var | Default |
|---|---|---|
| `--image-update-retention` | `IMAGE_UPDATE_RETENTION` | `720h` |

If Variables are unavailable (pre-1.4 cluster, ACL restrictions), the
feature degrades to memory-only with a logged warning: updates are still
received and surfaced, and a restart loses unactioned ones until the next
Diun-side event. That degradation is acceptable; correctness of the GitOps
core never depends on this data.

---

## The patch helper

Surfacing an available update is only useful if acting on it is cheap. The
helper endpoint emits a diff that can be fed straight into a PR:

```
GET /api/v1/image-updates/{job_id}/patch
```

returns `text/x-patch`, a unified diff against the HCL file at current HEAD
with each outstanding image update for that job applied:

```diff
--- a/jobs/api-server.hcl
+++ b/jobs/api-server.hcl
@@ -23,7 +23,7 @@
       driver = "docker"
       config {
-        image = "ghcr.io/example/api-server:1.43.0"
+        image = "ghcr.io/example/api-server:1.44.0"
       }
```

Query parameters narrow the selection when a job has several outstanding
updates: `?repository=…` and `?tag=…` (default: all outstanding updates for
the job, each at its most recently notified tag).

Implementation notes:

- The patch is produced by **exact string substitution** of the old image
  reference in the raw file content from the in-memory clone — not by
  re-rendering parsed HCL, which would destroy formatting and comments. The
  unified diff is generated with an established diff library, not
  hand-rolled.
- If the image reference in the file is not a plain literal (built from HCL
  variables or interpolation), substitution cannot work; the endpoint
  returns `422` with a body explaining which file and why. This limitation
  is documented rather than worked around.
- The workflow is deliberately one step short of automation:

  ```
  curl -s botherer:8080/api/v1/image-updates/api-server/patch \
    | git apply
  git checkout -b bump-api-server && git commit -am "Bump api-server to 1.44.0"
  ```

  or the same patch fed to the GitHub contents API by an external script.
  nomad-botherer itself never holds GitHub write credentials. If a later
  proposal wants PR automation, it builds on this endpoint from outside.

Once the PR merges, the change is ordinary Git drift and flows through the
apply path under the job's [update policy](update-policies.md) — for jobs
set to `image-only`, this circle is precisely the automation they opted
into.

---

## Observability

Following the metrics convention (`promauto.With(reg)`, `nomad_botherer_`
prefix):

- `nomad_botherer_diun_webhooks_received_total{status}` — counter; `status`
  is Diun's `new`/`update`.
- `nomad_botherer_diun_webhooks_unmatched_total` — counter; notifications
  matching no managed job.
- `nomad_botherer_diun_webhook_errors_total` — counter; auth failures and
  malformed payloads.
- `nomad_botherer_image_updates_available` — gauge; current outstanding
  `AvailableImageUpdate` count.
- `nomad_botherer_diun_image_list_entries` — gauge; entries in the generated
  watch list.
- `nomad_botherer_image_update_patches_served_total` — counter.

---

## Deployment sketch (Alternative B, co-scheduled)

One Nomad job, one group, two tasks sharing the allocation directory:

- `nomad-botherer` with `--diun-image-list-path=${NOMAD_ALLOC_DIR}/data/diun/images.yml`
  and `--diun-webhook-token` set.
- `diun` with the File provider pointed at that path, its embedded state db
  on ephemeral task disk — losing it merely re-notifies everything once,
  which nomad-botherer deduplicates by (repository, tag) — and the webhook
  notifier pointed at `http://127.0.0.1:<listen>/webhook/diun` with the
  matching bearer header.

No volumes, no external services; the whole pair is reschedulable to any
node, preserving the no-volume-claims principle.

---

## Open questions

- **Digest-only changes.** A `status: update` on a pinned tag (same tag, new
  digest) cannot be expressed as an HCL diff — the file already says the
  right thing. Surface it as a distinct class ("pinned tag was re-pushed
  upstream") with no patch offered? Probably yes; it is a supply-chain
  signal, not a version bump.
- **Tag constraint defaults.** Should jobs without `diun.include_tags` get a
  generated default (e.g. `sort_tags: semver` + `watch_repo: true`), or the
  Diun defaults? Generated defaults are friendlier but more magic.
- **Shared images.** Two jobs pinning different tags of the same repository
  produce one watch-list entry but two `AvailableImageUpdate` records with
  different `current_ref`s. The list renderer needs to merge constraints
  (union of include_tags?) — or emit one entry per distinct (repository,
  constraints) pair. The latter is simpler; verify Diun deduplicates
  registry calls itself.
- **Webhook payload versioning.** Diun's payload is documented but not
  versioned. The parser should ignore unknown fields and tolerate missing
  optional ones; a contract test against a captured real payload would catch
  upstream changes.
