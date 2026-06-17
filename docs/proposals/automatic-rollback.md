# Proposal: automatic rollback when a change goes badly

**Status**: draft (research)
**Date**: 2026-06-17

Related proposals: [gitops-job-updates.md](../design/gitops-job-updates.md) (the apply
path; "Rollback" open question and the dispatched-executor note),
[change-checkpointing.md](change-checkpointing.md) (the no-external-database /
state-lives-in-Git-and-Nomad principle), [update-policies.md](../design/update-policies.md)
(per-job control over what is applied).

## Background

nomad-botherer applies drift by re-registering a job from its HCL. Today the
apply path stops at "Nomad accepted the registration": the update is marked
`SUCCEEDED` the moment `Jobs.Register` returns, with no awareness of whether
the resulting deployment actually became healthy. If a change is bad — the new
image crash-loops, a config error fails health checks — nomad-botherer neither
notices nor reacts.

This proposal researches how Nomad handles a bad change, surveys prior art, and
designs the safest, most reliable way for nomad-botherer to add **optional**
automatic rollback. Two properties shape the whole design:

- **No persistent state we are responsible for.** Per the checkpointing
  proposal, all durable truth lives in Git (desired state) and Nomad (actual
  state, version history, deployment outcomes). nomad-botherer must be able to
  die at any instant and recompute. A rollback feature must not introduce a
  journal, a database, or a "last-known-bad" file of our own.
- **The flap-loop is the real hazard.** The naive failure mode is worse than
  doing nothing: nomad-botherer applies commit `C`, the deployment fails, Nomad
  (or we) revert to the prior version, the *next* diff cycle sees that Git still
  wants `C`, re-applies it, it fails again — forever. Any rollback design must
  answer "have we already tried this exact change and seen it fail?" without
  holding that answer in our own state.

## What Nomad already does — use it first

Nomad has a mature, native rollback mechanism. The single most important
conclusion of this research is: **nomad-botherer should not reimplement
health-watching rollback. It should lean on Nomad's, and confine its own work
to not fighting it and to not re-pushing a known-bad spec.**

### Deployments and `auto_revert`

For a **service job with an `update` stanza**, every registration creates a
*deployment*. The `update` block governs the rollout:

- `health_check` (`checks` / `task_states` / `manual`) — how an allocation's
  health is judged. `checks` requires the tasks running *and* their service
  checks passing.
- `min_healthy_time`, `healthy_deadline`, `progress_deadline` — the timers. If
  allocations do not reach healthy before `progress_deadline`, the deployment
  **fails**.
- `auto_revert = true` — on deployment failure, Nomad automatically reverts the
  job to the **last stable version** and redeploys it.
- `canary`, `auto_promote` — canary rollouts; a failed canary fails the
  deployment (and triggers `auto_revert` if set).

A job version is **stable** once a deployment placed all of its allocations
healthy. Nomad keeps a version history and a `Stable` flag per version, and
exposes `Jobs.Versions()`, `Jobs.Revert()`, `Jobs.Stable()`,
`Jobs.LatestDeployment()`, and the `Deployments` API.

So for the common case — a service job that declares
`update { auto_revert = true }` with health checks — **rollback already
happens, correctly, without nomad-botherer involved at all.** Nomad watches
health, fails the deployment, and reverts to the last healthy version. This is
the gold standard: it is in-cluster, survives nomad-botherer restarts, and is
the same machinery `nomad job run` users already rely on.

### Where native auto-revert does *not* apply

The gaps are what a nomad-botherer feature could fill:

- **Jobs with no `update` stanza, or `health_check = "task_states"` with no
  real checks.** These may not produce a meaningful deployment, or mark
  "healthy" as soon as the task process is running — a crash-loop that restarts
  fast enough can still look healthy. `auto_revert` only reverts on *deployment*
  failure.
- **Batch / sysbatch / system jobs.** These do not create deployments, so
  `auto_revert` does not apply at all. Failure shows up only as failed
  allocations.
- **Jobs whose authors did not set `auto_revert`.** Common; the default is
  `false`.

### Native gotchas worth knowing (reliability caveats)

