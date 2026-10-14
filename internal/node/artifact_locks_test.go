package node

import (
	"context"
	"errors"
	"testing"
)

func TestArtifactLocksAreScopedCancellableAndReleased(t *testing.T) {
	n := localNode(t, nil)
	unlock, err := n.lockTransfer(t.Context(), "one")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := n.lockTransfer(ctx, "one"); !errors.Is(err, context.Canceled) {
		t.Fatal("artifact lock ignored cancellation", err)
	}
	other, err := n.lockTransfer(t.Context(), "two")
	if err != nil {
		t.Fatal("unrelated artifact blocked", err)
	}
	other()
	unlock()
	n.artifactMu.Lock()
	defer n.artifactMu.Unlock()
	if len(n.transfers) != 0 {
		t.Fatal("artifact lock references leaked", n.transfers)
	}
}
