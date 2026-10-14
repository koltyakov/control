package node

import (
	"context"
	"errors"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func TestPendingLeaseAdmissionIsCancellableAndAtomic(t *testing.T) {
	n := localNode(t, nil)
	n.mu.Lock()
	n.leasePending, n.leaseSettled = true, make(chan struct{})
	n.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := n.lockLeaseSettled(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("pending lease wait ignored cancellation", err)
	}
	if !n.updateBusy() || n.pauseForUpdate() {
		t.Fatal("maintenance ignored pending lease persistence")
	}
	n.mu.Lock()
	n.lease = &model.Lease{ID: "published"}
	n.leasePending = false
	close(n.leaseSettled)
	n.mu.Unlock()
	if err := n.lockLeaseSettled(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n.mu.TryLock() {
		n.mu.Unlock()
		t.Fatal("lease admission returned without owning the mutex")
	}
	if n.lease.ID != "published" {
		n.mu.Unlock()
		t.Fatal("lease admission observed incomplete publication")
	}
	n.mu.Unlock()
}
