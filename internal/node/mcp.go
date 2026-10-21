package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpSetup struct {
	done    chan struct{}
	session *mcp.ClientSession
	err     error
}

func (n *Node) mcpSession(ctx context.Context, name string) (*mcp.ClientSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n.mcpMu.Lock()
	if session := n.mcpSessions[name]; session != nil {
		n.mcpMu.Unlock()
		return session, nil
	}
	cfg, ok := n.Config.MCP[name]
	if !ok {
		n.mcpMu.Unlock()
		return nil, fmt.Errorf("MCP server %q is not configured", name)
	}
	if n.mcpPending == nil {
		n.mcpPending = make(map[string]*mcpSetup)
	}
	setup := n.mcpPending[name]
	if setup == nil {
		n.mu.Lock()
		if n.closed || n.ctx == nil || n.ctx.Err() != nil {
			n.mu.Unlock()
			n.mcpMu.Unlock()
			return nil, errors.New("node is closed")
		}
		n.wg.Add(1)
		n.mu.Unlock()
		setup = &mcpSetup{done: make(chan struct{})}
		n.mcpPending[name] = setup
		go n.connectMCP(name, cfg, setup)
	}
	n.mcpMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.ctx.Done():
		return nil, n.ctx.Err()
	case <-setup.done:
		return setup.session, setup.err
	}
}

func (n *Node) connectMCP(name string, cfg Command, setup *mcpSetup) {
	defer n.wg.Done()
	session, err := n.openMCP(cfg)
	n.mcpMu.Lock()
	setup.session, setup.err = session, err
	if err == nil {
		n.mcpSessions[name] = session
	}
	delete(n.mcpPending, name)
	close(setup.done)
	n.mcpMu.Unlock()
	if err != nil {
		return
	}
	stop := context.AfterFunc(n.ctx, func() { _ = session.Close() })
	defer stop()
	_ = session.Wait()
	n.mcpMu.Lock()
	if n.mcpSessions[name] == session {
		delete(n.mcpSessions, name)
	}
	n.mcpMu.Unlock()
}

func (n *Node) openMCP(cfg Command) (*mcp.ClientSession, error) {
	var transport mcp.Transport
	if cfg.URL != "" {
		transport = &mcp.StreamableClientTransport{Endpoint: cfg.URL}
	} else {
		if cfg.Command == "" {
			return nil, errors.New("MCP server requires a command or URL")
		}
		cmd := command(n.ctx, cfg, n.Config.WorkDir)
		cmd.Stderr = os.Stderr
		transport = &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "control", Version: buildinfo.Version}, nil)
	initCtx, cancel := context.WithTimeout(n.ctx, 15*time.Second)
	defer cancel()
	return client.Connect(initCtx, transport, nil)
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
