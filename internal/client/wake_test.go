package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestWakeSender(t *testing.T) {
	target := model.Node{ID: "sleeping", Labels: map[string]string{"lan": "home"}, WakeInterfaces: []model.WakeInterface{{Address: "192.168.1.3/24", MAC: "02:00:00:00:00:03"}}}
	source := model.Node{ID: "sender-id", Name: "sender", Online: true, Labels: target.Labels, WakeInterfaces: []model.WakeInterface{{Address: "192.168.1.2/24", MAC: "02:00:00:00:00:02"}}, Capabilities: []model.Capability{{Name: "network.wake"}}}
	for _, from := range []string{"", "sender", "sender-id"} {
		got, err := wakeSender([]model.Node{target, source}, target, from)
		if err != nil || got.ID != source.ID {
			t.Fatal(got, err)
		}
	}
	for _, change := range []func(*model.Node){
		func(n *model.Node) { n.Online = false },
		func(n *model.Node) { n.Disabled = true },
		func(n *model.Node) { n.ControlPending = true },
		func(n *model.Node) { n.Labels = map[string]string{"lan": "office"} },
		func(n *model.Node) { n.Capabilities = nil },
		func(n *model.Node) { n.WakeInterfaces = nil },
	} {
		n := source
		change(&n)
		if _, err := wakeSender([]model.Node{n}, target, ""); err == nil {
			t.Fatal("selected unavailable or incompatible sender", n)
		}
	}
	if _, err := wakeSender([]model.Node{source}, target, "missing"); err == nil {
		t.Fatal("substituted a different sender")
	}
}

func TestWakeRequiresAccountRoutingAndValidSpec(t *testing.T) {
	for _, spec := range []WakeSpec{{}, {Node: "worker"}, {Node: "worker", From: "local"}, {Node: "worker", From: "sender", LAN: "home"}, {Node: "worker", From: "local", LAN: "home"}} {
		if _, err := (Client{}).Wake(context.Background(), spec); err == nil {
			t.Fatal("accepted invalid specification or local API", spec)
		}
	}
}

func TestWakeMCPContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	server, err := (Client{}).MCPServer().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		found = found || tool.Name == "control_wake"
	}
	if !found {
		t.Fatal("wake tool not advertised")
	}
	for _, args := range []map[string]any{{}, {"node": "target", "from": "local"}, {"node": "target", "lan": "home"}} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_wake", Arguments: args})
		if err == nil && !result.IsError {
			t.Fatal("accepted invalid wake arguments", args)
		}
	}
}

func TestWakeDirectoryAndAccountChecks(t *testing.T) {
	t.Parallel()
	ctx, c, workers, admin := delegationCluster(t, true)
	for _, q := range []struct {
		spec WakeSpec
		want string
	}{
		{WakeSpec{Node: "missing"}, "unknown wake target"},
		{WakeSpec{Node: "third"}, "already online"},
	} {
		if _, err := c.Wake(ctx, q.spec); err == nil || !strings.Contains(err.Error(), q.want) {
			t.Fatal(q.spec, err)
		}
	}
	common := (Client{}).WithStandalone(ctx, StandaloneConfig{Gateway: Admin{URL: admin.URL, Key: workers[0].Config.Token}, StateDir: t.TempDir(), RelayOnly: true, AccountRouting: true})
	defer func() { _ = common.Close() }()
	if _, err := common.Wake(ctx, WakeSpec{Node: "third", From: "local", LAN: "home"}); err == nil || !strings.Contains(err.Error(), "account key") {
		t.Fatal("common key reached local sender", err)
	}
	if err := workers[2].Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var nodes []model.Node
		if err := c.Call(ctx, "", "nodes.list", map[string]any{}, &nodes); err != nil {
			t.Fatal(err)
		}
		offline := false
		for _, n := range nodes {
			if n.ID == workers[2].Identity.ID {
				offline = !n.Online
			}
		}
		if offline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("target remained online")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := c.Wake(ctx, WakeSpec{Node: "third"}); err == nil || !strings.Contains(err.Error(), "no online wake sender") {
		t.Fatal("selected sender without LAN metadata", err)
	}
	if _, err := c.Wake(ctx, WakeSpec{Node: "third", From: "local", LAN: "home"}); err == nil || !strings.Contains(err.Error(), "matching lan label") {
		t.Fatal("local sender ignored target LAN", err)
	}
}
