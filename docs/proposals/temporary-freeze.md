# Proposal: temporarily freezing a job without editing Git

**Status**: draft
**Date**: 2026-10-14

## Background

nomad-gitops has two ways to stop it writing to a job: remove
`gitops_managed = "true"` from the job's HCL, or set
`gitops_update_policy = "none"`. Both live in Git, both are reviewable, both
are permanent until someone changes them back — and both require a commit, a
PR, a review, and a poll or webhook round-trip before they take effect.

That is the right mechanism for a decision, and the wrong mechanism for an
incident. The cases that keep coming up are all short-lived and all urgent:

- An operator is debugging a job by hand (`nomad job run` from a local file,
  `nomad alloc exec`, scaling a group manually) and nomad-gitops keeps
  reconciling the job back to the Git spec underneath them.
- A deploy is half-finished and visibly unhealthy, and the operator wants
  nomad-gitops to stop touching the job while they decide whether to revert
  in Git or forward-fix.
- A dependency is down (a registry, a Vault mount, a database) and
  re-registering the job would turn a degraded service into a stopped one.
  The Git spec is fine; the cluster is not ready for it.
- A `missing_from_hcl` job is mid-rename and `--enable-deregister` is on.
  The operator wants the deregister grace period held, not spent.

In each case the operator wants to withhold nomad-gitops's write authority
for the next few hours, on one named job, immediately, and then have normal
behaviour resume without anyone having to remember to undo it. Today the only
options are to edit Git (slow, and leaves a commit that misrepresents intent —
the job *should* be managed, just not right now) or to stop nomad-gitops
entirely (cluster-wide, and loses drift detection for everything else).

This proposal adds a **freeze**: a short-lived, per-job hold on nomad-gitops's
write authority, stored in Nomad Variables, readable and writable without
touching the repo, and reported loudly enough that a forgotten freeze is an
alert rather than a mystery.

---

## This does not weaken "Git is the source of truth"

