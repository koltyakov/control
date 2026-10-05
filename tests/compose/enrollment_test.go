//go:build compose

package compose_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/model"
)

func TestOneTimeInstallerRegistersAvailableMachine(t *testing.T) {
	ctx, c := environment(t)
	admin := client.Admin{URL: os.Getenv("CONTROL_GATEWAY"), Key: os.Getenv("CONTROL_SUPERUSER_KEY")}
	var link enrollment.Link
	output := cli(t, ctx, "machines", "add", "installed-worker", "--platform", runtime.GOOS+"/"+runtime.GOARCH, "--ttl", "5m", "--json")
	if err := json.Unmarshal(output, &link); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	cfg := filepath.Join(home, "node.json")
	var environment []string
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "CONTROL_HOME=") && !strings.HasPrefix(v, "CONTROL_INSTALL_DIR=") && !strings.HasPrefix(v, "CONTROL_SERVICE_MODE=") && !strings.HasPrefix(v, "CONTROL_CONFIG=") {
			environment = append(environment, v)
		}
	}
	environment = append(environment, "CONTROL_HOME="+home, "CONTROL_INSTALL_DIR="+bin, "CONTROL_SERVICE_MODE=process")
	installed := filepath.Join(bin, "control")
	var installedID string
	unregistered := false
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		cmd := exec.CommandContext(cleanup, installed, "--config", cfg, "service", "uninstall")
		cmd.Env = environment
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("cleanup installed node: %v %s", err, b)
			return
		}
		if unregistered {
			return
		}
		for cleanup.Err() == nil {
			if err := admin.JSON(cleanup, "DELETE", "/v1/admin/nodes/"+installedID, nil, nil); err == nil {
				return
			}
			select {
			case <-cleanup.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Error("could not remove installed fixture registration")
	})
	cmd := exec.CommandContext(ctx, "bash", "-c", link.Command)
	cmd.Env = environment
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated installer: %v\n%s", err, b)
	}
	var pool []model.Node
	call(t, ctx, c, "", "nodes.list", map[string]any{}, &pool)
	for _, n := range pool {
		if n.Name == link.Name && n.Online {
			installedID = n.ID
		}
	}
	if installedID == "" {
		t.Fatal("installer returned before registration was available")
	}
	call(t, ctx, c, link.Name, "exec.run", map[string]any{"command": "printf", "args": []string{"enrolled"}}, nil)
	resp, err := http.Get(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatal("used installer still available")
	}
	// Common node credentials must not be able to publish another installer.
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var config struct{ Token, DataDir string }
	if err = json.Unmarshal(b, &config); err != nil {
		t.Fatal(err)
	}
	if _, err = (client.Admin{URL: admin.URL, Key: config.Token}).Invite(ctx, enrollment.Request{Name: "forbidden", OS: runtime.GOOS, Arch: runtime.GOARCH}); err == nil {
		t.Fatal("installed common credential created another invitation")
	}
	for _, name := range []string{link.Name, "renamed-worker"} {
		previousName, previousToken := link.Name, config.Token
		link, err = admin.Invite(ctx, enrollment.Request{Name: name, OS: runtime.GOOS, Arch: runtime.GOARCH})
		if err != nil {
			t.Fatal(err)
		}
		cmd = exec.CommandContext(ctx, "bash", "-c", link.Command)
		cmd.Env = environment
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("replacement installer: %v\n%s", err, output)
		}
		if err := (client.Admin{URL: admin.URL, Key: previousToken}).JSON(ctx, "GET", "/v1/auth", nil, nil); err == nil {
			t.Fatal("previous enrollment credential still authenticates")
		}
		b, err = os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(b, &config); err != nil {
			t.Fatal(err)
		}
		call(t, ctx, c, "", "nodes.list", map[string]any{}, &pool)
		matches := 0
		for _, n := range pool {
			if n.ID == installedID {
				matches++
				if n.Name != name || !n.Online {
					t.Fatalf("replacement registration: %+v", n)
				}
			} else if n.Name == previousName || n.Name == name {
				t.Fatalf("duplicate machine after replacement: %+v", n)
			}
		}
		if matches != 1 {
			t.Fatalf("replacement has %d entries for original identity", matches)
		}
		call(t, ctx, c, name, "exec.run", map[string]any{"command": "printf", "args": []string{"replaced"}}, nil)
	}
	waitUpdate(t, ctx, func() bool {
		n, err := admin.ResolveMachine(ctx, link.Name)
		return err == nil && n.Managed
	})
	cli(t, ctx, "machines", "disable", link.Name)
	waitUpdate(t, ctx, func() bool {
		n, err := admin.ResolveMachine(ctx, link.Name)
		return err == nil && n.Disabled && !n.ControlPending
	})
	if err := c.Call(ctx, link.Name, "exec.run", map[string]any{"command": "true"}, nil); err == nil {
		t.Fatal("disabled installed agent accepted work")
	}
	cli(t, ctx, "machines", "enable", link.Name)
	waitUpdate(t, ctx, func() bool {
		n, err := admin.ResolveMachine(ctx, link.Name)
		return err == nil && !n.Disabled && !n.ControlPending
	})
	cli(t, ctx, "machines", "unregister", link.Name)
	unregistered = true
	dataDir := config.DataDir
	if !filepath.IsAbs(dataDir) {
		dataDir = filepath.Join(home, dataDir)
	}
	lock := flock.New(filepath.Join(dataDir, "runtime.lock"))
	defer func() { _ = lock.Close() }()
	waitUpdate(t, ctx, func() bool {
		held, err := lock.TryLock()
		if err != nil {
			t.Fatal(err)
		}
		return held
	})
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ResolveMachine(ctx, link.Name); err == nil {
		t.Fatal("unregistered process returned to the fleet")
	}
	previousID, previousDataDir := installedID, config.DataDir
	link, err = admin.Invite(ctx, enrollment.Request{Name: link.Name, OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.CommandContext(ctx, "bash", "-c", link.Command)
	cmd.Env = environment
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("installer after unregister: %v\n%s", err, output)
	}
	replacement, err := admin.ResolveMachine(ctx, link.Name)
	if err != nil {
		t.Fatal(err)
	}
	installedID, unregistered = replacement.ID, false
	if installedID == previousID || !replacement.Online {
		t.Fatal("installer reused retired identity or failed to restart", replacement)
	}
	b, err = os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &config); err != nil || config.DataDir == previousDataDir {
		t.Fatal("installer reused retired state", err)
	}
	if _, err = os.Stat(filepath.Join(previousDataDir, "identity.key")); err != nil {
		t.Fatal("installer removed previous state", err)
	}
	call(t, ctx, c, link.Name, "exec.run", map[string]any{"command": "printf", "args": []string{"reinstalled"}}, nil)
	t.Log("disable blocked execution, enable restored admission, and unregister stopped the installed supervisor")
	t.Log("a new invitation reinstalled the retired agent using fresh identity state and the existing profile")
	t.Log("copied Bash command installed a verified binary, redeemed one identity, started its node, and made it available for remote work")
}

