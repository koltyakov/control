//go:build !windows

package runbooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRunbookStoreBehindUnsearchableParentIsRestricted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	parent := filepath.Join(t.TempDir(), "system-state")
	store := filepath.Join(parent, "runbooks")
	writeBook(t, store, "shared/task.md", "# Hidden\n")
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	catalog, err := Discover(context.Background(), []Root{{Source: "worker-common", Path: store}}, Selector{}, Query{})
	if err != nil {
		t.Fatal(err)
	}
	source := catalog.Sources[0]
	if !source.Restricted || source.Available || source.Error != "" || catalog.Guidance != EmptyGuidance {
		t.Fatalf("unsearchable store was not reported as restricted: %+v", catalog)
	}
	// An exact read still fails instead of pretending the document is absent.
	if _, err = Discover(context.Background(), []Root{{Source: "worker-common", Path: store}}, Selector{}, Query{Source: "worker-common", File: "shared/task.md"}); err == nil {
		t.Fatal("exact read of a restricted store succeeded")
	}
}
