# Git access

nomad-botherer clones the repo entirely into memory (it writes nothing to disk)
and re-fetches it on every poll interval and on each webhook. For a private
repo it needs credentials. Three cases:

## Public repo

No credentials needed:

```bash
./nomad-botherer --repo-url https://github.com/myorg/nomad-jobs.git ...
```

## Private repo over HTTPS (token)

Use a GitHub PAT, GitLab deploy token, or equivalent:

```bash
./nomad-botherer \
  --repo-url https://github.com/myorg/nomad-jobs.git \
  --git-token ghp_... \
  ...
```

or via `GIT_TOKEN`. The token **requires an `https://` repo URL** — it is
refused for a plain `http://` URL, which would send it in cleartext.

Running under Nomad, supply the token from a Nomad Variable rather than
hardcoding it in the job's `env {}` block. The example job
([`examples/nomad-botherer.hcl`](../../examples/nomad-botherer.hcl)) shows a
`template` stanza that reads `GIT_TOKEN` from `nomad/jobs/nomad-botherer`.

## Private repo over SSH (key)

```bash
./nomad-botherer \
  --repo-url git@github.com:myorg/nomad-jobs.git \
  --git-ssh-key ~/.ssh/id_ed25519 \
  ...
```

| Flag | Env | Purpose |
|---|---|---|
| `--git-ssh-key` | `GIT_SSH_KEY` | Path to the private key file |
| `--git-ssh-key-password` | `GIT_SSH_KEY_PASSWORD` | Key passphrase, if any |
| `--git-ssh-known-hosts` | `GIT_SSH_KNOWN_HOSTS` | `known_hosts` file for host-key verification (defaults to the system locations) |

Host-key verification is on by default. Point `--git-ssh-known-hosts` at a file
containing the remote's host key, or rely on the default `~/.ssh/known_hosts`
search. In a container, mount both the key and a `known_hosts` file.

See [Configuration](../configuration.md) for the full flag reference.
