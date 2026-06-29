# Applying changes (GitOps mode)

By default nomad-botherer only *detects* drift. It can also *apply* it —
re-registering jobs from their HCL when Git and Nomad disagree — but every write
is opt-in twice over: the default update policy is `none`, and first-time
registration needs its own flag on top.

This is the longest part of the docs because the apply side is deliberately
conservative and has several gates. If you just want to switch a job (or a whole
deployment) on, the [Use cases](use-cases.md) page has copy-paste recipes; this
page is the reference for *why* each gate exists and exactly when it fires.

## Update policies

Each managed job has an effective update policy, resolved as: the job's HCL meta
key wins; otherwise `--default-update-policy` applies.

```hcl
job "api-server" {
  meta {
    gitops_managed       = "true"
    gitops_update_policy = "image-only"
  }
}
```

| Policy | Behaviour |
|---|---|
| `none` | Drift is detected and surfaced (diffs, API, metrics) but never applied. The default. |
| `image-only` | Drift is applied only when the *entire* plan diff is confined to Docker image references. Any other change — even bundled in the same commit as an image bump — leaves the whole update unapplied and surfaced as a diff. |
| `full` | Any detected drift between HCL and the cluster is applied. |

An unrecognised policy value in job meta is treated as `none` and logged. The
meta key name follows `--managed-meta-prefix`: with the default prefix the key
is `gitops_update_policy`. Every job meta key and its valid values are
catalogued in the [Meta-key reference](meta-keys.md).

More generally, any meta key under the managed prefix that nomad-botherer cannot
act on is flagged, because such keys silently change behaviour: an unknown key (a
typo like `gitops_managd`, or `gitops.managed` with a dot) is logged at WARN, and
a recognised key with an unusable value (such as `gitops_managed = "True"` — only
lowercase `true`/`false` count) is logged at ERROR. Each unique issue is logged
once per process and counted every cycle in `nomad_botherer_meta_key_issues_total`.
Both the HCL and the live job's meta are checked.

