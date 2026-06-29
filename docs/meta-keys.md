# Meta-key reference

This is the canonical list of every job meta key nomad-botherer reads, and the
valid value for each. These keys go in a job's HCL `meta {}` block and control
which jobs are managed and how. They are **read from Git** (the source of truth)
and **never written** by nomad-botherer.

## The prefix

Every key is named `<prefix>_<attribute>`, where `<prefix>` is set by
`--managed-meta-prefix` / `MANAGED_META_PREFIX` (default `gitops`). All examples
below use the default, so the keys are `gitops_managed`, `gitops_update_policy`,
and so on. If you change the prefix to e.g. `gitops_myteam`, the keys become
`gitops_myteam_managed`, etc. Setting the prefix to empty disables meta-based
selection entirely (and none of these keys are read).

## All keys at a glance

| Key (default prefix) | Valid values | Default when absent | Purpose |
|---|---|---|---|
| `gitops_managed` | `"true"`, `"false"` | not managed via meta | Opt the job in to (or explicitly out of) management. |
| `gitops_update_policy` | `"none"`, `"image-only"`, `"full"` | `--default-update-policy` (default `none`) | How much detected drift may be applied. |
| `gitops_flap_guard` | `"history"`, `"tag"`, `"off"` | `--flap-guard` (default `history`) | Flap-loop-guard mode for this job. |
| `gitops_rollback` | `"true"`, `"false"` | `--allow-rollback` (default `false`) | Whether active rollback is enabled for this job. |

All values are strings (HCL meta values are always strings) and are
**case-sensitive** — only lowercase `true`/`false` count, not `"True"` or
`"yes"`. Any other key under the prefix, or any of these keys with a value not
listed above, is flagged (see [Validation](#validation)).

## Keys in detail

### `gitops_managed`

```hcl
meta { gitops_managed = "true" }
```

- `"true"` — the job is managed: diffed against its HCL and (policy permitting)
  reconciled.
- `"false"` — explicit opt-out. Same effect as the key being absent, but
  documents intent.

This is the default selection mechanism. A job is watched if it carries
`gitops_managed = "true"` **or** matches `--job-selector-glob` (the two are a
union). See [Job selection](job-selection.md). Git is authoritative: the HCL
value selects the job even if the running job's meta disagrees.

### `gitops_update_policy`

```hcl
meta {
  gitops_managed       = "true"
  gitops_update_policy = "image-only"
}
```

- `"none"` — detect and surface drift, never apply it.
- `"image-only"` — apply drift only when the *entire* plan diff is Docker image
  references; anything else is held.
- `"full"` — apply any detected drift.

Overrides `--default-update-policy` for this job, in either direction. An
unrecognised value is treated as `"none"` (the conservative reading) and logged.
See [Update policies](applying-changes.md#update-policies).

### `gitops_flap_guard`

```hcl
meta {
  gitops_managed    = "true"
  gitops_flap_guard = "off"
}
```

- `"history"` — compare the spec against Nomad's version history (ephemeral).
- `"tag"` — additionally tag the failed version so the block survives version GC
  (requires a non-empty `--managed-meta-prefix`).
- `"off"` — disable the flap-loop guard for this job.

Overrides `--flap-guard`. An unrecognised value falls back to the flag default.
Only affects deployment-producing jobs (service jobs with an `update` stanza and
health checks). See [Rollback](rollback.md#the-flap-loop-guard---flap-guard-default-history).

### `gitops_rollback`

```hcl
meta {
  gitops_managed  = "true"
  gitops_rollback = "true"
}
```

- `"true"` — enable active rollback for this job (revert a failed deployment to
  the last stable version).
- `"false"` — disable it, even when `--allow-rollback` is set globally.

Overrides `--allow-rollback`. An unrecognised value falls back to the flag
default. Where the job's `update` stanza sets `auto_revert`, Nomad's own rollback
wins regardless. See [Rollback](rollback.md#active-rollback---allow-rollback-default-off).

## Syntax

HCL2 block-attribute names cannot contain dots, so the block form works for all
of nomad-botherer's keys (they use underscores):

```hcl
meta {
  gitops_managed       = "true"
  gitops_update_policy = "full"
}
```

Note that `gitops.managed` (with a dot) is **not** a valid spelling of
`gitops_managed` — it is treated as an unknown key (see below). If a job also
carries dotted keys for another tool (e.g. `diun.enable`), HCL requires the
object-expression form for the whole block, and the two cannot be mixed:

```hcl
meta = {
  "gitops_managed"       = "true"
  "gitops_update_policy" = "full"
  "diun.enable"          = "true"
}
```

## Validation

nomad-botherer checks every meta key under the prefix on both the HCL side and
the live job:

- An **unknown key** under the prefix (a typo like `gitops_managd`, or
  `gitops.managed` with a dot) is logged at **WARN** — it is almost certainly a
  mistake silently changing behaviour.
- A **recognised key with an invalid value** (e.g. `gitops_managed = "True"`,
  `gitops_update_policy = "everything"`) is logged at **ERROR** — the intent is
  clear and the value is being ignored or downgraded.

Each unique issue is logged once per process and counted every cycle in
`nomad_botherer_meta_key_issues_total{job,issue}` (`issue` is `unknown_key` or
`invalid_value`). *Changes* to these keys between cycles — added, removed, or
edited, on either the HCL or the live side — are logged at INFO with the
consequence and counted in `nomad_botherer_meta_key_changes_total`. See
[Applying changes](applying-changes.md#update-policies) for more on this tracking.

## Not a meta key: failed-version tags

The flap-guard `tag` mode (`gitops_flap_guard = "tag"`) writes a **Nomad job
version tag** named `<prefix>-failed-<fingerprint>` (e.g.
`gitops-failed-ab12cd…`). That is a version tag, not a job meta key — it is not
something you set in HCL, and it is the one piece of state nomad-botherer writes
into Nomad. It is listed here only so the name is not mistaken for a meta key.
See [Rollback](rollback.md).
