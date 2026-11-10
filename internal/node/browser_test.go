package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport/transporttest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A native test executable stands in for agent-browser on every supported OS.
// No test installs Chrome, opens a browser, or uses the host's browser state.
func TestMain(m *testing.M) {
	if mode := os.Getenv("CONTROL_TEST_AGENT_BROWSER"); mode != "" {
		browserFixture(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func browserFixture(mode string) {
	if slices.Equal(os.Args[1:], []string{"--version"}) {
		version := "0.41.2"
		if mode == "old" {
			version = "0.10.0"
		}
		_, _ = fmt.Fprintln(os.Stdout, "agent-browser", version)
		return
	}
	if marker := os.Getenv("CONTROL_TEST_BROWSER_EFFECT_MARKER"); marker != "" {
		if os.WriteFile(marker, []byte("executed"), 0600) != nil {
			os.Exit(93)
		}
	}
	input, _ := io.ReadAll(os.Stdin)
	data := map[string]any{
		"args": os.Args[1:], "stdin": string(input), "home": os.Getenv("HOME"),
		"namespace": os.Getenv("AGENT_BROWSER_NAMESPACE"), "session": os.Getenv("AGENT_BROWSER_SESSION"),
		"runtime": os.Getenv("AGENT_BROWSER_SOCKET_DIR"), "config": os.Getenv("AGENT_BROWSER_CONFIG"),
		"value": desktopCredential,
	}
	for _, key := range []string{"AGENT_BROWSER_CDP", "AGENT_BROWSER_PROFILE", "AGENT_BROWSER_STATE", "AGENT_BROWSER_DASHBOARD_ALLOWED_ORIGINS"} {
		if os.Getenv(key) != "" {
			os.Exit(91)
		}
	}
	fmt.Fprint(os.Stderr, desktopCredential[:8])
	fmt.Fprint(os.Stderr, desktopCredential[8:])
	switch mode {
	case "fail":
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"success": false, "error": "element not found " + desktopCredential})
		return
	case "exit":
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"success": true, "data": data})
		os.Exit(7)
	case "batch":
		_ = json.NewEncoder(os.Stdout).Encode([]any{map[string]any{"success": true}, map[string]any{"success": false, "error": "element not found " + desktopCredential}})
		return
	case "long-failure":
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"success": false, "error": desktopCredential + strings.Repeat("界", 3000)})
		return
	case "help":
		_, _ = fmt.Fprint(os.Stdout, "usage ", desktopCredential)
		return
	case "oversized":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", (2<<20)+1))
		return
	case "binary":
		_, _ = os.Stdout.Write([]byte{0xff})
		return
	case "sleep":
		time.Sleep(time.Minute)
	case "artifact":
		// Model a persistent daemon: file paths remain relative to the first
		// invocation's directory even when later CLIs run in task workspaces.
		state := filepath.Join(os.Getenv("HOME"), "fixture-"+os.Getenv("AGENT_BROWSER_SESSION")+".cwd")
		cwd, err := os.ReadFile(state)
		if os.IsNotExist(err) {
			dir, dirErr := os.Getwd()
			if dirErr != nil || os.WriteFile(state, []byte(dir), 0600) != nil {
				os.Exit(92)
			}
			cwd = []byte(dir)
		} else if err != nil {
			os.Exit(92)
		}
		if len(os.Args) == 4 && os.Args[2] == "screenshot" {
			path := os.Args[3]
			if !filepath.IsAbs(path) {
				path = filepath.Join(string(cwd), path)
			}
			if os.WriteFile(path, []byte("fake-browser-image"), 0600) != nil {
				os.Exit(92)
			}
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"success": true, "data": data})
}

func browserNode(t *testing.T, mode string) *Node {
	t.Helper()
	t.Setenv("CONTROL_TEST_AGENT_BROWSER", mode)
	// Avoid real user runtime storage in every fixture.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return localNode(t, func(c *Config) { c.Browser = &BrowserConfig{CLI: exe} })
}

func TestBrowserOptInAndValidation(t *testing.T) {
	n := localNode(t, nil)
	for _, cap := range n.Capabilities() {
		if cap.Name == "browser.run" {
			t.Fatal("browser enabled without configuration")
		}
	}
	for _, cfg := range []*BrowserConfig{{}, {CLI: "relative"}, {CLI: os.Args[0], Config: "relative"}} {
		n.Config.Browser = cfg
		if n.registerBrowser() == nil {
			t.Fatal("invalid browser configuration accepted")
		}
	}
	if _, err := n.privateBrowserDir(filepath.Join(n.Config.WorkDir, "exposed")); err == nil {
		t.Fatal("browser state exposed through workspace")
	}
}

