package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func schema(properties map[string]any, required ...string) json.RawMessage {
	if properties == nil {
		properties = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		s["required"] = required
	}
	return model.JSON(s)
}

func (n *Node) registerProviders() error {
	text := map[string]any{"type": "string"}
	object := map[string]any{"type": "object"}
	builtins := []provider{
		{model.Capability{Name: "exec.run", Description: "Run an executable with arguments, environment, and working directory. Use a task for long-running work.", InputSchema: schema(map[string]any{"command": text, "args": map[string]any{"type": "array", "items": text}, "env": object, "dir": text, "stdin": text}, "command")}, n.execRun},
		{model.Capability{Name: "agent.run", Description: "Run a configured AI CLI with a prompt on stdin; collect its output.", InputSchema: schema(map[string]any{"agent": text, "prompt": text}, "agent", "prompt")}, n.agentRun},
		{model.Capability{Name: "http.request", Description: "Make an HTTP request using this machine's network and DNS.", InputSchema: schema(map[string]any{"url": text, "method": text, "headers": object, "body": text}, "url")}, n.httpRequest},
		{model.Capability{Name: "files.list", Description: "List a directory relative to the configured filesystem root.", InputSchema: schema(map[string]any{"path": text})}, n.filesList},
		{model.Capability{Name: "files.read", Description: "Read up to 1 MiB at an offset; returns base64 bytes.", InputSchema: schema(map[string]any{"path": text, "offset": map[string]any{"type": "integer"}, "limit": map[string]any{"type": "integer"}}, "path")}, n.filesRead},
		{model.Capability{Name: "files.write", Description: "Write base64 bytes at an offset. Set truncate for replacement.", InputSchema: schema(map[string]any{"path": text, "data": text, "offset": map[string]any{"type": "integer"}, "truncate": map[string]any{"type": "boolean"}}, "path", "data")}, n.filesWrite},
		{model.Capability{Name: "mcp.call", Description: "Invoke a tool on a configured local MCP server.", InputSchema: schema(map[string]any{"server": text, "tool": text, "arguments": object}, "server", "tool")}, n.mcpCall},
		{model.Capability{Name: "mcp.request", Description: "Read MCP resources or prompts on a configured server.", InputSchema: schema(map[string]any{"server": text, "method": text, "params": object}, "server", "method")}, n.mcpRequest},
		{model.Capability{Name: "workflow.run", Description: "Execute dependency-ordered tasks across peers and transfer their artifact inputs.", InputSchema: schema(map[string]any{"steps": map[string]any{"type": "array", "items": object}}, "steps")}, n.workflowRun},
	}
	for _, p := range builtins {
		if err := n.Register(p); err != nil {
			return err
		}
	}
	if err := n.registerRPA(); err != nil {
		return err
	}
	for name, cfg := range n.Config.Providers {
		if cfg.Command == "" {
			return fmt.Errorf("provider %s requires a command", name)
		}
		if len(cfg.InputSchema) == 0 {
			cfg.InputSchema = schema(nil)
		}
		p := provider{cap: model.Capability{Name: name, Description: cfg.Description, InputSchema: cfg.InputSchema}, run: func(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
			input := string(model.JSON(map[string]any{"version": model.Version, "arguments": args, "workspace": e.Dir})) + "\n"
			result, err := runCommand(ctx, cfg, e, input)
			if err != nil {
				return result, err
			}
			if result.Truncated {
				return nil, errors.New("provider output exceeds 2 MiB")
			}
			var value any
			if err = json.Unmarshal([]byte(result.Stdout), &value); err != nil {
				return nil, fmt.Errorf("provider must return one JSON value on stdout: %w", err)
			}
			return value, nil
		}}
		if err := n.Register(p); err != nil {
			return err
		}
	}
	return nil
}

