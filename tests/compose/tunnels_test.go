//go:build compose

package compose_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

func TestPersistentTunnelCLIProcesses(t *testing.T) {
	ctx, c := environment(t)
	home := t.TempDir()
	t.Setenv("CONTROL_HOME", home)
	t.Setenv("CONTROL_TUNNEL_SERVICE_MODE", "process")
	// Use the fixture's required transport without enrolling an orchestrator node.
	if err := store.Write(filepath.Join(home, "node.json"), map[string]any{"relayOnly": os.Getenv("CONTROL_EXPECT_TRANSPORT") == "relay"}); err != nil {
		t.Fatal(err)
	}
	endpointPath := func() string {
		files, err := filepath.Glob(filepath.Join(home, "tunnels", "*", "endpoint.json"))
		if err != nil || len(files) != 1 {
			t.Fatalf("missing owned tunnel endpoint: %v %v", files, err)
		}
		return files[0]
	}
	stopService := func(path string) {
		var endpoint struct{ PID int }
		if err := store.Read(path, &endpoint); os.IsNotExist(err) {
			return
		} else if err != nil || endpoint.PID <= 1 {
			t.Errorf("invalid owned tunnel process: %v", err)
			return
		}
		process, err := os.FindProcess(endpoint.PID)
		if err == nil {
			err = process.Signal(syscall.SIGTERM)
			_ = process.Release()
		}
		if err != nil {
			t.Error(err)
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("owned tunnel service did not stop")
	}
	t.Cleanup(func() {
		files, _ := filepath.Glob(filepath.Join(home, "tunnels", "*", "endpoint.json"))
		for _, file := range files {
			stopService(file)
		}
	})
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		files, _ := filepath.Glob(filepath.Join(home, "tunnels", "*", "service.log"))
		for _, path := range files {
			file, err := os.Open(path)
			if err != nil {
				t.Log("tunnel service log:", err)
				continue
			}
			log, err := io.ReadAll(io.LimitReader(file, 16<<10))
			_ = file.Close()
			t.Logf("tunnel service log %s: %s (read error: %v)", path, log, err)
		}
	})
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := local.Addr().String()
	_ = local.Close()
	gatewayURL, err := url.Parse(os.Getenv("CONTROL_GATEWAY"))
	if err != nil {
		t.Fatal(err)
	}
	frontend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "vite-fixture") }))
	defer frontend.Close()
	const reverseListen = "127.0.0.1:49071"
	for _, args := range [][]string{
		{"tunnel", "start", "worker", gatewayURL.Host, "--listen", listen, "--id", "qa-forward"},
		{"tunnel", "start", "worker", frontend.Listener.Addr().String(), "--reverse", "--listen", reverseListen, "--id", "frontend-reverse"},
	} {
		var info client.PersistentTunnelInfo
		if err := json.Unmarshal(cli(t, ctx, args...), &info); err != nil || info.ID == "" || info.State != "starting" {
			t.Fatal("CLI failed durable acceptance", info, err)
		}
	}
	ready := func() {
		for {
			var infos []client.PersistentTunnelInfo
			if err := json.Unmarshal(cli(t, ctx, "tunnel", "list"), &infos); err != nil {
				t.Fatal(err)
			}
			if len(infos) == 2 && infos[0].State == "active" && infos[1].State == "active" {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("persistent listeners not ready", infos)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	verify := func() {
		// A forward routes through worker to the gateway fixture, without credentials.
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + listen + "/v1/auth")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatal("unexpected forwarded gateway response", resp.Status)
		}
		// The reverse listener is remote loopback, so reach it through a second stream.
		conn, err := c.Tunnel(ctx, "worker", reverseListen)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || string(body) != "vite-fixture" {
			t.Fatal("reverse frontend response", string(body), err)
		}
	}
	ready()
	verify()
	var dashboard model.PoolActivitySnapshot
	if err := json.Unmarshal(cli(t, ctx, "dashboard", "--json"), &dashboard); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, node := range dashboard.Nodes {
		if node.Name == "worker" {
			found = node.RetainedTunnels != nil && *node.RetainedTunnels == (model.TunnelCounts{Forward: 1, Reverse: 1})
		}
	}
	if !found {
		t.Fatal("CLI dashboard lost retained tunnel counts")
	}
	// The start commands have exited. Stop their detached owner, then use a new CLI
	// process to restore the definitions and the exact listener addresses.
	stopService(endpointPath())
	ready()
	verify()
	cli(t, ctx, "tunnel", "dispose", "qa-forward")
	cli(t, ctx, "tunnel", "dispose", "frontend-reverse")
	stopService(endpointPath())
	var retained []client.PersistentTunnelInfo
	if err := json.Unmarshal(cli(t, ctx, "tunnel", "list"), &retained); err != nil || len(retained) != 0 {
		t.Fatal("disposal did not survive process restart", retained, err)
	}
	t.Log("verified persistent CLI owner lifetime, forward/reverse traffic, restart, disposal, and dashboard counts")
}