- Reversion is itself a new deployment that goes back **through the canary
  phase** (hashicorp/nomad#10882), so recovery is not instant.
- `auto_revert` can try to revert to a version that has been **purged/GC'd**
  (hashicorp/nomad#3052), which then fails — relevant to how long the "last
  stable" version survives (see version GC below).
- A `deployment fail` is not immediate; it waits out the deadlines
  (hashicorp/nomad#10882, #10881).

## Prior art

- **Nomad native (`auto_revert`)** — described above. The reference
  implementation; everything else should defer to it.
- **Levant** (surveyed in `docs/prior-art.md`) — a single-shot deploy tool that
  watches a deployment to completion and supports canary auto-promotion and
  auto-revert. It is a thin wrapper over the same deployment-watch + revert
  APIs; useful as a model for *how* to watch a deployment, not as a
  reconciler.
- **Argo CD (Kubernetes GitOps)** — directly relevant to the flap-loop problem.
  Argo's automated sync **will not re-attempt a sync against the same
  commit-SHA + parameters once it has failed** ("Skipping auto-sync: failed
  previous sync attempt"); self-heal only retries after a timeout, and a
  rollback cannot run while auto-sync is enabled (you pause auto-sync to roll
  back). Argo provides exactly the hysteresis we need — but it stores the
  last-failed-revision in **its own** controller state, which is precisely what
  we want to avoid. The design question for us is: *get the same hysteresis from
  Nomad's state instead of our own.*
- **Flux / Argo Rollouts** — progressive-delivery controllers that watch metrics
  and roll back; far heavier than warranted here and Kubernetes-specific.

## The two problems to solve

Even when leaning entirely on Nomad's `auto_revert`, nomad-botherer must handle
two things, because it is a *continuous reconciler* (Levant and `nomad job run`
are one-shot and never see the next cycle):

### Problem 1 — Don't fight the revert (the flap-loop guard)

After Nomad auto-reverts a failed deployment of commit `C`, the live job is back
at the prior version's spec. nomad-botherer's next diff cycle compares Git
(which still says `C`) against the live (reverted) job, sees drift, and would
re-register `C` — re-triggering the failure. This must be prevented, and it is
the part the user specifically wants: *"have we tried this exact change before?"*

### Problem 2 — Roll back jobs Nomad won't (optional, heavier)

For jobs without `auto_revert` (no deployment, or author didn't opt in),
nomad-botherer could watch the outcome itself and call `Jobs.Revert()`. This is
the genuinely new, genuinely risky capability.

## Detecting "a change went badly" — entirely from Nomad

All signals are queryable; none need to be stored by us.

| Job kind | Failure signal | API |
|---|---|---|
| Service + `update` | Deployment status `failed` | `Jobs.LatestDeployment()` / `Deployments.Info()` |
| Any registered job | Version not `Stable`, newer stable version exists below it | `Jobs.Versions()` (`Job.Stable`, `Job.Version`) |
| Batch / no deployment | Failed allocations past restart budget | `Jobs.Allocations()` (`AllocClientStatusFailed`) |

The cheapest, most reliable signal is the **deployment outcome** for service
jobs, polled after an apply: `running → successful` or `running → failed`. For
the flap-guard we additionally need to *remember the verdict across cycles
without storing it* — which Nomad's version history gives us.

## The flap-loop guard, without persistent state

This is the heart of the proposal and the user's main concern. The question
each cycle is: **"the HCL at the current commit produces spec S; has a recent
registration of spec S already failed?"** If yes, hold — do not re-apply —
until Git moves on.

Two ways to answer it from Nomad, not from our own state:

### Approach A — query job version history (recommended, ephemeral)

For the job, call `Jobs.Versions(jobID, diffs=true)`. Nomad returns every
retained version with its full spec and `Stable` flag. The guard:

1. Canonicalize the current HCL job the same way the plan/diff already does
   (Nomad `ParseHCL(canonicalize=true)`, then ignore our own `gitops_*` meta
   keys and autoscaler-owned `Count`/`Scaling`, consistent with the existing
   diff classifier).
2. Find a recent version whose canonicalized spec **equals** that, and which is
   **not** `Stable` (its deployment failed or never became healthy) and was
   superseded by a revert to an older stable version.
3. If found → the current commit's spec is **known-failed** → do not enqueue an
   apply. Surface the diff with a new `apply_action` such as
   `blocked_known_failed` so an operator sees *why* it is sitting unapplied.

This needs **no state of our own**: the failed attempt is recorded by Nomad as a
non-stable version in the job's history. It is "heavy querying" (a `Versions`
call per managed job that is currently drifted), but it is read-only and bounded
to drifted jobs.

**Bound: version GC.** Nomad garbage-collects *untagged* dead versions after
`job_gc_threshold` (default ~hours). So the guard is **best-effort with a
time horizon**: if the failed version is GC'd, we will re-attempt the bad spec
once more, observe the failure again, and re-engage the guard. This is
acceptable and self-correcting — it degrades to "retry at most once per GC
window", never to a tight loop, and it matches the project's "recompute from
Git + Nomad, tolerate a cycle of latency" stance exactly. It is strictly better
than today (an unbounded loop) and needs nothing durable.

### Approach B — tag the failed version (durable, not the default)

Alternatively, when nomad-botherer observes a deployment fail, it could
`Jobs.TagVersion(jobID, failedVersion, "gitops-failed-<commit-short>")`. Tagged
versions survive GC, so the guard becomes durable across any time horizon and
across restarts, checked with a cheap tag lookup instead of a spec comparison.

This is rejected as the default because it is **state nomad-botherer writes into
Nomad** — the very thing the no-persistent-state rule pushes against. It is a
Nomad-native write (not a job-meta write, so it avoids the meta-drift problem),
and it is recoverable, but it accumulates tags that something must prune, and it
makes nomad-botherer responsible for state again. Offer it behind a flag for
operators who want hard durability; keep Approach A as the default.

### Why the guard releases correctly

The guard keys on the **spec**, not the commit hash, so it releases the instant
Git moves to a spec that has not failed — a fix commit, or even a revert commit
in Git. A new commit that changes the spec is, by construction, not the
known-failed spec, so it applies normally. (Keying on spec rather than commit
also means a no-op commit that doesn't touch the job doesn't spuriously re-arm
or release the guard.)

## Active rollback for jobs Nomad won't revert (optional)

For service jobs that did not opt into `auto_revert`, and for the operator who
wants nomad-botherer to centralize the behaviour, an *active* rollback:

1. After `Jobs.Register`, do not mark the update `SUCCEEDED` immediately. Move
   it to a new state and **watch the deployment** (`LatestDeployment` poll until
   terminal, or watch allocations for non-deployment jobs), bounded by a
   timeout (`--rollback-watch-timeout`, derived from the job's
   `progress_deadline` where present).
2. On `successful` → `SUCCEEDED`. On `failed` → determine the **last stable
   version** from `Jobs.Versions()` (most recent `Stable` version below the
   failed one) and call
   `Jobs.Revert(jobID, lastStable, enforcePriorVersion=<failedVersion>)`. The
   `enforcePriorVersion` is a CAS guard: the revert only lands if the job is
   still at the failed version, so a concurrent human change is never stomped.
3. Mark the update `ROLLED_BACK`; the flap-guard (above) then prevents
   re-applying the same spec.

This is the heavy, risky path and must be **off by default** (`--enable-rollback`).
It duplicates machinery Nomad already has for the common case, and — as the
job-updates proposal already notes — *watching a deployment is a long-running
phase that outlives a single nomad-botherer process*, which is the strongest
argument for the dispatched-executor model. The honest recommendation is to
make active rollback secondary and **prefer telling operators to set
`auto_revert` in their job HCL.**

### Restart safety of active rollback

No durable state is needed even here. On restart, in-flight watches are
recomputed: for each managed job, query `LatestDeployment`; if it is `running`
and was triggered by a version whose spec matches our last applied intent,
resume watching; if it already failed and Nomad/we reverted, the flap-guard sees
a known-failed spec and holds. A revert that was interrupted mid-call is safe to
re-issue because `Jobs.Revert` with `enforcePriorVersion` is idempotent against
the current version.

## Configuration (sketch)

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--block-known-failed` | `BLOCK_KNOWN_FAILED` | `true` | The flap-loop guard (Approach A). Read-only; safe to default on once apply is enabled. |
| `--enable-rollback` | `ENABLE_ROLLBACK` | `false` | Active rollback (watch deployment, `Jobs.Revert`) for jobs without native `auto_revert`. |
| `--rollback-watch-timeout` | `ROLLBACK_WATCH_TIMEOUT` | `10m` | Cap on watching a deployment before giving up (falls back to surfacing drift). |
| `--tag-failed-versions` | `TAG_FAILED_VERSIONS` | `false` | Durable known-failed via version tags (Approach B) instead of ephemeral history query. |

The guard could also be made policy-aware (only engage for jobs whose effective
update policy is `full`/`image-only`), consistent with the existing apply gates.

## Observability

Per the metrics convention:

- `nomad_botherer_deployments_watched_total{result}` — `successful` / `failed` /
  `timeout`, for the active-rollback path.
- `nomad_botherer_rollbacks_total{job,result}` — active `Jobs.Revert` calls.
- `nomad_botherer_updates_blocked_known_failed_total{job}` — applies withheld by
  the flap-guard (the signal an operator watches to find a job stuck on a
  known-bad commit awaiting a fix in Git).
- A new `apply_action` value `blocked_known_failed` on the diff, so `/diffs`,
  the API, and `/healthz` explain the hold (consistent with the existing
  apply-reason exposure).

## Alternatives summary

| Option | What it is | Verdict |
|---|---|---|
| **A. Lean on `auto_revert` + flap-guard** | Nomad reverts; we just don't re-push the known-bad spec, detected from version history | **Recommended.** Safest, most reliable, no new durable state, no duplicated machinery. |
| B. Active rollback via `Jobs.Revert` | We watch the deployment and revert jobs Nomad won't | Optional, flagged off. Heavy; long-running watch; prefer `auto_revert`. |
| C. Tag failed versions | Durable known-failed marker in Nomad | Flagged alternative to A's history query; we become responsible for state again. |
| D. Surface only | Mark post-apply failure as a diff, never act | The fallback when nothing is enabled; still benefits from the flap-guard so it does not loop. |

## Recommended path

1. **Phase 1 (read-only, high value): the flap-loop guard.** Implement Approach
   A — after an apply, and on every subsequent cycle for a drifted managed job,
   consult `Jobs.Versions()` and withhold re-applying a spec that a recent
   non-stable version already represents. Surface it as `blocked_known_failed`.
   This is the cheapest change, introduces no durable state, and removes the
   worst failure mode (the apply→fail→revert→re-apply loop) whether or not the
   job uses `auto_revert`. It also makes nomad-botherer a *good citizen*
   alongside native `auto_revert`: Nomad reverts, we stay out of the way.

2. **Encourage `auto_revert` in managed HCL.** Document that the supported way
   to get automatic rollback is `update { auto_revert = true }` with real health
   checks; nomad-botherer's job is to detect and not fight it. Optionally add a
   meta-key validation warning when a `full`-policy job has no `auto_revert`.

3. **Phase 2 (optional, flagged): active rollback.** Only for jobs Nomad will
   not revert, behind `--enable-rollback`, with the deployment-watch and CAS
   `Jobs.Revert`. Treat the long-running watch honestly: recompute on restart;
   revisit the dispatched-executor model if the watch needs to survive process
   death cleanly.

## Open questions

- **Spec-equality fidelity.** Comparing canonicalized job specs across versions
  must ignore exactly what the diff classifier ignores (`gitops_*` meta,
  autoscaler `Count`/`Scaling`) and nothing more, or the guard will mis-fire.
  Is the existing classification reusable as-is, or does version comparison need
  its own normalization (e.g. Nomad-injected fields like `SubmitTime`,
  `Version`, `ModifyIndex`)?
- **GC window vs. retry tolerance.** Is "retry the bad spec at most once per
  `job_gc_threshold`" acceptable for all operators, or do some need the durable
  (tag) guard? Should the guard surface *how long ago* the failure was?
- **What counts as "failed" for non-deployment jobs?** Batch jobs legitimately
  have failed allocations within a restart budget. Defining failure for them is
  fuzzier than a deployment status and may be out of scope for Phase 1.
- **Interaction with existing gates.** How does `blocked_known_failed` compose
  with `--apply-existing-drift`, supersession, and the meta-only handling? A
  newer commit that supersedes a known-failed pending update should clear the
  guard for the superseded spec.
- **Active rollback and `auto_revert` together.** If a job has *both*
  `auto_revert` and `--enable-rollback`, nomad-botherer must defer to Nomad and
  never double-revert; the deployment-watch should recognise a Nomad-initiated
  revert in progress and stand down.
