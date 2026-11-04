package main

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
)

var retirementPort atomic.Uint32

func retirementAPIAddress(t *testing.T) string {
	t.Helper()
	// The node starts in another process after this listener closes. Keep its
	// API port below the default ephemeral ranges so outbound sockets, including
	// readiness probes, cannot claim it before startup or during a restart.
	// The client package's tunnel fixtures use 10000..19999 concurrently.
	for range 10000 {
		port := 20000 + (retirementPort.Add(1)-1)%10000
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		address := listener.Addr().String()
		_ = listener.Close()
		return address
	}
	t.Fatal("no retirement test API port available outside the ephemeral range")
	return ""
}

func TestInstalledNodeUninstallsAfterOnlineOrOfflineUnregistration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// Reuse this executable's real CLI entry point instead of compiling a second
	// binary while go test is already building/running the race-enabled suite.
	t.Setenv("CONTROL_TEST_CLI_MAIN", "1")
	name := "control"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	built, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"online", "offline", "persisted"} {
		t.Run(mode, func(t *testing.T) {
			const token = "retirement-node-token-123456789"
			const adminKey = "retirement-superuser-key-123456789"
			g, err := gateway.New(t.TempDir(), token, gateway.Options{SuperuserKey: adminKey})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			server := httptest.NewServer(g.Handler())
			defer server.Close()
			home := t.TempDir()
			binary, config := filepath.Join(home, name), filepath.Join(home, "node.json")
			bytes, err := os.ReadFile(built)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(binary, bytes, 0700); err != nil {
				t.Fatal(err)
			}
			cfg := node.Config{Name: "retirement-" + mode, Token: token, Gateway: server.URL, Listen: retirementAPIAddress(t), DataDir: filepath.Join(home, "state"), WorkDir: filepath.Join(home, "work"), MetricsIntervalSeconds: -1}
			if err = store.Write(config, cfg); err != nil {
				t.Fatal(err)
			}
			key, err := identity.Load(cfg.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
				defer done()
				_ = installation.Service(cleanup, "uninstall", binary, config, "process")
			}()
			if err = installation.Service(ctx, "start", binary, config, "process"); err != nil {
				log, _ := os.ReadFile(filepath.Join(home, "node.log"))
				t.Fatalf("start installed node: %v\n%s", err, log)
			}
			admin := client.Admin{URL: server.URL, Key: adminKey}
			if mode != "online" {
				if err = installation.Service(ctx, "stop", binary, config, "process"); err != nil {
					t.Fatal(err)
				}
			}
			if err = admin.ManageMachine(ctx, key.ID, "unregister"); err != nil {
				t.Fatal(err)
			}
			if mode == "persisted" {
				if err = store.Write(filepath.Join(cfg.DataDir, "machine-state.json"), model.MachineState{Revision: 1, Unregistered: true}); err != nil {
					t.Fatal(err)
				}
				server.Close() // A locally saved retirement must work without the gateway.
			}
			if mode != "online" {
				cmd := exec.CommandContext(ctx, binary, "--token", "", "node", "--config", config)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("retired node reconnect: %v\n%s", err, output)
				}
			}
			deadline := time.NewTimer(15 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				if _, err := os.Stat(binary); os.IsNotExist(err) {
					break
				}
				select {
				case <-deadline.C:
					log, _ := os.ReadFile(filepath.Join(home, "node.log"))
					t.Fatalf("retired node did not uninstall\n%s", log)
				case <-tick.C:
				}
			}
			for _, path := range []string{config, filepath.Join(cfg.DataDir, "identity.key"), filepath.Join(cfg.DataDir, "machine-state.json")} {
				if _, err = os.Stat(path); err != nil {
					t.Fatal("uninstall removed retained profile/state", path, err)
				}
			}
		})
	}
}