type commandResult struct {
	ExitCode  int    `json:"exitCode"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
}

type capture struct {
	mu        sync.Mutex
	b         bytes.Buffer
	truncated bool
}

func (c *capture) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size := len(b)
	remaining := (2 << 20) - c.b.Len()
	if len(b) > remaining {
		b = b[:remaining]
		c.truncated = true
	}
	_, _ = c.b.Write(b)
	return size, nil
}

func command(ctx context.Context, cfg Command, dir string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.Dir = dir
	if cfg.Dir != "" {
		if filepath.IsAbs(cfg.Dir) {
			cmd.Dir = cfg.Dir
		} else {
			cmd.Dir = filepath.Join(dir, cfg.Dir)
		}
	}
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.WaitDelay = 2 * time.Second
	configureProcess(cmd)
	return cmd
}

func runCommand(ctx context.Context, cfg Command, e Execution, input string) (commandResult, error) {
	if cfg.Command == "" {
		return commandResult{}, errors.New("command is required")
	}
	cmd := command(ctx, cfg, e.Dir)
	cmd.Stdin = strings.NewReader(input)
	stdout, stderr := &capture{}, &capture{}
	cmd.Stdout = io.MultiWriter(stdout, e.Log)
	cmd.Stderr = io.MultiWriter(stderr, e.Log)
	err := cmd.Run()
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	result := commandResult{ExitCode: exit, Stdout: stdout.b.String(), Stderr: stderr.b.String(), Truncated: stdout.truncated || stderr.truncated}
	if err != nil {
		return result, fmt.Errorf("execute %s: %w", cfg.Command, err)
	}
	return result, nil
}

func (n *Node) execRun(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var input struct {
		Command
		Stdin string `json:"stdin"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}
	return runCommand(ctx, input.Command, e, input.Stdin)
}

func (n *Node) agentRun(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var input struct {
		Agent  string `json:"agent"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}
	cfg, ok := n.Config.Agents[input.Agent]
	if !ok {
		return nil, fmt.Errorf("agent %q is not configured", input.Agent)
	}
	return runCommand(ctx, cfg, e, input.Prompt)
}

func (n *Node) httpRequest(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var input struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}
	if input.Method == "" {
		input.Method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, input.Method, input.URL, strings.NewReader(input.Body))
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, errors.New("URL must use http or https")
	}
	for k, v := range input.Headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 2<<20 {
		return nil, errors.New("HTTP response exceeds 2 MiB; use a task and artifact for large responses")
	}
	return map[string]any{"status": resp.StatusCode, "headers": resp.Header, "body": base64.StdEncoding.EncodeToString(b), "encoding": "base64"}, nil
}

func (n *Node) filesList(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var q struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if q.Path == "" {
		q.Path = "."
	}
	f, err := n.root.Open(q.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(10001)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 10000 {
		return nil, errors.New("directory exceeds 10000 entries")
	}
	result := []map[string]any{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"name": entry.Name(), "directory": entry.IsDir(), "size": info.Size(), "modified": info.ModTime()})
	}
	return result, nil
}

func (n *Node) filesRead(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var q struct {
		Path   string `json:"path"`
		Offset int64  `json:"offset"`
		Limit  int64  `json:"limit"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if q.Offset < 0 || q.Limit < 0 || q.Limit > 1<<20 {
		return nil, errors.New("invalid offset or limit")
	}
	if q.Limit == 0 {
		q.Limit = 64 * 1024
	}
	f, err := n.root.Open(q.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("path must be a regular file")
	}
	if _, err = f.Seek(q.Offset, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, q.Limit))
	return map[string]any{"data": base64.StdEncoding.EncodeToString(b), "offset": q.Offset + int64(len(b)), "size": info.Size(), "encoding": "base64"}, err
}

func (n *Node) filesWrite(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var q struct {
		Path     string `json:"path"`
		Data     string `json:"data"`
		Offset   int64  `json:"offset"`
		Truncate bool   `json:"truncate"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if q.Offset < 0 {
		return nil, errors.New("invalid offset")
	}
	b, err := base64.StdEncoding.DecodeString(q.Data)
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, errors.New("write exceeds 1 MiB")
	}
	if err = n.root.MkdirAll(filepath.Dir(q.Path), 0700); err != nil {
		return nil, err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if q.Truncate {
		flags |= os.O_TRUNC
	}
	f, err := n.root.OpenFile(q.Path, flags, 0600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err = f.Seek(q.Offset, io.SeekStart); err != nil {
		return nil, err
	}
	written, err := f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	return map[string]any{"written": written}, err
}
