package client

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLocalAPIAndMCP(t *testing.T) {
	const token = "client-test-token-12345678"
	const account = "client-test-account-key-1234567890"
	g, err := gateway.New(t.TempDir(), token, gateway.Options{SuperuserKey: account})
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
	c := (Client{URL: api.URL, Token: token}).WithStandalone(ctx, StandaloneConfig{Gateway: Admin{URL: gatewayServer.URL, Key: account}, StateDir: t.TempDir(), RelayOnly: true, AccountRouting: true})
	defer func() { _ = c.Close() }()
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
	defer func() { _ = serverSession.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
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
	worker, err := node.New(node.Config{Name: "worker", Gateway: gatewayServer.URL, Token: token, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worker.Close() }()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	verifyEOFForward(t, ctx, c)
	verifyReverseForward(t, ctx, c)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = echo.Close() }()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = conn.Close() }(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	call := func(name string, args any, dest any) {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("%s: %v %+v", name, err, result)
		}
		if dest != nil {
			if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), dest); err != nil {
				t.Fatal(err)
			}
		}
	}
	var info SessionInfo
	call("control_session", map[string]any{}, &info)
	if info.Role != "client" || info.OwnerID == n.Identity.ID {
		t.Fatal(info)
	}
	var f ForwardInfo
	call("control_forward_start", map[string]any{"node": "worker", "address": echo.Addr().String()}, &f)
	conn, err := net.DialTimeout("tcp", f.Listen, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("mcp")); err != nil {
		t.Fatal(err)
	}
	var reply [3]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil || string(reply[:]) != "mcp" {
		t.Fatal(reply, err)
	}
	var forwards []ForwardInfo
	call("control_forward_list", map[string]any{}, &forwards)
	if len(forwards) != 1 || forwards[0].Active != 1 {
		t.Fatal(forwards)
	}
	call("control_forward_stop", map[string]any{"id": f.ID}, nil)
	if _, err := conn.Read(reply[:]); err == nil {
		t.Fatal("MCP stop left socket open")
	}
	call("control_forward_list", map[string]any{}, &forwards)
	if len(forwards) != 0 {
		t.Fatal("stopped forward retained", forwards)
	}
	call("control_forward_start", map[string]any{"node": "worker", "address": echo.Addr().String(), "reverse": true}, &f)
	if !f.Reverse {
		t.Fatal("MCP reverse flag lost", f)
	}
	reverseConn, err := net.DialTimeout("tcp", f.Listen, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reverseConn.Close() }()
	_ = reverseConn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := reverseConn.Write([]byte("mcp")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reverseConn, reply[:]); err != nil || string(reply[:]) != "mcp" {
		t.Fatal("MCP reverse forwarding", reply, err)
	}
	call("control_forward_stop", map[string]any{"id": f.ID}, nil)
	if _, err := reverseConn.Read(reply[:]); err == nil {
		t.Fatal("MCP reverse stop left socket open")
	}
	direct, err := c.Tunnel(ctx, "worker", echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = direct.Close() }()
	_ = direct.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := direct.Write([]byte("mcp")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(direct, reply[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := c.StartForward(ctx, ForwardSpec{Node: "local", Address: "invalid"}); err == nil {
		t.Fatal("invalid destination accepted")
	}
	for range maxForwards {
		if _, err := c.StartForward(ctx, ForwardSpec{Node: "local", Address: echo.Addr().String()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.StartForward(ctx, ForwardSpec{Node: "local", Address: echo.Addr().String()}); err == nil {
		t.Fatal("forward limit not enforced")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := direct.Read(reply[:]); err == nil {
		t.Fatal("client close retained direct local-API tunnel")
	}
	if _, err := c.StartForward(ctx, ForwardSpec{Node: "local", Address: echo.Addr().String()}); err == nil {
		t.Fatal("closed client published listener")
	}
}
