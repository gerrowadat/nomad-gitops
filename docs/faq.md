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
nomad-gitops detects and reports drift but never writes. Applying is opt-in —
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

A diff whose *only* change is to nomad-gitops's own meta keys (e.g. adding
`gitops_managed`, switching `gitops_update_policy`) is **managed-meta-only**: by
default it is neither counted as drift nor applied on its own. Re-registering a
running job just to stamp those keys on it would be disruptive for no functional
gain — the HCL is already authoritative, and the keys ride along the next real
update. Surfaced instead by `nomad_gitops_meta_only_diffs_total` and the
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

Deliberate: for a task group with a scaling policy, nomad-gitops treats
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
nomad-gitops runs on every drift check. The fix is to **exchange** the JWT for
a real ACL token via `POST /v1/acl/login`: set `--nomad-login-auth-method` (with
a JWT auth method, a named identity whose `aud` matches it, and a binding rule).
nomad-gitops does the exchange and refreshes the token before it expires. Full
setup: [Nomad access → Workload identity](setup/nomad-access.md#workload-identity-recommended-under-nomad).
(Also don't use `identity { env = true }`: an env token is captured once at task
start and never refreshed.)

## Workload identity worked, then broke after ~30 minutes (`ACL token expired`)

Your `identity` block is missing a `ttl`. Without one, Nomad issues a
**non-expiring** JWT and **never rewrites the file**, so once the first exchanged
ACL token expires (after the auth method's `max_token_ttl`), nomad-gitops
re-logins with the stale JWT and `/v1/acl/login` rejects it — every check then
fails with `ACL token expired`. Add `ttl` (and `change_mode = "noop"`) to the
identity block; Nomad then renews the file before it expires. nomad-gitops logs
a WARN at startup when the JWT has no expiry, so you don't have to wait for the
delayed failure. See
[Nomad access → Workload identity](setup/nomad-access.md#workload-identity-recommended-under-nomad)
(issue #76).

## An apply is stuck as `blocked_known_failed`

The flap-loop guard is holding it: this exact spec matches a recent deployment
that failed, so re-applying it would re-enter the failure. The guard releases the
moment Git moves to a spec that has not failed — push a fix (or a revert) commit.
See [Rollback](rollback.md).

## I froze a job and it still shows as drifting, and still alerts

That is deliberate. A [freeze](freezing.md) withholds *writes*; it does not hide
drift. You froze the job knowing it was drifting and not wanting that
reconciled yet, so the drift stays on `/diffs` (as `blocked_frozen`) and
`NomadJobDrift` keeps firing. A mechanism that silences alerts as a side effect
of an incident action is how drift gets forgotten.

If you want quiet during a known incident, take an Alertmanager silence on the
`nomad_job` label — explicit, time-boxed, and audited by Alertmanager rather
than by us.

## Nomad reverted my frozen job anyway

A freeze stops *nomad-gitops*, not Nomad. If the job's `update` stanza sets
`auto_revert = true`, Nomad watches the rollout itself and reverts a failed
deployment regardless of any freeze — that machinery is inside the cluster and
outside nomad-gitops's control. A freeze does hold nomad-gitops's own
[active rollback](rollback.md) (`--allow-rollback`).

## Every apply is being held and nothing is frozen

Check `/api/v1/freezes` for `"store_healthy": false`, and the console for a
"Freeze store:" line. A freeze store that **worked and then stopped** holds
every apply once the cached view passes `--freeze-max-staleness`, rather than
assuming nothing is frozen — a freeze ignored because of an ACL error is a
broken safety control. Fix the policy (see
[Nomad access](setup/nomad-access.md)), or set `--freeze-fail-open` to take the
other trade.

A store that has **never** been readable is a different case and holds nothing:
see the next entry. Drift detection is never affected either way.

## `freeze store has never been readable` in the logs

Freeze support is on by default and needs a Nomad Variables ACL policy that an
older deployment will not have. Rather than stopping your applies for a feature
you are not using, nomad-gitops honours no freezes at all in that state and
says so loudly — an ERROR at startup, `"store_healthy": false`, a console line,
and `NomadGitopsFreezeStoreErrors`.

Two ways to clear it: grant `variables { path "<prefix>/*" }` with `read` and
`list` (see [Nomad access](setup/nomad-access.md)) if you want freezes, or set
`--freeze-variable-prefix=""` to turn the mechanism off entirely. Do not leave
it in between: a freeze you set in this state will silently not be honoured.

## A freeze expired but the record is still there

Expired records are kept on purpose: deleting one would destroy the only record
of why the job was frozen, and a record left behind is a signal that somebody
meant to come back to it. The job itself resumed reconciling the moment the
freeze expired. Purge it with `nomad var purge <path>`, or set
`--freeze-expired-retention` to have nomad-gitops tidy them up.

## Should I freeze a job or change its policy in Git?

A freeze is for a hold measured in hours, during an incident, when a PR
round-trip is too slow. Anything longer-lived belongs in Git —
`gitops_update_policy = "none"` to keep detecting but never apply, or removing
`gitops_managed` to stop managing the job. The escalating alerts on long-lived
and indefinite freezes exist to push you back to Git. See
[Freezing a job](freezing.md#when-to-use-git-instead).

## Does it lose anything on restart?

No. nomad-gitops holds no persistent state of its own — the update queue and
drift results are in memory and are rebuilt from Git and Nomad on the next diff
cycle, which together hold all durable truth. A restart costs at most one diff
cycle. It needs no volume and can be scheduled on any node.

[Freezes](freezing.md) survive a restart for the same reason: the record lives
in Nomad Variables, not in the process, and is re-read every cycle.

## Can I run more than one replica?

No — run a single instance. All git state is in memory and nothing is written to
disk, so a second replica just produces duplicate drift reports and duplicate
apply attempts (the second is harmless thanks to CAS, but pointless).

## Does it ever write to my git repo?

Never. nomad-gitops reads Git and reads/writes Nomad — nothing else. No
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

## A diff is truncated with "exceeds maximum nesting depth"

nomad-gitops caps how deep it will walk into a Nomad plan diff's nested
objects (classification, redaction, and `/diffs` rendering all share the same
cap). No real job spec nests anywhere near this deep — it exists to stop a
pathologically nested job spec (however it got there) from recursing without
bound. A diff hitting the cap is conservatively classified as a non-image,
non-meta change, so it still requires the `full` update policy to apply
rather than being waved through as trivial.
