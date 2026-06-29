# Rollback: recovering from a bad change

When nomad-botherer applies drift it re-registers the job from HCL. If that
change is bad — a new image crash-loops, a config error fails health checks —
something has to stop the obvious failure mode: apply commit `C`, the deployment
fails, the job is reverted to the prior version, the next diff cycle sees Git
still wants `C`, re-applies it, it fails again, forever.

Two independent features address this. Both only apply to **deployment-producing
jobs** — service jobs with an `update` stanza and real health checks, which is
what makes Nomad create a *deployment* whose success or failure is observable.
Batch, system, and no-health-check jobs produce no deployment, so neither feature
engages for them.

## Best practice: let Nomad do the rollback

The supported, most reliable way to get automatic rollback is to set it in the
job's own HCL:

```hcl
job "api" {
  meta { gitops_managed = "true" }
  group "web" {
    update {
      auto_revert      = true
      health_check     = "checks"
      healthy_deadline = "5m"
    }
    # ... tasks with service checks ...
  }
}
```

With `auto_revert = true` and real health checks, **Nomad** watches the rollout,
fails the deployment if allocations do not become healthy, and reverts to the
last stable version — in-cluster, surviving any nomad-botherer restart, using the
same machinery `nomad job run` already relies on. nomad-botherer stays out of the
way. This is the gold standard; prefer it.

nomad-botherer's job in this case is only to *not fight the revert* — that is the
flap-loop guard, on by default.

## The flap-loop guard (`--flap-guard`, default `history`)

After a failed deployment of spec `S` is reverted (by Nomad's `auto_revert` or by
active rollback below), the live job is back at the prior version while Git still
wants `S`. The next cycle sees that as drift and would re-register `S`. The
flap-guard prevents it: before applying, it asks Nomad's own version history
*"has a recent deployment of this exact spec already failed?"* If yes, the apply
is withheld and surfaced as `blocked_known_failed`.

The guard keys on the **spec**, not the commit, so it releases automatically the
instant Git moves to a spec that has not failed — a fix commit, or a revert in
Git. It holds no state of its own: the failed attempt is recorded by Nomad as a
non-stable version.

| Mode | Behaviour | Trade-off |
|---|---|---|
| `history` (default) | Compares the HCL spec against Nomad's retained version history each cycle. | Read-only, no state written. Bounded by Nomad's version GC (`job_gc_threshold`): once the failed version is GC'd, the bad spec is retried **at most once more**, fails, and the guard re-engages. Degrades to "retry at most once per GC window", never a tight loop. |
| `tag` | As `history`, but also tags the failed version (`<prefix>-failed-<fingerprint>`) so the block survives GC. | Durable across any time horizon and restarts. The cost: nomad-botherer writes version tags into Nomad (a Nomad-native write, not a job-meta write, so no meta-drift) and becomes responsible for that state. A version carries at most one tag, so an already-tagged version is left alone. Requires a non-empty `--managed-meta-prefix` (tag names derive from it); rejected at config load otherwise. |
| `off` | The guard is disabled. | A known-bad spec can loop. Only sensible per-job, via the meta key, for a job where you accept the risk. |

Per job, override with the `<prefix>_flap_guard` meta key (`history`, `tag`, or
`off`) — see the [Meta-key reference](meta-keys.md).

Spec comparison is best-effort: server-side defaulting can make a genuinely
identical spec fingerprint differently, in which case the guard *misses* and the
bad spec is retried once more and caught again. That degradation is one-way and
safe — a *false block* of a good change would need a SHA-256 collision.

## Active rollback (`--allow-rollback`, default off)

For jobs that did **not** set `auto_revert` — and for operators who want
nomad-botherer to centralise the behaviour — active rollback makes nomad-botherer
do the revert itself. Each diff cycle it checks the latest deployment of each
rollback-enabled job; if it has **failed**, it reverts the job to its last stable
version with `Jobs.Revert`, CAS-guarded on the failed version so a concurrent
human change is never stomped.

This is off by default and is the heavier, riskier path: it duplicates machinery
Nomad already has for the `auto_revert` case. Prefer `auto_revert`.

- **`auto_revert` always wins.** If a rollback-enabled job's `update` stanza also
  sets `auto_revert`, nomad-botherer stands down and lets Nomad revert, logging
  the clash once. (Even if that check were bypassed, the CAS guard would reject
  the redundant revert.)
- Enable per job with `<prefix>_rollback = "true"` (or disable a job while the
  global flag is on with `"false"`).
- No deployment to revert to a stable version below the failed one → nothing is
  done; surfaced via the `rollbacks_total{result="no_stable_version"}` metric.

A revert is recorded in the update queue as a `REVERT` operation (visible on
`/api/v1/updates`), and the flap-guard then prevents re-applying the same bad
spec until Git is fixed.

The design background is in [`design/automatic-rollback.md`](design/automatic-rollback.md).
