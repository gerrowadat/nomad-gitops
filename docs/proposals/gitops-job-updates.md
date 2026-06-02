# Proposal: GitOps job updates

**Status**: draft  
**Date**: 2026-05-13

## Background

nomad-botherer currently detects drift between a Git repository and a live Nomad
cluster, but takes no action on it. This proposal describes how to extend it to
*apply* changes — turning it from a drift detector into a GitOps operator that
reconciles Nomad job state toward what is declared in Git.

The core questions are:

1. How should a job update be represented internally (and exposed via the JSON API)?
2. How should we use Nomad cluster state — particularly the Raft index and job
   version fields — to make updates safe and idempotent?
3. Which of three possible architectures should we adopt?

---

## What constitutes a job update

A GitOps update is triggered when `Check()` detects any of the following:

| Drift type | Desired action |
|---|---|
| `modified` | Re-register the job from the HCL file |
| `missing_from_nomad` | Register the job for the first time |
| `missing_from_hcl` | Deregister (or stop) the job |

The update carries enough context to execute the action and to record what
happened. It is distinct from a `JobDiff`: a diff is an observation, an update
is an intended transition.

---

## Go struct representation

The existing `JobDiff` struct in `internal/nomad/differ.go` captures the
observation side. A new `JobUpdate` struct captures the action side:

```go
// JobUpdate represents a single intended change to a Nomad job, derived from
// a detected diff between Git and the cluster. One diff produces one update.
type JobUpdate struct {
    // Unique identifier for this update, assigned by nomad-botherer.
    // Format: <job_id>/<git_commit_short> — stable across restarts.
    UpdateID string `json:"update_id"`

    JobID string `json:"job_id"`

    // Path to the HCL file that is the source of truth for this job.
    // Empty when operation is DEREGISTER (no HCL file for the job).
    HCLFile string `json:"hcl_file,omitempty"`

    // Git commit hash that triggered this update.
    GitCommit string `json:"git_commit"`

    Operation JobUpdateOperation `json:"operation"`
    Status    JobUpdateStatus    `json:"status"`

    // Nomad's JobModifyIndex at the time the drift was detected.
    // Used as a CAS (check-and-set) token when calling Jobs.Register().
    // Zero means the job did not exist in Nomad at detection time.
    NomadJobModifyIndex uint64 `json:"nomad_job_modify_index"`

    // Nomad's cluster Raft index at detection time; recorded for auditability.
    NomadRaftIndex uint64 `json:"nomad_raft_index"`

    DetectedAt string `json:"detected_at"`           // RFC3339
    AppliedAt  string `json:"applied_at,omitempty"`  // RFC3339; empty until applied
    Error      string `json:"error,omitempty"`
}

type JobUpdateOperation string

const (
    JobUpdateOperationRegister   JobUpdateOperation = "REGISTER"
    JobUpdateOperationDeregister JobUpdateOperation = "DEREGISTER"
)

type JobUpdateStatus string

const (
    JobUpdateStatusPending    JobUpdateStatus = "PENDING"
    JobUpdateStatusInProgress JobUpdateStatus = "IN_PROGRESS"
    JobUpdateStatusSucceeded  JobUpdateStatus = "SUCCEEDED"
    JobUpdateStatusFailed     JobUpdateStatus = "FAILED"
    JobUpdateStatusSuperseded JobUpdateStatus = "SUPERSEDED"
)
```

A `GET /api/v1/updates` endpoint gives operators visibility into the update queue,
returning the same JSON-serialised `[]JobUpdate` slice.

---

## Using Nomad cluster state

### JobModifyIndex as a CAS token

Nomad's `Jobs.Register()` accepts an `EnforceIndex` flag and a
`JobModifyIndex` value. When `EnforceIndex` is true, Nomad rejects the write
if the job's current `ModifyIndex` does not match the supplied value. This
prevents a stale update from overwriting a change that happened in Nomad between
the time of detection and the time of apply.

The flow is:

1. `Check()` calls `Jobs.Info()` to detect drift; the response includes
   `Job.JobModifyIndex`. Store this in `JobUpdate.nomad_job_modify_index`.
2. When applying, call `Jobs.Register()` with `EnforceIndex=true` and
   `JobModifyIndex` set to the stored value.
3. If Nomad returns a conflict error (HTTP 409), the job changed between
   detection and apply. Mark the update `FAILED`, re-run `Check()` immediately,
   and let the next cycle create a fresh update with current state.

For `missing_from_nomad` jobs, `nomad_job_modify_index` is 0 (job does not
exist). Nomad treats index 0 as "job must not already exist", which is correct.

### LastIndex for skipping redundant work

The existing `Differ` already caches `lastNomadIndex` from `Jobs.List()` and
skips per-job checks when both the Raft index and the git commit are unchanged.
The update machinery should respect the same signal: if a job's `JobModifyIndex`
has not advanced since the last successful apply, there is nothing new to apply.

Concretely: after a successful `REGISTER`, record the `JobModifyIndex` returned
by Nomad in the completed `JobUpdate`. On the next diff cycle, if
`Jobs.Info()` returns the same `JobModifyIndex`, the update for that job was
already applied and can be skipped.

### Job version for rollback awareness

Each job in Nomad has a `Version` field that increments on every registered
change. We do not use this for the apply decision, but it is worth recording in
the update so that a future rollback operator (not in scope here) can target a
specific version with `Jobs.Revert()`.

---

## Architecture alternatives

### Alternative A: in-process eager apply

**How it works**

`Check()` is extended to accept an optional `applier` interface. When drift is
detected, instead of (or in addition to) recording a `JobDiff`, it immediately
calls `applier.Apply(update)`. The apply happens synchronously within the same
goroutine that runs the diff check.

