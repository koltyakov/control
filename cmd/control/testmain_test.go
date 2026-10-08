package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/koltyakov/control/internal/buildinfo"
)

func TestMain(m *testing.M) {
	if os.Getenv("CONTROL_TEST_CLI_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func TestCLIFixtureMain(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROL_TEST_CLI_MAIN", "1")
	output, err := exec.CommandContext(t.Context(), executable, "version", "--json").CombinedOutput()
	if err != nil {
		t.Fatalf("fixture CLI: %v\n%s", err, output)
	}
	var version struct{ Version string }
	if err := json.Unmarshal(output, &version); err != nil || version.Version != buildinfo.Version {
		t.Fatalf("fixture did not run the CLI entry point: %s (%v)", output, err)
	}
	if output, err := exec.CommandContext(t.Context(), executable, "--invalid-fixture-flag").CombinedOutput(); err == nil {
		t.Fatalf("fixture CLI accepted an invalid flag: %s", output)
	}
}
