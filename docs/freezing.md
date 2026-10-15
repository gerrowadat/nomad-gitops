# Freezing a job: pausing applies without editing Git

Sometimes you need nomad-gitops to stop writing to one job *right now*, and
editing Git is the wrong instrument: you are debugging the job by hand, a
dependency is down and re-registering would turn degraded into stopped, or a
rename is half-finished. A commit, a review and a webhook round-trip is too slow
for that, and it leaves `main` claiming the job should not be managed when it
should.

A **freeze** is a temporary, per-job hold on nomad-gitops's authority to write
to that job. It holds registering, deregistering and active rollback, takes
effect within one diff cycle, and is set and cleared without touching the repo.

```console
# Freeze
$ nomad var put nomad/jobs/gitops/freezes/api-server \
    reason='INC-4412: fixing the migration by hand' \
    expires_at='2026-10-15T18:00:00Z'

# Thaw
$ nomad var purge nomad/jobs/gitops/freezes/api-server
```

## What a freeze does and does not do

| Held while frozen | Not affected |
|---|---|
| `REGISTER` — applying drift by re-registering from HCL | Drift detection: the job is still diffed every cycle |
| `DEREGISTER` — removing a job deleted from the repo | `/diffs`, `/healthz`, the JSON API, the drift metrics |
| `REVERT` — nomad-gitops's own [active rollback](rollback.md) | The drift alerts, which keep firing (see below) |
| | Nomad's own `auto_revert`, which is outside our control |

Two consequences worth internalising:

**A freeze does not hide drift, and does not silence drift alerts.** You froze
the job *because* it is drifting and you do not want that reconciled yet, so the
drift stays visible on `/diffs` and `NomadJobDrift` keeps firing. This is
deliberate: a mechanism that silences alerts as a side effect of an incident
action is how drift gets forgotten. If you want quiet during a known incident,
take an Alertmanager silence on the `nomad_job` label — explicit, time-boxed,
and audited by Alertmanager rather than by us.

**A freeze cannot hold Nomad's `auto_revert`.** If the job's `update` stanza
sets `auto_revert = true`, Nomad will still revert a failed deployment while the
job is frozen. A freeze stops *nomad-gitops*, not Nomad.

A frozen job's diff reports `apply_action: blocked_frozen`, with the reason and
expiry in `apply_detail`:

```
+/- Job: "api-server"  (jobs/api-server.hcl)
  ~ Meta[version]: "1.4.2" => "1.5.0"
  → not applied: frozen until 2026-10-15T18:00:00Z (7h21m0s from now): INC-4412: fixing the migration by hand
```

## The freeze record

One Nomad Variable per frozen job, under `--freeze-variable-prefix` (default
`nomad/jobs/gitops/freezes`), in the same namespace as the job. Items are flat
strings, so `nomad var put` needs no JSON quoting:

