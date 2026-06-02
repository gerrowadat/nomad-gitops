# Proposal: checkpointing ongoing job updates

**Status**: draft  
**Date**: 2026-05-13

## Background

The async update queue described in the GitOps job updates proposal is
in-memory. When nomad-botherer restarts — upgrade, crash, or eviction — any
updates that were `PENDING` or `IN_PROGRESS` are lost. The next diff cycle
recreates them, so correctness is not compromised, but there is a window where
an apply was already sent to Nomad but the outcome was never recorded, and a
second apply of the same change can occur.

For idempotent operations (re-registering an already-registered job with the
same content) this double-apply is harmless. For `DEREGISTER` operations it
could be a problem if the job was re-registered between the first apply and
the restart; the second apply could delete a job that should be running.

The more significant problem is durability of intent. If a long-running
multi-job rollout (e.g., deploying 30 jobs from a single commit) is interrupted
halfway, the operator has no record of which jobs were applied and which were
not. A fresh diff cycle will detect the remaining drift and queue new updates,
but whether those new updates correspond to the same intent as the interrupted
rollout is ambiguous.

This proposal describes three ways to checkpoint update state without a
standalone database.

---

## Requirements

- Survive a nomad-botherer process restart without losing knowledge of which
  updates were applied and which are still pending.
- Resume a partial rollout rather than re-deriving intent from scratch on every
  restart.
- Not require an external database (PostgreSQL, Redis, etcd, etc.).
- Ideally, not require additional infrastructure beyond what the service already
  talks to (Nomad, Git).

---

## Alternative 1: Nomad Variables as the checkpoint store

**How it works**

Nomad 1.4+ includes a built-in key-value store called Nomad Variables, backed
by Raft and replicated across the cluster. Variables have ACL integration,
support CAS (check-and-set via `ModifyIndex`), and survive cluster restarts.

nomad-botherer writes one Variable per in-flight rollout at a well-known path:

```
nomad/jobs/gitops/checkpoints/<git_commit>
```

The value is a JSON-serialised snapshot of the `JobUpdate`
slice for that commit. The Variable is created when the first update for a
commit is enqueued and updated atomically as each update transitions through
`PENDING → IN_PROGRESS → SUCCEEDED/FAILED`. When all updates for a commit reach
a terminal state, the Variable is deleted (or left for audit; configurable).

On startup, nomad-botherer reads all Variables under
`nomad/jobs/gitops/checkpoints/` and rehydrates the in-memory queue from any
non-terminal updates. Updates that were `IN_PROGRESS` are reset to `PENDING`
and retried (the CAS token from `JobModifyIndex` prevents double-apply harm).

**Interaction with Nomad Raft index**

Nomad Variables use the same Raft log as job state. A Variable write returns a
`ModifyIndex` that can be used for CAS on the next update, ensuring that two
concurrent nomad-botherer instances (e.g., during a rolling upgrade) cannot
write conflicting checkpoint data. The instance that loses the CAS retries after
re-reading the Variable.

**Implementation sketch**

```go
type CheckpointStore interface {
    // Write atomically updates the checkpoint for a commit.
    // modifyIndex is 0 for a new checkpoint, or the previous ModifyIndex.
    Write(ctx context.Context, commit string, updates []JobUpdate, modifyIndex uint64) (uint64, error)

    // Read returns the checkpoint for a commit, or nil if none exists.
    Read(ctx context.Context, commit string) ([]JobUpdate, uint64, error)

    // List returns all active checkpoints.
    List(ctx context.Context) (map[string][]JobUpdate, error)

    // Delete removes a checkpoint once a rollout is complete.
    Delete(ctx context.Context, commit string) error
}
```

The Nomad client already exists in the process; this adds usage of
`client.Variables()` from the same `github.com/hashicorp/nomad/api` package.

**Pros**

- No new infrastructure. Nomad is already a hard dependency.
- Raft-backed durability and replication match the cluster's own guarantees.
- CAS prevents split-brain between concurrent nomad-botherer instances.
- ACLs on Variable paths can restrict who can read or modify checkpoint state.
- Nomad's built-in UI shows Variables; operators can inspect checkpoints without
  extra tooling.

**Cons**

- Requires Nomad 1.4+ (Variables API). Older clusters cannot use this approach.
- Adds a new write path to Nomad for operational state, which may conflict with
  cluster ACL policies that restrict writes to the `nomad/jobs/` namespace.
- Variable size limit is 64 KiB per key. A very large rollout (hundreds of jobs)
  may exceed this; mitigation is one Variable per job rather than per commit,
  at the cost of more API calls on startup.
