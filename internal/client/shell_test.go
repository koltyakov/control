package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/shellstream"
	"github.com/koltyakov/control/internal/transport"
	"github.com/koltyakov/control/internal/transport/transporttest"
)

func TestShellAcrossPeerTransports(t *testing.T) {
	shell := "bash"
	if runtime.GOOS == "windows" {
		shell = "cmd"
	} else if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			c, worker, other := shellFixture(t, ctx, relay)
			request := model.ShellRequest{Shell: shell, Term: "xterm", Cols: 80, Rows: 24}

			assertShellError(t, Client{}, ctx, "shell-worker", request, "account-client routing")
			assertShellError(t, c, ctx, "other-worker", request, "disabled")
			assertShellError(t, c, ctx, "shell-worker", model.ShellRequest{Shell: "/bin/bash", Cols: 80, Rows: 24}, "shell must be")
			if err := c.Call(ctx, "shell-worker", model.ShellOpenMethod, request, nil); err == nil {
				t.Fatal("opened shell through JSON call")
			}
			if conn, _, err := other.Peer.OpenRPC(ctx, worker.Identity.ID, model.ShellOpenMethod, request); err == nil {
				_ = conn.Close()
				t.Fatal("worker credentials opened a shell")
			}
			foreignGrant := model.Delegation{Subject: other.Identity.ID, Method: model.ShellOpenMethod, Params: model.JSON(request)}
			if err := c.Call(ctx, "shell-worker", "access.grant", foreignGrant, nil); err == nil {
				t.Fatal("delegated an unrestricted shell to a worker")
			}

			var lease model.Lease
			if err := c.Call(ctx, "shell-worker", "leases.acquire", map[string]any{}, &lease); err != nil {
				t.Fatal(err)
			}
			assertShellError(t, c, ctx, "shell-worker", request, "node is leased")
			if err := c.Call(ctx, "shell-worker", "leases.release", map[string]string{"id": lease.ID}, nil); err != nil {
				t.Fatal(err)
			}

			s, err := c.OpenShell(ctx, "shell-worker", request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			mode := "webrtc"
			if relay {
				mode = "relay"
			}
			info, err := c.Session(ctx)
			if err != nil || !slices.ContainsFunc(info.Sessions, func(session model.PeerSession) bool {
				return session.Peer == worker.Identity.ID && session.Lane == string(transport.InteractiveLane) && session.Mode == mode && session.Outgoing == 1
			}) {
				t.Fatalf("expected live %s shell on the interactive lane: %+v, %v", mode, info.Sessions, err)
			}
			if err := s.Resize(103, 41); err != nil {
				t.Fatal(err)
			}
			assertShellError(t, c, ctx, "shell-worker", request, "execution slots are busy")
			if err := c.Call(ctx, "shell-worker", "leases.acquire", map[string]any{}, nil); err == nil {
				t.Fatal("lease overlapped the interactive shell")
			}
			cmd, expected := "stty size\rprintf '\\x43ONTROL_READY\\n'\rexit 23\r", "CONTROL_READY"
			if shell == "cmd" {
				cmd, expected = "set CONTROL_PROBE=ready\recho CONTROL-%CONTROL_PROBE%\rexit 23\r", "CONTROL-ready"
			}
			if _, err := io.WriteString(s, cmd); err != nil {
				t.Fatal(err)
			}
			out, err := io.ReadAll(io.LimitReader(s, 256<<10))
			code, exited := s.ExitCode()
			if err != nil || !exited || code != 23 || !strings.Contains(string(out), expected) {
				t.Fatalf("shell output=%q exit=(%d,%v) err=%v", out, code, exited, err)
			}
			if shell == "bash" && !strings.Contains(string(out), "41 103") {
				t.Fatalf("resize not applied: %q", out)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			awaitShellSlot(t, ctx, c)

			// A disconnected idle session must release its execution slot.
			idle, err := c.OpenShell(ctx, "shell-worker", request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = idle.Close() }()
			if !relay {
				_ = idle.Close()
			} else {
				// The relay fixture uses an instruction-only worker. Revoking
				// its one stream grant must cancel the already-running PTY.
				var grants []model.Delegation
				if err := c.Call(ctx, "shell-worker", "access.list", nil, &grants); err != nil {
					t.Fatal(err)
				}
				if len(grants) != 1 || grants[0].Method != model.ShellOpenMethod {
					t.Fatalf("expected one live shell grant, got %+v", grants)
				}
				if err := c.Call(ctx, "shell-worker", "access.revoke", map[string]string{"id": grants[0].ID}, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, idle); err == nil {
					t.Fatal("revoked shell returned clean EOF")
				}
			}
			awaitShellSlot(t, ctx, c)
		})
	}
}

