# Proposal: configurable drift-ignore rules

**Status**: draft
**Date**: 2026-07-19

## Background

Today nomad-gitops has exactly one field it will never treat as drift: a task
group's `Count` (and its `Scaling` object), and only when that group carries
an enabled Nomad `Scaling` policy. This is hardcoded in three places that all
have to agree with each other:

- `classifyDiff` (`internal/nomad/classify.go`) is handed a `skip` map built
  from `autoscaledGroups(job)` and drops `Count`/`Scaling` fields before they
  are bucketed into a `DiffClass`.
- `applyUpdate` (`internal/nomad/applier.go`) sets `RegisterOptions.PreserveCounts`
  when `preserveCounts` was computed from the same `autoscaledGroups` check
  (`internal/nomad/differ.go:1425`), so the actual `Jobs.Register` write asks
  Nomad to keep the live count instead of overwriting it with the HCL value.
  This works because Nomad's own API has native support for preserving
  `Count` specifically — nothing else.
- `specFingerprint` (`internal/nomad/rollback.go`) strips the same
  `Count`/`Scaling` fields from autoscaled groups before hashing a spec for
  the flap-loop guard, "so an autoscaler nudge does not change the
  fingerprint." This is a second, independent copy of the same exclusion
  logic that has to be kept in sync with the first by hand.

Nomad Autoscaler's count-scaling plugins are not the only thing that mutates
a running job out from under its Git spec:

- Nomad Autoscaler's **Dynamic Application Sizing** (DAS) plugin writes
  `Resources` (CPU/MemoryMB) recommendations directly onto tasks — the same
  shape of problem as `Count`, but for `Resources` instead.
- Vault Agent / `consul-template` template rendering can perturb `Templates`
  content or checksums in ways operators don't want reported as drift.
- Sidecar/mesh injectors, admission-style tooling, or other operators sharing
  the cluster sometimes write their own `Meta` keys or `Network` port
  assignments onto a job nomad-gitops also manages.
- Some clusters scale `Count` by hand or by an external script without a
  formal Nomad `Scaling` stanza, so the existing autoscaler detection (which
  keys off the *presence* of a `Scaling` policy) doesn't cover them.

This proposal generalizes "fields nomad-gitops will never call drift" into a
per-job configurable mechanism — a meta key and a global flag — that replaces
the single hardcoded `Count` special case with something that covers this
whole class of problem, while keeping today's default behavior unchanged for
anyone already relying on it.

---

## Decisions made before writing this proposal

These were resolved with the maintainer up front because they shape
everything below; recorded here so the reasoning doesn't have to be
reconstructed later.

1. **Addressing scheme: both.** A fixed vocabulary of named categories
   (`count`, `resources`, `env`, …) covers the common cases cheaply and
   safely. A `path:`-prefixed escape hatch covers the rest (one specific env
   var, one specific meta key) at the cost of a small path syntax.
2. **Flag/meta merge: union, with explicit narrowing.** The effective ignore
   set for a job is the global flag's list plus the job's own meta list,
   combined — not an override. A job can also *remove* something the global
   flag ignores by listing it with a `!` prefix.
3. **Ignore scope: fully invisible.** An ignored field is excluded from
   `classifyDiff` entirely — not counted as drift, not shown on `/diffs`, not
   in any drift metric. This matches today's `Count` behavior exactly; it does
   not adopt update-policy's "detect always, gate application" model.
4. **Apply-time writes: splice live values in before registering.** Where
   practical, ignoring a field also means preserving its live value across an
   apply triggered by *other* drift — the same guarantee `Count` already gets
   from `RegisterOptions.PreserveCounts` — rather than only suppressing
   detection and letting an unrelated apply silently overwrite it.
5. **The autoscaler `Count` default stays automatic.** A task group with an
   enabled `Scaling` policy keeps getting `count` excluded with zero
   configuration, exactly as today. This proposal re-implements that behavior
   as one instance of the general mechanism rather than removing it; existing
   deployments see no behavior change.

---

## Addressing scheme

### Named categories

A fixed vocabulary, chosen to line up with how `classifyDiff` already buckets
plan-diff leaves (image and managed-meta already get this treatment; this
extends the same idea to more buckets):

