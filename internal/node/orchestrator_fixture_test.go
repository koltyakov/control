package node

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

const testAccount = "node-test-account-key-1234567890123456"

type orchestratorFixture struct {
	client client.Client
	config client.StandaloneConfig
	owner  *identity.Identity
	peer   *transport.Peer
	ctx    context.Context
}

var testOrchestrators sync.Map

func prepareTestOrchestrator(t *testing.T, ctx context.Context, gateway, token, user string, relay bool) *orchestratorFixture {
	t.Helper()
	cfg := client.StandaloneConfig{Gateway: client.Admin{URL: gateway, Key: token}, StateDir: t.TempDir(), RelayOnly: relay, AccountRouting: true}
	owner, err := identity.Load(filepath.Join(cfg.StateDir, identity.ID([]byte(strings.TrimRight(gateway, "/")+"\x00"+user))))
	if err != nil {
		t.Fatal(err)
	}
	c := (client.Client{}).WithStandalone(ctx, cfg)
	transportID, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	p := transport.New(transport.Config{Client: true, Gateway: gateway, Token: token, Identity: transportID, ClientOwner: owner, Node: model.Node{ID: transportID.ID, Name: "cli-" + owner.ID[:16], PublicKey: transportID.Public, OS: runtime.GOOS}, RelayOnly: relay}, nil)
	if err = p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	f := &orchestratorFixture{client: c, config: cfg, owner: owner, peer: p, ctx: ctx}
	t.Cleanup(func() { _ = f.peer.Close(); _ = f.client.Close() })
	return f
}

func attachTestOrchestrator(t *testing.T, n *Node, f *orchestratorFixture) {
	t.Helper()
	testOrchestrators.Store(n.Identity.ID, f)
	t.Cleanup(func() { testOrchestrators.Delete(n.Identity.ID) })
}

func testOrchestrator(t *testing.T, n *Node) *orchestratorFixture {
	t.Helper()
	f, ok := testOrchestrators.Load(n.Identity.ID)
	if !ok {
		t.Fatal("missing account client fixture for", n.Config.Name)
	}
	return f.(*orchestratorFixture)
}

func testCall(t *testing.T, n *Node, ctx context.Context, target, method string, params, result any) error {
	t.Helper()
	if target == "" && method != "nodes.list" && method != "nodes.select" && method != "activities.pool" {
		target = n.Config.Name
	}
	return testOrchestrator(t, n).client.Call(ctx, target, method, params, result)
}

func testWaitTask(t *testing.T, n *Node, ctx context.Context, target, id string) (model.Task, error) {
	t.Helper()
	if target == "" {
		target = n.Config.Name
	}
	return testOrchestrator(t, n).client.Wait(ctx, target, id)
}

func testOpen(t *testing.T, n *Node, ctx context.Context, target, method string, params any) (net.Conn, json.RawMessage, error) {
	t.Helper()
	return testOrchestrator(t, n).peer.OpenRPC(ctx, target, method, params)
}

func testConnections(t *testing.T, n *Node) map[string]string {
	t.Helper()
	f := testOrchestrator(t, n)
	info, err := f.client.Session(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for id, mode := range f.peer.Connections() {
		info.Connections[id] = mode
	}
	return info.Connections
}
