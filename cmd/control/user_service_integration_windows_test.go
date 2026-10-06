package main

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/installation"
	"golang.org/x/sys/windows"
)

// This test requires a logged-in Windows session and creates a temporary login
// task for that user. No account password or administrative token is needed.
func TestWindowsUserStartupLifecycle(t *testing.T) {
	if os.Getenv("CONTROL_TEST_WINDOWS_USER_STARTUP") != "1" {
		t.Skip("set CONTROL_TEST_WINDOWS_USER_STARTUP=1 in a logged-in Windows session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "user profile with spaces")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "control.exe")
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	const token = "windows-user-startup-test-credential"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	config := filepath.Join(dir, "node.json")
	if err = createNodeConfig(config, "user-startup-test", server.URL, token, address, false); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := installation.Service(cleanup, "uninstall", binary, config, "user"); err != nil {
			t.Errorf("remove login task: %v", err)
		}
	}()
	if err = installation.Service(ctx, "start", binary, config, "user"); err != nil {
		t.Fatal(err)
	}
	c := client.Client{URL: "http://" + address, Token: token}
	var before, after struct{ ID string }
	if err = c.Call(ctx, "", "node.describe", map[string]any{}, &before); err != nil {
		t.Fatal(err)
	}
	var result struct{ Stdout string }
	args := map[string]any{"command": "powershell.exe", "args": []string{"-NoProfile", "-NonInteractive", "-Command", "[Security.Principal.WindowsIdentity]::GetCurrent().User.Value"}}
	if err = c.Call(ctx, "", "exec.run", args, &result); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || strings.TrimSpace(result.Stdout) != user.User.Sid.String() {
		t.Fatalf("node did not execute as installing user: %q, %v", result.Stdout, err)
	}
	if err = installation.Service(ctx, "stop", binary, config, ""); err != nil {
		t.Fatal(err)
	}
	if err = installation.Service(ctx, "start", binary, config, ""); err != nil {
		t.Fatal(err)
	}
	if err = c.Call(ctx, "", "node.describe", map[string]any{}, &after); err != nil || before.ID != after.ID {
		t.Fatalf("identity changed after user restart: %v", err)
	}
	// Repeated start must preserve the already-running user supervisor.
	if err = installation.Service(ctx, "start", binary, config, ""); err != nil {
		t.Fatal(err)
	}
}