```
git pull → Check() → detect diff → Apply() → Jobs.Register/Deregister
                                 → record JobUpdate (SUCCEEDED or FAILED)
```

There is no separate queue and no background worker. The update is fire-and-forget
within the check loop.

**Nomad state use**: `EnforceIndex` CAS on register; re-check on conflict.

**Pros**

- Minimal new code. The applier is a thin wrapper around the existing Nomad
  client.
- Fastest convergence: drift is corrected in the same cycle it is detected.
- No state to manage beyond the existing `Differ` fields.

**Cons**

- Apply failures block or delay subsequent checks (if synchronous).
- No visibility into what was applied and when without external logging.
- Difficult to add approval gates or dry-run modes later.
- A misbehaving apply (e.g., long network timeout) holds the diff check mutex,
  delaying the next observation.

**Verdict**: good for a simple first pass but does not scale to approval
workflows or per-job apply rate limits.

---

### Alternative B: async update queue with reconciliation loop

**How it works**

`Check()` produces `JobUpdate` records with status `PENDING` and places them in
an in-memory queue. A separate goroutine, the reconciler, drains the queue and
applies updates to Nomad. The reconciler and the checker communicate through the
queue; neither blocks the other.

```
git pull → Check() → enqueue JobUpdate (PENDING)
                           ↓
                    reconciler goroutine
                           ↓
                    dequeue → Apply() → update status (SUCCEEDED/FAILED)
                                      → re-enqueue with backoff on transient failure
```

The queue is a simple slice protected by a mutex, or a buffered channel if
ordering guarantees are not needed. Updates for the same job are deduplicated:
if a `PENDING` update for job X already exists and a new diff cycle produces
another update for job X, the old one is marked `SUPERSEDED` and the new one
takes its place. This handles rapid Git pushes cleanly.

**Nomad state use**: `EnforceIndex` CAS; `JobModifyIndex` stored in the update
at detection time. On CAS conflict, update is marked `FAILED` and a fresh diff
check is triggered.

**Pros**

- Diff checks and applies are decoupled. A slow or failing apply does not affect
  detection latency.
- Per-update status is first-class: `GET /api/v1/updates` has meaningful data.
- Natural place to add apply rate limiting (one update per N seconds per job)
  or dry-run mode (process queue but skip the actual `Jobs.Register()` call).
- The `SUPERSEDED` status handles pushes that arrive faster than applies finish.

**Cons**

- More goroutines and synchronisation primitives to reason about.
- The queue is in-memory: a restart loses all `PENDING` updates. They will be
  recreated on the next diff cycle, but there is a window where an operator
  looking at `ListUpdates` sees no pending work despite a live drift.
- Ordering between jobs is not guaranteed. If job B depends on job A being
  registered first, the reconciler may apply B before A.

**Verdict**: the right default choice. Decoupling detection from application is
worth the complexity, and the restart gap is acceptable given the diff loop will
recreate pending updates within one cycle.

---

### Alternative C: plan-and-approve with GitHub as control plane

**How it works**

nomad-botherer does not apply changes autonomously. Instead, it posts a Nomad
plan output as a GitHub commit status or pull request check whenever drift is
detected on a push event. A human (or a separate CI job) approves or rejects
the plan. Approval triggers a webhook back to nomad-botherer (or directly
triggers a Nomad job run) that performs the actual apply.

```
git push → webhook → Check() → nomad plan → post plan as GitHub status/check
                                                     ↓ approved
                               ← approval webhook ←
                                     ↓
                               Apply() → Jobs.Register()
```

The approval signal can be a GitHub environment protection rule, a separate
approval workflow, or a simple comment-based trigger parsed from the webhook
payload. The plan output is stored transiently in nomad-botherer memory between
the push and the approval; on restart it is re-generated from the current diff.

**Nomad state use**: plan is run against the cluster at detection time.
`JobModifyIndex` from the plan response is stored and used as a CAS token when
the approval arrives. If the cluster state has changed by then, the apply is
rejected and a new plan must be posted.

**Pros**

- Humans review every change before it reaches the cluster. Suitable for
  regulated environments or production clusters shared by multiple teams.
- The plan output (already rendered by nomad-botherer's `render.go`) is a
  natural review artefact.
- No persistent state needed: on restart, re-plan and re-post.
- GitHub becomes the audit log for who approved what.

**Cons**

- Requires GitHub API write access (commit status or checks API).
- Convergence latency is bounded by human review time, not machine time.
- Stale approvals: if the cluster changes between plan and approve, the CAS
  rejection means the approval is wasted and the cycle must repeat.
- Does not work for automated rollouts (e.g., CD pipelines) without an
  automatic approver, at which point it collapses to Alternative B with extra steps.

**Verdict**: worth building on top of Alternative B for teams that need
approval gates, but not the right default starting point.

---

## Recommended path

Implement Alternative B (async queue) first. It gives immediate operational
value, is the most compatible with the existing Differ/Watcher architecture,
and provides the hooks needed to add Alternative C's approval layer later without
restructuring the core.

The `JobUpdate` struct additions above should land in the same change as the queue
implementation. The `GET /api/v1/updates` endpoint makes the queue observable
without requiring log scraping.

---

## Open questions

- **Deregister behaviour**: should `missing_from_hcl` trigger a deregister
  unconditionally, or should it require an explicit `gitops: deregister` flag in
  the job meta to prevent accidental deletions when a file is renamed?
- **Apply ordering**: if multiple jobs change in one commit, should they be
  applied in dependency order? Nomad does not expose a dependency graph, so this
  would require an explicit ordering field in HCL or job meta.
- **Rollback**: if a registered job's health checks fail after apply, should
  nomad-botherer trigger `Jobs.Revert()`? This needs a separate health-check
  polling loop and is out of scope for an initial implementation.
