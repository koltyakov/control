package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/model"
)

const desktopCredential = "private-native-\"password-秘密"

func hasDesktopCredential(data []byte) bool {
	escaped, _ := json.Marshal(desktopCredential)
	for _, value := range []string{desktopCredential, string(escaped[1 : len(escaped)-1]), base64.StdEncoding.EncodeToString([]byte(desktopCredential))} {
		if bytes.Contains(data, []byte(value)) {
			return true
		}
	}
	return false
}

func TestDesktopHelperProcess(t *testing.T) {
	mode := os.Getenv("CONTROL_TEST_DESKTOP")
	if mode == "" {
		return
	}
	for _, item := range append(os.Args, os.Environ()...) {
		if strings.Contains(item, desktopCredential) || strings.HasPrefix(strings.ToUpper(item), "AGENT_DESKTOP_") {
			os.Exit(3)
		}
	}
	magic := make([]byte, 8)
	if _, err := io.ReadFull(os.Stdin, magic); err != nil || string(magic) != "CDSK0001" {
		os.Exit(4)
	}
	var fields []string
	for range 5 {
		var size uint32
		if binary.Read(os.Stdin, binary.BigEndian, &size) != nil || size > 32768 {
			os.Exit(5)
		}
		b := make([]byte, size)
		if _, err := io.ReadFull(os.Stdin, b); err != nil {
			os.Exit(6)
		}
		fields = append(fields, string(b))
	}
	var depth uint32
	if binary.Read(os.Stdin, binary.BigEndian, &depth) != nil || depth < 1 || depth > 32 {
		os.Exit(7)
	}
	if fields[0] == "setValue" && fields[3] != desktopCredential {
		os.Exit(8)
	}
	// Split diagnostics across writes to catch unsafe streaming redaction.
	fmt.Fprint(os.Stderr, desktopCredential[:8])
	fmt.Fprint(os.Stderr, desktopCredential[8:])
	response := map[string]any{"ok": true, "data": map[string]any{
		"value":           desktopCredential,
		"encoded":         base64.StdEncoding.EncodeToString([]byte(desktopCredential)),
		desktopCredential: []string{desktopCredential},
		"state_root":      fields[4], "platform": "test", "version": "1.0.2",
		"permissions": map[string]any{"note": desktopCredential},
	}}
	switch mode {
	case "partial-snapshot":
		response = map[string]any{"ok": true, "command": "snapshot", "data": map[string]any{
			"complete": false, "truncated": true, "nodes_observed": 1, "snapshot_id": "stest",
			"tree": map[string]any{"name": desktopCredential, "subtree_truncated": true},
		}}
	case "fail":
		response = map[string]any{"ok": false, "error": map[string]any{"message": desktopCredential, "disposition": map[string]any{"delivery": "unknown", "retry": "unsafe"}}}
	case "exit":
		_, _ = fmt.Fprint(os.Stdout, desktopCredential)
		os.Exit(9)
	case "malformed":
		_, _ = fmt.Fprint(os.Stdout, `{"ok":true,"data":"`+desktopCredential)
		os.Exit(0)
	case "oversized":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", 2<<20), desktopCredential)
		os.Exit(0)
	case "bad-outcome":
		response = map[string]any{"ok": desktopCredential}
	case "bad-payload":
		response = map[string]any{"ok": true, "data": desktopCredential}
	case "sleep":
		time.Sleep(time.Minute)
	}
	_ = json.NewEncoder(os.Stdout).Encode(response)
	os.Exit(0)
}

func desktopCommand(t *testing.T, mode string) *DesktopConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &DesktopConfig{Command: Command{Command: exe, Args: []string{"-test.run=^TestDesktopHelperProcess$"}, Env: map[string]string{
		"CONTROL_TEST_DESKTOP": mode, "AGENT_DESKTOP_SESSION": "must-not-inherit-tracing", "AGENT_DESKTOP_HOME": "must-not-inherit-path",
	}}}
}

func desktopNode(t *testing.T, mode string) *Node {
	t.Helper()
	n := localNode(t, func(cfg *Config) { cfg.Desktop = desktopCommand(t, mode) })
	n.rpaLockPath = filepath.Join(t.TempDir(), "desktop.lock")
	return n
}