func TestBrowserVersionCompatibility(t *testing.T) {
	for _, version := range []string{"agent-browser 0.41.2\n", "agent-browser 0.42.0", "agent-browser 1.0.0"} {
		if !compatibleBrowserVersion(version) {
			t.Fatal("compatible browser rejected", version)
		}
	}
	for _, version := range []string{"", "0.41.2", "other 0.41.2", "agent-browser 0.41.1", "agent-browser 0.40.99", "agent-browser 0.41.2-beta", "agent-browser 0.41", "agent-browser -1.42.0"} {
		if compatibleBrowserVersion(version) {
			t.Fatal("unsupported browser accepted", version)
		}
	}
}

func TestBrowserCLIIsolationAndRedaction(t *testing.T) {
	n := browserNode(t, "echo")
	t.Setenv("AGENT_BROWSER_CDP", "untrusted-browser")
	t.Setenv("AGENT_BROWSER_PROFILE", "untrusted-profile")
	t.Setenv("AGENT_BROWSER_DASHBOARD_ALLOWED_ORIGINS", "https://untrusted.example")
	setTestSecret(t, n, "app.password", desktopCredential)
	var namespaces []string
	for _, owner := range []string{"alice", "bob", "alice"} {
		var log bytes.Buffer
		args := []string{"future-command", "literal ; $(not-a-shell)"}
		value, err := n.browserRun(t.Context(), model.JSON(map[string]any{
			"args": args, "session": "work", "stdin": "public input",
		}), Execution{Dir: n.Config.WorkDir, Owner: owner, Log: &log})
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Data struct {
				Data struct {
					Args      []string `json:"args"`
					Stdin     string   `json:"stdin"`
					Home      string   `json:"home"`
					Namespace string   `json:"namespace"`
					Session   string   `json:"session"`
					Runtime   string   `json:"runtime"`
				} `json:"data"`
			} `json:"data"`
		}
		raw := model.JSON(value)
		if json.Unmarshal(raw, &got) != nil || !slices.Equal(got.Data.Data.Args, append([]string{"--json"}, args...)) ||
			got.Data.Data.Stdin != "public input" || got.Data.Data.Session != "work" || got.Data.Data.Home != "[REDACTED]" || got.Data.Data.Runtime != "[REDACTED]" {
			t.Fatal("browser forwarding/isolation changed", string(raw))
		}
		if log.Len() != 0 || hasDesktopCredential(raw) {
			t.Fatal("browser raw output leaked")
		}
		namespaces = append(namespaces, got.Data.Data.Namespace)
	}
	if namespaces[0] == namespaces[1] || namespaces[0] != namespaces[2] {
		t.Fatal("browser namespace is not stable and owner-scoped")
	}
}

func TestBrowserFailures(t *testing.T) {
	for _, mode := range []string{"fail", "exit", "batch", "help", "oversized", "binary", "sleep"} {
		t.Run(mode, func(t *testing.T) {
			n := browserNode(t, mode)
			setTestSecret(t, n, "app.password", desktopCredential)
			ctx := t.Context()
			if mode == "sleep" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			var log bytes.Buffer
			value, err := n.browserRun(ctx, model.JSON(map[string]any{"args": []string{"snapshot"}}), Execution{Dir: n.Config.WorkDir, Owner: "owner", Log: &log})
			if (err == nil) != (mode == "help") || log.Len() != 0 || hasDesktopCredential(model.JSON(value)) || (err != nil && hasDesktopCredential([]byte(err.Error()))) {
				t.Fatal("browser failure/privacy contract violated", err)
			}
		})
	}
}

