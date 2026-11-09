package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport/transporttest"
)

func TestDesktopCLIProcess(t *testing.T) {
	i := slices.Index(os.Args, "--control-desktop-cli-fixture")
	if i < 0 {
		return
	}
	mode, args := os.Args[i+1], os.Args[i+2:]
	input, _ := io.ReadAll(os.Stdin)
	for _, item := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(item), "AGENT_DESKTOP_") && !strings.HasPrefix(item, "AGENT_DESKTOP_HOME=") {
			os.Exit(91)
		}
	}
	home := os.Getenv("AGENT_DESKTOP_HOME")
	if !filepath.IsAbs(home) {
		os.Exit(92)
	}
	fmt.Fprint(os.Stderr, desktopCredential[:8])
	fmt.Fprint(os.Stderr, desktopCredential[8:])
	response := map[string]any{"ok": true, "data": map[string]any{
		"args": args, "stdin": string(input), "home": home, "value": desktopCredential,
	}}
	exit := 0
	switch mode {
	case "native-failure":
		response = map[string]any{"ok": false, "error": map[string]any{"message": desktopCredential, "disposition": map[string]any{"delivery": "unknown", "retry": "unsafe"}}}
	case "exit-failure":
		exit = 7 // Even a claimed successful envelope must fail with this status.
	case "batch-failure":
		response = map[string]any{"ok": true, "command": "batch", "data": map[string]any{
			"results": []any{map[string]any{"ok": false, "error": map[string]any{"message": desktopCredential}}},
		}}
	case "batch-success":
		response = map[string]any{"ok": true, "command": "batch", "data": map[string]any{"results": []any{map[string]any{"ok": true}}}}
	case "array":
		response = map[string]any{"ok": true, "data": []any{map[string]any{"value": desktopCredential}}}
	case "help", "text-failure":
		_, _ = fmt.Fprint(os.Stdout, "usage ", desktopCredential)
		if mode == "text-failure" {
			os.Exit(2)
		}
		os.Exit(0)
	case "oversized":
		fmt.Fprint(os.Stderr, strings.Repeat("x", (2<<20)+1))
		os.Exit(0)
	case "binary":
		_, _ = os.Stdout.Write([]byte{0xff})
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Minute)
	case "screenshot":
		if err := os.WriteFile("screen.png", []byte("fake-image-bytes"), 0600); err != nil {
			os.Exit(93)
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(response)
	os.Exit(exit)
}

func desktopCLIArgs(mode string, args ...string) []string {
	return append([]string{"-test.run=^TestDesktopCLIProcess$", "--", "--control-desktop-cli-fixture", mode}, args...)
}

func desktopCLINode(t *testing.T) *Node {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	n := localNode(t, func(c *Config) { c.Desktop = &DesktopConfig{CLI: exe} })
	n.desktopLockPath = filepath.Join(t.TempDir(), "desktop.lock")
	return n
}

func TestDesktopCLIFullCommandSurface(t *testing.T) {
	t.Setenv("AGENT_DESKTOP_SESSION", "untrusted-session")
	t.Setenv("AGENT_DESKTOP_HOME", "untrusted-state")
	n := desktopCLINode(t)
	setTestSecret(t, n, "app.password", desktopCredential)
	for _, command := range []string{"list-apps", "list-windows", "snapshot", "open-system-surface", "batch", "screenshot", "clipboard-get", "trace", "skills", "future-upstream-command"} {
		t.Run(command, func(t *testing.T) {
			args := []string{command, "literal ; $(not-a-shell)", "--headed"}
			var log bytes.Buffer
			value, err := n.desktopRun(t.Context(), model.JSON(map[string]any{
				"command": "cli", "args": desktopCLIArgs("echo", args...), "stdin": "public input\n",
			}), Execution{Dir: n.Config.WorkDir, Log: &log})
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Data struct {
					Args  []string `json:"args"`
					Stdin string   `json:"stdin"`
					Home  string   `json:"home"`
				} `json:"data"`
			}
			encoded := model.JSON(value)
			if json.Unmarshal(encoded, &got) != nil || !slices.Equal(got.Data.Args, args) || got.Data.Stdin != "public input\n" || got.Data.Home != "[REDACTED]" {
				t.Fatal("CLI arguments, stdin, or isolated shared state changed", string(encoded))
			}
			if log.Len() != 0 || hasDesktopCredential(encoded) || !bytes.Contains(encoded, []byte("[REDACTED]")) {
				t.Fatal("CLI output or diagnostics exposed credentials")
			}
		})
	}
}

func TestDesktopCLIFailuresAndText(t *testing.T) {
	for _, mode := range []string{"native-failure", "exit-failure", "batch-failure", "batch-success", "array", "help", "text-failure", "oversized", "binary", "sleep"} {
		t.Run(mode, func(t *testing.T) {
			n := desktopCLINode(t)
			setTestSecret(t, n, "app.password", desktopCredential)
			ctx := t.Context()
			if mode == "sleep" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			var log bytes.Buffer
			value, err := n.desktopRun(ctx, model.JSON(map[string]any{"command": "cli", "args": desktopCLIArgs(mode)}), Execution{Dir: n.Config.WorkDir, Log: &log})
			if (err == nil) != (mode == "help" || mode == "batch-success" || mode == "array") || log.Len() != 0 || hasDesktopCredential(model.JSON(value)) || (err != nil && hasDesktopCredential([]byte(err.Error()))) {
				t.Fatal("CLI failure or text response violated outcome/privacy contract", err)
			}
			if mode == "native-failure" && !strings.Contains(string(model.JSON(value)), `"retry":"unsafe"`) {
				t.Fatal("CLI failed-action disposition was lost")
			}
		})
	}
}

