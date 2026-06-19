# Design: automatic rollback when a change goes badly

**Status**: implemented — Unreleased. Phase 1 (the flap-loop guard, Approach A
and the tag variant Approach B) and Phase 2 (optional active rollback) both
shipped. See the CHANGELOG and the README "Rollback" section.
**Date**: 2026-06-17 (proposed, as research) · implemented 2026-06-19

> This records the research and the decisions behind the shipped rollback
> handling. Where the implementation diverged from this document it is noted
> inline below. The main divergences:
>
> - **Flags were renamed and consolidated.** The sketch proposed
>   `--block-known-failed` (bool), `--enable-rollback`, and
>   `--tag-failed-versions`. As shipped these are `--flap-guard=history|tag|off`
>   (one flag covering Approach A, Approach B, and disabling) and
>   `--allow-rollback`, each with a per-job meta override (`<prefix>_flap_guard`,
>   `<prefix>_rollback`). The default flap-guard mode is `history`.
> - **Active rollback is poll-based, not a long-running watch.** Rather than
>   moving an update into a watch state and blocking on `LatestDeployment` to a
>   terminal status, each diff cycle checks `LatestDeployment`; a `failed`
>   status enqueues a `REVERT`. This is restart-safe and stateless by
>   construction and sidesteps the dispatched-executor question for now, so the
>   proposed `--rollback-watch-timeout` was dropped (Nomad's `progress_deadline`
>   decides failure).
> - **Scope was narrowed to deployment-producing jobs.** Failure is keyed on a
>   `failed` deployment status, so batch/system/no-health-check jobs (no
>   deployment) get neither the guard nor active rollback. The open question
>   about defining failure for batch jobs is deferred, not answered.
> - **`ROLLED_BACK` was not added as a status.** A revert is a `REVERT`-operation
>   `JobUpdate` that reaches the normal `SUCCEEDED`/`FAILED` terminal states.

Related documents: [gitops-job-updates.md](gitops-job-updates.md) (the apply
path; "Rollback" open question and the dispatched-executor note),
[change-checkpointing.md](../proposals/change-checkpointing.md) (the no-external-database /
state-lives-in-Git-and-Nomad principle), [update-policies.md](update-policies.md)
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

Alternatively, when nomad-botherer observes a deployment fail, it tags the
failed version with `Jobs.TagVersion`. Tagged versions survive GC, so the guard
becomes durable across any time horizon and across restarts, recovered with a
cheap tag-name lookup instead of (or alongside) a spec comparison.

This is not the default because it is **state nomad-botherer writes into
Nomad** — the very thing the no-persistent-state rule pushes against. It is a
Nomad-native write (not a job-meta write, so it avoids the meta-drift problem),
and it is recoverable, but it accumulates tags that something must prune, and it
makes nomad-botherer responsible for state again. It is offered behind
`--flap-guard=tag` for operators who want hard durability; `history` is the
default.

**As shipped.** The tag name is `<prefix>-failed-<fingerprint>`, where
`<prefix>` is `--managed-meta-prefix` (default `gitops`) and `<fingerprint>` is
the same spec fingerprint Approach A compares (see "Spec fingerprinting" below).
Encoding the fingerprint in the tag name lets a later cycle recognise a
known-failed spec by reading the tag, with no recompute and even if the failed
deployment record itself has been GC'd. Because the tag name derives from the
prefix, `--flap-guard=tag` with an empty `--managed-meta-prefix` is rejected at
config load — an empty prefix would produce unrecognisable `-failed-<fp>` tags
and silently break durable blocking. A Nomad version carries at most one tag, so
a version already tagged (by us on an earlier cycle, or by anything else) is
left alone.

### Why the guard releases correctly

The guard keys on the **spec**, not the commit hash, so it releases the instant
Git moves to a spec that has not failed — a fix commit, or even a revert commit
in Git. A new commit that changes the spec is, by construction, not the
known-failed spec, so it applies normally. (Keying on spec rather than commit
also means a no-op commit that doesn't touch the job doesn't spuriously re-arm
or release the guard.)

### Spec fingerprinting (as shipped)

The "same spec" test is a SHA-256 over the job's JSON, normalised to drop
exactly what would otherwise cause spurious mismatches: Nomad-injected
bookkeeping (`Version`, `Stable`, `SubmitTime`, `ModifyIndex`, `JobModifyIndex`,
`CreateIndex`, `Status`, `StatusDescription`, `VersionTag`, `Namespace`), the
managed-prefix meta keys, and autoscaler-owned `Count`/`Scaling` on autoscaled
groups — the same exclusions the diff classifier makes. The candidate
fingerprint is computed from the HCL-parsed job; the failed-version fingerprint
is computed from the version Nomad stored (history mode) or read back from the
tag name (tag mode).

Failure is keyed on a **failed deployment status**, not merely a non-`Stable`
version: a job with no health checks is never `Stable`, so "not stable" would
mis-fire. This is why both the guard and active rollback are scoped to
deployment-producing jobs.

Comparing an HCL-parsed job against a Nomad-stored version is best-effort:
server-side defaulting can make a genuinely identical spec fingerprint
differently, in which case the guard *misses* and the bad spec is retried once
more and caught again. That degradation is one-way and safe — a *false block* of
a good change would need a SHA-256 collision.

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

This is the heavy, risky path and is **off by default** (`--allow-rollback`).
It duplicates machinery Nomad already has for the common case. The honest
recommendation is to make active rollback secondary and **prefer telling
operators to set `auto_revert` in their job HCL.**