| Category | What it covers | Motivating case |
|---|---|---|
| `count` | `TaskGroup.Count` / `Scaling` | Nomad Autoscaler (horizontal), manual out-of-band scaling |
| `resources` | Task `Resources` (CPU, MemoryMB, cores, …) | Nomad Autoscaler Dynamic Application Sizing |
| `env` | Task `Env` | Secrets/config injected by tooling other than Git |
| `meta` | `Meta` fields **not** under `--managed-meta-prefix` | Sidecar injectors, other operators tagging the same job |
| `templates` | Task `Templates` | Vault Agent / consul-template rendering |
| `constraints` | `Constraint` objects | Node-affinity or placement tooling that rewrites constraints |
| `network` | `Network` (ports) | Dynamic port allocators |
| `volumes` | `Volume` / `VolumeMount` | CSI provisioners that normalize volume attributes |

`image` is deliberately **not** ignorable through this mechanism. It already
has a dedicated, more specific control — `gitops_update_policy = "image-only"`
— and giving it a second, competing path to the same outcome would make the
two mechanisms fight over the same field.

Categories are job-wide by default: `resources` ignores every task's
`Resources`, not just one group's. Scoping to a single group or task is what
the path escape hatch is for.

### Path escape hatch

For anything a category is too coarse to express — one specific env var, one
specific meta key, one group's `Count` when the operator doesn't want the
whole job's counts ignored — an entry can instead be a dotted path pattern,
prefixed `path:`, using the same names the plan-diff tree already uses
internally (mirroring `classifyDiff`'s own traversal):

```
path:TaskGroups.web.Tasks.*.Env.DEBUG_TOKEN
path:TaskGroups.*.Meta.owner
path:TaskGroups.canary.Count
```

`*` matches any group or task name at that segment. Path patterns are matched
against the same `(TaskGroups, Tasks, Objects, Fields)` tree
`classifyDiff` already walks, by threading the current group/task name and
object-name chain through the existing recursion instead of adding a second
traversal.

A malformed path (unbalanced segments, empty component) is treated the same
way an unknown category is: logged once via the existing
`nomad_gitops_meta_key_issues_total` mechanism and **dropped**, not honored —
the same "conservative reading" the rest of this codebase already uses for
bad meta values (an unrecognised `gitops_update_policy` value falls back to
`none`, never to `full`). A broken ignore rule must fail toward "still show me
the drift," never toward "silently hide more than intended."

---

## Configuration surface

### Meta key

```hcl
meta {
  gitops_managed     = "true"
  gitops_ignore_diff = "resources,env,path:TaskGroups.web.Tasks.*.Env.DEBUG_TOKEN"
}
```

`gitops_ignore_diff` takes a comma-separated list of entries, each either a
bare category name, a `path:`-prefixed pattern, or a `!`-prefixed category
name that narrows the global list back down (see below). Same validation
posture as the other keys in `docs/meta-keys.md`: an unrecognised entry is
logged and dropped, not fatal to the rest of the key.

### Flag

| Flag | Env var | Default |
|---|---|---|
| `--default-ignore-diff` | `DEFAULT_IGNORE_DIFF` | *(empty)* |

Same comma-separated syntax as the meta key (`!` narrowing has no effect at
the global level — there's nothing to narrow away from — but is accepted and
ignored rather than rejected, to keep the grammar identical in both places).
Validated at `config.Load()` the same way `--default-update-policy` is:
an unknown category or unparsable path fails startup with a clear error,
since this is a flag a human is typing, not meta someone might have committed
months ago.

### Merge semantics

The effective ignore set for a job is:

```
effective = builtin(job)  ∪  (flagList  ∖  narrowed)  ∪  metaAdditions
```

where `builtin(job)` is `count` for every task group with an enabled
`Scaling` policy (see below — not configurable, not narrowable), `flagList`
is `--default-ignore-diff`, `narrowed` is the set of categories the job's
meta lists with a `!` prefix, and `metaAdditions` is everything else the
job's meta lists.

| `--default-ignore-diff` | `gitops_ignore_diff` | Effective (excluding builtin) |
|---|---|---|
| *(empty)* | *(absent)* | `{}` |
| `resources` | *(absent)* | `{resources}` |
| `resources` | `env` | `{resources, env}` — union |
| `resources` | `!resources` | `{}` — narrowed away |
| `resources` | `!resources,templates` | `{templates}` |
| *(empty)* | `resources` | `{resources}` — meta can add even when the flag is empty |
| `resources` | `!network` | `{resources}` — `!network` is a no-op (nothing to narrow); logged as a lint-level issue |

A job cannot narrow away `builtin(job)`'s per-group `count` entry via
`!count` — that reflects a live Nomad `Scaling` policy, not a configuration
choice, and removing it would mean fighting the autoscaler, which is exactly
what this whole mechanism (and the "do not fight the autoscaler" design rule)
exists to prevent.

If a job's own meta lists both `x` and `!x`, that's a self-contradiction:
treated as invalid, `x` is **not** ignored (conservative), and it's logged
the same way a recognised-key-bad-value case is elsewhere in `metacheck.go`.

---

## Where ignoring takes effect

**`classifyDiff`.** The existing `skip map[string]bool` — today built only
from `autoscaledGroups` for `Count` — generalizes to the job's full effective
ignore set. Category membership reuses the same field/object-name bucketing
`classifyDiff` already does for `image` and managed-meta; path patterns are
tested against a path stack (current group name, task name, object chain)
threaded through the existing `addObject`/`addFields` recursion, rather than
a second walk of the diff tree.

**`specFingerprint` (`internal/nomad/rollback.go`).** This function already
independently strips `Count`/`Scaling` for autoscaled groups and the
managed-meta prefix, for exactly the same reason `classifyDiff` does — "so an
autoscaler nudge does not change the fingerprint." That's a second
hand-maintained copy of today's one special case; generalizing the ignore
mechanism without also generalizing this call site would leave the flap-loop
guard silently out of sync with whatever the operator configured. Concretely:
if `templates` is ignored for drift purposes but *not* stripped from the
fingerprint, a live spec that differs from the last-known-failed version only
in externally-rendered template content would fingerprint as "different,"
and the guard would never recognize it as the same already-failed spec —
defeating the guard for exactly the jobs this feature is meant to help. Both
call sites must consume the same effective-ignore-set computation.

**`applyUpdate` (`internal/nomad/applier.go`).** Beyond suppressing
detection, ignored *category* fields also get spliced: before `Jobs.Plan` /
`Jobs.Register`, for each ignored category, the live job's value for that
field is copied into the HCL-parsed job being registered, for every
group/task name present on both sides. `count` keeps using Nomad's native
`RegisterOptions.PreserveCounts` (no need to reimplement what Nomad already
does correctly); the other categories need nomad-gitops to do the splice
itself, since Nomad's register API has no equivalent for anything but
`Count`. Groups/tasks added or removed by Git are unaffected — there's no
live value to preserve for a task group that doesn't exist yet.

**Path-based rules are detection-only in this proposal.** Splicing a category
is tractable because it targets one well-known, whole field per task/group.
Splicing an arbitrary path is not: the pattern might match a field that was
added or removed rather than edited, or resolve to zero matches for a
particular job's shape, and merging "the live value at this path" back into a
freshly-parsed HCL job without a real risk of producing an inconsistent spec
is a meaningfully harder problem. So: a `path:` rule suppresses that leaf from
drift and from the fingerprint, exactly like a category, but does **not**
get preserved across an unrelated apply — if some other drift triggers a
register, a path-ignored field is written to whatever the HCL says, same as
any other field. This asymmetry is called out explicitly in
`docs/meta-keys.md` when this ships, since "ignored" quietly means two
different guarantees depending on which addressing scheme was used.

---

## Autoscaler `Count`: folded in, not removed

Per decision 5, the existing behavior is preserved exactly: any task group
with an enabled `Scaling` policy gets `count` in its effective ignore set
unconditionally, sourced from live Nomad state rather than from
`--default-ignore-diff` or `gitops_ignore_diff`. The difference after this
proposal ships is purely internal — `autoscaledGroups` becomes one input to
`effectiveIgnoreSet(job, liveGroups)` instead of a bespoke `skip` map
constructed separately in three files. No migration, no config change
required for anyone already relying on today's behavior.

---

## Observability

Following the existing convention (`promauto.With(reg)`, `nomad_gitops_`
prefix):

- `nomad_gitops_diff_ignored_total{job, category}` — counter, incremented
  each time a leaf is excluded from a plan diff by an ignore rule. `category`
  is the category name, or the literal string `path` for `path:` matches (raw
  path strings are not used as a label value — unbounded cardinality).
- `nomad_gitops_preserve_writes_total{job, category}` — counter, incremented
  each time the apply-time splice actually copied a live value into an
  outgoing `Jobs.Register` call. Distinguishes "we suppressed drift" from "we
  actively intervened in a write," which matters for auditing what
  nomad-gitops changed about the job it registered versus the literal HCL.
- Existing `nomad_gitops_meta_key_issues_total{job, issue}` covers malformed
  `gitops_ignore_diff` entries (unknown category, bad path syntax,
  contradictory `x`/`!x`) — no new metric needed there, just new cases
  feeding the existing one.

---

## Interaction with update policies

Orthogonal, the same way image-drift classification and update policy are
orthogonal today. Ignore rules act *before* `classifyDiff` buckets a diff
into `DiffClassImageOnly` / `DiffClassManagedMetaOnly` / `DiffClassOther` —
an ignored field is removed from consideration entirely, so it never
participates in that bucketing at all. A job can be `gitops_update_policy =
"image-only"` and also ignore `resources`; the two gates don't need to know
about each other.

---

## Testing considerations

- `classify_test.go`: table-driven cases per category (one field changed,
  category ignores it; same field changed, category not configured, it's
  `DiffClassOther`), plus path-pattern matches and non-matches, plus the
  contradictory-narrowing case.
- A new config-validation test set for `--default-ignore-diff` parsing
  (unknown category, malformed path, mixed valid/invalid entries).
- `metacheck_test.go`: `gitops_ignore_diff` added to the switch in
  `validateManagedMeta`, with unknown-key and invalid-value cases.
- `rollback_test.go`: a fingerprint test asserting a category-ignored field's
  change does not alter `specFingerprint`'s output.
- `apply_test.go`: a splice test per non-`count` category, asserting the
  registered job carries the live value for an ignored field even when an
  unrelated field's change triggered the apply.

---

## Docs to update once this ships

Per this repo's own documentation rules:

- `docs/configuration.md` — add `--default-ignore-diff` / `DEFAULT_IGNORE_DIFF`
  to the flag table.
- `docs/meta-keys.md` — add `gitops_ignore_diff`, including the category
  table, path syntax, narrowing syntax, and the category-vs-path preservation
  asymmetry.
- `docs/applying-changes.md` — note that ignored fields never appear as drift
  and (for categories) are preserved across unrelated applies.
- This file moves to `docs/design/`, retitled `# Design: configurable
  drift-ignore rules`, with status/date updated to record what shipped and
  where it diverged from this draft.

---

## Open questions

- **Starting category list.** Ship all eight categories above, or start with
  the ones that have a concrete motivating case today (`count`, `resources`,
  `env`, `meta`) and add `templates`/`constraints`/`network`/`volumes` only
  when someone needs them? Smaller surface is easier to get right first.
- **Does path-based preservation ever get built?** This proposal leaves
  `path:` rules detection-only permanently, but a future proposal could
  revisit narrow, well-defined path shapes (e.g. exactly one env var) as
  splice-able, if the asymmetry proves confusing in practice.
- **Per-group scoping without the path escape hatch.** Categories are
  job-wide; is that coarse enough to be annoying for jobs with one delicate
  group and several disposable ones? The path syntax covers this today, but
  it's more ceremony than a hypothetical `gitops_ignore_diff@web = "resources"`
  per-group meta convention. Not proposed here since Nomad meta doesn't
  cleanly support per-group keys without a naming convention of our own
  invention — worth revisiting only if real usage asks for it.
- **`--redact-secrets` interaction.** An ignored `env` category means secret
  values in `Env` are never diffed at all; is there still value in redacting
  them for the `nomad_gitops_diff_ignored_total` path, or does "ignored"
  mean nomad-gitops shouldn't even read them into memory for comparison?
  Likely a non-issue (the plan-diff still comes back from Nomad regardless),
  but worth a look during implementation.
