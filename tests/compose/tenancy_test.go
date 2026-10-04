//go:build compose

package compose_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

func TestInstalledFleetsArePrivate(t *testing.T) {
	ctx, legacy := environment(t)
	admin := client.Admin{URL: os.Getenv("CONTROL_GATEWAY"), Key: os.Getenv("CONTROL_SUPERUSER_KEY")}
	create := func(name string) client.Admin {
		t.Helper()
		var account struct {
			Token string
			User  struct{ ID string }
		}
		if err := json.Unmarshal(cli(t, ctx, "users", "create", name), &account); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := admin.JSON(cleanup, "DELETE", "/v1/admin/users/"+account.User.ID, nil, nil); err != nil {
				t.Error(err)
			}
		})
		return client.Admin{URL: admin.URL, Key: account.Token}
	}
	a, b := create("compose-alice"), create("compose-bob")
	install := func(owner client.Admin, name, script string) client.Client {
		t.Helper()
		home := t.TempDir()
		binary := filepath.Join(home, "bin", "control")
		config := filepath.Join(home, "node.json")
		var env []string
		for _, v := range os.Environ() {
			key, _, _ := strings.Cut(v, "=")
			if !strings.HasPrefix(key, "CONTROL_") && key != "HOME" {
				env = append(env, v)
			}
		}
		env = append(env, "HOME="+home, "CONTROL_HOME="+home, "CONTROL_INSTALL_DIR="+filepath.Dir(binary), "CONTROL_SERVICE_MODE=process")
		var cmd *exec.Cmd
		if script != "" {
			cmd = exec.CommandContext(ctx, "bash", "-c", script)
		} else {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			listen := listener.Addr().String()
			_ = listener.Close()
			cmd = exec.CommandContext(ctx, "control", "setup", "--gateway", owner.URL, "--name", name, "--listen", listen, "--service", "process")
		}
		cmd.Env = append(append([]string{}, env...), "CONTROL_USER_KEY="+owner.Key)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("user install: %v\n%s", err, out)
		}
		if os.Getenv("CONTROL_EXPECT_TRANSPORT") == "relay" {
			cmd = exec.CommandContext(ctx, binary, "service", "stop")
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("stop for relay configuration: %v %s", err, out)
			}
			var saved map[string]any
			if err := store.Read(config, &saved); err != nil {
				t.Fatal(err)
			}
			saved["relayOnly"] = true
			if err := store.Write(config, saved); err != nil {
				t.Fatal(err)
			}
			cmd = exec.CommandContext(ctx, binary, "service", "start", "--mode", "process")
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("start relay peer: %v %s", err, out)
			}
		}
		var cfg struct{ Token, Listen string }
		data, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		c := client.Client{URL: "http://" + cfg.Listen, Token: cfg.Token}
		var description struct{ ID string }
		if err = c.Call(ctx, "", "node.describe", map[string]any{}, &description); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cleanup, binary, "service", "uninstall")
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("stop installed fleet peer: %v %s", err, out)
				return
			}
			for cleanup.Err() == nil {
				if owner.JSON(cleanup, "DELETE", "/v1/fleet/nodes/"+description.ID, nil, nil) == nil {
					return
				}
				select {
				case <-cleanup.Done():
				case <-time.After(50 * time.Millisecond):
				}
			}
			t.Error("fleet peer cleanup failed")
		})
		if script == "" {
			// Exercise the saved user key, with no account or gateway environment.
			cmd = exec.CommandContext(ctx, binary, "machines", "invites")
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("saved user login: %v %s", err, out)
			}
		}
		return c
	}
	mainA := install(a, "main", "")
	otherA := install(a, "second-agent", "")
	mainB := install(b, "main", "")
	request := enrollment.Request{Name: "private-worker", OS: runtime.GOOS, Arch: runtime.GOARCH}
	link, err := a.Invite(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	otherLink, err := b.Invite(ctx, request)
	if err != nil {
		t.Fatal("machine name collided across users", err)
	}
	worker := install(a, "private-worker", link.Command)
	var description struct{ ID string }
	if err = worker.Call(ctx, "", "node.describe", map[string]any{}, &description); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []client.Client{mainA, otherA} {
		if err = agent.Call(ctx, "private-worker", "exec.run", map[string]any{"command": "printf", "args": []string{"private-fleet"}}, nil); err != nil {
			t.Fatal("same user's host could not use its worker", err)
		}
		var observed struct{ Connections map[string]string }
		if err = agent.Call(ctx, "", "node.describe", map[string]any{}, &observed); err != nil {
			t.Fatal(err)
		}
		if mode := observed.Connections[description.ID]; mode != os.Getenv("CONTROL_EXPECT_TRANSPORT") {
			t.Fatalf("fleet transport = %q", mode)
		}
	}
	for _, outsider := range []client.Client{mainB, legacy} {
		var nodes []model.Node
		if err = outsider.Call(ctx, "", "nodes.list", map[string]any{}, &nodes); err != nil {
			t.Fatal(err)
		}
		for _, n := range nodes {
			if n.ID == description.ID {
				t.Fatal("foreign machine in directory")
			}
		}
		for _, method := range []string{"node.describe", "exec.run", "tasks.list", "artifacts.list", "activities.list"} {
			if err = outsider.Call(ctx, description.ID, method, map[string]any{}, nil); err == nil {
				t.Fatal("cross-user access", method)
			}
		}
	}
	if err = a.JSON(ctx, "DELETE", "/v1/fleet/installations/"+otherLink.ID, nil, nil); err == nil {
		t.Fatal("user revoked another user's invite")
	}
	if err = b.JSON(ctx, "DELETE", "/v1/fleet/installations/"+otherLink.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Log("separate user accounts, duplicate names, saved account credentials, installed worker, multiple host agents, and cross-user denial verified")
}
