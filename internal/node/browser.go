package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/browser"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/secrets"
	"github.com/koltyakov/control/internal/store"
)

func (n *Node) registerBrowser() error {
	if n.Config.Browser == nil {
		return nil
	}
	cfg := n.Config.Browser
	if !filepath.IsAbs(cfg.CLI) || (cfg.Config != "" && !filepath.IsAbs(cfg.Config)) {
		return errors.New("browser.cli and optional browser.config must be absolute paths")
	}
	if _, err := n.privateBrowserDir(filepath.Join(n.Config.DataDir, "browser")); err != nil {
		return err
	}
	return n.Register(provider{cap: model.Capability{
		Name: "browser.run", InputSchema: model.JSON(browser.Schema()),
		Description: "Run the installed agent-browser CLI on this worker with structured args and optional non-secret stdin. Default home, namespace and runtime are private to this profile and stable caller owner; commands serialize per owner. Load --version and skills get core first. Text results mask worker secrets; no raw task logs. CLI input is task-visible, not protected credential input. Live viewport, dashboard, files and upstream activity are not redacted. For manual login/2FA use a loopback dashboard through a Control forward; pause automation explicitly. Sessions/daemons persist until explicitly closed or upstream idle expiry. Never replay uncertain effects.",
	}, run: n.browserRun})
}

// Browser state includes cookies, auth vaults and runtime sidecars. Keep every
// default directory outside files.* and reject symlinked directory leaves.
func (n *Node) privateBrowserDir(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", errors.New("cannot create private browser directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return "", errors.New("browser directory must not be a symlink")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", errors.New("cannot resolve private browser directory")
	}
	work, err := os.Stat(n.Config.WorkDir)
	if err != nil {
		return "", errors.New("cannot validate browser workspace")
	}
	for current := resolved; ; current = filepath.Dir(current) {
		item, err := os.Stat(current)
		if err != nil {
			return "", errors.New("cannot validate private browser directory")
		}
		if os.SameFile(item, work) {
			return "", errors.New("browser directories must be outside workDir")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", errors.New("cannot protect private browser directory")
	}
	return resolved, nil
}

func (n *Node) browserRun(ctx context.Context, raw json.RawMessage, e Execution) (any, error) {
	r, err := browser.Decode(raw)
	if err != nil {
		return nil, err
	}
	// Include profile identity so equal session names on other nodes/profiles
	// cannot share a daemon, including Windows' namespace-derived TCP ports.
	digest := sha256.Sum256([]byte(n.Config.DataDir + "\x00" + e.Owner))
	namespace := "c" + hex.EncodeToString(digest[:8])
	root, err := n.privateBrowserDir(filepath.Join(n.Config.DataDir, "browser"))
	if err != nil {
		return nil, err
	}
	home, err := n.privateBrowserDir(filepath.Join(root, namespace))
	if err != nil {
		return nil, err
	}
	// Unix-domain sockets have short path limits. Do not put them under the
	// potentially long node data directory or task workspace.
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, errors.New("cannot locate browser runtime storage")
	}
	runtime, err := n.privateBrowserDir(filepath.Join(cache, "control", "b"))
	if err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(home, "command.lock"))
	defer func() { _ = lock.Close() }()
	if held, err := lock.TryLockContext(ctx, 50*time.Millisecond); err != nil || !held {
		return nil, errors.New("browser command lock unavailable or request cancelled")
	}
	config := n.Config.Browser.Config
	if config == "" {
		config = filepath.Join(home, "config.json")
		// This durable empty config prevents automatic workspace/user config
		// discovery from silently attaching to another browser or enabling traces.
		if err := store.Write(config, map[string]any{}); err != nil {
			return nil, errors.New("cannot write private browser configuration")
		}
	}
	values, err := n.secretValues()
	if err != nil {
		return nil, errors.New("cannot load worker-local credentials")
	}
	values[""] = home
	values["\x00runtime"] = runtime
	cmd := command(ctx, Command{Command: n.Config.Browser.CLI, Args: append([]string{"--json"}, r.Args...)}, e.Dir)
	cmd.Env = browserEnvironment(cmd.Env, home, runtime, namespace, config, r.Session)
	if err := n.checkBrowserVersion(ctx, cmd.Env, e.Dir); err != nil {
		return nil, err
	}
	if r.Stdin != nil {
		cmd.Stdin = strings.NewReader(*r.Stdin)
	}
	stdout, stderr := &capture{}, &capture{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("agent-browser cancelled; effects may already have occurred; inspect the same session before continuing: %w", ctx.Err())
	}
	var exited *exec.ExitError
	if stdout.truncated || stderr.truncated || (err != nil && !errors.As(err, &exited)) {
		return nil, errors.New("agent-browser failed or exceeded output limits; effects may already have occurred; raw diagnostics suppressed")
	}
	if !utf8.Valid(stdout.b.Bytes()) || !utf8.Valid(stderr.b.Bytes()) {
		return nil, errors.New("agent-browser returned non-text output; use declared task output artifacts for binary data")
	}
	var upstream any
	if json.Unmarshal(stdout.b.Bytes(), &upstream) != nil {
		upstream = map[string]any{"stdout": stdout.b.String()}
	}
	ok := err == nil && !browserFailed(upstream)
	result := model.JSON(map[string]any{
		"success": ok, "data": upstream,
		"process": map[string]any{"exitCode": cmd.ProcessState.ExitCode(), "stderr": stderr.b.String()},
	})
	clean, cleanErr := secrets.NewRedactor(values).JSON(result)
	if cleanErr != nil {
		return nil, errors.New("cannot sanitize browser response")
	}
	if !ok {
		return json.RawMessage(clean), errors.New("browser request failed; inspect sanitized result; never replay automatically")
	}
	return json.RawMessage(clean), nil
}

