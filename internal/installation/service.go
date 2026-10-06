package installation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/processutil"
)

// ServiceMode resolves the saved startup selection without changing the profile.
func ServiceMode(config, mode string) (string, error) {
	config, err := filepath.Abs(config)
	if err != nil {
		return "", err
	}
	mode, err = resolveServiceMode(config, mode)
	if err != nil {
		return "", err
	}
	return mode, ValidateServiceMode(mode)
}

// CheckServiceProfile validates installation privileges before credentials are
// issued or an invitation is redeemed, including system-to-user migration.
func CheckServiceProfile(config, mode string) error {
	if err := CheckServiceMode(mode); err != nil {
		return err
	}
	config, err := filepath.Abs(config)
	if err != nil {
		return err
	}
	return checkServiceProfile(config, mode)
}

func Service(ctx context.Context, operation, binary, config, mode string) error {
	if err := ValidateServiceMode(mode); err != nil {
		return err
	}
	var err error
	config, err = filepath.Abs(config)
	if err != nil {
		return err
	}
	mode, err = resolveServiceMode(config, mode)
	if err != nil {
		return err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return err
	}
	cfg, err := node.LoadConfig(config)
	if err != nil {
		if (operation == "stop" || operation == "uninstall") && os.IsNotExist(err) {
			if handled, serviceErr := platformService(ctx, operation, binary, config, mode, node.Config{}); handled {
				return serviceErr
			}
		}
		if operation == "uninstall" && os.IsNotExist(err) {
			return nil
		}
		return err
	}
	c := client.Client{URL: API(cfg), Token: cfg.Token}
	if operation == "start" {
		if err = CheckServiceProfile(config, mode); err != nil {
			return err
		}
		if err = rememberInstallation(ctx, binary, config, mode, cfg); err != nil {
			return err
		}
	}
	if operation == "firewall" {
		return configureFirewall(ctx, binary, config, cfg)
	}
	if handled, err := platformService(ctx, operation, binary, config, mode, cfg); handled {
		if err == nil && operation == "start" {
			return waitReady(ctx, c, cfg)
		}
		return err
	}
	switch operation {
	case "stop", "uninstall":
		if operation == "uninstall" {
			if err = uninstallService(ctx, config); err != nil {
				return err
			}
		}
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(requestCtx, "POST", c.URL+"/v1/service/stop", nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if operation == "uninstall" {
				if stopErr := waitStopped(requestCtx, cfg); stopErr != nil {
					return fmt.Errorf("stop local node: %w", err)
				}
				return finishUninstall(requestCtx)
			}
			return fmt.Errorf("stop local node: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("stop local node: %s", resp.Status)
		}
		if err = waitStopped(requestCtx, cfg); err != nil {
			return err
		}
		if err = finishServiceStop(requestCtx, config, mode); err != nil {
			return err
		}
		if operation == "uninstall" {
			return finishUninstall(requestCtx)
		}
		return nil
	case "status":
		return ready(ctx, c, cfg)
	case "start":
		probe, cancel := context.WithTimeout(ctx, time.Second)
		err = ready(probe, c, cfg)
		cancel()
		if err == nil {
			return nil
		}
		if mode == "" {
			mode = "auto"
		}
		if mode != "process" {
			err = installService(ctx, binary, config)
			if err == nil {
				return waitReady(ctx, c, cfg)
			}
			if mode == "user" {
				return err
			}
			fmt.Fprintln(os.Stderr, "User startup unavailable; starting a background process:", err)
		}
		if err = startDetached(binary, config); err != nil {
			return err
		}
		return waitReady(ctx, c, cfg)
	default:
		return errors.New("service command must be start, stop, status, or uninstall")
	}
}

// ValidateServiceMode rejects unknown startup modes on every platform.
func ValidateServiceMode(mode string) error {
	if mode != "" && mode != "auto" && mode != "user" && mode != "system" && mode != "process" {
		return errors.New("service mode must be auto, user, system, or process")
	}
	return nil
}

func waitStopped(ctx context.Context, cfg node.Config) error {
	lock := flock.New(filepath.Join(cfg.DataDir, "runtime.lock"))
	defer func() { _ = lock.Close() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		held, err := lock.TryLock()
		if err != nil {
			return err
		}
		if held {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("node supervisor did not stop: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func ready(ctx context.Context, c client.Client, cfg node.Config) error {
	var description struct{ ID, Name string }
	if err := c.Call(ctx, "", "node.describe", map[string]any{}, &description); err != nil {
		return err
	}
	id, err := identity.Load(cfg.DataDir)
	if err != nil {
		return err
	}
	if description.ID != id.ID || description.Name != cfg.Name {
		return errors.New("local API belongs to another node")
	}
	var nodes []model.Node
	if err := c.Call(ctx, "", "nodes.list", map[string]any{}, &nodes); err != nil {
		return err
	}
	for _, n := range nodes {
		if n.ID == id.ID && n.Online {
			return nil
		}
	}
	return errors.New("node is not registered and online")
}
func waitReady(ctx context.Context, c client.Client, cfg node.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		probe, done := context.WithTimeout(ctx, 2*time.Second)
		last = ready(probe, c, cfg)
		done()
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("node did not become available; inspect %s: %w", filepath.Join(filepath.Dir(cfg.DataDir), "node.log"), last)
		case <-ticker.C:
		}
	}
}
func startDetached(binary, config string) error {
	if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(filepath.Dir(config), "node.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	cmd := exec.Command(binary, "--token", "", "node", "--config", config)
	cmd.Env = cleanEnvironment()
	cmd.Stdout, cmd.Stderr = log, log
	detach(cmd)
	if err = cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
func cleanEnvironment() []string {
	values := []string{}
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if name == "CONTROL_TOKEN" || name == "CONTROL_USER_KEY" || name == "CONTROL_SUPERUSER_KEY" || name == "CONTROL_RELEASE_TOKEN" {
			continue
		}
		values = append(values, value)
	}
	return values
}
func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	processutil.HideWindow(cmd)
	cmd.Env = cleanEnvironment()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}
