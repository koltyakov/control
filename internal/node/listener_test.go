package node

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

func TestReverseListenerActivitySurvivesSocketClose(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, _ := cluster(t, relay, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			conn, address, err := testOrchestrator(t, source).peer.OpenTCPListener(ctx, "worker", "")
			if err != nil {
				t.Fatal(err)
			}
			listener, err := transport.NewTCPListener(ctx, conn, address)
			if err != nil {
				_ = conn.Close()
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			initial := worker.activitySnapshot(model.ActivityQuery{})
			if len(initial.Active) != 1 || initial.Active[0].Operation != "tcp.listen" {
				t.Fatalf("idle listener not tracked: %+v", initial)
			}
			if initial.Tunnels == nil || *initial.Tunnels != (model.TunnelCounts{Reverse: 1}) {
				t.Fatalf("idle listener not counted: %+v", initial.Tunnels)
			}
			id := initial.Active[0].ID
			for i := range 2 {
				socket, err := net.DialTimeout("tcp", address, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = socket.Close() })
				accepted, err := listener.Accept()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = accepted.Close() })
				_ = socket.SetDeadline(time.Now().Add(5 * time.Second))
				_ = accepted.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := socket.Write([]byte("ping")); err != nil {
					t.Fatal(err)
				}
				var data [4]byte
				if _, err := io.ReadFull(accepted, data[:]); err != nil || string(data[:]) != "ping" {
					t.Fatal("listener receive", data, err)
				}
				if _, err := accepted.Write([]byte("pong")); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(socket, data[:]); err != nil || string(data[:]) != "pong" {
					t.Fatal("listener send", data, err)
				}
				_ = accepted.Close()
				_ = socket.Close()
				snapshot := worker.activitySnapshot(model.ActivityQuery{Recent: 5})
				if len(snapshot.Active) != 1 || snapshot.Active[0].ID != id || snapshot.Active[0].Phase != "listening" || len(snapshot.Recent) != 0 {
					t.Fatalf("socket close finished listener activity: %+v", snapshot)
				}
				if snapshot.Tunnels == nil || *snapshot.Tunnels != (model.TunnelCounts{Reverse: 1}) {
					t.Fatalf("socket close changed listener count: %+v", snapshot.Tunnels)
				}
				activity := snapshot.Active[0]
				if want := int64((i + 1) * 4); activity.BytesSent != want || activity.BytesReceived != want {
					t.Fatalf("listener traffic not accumulated: %+v", activity)
				}
				if health := worker.healthSnapshot(); health.ActiveCount != 1 {
					t.Fatalf("idle listener missing from health: %+v", health)
				}
			}
			_ = listener.Close()
			eventually(t, ctx, func() bool { return worker.healthSnapshot().ActiveCount == 0 })
			finished := worker.activitySnapshot(model.ActivityQuery{Recent: 5})
			if len(finished.Recent) != 1 || finished.Recent[0].ID != id || finished.Recent[0].BytesSent != 8 || finished.Recent[0].BytesReceived != 8 {
				t.Fatalf("listener completion lost final traffic: %+v", finished)
			}
			if finished.Tunnels == nil || *finished.Tunnels != (model.TunnelCounts{}) {
				t.Fatalf("closed listener still counted: %+v", finished.Tunnels)
			}
		})
	}
}

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
