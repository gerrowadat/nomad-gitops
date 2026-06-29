# Common use cases

Recipes for the things people actually do with nomad-botherer, in roughly
increasing order of how much it is allowed to write. Each is a starting point —
follow the links for the full behaviour and edge cases.

Everything below assumes you have it [running](setup/running.md) and pointed at
your repo and cluster.

---

## Detect drift and alert — never write (the default)

**Goal:** know when the cluster has diverged from Git, change nothing.

Out of the box this is exactly what you get. Opt jobs in with
`meta { gitops_managed = "true" }` (or a glob), wire up the
[metrics and alerts](monitoring.md), and watch `nomad_botherer_drifted_jobs`.
No update-policy or apply flags needed — the default policy is `none`, so
nomad-botherer never writes.

```bash
./nomad-botherer --repo-url https://github.com/myorg/nomad-jobs.git \
  --nomad-addr http://nomad.example.com:4646
```

See [Job selection](job-selection.md) and [Monitoring](monitoring.md).

---

## Watch only specific jobs, or everything

**Goal:** choose which jobs are in scope.

- Per job (default): add `meta { gitops_managed = "true" }` to its HCL.
- By name: `--job-selector-glob='production-*'` (or `'*'` for all).

The two are a union. See [Job selection](job-selection.md).

---

## Auto-apply image bumps, hold everything else

**Goal:** a deployment that lets image tag changes flow automatically but holds
config/resource changes for a human.

Set the job's policy to `image-only`:

```hcl
job "api-server" {
  meta {
    gitops_managed       = "true"
    gitops_update_policy = "image-only"
  }
  # ...
}
```

An image-tag change in Git is re-registered automatically; anything else (env,
resources, constraints) is surfaced as drift and left for you. Note the *whole*
plan diff must be image-only — an image bump bundled with an env change in the
same commit is held in full. See [Update policies](applying-changes.md#update-policies).

---

## Full reconciliation for a job

**Goal:** the cluster should always match Git for this job.

```hcl
job "api-server" {
  meta {
    gitops_managed       = "true"
    gitops_update_policy = "full"
  }
  # ...
}
```

Any drift is re-registered from HCL (plan-first, CAS-guarded). To also let
nomad-botherer create the job the first time it appears in Git, add
`--enable-job-creation`. See [What gets applied](applying-changes.md#what-gets-applied-and-how).

---

## Turn reconciliation on for a whole deployment

**Goal:** make `full` the default for every managed job, rather than annotating
each one.

```bash
./nomad-botherer --default-update-policy=full ...
```

Per-job `gitops_update_policy` still overrides the default in either direction
(a delicate job can set `none` or `image-only`). The flag is the
deployment-level gate; out of the box it is `none`, so flipping it is the
explicit act that enables writing.

---

## Enrol jobs gradually / promote a policy

**Goal:** bring jobs under management one at a time, or tighten/loosen a job's
policy, without surprise mass-deploys.

Add `gitops_managed` (or widen `gitops_update_policy`, e.g. `image-only` →
`full`) in a commit. By default, drift that was **already there** when you made
that change is **not** retroactively applied — only changes committed afterwards
are. This keeps "I just opted this in" from deploying a backlog you didn't mean
to ship right now.

If you *do* want the existing drift applied at the moment of enrolment/promotion,
run with `--apply-existing-drift`. See
[Drift that pre-dates a scope change](applying-changes.md#drift-that-pre-dates-a-scope-change-opt-in-or-policy-widening).

---

## Remove a job through Git

**Goal:** deleting a job's HCL should stop the job.

This is off by default and heavily gated, because it is the one destructive
write nomad-botherer can make. With `--enable-deregister`, a job that is
**removed from the repo entirely** (file deleted or renamed) and still carries
`gitops_managed = "true"` with policy `full` is deregistered after a grace
period. Removing only the `gitops_managed` line (job still in the repo) never
deletes anything — it just stops managing the job. See
[Deregistration](applying-changes.md#deregistration-jobs-removed-from-the-repo).

---

## Recover automatically from a bad deploy

**Goal:** a bad change should not crash-loop or get re-applied forever.

The best answer is Nomad-native: put `update { auto_revert = true }` with real
health checks in the job. Nomad reverts a failed deployment itself, and
nomad-botherer's flap-loop guard (on by default) keeps it from re-pushing the
known-bad spec. For jobs that do not use `auto_revert`, `--allow-rollback` lets
nomad-botherer do the revert. See [Rollback](rollback.md).

---

## Run on the cluster it watches, with no token to manage

**Goal:** deploy under Nomad on an ACL-enabled cluster without minting and
rotating a token.

Use workload identity: `identity { file = true }` on the task plus an ACL policy
bound to the job. The example job is already set up for this. See
[Nomad access](setup/nomad-access.md).

---

## React to pushes immediately

**Goal:** apply/detect within seconds of a push instead of waiting for the poll
interval.

Configure a webhook. See [Webhooks](setup/webhooks.md).