| Item | Required | Meaning |
|---|---|---|
| `reason` | **yes** | Why the job is frozen. A record with no reason is treated as malformed (and still held — see below). An unexplained freeze is the thing this design most tries to avoid. |
| `expires_at` | no | RFC3339. After this instant the freeze stops holding anything. **Absent means indefinite**, which is allowed but alerted on. |
| `frozen_by` | no | Whoever set it. Self-reported: Nomad does not record who wrote a Variable. |
| `job` | only sometimes | The exact job ID, needed when the path segment is not the job ID — see [job IDs that are not path-safe](#job-ids-that-are-not-path-safe). |

There is no `created_at` item: the Variable's own `CreateTime` already records
when the freeze was set, and nomad-gitops reads it from there.

### Record states

| State | Meaning | Holds writes? |
|---|---|---|
| `active` | Expiry is in the future. | Yes |
| `indefinite` | No `expires_at`. Never lifts on its own. | Yes |
| `expired` | Past its `expires_at`. | **No** — the job reconciles normally again |
| `malformed` | No reason, an unparseable `expires_at`, a `job` item that disagrees with its path, or a record that could not be read. | **Yes** — read conservatively as indefinite |

A malformed record holds writes on purpose. A safety control that cannot be
parsed must not quietly fail open, so an unreadable or nonsensical record is
read as "this job is frozen" and alerted (`NomadGitopsFreezeMalformed`).

### Expiry, and why expired records are left behind

Expiry is optional and indefinite freezes are allowed. A mandatory TTL only
buys a dishonest expiry or a cron job that re-freezes; "this is on hold and
nobody has decided for how long" is more useful information than a fabricated
deadline — so it is permitted, and reported loudly until someone deals with it.

When a freeze expires the job resumes reconciling on the next cycle with no
action from anyone. **The record itself is not deleted**, for two reasons:
deleting it would destroy the only record of why the job was frozen, and a
record left sitting there is a signal — somebody meant to come back to this. So
an expired record suppresses nothing but still shows up in the console and fires
`NomadGitopsFreezeExpired` until it is purged.

Set `--freeze-expired-retention` (e.g. `24h`) if you would rather nomad-gitops
tidied them up. Off by default: a tool that deletes the audit trail of its own
withheld writes should be opted into.

## When the freeze store cannot be read

nomad-gitops re-reads the freeze store once per diff cycle, and again before
draining the apply queue. There are two failure cases, and they are treated
differently on purpose.

**A store that worked and then stopped** (an ACL change, a Raft hiccup) keeps
the last successfully-read view in force — stale but safe beats fresh and
empty. Once that view is older than `--freeze-max-staleness` (default `5m`),
**every apply is held**, with the reason named in `apply_detail`, and
`NomadGitopsFreezeViewStale` fires as critical. This is fail-closed, and
deliberate: the alternative is that a transient problem quietly re-enables
writes to the one job you froze precisely because writing to it would break
something. If you would rather take the other trade, set `--freeze-fail-open`.

**A store that has never once been readable** holds nothing. Freeze support is
on by default, so a deployment whose ACL policy predates it — no
`variables:read` on the freeze prefix — must not have its GitOps stopped by a
feature it is not using, and nobody can be relying on a freeze nomad-gitops has
never been able to see. The condition is loud rather than silent: an ERROR at
startup, a rising `nomad_gitops_freeze_store_errors_total`,
`"store_healthy": false` on `/api/v1/freezes`, a console line saying freezes are
not being honoured, and `NomadGitopsFreezeStoreErrors`.

In both cases **drift detection is never degraded** — diffs, `/healthz` and the
drift metrics carry on exactly as before.

So: if you intend to use freezes, grant the ACL policy below and confirm
`store_healthy` is `true` *before* you need it. If you do not intend to use
them, set `--freeze-variable-prefix=""` and the whole mechanism, including its
alerts, goes away.

## The HTTP API

Reading freezes is always available (with `--api-key`):

```console
$ curl -H "Authorization: Bearer $API_KEY" http://localhost:8080/api/v1/freezes
```

Writing them needs `--enable-freeze-api` as well, which also requires an API
key and a non-empty freeze prefix — the same opt-in-twice-over shape as
`--enable-job-creation` and `--enable-deregister`. These are the only endpoints
that change cluster-visible state, so they are off by default; the endpoints
return **501** naming the flag when they are.

```console
# Freeze for four hours
$ curl -XPOST -H "Authorization: Bearer $API_KEY" \
    -d '{"reason":"INC-4412: by hand","frozen_by":"doc","expires_in":"4h"}' \
    http://localhost:8080/api/v1/freezes/api-server

# Thaw
$ curl -XDELETE -H "Authorization: Bearer $API_KEY" \
    http://localhost:8080/api/v1/freezes/api-server
```

`reason` is required (400 without one). Give `expires_in` (a Go duration) or
`expires_at` (RFC3339), not both; neither means `--freeze-default-ttl`, which is
indefinite unless you set it. A write that loses a check-and-set race against
another replica or a concurrent `nomad var put` is retried once and then
returns **409**.

The write path is CAS-protected on the record's last-read `ModifyIndex`, so two
nomad-gitops replicas, or a replica racing a human, cannot silently clobber each
other.

## Job IDs that are not path-safe

Nomad restricts a Variable path to `^[a-zA-Z0-9-_~/]{1,128}$`, and job IDs are
more permissive — a job ID may contain `.`, a path may not. The path segment is
therefore the job ID with any other character replaced by `-`, and a record
whose segment differs from the exact job ID **must** carry a `job` item:

```console
$ nomad var put nomad/jobs/gitops/freezes/api-server-v2 \
    job='api-server.v2' \
    reason='INC-4412'
```

For the overwhelmingly common case — job IDs that are already path-safe — the
path is the job ID verbatim and none of this is visible. Two job IDs can slug to
the same segment (`a.b` and `a-b`); the `job` item resolves which one a record
is for, and a record whose `job` item does not match its own path is malformed.

## ACL policy

Freeze support needs an explicit Variables policy. A workload identity's
*implicit* access covers only `nomad/jobs/<job>/<group>/<task>`, which the
freeze prefix does not match, so this is not free the way a task's access to its
own variables is:

```hcl
namespace "default" {
  variables {
    path "nomad/jobs/gitops/freezes/*" {
      capabilities = ["read", "list"]
    }
  }
}
```

Add `write` and `destroy` if you enable `--enable-freeze-api`. See
[Nomad access](setup/nomad-access.md). nomad-gitops logs which mode it is in at
startup, so a missing policy shows up as an error log and a rising
`nomad_gitops_freeze_store_errors_total` rather than as freezes that silently do
nothing.

## Watching for forgotten freezes

A freeze is meant to be temporary, so everything about it is built to be noisy
when it stops being. The console shows frozen jobs and, separately, expired
records left behind:

```
Frozen: 2 job(s) — writes held

  api-server       31m ago, expires in 8h 29m
                   INC-4412: fixing the migration by hand
  batch-reindex    42 days ago, no expiry — indefinite

Expired freeze records: 1

  old-worker       expired 6 days ago
                   purge with nomad var purge nomad/jobs/gitops/freezes/old-worker
```

The shipped [alert rules](../monitoring/alerts.yml) cover long-lived freezes,
indefinite freezes, expired-but-uncleared records, malformed records, and an
unreadable or stale freeze store. See [Monitoring](monitoring.md#freezes).

## When to use Git instead

A freeze is for a hold measured in hours. If the answer to "how long?" is
"indefinitely" or "until we decide something", that belongs in Git, where it is
reviewed and visible in the repo's history:

- `gitops_update_policy = "none"` — keep detecting drift on this job, never
  apply it.
- Removing `gitops_managed = "true"` — stop managing the job entirely.

Both are in [Meta-key reference](meta-keys.md). The escalating alerts on
long-lived and indefinite freezes exist specifically to push you back here.

The design reasoning, including why a non-Git input is allowed to withhold write
authority at all, is in
[`design/temporary-freeze.md`](design/temporary-freeze.md).
