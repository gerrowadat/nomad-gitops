# Installation

Get a nomad-gitops binary or container image. To then run it, see
[Running nomad-gitops](running.md).

## From a Docker image (recommended)

```bash
docker pull ghcr.io/gerrowadat/nomad-gitops:latest
```

Pre-built images are published to GitHub Container Registry for `linux/amd64`
and `linux/arm64` (Raspberry Pi 4+).

| Tag | Description |
|-----|-------------|
| `latest` | Most recent release |
| `1`, `1.2`, `1.2.3` | Semver aliases, updated on each release |

## From source

Requires Go 1.27+ (the version in `go.mod`; an older `go` command downloads it automatically).

```bash
git clone https://github.com/gerrowadat/nomad-gitops.git
cd nomad-gitops
make build
./nomad-gitops --help
```

`make install` installs the binary to `$GOPATH/bin`. See
[Development](../development.md) for the full build/test workflow.
