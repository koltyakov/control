package node

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestConnectionTestAdmissionAndPermissions(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, consumer := cluster(t, relay, func(i int, cfg *Config) {
				if i == 1 {
					cfg.Allow = map[string][]string{"source": {model.ConnectionOpenMethod, "node.describe"}}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			q := model.ConnectionOpenRequest{Protocol: model.ConnectionTestProtocol, ConnectionTestOptions: model.ConnectionTestOptions{Bytes: 1024, Samples: 2}}
			for _, invalid := range []model.ConnectionOpenRequest{{Protocol: "unsupported"}, {Protocol: model.ConnectionTestProtocol, ConnectionTestOptions: model.ConnectionTestOptions{Bytes: -1}}, {Protocol: model.ConnectionTestProtocol, ConnectionTestOptions: model.ConnectionTestOptions{Samples: 101}}} {
				if stream, _, err := testOpen(t, source, ctx, "worker", model.ConnectionOpenMethod, invalid); err == nil {
					_ = stream.Close()
					t.Fatal("accepted invalid request", invalid)
				}
			}
			if stream, _, err := testOpen(t, consumer, ctx, "worker", model.ConnectionOpenMethod, q); err == nil {
				_ = stream.Close()
				t.Fatal("ignored connection.open permission")
			}
			stream, _, err := testOpen(t, source, ctx, "worker", model.ConnectionOpenMethod, q)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			if worker.pauseForUpdate() {
				t.Fatal("maintenance admitted during idle test stream")
			}
			_ = stream.Close()
			eventually(t, ctx, func() bool { return worker.pauseForUpdate() })
			blocked, stop := context.WithTimeout(ctx, 100*time.Millisecond)
			if stream, _, err := testOpen(t, source, blocked, "worker", model.ConnectionOpenMethod, q); err == nil {
				_ = stream.Close()
				t.Fatal("test admitted during maintenance")
			}
			stop()
			worker.work.Resume()
			worker.work.SetDisabled(true)
			if stream, _, err := testOpen(t, source, ctx, "worker", model.ConnectionOpenMethod, q); err == nil {
				_ = stream.Close()
				t.Fatal("test admitted on disabled receiver")
			}
			worker.work.SetDisabled(false)
			// A normal JSON test also rejects a disabled initiating worker.
			source.work.SetDisabled(true)
			if err := testCall(t, source, ctx, "source", model.ConnectionTestMethod, model.ConnectionTestRequest{Target: "consumer"}, nil); err == nil {
				t.Fatal("test admitted on disabled source")
			}
			source.work.SetDisabled(false)
		})
	}
}

func TestConnectionTestReceiverCancellation(t *testing.T) {
	_, worker, _ := cluster(t, true, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	done := make(chan struct{})
	q := model.ConnectionOpenRequest{Protocol: model.ConnectionTestProtocol, ConnectionTestOptions: model.ConnectionTestOptions{Bytes: 1024, Samples: 1}}
	go func() { worker.serveConnectionTest(ctx, worker.Identity.ID, b, model.JSON(q)); close(done) }()
	_ = a.SetDeadline(time.Now().Add(3 * time.Second))
	var ack model.Response
	if err := readFrame(a, &ack); err != nil || ack.Error != "" {
		t.Fatal("initial acknowledgement", ack, err)
	}
	if worker.pauseForUpdate() {
		t.Fatal("receiver did not hold maintenance admission")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("receiver ignored cancellation")
	}
	if active, _ := worker.work.State(); active != 0 || len(worker.connectionTests) != 0 {
		t.Fatal("receiver leaked admission or diagnostic slot", active)
	}
}