The hard invariant is that Git alone decides whether a job is managed and under
which policy, with no flag override
([`philosophy.md`](../philosophy.md#git-is-the-source-of-truth--including-for-the-tools-own-behaviour),
[`job-selection.md`](../job-selection.md#git-is-the-source-of-truth-for-the-meta-key)).
`--managed-meta-hcl-canonical` was removed because it inverted which source
*decides*. A freeze has to be held to that standard, and it is, for three
reasons.

**A freeze is deny-only.** It can only ever make nomad-gitops do *less* than
Git asks. It cannot select a job that Git excluded, widen a policy, grant
first-time creation, authorise a deregister, or cause any write that the HCL
does not already call for. Git remains the only source that can *grant*
authority; a freeze can only withhold it. Stated as an amendment to the
invariant:

> Git is the only input that can grant nomad-gitops authority to write to a
> job. Other inputs may withhold that authority, never extend it.

Every existing gate already obeys this reading — `--default-update-policy`,
`--enable-job-creation`, `--enable-deregister`, `--apply-existing-drift` and
the flap-loop guard all narrow what Git asks for, and none of them widens it.

**There is already a shipped precedent.** The flap-loop guard in `tag` mode
writes a durable marker into Nomad (a job version tag) and then refuses to
apply a spec that Git is actively asking for, until a human changes Git. That
is a non-Git input withholding an apply, with its state stored in Nomad, and
[`philosophy.md`](../philosophy.md#no-persistent-state-we-are-responsible-for)
names it explicitly as the acceptable shape for durable per-job state. A freeze
is the same mechanism with the decision moved from the tool to the operator:
the flap guard is a freeze nomad-gitops sets for itself.

**Nothing durable moves inside nomad-gitops.** The freeze record lives in
Nomad's Raft log, not in the process. The tool re-reads it every diff cycle and
holds no authority to invent one. A restart costs one read; two replicas see
the same record. This is the same split as everything else: Git holds desired
state, Nomad holds actual state and now also the operator's hold, and
nomad-gitops holds nothing it cannot re-derive.

What a freeze is *not*: it is not a way to express "this job should not be
managed". That is still a Git decision, and the loud, escalating alerting on
long-lived and indefinite freezes exists specifically to push operators back to
Git when a hold stops being temporary.

---

## Requirements

- Freeze and unfreeze one named job without a commit, and have it take effect
  by the next diff cycle at the latest.
- Source of truth outside the nomad-gitops process, so a restart, a
  rescheduling, or a second replica cannot lose or disagree about a freeze.
- No new infrastructure: Nomad and Git are the only dependencies, and
  [never write to Git](../philosophy.md) stands.
- Deny-only, per the section above.
- Drift detection, reporting and metrics continue unchanged for a frozen job.
  A freeze suppresses *writes*, never *visibility*.
- A freeze that is forgotten, expired-but-uncleared, or indefinite is visible
  in the server UI and fires an alert. Nothing is quietly paused forever.
- Fail safe: if nomad-gitops cannot read the freeze store, it must not assume
  nothing is frozen.
- Hand-operable: `nomad var put` and the Nomad UI are sufficient. The HTTP API
  is a convenience, not the only way in.

---

## Verified against upstream docs

Four load-bearing assumptions, checked against Nomad's own documentation and
against the `hashicorp/nomad/api` version this repo pins
(`v0.0.0-20260306161221-000e1028d589`):

- **Variable items are a flat string→string map.** `type VariableItems
  map[string]string` in `variables.go`. A freeze record can therefore be a
  handful of flat string items — no JSON blob, nothing to quote-escape on a
  `nomad var put` command line. The CLI takes "a series of key-value pairs"
  directly
  ([`nomad var put`](https://github.com/hashicorp/web-unified-docs/blob/main/content/nomad/v2.0.x/content/commands/var/put.mdx)).
- **Variable paths are restricted to `^[a-zA-Z0-9-_~/]{1,128}$`.** Quoting the
  same page: "Variable paths are restricted to RFC3986 URL-safe characters that
  don't conflict with the use of the characters `@` and `.` in template blocks.
  This includes alphanumeric characters and the special characters `-`, `_`,
  `~`, and `/`… Paths may be up to 128 bytes long." Nomad **job IDs are more
  permissive than this** — a job ID containing `.` is legal in Nomad and
  illegal in a Variable path — so the path cannot simply be the job ID. See
  [Addressing a freeze](#addressing-a-freeze) for the encoding this forces.
- **CAS is available on Variable writes.** `CheckedCreate` issues `?cas=0` and
  `CheckedUpdate` issues `?cas=<ModifyIndex>`, returning an unwrappable
  `ErrCASConflict`. Two nomad-gitops replicas, or a replica racing a human
  `nomad var put`, cannot silently clobber each other.
- **A workload identity's *implicit* Variable access does not reach the freeze
  path.** The implicit policy grants read and list on exactly four prefixes:
  `nomad/jobs`, `nomad/jobs/$job_id`, `nomad/jobs/$job_id/$task_group`, and
  `nomad/jobs/$job_id/$task_group/$task_name`
  ([Task Access to Variables](https://github.com/hashicorp/web-unified-docs/blob/main/content/nomad/v2.0.x/content/docs/concepts/variables.mdx)).
  A four-segment path like `nomad/jobs/gitops/freezes/api-server` only matches
  that shape if nomad-gitops happens to run as job `gitops`, group `freezes`,
  task `api-server`. So an **explicit ACL policy is required** — this is not
  free the way task self-access to its own variables is. The proposal below
  states the policy rather than hand-waving it.

Variables also require **Nomad 1.4+**. nomad-gitops's verified compatibility
floor is well above that ([`nomad-versions.md`](../nomad-versions.md)), so this
is not a practical constraint, but it is the reason the feature degrades
gracefully rather than failing startup if the API is absent.

---

## Design

### The freeze record

One Variable per frozen job, under a configurable prefix, in the same Nomad
namespace as the job (`--nomad-namespace`), so namespace isolation comes for
free and no namespace appears in the path:

```
<freeze-variable-prefix>/<job-path-segment>

# default prefix: nomad/jobs/gitops/freezes
nomad/jobs/gitops/freezes/api-server
```

Items, all strings, all optional except `reason`:

| Item | Required | Meaning |
|---|---|---|
| `reason` | yes | Free text. Why this job is frozen. Shown in the UI, the API, `/diffs`, and the logs. Rejecting a record with no reason is deliberate: an unexplained freeze is the thing this design is most trying to avoid. |
| `expires_at` | no | RFC3339. After this instant the freeze no longer suppresses anything. Absent means **indefinite** — permitted, but alerted on permanently. |
| `frozen_by` | no | Free text; whoever or whatever set it. Nomad does not record the writer of a Variable, so this is self-reported and advisory. |
| `job` | only when the path segment is not the exact job ID | The exact job ID, for the encoding case below. |

The record's creation time is **not** an item: `Variable.CreateTime` already
carries it, Nomad maintains it, and a hand-written `created_at` would only ever
be a worse copy. `ModifyTime` likewise gives "when was this freeze last
touched" for free.

Freezing a job by hand is then one command:

```console
$ nomad var put nomad/jobs/gitops/freezes/api-server \
    reason='INC-4412: holding while we fix the migration by hand' \
    expires_at='2026-10-14T18:00:00Z'
```

and thawing it is `nomad var purge nomad/jobs/gitops/freezes/api-server`.

### Addressing a freeze

Nomad job IDs can contain characters that a Variable path cannot (`.` is the
common one). The path segment is therefore the job ID with every character
outside `[A-Za-z0-9_~-]` replaced by `-`, and a record whose segment differs
from the exact job ID **must** carry a `job` item:

```console
$ nomad var put nomad/jobs/gitops/freezes/api-server-v2 \
    job='api-server.v2' \
    reason='...'
```

Matching is by the `job` item when present, by the exact path segment
otherwise. Two consequences worth stating:

- The slug can collide (`a.b` and `a-b` both slug to `a-b`). The `job` item
  resolves it; two records claiming the same job ID, or a record whose `job`
  item does not match its own slug, is a **malformed record** (below).
- Prefix plus segment must fit in 128 bytes. nomad-gitops validates this when
  writing and reports an unaddressable job rather than silently failing — a job
  whose ID is too long to freeze is something the operator must know about
  before an incident, not during one.

For the overwhelmingly common case — job IDs that are already path-safe — the
path is the job ID verbatim and none of this is visible.

### What a freeze suppresses

A freeze withholds **all three write operations** for that job:

| Operation | Frozen behaviour |
|---|---|
| `REGISTER` | Not enqueued. Drift is still detected, still reported, still counted, still ages on the drift metrics. |
| `DEREGISTER` | Not enqueued, and the `--deregister-grace` clock is **held, not reset**: a job that was 4 minutes into a 5-minute grace when frozen is still 4 minutes in when thawed. Freezing buys time without spending it, which is exactly the mid-rename case. |
| `REVERT` | Active rollback (`--allow-rollback`) stands down. Nomad's own `auto_revert` is outside nomad-gitops's control and **will still fire** — this is the one thing a freeze cannot hold, and the UI and docs must say so plainly. |

Explicitly unaffected: diff detection, `/diffs`, `/healthz`, the drift metrics,
the Raft-index skip optimisation, git polling, and the flap-loop guard's own
bookkeeping. A freeze is a gate in front of the write, not a hole in the
observer.

### Where the gate sits

Two checks, for two different reasons.

**At enqueue, in `Check()`.** The primary gate. A frozen job produces no
`JobUpdate` at all; the `JobDiff` instead carries a new
`ApplyAction` so every surface that already explains *why* a diff was not
applied explains this one too:

```go
// ApplyActionFrozen means an operator-set freeze is withholding the write.
ApplyActionFrozen ApplyAction = "blocked_frozen"
```

with `Describe()` returning `"not applied: frozen until 2026-10-14T18:00:00Z
(INC-4412: holding while we fix the migration by hand)"`, or `"… frozen
indefinitely (reason)"` when there is no expiry. This slots in beside
`blocked_by_policy` and `blocked_known_failed` and needs no new rendering code
in `internal/server/render.go`. Because nothing is enqueued, a long freeze
costs no queue churn — the diff cycle simply keeps reporting a diff it will not
act on, which is what `--default-update-policy=none` already does.

**At drain, in `applyUpdate()`.** A freeze can land in the window between
enqueue and apply, and that window is exactly when an operator reaching for a
freeze most needs it to work. So the applier re-reads the job's freeze state
immediately before the write — the same "recheck live state immediately before
the call" discipline that
[`change-checkpointing.md`](change-checkpointing.md) requires for deregister
safety. A freeze found here needs a queue status, and none of the existing five
fits: it is not a failure, nothing superseded it, and leaving it `PENDING`
would spin `drainUpdates` forever on a `NextPending` that never clears. This
proposal adds one:

| `JobUpdateStatus` | Meaning |
|---|---|
| `HELD` | The update was not applied because a freeze was in force when it reached the front of the queue. Terminal in the queue; the intent is re-derived by the next diff cycle after the thaw. |

`HELD` is terminal for the same reason `FAILED` is: intent is cheap to
recompute and expensive to remember. It does not count as a failure for the
flap-loop guard, does not tag a version, and does not invalidate the skip
optimisation — a freeze is not drift, and the next ordinary cycle will re-derive
the update once the freeze lifts. It should appear in the vocabulary table in
`CLAUDE.md` and in
[`applying-changes.md`](../applying-changes.md) alongside the others.

### Reading the freeze set, and failing safe

The freeze set is read once per diff cycle with `Variables().PrefixList()` plus
a `Peek()` per record, and cached in the `Differ` with the time of the last
successful read. Costs are bounded by the number of *frozen* jobs, not the
number of managed jobs, and that number is normally zero — the list call
against an empty prefix is the steady-state cost.

The failure mode matters more than the happy path. A freeze that gets ignored
because of an ACL error is a broken safety control, so the read path fails
**closed, with a bounded staleness window**:

- The last successfully-read freeze set stays in force while reads are failing.
  Stale-but-safe beats fresh-and-empty.
- Once the cached set is older than `--freeze-max-staleness` (default `5m`),
  nomad-gitops **holds all applies** — every enqueue is `blocked_frozen` with a
  reason naming the stale store — and the condition is alerted as critical.
  Detection carries on reporting drift throughout.
- Before the first successful read after startup, applies are held. The first
  read is sequenced before the first drain, so in practice this is invisible;
  if the store is unreachable at startup it is immediately loud.
- A single record that is unreadable or malformed (no `reason`, unparseable
  `expires_at`, a `job` item that disagrees with its own slug) is treated as an
  **indefinite freeze on the job it appears to name**, and alerted. This is the
  same conservative reading the differ already applies to an unrecognised
  `gitops_update_policy` value, which falls back to `none`.

Holding every apply cluster-wide on a Variables outage is a deliberate choice
and the most arguable one in this proposal. The alternative — ignore freezes
when the store is unreachable — means an ACL change or a Raft hiccup can quietly
re-enable writes to the one job an operator froze precisely because writing to
it would break something. Detection is never degraded, the condition is
critical-alerted within minutes, and the escape hatch is a flag
(`--freeze-fail-open`, default off) for anyone who would rather take the other
trade.

### Expiry

Expiry is optional and **indefinite freezes are allowed**. The alternative —
mandatory TTLs — pushes operators into either a dishonest expiry or a cron job
that re-freezes, and the honest signal ("this is on hold and nobody has decided
how long for") is more useful than a fabricated deadline.

An expired record is treated as **absent**: the job resumes normal
reconciliation on the next cycle, with no action from anyone. The record itself
is **not deleted**, for two reasons: deleting it would destroy the only evidence
of why the job was frozen, and an expired-but-present record is an operator
signal worth surfacing — a job somebody meant to look at again. So:

- Expired record, still present → the job reconciles normally, and
  `NomadGitopsFreezeExpired` fires until someone purges it. A record left
  sitting there is exactly the "left paused" case the UI and alerts must call
  out, even though it is no longer suppressing anything.
- `--freeze-expired-retention` (default `0`, disabled) optionally lets
  nomad-gitops purge records that have been expired for longer than the
  retention period, for sites that would rather not hand-tidy. Off by default:
  a tool that deletes the audit trail of its own held writes should be opted
  into, not inherited.

### Concurrency

Writes through the API use `CheckedCreate` / `CheckedUpdate` so a replica
racing another replica or a human `nomad var put` loses cleanly and retries
after re-reading. Reads need no coordination: the record is advisory in one
direction only, and two replicas that both see a freeze both decline to write,
which is the safe disagreement.

### ACL policy

An explicit policy is required (see
[Verified against upstream docs](#verified-against-upstream-docs)). Read-only
mode, which is the default:

```hcl
namespace "default" {
  variables {
    path "nomad/jobs/gitops/freezes/*" {
      capabilities = ["read", "list"]
    }
  }
}
```

With `--enable-freeze-api`, add `write` and `destroy`. This belongs in
[`setup/nomad-access.md`](../setup/nomad-access.md) next to the existing job
capabilities, and the startup log should state which mode is active so a
misconfigured policy shows up as a log line and a metric rather than as freezes
that silently do nothing.

A note for the docs: putting the freeze prefix under `nomad/jobs/` keeps it in
the Nomad-owned namespace the UI and CLI expect, but it is *not* where the
implicit workload-identity grant reaches, and the prefix is configurable for
sites whose ACL policy restricts writes under `nomad/jobs/`.

---

## Configuration

| Flag | Env var | Default | Purpose |
|---|---|---|---|
| `--freeze-variable-prefix` | `FREEZE_VARIABLE_PREFIX` | `nomad/jobs/gitops/freezes` | Nomad Variables path prefix holding freeze records. Empty disables freeze support entirely (no reads, no API, no metrics). |
| `--freeze-max-staleness` | `FREEZE_MAX_STALENESS` | `5m` | How stale the cached freeze view may be before all applies are held. `0` disables the hold (reads still retried; prefer `--freeze-fail-open` if that is the intent). |
| `--freeze-fail-open` | `FREEZE_FAIL_OPEN` | `false` | On an unreadable freeze store, proceed as if nothing is frozen instead of holding applies. Off by default. |
| `--enable-freeze-api` | `ENABLE_FREEZE_API` | `false` | Enable the mutating freeze endpoints (`POST`/`DELETE /api/v1/freezes/{job}`). Off by default; reading is always available. Requires `--api-key`. |
| `--freeze-default-ttl` | `FREEZE_DEFAULT_TTL` | `0` | Expiry applied to a freeze created through the API with no explicit expiry. `0` means indefinite. Does not affect records written with `nomad var put`. |
| `--freeze-expired-retention` | `FREEZE_EXPIRED_RETENTION` | `0` | Purge freeze records that have been expired at least this long. `0` disables; expired records are kept and alerted on. |

All six go in the table in [`configuration.md`](../configuration.md) with their
env vars when this lands. No existing flag or default changes, and with
`--enable-freeze-api` off and no records present, behaviour is byte-identical
to today.

---

## HTTP surface

### Reading (always available with `--api-key`)

`GET /api/v1/freezes`:

```json
{
  "freezes": [
    {
      "job_id": "api-server",
      "variable_path": "nomad/jobs/gitops/freezes/api-server",
      "reason": "INC-4412: holding while we fix the migration by hand",
      "frozen_by": "doc@gerrup.eu",
      "frozen_at": "2026-10-14T09:12:04Z",
      "expires_at": "2026-10-14T18:00:00Z",
      "state": "active"
    },
    {
      "job_id": "batch-reindex",
      "variable_path": "nomad/jobs/gitops/freezes/batch-reindex",
      "reason": "waiting on the new index schema",
      "frozen_at": "2026-09-02T11:40:00Z",
      "state": "indefinite"
    }
  ],
  "store_last_read": "2026-10-14T09:40:02Z",
  "store_healthy": true
}
```

`state` is one of `active`, `indefinite`, `expired`, `malformed`. The last two
are the ones the UI and the alerts shout about.

`/healthz` gains `frozen_count` and a `frozen` array of
`{job_id, reason, expires_at, state}`, so the existing health check carries the
"what is paused" answer without a second request. Note `/healthz` status stays
`diffs_detected` for a frozen drifting job — a freeze does not make drift
disappear.

### Writing (gated by `--enable-freeze-api`)

```
POST   /api/v1/freezes/{job}    {"reason": "...", "expires_in": "4h", "frozen_by": "..."}
DELETE /api/v1/freezes/{job}
```

`POST` writes the Variable (CAS-protected) and returns the stored record;
`expires_in` is a Go duration, or `expires_at` an RFC3339 instant, or neither
for `--freeze-default-ttl`. `DELETE` purges the record. `reason` is required on
`POST`; a 400 with no reason is the point. Both are behind the existing
`requireAPIKey` middleware on the `/api/v1/` mux, and both are no-ops returning
501 when `--enable-freeze-api` is off, with the response naming the flag.

These are the first mutating endpoints that change cluster-visible state
(`POST /api/v1/refresh` only pokes the diff loop), which is why they are
flag-gated on top of the API key rather than relying on it alone — the same
opt-in-twice-over shape as `--enable-job-creation` and `--enable-deregister`.

### Server UI

The index page gains a frozen section, rendered whenever the count is non-zero
or any record is expired or malformed — so a clean cluster looks exactly as it
does today, and anything hanging is impossible to miss:

```
Frozen: 2 job(s) — writes held

  api-server       frozen 31m ago, expires in 8h 29m
                   INC-4412: holding while we fix the migration by hand
  batch-reindex    frozen 42 days ago, no expiry — indefinite        [!]

Expired freeze records (not suppressing anything, left behind): 1

  old-worker       expired 6 days ago — purge with
                   nomad var purge nomad/jobs/gitops/freezes/old-worker
```

Indefinite, expired and malformed records render in the existing `.bad` style;
active time-boxed freezes in `.starting` amber. Two more UI additions covering
the "hanging" half of the ask:

- **Pending updates that are not draining.** The page already computes
  `PendingUpdates` and prints the count. It should also print the age of the
  oldest pending update, because a count of 3 is meaningless and "3 pending,
  oldest 22 minutes" is a diagnosis. The same number closes a real alerting gap
  that exists today, independent of freezes — see below.
- **A freeze-store health line** when the store is unreadable or the view is
  stale, stating plainly that applies are being held cluster-wide and why.

---

## Observability

### New metrics

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `nomad_gitops_job_frozen` | Gauge | `nomad_job` | `1` for each job with a freeze currently in force; absent otherwise. The join series for every alert and dashboard below. |
| `nomad_gitops_frozen_jobs` | Gauge | — | Total jobs currently frozen. The single "is anything paused?" number. |
| `nomad_gitops_job_freeze_created_timestamp_seconds` | Gauge | `nomad_job` | Variable `CreateTime`. `time() - metric` is how long the job has been frozen. |
| `nomad_gitops_job_freeze_expires_timestamp_seconds` | Gauge | `nomad_job` | Freeze expiry. **Absent for an indefinite freeze**, which is what makes indefinite freezes queryable with `unless on(nomad_job)`. |
| `nomad_gitops_freeze_records` | Gauge | `state` | Record count by `active` / `indefinite` / `expired` / `malformed`. |
| `nomad_gitops_freeze_store_last_read_timestamp_seconds` | Gauge | — | Last successful read of the freeze store. |
| `nomad_gitops_freeze_store_errors_total` | Counter | `op` | Freeze-store errors by `list` / `read` / `write` / `delete`. |
| `nomad_gitops_applies_held_by_freeze_total` | Counter | `operation` | Writes withheld by a freeze, by `REGISTER` / `DEREGISTER` / `REVERT`. The counter that shows a freeze is doing work. |
| `nomad_gitops_oldest_pending_update_age_seconds` | Gauge | — | Age of the oldest non-terminal update in the queue; `0` when the queue is empty. Not freeze-specific — it is the "is anything hanging?" signal. |

All registered via `promauto.With(reg)` in the `Differ` constructor, under the
existing `nomad_gitops_` prefix, and documented in
[`monitoring.md`](../monitoring.md).

### New recording rules

To be added to [`monitoring/recording_rules.yml`](../../monitoring/recording_rules.yml):

```yaml
      # How long each frozen job has been frozen (seconds).
      - record: nomad_gitops:job_freeze_age_seconds
        expr: time() - nomad_gitops_job_freeze_created_timestamp_seconds

      # Time left on each time-boxed freeze (seconds). Negative once expired.
      # Absent for indefinite freezes, which have no expiry series.
      - record: nomad_gitops:job_freeze_remaining_seconds
        expr: nomad_gitops_job_freeze_expires_timestamp_seconds - time()

      # Frozen jobs with no expiry set: frozen, but not in the expiry series.
      - record: nomad_gitops:job_frozen_indefinite
        expr: >
          nomad_gitops_job_frozen
          unless on(nomad_job) nomad_gitops_job_freeze_expires_timestamp_seconds

      # Seconds since the freeze store was last read successfully.
      - record: nomad_gitops:seconds_since_freeze_store_read
        expr: time() - nomad_gitops_freeze_store_last_read_timestamp_seconds

      # 5-minute rate of freeze-store errors, labelled by operation.
      - record: nomad_gitops:freeze_store_error_rate5m
        expr: rate(nomad_gitops_freeze_store_errors_total[5m])
```

### New alert rules

To be added to [`monitoring/alerts.yml`](../../monitoring/alerts.yml) as a
`nomad_gitops_freeze` group. The design decision recorded here: **the existing
drift alerts keep firing for a frozen job.** `NomadJobDrift` and
`NomadJobModifiedPersistent` are not suppressed or joined against
`nomad_gitops_job_frozen`. A freeze is a reason the drift is not being
reconciled, not a reason to stop reporting it, and a mechanism that silences
alerts as a side effect of an incident action is how drift gets forgotten. The
freeze alerts are **additive**; operators who want quiet during a known incident
should use an Alertmanager silence on the `nomad_job` label, which is explicit,
time-boxed and audited by Alertmanager itself. That trade — extra noise during
an incident, nothing ever hidden — should be stated in a comment in `alerts.yml`
so the next person does not "fix" it.

```yaml
  - name: nomad_gitops_freeze
    rules:

      # A job has been frozen for a long time. A freeze is meant to be
      # temporary: anything still held after a day is either forgotten or
      # should be a Git change (gitops_update_policy = "none") instead.
      # Note the ordinary drift alerts are NOT suppressed for frozen jobs —
      # see the comment above the drift group.
      - alert: NomadGitopsJobFrozen
        expr: nomad_gitops:job_freeze_age_seconds > 86400
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "Nomad job {{ $labels.nomad_job }} has been frozen for {{ $value | humanizeDuration }}"
          description: >
            nomad-gitops has been withholding writes to job
            {{ $labels.nomad_job }} for over a day because of a freeze record.
            Check /api/v1/freezes for the reason. If the hold is permanent,
            express it in Git with gitops_update_policy = "none" instead.

      # A freeze with no expiry. Permitted, but it will never lift on its
      # own, so it is reported from one hour in rather than one day.
      - alert: NomadGitopsJobFrozenIndefinite
        expr: nomad_gitops:job_frozen_indefinite > 0
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "Nomad job {{ $labels.nomad_job }} is frozen with no expiry"
          description: >
            The freeze on job {{ $labels.nomad_job }} has no expires_at, so
            nomad-gitops will withhold writes to it until someone purges the
            record. Set an expiry, purge it, or move the decision into Git.

      # An expired record left lying around. It is no longer suppressing
      # anything — the job reconciles normally — but it is misleading to
      # anyone reading the Variables, and somebody meant to come back to it.
      - alert: NomadGitopsFreezeExpired
        expr: nomad_gitops_freeze_records{state="expired"} > 0
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "{{ $value }} expired nomad-gitops freeze record(s) left in place"
          description: >
            One or more freeze records have passed their expires_at but are
            still present. The affected jobs are reconciling normally again.
            Purge the records (nomad var purge) or set
            --freeze-expired-retention to have them cleaned up.

      # A record nomad-gitops cannot make sense of. Treated as an indefinite
      # freeze on the job it appears to name, so this is blocking writes.
      - alert: NomadGitopsFreezeMalformed
        expr: nomad_gitops_freeze_records{state="malformed"} > 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "{{ $value }} malformed nomad-gitops freeze record(s)"
          description: >
            A freeze record is missing a reason, has an unparseable
            expires_at, or has a job item that disagrees with its path.
            nomad-gitops reads these conservatively as an indefinite freeze,
            so writes to the named job are being withheld. Fix or purge them.

      # The freeze store is unreachable. Unless --freeze-fail-open is set,
      # applies are being held cluster-wide once the cached view goes stale.
      - alert: NomadGitopsFreezeStoreErrors
        expr: nomad_gitops:freeze_store_error_rate5m > 0
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "nomad-gitops cannot reach the freeze store (op={{ $labels.op }})"
          description: >
            Nomad Variables {{ $labels.op }} operations against the freeze
            prefix have been failing for 10 minutes. Usually an ACL policy
            problem. Drift detection is unaffected.

      # The cached freeze view has gone stale, so every apply is held.
      # Threshold must exceed --freeze-max-staleness (default 5m).
      - alert: NomadGitopsFreezeViewStale
        expr: nomad_gitops:seconds_since_freeze_store_read > 300
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "nomad-gitops freeze view is {{ $value | humanizeDuration }} stale — all applies held"
          description: >
            nomad-gitops has not read the freeze store within
            --freeze-max-staleness and is therefore withholding every apply
            (fail-closed). Drift is still detected and reported. Fix the
            Variables ACL or set --freeze-fail-open to take the other trade.
```

### Closing an existing gap: updates that hang

`nomad_gitops_job_updates_pending` has existed and been documented since the
apply path shipped, and **nothing alerts on it**. A queue that stops draining —
a wedged applier, a Nomad API that accepts calls but never completes, a bug —
is invisible today unless someone loads the UI. That is the other half of
"report anything hanging", it is not freeze-specific, and it should not wait
for this proposal to be implemented:

```yaml
      # The update queue is not draining. Updates are applied within seconds
      # of being enqueued (--apply-interval, default 10s), so anything still
      # pending after 15 minutes means the applier is stuck, not busy.
      - alert: NomadGitopsUpdatesNotDraining
        expr: nomad_gitops_job_updates_pending > 0
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "nomad-gitops has {{ $value }} update(s) pending for over 15 minutes"
          description: >
            The update queue is not draining. Updates are normally applied
            within one --apply-interval of being enqueued. Check
            /api/v1/updates and the logs for a stuck or repeatedly failing
            apply.
```

Once `nomad_gitops_oldest_pending_update_age_seconds` exists this becomes the
sharper `nomad_gitops_oldest_pending_update_age_seconds > 900`, which does not
need the `for:` clause and does not reset when the queue churns. Both forms are
worth shipping in sequence: the gauge-based one when this proposal lands, the
`for:`-based one now.

---

## Rejected alternatives

**A `gitops_frozen` meta key in HCL.** Consistent with the existing keys and
needs no new storage — and completely misses the point. It requires a commit,
a review and a webhook round-trip during an incident, and it puts a lie in the
repo: the job *should* be managed, and `main` would say otherwise until someone
remembers to revert. `gitops_update_policy = "none"` already covers the Git
expression of "do not write to this job", and a freeze exists precisely for the
cases where Git is the wrong instrument.

**An in-process freeze, set via the API and held in memory.** Simplest possible
implementation, and it fails every requirement that matters: a restart or a
reschedule thaws the job silently at the worst possible moment, two replicas
disagree, and nothing outside the process can see or set the hold. It also puts
non-recomputable state inside nomad-gitops, which
[`philosophy.md`](../philosophy.md#no-persistent-state-we-are-responsible-for)
rules out.

**A file in the Git repo (`.gitops-freeze.yaml`).** Has the same PR round-trip
problem as the meta key, with the added disadvantage that the tool would need
to either write to Git (forbidden outright) or depend on humans to clean up a
file that is now part of the repo's reviewed state.

**Consul KV.** Works, is CAS-capable, and many Nomad clusters run Consul — but
it is a dependency nomad-gitops does not currently have, and Nomad Variables
give the same Raft durability and CAS with no new client, no new address flag
and no new failure mode. If a site has Consul and not Variables, the
`FreezeStore` boundary (below) is where that backend would go.

**`nomad job stop` / scaling to zero.** Sometimes the right operator action,
but it is a different thing: it stops the workload, and nomad-gitops would
then see `missing_from_nomad` and try to re-register it. A freeze stops
*nomad-gitops*, not the job.

**Suppressing drift alerts for frozen jobs.** Considered and rejected; the
reasoning is in [New alert rules](#new-alert-rules).

---

## Implementation shape

A small interface, for the same reason
[`change-checkpointing.md`](change-checkpointing.md) wants one — so the differ
does not know or care which backend is behind it, and tests do not need a live
Nomad:

```go
// FreezeStore holds operator-set holds on nomad-gitops's write authority.
// The store is the source of truth; nomad-gitops caches reads but never
// invents a record.
type FreezeStore interface {
    // List returns every freeze record, including expired and malformed ones
    // (the caller classifies). Errors must be distinguishable from an empty
    // store: an empty store means nothing is frozen, an error does not.
    List(ctx context.Context) ([]FreezeRecord, error)

    // Put writes a record, CAS-protected on modifyIndex (0 to create).
    Put(ctx context.Context, rec FreezeRecord, modifyIndex uint64) (FreezeRecord, error)

    // Delete purges a record.
    Delete(ctx context.Context, jobID string) error
}
```

`nomadVariableFreezeStore` is the only implementation; a `nil` store (empty
`--freeze-variable-prefix`) disables the feature with no branching in the
differ beyond one nil check. A fake store in tests makes the whole gate
testable without a cluster, matching the `NomadJobsClient` pattern.

Nothing here needs a change to `NomadJobsClient`: Variables are a separate
client namespace (`client.Variables()`), so the freeze store takes the
`*nomadapi.Client` directly and the jobs interface stays about jobs.

### When this is implemented

Per the repo's rules, the feature's own PR carries all of this:

- `git mv docs/proposals/temporary-freeze.md docs/design/temporary-freeze.md`,
  retitled `# Design: temporarily freezing a job`, with a status note saying
  what shipped, in which release, and where it diverged — leaving any deferred
  parts (see below) behind in `docs/proposals/`.
- The six flags and their env vars in [`configuration.md`](../configuration.md).
- The new `apply_action` reason in
  [`applying-changes.md`](../applying-changes.md), and `HELD` in the status
  list there and in the `CLAUDE.md` vocabulary table.
- The metrics table and a freeze section in [`monitoring.md`](../monitoring.md);
  the recording and alert rules into `monitoring/`.
- The endpoints in [`json-api.md`](../json-api.md) and the ACL policy in
  [`setup/nomad-access.md`](../setup/nomad-access.md).
- A [`faq.md`](../faq.md) entry ("I froze a job and it still shows as drifting
  / still alerted — why?", and "Nomad reverted my frozen job anyway" for
  `auto_revert`), and a [`use-cases.md`](../use-cases.md) recipe for the
  incident flow.
- Tests: the gate at both enqueue and drain, `HELD`, expiry boundaries,
  malformed records, the slug/`job`-item encoding and its collision case,
  fail-closed and `--freeze-fail-open`, the staleness hold, API auth and the
  `--enable-freeze-api` 501, and the UI rendering of each record state.
- `meta-keys.md` needs **no** change: this adds no meta key, deliberately.

No existing meta key, flag default, or diff behaviour changes, so there is
nothing here that alters behaviour for a deployment that does not use it.

---

## Open questions

- **Cluster-wide freeze.** A single record (`<prefix>/_all`, or a separate
  path) that holds every apply would be useful as an incident-wide "stop
  writing to anything" lever, and the fail-closed path already implements
  exactly that behaviour internally. Deferred because the per-job case is what
  was asked for and a global switch deserves its own thinking about who is
  allowed to set it. Note `_all` is path-legal and could collide with a job of
  that name, which the `job`-item rule resolves.
- **Scoped freezes.** Should a record be able to hold only some operations
  (e.g. `scope = "deregister"` to hold a rename grace period while still
  letting image updates through)? The record format has room for it
  (`scope = "register,deregister,revert"`), but every freeze suppressing
  everything is easier to reason about under pressure, which is when freezes
  get used. Proposed for v1: all three, no scope item.
- **Expiry notice.** An alert for freezes about to expire
  (`nomad_gitops:job_freeze_remaining_seconds < 1800`) would warn before writes
  resume. Left out of the proposed rules as probably-noisy for the short
  freezes that are the common case, but it is a one-rule addition if operators
  want it.
- **Freeze on detection.** Should nomad-gitops be able to freeze a job
  *itself* — e.g. after N consecutive failed applies — rather than only
  honouring operator freezes? The flap-loop guard already covers the specific
  case it was built for, and a tool that can freeze itself out of writing is a
  larger behavioural change than this proposal wants to make. Deliberately out
  of scope.
- **Shared prefix with checkpointing.** [`change-checkpointing.md`](change-checkpointing.md)
  proposes `nomad/jobs/gitops/checkpoints/` and asks whether the prefix should
  be configurable. If both land, a single `--nomad-variable-prefix` with
  `freezes/` and `checkpoints/` beneath it is probably better than two
  independent flags; whichever ships first should pick the shape.
