# Prior art: GitOps for Nomad

This document surveys the existing tooling for GitOps-style job management in
Nomad, identifies what each tool does well and where each falls short, and
explains what problems nomad-botherer is trying to solve differently.

Job *application* (actually submitting changes to Nomad from Git) is not yet
implemented in nomad-botherer. The design proposals that inform that work are in
[`docs/proposals/`](proposals/).

---

## The tools

### nomad-gitops-operator (jonasvinther)

A single Go binary that clones a Git repo, compares its HCL files to a live
Nomad cluster, and applies the difference. Deletion is controlled by a `--delete`
flag; when on, any job tagged with the operator's metadata key that is absent
from the current repo glob is purged. Written with go-git.

**What it gets right**: the basic shape of the problem — clone, parse, plan,
register — is correct. Tags managed jobs in Nomad metadata so deletion scope is
bounded to jobs the operator registered.

**Where it falls short**:

- Re-clones the entire repo from scratch on every 30-second cycle. There is no
  incremental fetch, no HEAD comparison to skip unchanged commits, no Raft index
  check to skip unchanged Nomad state. Every cycle parses and plans every job
  regardless of whether anything changed.
- Calls `Jobs.Register()` unconditionally on every cycle, even when the plan
  shows no diff. This generates unnecessary Nomad evaluations continuously.
- No CAS or conflict handling. There is no use of `EnforceIndex` /
  `JobModifyIndex`, so a concurrent human edit between plan and register is
  silently overwritten or silently wins depending on timing.
- The 30-second poll interval is hardcoded with no flag or environment variable.
- No webhook support; detection latency is always at least 30 seconds.
- No Prometheus metrics, no HTTP health endpoint.
- The managed-job metadata key has a typo (`nomoporater`, with an `e`) that is
  now a permanent compatibility constraint across versions.
- Minimal active development since early 2024.

### nomad-ops (nomad-ops/nomad-ops)

A more complete service with a Go backend, a React/PocketBase frontend, and
SQLite state storage. Repositories are configured as "Sources" through a UI.
Each Source points at a URL, branch, path glob, Nomad namespace, and region.
Multiple Sources run independently.

**What it gets right**:

- Disk-cached clones with incremental `git pull`, configurable interval (default
  60 seconds).
- A `hasUpdate()` function that reads the Nomad plan diff before deciding whether
  to call Register. If the plan shows no real field changes, Register is skipped.
- Autoscaler awareness: diffs that are only in `Count` or `Scaling` fields are
  treated as not requiring re-registration, so nomad-ops does not fight an
  autoscaler running alongside it.
- Deletion excludes batch, periodic, and dispatch-child jobs, which is a
  meaningful safety default.
- Prometheus metrics and structured state in PocketBase for event history.

**Where it falls short**:

- Still no CAS on Register. The plan-then-register window has no optimistic
  locking.
- No webhook trigger. Detection latency is bounded by the poll interval.
- Requires SQLite via PocketBase for all state. This means the pod needs
  persistent storage (or NFS) if it needs to be rescheduled to a different node.
- No multi-cluster support.
- No workload identity (JWT tokens); requires a static `NOMAD_TOKEN`.
- Deletion is always on for managed jobs with no per-job override or protection
  flag. A job whose file was renamed or moved triggers deregistration before the
  new file is registered.

### Levant (hashicorp/levant) — archived June 2025

A CLI templating and single-shot deployment tool. Takes a job HCL template,
substitutes variables, runs `nomad job plan`, registers the job, and polls
allocations until stable. Supports canary auto-promotion and auto-revert.

Not a GitOps operator: no polling loop, no continuous reconciliation, no drift
detection. You call it from a CI pipeline on git push. Its replacement, nomad-pack,
is a similar CLI with a packaging layer on top. Neither has reconciliation
behaviour.

### Waypoint (HashiCorp) — OSS archived January 2024

