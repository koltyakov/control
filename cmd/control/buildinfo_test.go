package main

import (
	"os"
	"testing"

	"github.com/koltyakov/control/internal/buildinfo"
)

func TestReleaseRepositoryOverride(t *testing.T) {
	t.Setenv("CONTROL_RELEASE_REPO", "other/fork")
	if got := releaseRepository(); got != "other/fork" {
		t.Fatalf("environment override ignored: %q", got)
	}
	t.Setenv("CONTROL_RELEASE_REPO", "")
	if got := releaseRepository(); got != "" {
		t.Fatalf("explicit empty override ignored: %q", got)
	}
	if err := os.Unsetenv("CONTROL_RELEASE_REPO"); err != nil {
		t.Fatal(err)
	}
	if got := releaseRepository(); got != buildinfo.ReleaseRepo {
		t.Fatalf("embedded default ignored: %q", got)
	}
}