func TestDesktopOptInAndPrivateState(t *testing.T) {
	plain := localNode(t, nil)
	if _, exists := plain.providers["desktop.run"]; exists {
		t.Fatal("native desktop enabled by default")
	}
	for _, setup := range []func(*Config){
		func(c *Config) { c.Desktop = &DesktopConfig{} },
		func(c *Config) { c.Desktop = &DesktopConfig{Library: "relative-library"} },
		func(c *Config) {
			c.Desktop = &DesktopConfig{Library: filepath.Join(t.TempDir(), "library"), Command: Command{Args: []string{"unexpected"}}}
		},
		func(c *Config) {
			c.Desktop = &DesktopConfig{Library: filepath.Join(t.TempDir(), "library"), Command: Command{Env: map[string]string{"KEY": "value"}}}
		},
		func(c *Config) {
			c.Desktop = &DesktopConfig{Library: filepath.Join(t.TempDir(), "library"), Command: Command{Dir: t.TempDir()}}
		},
		func(c *Config) {
			c.Desktop = desktopCommand(t, "echo")
			c.Desktop.Library = filepath.Join(t.TempDir(), "library")
		},
		func(c *Config) { c.Desktop = desktopCommand(t, "echo"); c.DataDir = filepath.Join(c.WorkDir, "state") },
	} {
		cfg := Config{Name: "bad", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir()}
		setup(&cfg)
		if n, err := New(cfg); err == nil {
			_ = n.Close()
			t.Fatal("accepted unsafe native desktop configuration")
		}
	}
	// Opting in advertises the capability without loading the library in the node.
	library := filepath.Join(t.TempDir(), "not-installed-library")
	n := localNode(t, func(c *Config) { c.Desktop = &DesktopConfig{Library: library} })
	if _, exists := n.providers["desktop.run"]; !exists {
		t.Fatal("library mode did not register desktop.run")
	}
	encoded, err := json.Marshal(n.Config.Desktop)
	var decoded DesktopConfig
	if err != nil || json.Unmarshal(encoded, &decoded) != nil || decoded.Library != library || decoded.Command.Command != "" {
		t.Fatal("library configuration did not round-trip")
	}
}

func TestDesktopRejectsSymlinkState(t *testing.T) {
	n := desktopNode(t, "echo")
	home := filepath.Join(n.Config.DataDir, "desktop")
	if err := os.Remove(home); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(n.Config.WorkDir, home); err != nil {
		t.Skip("directory symlink unavailable on this host")
	}
	if _, err := n.desktopStateDir(); err == nil {
		t.Fatal("native state was allowed to expose the workspace")
	}
}

func TestDesktopSuppressesLogsWithoutSecrets(t *testing.T) {
	n := desktopNode(t, "echo")
	var log bytes.Buffer
	if _, err := n.desktopRun(context.Background(), json.RawMessage(`{"command":"status"}`), Execution{Dir: n.Config.WorkDir, Log: &log}); err != nil || log.Len() != 0 {
		t.Fatal("raw native logs were retained without a secret input")
	}
}

func TestDesktopSanitizesEveryResponse(t *testing.T) {
	for _, command := range []string{"setValue", "snapshot", "status"} {
		t.Run(command, func(t *testing.T) {
			n := desktopNode(t, "echo")
			setTestSecret(t, n, "app.password", desktopCredential)
			args := map[string]any{"command": command}
			switch command {
			case "setValue":
				args["ref"], args["secret"] = "@s123:e1", "app.password"
			case "snapshot":
				args["app"] = "Test login"
			}
			var log bytes.Buffer
			value, err := n.desktopRun(context.Background(), model.JSON(args), Execution{Dir: n.Config.WorkDir, Log: &log})
			encoded := string(model.JSON(value))
			if err != nil || log.Len() != 0 || hasDesktopCredential(model.JSON(value)) || !strings.Contains(encoded, "[REDACTED]") {
				t.Fatal("response, error, or raw logs violated credential protection")
			}
			if command == "status" && (strings.Contains(encoded, "state_root") || strings.Contains(encoded, n.Config.DataDir)) {
				t.Fatal("status exposed private state")
			}
		})
	}
}

