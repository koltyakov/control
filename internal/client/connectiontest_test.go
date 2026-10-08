package client

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConnectionTests(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			// This fixture runs several bidirectional bulk tests, not just RPCs.
			// Leave room for hashing/encryption under the race detector on CI hosts.
			ctx, c, workers, _ := delegationClusterWithTimeout(t, relay, 90*time.Second)
			mode := "webrtc"
			if relay {
				mode = "relay"
			}
			q := model.ConnectionTestOptions{Bytes: 9 << 20, Samples: 3}
			for _, from := range []string{"", "first"} {
				result, err := c.TestConnection(ctx, ConnectionTestSpec{Node: "second", From: from, ConnectionTestOptions: q})
				if err != nil {
					t.Fatalf("connection test from %q: %v (fixture context: %v)", from, err, ctx.Err())
				}
				if result.Transport != mode || result.TargetID != workers[1].Identity.ID || result.Upload.Bytes != q.Bytes || result.Download.Bytes != q.Bytes || result.Latency.Samples != q.Samples || result.Upload.Mbps <= 0 || result.Download.Mbps <= 0 {
					t.Fatalf("invalid result: %+v", result)
				}
				if from == "first" && result.SourceID != workers[0].Identity.ID {
					t.Fatal("worker test ran on orchestrator", result)
				}
				if from == "" && result.SourceID != c.routing.peer.IdentityID() {
					t.Fatal("orchestrator test ran on worker", result)
				}
			}
			var grants []model.Delegation
			if err := c.Call(ctx, "second", "access.list", map[string]any{}, &grants); err != nil || len(grants) != 0 {
				t.Fatal("diagnostic grant retained", grants, err)
			}
			for _, w := range workers {
				var artifacts []model.Artifact
				if err := c.Call(ctx, w.Config.Name, "artifacts.list", map[string]any{}, &artifacts); err != nil || len(artifacts) != 0 {
					t.Fatal("test created artifacts", err)
				}
			}
			if _, err := workers[0].Peer.TestConnection(ctx, "second", q); err == nil {
				t.Fatal("worker tested without delegation")
			}
			if _, err := c.TestConnection(ctx, ConnectionTestSpec{Node: "second", From: "second"}); err == nil {
				t.Fatal("tested same worker")
			}
			if err := c.Call(ctx, "second", "access.grant", model.Delegation{Subject: "first", Method: model.ConnectionTestMethod, Params: model.JSON(model.ConnectionTestRequest{Target: "third"})}, nil); err == nil {
				t.Fatal("delegated nested coordination")
			}
			// The MCP adapter uses the same implementation and source selection.
			st, ct := mcp.NewInMemoryTransports()
			server, err := c.MCPServer().Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = server.Close() }()
			session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = session.Close() }()
			tool, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_speedtest", Arguments: map[string]any{"node": "third", "from": "second", "bytes": 65537, "samples": 2}})
			if err != nil || tool.IsError {
				t.Fatal("MCP speedtest", tool, err)
			}
		})
	}
}

func TestConnectionTestStreamAdmissionAndRevocation(t *testing.T) {
	ctx, c, workers, _ := delegationCluster(t, true)
	peer, err := c.backend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q := model.ConnectionOpenRequest{Protocol: model.ConnectionTestProtocol, ConnectionTestOptions: model.ConnectionTestOptions{Bytes: 1024, Samples: 2}}
	// Idle acknowledged streams count as work and fill the diagnostic limit.
	a, _, err := peer.OpenRPC(ctx, "second", model.ConnectionOpenMethod, q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, _, err := peer.OpenRPC(ctx, "second", model.ConnectionOpenMethod, q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if conn, _, err := peer.OpenRPC(ctx, "second", model.ConnectionOpenMethod, q); err == nil {
		_ = conn.Close()
		t.Fatal("exceeded diagnostic limit")
	}
	_ = a.Close()
	_ = b.Close()
	// Wait for stream handlers to release, without running another speed test.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var snapshot model.NodeActivitySnapshot
		if err := c.Call(ctx, "second", "activities.list", map[string]any{}, &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.ActiveCount == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream work was not released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var grant model.Delegation
	if err := c.Call(ctx, "second", "access.grant", model.Delegation{Subject: "first", Method: model.ConnectionOpenMethod, Params: model.JSON(q)}, &grant); err != nil {
		t.Fatal(err)
	}
	forged := grant
	changed := q
	changed.Bytes++
	forged.Params = model.JSON(changed)
	if conn, _, err := workers[0].Peer.OpenRPC(model.WithDelegations(ctx, []model.Delegation{forged}), "second", model.ConnectionOpenMethod, changed); err == nil {
		_ = conn.Close()
		t.Fatal("changed test arguments accepted")
	}
	stream, _, err := workers[0].Peer.OpenRPC(model.WithDelegations(ctx, []model.Delegation{grant}), "second", model.ConnectionOpenMethod, q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if err := c.Call(ctx, "second", "access.revoke", map[string]any{"id": grant.ID}, nil); err != nil {
		t.Fatal(err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(stream, make([]byte, 8)); err == nil {
		t.Fatal("revocation left stream open")
	}
	// Cancellation of the initiating stream terminates the receiver too.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.TestConnection(cancelled, ConnectionTestSpec{Node: "second"}); err == nil {
		t.Fatal("cancelled test succeeded")
	}
	if _, err := (Client{}).TestConnection(ctx, ConnectionTestSpec{Node: "second"}); err == nil {
		t.Fatal("local API falsely measured orchestrator connection")
	}
}
