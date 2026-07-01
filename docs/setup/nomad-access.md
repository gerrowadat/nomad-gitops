# Nomad access (authentication)

When the cluster has ACLs enabled, nomad-botherer needs an ACL token to call the
Nomad API. It picks one from these sources, in precedence order:

1. **Workload-identity login** (`--nomad-login-auth-method`) — the way to use
   Nomad workload identity. nomad-botherer exchanges the task's identity **JWT**
   for a real ACL token via `POST /v1/acl/login`, and re-exchanges it before it
   expires. See [Workload identity](#workload-identity-recommended-under-nomad).
2. **A token file** (`--nomad-token-file` / `NOMAD_TOKEN_FILE`) — a real ACL
   token **SecretID** in a file, re-read every `--nomad-token-poll-interval`
   (default `30s`). Use it for a token written by a sidecar, or a rotating
   static token. It must be a 36-char SecretID, **not** a raw workload-identity
   JWT.
3. **A static token** (`--nomad-token` / `NOMAD_TOKEN`) — for manual running and
   testing. Never re-read.
4. **None** — anonymous access, which works only when the cluster has ACLs
   disabled.

The chosen source is logged at startup.

> **Why not just use the identity JWT as a token?** A raw workload-identity JWT
> authenticates *read* RPCs, but Nomad's `Job.Plan` RPC rejects it
> (`500 … UUID must be 36 characters`), and nomad-botherer runs a plan on every
> drift check. So the JWT must be *exchanged* for an ACL token — that is what
> login mode does. (See
> [issue #74](https://github.com/gerrowadat/nomad-botherer/issues/74).) A
> previous version tried to use the JWT directly; that did not work.

## Workload identity (recommended under Nomad)

This gives the job API access with no long-lived token to manage — the identity
JWT is short-lived and Nomad-issued, and nomad-botherer exchanges and refreshes
it automatically. It has a few one-time cluster prerequisites.

### 1. A JWT auth method

Create a JWT ACL auth method that trusts Nomad's own identity issuer (its JWKS),
if you don't already have one:

```hcl
# nomad-workloads.hcl
type          = "JWT"
token_locality = "local"
max_token_ttl = "30m"

config {
  jwks_url          = "https://nomad.example.com:4646/.well-known/jwks.json"
  bound_audiences   = ["nomad.io"]
  claim_mappings    = { nomad_job_id = "nomad_job_id", nomad_namespace = "nomad_namespace" }
}
```

```bash
nomad acl auth-method create -name nomad-workloads -type JWT \
  -max-token-ttl 30m -token-locality local -config @nomad-workloads.hcl
```

`max_token_ttl` bounds how long each exchanged token is valid — nomad-botherer
re-logins before it expires. `bound_audiences` must match the identity's `aud`
(next step).

### 2. An identity whose audience matches the auth method

Give the task an `identity` block whose `aud` matches the auth method's
`bound_audiences`, written to a file. **The *default* identity's audience does
not match a custom auth method**, so use a named identity:

```hcl
task "nomad-botherer" {
  identity {
    name = "nomad-api"
    aud  = ["nomad.io"]
    file = true          # -> ${NOMAD_SECRETS_DIR}/nomad_nomad-api.jwt
  }
  # ...
}
```

Do **not** rely on `env = true` for the API token — env is captured once and
never refreshed.

### 3. A binding rule mapping the job to a policy

Write the policy nomad-botherer needs and a binding rule that grants it to this
job's identity on login:

```hcl
# nomad-botherer-policy.hcl
namespace "default" {
  # read-job + list-jobs: detect drift. submit-job: plan, register, deregister,
  # revert (the apply side). Drop submit-job for a detection-only deployment.
  capabilities = ["list-jobs", "read-job", "submit-job"]
}
```

```bash
nomad acl policy apply nomad-botherer nomad-botherer-policy.hcl

nomad acl binding-rule create \
  -auth-method nomad-workloads -bind-type policy \
  -bind-name nomad-botherer \
  -selector 'value.nomad_job_id == "nomad-botherer"'
```

`submit-job` is required for `Job.Plan` even in detection-only mode — Nomad's
plan RPC needs it. (There is no read-only capability that covers plan.)

### 4. Point nomad-botherer at the login

Set the auth method (this enables login mode) and the JWT file:

```
NOMAD_LOGIN_AUTH_METHOD = "nomad-workloads"
NOMAD_LOGIN_JWT_FILE    = "${NOMAD_SECRETS_DIR}/nomad_nomad-api.jwt"
```

On startup you'll see `Obtained a Nomad ACL token via workload-identity login`
with the token's expiry; nomad-botherer re-logins at roughly half that interval.
The `--nomad-login-jwt-file` default is `${NOMAD_SECRETS_DIR}/nomad_token` (the
default identity); set it explicitly to the named-identity file as above.

## Sidecar alternative (no native login)

If you cannot use native login, a sidecar can perform the `/v1/acl/login`
exchange and write the resulting SecretID to a shared file, and nomad-botherer
reads it via `--nomad-token-file`. Native login is simpler and needs no sidecar.

## Static token (manual / testing)

Running the binary by hand, pass a real ACL token SecretID:

```bash
NOMAD_TOKEN=$(nomad acl token self -t …) ./nomad-botherer --repo-url …
# or
./nomad-botherer --nomad-token <secret-id> --repo-url …
```

A static token does not refresh, so it is fine for manual use but unsuitable for
a long-running deployment with short-lived tokens — use login mode there.
