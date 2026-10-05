package main

import (
	"strings"
	"testing"
)

func TestLocalUpdateNeedsNoGatewayOrProfile(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	t.Setenv("CONTROL_RELEASE_REPO", "")
	for _, command := range []string{"update", "upgrade"} {
		for _, help := range []string{"--help", "help"} {
			if err := run(t.Context(), []string{"--config", t.TempDir(), command, help}); err != nil {
				t.Fatalf("%s %s: %v", command, help, err)
			}
		}
		// A missing release repository must be reported without gateway auth.
		err := run(t.Context(), []string{"--config", t.TempDir(), command})
		if err == nil || !strings.Contains(err.Error(), "release repository") {
			t.Fatalf("%s: %v", command, err)
		}
		if err := run(t.Context(), []string{command, "unexpected"}); err == nil {
			t.Fatal("accepted unexpected positional argument")
		}
	}
	if !strings.Contains(usage, "update [--version TAG]") {
		t.Fatal("ordinary help omits local update")
	}
}
