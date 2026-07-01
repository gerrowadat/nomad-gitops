//go:build regression

// Package regression contains heavy-weight regression tests intended to be run
// before cutting a new release. Tests spin up real Nomad infrastructure (via
// Docker) rather than using mocks, so they catch integration failures that unit
// tests cannot.
//
// Quick start:
//
//	# Start Nomad via Docker (default version) and run all tests
//	go test -tags=regression -timeout 15m -v ./tests/regression/...
//
//	# Test against a specific Nomad version
//	NOMAD_VERSION=1.9.3 go test -tags=regression -timeout 15m -v ./tests/regression/...
//
//	# Use an already-running Nomad cluster (skip Docker startup)
//	NOMAD_ADDR=http://127.0.0.1:4646 go test -tags=regression -timeout 15m -v ./tests/regression/...
//
// Environment variables:
//
//	NOMAD_VERSION   Nomad version to pull from Docker Hub (default: 1.9.3)
//	NOMAD_ADDR      Use an existing cluster; skips Docker startup entirely
//	NOMAD_TOKEN     ACL token for the cluster (if needed)
package regression

import "time"

// defaultNomadVersion is used when NOMAD_VERSION is unset and Docker is available.
const defaultNomadVersion = "1.9.6"

// VersionRecord documents the result of running the regression suite against
// a specific Nomad version. Records are added manually after each release.
type VersionRecord struct {
	// NomadVersion is the Nomad release tested, e.g. "1.9.3".
	NomadVersion string
	// BothererRelease is the nomad-botherer release tag, e.g. "v0.3.0".
	BothererRelease string
	// TestedAt is when the suite was last run against this combination.
	TestedAt time.Time
	// Passed is true when all tests passed.
	Passed bool
	// Notes contains any caveats or known limitations.
	Notes string
}

// TestedVersions is the compatibility matrix, most-recent first.
// Update after each nomad-botherer release:
//
//  1. Run: NOMAD_VERSION=X.Y.Z go test -tags=regression -timeout 15m ./tests/regression/...
//  2. Add a VersionRecord entry below.
//  3. Update docs/nomad-versions.md.
var TestedVersions = []VersionRecord{
	{
		NomadVersion:    "2.0.2",
		BothererRelease: "v0.9.1",
		TestedAt:        time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.11.3",
		BothererRelease: "v0.9.1",
		TestedAt:        time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.10.5",
		BothererRelease: "v0.9.1",
		TestedAt:        time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.9.6",
		BothererRelease: "v0.9.1",
		TestedAt:        time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "2.0.2",
		BothererRelease: "v0.9.0",
		TestedAt:        time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.11.3",
		BothererRelease: "v0.9.0",
		TestedAt:        time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.10.5",
		BothererRelease: "v0.9.0",
		TestedAt:        time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.9.6",
		BothererRelease: "v0.9.0",
		TestedAt:        time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "2.0.2",
		BothererRelease: "v0.8.0",
		TestedAt:        time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.11.3",
		BothererRelease: "v0.8.0",
		TestedAt:        time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.10.5",
		BothererRelease: "v0.8.0",
		TestedAt:        time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.9.6",
		BothererRelease: "v0.8.0",
		TestedAt:        time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "2.0.2",
		BothererRelease: "v0.7.0",
		TestedAt:        time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.11.3",
		BothererRelease: "v0.7.0",
		TestedAt:        time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.10.5",
		BothererRelease: "v0.7.0",
		TestedAt:        time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.9.6",
		BothererRelease: "v0.7.0",
		TestedAt:        time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "2.0.2",
		BothererRelease: "v0.6.0",
		TestedAt:        time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.11.3",
		BothererRelease: "v0.6.0",
		TestedAt:        time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.10.5",
		BothererRelease: "v0.6.0",
		TestedAt:        time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.9.6",
		BothererRelease: "v0.6.0",
		TestedAt:        time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "2.0.2",
		BothererRelease: "v0.4.0",
		TestedAt:        time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.11.3",
		BothererRelease: "v0.4.0",
		TestedAt:        time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.10.5",
		BothererRelease: "v0.4.0",
		TestedAt:        time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.9.6",
		BothererRelease: "v0.4.0",
		TestedAt:        time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "2.0.2",
		BothererRelease: "v0.1.2",
		TestedAt:        time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.11.3",
		BothererRelease: "v0.1.2",
		TestedAt:        time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.10.5",
		BothererRelease: "v0.1.2",
		TestedAt:        time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
	{
		NomadVersion:    "1.9.6",
		BothererRelease: "v0.1.2",
		TestedAt:        time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		Passed:          true,
	},
}