- Nomad Variables are not designed as a queue and have no watch/notify semantics;
  polling is required.

**Verdict**: the cleanest option when Nomad 1.4+ is available. Keeps all
operational state inside the system being managed.

---

## Alternative 2: Git branch as the checkpoint store

**How it works**

nomad-botherer maintains a dedicated branch in the same repository it watches,
e.g., `gitops-state`, which it treats as a write-only append log. The branch
holds one file per active rollout:

```
checkpoints/<git_commit>.json
```

Each file contains the `JobUpdate` slice for that commit, serialised as JSON.
The file is committed and pushed when updates are enqueued, and updated with
terminal statuses when each update completes. The branch is never merged into
the main branch; it is purely operational state.

On startup, nomad-botherer shallow-fetches the `gitops-state` branch (a small
fetch since it only contains checkpoint files, not job HCL), reads all
non-terminal checkpoint files, and rehydrates the queue.

**Concurrency control**

Git itself provides concurrency control via push rejection. If two instances try
to push a checkpoint update simultaneously, one will receive a non-fast-forward
rejection and must pull, merge, and retry. For checkpoint files (one file per
commit, independent between commits), merge conflicts are essentially impossible;
the only conflict would be two instances updating the same file, which is
prevented by the single-writer design (only the instance that detected the
commit owns its checkpoint).

**Implementation sketch**

The existing `gitwatch.Watcher` uses `go-git` and `memory.NewStorage()`. The
checkpoint writer needs a separate, persistable storage (not in-memory) so that
pushes can be made. This likely means a second `go-git` clone with disk-backed
storage, or a thin wrapper around `git` CLI calls for the state branch.

```go
type GitCheckpointStore struct {
    repoURL    string
    branch     string  // "gitops-state"
    workDir    string  // disk path for the state clone
    auth       transport.AuthMethod
}
```

**Pros**

- Git is already a dependency; no new credentials or network endpoints needed
  (assuming write access to the repo is already granted via token or SSH key).
- The checkpoint history is a full Git log: every state transition is an
  immutable commit, with timestamp and message. This is a better audit trail
  than the in-memory or Nomad Variables approaches.
- Standard Git tooling (`git log`, `git diff`, `git show`) lets operators
  inspect and manipulate checkpoint state without custom tooling.
- No external API version constraints (works with any Git host).

**Cons**

- Requires write access to the repository. Read-only tokens (common for
  pull-based GitOps setups) are not sufficient.
- Adds Git push latency to the hot path of every status update. A rollout of
  30 jobs produces 30+ commits to the state branch.
- Branch history grows unboundedly unless a periodic cleanup job prunes old
  checkpoint commits (e.g., `git push --force` with a truncated history, or
  a separate cleanup cron).
- Mixing operational state and source code in one repository is operationally
  awkward. Teams that have separate read/write access policies for source vs
  operational state cannot use this without repo restructuring.
- The state branch must be protected from human pushes that could corrupt
  checkpoint data; this requires branch protection rules on the Git host.

**Verdict**: good for teams that already have write access and want a full audit
trail, but the per-update commit overhead and the read-write access requirement
make it awkward as a default.

---

## Alternative 3: Nomad job meta as opt-in selector

**The pattern**

Rather than nomad-botherer managing every job it finds in Git, jobs must opt in
to GitOps management by declaring a meta key in their HCL:

```hcl
job "api-server" {
  meta {
    gitops_managed = "true"
  }
  # rest of job spec
}
```

nomad-botherer reads this key from both the parsed HCL and the live job
(`Jobs.Info()`) to decide whether to include a job in its reconciliation scope.
A job without `gitops_managed = "true"` is skipped entirely — no diff, no
apply, no deregister. The meta key is a scope selector, not a state store.

This is a direct application of what the Nomad community calls the **Operator
Pattern**: an external controller watches for jobs bearing a specific meta key
and acts only on those, leaving everything else alone. It is the Nomad
equivalent of the annotation-based opt-in used by Kubernetes controllers such as
cert-manager (`cert-manager.io/cluster-issuer: "letsencrypt"`) or the Prometheus
auto-discovery annotations (`prometheus.io/scrape: "true"`).

**Prior art in the Nomad ecosystem**

The pattern is established and in production use:

