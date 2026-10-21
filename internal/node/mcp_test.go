package node

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPSetupDoesNotBlockOtherServersOrWaiterCancellation(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer slow.Close()
	defer close(release)
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	fast := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer fast.Close()
	n := localNode(t, func(c *Config) { c.MCP = map[string]Command{"slow": {URL: slow.URL}, "fast": {URL: fast.URL}} })
	defer func() { _ = n.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := n.mcpSession(ctx, "slow"); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("setup did not start")
	}
	quick, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	if _, err := n.mcpSession(quick, "fast"); err != nil {
		t.Fatal("unrelated setup blocked", err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled waiter succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter ignored cancellation")
	}
	n.mcpMu.Lock()
	pending := len(n.mcpPending)
	n.mcpMu.Unlock()
	if pending != 1 {
		t.Fatal("caller cancelled shared setup", pending)
	}
}
