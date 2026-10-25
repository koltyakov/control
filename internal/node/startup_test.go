package node

import (
	"context"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestStartupReservation(t *testing.T) {
	n, err := New(Config{Name: "startup", Gateway: "http://127.0.0.1:1", Token: "startup-token", DataDir: t.TempDir(), WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	ctx := t.Context()
	release, err := n.work.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n.reserveStartupChange() == nil {
		t.Fatal("reserved startup during work")
	}
	release()
	if !n.pauseForUpdate() || n.reserveStartupChange() == nil {
		t.Fatal("startup stole an update reservation")
	}
	n.work.Resume()
	n.lease = &model.Lease{ID: "lease", Expires: time.Now().Add(time.Minute)}
	if n.reserveStartupChange() == nil {
		t.Fatal("startup ignored a lease")
	}
	n.lease = nil
	n.work.SetDisabled(true)
	if n.reserveStartupChange() == nil {
		t.Fatal("startup ignored disabled admission")
	}
	n.work.SetDisabled(false)
	if err := n.reserveStartupChange(); err != nil {
		t.Fatal(err)
	}
	if n.pauseForUpdate() || !n.updateBusy() {
		t.Fatal("updates stole an admitted startup change")
	}
	blocked, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := n.work.Enter(blocked); err == nil {
		t.Fatal("new work entered during startup migration")
	}
}
