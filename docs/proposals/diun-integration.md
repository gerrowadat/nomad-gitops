# Proposal: image update tracking with Diun

**Status**: draft  
**Date**: 2026-06-11

> **Revision note**: this proposal has shrunk twice, deliberately. An early
> draft had nomad-botherer receiving Diun's webhooks and persisting
> "available update" records; that went away because a notification is not
> actionable by nomad-botherer (nothing changes in Nomad until Git changes)
> and would have been the only state in the design not recomputable from Git
> and Nomad. A second draft had nomad-botherer generating Diun's watch list
> from the HCL in Git; that went away because Diun's own Nomad provider
> covers the need with zero integration code. The result: **nomad-botherer
> and Diun do not talk to each other at all.** The only thing this proposal
> adds to nomad-botherer is a stateless patch endpoint.

## Background

GitOps pins image tags in HCL. That is the point — the cluster runs what Git
says — but it means nobody is watching the other direction: upstream
publishes `api-server:1.44.0` and Git happily pins `1.43.0` forever. The
missing piece is *update availability*: knowing that a newer image exists for
something Git manages, and making it easy to act on.

[Diun](https://github.com/crazy-max/diun) (Docker Image Update Notifier,
crazy-max/diun) is the established tool for this: it watches container
registries for new or changed tags and sends notifications. Per the
no-reimplementation rule, nomad-botherer must not grow its own registry
polling.

The constraints that frame the design:

1. **nomad-botherer never writes to Git.** Not directly, not via the GitHub
   API, not on a side branch. Bumping a tag in a job file is a Git change
   like any other: it arrives by PR, authored by a human or by automation
   that is not this tool.
2. **nomad-botherer acts only on Git and Nomad state.** Everything it knows
   must be recomputable from those two (see "Restart safety and recovery" in
   [gitops-job-updates.md](../design/gitops-job-updates.md)). Diun notifications are
   delivered exactly once and are not recomputable, so nomad-botherer does
   not consume them.

## Division of labour

```
Diun             watch the cluster's images (Nomad provider, all jobs)
                 → check registries on its own schedule
                 → notify (Slack/Matrix/email/webhook/… — Diun's job)
outside          turn a notification into a Git PR: a human, or a small
                 separate "bumper" job — either may use nomad-botherer's
                 patch endpoint to get a ready-made diff
Git              PR review + merge: the only write path
nomad-botherer   ordinary GitOps apply of the merged commit (policy-gated,
                 see update-policies.md)
```

The circle closes — Git's images stay current, and for jobs with
`gitops_update_policy = "image-only"` the merged bump is applied
automatically — but nomad-botherer appears in it only at the two places it
already lives: rendering a diff from the repo at HEAD, and applying merged
commits to Nomad.

---

## How Diun watches: the Nomad provider

Diun's Nomad provider connects to the cluster (address/token, the same
`NOMAD_ADDR` conventions as everything else) and watches the images of
running Docker-driver tasks. The chosen configuration is
`watchByDefault: true`: **all jobs are watched**, managed or not, with no
per-job opt-in required and no involvement from nomad-botherer.

Per-job tuning still lives in Git. Diun reads its `diun.*` options from job,
group, or task meta — `diun.include_tags`, `diun.exclude_tags`,
`diun.sort_tags` (including `semver`), `diun.watch_repo`, `diun.max_tags`,
`diun.notify_on`, and `diun.enable = "false"` to exclude a job — and for
managed jobs that meta is written in the HCL and flows to the live job
through registration. So tag-filter policy remains version-controlled and
reviewable even though nomad-botherer never touches Diun. (Dotted meta keys
need HCL's object-expression form; see the syntax note in
[update-policies.md](../design/update-policies.md).)

Accepted tradeoffs of watching the cluster rather than the repo:

- The watch list is the *running cluster*. A job committed to Git but never
  registered is not watched; a stopped job drops out of tracking; during a
  drift window the watched tag is the running one, not the declared one.
  For a setup where everything in Git is expected to be running, these
  windows are short and harmless.
- Diun needs its own Nomad token with job-read access.
- Only Docker-driver tasks are seen.

If those tradeoffs ever bite, the previously-drafted alternative —
nomad-botherer rendering a Diun File-provider list from the parsed HCL at
HEAD, so Git rather than the cluster defines the watch set — is recorded in
this file's history (and the File provider's local-filesystem-only delivery
constraint with it). It is not part of the current design. Likewise
querying Diun directly was examined and rejected: Diun is push-only, with
no HTTP query API (its gRPC interface is internal to its own CLI).

