# Design philosophy

Why nomad-botherer behaves the way it does. These principles explain most of the
non-obvious behaviour in the [FAQ](faq.md), and they are load-bearing — they are
the reasons the apply side is safe to turn on. The retrospective design records
for individual features live in [`design/`](design/); the survey of other tools
and the mistakes deliberately avoided is in [`prior-art.md`](prior-art.md).

## A drift detector first, a GitOps operator second

nomad-botherer always observes and reports the difference between Git and a live
Nomad cluster. Applying that difference — re-registering jobs from their HCL — is
a *second*, optional mode that is off by default. Detection is the product;
reconciliation is a feature you opt into. This ordering is why running the tool
is safe with zero configuration beyond pointing it at a repo and a cluster.

## Conservative by default — opt-in, twice over

Out of the box nomad-botherer never writes. Turning on reconciliation is
deliberately layered:

- `--default-update-policy` defaults to `none`, so a fresh deployment never
  writes;
- the per-job `gitops_update_policy` meta key overrides it in either direction;
  and
- first-time job creation needs `--enable-job-creation` *and* an effective policy
  of `full` on top.

Deletion is gated even harder (see *Conservative deletion* below). The point is
that every write is the result of an explicit decision, never a side effect of
running the tool, and that decision can be made per job.

## Git is the source of truth — including for the tool's own behaviour

When a job has an HCL file in the watched repo, that file alone decides whether
the job is managed and under which policy. A stale `gitops_*` key on the live job
never overrides Git; it is just drift that converges. There is deliberately no
flag to invert this. This makes the system predictable: to change what
nomad-botherer does to a job, you change the job's HCL and commit it —
reviewable, version-controlled, and the same whether the tool was running at the
time or not.

The corollary is **decisions are derived from git history, not remembered**.
"Has this drift been here since before the job was opted in?" and "did this spec
already fail?" are both answered by querying Git and Nomad, so they hold
identically across restarts.

## No persistent state we are responsible for

All durable truth lives in Git (desired state) and Nomad (actual state, version
history, deployment outcomes). nomad-botherer holds its working state — the diff
results and the update queue — in memory only, and rebuilds it from a single diff
cycle after a restart. Consequences:

- It needs no volume and can be scheduled on any node.
- There is no external database — no SQLite, Postgres, or Redis. Where durable
  per-job state is genuinely needed (e.g. the flap-guard's `tag` mode), it is
  stored in Nomad itself (a version tag), not in a store the tool owns.
- A restart costs at most one diff cycle; it can die at any instant and recompute.

## Plan before every write, CAS on every write

No job is ever registered without a fresh `Jobs.Plan()` confirming there is
something to apply, and every `Jobs.Register()` uses `EnforceIndex` with the
`JobModifyIndex` captured at detection time. If the cluster moved between
detection and apply, the write is rejected, the update is marked failed, and the
next cycle re-detects against current state. This is what makes a second replica
or a racing human change harmless rather than destructive. (Several other Nomad
GitOps tools skip the CAS check; [`prior-art.md`](prior-art.md) explains why that
is a correctness gap, not a performance trade-off.)

## Detection and application are decoupled

The diff loop and the apply loop are separate. Detected drift becomes an entry in
an in-memory queue that a separate apply loop drains, so a slow or failing apply
never delays the next check. When a newer commit arrives before an older update
applies, the older one is superseded — the most recent intended state wins.

## Never write to Git

nomad-botherer reads Git and reads/writes Nomad — nothing else. No commits, no
pushes, no GitHub API writes, no state branch; it holds no Git write credentials.
Repo changes (including image-tag bumps surfaced by external tooling) always
arrive by PR from humans or from automation that is *not* this tool. The most it
will ever offer is a read-only diff for someone else to turn into a PR.

## Conservative deletion

Removing drift is one thing; removing a *job* is another. A job that merely loses
its `gitops_managed` tag is left running untouched — one deleted line should never
destroy a job whose full spec is still in Git. Deregistration happens only when a
job is removed from the repo *entirely*, and even then only behind an explicit
flag, a confirming live tag, an effective `full` policy, a grace period, and an
immediate re-check before the call. Purge is never the default.

## Don't fight the platform

Where Nomad already does a job well, nomad-botherer stays out of the way rather
than reimplementing it:

- **The autoscaler owns `Count`.** For groups with a scaling policy, Count/Scaling
  changes are excluded from drift consideration, so the tool never overwrites
  autoscaler-managed counts every cycle.
- **Nomad's `auto_revert` owns rollback.** The recommended way to recover from a
  bad deploy is Nomad's native health-checked rollback; nomad-botherer's job is to
  *not fight the revert* (the flap-loop guard) rather than to duplicate it. Active
  rollback exists for jobs that opt out of `auto_revert`, but it always stands
  down when Nomad would act.

## Efficient by construction

Detection is cheap so it can run often: the repo is cloned once into memory and
only re-fetched on change; a cached Raft index plus the last git commit let a
whole diff cycle be skipped when nothing has moved; and per-job plan calls are
only made for jobs that could have changed. (Re-cloning the repo every cycle and
calling Register unconditionally are exactly the prior-art mistakes
[`prior-art.md`](prior-art.md) documents.)
