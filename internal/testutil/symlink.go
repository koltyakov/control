// Package testutil provides shared helpers for isolated test fixtures.
// Production code must not depend on this package.
package testutil

import (
	"os"
	"strconv"
	"testing"
)

// RequireSymlinksEnv declares that the test runner has been provisioned for
// symbolic-link creation. It is not inferred from CI or administrator status.
const RequireSymlinksEnv = "CONTROL_TEST_REQUIRE_SYMLINKS"

type symlinkReporter interface {
	Helper()
	Skipf(string, ...any)
	Fatalf(string, ...any)
}

// Symlink creates a fixture link or ends the current test. Unprovisioned
// environments skip only recognized missing-capability errors; unexpected
// errors always fail. A true RequireSymlinksEnv makes every creation error fail.
// No privileges, Developer Mode or filesystem settings are changed.
func Symlink(t testing.TB, target, link string) {
	t.Helper()
	symlink(t, target, link, os.Symlink)
}

func symlink(t symlinkReporter, target, link string, create func(string, string) error) {
	t.Helper()
	required := false
	if value := os.Getenv(RequireSymlinksEnv); value != "" {
		var err error
		required, err = strconv.ParseBool(value)
		if err != nil {
			t.Fatalf("invalid %s=%q: expected a boolean: %v", RequireSymlinksEnv, value, err)
			return
		}
	}
	if err := create(target, link); err != nil {
		if !required && symlinkUnavailable(err) {
			t.Skipf("symlink creation unavailable: %v; provision this runner and set %s=1 to require coverage", err, RequireSymlinksEnv)
			return
		}
		if required {
			t.Fatalf("symlink creation required by %s: %v", RequireSymlinksEnv, err)
			return
		}
		t.Fatalf("unexpected symlink creation failure: %v", err)
	}
}