func TestDesktopPreservesPartialSnapshot(t *testing.T) {
	n := desktopNode(t, "partial-snapshot")
	setTestSecret(t, n, "app.password", desktopCredential)
	var log bytes.Buffer
	value, err := n.desktopRun(context.Background(), json.RawMessage(`{"command":"snapshot","app":"Test login"}`), Execution{Dir: n.Config.WorkDir, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	encoded := model.JSON(value)
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Complete      *bool  `json:"complete"`
			Truncated     bool   `json:"truncated"`
			NodesObserved int    `json:"nodes_observed"`
			SnapshotID    string `json:"snapshot_id"`
			Tree          struct {
				Name             string `json:"name"`
				SubtreeTruncated bool   `json:"subtree_truncated"`
			} `json:"tree"`
		} `json:"data"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	data := envelope.Data
	if !envelope.OK || data.Complete == nil || *data.Complete || !data.Truncated || data.NodesObserved != 1 || data.SnapshotID != "stest" || !data.Tree.SubtreeTruncated {
		t.Fatal("partial snapshot metadata was lost or treated as a complete observation")
	}
	if hasDesktopCredential(encoded) || data.Tree.Name != "[REDACTED]" || log.Len() != 0 {
		t.Fatal("partial snapshot exposed credentials or native logs")
	}
}

func TestDesktopFailuresDoNotLeak(t *testing.T) {
	for _, mode := range []string{"fail", "exit", "malformed", "oversized", "bad-outcome", "bad-payload", "sleep"} {
		t.Run(mode, func(t *testing.T) {
			n := desktopNode(t, mode)
			setTestSecret(t, n, "app.password", desktopCredential)
			// A race-instrumented helper waits briefly during process exit.
			// Only the cancellation case should kill an otherwise valid response.
			timeout := 5 * time.Second
			if mode == "sleep" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			var log bytes.Buffer
			value, err := n.desktopRun(ctx, json.RawMessage(`{"command":"setValue","ref":"@s123:e1","secret":"app.password"}`), Execution{Dir: n.Config.WorkDir, Log: &log})
			if err == nil || hasDesktopCredential([]byte(err.Error())) || hasDesktopCredential(model.JSON(value)) || log.Len() != 0 {
				t.Fatal("failure exposed a credential or was treated as success")
			}
			if mode == "fail" && !strings.Contains(string(model.JSON(value)), "[REDACTED]") {
				t.Fatal("sanitized partial failure was not retained")
			}
		})
	}
}

func TestDesktopMissingSecretAndLockCancellation(t *testing.T) {
	n := desktopNode(t, "echo")
	lock := flock.New(n.rpaLockPath)
	defer func() { _ = lock.Close() }()
	if ok, err := lock.TryLock(); !ok || err != nil {
		t.Fatal(err)
	}
	// Missing references and disallowed commands fail before waiting or launch.
	for _, args := range []string{`{"command":"setValue","ref":"@s123:e1","secret":"missing"}`, `{"command":"screenshot"}`} {
		if _, err := n.desktopRun(context.Background(), json.RawMessage(args), Execution{}); err == nil {
			t.Fatal("accepted missing secret or unsupported command")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := n.desktopRun(ctx, json.RawMessage(`{"command":"status"}`), Execution{}); err == nil {
		t.Fatal("cancelled lock waiter executed")
	}
}

func TestDesktopTaskRetainsOnlyReferences(t *testing.T) {
	n := desktopNode(t, "echo")
	setTestSecret(t, n, "app.password", desktopCredential)
	spec := model.TaskSpec{ID: "native-secret", Capability: "desktop.run", Args: json.RawMessage(`{"command":"setValue","ref":"@s123:e1","secret":"app.password"}`)}
	accepted, err := n.startTaskContext(context.Background(), "owner", spec)
	if err != nil {
		t.Fatal(err)
	}
	value, err := n.taskMethod(context.Background(), "owner", "tasks.get", model.JSON(map[string]any{"id": accepted.ID, "waitSeconds": 10}))
	if err != nil {
		t.Fatal(err)
	}
	finished := value.(model.Task)
	if finished.State != "succeeded" || !bytes.Equal(finished.Spec.Args, spec.Args) {
		t.Fatal("task failed or reference arguments changed")
	}
	for _, path := range []string{n.taskPath(spec.ID), n.logPath(spec.ID)} {
		data, err := os.ReadFile(path)
		if err != nil || hasDesktopCredential(data) {
			t.Fatal("retained task or logs exposed a credential")
		}
		if path == n.logPath(spec.ID) && len(data) != 0 {
			t.Fatal("raw native helper logs were retained")
		}
	}
	repeated, err := n.startTaskContext(context.Background(), "owner", spec)
	if err != nil || repeated.Updated != finished.Updated {
		t.Fatal("task reconciliation replayed credential input")
	}
}
