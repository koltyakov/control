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
	"unicode/utf8"

	"github.com/koltyakov/control/internal/commandexec"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/secrets"
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
	encodingSchema := map[string]any{"type": "string", "enum": []string{encodingBase64, encodingText}, "description": "Requested result encoding. text returns valid UTF-8 directly and falls back to base64 otherwise; default base64."}
	builtins := []provider{
		{model.Capability{Name: "network.wake", Description: "Send Wake-on-LAN magic packets to an offline registered machine using an automatically verified LAN relationship and live interfaces. Success means sent, not awake. Never retries.", InputSchema: schema(map[string]any{"target": text}, "target")}, n.wake},
		{model.Capability{Name: "exec.run", Description: "Run an executable with arguments and environment. envSecrets maps environment variable names to worker-local secret names; stdinSecret supplies one secret as exact stdin bytes instead of stdin. References resolve at execution, never into task specs or argv. Secret-bearing output and errors are masked; logs are delayed until complete output can be masked. Files and arbitrary transformations are not masked. Ordinary descendants are terminated before completion; use an OS service manager for persistent services. A relative dir resolves against the invocation workspace, workDir/tasks/ID for a tracked task, not the files.* workDir root. An absolute dir is used unchanged. Declared task outputs still resolve against the original task workspace. Use a task for long-running work.", InputSchema: schema(map[string]any{"command": text, "args": map[string]any{"type": "array", "items": text}, "env": object, "envSecrets": map[string]any{"type": "object", "maxProperties": 32, "propertyNames": map[string]any{"pattern": "^[a-zA-Z_][a-zA-Z0-9_]{0,127}$"}, "additionalProperties": map[string]any{"type": "string", "pattern": "^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$"}, "description": "Environment variable to worker-local secret name. Names must be unique and cannot overlap env, ignoring case. Overrides inherited environment only for this invocation."}, "dir": map[string]any{"type": "string", "description": "Absolute working directory or path relative to the invocation workspace; tracked tasks use workDir/tasks/ID. Unlike files.* paths, this is not relative to workDir."}, "stdin": text, "stdinSecret": map[string]any{"type": "string", "pattern": "^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$", "description": "Worker-local secret supplying exact stdin bytes, without an added newline. Mutually exclusive with stdin, including an empty stdin."}}, "command")}, n.execRun},
		{model.Capability{Name: "agent.run", Description: "Run a configured AI CLI with a prompt on stdin; collect its output and terminate ordinary descendants before completion.", InputSchema: schema(map[string]any{"agent": text, "prompt": text}, "agent", "prompt")}, n.agentRun},
		{model.Capability{Name: "http.request", Description: "Make an HTTP request using this machine's network and DNS. headerSecrets maps header names to {secret, prefix}; references resolve only on this worker. Secret-bearing requests never follow redirects. Returned text masks configured secret values. The body is base64 unless encoding is text and it is valid UTF-8; check the returned encoding.", InputSchema: schema(map[string]any{"url": text, "method": text, "encoding": encodingSchema, "headers": object, "headerSecrets": map[string]any{"type": "object", "maxProperties": 32, "additionalProperties": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"secret"}, "properties": map[string]any{"secret": map[string]any{"type": "string", "pattern": "^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$"}, "prefix": text}}}, "body": text}, "url")}, n.httpRequest},
		{model.Capability{Name: "http.download", Description: "Stream a GET response body into an immutable content-addressed artifact on this machine, held by the caller's task owner; returns status, selected headers and the artifact reference, never the body. Requires 200 OK. maxBytes defaults to 1 GiB; maxArtifactBytes and the disk reserve also apply. sha256 is verified before publication. Ends after stallSeconds (default 30) without headers or body bytes; the invocation or task deadline bounds the whole download. Failures publish nothing and remove partial bytes; there is no retry or resume. headerSecrets match http.request; secret-bearing requests never follow redirects, others follow at most 10 http/https redirects. Errors and headers are masked; downloaded bytes are not. Prefer a tracked task.", InputSchema: schema(map[string]any{"url": text, "method": map[string]any{"type": "string", "enum": []string{http.MethodGet}}, "headers": object, "headerSecrets": map[string]any{"type": "object", "maxProperties": 32, "additionalProperties": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"secret"}, "properties": map[string]any{"secret": map[string]any{"type": "string", "pattern": "^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$"}, "prefix": text}}}, "sha256": map[string]any{"type": "string", "pattern": "^[a-fA-F0-9]{64}$"}, "maxBytes": map[string]any{"type": "integer", "minimum": 1, "maximum": maxDownloadBytes}, "name": map[string]any{"type": "string", "maxLength": 255}, "stallSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 3600}}, "url")}, n.httpDownload},
		{model.Capability{Name: "files.list", Description: "List a directory relative to the configured filesystem root. Rejects non-directory targets, including special files.", InputSchema: schema(map[string]any{"path": text})}, n.filesList},
		{model.Capability{Name: "files.read", Description: "Read up to 1 MiB at an offset; returns base64 bytes, or UTF-8 text when encoding is text and the bytes are valid text. Text never splits a character; continue from the returned offset and check the returned encoding.", InputSchema: schema(map[string]any{"path": text, "offset": map[string]any{"type": "integer"}, "limit": map[string]any{"type": "integer"}, "encoding": encodingSchema}, "path")}, n.filesRead},
		{model.Capability{Name: "files.write", Description: "Write base64 bytes at an offset to a regular file. Creates missing files and rejects special files. Set truncate for replacement.", InputSchema: schema(map[string]any{"path": text, "data": text, "offset": map[string]any{"type": "integer"}, "truncate": map[string]any{"type": "boolean"}}, "path", "data")}, n.filesWrite},
		{model.Capability{Name: "mcp.call", Description: "Invoke a tool on a configured local MCP server. Tool-reported isError fails execution; tracked tasks retain the original result, including partial output. Failure does not authorize replay.", InputSchema: schema(map[string]any{"server": text, "tool": text, "arguments": object}, "server", "tool")}, n.mcpCall},
		{model.Capability{Name: "mcp.request", Description: "Read MCP resources or prompts on a configured server.", InputSchema: schema(map[string]any{"server": text, "method": text, "params": object}, "server", "method")}, n.mcpRequest},
		{model.Capability{Name: "workflow.run", Description: "Execute dependency-ordered tasks across peers and transfer their artifact inputs. maxParallel runs up to that many ready steps at once (default 1, sequential). Parallel ready ordering skips capacity probes for identical destination and pool requirements; targets and accepted tasks never move or replay. Transient status reads retry; unresolved observations retain child IDs without cancelling healthy work.", InputSchema: schema(map[string]any{"steps": map[string]any{"type": "array", "items": object}, "maxParallel": map[string]any{"type": "integer", "minimum": 1, "maximum": model.MaxWorkflowParallel}}, "steps")}, n.workflowRun},
		{model.Capability{Name: "peers.call", Description: "Carry out an orchestrator-authorized instruction on another worker. Worker credentials alone permit discovery, not peer execution. CLI/MCP prepares an instruction-bound delegation.", InputSchema: schema(map[string]any{"target": text, "method": text, "params": object}, "target", "method", "params")}, n.peerCall},
	}
	for _, p := range builtins {
		if err := n.Register(p); err != nil {
			return err
		}
	}
	if err := n.registerDesktop(); err != nil {
		return err
	}
	if err := n.registerBrowser(); err != nil {
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
	exit, err := commandexec.Run(ctx, cmd)
	result := commandResult{ExitCode: exit, Stdout: stdout.b.String(), Stderr: stderr.b.String(), Truncated: stdout.truncated || stderr.truncated}
	if err != nil {
		return result, fmt.Errorf("execute %s: %w", cfg.Command, err)
	}
	return result, nil
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

func (n *Node) httpRequest(ctx context.Context, args json.RawMessage, e Execution) (_ any, runErr error) {
	var input struct {
		URL           string            `json:"url"`
		Method        string            `json:"method"`
		Encoding      string            `json:"encoding"`
		Headers       map[string]string `json:"headers"`
		Body          string            `json:"body"`
		HeaderSecrets map[string]struct {
			Secret string `json:"secret"`
			Prefix string `json:"prefix"`
		} `json:"headerSecrets"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}
	if len(input.HeaderSecrets) > 32 {
		return nil, errors.New("at most 32 secret headers are allowed")
	}
	if err := validEncoding(input.Encoding); err != nil {
		return nil, err
	}
	values, err := n.secretValues()
	if err != nil {
		return nil, err
	}
	redactor := secrets.NewRedactor(values)
	defer func() {
		if runErr != nil && redactor.Text(runErr.Error()) != runErr.Error() {
			runErr = errors.New(redactor.Text(runErr.Error()))
		}
	}()
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
	for k, ref := range input.HeaderSecrets {
		if req.Header.Values(k) != nil {
			return nil, errors.New("header cannot have both a literal value and a secret reference")
		}
		value, err := resolveSecret(values, ref.Secret)
		if err != nil {
			return nil, err
		}
		req.Header.Set(k, ref.Prefix+value)
	}
	httpClient := &http.Client{Timeout: 5 * time.Minute}
	if len(input.HeaderSecrets) != 0 {
		httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	resp, err := httpClient.Do(req)
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
	cleanHeaders := http.Header{}
	for k, items := range resp.Header {
		for _, v := range items {
			cleanHeaders[redactor.Text(k)] = append(cleanHeaders[redactor.Text(k)], redactor.Text(v))
		}
	}
	if len(values) != 0 && json.Valid(b) {
		// An API may JSON-escape Unicode credentials when echoing input.
		b, err = redactor.JSON(b)
		if err != nil {
			return nil, errors.New("cannot sanitize HTTP response")
		}
	} else {
		b = []byte(redactor.Text(string(b)))
	}
	body, encoding := encodeBytes(b, input.Encoding)
	return map[string]any{"status": resp.StatusCode, "headers": cleanHeaders, "body": body, "encoding": encoding}, nil
}

const (
	encodingBase64 = "base64"
	encodingText   = "text"
)

func validEncoding(encoding string) error {
	if encoding != "" && encoding != encodingBase64 && encoding != encodingText {
		return errors.New("encoding must be base64 or text")
	}
	return nil
}

// encodeBytes returns text only when requested and lossless. Base64 remains
// the default, so callers that always decode it keep working.
func encodeBytes(b []byte, requested string) (string, string) {
	if requested == encodingText && utf8.Valid(b) {
		return string(b), encodingText
	}
	return base64.StdEncoding.EncodeToString(b), encodingBase64
}

// completeText trims at most one incomplete trailing UTF-8 sequence from a
// chunk that ended before EOF, so a text read never splits a character.
func completeText(b []byte) []byte {
	for n := 1; n < utf8.UTFMax && n <= len(b); n++ {
		if utf8.RuneStart(b[len(b)-n]) {
			if !utf8.FullRune(b[len(b)-n:]) {
				return b[:len(b)-n]
			}
			break
		}
	}
	return b
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
	if err := n.uploads.CheckPath(q.Path); err != nil {
		return nil, err
	}
	f, err := openDirectory(n.root, q.Path)
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
		if n.uploads.CheckPath(filepath.Join(q.Path, entry.Name())) != nil {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue // Removed after the directory was read.
		}
		if err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"name": entry.Name(), "directory": entry.IsDir(), "size": info.Size(), "modified": info.ModTime()})
	}
	return result, nil
}

func (n *Node) filesRead(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var q struct {
		Path     string `json:"path"`
		Offset   int64  `json:"offset"`
		Limit    int64  `json:"limit"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if q.Offset < 0 || q.Limit < 0 || q.Limit > 1<<20 {
		return nil, errors.New("invalid offset or limit")
	}
	if err := validEncoding(q.Encoding); err != nil {
		return nil, err
	}
	if q.Limit == 0 {
		q.Limit = 64 * 1024
	}
	if err := n.uploads.CheckPath(q.Path); err != nil {
		return nil, err
	}
	f, info, err := openRegularFile(n.root, q.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err = f.Seek(q.Offset, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, q.Limit))
	if q.Encoding == encodingText && int64(len(b)) == q.Limit && q.Offset+int64(len(b)) < info.Size() {
		if trimmed := completeText(b); len(trimmed) != 0 && utf8.Valid(trimmed) {
			b = trimmed
		}
	}
	data, encoding := encodeBytes(b, q.Encoding)
	return map[string]any{"data": data, "offset": q.Offset + int64(len(b)), "size": info.Size(), "encoding": encoding}, err
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
	if err := n.uploads.CheckPath(q.Path); err != nil {
		return nil, err
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
	f, err := openRegularFileForWrite(n.root, q.Path, q.Truncate)
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
