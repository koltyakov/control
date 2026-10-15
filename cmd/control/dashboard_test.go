package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
)

func TestDashboardGatewayCredentialsFollowSelectedProfile(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	for _, name := range []string{"CONTROL_GATEWAY", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY", "CONTROL_TOKEN"} {
		t.Setenv(name, "")
	}
	if err := installation.SaveAdmin(installation.AdminProfile{Gateway: "https://first.example", Key: "first-key"}); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(t.TempDir(), "node.json")
	if err := store.Write(profile, node.Config{Name: "host", Gateway: "https://second.example", Token: "second-key"}); err != nil {
		t.Fatal(err)
	}
	check := func(want client.Admin, override *string) {
		t.Helper()
		got, err := gatewayClient(profile, override)
		if err != nil || got != want {
			t.Fatalf("got %+v %v, want %+v", got, err, want)
		}
	}
	check(client.Admin{URL: "https://second.example", Key: "second-key"}, nil)
	t.Setenv("CONTROL_GATEWAY", "https://third.example")
	check(client.Admin{URL: "https://third.example"}, nil)
	t.Setenv("CONTROL_GATEWAY", "https://first.example/")
	check(client.Admin{URL: "https://first.example", Key: "first-key"}, nil)
	t.Setenv("CONTROL_USER_KEY", "env-key")
	check(client.Admin{URL: "https://first.example", Key: "env-key"}, nil)
	explicit := "explicit-key"
	check(client.Admin{URL: "https://first.example", Key: explicit}, &explicit)
}

func TestDashboardLogsDoNotWriteIntoLiveTerminal(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})).With("command", "dashboard")
	slog.SetDefault(logger)

	var peerLogger *slog.Logger
	func() {
		restore := suspendDashboardLogs(true)
		defer restore()
		// Transport peers keep this logger for later reconnects and shutdown.
		peerLogger = slog.Default()
		for range 100 {
			slog.Info("network changed; reconnecting gateway and retiring peer sessions")
			peerLogger.Debug("gateway reconnect failed")
			peerLogger.Warn("gateway reconnect failed")
			peerLogger.Info("gateway reconnected")
			peerLogger.Error("peer failed")
		}
	}()
	peerLogger.Info("peer stopped")
	if output.Len() != 0 {
		t.Fatalf("background logs reached the live terminal: %s", &output)
	}
	if slog.Default() != logger {
		t.Fatal("dashboard did not restore the previous logger")
	}
	slog.Debug("after dashboard")
	if !strings.Contains(output.String(), "after dashboard") || !strings.Contains(output.String(), "command=dashboard") {
		t.Fatalf("restored logger lost its level or attributes: %s", &output)
	}
}

func TestDashboardPreservesRedirectedLogs(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)
	restore := suspendDashboardLogs(false)
	defer restore()
	if slog.Default() != logger {
		t.Fatal("dashboard replaced redirected logging")
	}
	slog.Debug("gateway reconnect failed")
	if !strings.Contains(output.String(), "gateway reconnect failed") {
		t.Fatalf("redirected diagnostics were lost: %s", &output)
	}
}