*Changes* to these keys are tracked too: when a job gains or loses
`gitops_managed`, switches update policy, or any prefix key appears, disappears,
or changes value — on either the HCL side or the live job — nomad-botherer logs
the transition at INFO with the old and new values and what it will do to honour
the change (e.g. "job is now opted in: it will be diffed and applied per its
effective update policy", or "opt-in removed but the job still matches
`--job-selector-glob` and remains watched"). A manual `nomad job run` that
silently strips the keys from the live job is logged the same way. Transitions
are counted in `nomad_botherer_meta_key_changes_total`. The first check after
startup is a baseline and logs nothing.

## Changes to our own meta keys are not, on their own, drift

Because Git is the source of truth for the `gitops_*` keys, nomad-botherer reads
them straight from the HCL — the running job does not need to carry them for the
tool to behave correctly. So when a commit adds or changes one of *our* keys and
nothing else differs, that diff is **managed-meta-only**:

- It is **not applied** by default. Re-registering a running job purely to stamp
  `gitops_managed` onto it would be a disruptive change for no functional gain.
  The keys converge opportunistically on the next *real* update — when an image,
  env, resource, or other change re-registers the job, the current HCL (meta
  included) is what gets written. This holds even under an `image-only` policy:
  an image bump carries the meta along.
- It is **not counted as drift** by default, so it does not show up on `/diffs`
  or `/healthz` and does not move the drift metrics — these expected differences
  should not page anyone. They are surfaced instead by
  `nomad_botherer_meta_only_diffs_total{job}` and the meta-change logs above.

Both behaviours are independently configurable: `--apply-meta-only-changes` makes
such a diff trigger an update (subject to the normal policy — a pure meta change
is still not an image change, so `image-only` keeps blocking it), and
`--count-meta-only-changes` makes it count as drift. A diff that mixes a meta-key
change with any *other* change is a normal diff and is unaffected by these flags.

## Drift that pre-dates a scope change (opt-in or policy widening)

A change can bring drift that was already there *into scope* to apply. Two such
changes are treated **identically**:

- **Enablement** — you add `gitops_managed` to a job that already differs from
  its HCL (say you bumped its image in Git a while ago, and only now opt it in).
- **Policy widening** — you widen a managed job's `gitops_update_policy` to cover
  drift the stricter policy had been deferring. The motivating case: an
  `image-only` job has a non-image change (e.g. a memory bump) sitting unapplied;
  you then switch it to `full`. The memory change was live drift the whole time,
  merely held back by `image-only`.

In both cases the drift is **pre-existing**, and by default nomad-botherer does
**not** apply it. Changing a job's scope expresses intent about *future*
reconciliation, not "deploy the backlog now": it should not, on its own, trigger
a mutation from drift you may not have intended to ship at that moment. Only
changes committed *after* the scope change apply.

Set `--apply-existing-drift` to apply pre-existing drift at the scope change
instead — then the job converges to its HCL as soon as the change lands. This is
one switch for both cases, deliberately, so enablement and policy promotion never
diverge (see [issue #69](https://github.com/gerrowadat/nomad-botherer/issues/69)).

The decision is made from **git history**, so it holds the same whether the
change lands while nomad-botherer is running or before it starts (a fresh start
or a restart). For a job's HCL file at HEAD, the rule is: a diff is pre-existing
if it would **not** have been applied under the job's effective scope in the
commit *before* HEAD — the job was unmanaged there, or its policy there did not
cover this diff's class — but the scope at HEAD does cover it. A job whose tag
and policy were already established before HEAD reconciles normally; a file
created with the tag in a single commit is not a retroactive opt-in (the tag and
spec arrived together) so it applies. Because the signal is git-derived rather
than remembered, a restart never freezes an already-managed cluster. Jobs
selected by `--job-selector-glob` are always in scope and have no opt-in moment,
so this gate never applies to them. (The global `--default-update-policy` is not
a per-job git change, so changing *that* flag is not detected as a per-job scope
widening.) The freeze is counted in
`nomad_botherer_updates_blocked_preexisting_total{job}`, and each held diff is
shown on `/diffs` and the API with its reason (see
[Why a diff is or is not applied](#why-a-diff-is-or-is-not-applied)).

## What gets applied, and how

| Drift type | Action |
|---|---|
| `modified` | Re-register the job from HCL — if the policy allows the change. |
| `missing_from_nomad` | Register the job for the first time — only with `--enable-job-creation` *and* an effective policy of `full` (a first registration is never an image-only change). Dead jobs count as missing here. |
| `missing_from_hcl` | Left running and observation-only by default. With `--enable-deregister`, a job that was *removed from the repo entirely* (file deleted or renamed) is deregistered — see [Deregistration](#deregistration-jobs-removed-from-the-repo). |

Every apply is conservative by construction:

- **Plan first.** A job is never registered without a fresh `Jobs.Plan()`; if
  the plan shows nothing left to apply, the update completes as a no-op.
- **CAS on every write.** `Jobs.Register()` runs with `EnforceIndex` and the
  `JobModifyIndex` captured at detection time. If the job changed in Nomad
  between detection and apply, the write is rejected, the update is marked
  `FAILED`, and the next diff cycle re-detects with current state. For new jobs
  the index is 0, which Nomad reads as "must not already exist".
- **The autoscaler owns Count.** Task groups with a scaling policy register with
  `PreserveCounts`, and Count/Scaling changes on those groups neither trigger nor
  block an update.

Detection and application are decoupled: diffs land in an in-memory update queue
drained by a separate apply loop, so a slow or failing apply never delays the
next check. If a newer commit arrives before an older update is applied, the
older update is marked `SUPERSEDED` — the most recent intended state wins. The
queue is deliberately not persisted: after a restart the next diff cycle rebuilds
it from Git and Nomad, which together hold all durable truth. The queue is
visible at `GET /api/v1/updates`.

Each update carries a stable ID (`<job_id>/<short_commit>`), the operation,
status (`PENDING`, `IN_PROGRESS`, `SUCCEEDED`, `FAILED`, `SUPERSEDED`), the
policy that allowed it, and the CAS token used.

The design background is in [`design/gitops-job-updates.md`](design/gitops-job-updates.md)
and [`design/update-policies.md`](design/update-policies.md).

## Deregistration (jobs removed from the repo)

nomad-botherer does not look after jobs *going away* by default — a job either
enters its purview and stays in it, or stops being GitOps-managed and is left
running. There are two ways a managed job leaves scope, both logged:

- **The `gitops_managed` tag is removed** (the job is still declared in the
  repo). The job stops being managed and is **left running, untouched** —
  removing one line should never delete a job whose full spec is still in Git.
  This is logged via the meta-change tracking and is never a deregistration.
- **The job is removed from the repo entirely** — its HCL file is deleted, or the
  job is renamed (so the old ID no longer appears in any HCL). The still-running
  old job is surfaced as a `missing_from_hcl` diff and logged as having left
  management.

By default the removed-from-repo job is also just left running
(`observation_only`). `--enable-deregister` turns on actually deregistering it,
but only under all of these guards:

- The live job carries `gitops_managed = "true"` (it was genuinely under
  management; a job that merely matched `--job-selector-glob` is never
  deregistered).
- Its effective update policy (read from the live job's meta) is `full`.
- It has been continuously orphaned for `--deregister-grace` (default `5m`),
  which absorbs transient renames and mid-edit commits.
- Immediately before deregistering, live state is re-checked: the job must still
  exist and still carry the tag, or the deregistration is abandoned ("recheck,
  don't remember").

Deregistration is a **graceful stop** by default (the job becomes `dead` but
stays queryable and is garbage-collected by Nomad later); `--deregister-purge`
removes it from Nomad's state immediately. Jobs leaving management are counted in
`nomad_botherer_jobs_left_management_total{job,reason}`.

## Why a diff is or is not applied

Every diff carries an `apply_action` describing what nomad-botherer will do about
it, so you never have to scrape logs to find out why a drift is sitting
unapplied. It appears on `/diffs` (as a `→ …` line under each job), in the
`/api/v1/diffs` and `/healthz` JSON (the `apply_action` field), and is documented
in the OpenAPI spec. Values:

| `apply_action` | Meaning |
|---|---|
| `queued` | An update was enqueued and will be applied. |
| `blocked_by_policy` | The effective update policy disallows it (e.g. `none`, or `image-only` for a non-image change). |
| `blocked_preexisting_drift` | The drift pre-dates the scope change that brought it in — the job's opt-in, or a policy widening (e.g. `image-only` → `full`); set `--apply-existing-drift` to apply. |
| `blocked_creation_disabled` | First-time registration needs `--enable-job-creation`. |
| `skipped_meta_only` | The change is confined to `gitops_*` meta keys (only shown when `--count-meta-only-changes` is on). |
| `observation_only` | `missing_from_hcl`: running but absent from the repo, left untouched (deregistration disabled, or the job is not deregister-eligible). |
| `queued_deregister` | The job was removed from the repo and will be deregistered. |
| `deregister_pending_grace` | Removed from the repo; deregistration is waiting out `--deregister-grace`. |
| `no_actionable_change` | The only diff is autoscaler-owned Count/Scaling churn. |
| `blocked_known_failed` | The flap-loop guard is holding the apply: this spec matches a recent deployment that failed. Released when Git moves to a spec that has not failed. See [Rollback](rollback.md). |
