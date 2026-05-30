//go:build regression

package regression

import (
	"fmt"
	"os"
	"testing"

	nomadapi "github.com/hashicorp/nomad/api"
)

// testNomadAddr is the HTTP address of the Nomad cluster under test.
var testNomadAddr string

// testNomadClient is an API client connected to testNomadAddr.
// Use testNomadClient.Jobs() for test setup (register, deregister).
var testNomadClient *nomadapi.Client

// testNomadVersion records the Nomad version under test (informational).
var testNomadVersion string

// testBinaryPath is the path to the compiled nomad-botherer binary.
// It is empty when the build failed; E2E tests skip themselves in that case.
var testBinaryPath string

func TestMain(m *testing.M) {
	var cleanup func()
	var err error

	switch {
	case os.Getenv("NOMAD_ADDR") != "":
		testNomadAddr = os.Getenv("NOMAD_ADDR")
		testNomadVersion = os.Getenv("NOMAD_VERSION") // informational only
		cleanup = func() {}

	default:
		ver := os.Getenv("NOMAD_VERSION")
		if ver == "" {
			ver = defaultNomadVersion
		}
		testNomadVersion = ver
		testNomadAddr, cleanup, err = startNomadDocker(ver)
		if err != nil {
			fmt.Fprintf(os.Stderr, "regression: cannot start Nomad %s via Docker: %v\n", ver, err)
			fmt.Fprintln(os.Stderr, "  Tip: set NOMAD_ADDR to point at an existing cluster.")
			fmt.Fprintln(os.Stderr, "  Tip: set NOMAD_VERSION to pull a different image.")
			os.Exit(1)
		}
	}
	defer cleanup()

	cfg := nomadapi.DefaultConfig()
	cfg.Address = testNomadAddr
	if tok := os.Getenv("NOMAD_TOKEN"); tok != "" {
		cfg.SecretID = tok
	}
	testNomadClient, err = nomadapi.NewClient(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "regression: nomad client: %v\n", err)
		os.Exit(1)
	}

	testBinaryPath, err = buildBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "regression: build failed (E2E tests will be skipped): %v\n", err)
		testBinaryPath = ""
	}

	fmt.Printf("regression: Nomad %s at %s\n", testNomadVersion, testNomadAddr)
	os.Exit(m.Run())
}