Waypoint had a runner component that polled a Git repo and ran `waypoint up`
pipelines on new commits. For Nomad, the `nomad-jobspec` plugin submitted a job
spec. This was push-triggered CD, not continuous state reconciliation: a new
commit triggered a pipeline run, but there was no comparison of desired vs
actual cluster state and no reverting of manual drift. The HCP SaaS successor
focuses on internal developer platforms and has no Nomad deployment story.

---

## Adjacent prior art: image update automation

GitOps pins image tags; something else has to notice that upstream moved.
The tools in this space differ mainly in *where the bump is written*, which
is the design-relevant axis (see
[`docs/proposals/diun-integration.md`](proposals/diun-integration.md)):

### Diun (crazy-max/diun)

Watches container registries for new tags and changed digests, and sends
notifications (webhook among ~20 channels). Providers define the watch list:
Docker, Swarm, Kubernetes, **Nomad** (watches running Docker-driver tasks,
opt-in via `diun.enable` meta — the same Operator Pattern shape as scalad),
File (a YAML list on local disk), and Dockerfile. Per-image options cover
tag regexes, semver sorting, and notify-on-new vs notify-on-update. It
deliberately *only notifies*: it never writes to a cluster or a repo, has no
query API (push-only, each event delivered once), and keeps its own seen-state
in an embedded store. This makes it composable with a GitOps operator rather
than competing with one. The planned setup points Diun's Nomad provider at
the cluster, watching all jobs; nomad-botherer and Diun do not talk to each
other at all. nomad-botherer's only contribution is a read-only patch
endpoint for whoever acts on the notifications — consuming them and writing
the bump to Git stays outside the tool.

### Renovate (renovatebot/renovate)

Opens PRs against the repo when dependencies — including Docker image
references — have newer versions. Writes to *Git*, which keeps the GitOps
invariant intact. The catch for this project: Nomad HCL is not a natively
supported manager, so image references in job files need custom regex
managers. Renovate can coexist with nomad-botherer (Renovate bumps Git,
nomad-botherer applies), and the planned patch-helper endpoint deliberately
leaves room for it or similar tooling to own the PR side.

### argocd-image-updater (Argo project)

The Kubernetes precedent for bolting image tracking onto a GitOps operator.
Watches registries for images used by Argo CD applications and supports two
write-back methods: commit the bump to Git (preserves the GitOps model) or
mutate the application's parameters directly in-cluster (faster, but the
live state now disagrees with Git). The existence of both modes — and the
documented advice to prefer the Git write-back — is a useful confirmation
that "surface the update, let Git change first" is the defensible default.

### Keel (keel-sh/keel)

A Kubernetes operator that updates workloads *in-cluster* when new images
appear, with optional approval workflows. The cluster drifts ahead of Git
by design; Git stops being the source of truth. This is precisely the
failure mode nomad-botherer avoids by never applying anything that is not
in Git: Diun notices the update, nomad-botherer offers a ready-made diff,
but the change must land in the repo before it lands in Nomad.

---

## What nomad-botherer does differently today

nomad-botherer detects drift always and applies it only where per-job update
policies and deployment flags allow (the default is detection-only). Both
sides address the problems above:

**Incremental change detection.** The git watcher stores HEAD between cycles.
If HEAD has not advanced since the last check, no per-job work is triggered from
the git side. On the Nomad side, `Jobs.List()` returns a Raft index
(`QueryMeta.LastIndex`); if that index has not advanced since the last check,
the per-job plan calls are skipped entirely. A quiet cluster with no git
activity costs one `Jobs.List()` call per diff interval, not N plan calls.

**Webhook trigger.** A GitHub push webhook can trigger an immediate fetch and
diff check, reducing detection latency from poll-interval to seconds.

**Prometheus metrics throughout.** Every meaningful operation — git fetches,
diff checks, skipped checks, API errors, per-job drift, drift duration,
webhook events — is a counter or gauge. This makes the detection loop
observable without log scraping.

