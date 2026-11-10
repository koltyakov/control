package client

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/skills"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPAndSkillInstructions(t *testing.T) {
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
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, required := range map[string][]string{
		"control_browser": {
			"opt-in", "args, optional session and non-secret stdin", "--version and skills get core",
			"task-visible", "no secret-reference substitution", "Text results are masked",
			"live frames/files/upstream activity are not", "Pause automation", "Never replay uncertain effects",
		},
		"control_browser_view": {
			"already started", "loopback", "interactive access to every session", "not read-only",
			"Does not start a browser/dashboard or verify readiness", "control_forward_stop or MCP exit",
			"stopping it does not close the worker browser/dashboard", "on this MCP server host",
			"Live frames and user input are not redacted", "docs/browser.md#mcp-workflow",
		},
	} {
		found := false
		for _, tool := range tools.Tools {
			if tool.Name != name {
				continue
			}
			found = true
			for _, fragment := range required {
				if !strings.Contains(tool.Description, fragment) {
					t.Errorf("%s description omits usage or safety guidance %q", name, fragment)
				}
			}
		}
		if !found {
			t.Errorf("missing browser tool %s", name)
		}
	}
	// Check the complete installed bundle for retained workflow safeguards. The
	// skills package separately checks that the core loads references on demand.
	skill := string(skills.Control)
	references, err := fs.Glob(skills.ControlFiles, "control/references/*.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range references {
		content, err := skills.ControlFiles.ReadFile(reference)
		if err != nil {
			t.Fatal(err)
		}
		skill += "\n" + string(content)
	}
	skill = strings.ReplaceAll(skill, "\r\n", "\n")
	for name, text := range map[string]string{
		"mcp":               session.InitializeResult().Instructions,
		"skill bundle LF":   skill,
		"skill bundle CRLF": strings.ReplaceAll(skill, "\n", "\r\n"),
	} {
		// Windows checkouts can embed CRLF; these checks concern content, not line endings.
		text = strings.ReplaceAll(text, "\r\n", "\n")
		for _, term := range []string{"orchestrator", "worker", "gateway", "client", "node", "AI software"} {
			if !strings.Contains(text, term) {
				t.Errorf("%s instructions omit %q", name, term)
			}
		}
		for _, obsolete := range []string{"fleet agent", "host agent", "Lifecycle-capable agents", "rpa.run", "control_rpa"} {
			if strings.Contains(text, obsolete) {
				t.Errorf("%s instructions use obsolete terminology %q", name, obsolete)
			}
		}
		required := []string{
			"target worker's system.os and system.arch", "not the orchestrator's platform",
			"Prefer native OS/architecture assets", "build targets",
			"Control reports its node process architecture",
			"checksum", "actual running executable before claiming an architecture mismatch",
			"Review existing installation evidence", "high memory use alone does not establish emulation",
			"browser.run", "control_browser", "control_browser_view", "skills", "core",
			"2FA", "dashboard", "loopback", "task-visible", "not redacted",
			"busy worker is not unavailable", "independent work", "maxTasks", "maxOperations", "inUse",
			"queued", "unknown", "one separate desktop slot", "per-user file lock",
			"inputStaging", "outputPublication", "maxTaskStaging", "maxTaskPublications", "replaying the provider",
			"32 unstarted ready steps", "eight destinations", "moves/resubmits accepted tasks",
			"machine-wide lease", "conflicting", "external state",
			"control_upload", "control_upload_resume", "control_upload_status", "files.write", "localPath", "no-overwrite",
			"runbooks", "control_runbooks", ".control/runbooks/",
			"requires", "agent-friendly", "plain",
			"before ad-hoc probing", "contentOmitted", "uncertain actions",
			"https://github.com/vercel-labs/agent-desktop",
			"version", "desktop", "platform", "data.content", "status",
			"exec.run", "structured", "worker", "--headed", "explicit user permission",
			"combined runbook catalog", "same catalog unless it is missing or incomplete",
			"source error is not a reason to retry or bypass it", "workDir/tasks/ID",
			"For isolated file tasks with task creation authorized", "one tracked inline command", "hashes protected files",
			"When task creation is prohibited", "not tracked read tasks", "Respect leases",
			"if no permitted read route exists, report the blocker",
			"variable names are case-insensitive", "$q", "$Q", "Get-History",
			"explicit encoding", "quoted multiline", "hand-written parser",
			"creates output without overwriting", "Report failed checks and partial effects",
			"does not permit batching dependent UI actions, credential operations, or uncertain-effect retries",
			"control secrets set NAME", "secret", "setValue", "profile",
			"exec.run output is not secret-masked",
			"desktop.run", "control_desktop", "private", "masked",
			"desktop.cli", "command whitelist", "list-apps", "default state", "declare",
			"suggest installation", "After approval", "latest compatible release", "agent-desktop-ffi-*",
			"https://github.com/vercel-labs/agent-desktop/releases",
			"SHA-256", "checksums.txt", "same release",
			"ABI major 6 or 7", "96-byte AdAction", "reject incompatible libraries", "before credential use",
			"data.complete", "nodes_observed", "subtree_truncated", "partial observation",
			"companions outside workDir", "desktop.library",
			"other settings", "while idle", "separate approval",
			"user-visible UI actions", "presentation-only cursor overlay with --size 0.75",
			"Skip the overlay for background-only accessibility-tree reads",
			"Headless semantic actions can still expose UI", "unless the user requests it",
			"session start --no-trace before taking snapshots", "cursor-overlay enable --size 0.75",
			"--session ID explicitly to every overlay, snapshot and action command",
			"native credential input sessionless with fresh native refs",
			"Disable the overlay when returning to background-only work", "end only sessions you created",
			"data.rendered", "no cursor was drawn", "unsupported Linux Wayland sessions",
			"Missing overlay support does not block otherwise permitted UI work",
			"Do not install software, change display servers, grant OS permissions, enable tracing or add --headed merely to show a cursor",
		}
		if name == "mcp" {
			required = append(required,
				"arm64/aarch64", "amd64/x86_64/x64",
				"verify the native OS architecture on that worker first",
				"Do not silently fall back to emulation", "explain the limitation and ask",
				"selected download's checksum and installed executable's architecture",
				"label alone distinguishes neither ARM64 nor x64",
				"npx skills add https://github.com/vercel-labs/agent-desktop --skill agent-desktop",
				"never substitute a shell credential workaround",
				"credentials are no longer visible", "no action batches", "not feature-equivalent")
		} else {
			required = append(required,
				"browser.cli", "control_forward_stop",
				"ask before an emulated fallback", "may differ from the native host",
				"Only when a task requires GUI interaction",
				"skip these checks and skill loads for non-GUI tasks",
				"https://github.com/koltyakov/control/blob/main/docs/desktop.md",
				"stop for manual login", "never retrieve credentials through a shell",
				"Do not create temporary request files", "control task start worker --wait",
				"leaseId", "Reduce model turns, not safety checks", "fresh masked observations",
				"does not cancel accepted work")
			if strings.Contains(text, "control task start worker @task.json") {
				t.Errorf("%s teaches temporary orchestration request files", name)
			}
			_, gui, found := strings.Cut(text, "## GUI work, only when needed\n")
			gui, _, _ = strings.Cut(gui, "\n## ")
			if !found || len(strings.Fields(gui)) > 200 {
				t.Errorf("%s should keep GUI guidance brief and load details on demand", name)
			}
		}
		// The skill uses Markdown code spans; MCP uses plain instructions.
		plain := strings.ReplaceAll(text, "`", "")
		for _, fragment := range required {
			if !strings.Contains(plain, fragment) {
				t.Errorf("%s instructions omit workflow guidance %q", name, fragment)
			}
		}
	}
}

