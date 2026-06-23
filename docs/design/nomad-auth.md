# Design: authenticating to Nomad (workload identity)

**Status**: implemented — Unreleased. See the CHANGELOG and the README
"Authenticating to Nomad" section.
**Date**: implemented 2026-06-23

Related: [change-checkpointing.md](../proposals/change-checkpointing.md) (the
"no external state, schedulable anywhere" principle this builds on).

## Problem

nomad-botherer authenticates to the Nomad API with an ACL token. Originally the
only source was a static token (`--nomad-token` / `NOMAD_TOKEN`). When the tool
runs as a Nomad job on the cluster it watches — the common deployment — a static
token is the wrong shape:

- It has to be minted, stored as a secret, injected, and rotated by hand.
- A long-lived static token is a standing credential; a short-lived one expires
  and takes the tool down.

Nomad already solves this for workloads with **workload identity**: a task gets
its own identity, an ACL policy is bound to that identity, and Nomad issues and
**rotates** a token for it automatically. The tool should use that and keep the
static token only for manual running and testing.

## What Nomad provides

With `identity { file = true }` on the task, Nomad writes the task's default
identity token to `${NOMAD_SECRETS_DIR}/nomad_token` and refreshes the file
before the token expires. An ACL policy is associated with the workload via
`nomad acl policy apply -job <id> …` (optionally `-group`/`-task`). The token is
a normal ACL token as far as the API client is concerned; only its lifecycle
differs (Nomad-managed, rotating).

There is also `identity { env = true }`, which sets `$NOMAD_TOKEN`. We
deliberately do **not** use that: the environment is captured once at task start
and never updated, so an env token silently expires. The file is the only source
that reflects rotation.

## Design

Token resolution has three sources, in precedence order (`resolveNomadToken`):

1. **Token file** (`--nomad-token-file` / `NOMAD_TOKEN_FILE`) — preferred.
   Re-read every `--nomad-token-poll-interval` (default `30s`) and applied to the
   live API client via `Client.SetSecretID`, which the client reads per request,
   so a rotated token takes effect without a reconnect or restart. When no token
   is configured at all, `${NOMAD_SECRETS_DIR}/nomad_token` is **auto-detected**
   (by file existence, since `NOMAD_SECRETS_DIR` is always set but the file only
   appears with `identity { file = true }`). So a correctly set-up job needs zero
   token configuration.
2. **Static token** (`--nomad-token` / `NOMAD_TOKEN`) — manual/testing. Not
   re-read.
3. **None** — anonymous; works only with ACLs disabled.

### Precedence rationale

- **An explicit token file beats a static token.** Setting `--nomad-token-file`
  is an unambiguous "use workload identity" signal; if a static token is also
  present (e.g. a stray `NOMAD_TOKEN`), the refreshing file is the safer choice
  and the static token is ignored with a warning.
- **A static token beats auto-detection.** Auto-detection is a fallback, not an
  override: passing `--nomad-token` for a manual test must still work even when
  run inside a task environment where the WI file happens to exist.
- The net effect is "prefer workload identity": the normal Nomad deployment
  (`identity { file = true }`, no `NOMAD_TOKEN`) auto-uses the rotating file,
  while manual runs use whatever token you pass.

### Why a poll, not a watch

The refresher (`refreshTokenFile`) polls the file rather than using inotify/
fsnotify: it avoids a new dependency, the token rotates on the scale of hours so
30 s latency is irrelevant, and a poll is trivially testable. The interval is a
flag (`--nomad-token-poll-interval`) per the project's "no hardcoded intervals"
rule. The refresher runs as its own goroutine (`Differ.RunTokenRefresher`),
mirroring `RunApplier`, and is a no-op for the static and anonymous cases.

### No new persistent state

This introduces nothing for nomad-botherer to own: the token file is written and
rotated by Nomad, read-only from the tool's perspective. Consistent with "no
external database, schedulable on any node" — the workload-identity path needs no
volume and no secret store.

## Observability

`nomad_botherer_nomad_token_refreshes_total{result}` — `rotated` when a changed
token is applied, `error` when the file can't be read (the previous token is
kept). The active source (file / static / none) is logged once at startup.

## Out of scope / future

- **Per-request token re-read.** Not needed: `SetSecretID` updates the shared
  client config that every request reads, so the poll is sufficient.
- **Vault/OIDC or non-default identities.** The default workload identity is
  enough for Nomad's own API. Custom identities (e.g. a specific `aud`) are not
  required and not implemented.
- **TLS client certs / mTLS to the Nomad API.** Unchanged; `DefaultConfig` still
  honours the standard `NOMAD_*` TLS environment variables.
