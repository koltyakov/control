package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/clipboard"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestClipboardClientFilePaste(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			const token = "clipboard-client-test-token-12345"
			g, err := gateway.New(t.TempDir(), token)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			gatewayServer := httptest.NewServer(g.Handler())
			defer gatewayServer.Close()
			worker, err := node.New(node.Config{Name: "worker", Gateway: gatewayServer.URL, Token: token, WorkDir: t.TempDir(), DataDir: t.TempDir(), RelayOnly: relay})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = worker.Close() }()
			if err := worker.Start(ctx); err != nil {
				t.Fatal(err)
			}
			local, err := node.New(node.Config{Name: "local", Gateway: gatewayServer.URL, Token: token, WorkDir: t.TempDir(), DataDir: t.TempDir(), RelayOnly: relay})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = local.Close() }()
			if err := local.Start(ctx); err != nil {
				t.Fatal(err)
			}
			api, relayAPI := httptest.NewServer(worker.Handler()), httptest.NewServer(local.Handler())
			defer api.Close()
			defer relayAPI.Close()
			for i, apiURL := range []string{api.URL, relayAPI.URL, ""} {
				c := (Client{URL: apiURL, Token: token}).WithLifetime(ctx)
				if apiURL == "" {
					c = (Client{URL: refusedAPI(t)}).WithStandalone(ctx, StandaloneConfig{Gateway: Admin{URL: gatewayServer.URL, Key: token}, StateDir: t.TempDir(), RelayOnly: relay})
				}
				defer func() { _ = c.Close() }()
				path := filepath.Join(t.TempDir(), fmt.Sprintf("source-%d.bin", i))
				data := bytes.Repeat([]byte("binary\x00\xff"), 200000)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				batch, err := clipboard.Snapshot(clipboard.Value{Paths: []string{path}})
				if err != nil {
					t.Fatal(err)
				}
				defer batch.Close()
				result, err := c.sendClipboard(ctx, ClipboardSpec{Node: "worker"}, batch)
				if err != nil || result.Kind != "files" || result.Bytes != int64(len(data)) {
					t.Fatal("paste", result, err)
				}
				got, err := os.ReadFile(filepath.Join(worker.Config.WorkDir, filepath.Base(path)))
				if err != nil || !bytes.Equal(got, data) {
					t.Fatal("streamed data differed", err)
				}
			}
		})
	}
}

func TestClipboardMCPReversePaste(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote.bin")
	data := bytes.Repeat([]byte("remote clipboard"), 10000)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	// A fake OS clipboard stream keeps tests independent of desktop sessions and never touches the user's clipboard.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		conn := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
		var q clipboard.Request
		if wire.ReadFrame(conn, &q) != nil || q.Protocol != clipboard.Protocol {
			return
		}
		batch, err := clipboard.Snapshot(clipboard.Value{Paths: []string{path}})
		if err != nil {
			return
		}
		defer batch.Close()
		if wire.WriteFrame(conn, model.Response{Result: model.JSON(clipboard.Ack{Protocol: clipboard.Protocol, Content: batch.Content})}) != nil {
			return
		}
		var ready clipboard.Ready
		if wire.ReadFrame(conn, &ready) != nil || !ready.Ready {
			return
		}
		if batch.Send(r.Context(), conn) != nil {
			return
		}
		_ = wire.WriteFrame(conn, model.Response{Result: model.JSON(batch.Content.Result())})
	}))
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := (Client{URL: api.URL}).WithLifetime(ctx)
	defer func() { _ = c.Close() }()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := c.MCPServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	dir := t.TempDir()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_clipboard_paste", Arguments: map[string]any{"node": "worker", "reverse": true, "dir": dir}})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	var metadata clipboard.Result
	if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &metadata); err != nil || metadata.Bytes != int64(len(data)) {
		t.Fatal(metadata, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "remote.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("reverse MCP paste differs", err)
	}
	// A repeated explicit paste must not overwrite the first one.
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "control_clipboard_paste", Arguments: map[string]any{"node": "worker", "reverse": true, "dir": dir}})
	if err != nil || !result.IsError {
		t.Fatal("existing destination accepted", result, err)
	}
}
