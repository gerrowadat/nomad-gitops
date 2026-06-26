# Design: per-job update policies

**Status**: implemented — v0.5.0 (`full`/`image-only`/`none`, the diff
classifier), with later refinements (meta-only handling, pre-existing-drift
gate) in v0.6.0, and the policy-widening ruling (issue #69) after v0.8.0.
See the CHANGELOG.
**Date**: 2026-06-11 (proposed) · moved to design 2026-06-17

> This records the thinking behind the shipped policy model. One notable
> change from the original proposal: the **default policy is `none`, not
> `full`**, and there is no separate global apply switch — the policy flag is
> the deployment-level gate (see the "policy key" section, corrected inline).
> The classifier lives in `internal/nomad/classify.go`.

## Background

The [GitOps job updates proposal](gitops-job-updates.md) describes how
nomad-botherer will apply detected drift to the cluster. That proposal treats
apply as all-or-nothing: a job is either managed (`gitops_managed = "true"`)
and fully reconciled, or not managed and never touched.

In practice, jobs want different degrees of automation. A stateless web
frontend can absorb any change Git throws at it. A database job might want
image version bumps applied automatically but anything touching volumes,
resources, or constraints held for a human. A particularly delicate job might
want drift *reported* but never applied.

This proposal adds a per-job policy key to the existing meta opt-in mechanism,
so that the degree of automation is declared in the job's HCL alongside the
opt-in flag — version-controlled, reviewable, and human-written (no
meta-drift; see [change-checkpointing.md](../proposals/change-checkpointing.md)).

---

## The policy key

```hcl
job "api-server" {
  meta {
    gitops_managed       = "true"
    gitops_update_policy = "image-only"
  }
}
```

`gitops_update_policy` takes one of three values:

| Value | Meaning |
|---|---|
| `full` | Any detected drift between the HCL and the cluster is applied (plan → CAS register, per the apply flow). |
| `image-only` | Drift is applied only when the *entire* plan diff is confined to Docker image references. Any other change — even one bundled in the same commit as an image bump — leaves the whole update unapplied and surfaced as a diff. |
| `none` | Drift is detected and surfaced (diffs API, metrics) but never applied. Detection-only, per job. |

A job with `gitops_managed = "true"` and no policy key gets the default
policy, which is a config flag:

| Flag | Env var | Default |
|---|---|---|
| `--default-update-policy` | `DEFAULT_UPDATE_POLICY` | `none` |

This proposal originally argued for a default of `full` guarded by a
separate global apply-mode switch. **As implemented, the default is `none`
and there is no separate switch**: the policy flag itself is the
deployment-level gate. Out of the box nothing is ever written; raising the
default or adding a per-job meta key is the explicit act that enables
applying. A job whose meta declares `full` or `image-only` is applied even
while the default is `none` — the meta key overrides in both directions.
Deployments enrol jobs gradually by promoting them one at a time with
explicit policy keys.

---

## Semantics by drift type

| Drift type | `full` | `image-only` | `none` |
|---|---|---|---|
| `modified` (image fields only) | apply | apply | surface only |
| `modified` (anything else) | apply | surface only | surface only |
| `missing_from_nomad` | register | surface only | surface only |
| `missing_from_hcl` | see below | never | never |

Notes:

- **Initial registration is `full`-only.** Registering a job for the first
  time is by definition not an image-only change; an `image-only` job that is
  missing from the cluster is surfaced as `missing_from_nomad` and left for a
  human to register. This keeps `image-only` true to its name: the only write
  it ever performs is a re-register where the plan shows image changes alone.
- **Deregistration is not enabled by any policy value.** Per the existing
  design intent, deregister requires the global enable flag *and*
  `gitops_managed = "true"` on the live job. When that lands, it should
  additionally require policy `full` (effective, after defaulting) — but the
  policy key never turns deregistration on by itself.
- For `missing_from_nomad` and `missing_from_hcl`, there is no live/parsed
  job pair to compare, so "image-only" has no meaningful diff to classify;
  hence the table above.

The policy is read from the *HCL side* (the parsed job in Git) when both
sides exist, because Git is the source of truth for intent. For
`missing_from_hcl` there is no HCL; the live job's meta is all we have, which
is consistent with the deregister guard already reading the live meta.

---

## Changing a policy: drift that accumulated under the stricter policy (issue #69)

When a managed job's `gitops_update_policy` is widened — `image-only` → `full`,
or `none` → either — the next reconcile could apply drift that was **live the
whole time but deferred by the stricter policy**. The motivating case: an
`image-only` job has a memory bump committed earlier and sitting unapplied
(correctly held by `image-only`); the operator then switches it to `full`. Should
the memory change ride along with the policy flip?

**Ruling: no, not by default. A policy widening is treated exactly like a job's
opt-in.** The reconciler already has a "pre-existing drift" gate
(`--apply-existing-drift`, default off) for the enablement case: when a job gains
`gitops_managed`, drift that pre-dates the opt-in is not retroactively applied;
only changes committed *after* the opt-in apply. A policy widening is the same
kind of event — it brings drift *into scope to apply* — so it gets the same
treatment and the same switch.

This was chosen over "apply all current drift on the switch" (the v0.8.0
behaviour) because:

- **Consistency.** Enablement and promotion are both "scope-widening" events;
  having them behave differently is surprising and was never a deliberate
  decision. One rule, one flag.
- **Least astonishment.** Changing a policy expresses intent about *future*
  reconciliation, not "deploy the backlog now." Bundling an older,
  deliberately-deferred change into the same reconcile as the policy flip can
  ship a change at a moment the operator didn't choose.
- **It's free of new state.** The signal is git history, which is already used
  for the opt-in gate.

### How it is decided

Generalise the opt-in test from "was the managed tag present at the parent
commit?" to "**would this diff have been applied under the job's effective scope
at the parent commit?**" A diff is pre-existing iff it would *not* have been
applied at the parent (the job was unmanaged there, or the parent's policy did
not cover this diff's class) but the scope at HEAD *does* cover it. Both inputs
come from the parent version of the job's HCL file (`HistorySource.FileAtParentOf`):
the managed tag via the existing regexp, the policy via a second regexp reading
`<prefix>_update_policy`. `policyPermits(policy, class)` mirrors the apply-time
policy gate.

Worked outcomes (default `--apply-existing-drift=false`):

| parent policy | HEAD policy | diff class | applied at HEAD? |
|---|---|---|---|
| image-only | full | other (e.g. memory) | **deferred** (came into scope) |
| image-only | full | image | applied (was already in scope) |
| none | full / image-only | any | **deferred** |
| full | full | any | applied (no widening) |
| full | image-only | other | n/a — blocked by HEAD policy anyway |

With `--apply-existing-drift=true`, every "deferred" above applies immediately.

### Boundaries

- A change bundled into the *same commit* as the widening is deferred (it did not
  predate the widening, but neither was it committed after it) — consistent with
  the opt-in rule, where drift in the opt-in commit is deferred and only later
  commits apply.
- A managed-meta-only diff (the policy flip with no other drift) is governed by
  the meta-only gate, not this one.
- The global `--default-update-policy` flag is not a per-job git change, so
  changing *that* is not detected as a per-job widening; only the per-job meta
  key's history is consulted.
- Glob-selected jobs have no opt-in moment and are exempt, as before.

---

## Classifying a diff as image-only

`Jobs.Plan()` returns a `JobPlanResponse` whose `Diff` field is a structured
tree: job-level `Fields` and `Objects`, then `TaskGroups[]`, then `Tasks[]`,
each with their own `Fields` and `Objects`. The Docker image lives at
task level as the `image` field inside the `Config` object.

A plan diff is image-only when every leaf marked changed (`Type` of `Added`,
`Deleted`, or `Edited`) is:

- a `Fields` entry named `image` inside a task's `Config` object, or
- an annotation-only artefact of that change (Nomad marks the enclosing
  task/group/job as `Edited` when any child changes; those container nodes
  are not leaves).

Everything else — env vars, resources, count, constraints, templates, meta
itself — disqualifies the diff.

Two existing principles interact with this classification and apply to *all*
policies, not just `image-only`:

- **Autoscaler ownership of `Count`.** Per the design intent (and nomad-ops
  prior art), when a job has a scaling policy, changes to `Count`/`Scaling`
  fields are excluded from drift consideration before policy is evaluated.
  A diff that is "image change + autoscaler-owned count change" is still
  image-only.
- **Plan-before-apply.** The classification is done on the same plan response
  that gates the register call, so there is no second round trip and no
  window where the classified diff differs from the applied one. The
  `JobModifyIndex` captured for CAS covers the classification too: if the
  cluster moves after the plan, the register is rejected regardless of
  policy.

The classifier should be a pure function over `*api.JobDiff`
(`classifyDiff(diff) DiffClass`) with table-driven tests covering: image-only,
image+env, image+count-with-scaling-policy, image+count-without-scaling-policy,
added task, removed group. It is the natural extension point if more
categories are wanted later (see open questions).

---

## Meta key syntax

HCL2 block attribute names cannot contain dots, so the block form works for
nomad-botherer's own keys:

```hcl
meta {
  gitops_managed       = "true"
  gitops_update_policy = "image-only"
}
```

The [Diun integration proposal](../proposals/diun-integration.md) reuses Diun's dotted
`diun.*` meta vocabulary, and dotted keys require the object-expression form.
HCL does not allow mixing the two forms in one block, so a job using both
families of keys writes:

```hcl
meta = {
  "gitops_managed"       = "true"
  "gitops_update_policy" = "image-only"
  "diun.enable"          = "true"
  "diun.include_tags"    = "^1\\."
}
```

This is cosmetic but worth documenting in the README when implemented,
because the error HCL produces when you mix forms is unhelpful.

---

## Relationship to image update tracking

This proposal governs the *downstream* direction: Git has changed, how much
of that change may be applied to Nomad automatically.

The [Diun integration proposal](../proposals/diun-integration.md) governs the *upstream*
direction: a newer image exists in a registry, but Git still pins the old
tag. That is not drift — Git and Nomad agree — so no policy value causes it
to be applied, and nomad-botherer does not even consume the notification:
Diun notifies whoever closes that loop (a human, or a separate bumper job),
Git gets updated by PR, and the change then flows back through this
proposal's policy gate like any other commit.

The two are deliberately orthogonal: `gitops_update_policy` never causes a
write that is not in Git, and Diun tracking never causes a write at all.

---

## Observability

Per the metrics convention:

- `nomad_botherer_updates_blocked_by_policy_total{job_id, policy}` — counter,
  incremented when a detected diff would have produced a `JobUpdate` but the
  effective policy filtered it out. This is the signal an operator watches to
  find jobs accumulating unapplied drift.
- The existing per-job drift gauges already cover the "surfaced but not
  applied" state; blocked updates remain visible as diffs.

Policy filtering happens at update *creation* time, not at apply time: a
policy-blocked diff never becomes a `JobUpdate`, so no new `JobUpdateStatus`
value is needed and the updates API only ever shows actionable intent.

---

## Open questions

- **More categories.** Is `image-only` the only restricted policy worth
  having, or do `env-only` / `resources-only` earn their keep? Proposal:
  ship `full`/`image-only`/`none` and let the diff classifier's design make
  additions cheap, rather than speculating now.
- **Per-task or per-group policy.** A job with a delicate stateful task and a
  disposable sidecar might want different policies per task. Nomad meta
  exists at job, group, and task level, so the mechanism allows it — but
  Nomad applies job registrations atomically, so a partial apply is not
  actually possible. Job-level only, unless a real need appears.
- **Policy on the live job disagreeing with HCL.** If the live job says
  `full` and the HCL says `none`, the HCL wins (Git is intent). But the
  *transition* commit that changes the policy is itself drift that the old
  policy may block applying. Probably fine — a human registered the policy
  change manually or the default policy applies — but worth a test case.
