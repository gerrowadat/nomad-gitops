# nomad-botherer — Claude instructions

## Rules

**Always update tests.** Every code change must have corresponding test coverage. No exceptions.

**Always update docs.** Config flag added or changed? Update the README table. Behaviour changed? Update the relevant section. Keep docs current.

**Do not merge PRs.** Create the branch, commit, push, open the PR — then stop. Leave merging to the human.

**Never push directly to main.** All changes go through a branch and PR, no matter how small. This includes docs, README, and config-only changes.

**Write plain commit messages and PR descriptions.** Describe what changed and why. No superlatives, no "seamlessly", no "robust", no bullet-point sales pitches. A PR description should read like a code review, not a product announcement.

**Do not re-implement incumbents.** Before writing a library or utility from scratch, check whether a well-established Go package exists for it. "Well-established" means high GitHub stars and active maintenance. If something like `go-git`, `prometheus/client_golang`, or `hashicorp/nomad/api` already does the job, use it.

**Add Prometheus metrics for observable behaviour.** Any new operation that can fail, be counted, or be timed should have a corresponding counter or gauge registered in the Prometheus registry. Follow the existing pattern: register via `promauto.With(reg)` in the constructor, keep metric names under the `nomad_botherer_` prefix.

## Project layout

```
cmd/nomad-botherer/     entry point
internal/config/        flag + env config
internal/gitwatch/      in-memory git clone and polling
internal/nomad/         HCL parsing, Nomad diff logic
internal/server/        HTTP: /, /healthz, /diffs, /metrics, /webhook
```

## Key conventions

- All config flags have env var counterparts; document both in README
- Tests use injected interfaces (`NomadJobsClient`, `DiffSource`, etc.) — keep production code testable without a live Nomad cluster
- Per-test Prometheus registries (`prometheus.NewRegistry()`) to avoid duplicate-registration panics
- `/{$}` for exact root match (Go 1.22+ ServeMux)

## Design intent

### What nomad-botherer is and where it is going

nomad-botherer is currently a **drift detector**: it watches a Git repo and a
Nomad cluster and reports when they disagree. It does not apply changes.

The intended next phase is **GitOps application**: when drift is detected,
nomad-botherer applies the Git state to Nomad. The design proposals in
`docs/proposals/` cover this in detail. `docs/prior-art.md` surveys the
existing tooling (nomad-gitops-operator, nomad-ops, Levant, Waypoint) and
explains what problems they have that nomad-botherer is trying to avoid.

Before implementing the apply side, read both proposal docs. The decisions in
them were made deliberately and the reasoning matters.

### Core design principles for the apply side

**Conservative by default.** Never register a job without running a plan first.
Never register without a CAS token. The apply path is:
`Jobs.Info()` (capture `JobModifyIndex`) → `Jobs.Plan()` (confirm diff) →
`Jobs.Register(EnforceIndex=true, JobModifyIndex=<captured>)`. If Nomad rejects
the write because the index changed, mark the update failed, trigger a fresh
diff, and let the next cycle produce a new update with current state.

**Opt-in scope via job meta.** Jobs must declare `meta { "gitops_managed" =
"true" }` in their HCL to be managed by nomad-botherer. A job without this key
is never diffed for application purposes, never registered, and never
deregistered — even if it is running in Nomad without a corresponding HCL file.
This is the "Operator Pattern in Nomad" (see scalad, Pondidum/nomad-operator).
Do not change this default without a strong reason; it is what prevents
nomad-botherer from touching manually-managed jobs on a shared cluster.

**Separate the two uses of job meta.** The `gitops_managed` flag is set by
humans in HCL and read by the tool — that is fine. Writing tool state (applied
commit, timestamps, status) back into the live job's meta is a different thing
and causes the meta-drift problem: the next `nomad job run` from plain HCL
silently removes those keys. Store apply state in Nomad Variables instead
(see `docs/proposals/change-checkpointing.md`). Do not write tool-generated
keys into the live job's meta stanza.