// Older CLIs silently ignore settings used for private runtime namespaces and
// lack the bundled dashboard. Check the configured executable before any work,
// including after it has been replaced or upgraded outside Control.
func (n *Node) checkBrowserVersion(ctx context.Context, env []string, dir string) error {
	cmd := command(ctx, Command{Command: n.Config.Browser.CLI, Args: []string{"--version"}}, dir)
	cmd.Env = env
	stdout, stderr := &capture{}, &capture{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("browser version check cancelled; requested command was not run: %w", ctx.Err())
	}
	if err != nil || stdout.truncated || stderr.truncated || !compatibleBrowserVersion(stdout.b.String()) {
		return errors.New("browser.cli requires agent-browser 0.41.2 or newer with native namespace/config/dashboard support; requested command was not run; dependency installation or upgrade needs approval")
	}
	return nil
}

func compatibleBrowserVersion(text string) bool {
	fields := strings.Fields(text)
	if len(fields) != 2 || fields[0] != "agent-browser" {
		return false
	}
	version := strings.Split(strings.TrimPrefix(fields[1], "v"), ".")
	if len(version) != 3 {
		return false
	}
	numbers := [3]int{}
	for i, part := range version {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return false
		}
		numbers[i] = n
	}
	return numbers[0] > 0 || numbers[1] > 41 || (numbers[1] == 41 && numbers[2] >= 2)
}

// Upstream JSON uses success, not agent-desktop's ok. Batch output is an array
// of individual outcomes; retain partial output but fail dependent work.
func browserFailed(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		if success, exists := v["success"]; exists {
			return success != true
		}
	case []any:
		for _, item := range v {
			if browserFailed(item) {
				return true
			}
		}
	}
	return false
}

func browserEnvironment(env []string, home, runtime, namespace, config, session string) []string {
	clean := make([]string, 0, len(env)+9)
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "AGENT_BROWSER_") || upper == "HOME" || upper == "USERPROFILE" ||
			upper == "XDG_CONFIG_HOME" || upper == "XDG_DATA_HOME" || upper == "XDG_CACHE_HOME" {
			continue
		}
		clean = append(clean, item)
	}
	if session == "" {
		session = "default"
	}
	return append(clean, "HOME="+home, "USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_DATA_HOME="+filepath.Join(home, "data"),
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"), "AGENT_BROWSER_SOCKET_DIR="+runtime,
		"AGENT_BROWSER_NAMESPACE="+namespace, "AGENT_BROWSER_CONFIG="+config, "AGENT_BROWSER_SESSION="+session)
}
