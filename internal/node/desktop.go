package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/desktop"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/secrets"
)

func (n *Node) registerDesktop() error {
	if n.Config.Desktop == nil {
		return nil
	}
	cfg := n.Config.Desktop
	if cfg.Library != "" {
		if !filepath.IsAbs(cfg.Library) || cfg.Command.Command != "" || len(cfg.Args) != 0 || len(cfg.Env) != 0 || cfg.Dir != "" {
			return errors.New("desktop.library must be absolute and cannot be combined with an external helper command")
		}
	} else if cfg.Command.Command == "" {
		return errors.New("desktop requires an absolute native library path or an explicit helper command")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	// Keep the legacy filename so older and newer profiles share the same lock.
	n.desktopLockPath = filepath.Join(dir, "control", "rpa.lock")
	if _, err := n.desktopStateDir(); err != nil {
		return err
	}
	return n.Register(provider{cap: model.Capability{
		Name: "desktop.run", InputSchema: model.JSON(desktop.Schema()),
		Description: "Opt-in native agent-desktop status, scoped snapshot, click, clear, or headless setValue. Use secret names for credentials; values travel only to a worker-local native helper over stdin. All responses mask currently configured secrets and raw helper logs are suppressed. No screenshots, clipboard, tracing, or headed input. Use fresh qualified refs and never replay uncertain effects.",
	}, run: n.desktopRun})
}

// Native refmaps can contain desktop text. Never expose them through files.*.
func (n *Node) desktopStateDir() (string, error) {
	dir := filepath.Join(n.Config.DataDir, "desktop")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", errors.New("cannot create private desktop state")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return "", errors.New("desktop state must be a directory, not a symlink")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", errors.New("cannot resolve desktop state")
	}
	work, err := os.Stat(n.Config.WorkDir)
	if err != nil {
		return "", errors.New("cannot validate desktop workspace")
	}
	for current := resolved; ; current = filepath.Dir(current) {
		item, err := os.Stat(current)
		if err != nil {
			return "", errors.New("cannot validate private desktop state")
		}
		if os.SameFile(item, work) {
			return "", errors.New("desktop state must be outside workDir")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", errors.New("cannot protect private desktop state")
	}
	return resolved, nil
}

func (n *Node) desktopRun(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	r, err := desktop.Decode(args)
	if err != nil {
		return nil, err
	}
	values, err := n.secretValues()
	if err != nil {
		return nil, errors.New("cannot load worker-local credentials")
	}
	if r.Secret != "" {
		value, err := resolveSecret(values, r.Secret)
		if err != nil {
			return nil, err
		}
		r.Text, r.Secret = &value, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(n.desktopLockPath), 0700); err != nil {
		return nil, errors.New("cannot open desktop lock")
	}
	lock := flock.New(n.desktopLockPath)
	defer func() { _ = lock.Close() }()
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil || !locked {
		return nil, errors.New("desktop lock unavailable or request cancelled")
	}
	home, err := n.desktopStateDir()
	if err != nil {
		return nil, err
	}
	// Always suppress raw output, including observations without secret inputs.
	helper := n.Config.Desktop.Command
	if n.Config.Desktop.Library != "" {
		binary, err := os.Executable()
		if err != nil {
			return nil, errors.New("cannot locate built-in native desktop helper")
		}
		helper = Command{Command: binary, Args: []string{"__desktop", n.Config.Desktop.Library}}
	}
	cmd := command(ctx, helper, e.Dir)
	cmd.Env = desktopEnvironment(cmd.Env)
	cmd.Stdin = bytes.NewReader(desktop.Packet(r, home))
	stdout := &capture{}
	cmd.Stdout, cmd.Stderr = stdout, io.Discard
	if err := cmd.Run(); err != nil || stdout.truncated {
		return nil, errors.New("native desktop helper failed or exceeded output limit; effects may already have occurred; diagnostics suppressed")
	}
	raw := stdout.b.Bytes()
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || envelope == nil {
		return nil, errors.New("invalid native desktop response; effects may already have occurred")
	}
	var ok bool
	if raw, exists := envelope["ok"]; !exists || json.Unmarshal(raw, &ok) != nil || string(raw) == "null" {
		return nil, errors.New("native desktop response is missing its outcome; effects may already have occurred")
	}
	payloadKey := "error"
	if ok {
		payloadKey = "data"
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(envelope[payloadKey], &payload) != nil || payload == nil {
		return nil, errors.New("native desktop response has an invalid payload; effects may already have occurred")
	}
	// The empty key cannot be a stored secret name. Mask the native state path
	// in errors and observations too, not just successful status responses.
	values[""] = home
	clean, err := secrets.NewRedactor(values).JSON(raw)
	if err != nil {
		return nil, errors.New("cannot sanitize native desktop response")
	}
	if r.Command == "status" && ok {
		// Status is health, not a way to discover private state paths or artifacts.
		var status struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(clean, &status)
		data := map[string]json.RawMessage{}
		for _, key := range []string{"platform", "permissions", "version"} {
			if value, exists := status.Data[key]; exists {
				data[key] = value
			}
		}
		clean = model.JSON(map[string]any{"ok": true, "data": data})
	}
	if !ok {
		return json.RawMessage(clean), errors.New("native desktop request failed; inspect sanitized result; never replay automatically")
	}
	return json.RawMessage(clean), nil
}

// Kept separate from command() so generic providers retain their environment.
func desktopEnvironment(env []string) []string {
	clean := make([]string, 0, len(env))
	for _, item := range env {
		if !strings.HasPrefix(strings.ToUpper(item), "AGENT_DESKTOP_") {
			clean = append(clean, item)
		}
	}
	return clean
}