func TestHostSetupInstallsAgentAndRemembersGateway(t *testing.T) {
	ctx, c := environment(t)
	home := t.TempDir()
	binary := filepath.Join(home, "bin", "control")
	config := filepath.Join(home, "node.json")
	var clean []string
	for _, v := range os.Environ() {
		name, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(name, "CONTROL_") || name == "HOME" {
			continue
		}
		clean = append(clean, v)
	}
	clean = append(clean, "HOME="+home, "CONTROL_HOME="+home, "CONTROL_INSTALL_DIR="+filepath.Dir(binary), "CONTROL_SERVICE_MODE=process")
	cmd := exec.CommandContext(ctx, "control", "login", "--gateway", os.Getenv("CONTROL_GATEWAY"), "--key-stdin")
	cmd.Env = clean
	cmd.Stdin = strings.NewReader(os.Getenv("CONTROL_SUPERUSER_KEY"))
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("host login: %v\n%s", err, b)
	}
	cmd = exec.CommandContext(ctx, "control", "setup", "--name", "installed-host", "--client", "agents", "--service", "process", "--listen", "127.0.0.1:7332")
	cmd.Env = clean
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("host setup: %v\n%s", err, b)
	}
	admin := client.Admin{URL: os.Getenv("CONTROL_GATEWAY"), Key: os.Getenv("CONTROL_SUPERUSER_KEY")}
	var description struct{ ID string }
	call(t, ctx, c, "installed-host", "node.describe", map[string]any{}, &description)
	if err := c.Call(ctx, "installed-host", "exec.run", map[string]any{"command": "true"}, nil); err == nil {
		t.Fatal("a remote worker gained execution access to the host's administrative account")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cleanup, binary, "service", "uninstall")
		cmd.Env = clean
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("host cleanup: %v %s", err, b)
			return
		}
		for cleanup.Err() == nil {
			if admin.JSON(cleanup, "DELETE", "/v1/admin/nodes/"+description.ID, nil, nil) == nil {
				return
			}
			select {
			case <-cleanup.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Error("host fixture registration was not removed")
	})
	cmd = exec.CommandContext(ctx, binary, "machines", "add", "host-invited", "--platform", runtime.GOOS+"/"+runtime.GOARCH, "--json")
	cmd.Env = clean
	output, err := cmd.Output()
	if err != nil {
		t.Fatal("saved host could not issue invitation without credential environment", err)
	}
	var link enrollment.Link
	if err = json.Unmarshal(output, &link); err != nil {
		t.Fatal(err)
	}
	if err = admin.JSON(ctx, "DELETE", "/v1/admin/installations/"+link.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	cmd = exec.CommandContext(ctx, binary, "machines")
	cmd.Env = clean
	if output, err = cmd.Output(); err != nil || !strings.Contains(string(output), "installed-host") {
		t.Fatalf("saved local configuration not used: %v %s", err, output)
	}
	mcp, err := os.ReadFile(filepath.Join(home, ".agents", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mcp), config) || strings.Contains(string(mcp), os.Getenv("CONTROL_SUPERUSER_KEY")) {
		t.Fatal("MCP config missing profile or contains administrator secret")
	}
	if _, err = os.Stat(filepath.Join(home, ".agents", "skills", "control", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	t.Log("host setup started a node, installed MCP and skill, and issued invitations using saved credentials")
}
