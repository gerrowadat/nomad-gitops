# Nomad Version Compatibility

This table records which Nomad versions have been verified to work with each
nomad-botherer release by running the full regression suite.

## How to run

```bash
# Against a specific Nomad version (pulls Docker image automatically)
NOMAD_VERSION=1.9.3 go test -tags=regression -timeout 15m -v ./tests/regression/...

# Against multiple versions
make test-regression-versions NOMAD_VERSIONS="1.9.3 1.10.2"

# Against an already-running cluster
NOMAD_ADDR=http://127.0.0.1:4646 go test -tags=regression -timeout 15m -v ./tests/regression/...
```

After a successful run, add a row to the table below and update `tests/regression/compat.go`.

## Compatibility matrix

| nomad-botherer | Nomad 1.8.x | Nomad 1.9.x | Nomad 1.10.x | Notes |
|----------------|:-----------:|:-----------:|:------------:|-------|
| (unreleased)   |             |             |              | Matrix will be filled as releases are cut |

### Column key

- ✅ All regression tests pass
- ⚠️ Passes with caveats (see Notes)
- ❌ Known failures
- — Not tested

## Adding a new row

1. Run: `NOMAD_VERSION=X.Y.Z go test -tags=regression -timeout 15m -count=1 ./tests/regression/...`
2. Note the result (pass/fail/caveats).
3. Add a row to the table above.
4. Add a `VersionRecord` entry to `tests/regression/compat.go`.
5. Open a PR with both changes.

## Known Nomad API compatibility notes

- **ParseHCL**: available since Nomad 0.8; no known breaking changes in the 1.x series.
- **Jobs.Plan with diff=true**: stable across 1.x; the `Diff.Type` field values
  ("None", "Added", "Edited", "Destroyed") are unchanged.
- **Jobs.List `QueryMeta.LastIndex`**: Raft index semantics are stable.
- **raw_exec driver**: available in dev mode across all 1.x versions; requires
  `--privileged` when Nomad itself runs inside Docker.
