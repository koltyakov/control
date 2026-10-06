package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/skills"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPAndSkillUseCanonicalTerminology(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := (Client{}).MCPServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	for name, text := range map[string]string{"mcp": session.InitializeResult().Instructions, "skill": string(skills.Control)} {
		for _, term := range []string{"orchestrator", "worker", "gateway", "client", "node", "AI software"} {
			if !strings.Contains(text, term) {
				t.Errorf("%s instructions omit %q", name, term)
			}
		}
		for _, obsolete := range []string{"fleet agent", "host agent", "Lifecycle-capable agents"} {
			if strings.Contains(text, obsolete) {
				t.Errorf("%s instructions use obsolete terminology %q", name, obsolete)
			}
		}
	}
}
