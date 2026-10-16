# Nomad Version Compatibility

This table records which Nomad versions have been verified to work with each
nomad-gitops release by running the full regression suite.

## How to run

```bash
# Against a specific Nomad version (pulls Docker image automatically)
NOMAD_VERSION=1.9.7 go test -tags=regression -timeout 15m -v ./tests/regression/...

# Against multiple versions
make test-regression-versions NOMAD_VERSIONS="1.9.7 1.10.5 1.11.3 2.0.7"

# Against an already-running cluster
NOMAD_ADDR=http://127.0.0.1:4646 go test -tags=regression -timeout 15m -v ./tests/regression/...
```

After a successful run, add a row to the table below and update `tests/regression/compat.go`.

## Compatibility matrix

| nomad-gitops | Nomad 1.9.x | Nomad 1.10.x | Nomad 1.11.x | Nomad 2.0.x | Notes |
|----------------|:-----------:|:------------:|:------------:|:-----------:|-------|
| v1.2.0         | ✅ 1.9.7    | ✅ 1.10.5    | ✅ 1.11.3    | ✅ 2.0.7    | Freezes (Nomad Variables read, and write with `--enable-freeze-api`). The freeze regression tests exercise the Variables API end to end on every version above |
| v1.1.0         | ✅ 1.9.7    | ✅ 1.10.5    | ✅ 1.11.3    | ✅ 2.0.4    | `apply_detail` on diffs; per-job drift alert; `--reclone-interval`; dependency bumps (no Nomad-facing behaviour change) |
| v1.0.2         | ✅ 1.9.7    | ✅ 1.10.5    | ✅ 1.11.3    | ✅ 2.0.4    | Security/dependency patch; plan-diff depth cap (no behaviour change for well-formed jobs) |
| v1.0.0         | ✅ 1.9.6    | ✅ 1.10.5    | ✅ 1.11.3    | ✅ 2.0.2    | Renamed from nomad-botherer; metrics `nomad_botherer_*` → `nomad_gitops_*` (no behaviour change) |
| v0.9.1         | ✅ 1.9.6    | ✅ 1.10.5    | ✅ 1.11.3    | ✅ 2.0.2    | Workload identity fixed via `/v1/acl/login` token exchange (#74) |
| v0.9.0         | ✅ 1.9.6    | ✅ 1.10.5    | ✅ 1.11.3    | ✅ 2.0.2    | Nomad workload-identity auth; policy-widening defers pre-existing drift (#69) |
| v0.8.0         | ✅ 1.9.6    | ✅ 1.10.5    | ✅ 1.11.3    | ✅ 2.0.2    | Adds flap-loop guard and optional active rollback |
| v0.7.0         | ✅ 1.9.6    | ✅ 1.10.5    | ✅ 1.11.3    | ✅ 2.0.2    | Adds deregistration of repo-removed jobs |
| v0.6.0         | ✅ 1.9.6    | ✅ 1.10.5    | ✅ 1.11.3    | ✅ 2.0.2    | Full apply suite (policies, pre-existing drift, apply_action) |
| v0.4.0         | ✅ 1.9.6    | ✅ 1.10.5    | ✅ 1.11.3    | ✅ 2.0.2    |       |
| v0.1.2         | ✅ 1.9.6    | ✅ 1.10.5    | ✅ 1.11.3    | ✅ 2.0.2    |       |

v1.1.1 and v1.1.2 are absent deliberately: both were dependency-only patch
releases with no Nomad-facing change, and neither was separately run against
the matrix. v1.2.0 above supersedes them.

Each line is tested at its latest patch at the time of the release. For
v1.2.0 that moved Nomad 2.0.x from 2.0.4 to 2.0.7; the other three lines had
no new patch since v1.1.0.

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

## Flakes and how the suite guards against them

Test ports come from `freePorts`, which probes for a free port and then closes
the listener, so there is an unavoidable TOCTOU window before the agent binds
it. On Linux the suite uses host networking, where losing that race is not a
clean failure: Nomad binds HTTP first, so it can answer `/v1/agent/self` and
then exit on the RPC or serf bind — surfacing much later as a "connection reset
by peer" inside whichever test happens to be running, which looks like a
product bug and is not one.

`startNomadDockerWithConfig` therefore confirms the agent is still running and
still answering a second after it first responded, and retries startup up to
three times with fresh ports. Back-to-back matrix runs are the likeliest
trigger, because containers from the previous version are still tearing down
while the next one starts.

If you do see this, the error now names it ("Nomad became ready then stopped")
and includes the container logs.