- **scalad** (trivago/scalad): a horizontal autoscaler for Nomad. Jobs opt in
  with `meta { scaler = "true" }`, then supply additional meta keys for
  min/max counts, cooldown periods, and scaling queries. The entire scaling
  policy lives in the job spec alongside the opt-in flag. This is the clearest
  real-world example of the pattern.
  (https://github.com/trivago/scalad)

- **nomad-operator** (Pondidum/nomad-operator): a community-documented
  implementation applied to automated database backups. Jobs declare
  `meta { auto-backup = "true"; backup-schedule = "@daily" }`. The operator
  watches the Nomad event stream for job registration events, reads the meta
  keys, and creates or removes a backup job accordingly. This is the only
  written description of the pattern under the name "The Operator Pattern in
  Nomad" (Andy Dote, 2021). There is a corresponding HashiCorp Discuss thread.
  (https://andydote.co.uk/2021/11/22/nomad-operator-pattern/,
  https://github.com/Pondidum/nomad-operator)

- **Nomad Autoscaler** (HashiCorp official): uses the same conceptual pattern
  but resolved it with a first-class `scaling` stanza rather than the freeform
  `meta` map. The `scaling` block has an `enabled` boolean for opt-in and a
  `policy` map for per-job configuration — effectively the pattern promoted to
  a named stanza in the job spec. Conceptually identical; mechanically cleaner.
  (https://developer.hashicorp.com/nomad/tools/autoscaling/policy)

Kubernetes formalised annotation-based opt-in under two conventions: labels are
for identity and selection (controllers use `labelSelector` to query), while
annotations carry per-object configuration. It also mandates DNS-subdomain
prefixes (e.g., `cert-manager.io/`) to prevent key collisions between tools.
Nomad has no equivalent formal convention. The observed practice across the
tools above is a short tool-name prefix with a separator (`gitops_managed`,
`scaler`, `auto_backup`). Key naming is worth being deliberate about: using
underscores keeps meta keys valid HCL2 identifiers, allowing the block form
(`meta { gitops_managed = "true" }`) rather than the object-expression form with
quoted keys. Keys are exposed as `NOMAD_META_*` environment variables inside
tasks using the original form.

**Separating opt-in from state storage**

The version of this alternative described in earlier drafts conflated two
distinct uses of the `Meta` map:

1. **Opt-in flag** (set by humans in HCL, read by the tool): `gitops_managed =
   "true"`. The human controls this; it lives in the HCL file and is therefore
   version-controlled. Safe and stable.

2. **Applied state** (written by the tool back into the live job): `gitops.commit
   = "abc123"`, `gitops.applied_at = "..."`. This is where the approach breaks
   down.

When a tool writes state back into a job's meta, the live job spec diverges from
the HCL in Git. The next time a human runs `nomad job run jobs/api-server.hcl`
without those keys, Nomad silently removes the tool-written fields. This is the
**meta-drift problem**: human submissions clobber tool state, and tool writes
clobber human submissions. `jonasvinther/nomad-gitops-operator` ran into this
directly when using meta fields to track reconciliation state and documented it
as an open limitation. The underlying Nomad issue (#19329, "Add meta for Nomad
Variables") remains open as of 2026.

The clean resolution: use `meta` only for the opt-in flag that the human writes
and controls; store applied state in Nomad Variables (Alternative 1).

**Hybrid: meta opt-in + Variables state**

```hcl
// jobs/api-server.hcl (human-controlled, version-controlled)
job "api-server" {
  meta {
    gitops_managed = "true"
  }
}
```

nomad-botherer behaviour with this flag:

- Include a job in the diff scope if and only if its HCL file or its live
  `Jobs.Info()` response contains `meta["gitops_managed"] == "true"`.
- Store checkpoint state in Nomad Variables (Alternative 1). Never write back
  to the job's `meta` stanza.
- A live job that does not have `gitops_managed` is never flagged as
  `missing_from_hcl`, even if it has no corresponding HCL file. This prevents
  nomad-botherer from attempting to deregister manually-registered jobs.

**Effect on the differ**

`Check()` changes from "compare all HCL files against all Nomad jobs" to a
narrower scope:

- For HCL files: include only files whose parsed job spec has
  `meta["gitops_managed"] == "true"`.
- For live Nomad jobs: include only jobs with `meta["gitops_managed"] == "true"`
  in their live spec.
- The `missing_from_hcl` drift type becomes "a managed job (live meta has the
  flag) has no HCL file". This is a meaningful and intentional signal, not "any
  job not in Git".

The `missingFromNomad` type changes to "an HCL file has the opt-in key but the
job does not exist in Nomad yet". The first registration also writes
`gitops_managed = "true"` to the live job, which is already in the HCL, so
there is no meta drift from this write.

**Pros**

- Operators explicitly enrol jobs. Manual jobs and legacy jobs are never
  touched, regardless of whether they have a corresponding HCL file.
- The opt-in key lives in the HCL file and is therefore in Git history; the
  decision to enrol a job is auditable and reviewable.
- The pattern is recognisable to anyone who has used scalad or the Nomad
  Operator Pattern. It does not require explanation.
- When combined with Nomad Variables for state (the hybrid), there is no
  meta-drift problem and no spurious diff loops.
- Works with all Nomad versions; no Variables API required for the opt-in
  mechanism itself.

**Cons**

- Every job that should be managed must have `gitops_managed = "true"` in its
  HCL. Easy to forget; there is no directory-level default.
- The key appears as `NOMAD_META_gitops_managed` in every allocation's
  environment, which is minor but visible noise.
- Nomad has no server-side filtering by meta value. nomad-botherer must list
  all jobs and filter client-side, which is the same cost as today.
- If a job is registered manually from an HCL file that lacks the opt-in key,
  but the canonical HCL in Git does have it, nomad-botherer sees a `modified`
  diff and will re-register the job with the key present. This is correct GitOps
  behaviour but will surprise operators the first time they encounter it.

**Verdict**: the opt-in pattern is the right default for production deployments
managing a cluster shared with manually-run jobs. It should be paired with
Alternative 1 (Nomad Variables) for state storage. The `meta` flag is a scope
selector; it is not a checkpoint mechanism.

---

## Comparison

The three alternatives address the same problem (where to persist checkpoint
state) but sit on different infrastructure axes. Alternative 3 is also an answer
to a slightly different question (which jobs should be managed at all), so it
layers on top of either of the other two rather than replacing them.

| Property | Alt 1: Nomad Variables | Alt 2: Git state branch | Alt 3: meta opt-in |
|---|---|---|---|
| What it stores | Pending/completed updates | Pending/completed updates | Scope selector only |
| Infrastructure | Nomad 1.4+ | Git write access | Any Nomad |
| Durability | Raft-backed | Full Git history | N/A (opt-in flag, not state) |
| Audit trail | Moderate | Best | N/A |
| Startup cost | List Variables + rehydrate | Clone state branch + read files | Already paid by diff cycle |
| Concurrent instances | CAS on Variable ModifyIndex | Push rejection + retry | N/A |
| Diff loop risk | None | None | None (flag is in HCL, not written by tool) |
| Deregister tracking | Variable deleted on success | Checkpoint file removed | No record |
| Nomad version required | 1.4+ | Any | Any |
| Prevents touching manual jobs | No | No | Yes |

---

## Recommended path

Implement the hybrid of Alternative 3 + Alternative 1:

1. Gate all GitOps behaviour behind the `gitops_managed = "true"` meta opt-in
   (Alternative 3). This scopes the operator and prevents accidental deregistration
   of manually-managed jobs. It requires no infrastructure and works on any Nomad
   version.

2. Use Nomad Variables for checkpoint state (Alternative 1), gated behind a
   config flag (`--checkpoint-store`, default: `nomad-variables`). This provides
   durable, CAS-protected restart state without an external database.

3. Offer Alternative 2 (Git state branch) as `--checkpoint-store=git-branch` for
   teams that need a full audit trail and already have repo write access.

The `CheckpointStore` interface above is the right abstraction boundary. Each
alternative is an implementation of that interface; the update queue does not
need to know which backend is active. The opt-in flag is orthogonal to this
interface and should be a config-level default (`--gitops-opt-in-key`, default:
`gitops_managed`) so teams can rename it if they have a key naming convention.
When customising the prefix, keeping `gitops` as a root (e.g. `gitops_myteam`)
is recommended so all nomad-botherer keys remain visually grouped across teams.

---

## Open questions

- **Variable path prefix**: should the path be configurable
  (`--nomad-variable-prefix`) to allow multiple nomad-botherer instances
  managing different namespaces to coexist without collision?
- **Cleanup policy**: should terminal checkpoints be deleted immediately or
  retained for a configurable duration (e.g., `--checkpoint-retention 24h`)
  for post-hoc debugging?
- **IN_PROGRESS fence**: if a Variable shows `IN_PROGRESS` on startup (the
  previous instance crashed mid-apply), should nomad-botherer immediately
  retry, wait one diff interval, or require manual intervention? Retrying is
  safe due to CAS, but may surprise operators who want to inspect state before
  resuming.
