# Job selection

nomad-botherer does not watch every job in a cluster by default. A job must
match at least one of the configured selection criteria to be diffed:

| Criterion | Flag | Default |
|-----------|------|---------|
| Name glob | `--job-selector-glob` | *(empty — no glob selection)* |
| Meta prefix | `--managed-meta-prefix` | `gitops` |

The two criteria are a **union**: a job is selected if it matches the glob *or*
has the `<prefix>_managed` meta key set to `"true"`. With the defaults (no glob,
prefix `gitops`), only jobs declaring `gitops_managed = "true"` in their
registered Nomad meta are watched.

The prefix is a namespace for all meta keys nomad-botherer reads or writes.
Using `gitops` means the opt-in key is `gitops_managed`, and other attributes
follow the same `gitops_<attribute>` pattern. The full set is catalogued in the
[Meta-key reference](meta-keys.md).

If you need to change the prefix — for example because another team already owns
`gitops_*` on the cluster — keep `gitops` as a root and append your qualifier:
`gitops_myteam`, `gitops_platform`, etc. This keeps all nomad-botherer keys
visually grouped across teams and avoids conflicts with unrelated meta keys.

## Git is the source of truth for the meta key

**Git is always — always — the source of truth for nomad-botherer's own
behaviour.** There is deliberately no flag to change this. Concretely, when a
job has an HCL file in the watched repo, that file alone decides whether the job
is managed and under which update policy:

- `gitops_managed = "true"` in HCL selects the job even when the running job's
  meta does not carry the key. The key's absence on the live job is itself drift
  (it shows up in the plan as a meta addition), and applying that drift (policy
  permitting) is how the live job converges. Opting a running job in is a single
  commit; no manual re-register is needed.
- The reverse holds too: if the HCL exists and does *not* carry the key, a stale
  key on the live job never selects it. Removing the key from Git unmanages the
  job immediately, regardless of what the live job claims.
- Jobs not yet in Nomad are detected as `missing_from_nomad` from their HCL
  alone.

The live job's meta only matters for jobs Git knows nothing about: a running job
with `gitops_managed = "true"` and no HCL file in the repo is reported as
`missing_from_hcl`. Live-side key changes on jobs that *do* have HCL are still
noticed and logged (see
[meta-key change tracking](applying-changes.md#update-policies)), but they never
change behaviour.

## Examples

**Opting a job in via meta tag (the default method):**

```hcl
job "my-service" {
  meta {
    gitops_managed = "true"
  }
  # ...rest of job definition
}
```

**Watching all jobs in a directory:**

```bash
./nomad-botherer --job-selector-glob='*' ...
```

**Watching a named prefix:**

```bash
./nomad-botherer --job-selector-glob='production-*' ...
```

**Changing the meta prefix** (useful when sharing a cluster with multiple teams
or tools):

```bash
./nomad-botherer --managed-meta-prefix='gitops_myteam' ...
# opts in jobs with meta { gitops_myteam_managed = "true" }
```

Keeping `gitops` as the root of a custom prefix makes all nomad-botherer keys
easy to identify across a shared cluster.

**Disabling meta-based selection entirely** (glob only):

```bash
./nomad-botherer --managed-meta-prefix='' --job-selector-glob='myprefix-*' ...
```

If both `--job-selector-glob` and `--managed-meta-prefix` are empty, no jobs are
selected and no diffs will be reported. The current selection criteria are shown
on the `/` status page.