func TestMCPBrowserProgressiveDisclosure(t *testing.T) {
	if words := len(strings.Fields(browserInstructions)); words > 130 {
		t.Errorf("unconditional browser instructions have %d words; keep workflow details in the browser guide", words)
	}
	for _, required := range []string{
		"For browser work only", "discover browser.run", "control_browser on the target worker",
		"--version and skills get core", "same owner/session and fresh refs",
		"agent-browser or Chrome requires approval", "task-visible", "cannot resolve Control secret references",
		"never put credentials in chat/tool inputs or retrieve them through a shell",
		"Frames, files and upstream activity are not redacted",
		"Pause AI mutations", "explicit hand-back and a fresh snapshot", "Never replay uncertain effects",
		"https://github.com/koltyakov/control/blob/main/docs/browser.md",
		"loopback control_browser_view", "separate browser/forward cleanup",
		"Skip browser probes and guidance for command-only or file-only work",
	} {
		if !strings.Contains(browserInstructions, required) {
			t.Errorf("browser instructions omit routing or safety guidance %q", required)
		}
	}
	for _, detailed := range []string{"dashboard,start", "4848", "browser.cli", "maxTasks", "homes/runtime/namespaces", "Passkeys/security keys"} {
		if strings.Contains(browserInstructions, detailed) {
			t.Errorf("unconditional browser instructions still include workflow detail %q", detailed)
		}
	}
	content, err := skills.ControlFiles.ReadFile("control/references/browser.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"browser.cli", "maxTasks", "dashboard start --port 4848", "distinct worker port",
		"all sessions in its namespace", "Passkeys/security keys are not forwarded",
		"control_forward_stop", "dashboard stop", "can survive Control calls/restarts",
		"https://github.com/koltyakov/control/blob/main/docs/browser.md",
	} {
		if !strings.Contains(string(content), required) {
			t.Errorf("targeted browser reference omits workflow guidance %q", required)
		}
	}
}

