# nomad-gitops documentation

Start at the [project README](../README.md) for what nomad-gitops is and a
60-second quick start. This directory is the full documentation set.

## Setup

How to run it and connect it to your repo and cluster — pick the path that fits
your deployment.

- [Installation](setup/installation.md) — get the binary or container image.
- [Running nomad-gitops](setup/running.md) — as a Nomad job, a standalone
  binary, or a Docker container; and opting jobs in.
- [Git access](setup/git-access.md) — public, HTTPS token, or SSH key.
- [Nomad access](setup/nomad-access.md) — workload identity (recommended) or a
  static token, on an ACL-enabled cluster.
- [Webhooks](setup/webhooks.md) — react to pushes immediately.

## Using it

- [Common use cases](use-cases.md) — copy-paste recipes for the usual goals.
- [Configuration](configuration.md) — the full flag / env-var reference.
- [Meta-key reference](meta-keys.md) — the canonical list of every job meta key
  and its valid values.
- [Job selection](job-selection.md) — choosing which jobs are watched, and why
  Git is the source of truth.
- [Applying changes (GitOps mode)](applying-changes.md) — update policies,
  what gets applied, deregistration, and the `apply_action` reasons.
- [Rollback](rollback.md) — the flap-loop guard and active rollback.
- [Monitoring](monitoring.md) — `/healthz`, Prometheus metrics, and alerts.
- [JSON API](json-api.md) — endpoints and authentication.

## Understanding it

- [FAQ & gotchas](faq.md) — deliberate behaviour that isn't obvious.
- [Design philosophy](philosophy.md) — why it behaves the way it does.
- [Prior art](prior-art.md) — other Nomad GitOps tools and the mistakes avoided.
- [`design/`](design/) — retrospective design records for shipped features.
- [`proposals/`](proposals/) — not-yet-built ideas.

## Contributing

- [Development](development.md) — build, test, the regression suite, security
  scanning, and the release process.
- [Nomad version compatibility](nomad-versions.md) — which Nomad versions each
  release is verified against.

## License

nomad-gitops is licensed under the Apache License, Version 2.0. See
[`LICENSE`](../LICENSE).
