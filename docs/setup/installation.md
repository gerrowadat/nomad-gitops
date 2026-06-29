# Installation

Get a nomad-botherer binary or container image. To then run it, see
[Running nomad-botherer](running.md).

## From a Docker image (recommended)

```bash
docker pull ghcr.io/gerrowadat/nomad-botherer:latest
```

Pre-built images are published to GitHub Container Registry for `linux/amd64`
and `linux/arm64` (Raspberry Pi 4+).

| Tag | Description |
|-----|-------------|
| `latest` | Most recent release |
| `1`, `1.2`, `1.2.3` | Semver aliases, updated on each release |

## From source

Requires Go 1.25+.

```bash
git clone https://github.com/gerrowadat/nomad-botherer.git
cd nomad-botherer
make build
./nomad-botherer --help
```

`make install` installs the binary to `$GOPATH/bin`. See
[Development](../development.md) for the full build/test workflow.