func TestBrowserCompatibilityAndLock(t *testing.T) {
	n := browserNode(t, "old")
	marker := filepath.Join(t.TempDir(), "effect")
	t.Setenv("CONTROL_TEST_BROWSER_EFFECT_MARKER", marker)
	raw := model.JSON(map[string]any{"args": []string{"open", "https://example.com"}})
	e := Execution{Dir: n.Config.WorkDir, Owner: "owner", Log: io.Discard}
	if _, err := n.browserRun(t.Context(), raw, e); err == nil {
		t.Fatal("incompatible browser accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("incompatible browser executed the requested command")
	}
	t.Setenv("CONTROL_TEST_AGENT_BROWSER", "echo")
	if _, err := n.browserRun(t.Context(), raw, e); err != nil {
		t.Fatal(err)
	}
	owners, err := os.ReadDir(filepath.Join(n.Config.DataDir, "browser"))
	if err != nil || len(owners) != 1 {
		t.Fatal("missing private owner directory", err)
	}
	lock := flock.New(filepath.Join(n.Config.DataDir, "browser", owners[0].Name(), "command.lock"))
	defer func() { _ = lock.Close() }()
	if held, err := lock.TryLock(); err != nil || !held {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := n.browserRun(ctx, raw, e); err == nil {
		t.Fatal("browser bypassed owner command serialization")
	}
}

func TestBrowserPeerAuthorizationAndDurability(t *testing.T) {
	t.Setenv("CONTROL_TEST_AGENT_BROWSER", "echo")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, other := cluster(t, relay, func(i int, cfg *Config) {
				if i == 1 {
					cfg.Browser = &BrowserConfig{CLI: exe}
					cfg.Allow = map[string][]string{"source": {"browser.run", "tasks.*", "leases.*"}}
				}
			}, transporttest.Loopback())
			setTestSecret(t, worker, "app.password", desktopCredential)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			args := model.JSON(map[string]any{"session": "work", "args": []string{"snapshot", "-i"}})
			if err := testCall(t, other, ctx, "worker", "browser.run", args, nil); err == nil {
				t.Fatal("unauthorized caller operated browser")
			}
			var lease model.Lease
			call(t, source, "worker", "leases.acquire", map[string]any{}, &lease)
			if err := testCall(t, source, ctx, "worker", "browser.run", args, nil); err == nil {
				t.Fatal("browser bypassed exclusive lease")
			}
			spec := model.TaskSpec{ID: "browser-once", Capability: "browser.run", Args: args, LeaseID: lease.ID}
			var accepted model.Task
			call(t, source, "worker", "tasks.start", spec, &accepted)
			finished, err := testWaitTask(t, source, ctx, "worker", accepted.ID)
			if err != nil || finished.State != "succeeded" || hasDesktopCredential(model.JSON(finished)) {
				t.Fatal("browser peer task failed or exposed raw credentials", finished, err)
			}
			call(t, source, "worker", "tasks.start", spec, &accepted)
			if accepted.Updated != finished.Updated {
				t.Fatal("browser peer task was replayed")
			}
			call(t, source, "worker", "leases.release", map[string]string{"id": lease.ID}, nil)
			t.Setenv("CONTROL_TEST_AGENT_BROWSER", "batch")
			err = testCall(t, source, ctx, "worker", "browser.run", args, nil)
			if err == nil || !strings.Contains(err.Error(), "element not found") || !strings.Contains(err.Error(), `"success":false`) || hasDesktopCredential([]byte(err.Error())) {
				t.Fatal("peer browser failure lost sanitized diagnostics", err)
			}
		})
	}
}

func TestBrowserTaskArtifactsAndReconciliation(t *testing.T) {
	n := browserNode(t, "artifact")
	setTestSecret(t, n, "app.password", desktopCredential)
	original := t.TempDir()
	if _, err := n.browserRun(t.Context(), model.JSON(map[string]any{"session": "work", "args": []string{"open", "https://example.com"}}), Execution{Dir: original, Owner: "owner"}); err != nil {
		t.Fatal(err)
	}
	spec := model.TaskSpec{ID: "browser-image", Capability: "browser.run", Outputs: []string{"screen.png"}, Args: model.JSON(map[string]any{"session": "work", "args": []string{"screenshot", "screen.png"}})}
	accepted, err := n.startTaskContext(t.Context(), "owner", spec)
	if err != nil {
		t.Fatal(err)
	}
	finished, err := n.waitTask(t.Context(), "owner", accepted.ID, 10*time.Second)
	if err != nil || finished.State != "succeeded" || len(finished.Artifacts) != 1 {
		t.Fatal("browser artifact task failed", finished, err)
	}
	for _, path := range []string{n.taskPath(spec.ID), n.logPath(spec.ID)} {
		data, err := os.ReadFile(path)
		if err != nil || hasDesktopCredential(data) || (path == n.logPath(spec.ID) && len(data) != 0) {
			t.Fatal("browser task retained raw output", err)
		}
	}
	repeated, err := n.startTaskContext(t.Context(), "owner", spec)
	if err != nil || repeated.Updated != finished.Updated {
		t.Fatal("browser task replayed during reconciliation")
	}
	spec.ID = "browser-image-second"
	if _, err := n.startTaskContext(t.Context(), "owner", spec); err != nil {
		t.Fatal(err)
	}
	second, err := n.waitTask(t.Context(), "owner", spec.ID, 10*time.Second)
	if err != nil || second.State != "succeeded" || len(second.Artifacts) != 1 {
		t.Fatal("reused session did not publish the second task's screenshot", second, err)
	}
	if _, err := os.Stat(filepath.Join(original, "screen.png")); !os.IsNotExist(err) {
		t.Fatal("screenshot was saved in the daemon's original directory", err)
	}
}

