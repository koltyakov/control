package main

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/processutil"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// This test creates a real, temporary SCM service. Run from Administrator
// PowerShell with CONTROL_TEST_WINDOWS_SERVICE=1.
func TestWindowsServiceLifecycle(t *testing.T) {
	if os.Getenv("CONTROL_TEST_WINDOWS_SERVICE") != "1" {
		t.Skip("set CONTROL_TEST_WINDOWS_SERVICE=1 in Administrator PowerShell")
	}
	if err := installation.CheckServiceMode("auto"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "service with spaces")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "control.exe")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	const token = "windows-service-test-credential"
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
	if err = createNodeConfig(config, "service-test", server.URL, token, address, false); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := installation.Service(cleanup, "uninstall", binary, config, "auto"); err != nil {
			t.Errorf("remove test service: %v", err)
		}
	}()
	// Migrate a live detached supervisor, including its persisted identity.
	legacy := exec.CommandContext(ctx, binary, "--token", "", "node", "--config", config)
	processutil.HideWindow(legacy)
	if err = legacy.Start(); err != nil {
		t.Fatal(err)
	}
	legacyDone := make(chan error, 1)
	go func() { legacyDone <- legacy.Wait() }()
	c := client.Client{URL: "http://" + address, Token: token}
	var before struct{ ID string }
	waitWindowsNode(t, ctx, func() bool {
		return c.Call(ctx, "", "node.describe", map[string]any{}, &before) == nil
	})
	if err = installation.Service(ctx, "start", binary, config, "auto"); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-legacyDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("legacy supervisor survived migration")
	}
	m, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(installation.WindowsServiceName(config))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings, err := s.Config()
	if err != nil || settings.StartType != mgr.StartAutomatic || settings.ServiceStartName != `NT AUTHORITY\LocalService` {
		t.Fatalf("service settings: %+v, %v", settings, err)
	}
	status, err := s.Query()
	if err != nil || status.State != svc.Running {
		t.Fatalf("service status: %+v, %v", status, err)
	}
	process, err := os.FindProcess(int(status.ProcessId))
	if err != nil {
		t.Fatal(err)
	}
	if err = process.Kill(); err != nil {
		t.Fatal(err)
	}
	process.Release()
	waitWindowsNode(t, ctx, func() bool {
		after, err := s.Query()
		return err == nil && after.State == svc.Running && after.ProcessId != status.ProcessId &&
			installation.Service(ctx, "status", binary, config, "auto") == nil
	})
	var after struct{ ID string }
	if err = c.Call(ctx, "", "node.describe", map[string]any{}, &after); err != nil || before.ID != after.ID {
		t.Fatalf("identity changed after service recovery: %v", err)
	}
	if err = installation.Service(ctx, "stop", binary, config, "auto"); err != nil {
		t.Fatal(err)
	}
	status, err = s.Query()
	if err != nil || status.State != svc.Stopped {
		t.Fatalf("service did not stop: %+v, %v", status, err)
	}
	if err = installation.Service(ctx, "start", binary, config, "auto"); err != nil {
		t.Fatal(err)
	}
	// SCM cleanup must still work if the profile has already been removed.
	if err = os.Remove(config); err != nil {
		t.Fatal(err)
	}
}

func waitWindowsNode(t *testing.T, ctx context.Context, ready func() bool) {
	t.Helper()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-ctx.Done():
			t.Fatal("node did not become available:", ctx.Err())
		case <-ticker.C:
		}
	}
}