**No persistent storage.** State is entirely in-memory. There is no database,
no local disk requirement. The tradeoff is that a restart re-derives all state
(including the update queue) from the next diff cycle. This is safe for the
apply side too: CAS plus re-planning make a re-detected apply idempotent.

**The apply side avoids the specific mistakes above.** Unlike
nomad-gitops-operator, it never registers without a plan showing a real diff,
and every register uses `EnforceIndex` with the `JobModifyIndex` captured at
detection. Unlike nomad-ops, autoscaler-owned `Count`/`Scaling` changes
neither trigger nor block updates, and registers on autoscaled jobs use
`PreserveCounts`. Per-job `gitops_update_policy` meta (`full` / `image-only` /
`none`) bounds how much automation each job gets.

**Staleness guards.** `--max-git-staleness` and `--max-nomad-staleness` can
force refreshes if the normal polling path falls behind, independently of the
main poll interval.

---

## What nomad-botherer wants to do differently for job application

The proposals in [`docs/proposals/`](proposals/) describe the intended approach
to applying changes. The main design decisions, and how they differ from the
tools above:

**Plan before every apply, CAS on register.** Rather than calling Register
unconditionally or after a simple plan check, the intent is to use Nomad's
`JobModifyIndex` as a check-and-set token. The index is recorded at detection
time; Register is called with `EnforceIndex=true` and that index value. If the
cluster state changed between detection and apply, Nomad rejects the write. This
prevents the plan-then-register race that neither existing operator handles.

**Opt-in scope via job meta.** Rather than managing every job found in the repo
or every job tagged with an operator-specific key the tool wrote, the intent is
to require an explicit `gitops_managed = "true"` key in the job's HCL meta
stanza. This is the "Operator Pattern in Nomad" — the same model that scalad
(trivago) uses for autoscaling opt-in and that Pondidum/nomad-operator
documented for backup automation in 2021. A job without the opt-in key is never
touched, even if it is running in Nomad without a corresponding HCL file. This
prevents accidental deregistration of manually-managed jobs.

**No external database for state.** nomad-ops requires SQLite on persistent
storage. The design proposals use Nomad Variables (Raft-backed KV built into
Nomad 1.4+) for checkpoint state instead, paired with a meta opt-in scope
selector; a dedicated Git state branch was considered and rejected, because
nomad-botherer never writes to Git. The goal is that nomad-botherer can be
rescheduled to any node without volume claims.

**Async apply queue, not synchronous blocking.** An in-process queue decouples
detection from application. A slow or failing apply does not delay the next diff
check. Updates for the same job that arrive faster than applies finish are
marked superseded; the most recent intended state always wins.

**Conservative deletion.** The `missing_from_hcl` drift type in nomad-botherer
is an observation, not an automatic action. Any future deregister behaviour
should require the opt-in key to be present on the live job (confirming the job
was previously managed by this operator), and should probably require an explicit
flag, not be on-by-default.

---

## What no existing tool solves

Several problems remain open across all Nomad GitOps tooling, including
nomad-botherer's planned work:

- **No multi-cluster support** in any OSS tool. Cluster fan-out requires a
  separate instance per cluster.
- **No workload identity.** All tools require a static Nomad token. Nomad's
  JWT-based workload identity (available when nomad-botherer itself runs as a
  Nomad job) is not used by any of the tools surveyed.
- **No dependency ordering.** If job B depends on job A being registered first,
  there is no mechanism to express or enforce this. Nomad does not expose a
  dependency graph; any ordering would require explicit metadata in HCL.
- **No standard has emerged.** HashiCorp has no official GitOps operator for
  Nomad and does not recommend one. The dominant production pattern remains
  push-based: a CI pipeline calls `nomad job run` on git push. nomad-ops is the
  most feature-complete pull-based operator but has ~111 stars and is maintained
  by a small team. There is no ArgoCD equivalent for Nomad.
