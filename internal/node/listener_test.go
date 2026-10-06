package node

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/transport"
)

func TestReverseListenersBoundAdmissionAndCleanup(t *testing.T) {
	source, worker, consumer := cluster(t, true, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if conn, _, err := testOpen(t, source, ctx, "worker", "tcp.listen", map[string]string{"protocol": "unknown"}); err == nil {
		_ = conn.Close()
		t.Fatal("unknown reverse protocol accepted")
	}
	listeners := make([]net.Listener, 0, 32)
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for range 32 {
		conn, address, err := testOrchestrator(t, source).peer.OpenTCPListener(ctx, "worker", "")
		if err != nil {
			t.Fatal(err)
		}
		listener, err := transport.NewTCPListener(ctx, conn, address)
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
	}
	if worker.pauseForUpdate() {
		t.Fatal("idle reverse listeners did not block maintenance")
	}
	if conn, _, err := testOrchestrator(t, consumer).peer.OpenTCPListener(ctx, "worker", ""); err == nil {
		_ = conn.Close()
		t.Fatal("receiver listener limit did not span callers")
	}
	for _, listener := range listeners {
		_ = listener.Close()
	}
	eventually(t, ctx, func() bool { return worker.pauseForUpdate() })
	defer worker.work.Resume()
	if source.updateBusy() {
		t.Fatal("sender admission retained after close")
	}
	blocked, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	conn, _, err := testOrchestrator(t, consumer).peer.OpenTCPListener(blocked, "worker", "")
	stop()
	if err == nil {
		_ = conn.Close()
		t.Fatal("reverse listener bypassed maintenance reservation")
	}
}