func TestShellWindowsPowerShell(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell requires Windows")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	c, _, _ := shellFixture(t, ctx, true)
	for _, name := range []string{"powershell", "pwsh"} {
		t.Run(name, func(t *testing.T) {
			if name == "pwsh" {
				if _, err := exec.LookPath("pwsh.exe"); err != nil {
					t.Skip("PowerShell 7 is not installed")
				}
			}
			s, err := c.OpenShell(ctx, "shell-worker", model.ShellRequest{Shell: name, Cols: 80, Rows: 24})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if _, err := io.WriteString(s, "[Console]::WriteLine('control-'+'powershell')\rexit 19\r"); err != nil {
				t.Fatal(err)
			}
			out, err := io.ReadAll(io.LimitReader(s, 256<<10))
			code, exited := s.ExitCode()
			if err != nil || !exited || code != 19 || !strings.Contains(string(out), "control-powershell") {
				t.Fatalf("shell output=%q exit=(%d,%v) err=%v", out, code, exited, err)
			}
		})
		awaitShellSlot(t, ctx, c)
	}
}

func shellFixture(t *testing.T, ctx context.Context, relay bool) (Client, *node.Node, *node.Node) {
	t.Helper()
	const common = "shell-test-worker-key-1234567890"
	const account = "shell-test-account-key-1234567890"
	g, err := gateway.New(t.TempDir(), common, gateway.Options{SuperuserKey: account})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	opts := transporttest.Loopback()
	worker := func(name string, enabled bool) *node.Node {
		cfg := node.Config{Name: name, Gateway: server.URL, Token: common, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: relay, MaxTasks: 1, ShellEnabled: enabled, InstructionOnly: relay && enabled, MetricsIntervalSeconds: -1}
		n, err := node.New(cfg, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = n.Close() })
		if err := n.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return n
	}
	w, other := worker("shell-worker", true), worker("other-worker", false)
	admin := Admin{URL: server.URL, Key: account}
	c := (Client{}).WithStandalone(ctx, StandaloneConfig{Gateway: admin, StateDir: t.TempDir(), RelayOnly: relay, AccountRouting: true, PeerOptions: []transport.Options{opts}})
	t.Cleanup(func() { _ = c.Close() })
	return c, w, other
}

func assertShellError(t *testing.T, c Client, ctx context.Context, worker string, request model.ShellRequest, want string) {
	t.Helper()
	s, err := c.OpenShell(ctx, worker, request)
	if s != nil {
		_ = s.Close()
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("opening %s: got %v, want %q", worker, err, want)
	}
}

func awaitShellSlot(t *testing.T, ctx context.Context, c Client) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var d struct {
			Concurrency model.NodeConcurrency `json:"concurrency"`
		}
		if err := c.Call(ctx, "shell-worker", "node.describe", map[string]string{"view": "concurrency"}, &d); err != nil {
			t.Fatal(err)
		}
		if d.Concurrency.Execution.InUse == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("shell did not release its execution slot")
		case <-ticker.C:
		}
	}
}

func TestShellMissingExitRecordIsAnInterruption(t *testing.T) {
	local, remote := net.Pipe()
	defer func() { _ = local.Close() }()
	go func() {
		defer func() { _ = remote.Close() }()
		var w shellstream.Writer
		_ = w.Write(remote, shellstream.Output, []byte("before disconnect"))
	}()
	s := &Shell{conn: local}
	got, err := io.ReadAll(s)
	if !errors.Is(err, io.ErrUnexpectedEOF) || string(got) != "before disconnect" {
		t.Fatalf("read shell = %q, %v", got, err)
	}
	if _, ok := s.ExitCode(); ok {
		t.Fatal("transport failure supplied a bogus exit status")
	}
}