func TestBrowserFailureDiagnostics(t *testing.T) {
	for _, mode := range []string{"fail", "exit", "batch", "long-failure"} {
		t.Run(mode, func(t *testing.T) {
			n := browserNode(t, mode)
			setTestSecret(t, n, "app.password", desktopCredential)
			value, err := n.browserRun(t.Context(), model.JSON(map[string]any{"args": []string{"snapshot"}}), Execution{Dir: n.Config.WorkDir, Owner: "owner"})
			if err == nil || !json.Valid(model.JSON(value)) || hasDesktopCredential([]byte(err.Error())) || !utf8.ValidString(err.Error()) {
				t.Fatal("missing, malformed or unredacted failure diagnostics", err)
			}
			message := err.Error()
			for _, want := range []string{"never replay automatically", "sanitized result:", "[REDACTED]"} {
				if !strings.Contains(message, want) {
					t.Fatalf("failure omits %q: %s", want, message)
				}
			}
			if mode == "exit" && !strings.Contains(message, "exit code 7") || mode == "long-failure" && (!strings.Contains(message, "[truncated;") || len(message) > 4400) {
				t.Fatal("failure exit status or diagnostic bound lost", message)
			}
			if mode == "long-failure" {
				if strings.Count(string(model.JSON(value)), "界") != 3000 {
					t.Fatal("bounded error altered the full sanitized result")
				}
				spec := model.TaskSpec{ID: "browser-failure", Capability: "browser.run", Args: model.JSON(map[string]any{"args": []string{"snapshot"}})}
				if _, err := n.startTaskContext(t.Context(), "owner", spec); err != nil {
					t.Fatal(err)
				}
				finished, err := n.waitTask(t.Context(), "owner", spec.ID, 10*time.Second)
				if err != nil || finished.State != "failed" || strings.Count(string(finished.Result), "界") != 3000 || hasDesktopCredential(model.JSON(finished)) {
					t.Fatal("failed task lost its full sanitized result", finished, err)
				}
			}
		})
	}
}

func TestBrowserConvenienceFailureDiagnostics(t *testing.T) {
	n := browserNode(t, "batch")
	setTestSecret(t, n, "app.password", desktopCredential)
	api := httptest.NewServer(n.Handler())
	defer api.Close()
	c := client.Client{URL: api.URL, Token: n.Config.Token}
	request := map[string]any{"args": []string{"batch"}, "stdin": `[["snapshot"],["click","@e1"]]`}
	check := func(message string) {
		t.Helper()
		for _, want := range []string{"element not found", `"success":true`, `"success":false`, "[REDACTED]", "never replay automatically"} {
			if !strings.Contains(message, want) {
				t.Fatalf("convenience failure omits %q: %s", want, message)
			}
		}
		if hasDesktopCredential([]byte(message)) {
			t.Fatal("convenience failure leaked a credential")
		}
	}
	var value json.RawMessage
	if err := c.Call(t.Context(), "", "browser.run", request, &value); err != nil {
		check(err.Error())
	} else {
		t.Fatal("failed browser call succeeded")
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := c.MCPServer().Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "control_browser", Arguments: map[string]any{"node": n.Config.Name, "request": request}})
	if err != nil || !result.IsError || len(result.Content) != 1 {
		t.Fatal("failed browser MCP call did not report an error", result, err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatal("browser MCP error was not text")
	}
	var failure model.RPCError
	if json.Unmarshal([]byte(text.Text), &failure) != nil || failure.Outcome != "unknown" {
		t.Fatal("browser MCP failure lost its uncertain outcome", text.Text)
	}
	check(failure.Message)
}
