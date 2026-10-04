package client

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLocalAPIAndMCP(t *testing.T) {
	const token = "client-test-token-12345678"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	gatewayServer := httptest.NewServer(g.Handler())
	t.Cleanup(gatewayServer.Close)
	t.Cleanup(func() { _ = g.Close() })
	n, err := node.New(node.Config{Name: "local", Gateway: gatewayServer.URL, Token: token, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err = n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(n.Handler())
	defer api.Close()
	c := Client{URL: api.URL, Token: token}
	var nodes []model.Node
	if err = c.Call(ctx, "", "nodes.list", map[string]any{}, &nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "local" {
		t.Fatal(nodes)
	}
	bad := Client{URL: api.URL, Token: "wrong"}
	if err = bad.Call(ctx, "", "nodes.list", map[string]any{}, nil); err == nil {
		t.Fatal("API accepted wrong token")
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := c.MCPServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) < 10 {
		t.Fatal("missing routing tools")
	}
	for _, name := range []string{"control_nodes", "control_describe", "control_call"} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{"node": "local", "method": "files.list", "params": map[string]any{}}})
		if err != nil || result.IsError {
			t.Fatalf("%s: %v %+v", name, err, result)
		}
	}
}