func TestDesktopCLIBackendChecksAndLock(t *testing.T) {
	n := desktopCLINode(t)
	if _, err := n.desktopRun(t.Context(), model.JSON(map[string]any{"command": "status"}), Execution{}); err == nil {
		t.Fatal("CLI-only profile claimed native credential support")
	}
	native := desktopNode(t, "echo")
	if _, err := native.desktopRun(t.Context(), model.JSON(map[string]any{"command": "cli", "args": []string{"list-apps"}}), Execution{}); err == nil {
		t.Fatal("native-only profile ran an unconfigured CLI")
	}
	lock := flock.New(n.desktopLockPath)
	defer func() { _ = lock.Close() }()
	if held, err := lock.TryLock(); err != nil || !held {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := n.desktopRun(ctx, model.JSON(map[string]any{"command": "cli", "args": desktopCLIArgs("echo")}), Execution{}); err == nil {
		t.Fatal("CLI bypassed the desktop lock")
	}
}

func TestDesktopCLIBatchOutcome(t *testing.T) {
	for _, tc := range []struct {
		data string
		fail bool
	}{
		{`{"results":[{"ok":true}]}`, false},
		{`{"results":[{"ok":false}]}`, true},
		{`{"results":[{}]}`, true},
		{`{"results":[]}`, false},
		{`{"results":[],"not_started_entries":1}`, true},
		{`{"results":[],"stopped":{"reason":"deadline"}}`, true},
		{`{"results":[],"stopped":null}`, false},
		{`{}`, true},
		{`null`, true},
		{`"malformed"`, true},
	} {
		envelope := map[string]json.RawMessage{"command": model.JSON("batch"), "data": json.RawMessage(tc.data)}
		if desktopBatchFailed(envelope) != tc.fail {
			t.Errorf("batch outcome incorrectly classified: %s", tc.data)
		}
	}
}

func TestDesktopCLITaskArtifactsAndReconciliation(t *testing.T) {
	n := desktopCLINode(t)
	setTestSecret(t, n, "app.password", desktopCredential)
	spec := model.TaskSpec{ID: "cli-screenshot", Capability: "desktop.run", Outputs: []string{"screen.png"}, Args: model.JSON(map[string]any{"command": "cli", "args": desktopCLIArgs("screenshot")})}
	accepted, err := n.startTaskContext(t.Context(), "owner", spec)
	if err != nil {
		t.Fatal(err)
	}
	finished, err := n.waitTask(t.Context(), "owner", accepted.ID, 10*time.Second)
	if err != nil || finished.State != "succeeded" || len(finished.Artifacts) != 1 {
		t.Fatal("CLI task did not publish its declared binary output", finished, err)
	}
	for _, path := range []string{n.taskPath(spec.ID), n.logPath(spec.ID)} {
		data, err := os.ReadFile(path)
		if err != nil || hasDesktopCredential(data) || (path == n.logPath(spec.ID) && len(data) != 0) {
			t.Fatal("CLI task or logs retained raw credentials", err)
		}
	}
	repeated, err := n.startTaskContext(t.Context(), "owner", spec)
	if err != nil || repeated.Updated != finished.Updated {
		t.Fatal("CLI task was replayed on reconciliation")
	}
}

func TestDesktopCLIPeerAuthorizationAndDurability(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, other := cluster(t, relay, func(i int, cfg *Config) {
				if i == 1 {
					cfg.Desktop = &DesktopConfig{CLI: exe}
					cfg.Allow = map[string][]string{"source": {"desktop.run", "tasks.*", "leases.*"}}
				}
			}, transporttest.Loopback())
			worker.desktopLockPath = filepath.Join(t.TempDir(), "desktop.lock")
			setTestSecret(t, worker, "app.password", desktopCredential)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			args := model.JSON(map[string]any{"command": "cli", "args": desktopCLIArgs("echo", "list-apps")})
			if err := testCall(t, other, ctx, "worker", "desktop.run", args, nil); err == nil {
				t.Fatal("unauthorized caller accessed the full CLI")
			}
			var lease model.Lease
			call(t, source, "worker", "leases.acquire", map[string]any{}, &lease)
			if err := testCall(t, source, ctx, "worker", "desktop.run", args, nil); err == nil {
				t.Fatal("full CLI bypassed exclusive lease")
			}
			spec := model.TaskSpec{ID: "full-cli-once", Capability: "desktop.run", Args: args, LeaseID: lease.ID}
			var accepted model.Task
			call(t, source, "worker", "tasks.start", spec, &accepted)
			finished, err := testWaitTask(t, source, ctx, "worker", accepted.ID)
			if err != nil || finished.State != "succeeded" || hasDesktopCredential(model.JSON(finished)) {
				t.Fatal("full CLI task failed or leaked credentials", err)
			}
			call(t, source, "worker", "tasks.start", spec, &accepted)
			if accepted.Updated != finished.Updated {
				t.Fatal("CLI peer reconciliation replayed work")
			}
			call(t, source, "worker", "leases.release", map[string]string{"id": lease.ID}, nil)
		})
	}
}