func TestMCPRoutesBrowser(t *testing.T) {
	requests := make(chan map[string]json.RawMessage, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- request
		_, _ = w.Write([]byte(`{"result":{"success":true}}`))
	}))
	defer api.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := (Client{URL: api.URL, Token: "test"}).WithLifetime(ctx).MCPServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_browser", Arguments: map[string]any{
		"node": "worker", "request": map[string]any{"session": "work", "args": []string{"snapshot", "-i"}},
	}})
	if err != nil || result.IsError {
		t.Fatal("browser MCP call failed", result, err)
	}
	select {
	case request := <-requests:
		if string(request["target"]) != `"worker"` || string(request["method"]) != `"browser.run"` || !strings.Contains(string(request["params"]), `"session":"work"`) {
			t.Fatal("browser MCP routing changed", request)
		}
	case <-ctx.Done():
		t.Fatal("browser MCP call was not routed")
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "control_browser_view", Arguments: map[string]any{
		"node": "worker", "listen": "0.0.0.0:1234",
	}})
	if err == nil && !result.IsError {
		t.Fatal("MCP exposed browser view publicly")
	}
}

func TestMCPRoutesDesktop(t *testing.T) {
	requests := make(chan map[string]json.RawMessage, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"ok":true}}`))
	}))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := (Client{URL: httpServer.URL, Token: "test"}).MCPServer().Connect(ctx, serverTransport, nil)
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
	for _, tool := range tools.Tools {
		if tool.Name == "control_rpa" {
			t.Fatal("removed RPA tool is still advertised")
		}
	}
	if result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_rpa", Arguments: map[string]any{}}); err == nil && !result.IsError {
		t.Fatal("removed RPA tool was callable")
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_desktop", Arguments: map[string]any{
		"node": "desktop", "request": map[string]any{"command": "setValue", "ref": "@s123:e1", "secret": "app.password"},
	}})
	if err != nil || result.IsError {
		t.Fatalf("native desktop tool failed: %+v, %v", result, err)
	}
	select {
	case request := <-requests:
		if string(request["target"]) != `"desktop"` || string(request["method"]) != `"desktop.run"` ||
			!strings.Contains(string(request["params"]), `"secret":"app.password"`) || strings.Contains(string(request["params"]), `"text"`) {
			t.Fatal("native desktop did not route a reference-only request")
		}
	case <-ctx.Done():
		t.Fatal("native desktop did not route a request")
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "control_desktop", Arguments: map[string]any{
		"node": "desktop", "request": map[string]any{"command": "cli", "args": []string{"list-apps"}},
	}})
	if err != nil || result.IsError {
		t.Fatalf("full desktop CLI tool failed: %+v, %v", result, err)
	}
	select {
	case request := <-requests:
		if string(request["method"]) != `"desktop.run"` || !strings.Contains(string(request["params"]), `"list-apps"`) {
			t.Fatal("full agent-desktop commands did not route through desktop.run")
		}
	case <-ctx.Done():
		t.Fatal("full desktop CLI request was not routed")
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "control_desktop", Arguments: map[string]any{
		"node": "desktop", "request": map[string]any{"command": "screenshot"},
	}})
	if err == nil && !result.IsError {
		t.Fatal("native desktop tool accepted an unprotected command")
	}
}