One Diun behaviour worth knowing operationally: each new tag or changed
digest is notified **once**, with seen-state kept in Diun's embedded store.
If that store is lost (ephemeral disk, reschedule), everything is re-notified
once — noise for the notification consumer, irrelevant to nomad-botherer.

---

## The patch helper

nomad-botherer holds one thing the notification consumer wants and cannot
cheaply get: the parsed repo at HEAD, including which managed HCL files
reference a given image repository and the exact literal to substitute. The
patch helper exposes that as a stateless endpoint — the caller brings the
facts from the notification, nomad-botherer renders the diff:

```
GET /api/v1/image-patch?repository=ghcr.io/example/api-server&tag=1.44.0
```

returns `text/x-patch`: a unified diff against the repo at current HEAD that
bumps every managed job's reference to that repository up to the given tag
(possibly spanning multiple files, when several jobs pin the same image).
An optional `&job=<job_id>` narrows it to one job's file.

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

Implementation notes:

- The patch is produced by **exact string substitution** of the old image
  reference in the raw file content from the in-memory clone — not by
  re-rendering parsed HCL, which would destroy formatting and comments. The
  unified diff is generated with an established diff library, not
  hand-rolled.
- The endpoint is a pure function of (HEAD, repository, tag). No notification
  state, nothing to persist, nothing to lose on restart.
- `404` when no managed job references the repository; `422` when a
  referencing file builds the image string from HCL variables or
  interpolation, with a body naming the file — substitution cannot work
  there, and the limitation is documented rather than worked around.
- The tag is taken on faith. Validating that it exists in the registry would
  mean nomad-botherer talking to registries, which is Diun's job; the caller
  got the tag *from* Diun.

The endpoint deliberately stops one step short of a PR. nomad-botherer never
holds GitHub write credentials.

---

## Closing the loop, outside nomad-botherer

How a Diun notification becomes a Git PR is out of scope for this tool, by
design. Two shapes, for illustration:

**Manually.** Diun notifies a Slack/Matrix/email channel. A human sees
"api-server 1.44.0 available" and runs:

```
curl -s "http://botherer:8080/api/v1/image-patch?repository=ghcr.io/example/api-server&tag=1.44.0" \
  | git apply
git checkout -b bump-api-server
git commit -am "Bump api-server to 1.44.0" && git push
```

**A separate bumper job.** A small service (or scheduled Nomad job — *not*
nomad-botherer, not this repo) receives Diun's generic webhook, calls the
patch endpoint with the notified repository and tag, and opens a PR via the
GitHub API. It owns the GitHub token, its own auth on the webhook, and its
own policy about which notifications deserve automatic PRs. If it dies and
loses a notification, that is between it and Diun — nomad-botherer's
correctness and state are untouched. Tools like Renovate occupy the same
seat (see [prior-art.md](../prior-art.md)).

Either way, the merged PR is ordinary Git drift and flows through the apply
path under the job's [update policy](../design/update-policies.md) — for jobs set to
`image-only`, this circle is precisely the automation they opted into.

---

## Observability

Following the metrics convention (`promauto.With(reg)`, `nomad_botherer_`
prefix):

- `nomad_botherer_image_patches_served_total` — counter.
- `nomad_botherer_image_patch_errors_total{reason}` — counter; `reason` of
  `unknown_repository` or `non_literal_reference`.

Diun has its own Prometheus metrics for the registry-watching side; nothing
to duplicate here.

---

## Deployment sketch

Diun runs as its own Nomad job, independent of nomad-botherer:

- Nomad provider: cluster address, a read-only Nomad token,
  `watchByDefault: true`.
- Embedded state db on ephemeral task disk (loss is tolerable: one round of
  re-notification).
- Notifiers pointed wherever the loop is closed — a chat channel for the
  manual workflow, the bumper job's endpoint for the automated one.

nomad-botherer needs no Diun-related configuration at all; the patch
endpoint is part of its normal HTTP server.

---

## Open questions

- **Digest-only re-pushes.** Diun's `status: update` (same tag, new digest)
  has no HCL expression — the file already names the right tag — so the
  patch endpoint can do nothing with it. It is still a signal worth routing
  somewhere (a mutable pinned tag changed under you), but that routing is
  the notification consumer's concern.
- **Tag-constraint hygiene.** With `watchByDefault: true` and no
  `diun.include_tags`, noisy repositories (nightly tags, arch-suffixed tags)
  may generate notification spam. That is tuned per job in HCL via `diun.*`
  meta, which is a documentation task here, not a code one.
- **Patch endpoint and unmanaged jobs.** Diun watches *all* jobs, but the
  patch endpoint only knows files of managed ones. A notification for an
  unmanaged job's image gets a `404` from the endpoint — correct, but the
  README should say so to avoid confusion.