**No external database.** nomad-botherer should be schedulable on any node
without volume claims. The checkpointing proposal recommends Nomad Variables
(Raft-backed, CAS-protected, requires Nomad 1.4+) as the default, with a
`CheckpointStore` interface so the backend is swappable. Do not introduce
SQLite, PostgreSQL, Redis, or any other external store.

**Decoupled detection and application.** The diff check loop and the apply loop
are separate. A slow or failing apply must not delay the next diff check. The
async queue model (Alternative B in the job updates proposal) is the right
shape. Updates for the same job that arrive while a prior update is still pending
should be marked `SUPERSEDED`; the most recent intended state wins.

**Conservative deletion.** `missing_from_hcl` is an observation today. When
deregister is implemented, it should require: (a) `gitops_managed = "true"` on
the live job (confirming the operator previously registered it), and (b) probably
an explicit config flag to enable deregister at all. Purge (`purge=true` in the
Nomad API) should not be the default.

### What the existing detection code already does — preserve it

The detection side has optimisations that are easy to accidentally break:

- **Raft index skip**: `Differ.lastNomadIndex` caches the `QueryMeta.LastIndex`
  from `Jobs.List()`. If that index and `lastCommit` are both unchanged from the
  prior cycle, per-job plan calls are skipped entirely. Keep this optimisation
  when extending `Check()`.
- **In-memory clone**: `gitwatch.Watcher` uses `memory.NewStorage()` —
  no disk writes, no persistent files. Any new git interaction should stay
  in-memory or go through a clearly separate path (e.g. a checkpoint writer on
  a state branch).
- **Webhook coalescing**: `Watcher.triggerCh` is a buffered channel of size 1.
  Multiple rapid triggers collapse to one fetch. Don't change this to unbuffered.

### Prior art pitfalls to avoid

These are the specific mistakes made by nomad-gitops-operator and nomad-ops that
should not be repeated:

- **Do not re-clone on every cycle.** nomad-gitops-operator clones the full repo
  every 30 seconds. Use HEAD comparison and Raft index to skip work when nothing
  has changed.
- **Do not Register unconditionally.** nomad-gitops-operator calls Register on
  every job every cycle, whether the plan shows a diff or not. Only register
  when `Jobs.Plan()` shows a real change.
- **Do not skip CAS.** Neither existing operator uses `EnforceIndex`. This is
  a correctness gap, not a performance tradeoff. Always use the `JobModifyIndex`
  captured at detection time.
- **Do not fight the autoscaler.** nomad-ops filters out `Count`/`Scaling` diffs
  before deciding to register, so it does not overwrite autoscaler-managed counts
  every cycle. If a scaling policy is present, treat changes to `Count` as owned
  by the autoscaler, not by Git.
- **Do not hardcode intervals.** Every timing parameter should be a config flag
  with a corresponding env var. Document both in the README table.

### Vocabulary used in proposals

Use these terms consistently so the code and docs match:

| Term | Meaning |
|---|---|
| `JobDiff` | An observation: detected drift between Git and Nomad. Already implemented. |
| `JobUpdate` | An intended transition: a planned change to apply to Nomad. Not yet implemented. |
| `JobUpdateOperation` | `REGISTER` or `DEREGISTER` |
| `JobUpdateStatus` | `PENDING`, `IN_PROGRESS`, `SUCCEEDED`, `FAILED`, `SUPERSEDED` |
| opt-in key | `gitops_managed = "true"` in job HCL meta — scope selector, never written by the tool |
| meta-drift | The problem of tool-written meta keys being clobbered by the next human `nomad job run` |
| `CheckpointStore` | Interface for persisting update queue state across restarts |
| Raft index | `QueryMeta.LastIndex` from `Jobs.List()` — used for skip optimisation, not locking |
| `JobModifyIndex` | Per-job monotonic counter from `Jobs.Info()` — used as CAS token on `Jobs.Register()` |
