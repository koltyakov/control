package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (n *Node) mcpSession(ctx context.Context, name string) (*mcp.ClientSession, error) {
	n.mcpMu.Lock()
	defer n.mcpMu.Unlock()
	if session := n.mcpSessions[name]; session != nil {
		return session, nil
	}
	cfg, ok := n.Config.MCP[name]
	if !ok {
		return nil, fmt.Errorf("MCP server %q is not configured", name)
	}
	var transport mcp.Transport
	if cfg.URL != "" {
		transport = &mcp.StreamableClientTransport{Endpoint: cfg.URL}
	} else {
		if cfg.Command == "" {
			return nil, fmt.Errorf("MCP server %q requires a command or URL", name)
		}
		cmd := command(n.ctx, cfg, n.Config.WorkDir)
		cmd.Stderr = os.Stderr
		transport = &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "control", Version: "0.1.0"}, nil)
	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	session, err := client.Connect(initCtx, transport, nil)
	if err != nil {
		return nil, err
	}
	n.mcpSessions[name] = session
	go func() {
		_ = session.Wait()
		n.mcpMu.Lock()
		if n.mcpSessions[name] == session {
			delete(n.mcpSessions, name)
		}
		n.mcpMu.Unlock()
	}()
	return session, nil
}

func (n *Node) discoverMCP(ctx context.Context, args json.RawMessage) (any, error) {
	var q struct {
		Server string `json:"server"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if q.Server == "" {
		servers := map[string]string{}
		for name, cfg := range n.Config.MCP {
			servers[name] = cfg.Description
		}
		return servers, nil
	}
	session, err := n.mcpSession(ctx, q.Server)
	if err != nil {
		return nil, err
	}
	tools := []*mcp.Tool{}
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return map[string]any{"server": q.Server, "tools": tools, "capabilities": session.InitializeResult().Capabilities}, nil
}

func (n *Node) mcpCall(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var q struct {
		Server    string         `json:"server"`
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	session, err := n.mcpSession(ctx, q.Server)
	if err != nil {
		return nil, err
	}
	return session.CallTool(ctx, &mcp.CallToolParams{Name: q.Tool, Arguments: q.Arguments})
}

func (n *Node) mcpRequest(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var q struct {
		Server string          `json:"server"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if len(q.Params) == 0 {
		q.Params = json.RawMessage(`{}`)
	}
	session, err := n.mcpSession(ctx, q.Server)
	if err != nil {
		return nil, err
	}
	switch q.Method {
	case "resources/list":
		var p mcp.ListResourcesParams
		if err = json.Unmarshal(q.Params, &p); err != nil {
			return nil, err
		}
		return session.ListResources(ctx, &p)
	case "resources/read":
		var p mcp.ReadResourceParams
		if err = json.Unmarshal(q.Params, &p); err != nil {
			return nil, err
		}
		return session.ReadResource(ctx, &p)
	case "resources/templates/list":
		var p mcp.ListResourceTemplatesParams
		if err = json.Unmarshal(q.Params, &p); err != nil {
			return nil, err
		}
		return session.ListResourceTemplates(ctx, &p)
	case "prompts/list":
		var p mcp.ListPromptsParams
		if err = json.Unmarshal(q.Params, &p); err != nil {
			return nil, err
		}
		return session.ListPrompts(ctx, &p)
	case "prompts/get":
		var p mcp.GetPromptParams
		if err = json.Unmarshal(q.Params, &p); err != nil {
			return nil, err
		}
		return session.GetPrompt(ctx, &p)
	default:
		return nil, fmt.Errorf("unsupported MCP request %q", q.Method)
	}
}