**As shipped — poll-based, not a long-running watch.** The numbered sketch above
moves an update into a watch state and blocks on the deployment to a terminal
status; the implementation does not. Instead, each diff cycle the rollback poll
checks `LatestDeployment` for every managed job that has rollback enabled, and a
`failed` status enqueues a `REVERT` update to the last stable version (most
recent `Stable` version below the failed one, from `Jobs.Versions`). The applier
runs `Jobs.Revert(jobID, lastStable, enforcePriorVersion=<failedVersion>)` — the
same CAS guard. This sidesteps the long-running-watch and dispatched-executor
question entirely: there is no watch to outlive a process, so the proposed
`--rollback-watch-timeout` was dropped (Nomad's `progress_deadline` decides when
a deployment fails). The revert is a normal `REVERT`-operation `JobUpdate` that
reaches the usual `SUCCEEDED`/`FAILED` terminal states; no `ROLLED_BACK` status
was added. After a successful revert the live job is back at the stable version
while Git still wants the failed spec, so the next cycle's drift is held by the
flap-guard.

**auto_revert always wins.** If a rollback-enabled job's `update` stanza (at the
job or group level) also sets `auto_revert`, nomad-botherer stands down and lets
Nomad revert, logging the clash once per job. Even if that check were bypassed,
the CAS guard would reject the redundant revert because Nomad's own revert has
already moved the job off the failed version.

### Restart safety of active rollback

No durable state is needed. The poll recomputes from scratch every cycle: it
reads `LatestDeployment` and `Jobs.Versions` fresh, so a restart simply resumes
polling. A revert interrupted mid-call is safe to re-issue because `Jobs.Revert`
with `enforcePriorVersion` is idempotent against the current version, and the
`REVERT` update's stable ID (`<job_id>/revert-<failed_version>`) dedups a
re-enqueue of the same recovery.

## Configuration (as shipped)

The sketch's three booleans collapsed into one tri-state guard flag plus one
rollback flag, each with a per-job meta override.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--flap-guard` | `FLAP_GUARD` | `history` | Flap-loop guard mode: `history` (Approach A, ephemeral), `tag` (Approach B, durable), or `off`. Per-job override: `<prefix>_flap_guard`. `tag` requires a non-empty `--managed-meta-prefix`. |
| `--allow-rollback` | `ALLOW_ROLLBACK` | `false` | Active rollback for jobs without native `auto_revert`. Per-job override: `<prefix>_rollback`. |

The proposed `--rollback-watch-timeout` was dropped (the poll has no watch to
cap). The guard is not policy-aware: it engages for any would-be apply
regardless of `full`/`image-only`, because by the time a candidate reaches the
guard it has already passed the policy gate. Both features engage only for
deployment-producing jobs.

## Observability

Per the metrics convention, as shipped:

- `nomad_botherer_updates_blocked_known_failed_total{job}` — applies withheld by
  the flap-guard (the signal an operator watches to find a job stuck on a
  known-bad commit awaiting a fix in Git).
- `nomad_botherer_rollbacks_total{job,result}` — active-rollback outcomes:
  `queued` (a revert was enqueued), `deferred_auto_revert` (stood down for
  Nomad's `auto_revert`), `no_stable_version` (nothing to revert to).
- `nomad_botherer_failed_versions_tagged_total{job}` — failed versions tagged by
  `--flap-guard=tag`.
- `REVERT` joins `REGISTER`/`DEREGISTER` in
  `nomad_botherer_job_updates_total{operation,status}`.
- A new `apply_action` value `blocked_known_failed` on the diff, so `/diffs`,
  the API, and `/healthz` explain the hold (consistent with the existing
  apply-reason exposure).

The proposed `deployments_watched_total{result}` was not added: there is no
deployment watch in the poll-based design.

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
   not revert, behind `--allow-rollback`, with a CAS `Jobs.Revert`. Shipped as a
   per-cycle poll rather than a long-running watch, so the dispatched-executor
   question did not need answering: there is no watch to survive process death.

## Open questions (resolved)

- **Spec-equality fidelity.** *Resolved.* Version comparison needed its own
  normalization: the fingerprint strips the diff classifier's exclusions
  (`gitops_*` meta, autoscaler `Count`/`Scaling`) *and* the Nomad-injected
  fields (`SubmitTime`, `Version`, `ModifyIndex`, and the rest listed under
  "Spec fingerprinting"). Cross-source comparison stays best-effort and degrades
  one-way (a miss, never a false block).
- **GC window vs. retry tolerance.** *Resolved.* `history` accepts "retry at
  most once per `job_gc_threshold`"; operators who want hard durability use
  `--flap-guard=tag`. The guard does not currently surface how long ago the
  failure was — a possible future addition, not built.
- **What counts as "failed" for non-deployment jobs?** *Deferred by scoping
  out.* Both features apply only to deployment-producing jobs, keyed on a
  `failed` deployment status. Batch/system/no-health-check jobs get neither, so
  defining failure for them is not on the critical path.
- **Interaction with existing gates.** *Resolved.* The flap-guard runs last in
  `decideApplyAction`, after the policy / pre-existing-drift / meta-only /
  creation gates, so it only ever holds a would-be apply. Supersession is
  unchanged: a newer commit with a different spec is, by construction, not the
  known-failed spec, so it is not blocked.
- **Active rollback and `auto_revert` together.** *Resolved.* When a
  rollback-enabled job also sets `auto_revert`, nomad-botherer stands down and
  logs the clash once; the CAS guard on `Jobs.Revert` is a second line of
  defence against a double-revert.
