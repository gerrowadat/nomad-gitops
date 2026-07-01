# FAQ & gotchas

Behaviour that is deliberate but not obvious. Most of these are consequences of
the [design philosophy](philosophy.md) — conservative by default, Git as the
source of truth, no persistent state.

## My job isn't being watched

A job is only watched if it matches a selection criterion: it declares
`meta { gitops_managed = "true" }`, **or** its name matches `--job-selector-glob`.
With the defaults (no glob), only the meta tag selects. Check
`GET /api/v1/selected-jobs` or the `/` status page to see what is in scope and
why. See [Job selection](job-selection.md).

## Drift is detected but nothing is applied

This is the default and is intentional: the default update policy is `none`, so
nomad-botherer detects and reports drift but never writes. Applying is opt-in —
set a per-job `gitops_update_policy` or raise `--default-update-policy`. Every
diff carries an `apply_action` (on `/diffs`, the API, `/healthz`) telling you
exactly why it was or wasn't applied. See
[Applying changes](applying-changes.md) and
[Why a diff is or is not applied](applying-changes.md#why-a-diff-is-or-is-not-applied).

## I opted a job in, but an older change in Git didn't deploy

When you add `gitops_managed` to a job that *already* differs from its HCL, that
pre-existing drift is **not** applied by default — only changes committed *after*
the opt-in are. Opting a job in expresses intent about future reconciliation, not
"deploy whatever's already different right now." Use `--apply-existing-drift` to
apply it at opt-in. See
[Drift that pre-dates a scope change](applying-changes.md#drift-that-pre-dates-a-scope-change-opt-in-or-policy-widening).

## I switched a job `image-only` → `full` and a previously-held change didn't deploy

Same rule as opt-in, by design: widening a policy brings drift *into scope*, and
drift that accumulated under the stricter policy is treated as pre-existing —
deferred by default, applied with `--apply-existing-drift`. A change committed
after the policy switch applies normally. See
[Drift that pre-dates a scope change](applying-changes.md#drift-that-pre-dates-a-scope-change-opt-in-or-policy-widening).

## `image-only` didn't apply my image bump

`image-only` applies a diff only when the **entire** plan diff is image
references. If the same commit also changed an env var, a resource, or anything
else, the whole update is held — not just the non-image part. Split the image
bump into its own commit, or use `full`. See
[Update policies](applying-changes.md#update-policies).

## I changed a `gitops_*` meta key in Git but `/diffs` shows nothing

A diff whose *only* change is to nomad-botherer's own meta keys (e.g. adding
`gitops_managed`, switching `gitops_update_policy`) is **managed-meta-only**: by
default it is neither counted as drift nor applied on its own. Re-registering a
running job just to stamp those keys on it would be disruptive for no functional
gain — the HCL is already authoritative, and the keys ride along the next real
update. Surfaced instead by `nomad_botherer_meta_only_diffs_total` and the
meta-change logs. `--count-meta-only-changes` / `--apply-meta-only-changes` change
this. See
[Changes to our own meta keys](applying-changes.md#changes-to-our-own-meta-keys-are-not-on-their-own-drift).

## I removed `gitops_managed` and the job is still running

Correct — removing the tag never deletes anything. The job simply stops being
managed and is left running untouched; removing one line should not destroy a job
whose full spec is still in Git. Deregistration happens only when a job is
**removed from the repo entirely** (file deleted/renamed) **and**
`--enable-deregister` is set, under several more guards. See
[Deregistration](applying-changes.md#deregistration-jobs-removed-from-the-repo).

## A stopped (dead) job shows as `missing_from_nomad`

By default a dead job is treated the same as a missing one — a stopped job is
expected state. Pass `--include-dead-jobs` to compare dead jobs' specs against
their HCL instead.

## My autoscaled job's count keeps differing but isn't reconciled

Deliberate: for a task group with a scaling policy, nomad-botherer treats
`Count`/`Scaling` as owned by the autoscaler. Those changes neither trigger nor
block an update, and a diff that is *only* autoscaler churn shows
`apply_action: no_actionable_change`. It will not fight the autoscaler.

## The live job's meta disagrees with the HCL — which wins?

Git, always. When a job has an HCL file in the repo, that file alone decides
whether the job is managed and under which policy; a stale key on the live job is
ignored (and surfaced as drift that converges). The live job's meta only drives
behaviour for jobs Git knows nothing about (a running job with no HCL). See
[Git is the source of truth](job-selection.md#git-is-the-source-of-truth-for-the-meta-key).

## Workload identity: every diff/plan fails with `500 … UUID must be 36 characters`

The task's raw workload-identity **JWT** cannot be used directly as a Nomad
token — Nomad accepts it for read RPCs but **rejects it on `Job.Plan`**, which
nomad-botherer runs on every drift check. The fix is to **exchange** the JWT for
a real ACL token via `POST /v1/acl/login`: set `--nomad-login-auth-method` (with
a JWT auth method, a named identity whose `aud` matches it, and a binding rule).
nomad-botherer does the exchange and refreshes the token before it expires. Full
setup: [Nomad access → Workload identity](setup/nomad-access.md#workload-identity-recommended-under-nomad).
(Also don't use `identity { env = true }`: an env token is captured once at task
start and never refreshed.)

## An apply is stuck as `blocked_known_failed`

The flap-loop guard is holding it: this exact spec matches a recent deployment
that failed, so re-applying it would re-enter the failure. The guard releases the
moment Git moves to a spec that has not failed — push a fix (or a revert) commit.
See [Rollback](rollback.md).

## Does it lose anything on restart?

No. nomad-botherer holds no persistent state of its own — the update queue and
drift results are in memory and are rebuilt from Git and Nomad on the next diff
cycle, which together hold all durable truth. A restart costs at most one diff
cycle. It needs no volume and can be scheduled on any node.

## Can I run more than one replica?

No — run a single instance. All git state is in memory and nothing is written to
disk, so a second replica just produces duplicate drift reports and duplicate
apply attempts (the second is harmless thanks to CAS, but pointless).

## Does it ever write to my git repo?

Never. nomad-botherer reads Git and reads/writes Nomad — nothing else. No
commits, no pushes, no state branch; it holds no Git write credentials. Repo
changes always arrive by PR from humans or other automation. See
[Design philosophy](philosophy.md).

## Why are values `[REDACTED]` in `/diffs`?

`--redact-secrets` is on by default: env vars, template bodies, and fields with
secret-like names are redacted before the diff is stored, so they never reach
`/diffs` or the API. The diff structure and field names are kept. Set
`--redact-secrets=false` to show real values. See [Monitoring](monitoring.md).

## Can one instance watch multiple namespaces?

An instance watches one `--nomad-namespace`. Run one per namespace (and, with
workload identity, grant the ACL policy on each namespace you target).
