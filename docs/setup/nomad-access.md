# Nomad access (authentication)

When the cluster has ACLs enabled, nomad-botherer needs a token to call the
Nomad API. It picks one from three sources, in this order:

1. **A token file** (`--nomad-token-file` / `NOMAD_TOKEN_FILE`) — **preferred**.
   The file is re-read every `--nomad-token-poll-interval` (default `30s`), so a
   rotating token stays current. This is how Nomad **workload identity** works:
   when nomad-botherer runs as a Nomad task with `identity { file = true }`,
   Nomad writes the task's own identity token to `${NOMAD_SECRETS_DIR}/nomad_token`
   and rotates it before it expires. That path is **auto-detected** when no token
   is otherwise configured, so a correctly set-up job needs no token settings at
   all.
2. **A static token** (`--nomad-token` / `NOMAD_TOKEN`) — for manual running and
   testing. It is never re-read, so it is unsuitable for a long-running
   deployment with short-lived tokens.
3. **None** — anonymous access, which works only when the cluster has ACLs
   disabled.

A token file takes precedence over a static token; the static token wins over
auto-detection (so `--nomad-token` still works for testing inside a task). The
chosen source is logged at startup. The design rationale is in
[`docs/design/nomad-auth.md`](../design/nomad-auth.md).

## Workload identity setup (recommended)

Running under Nomad, give the job API access without managing any token:

1. **Expose the identity token to the task.** The example job
   ([`examples/nomad-botherer.hcl`](../../examples/nomad-botherer.hcl)) already
   does this:

   ```hcl
   task "nomad-botherer" {
     identity {
       file = true   # writes the token to ${NOMAD_SECRETS_DIR}/nomad_token
     }
     # ...
   }
   ```

   Do **not** also set `env = true` and read `NOMAD_TOKEN` from the environment:
   the env value is captured once at task start and never refreshed, so it will
   eventually expire. nomad-botherer auto-detects the file and re-reads it.

2. **Bind an ACL policy to the workload.** Write a policy with the capabilities
   nomad-botherer needs, then attach it to the job's workload identity:

   ```hcl
   # nomad-botherer-policy.hcl
   namespace "default" {
     # read-job + list-jobs: detect drift. submit-job: plan, register,
     # deregister, and revert (the apply side). Drop submit-job for a
     # detection-only deployment.
     capabilities = ["list-jobs", "read-job", "submit-job"]
   }
   ```

   ```bash
   nomad acl policy apply \
     -namespace default -job nomad-botherer \
     nomad-botherer nomad-botherer-policy.hcl
   ```

   The `-job nomad-botherer` flag ties the policy to that job's workload
   identity, so any allocation of the job authenticates with exactly these
   capabilities — no token to create, distribute, or rotate. Add
   `-group`/`-task` flags to narrow it further if you like.

For a detection-only deployment, omit `submit-job`. To watch more than one
namespace, grant the capabilities on each (or use a wildcard namespace policy).

## Static token (manual / testing)

Running the binary by hand, pass a token the usual way:

```bash
NOMAD_TOKEN=$(nomad acl token self -t …) ./nomad-botherer --repo-url …
# or
./nomad-botherer --nomad-token <token> --repo-url …
```

A static token does not refresh, so it is fine for manual use but unsuitable
for a long-running deployment with short-lived tokens — use the file/workload
identity path there instead.
