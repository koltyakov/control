package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
	"golang.org/x/term"
)

func randomCredential() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ctl_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func setupCLI(ctx context.Context, args []string, config string) error {
	saved, err := installation.ReadAdmin()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("setup", flag.ContinueOnError)
	base := f.String("gateway", env("CONTROL_GATEWAY", saved.Gateway), "gateway URL")
	name := f.String("name", "", "this machine's name")
	agent := f.String("client", "", "install MCP and skill for this AI client")
	listen := f.String("listen", "127.0.0.1:7331", "loopback local API address")
	mode := f.String("service", env("CONTROL_SERVICE_MODE", "auto"), "auto (Windows system service), user, or process")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *base == "" || *name == "" {
		return errors.New("usage: control setup --gateway URL --name NAME [--client opencode] [--service auto|user|process]")
	}
	if !machineName.MatchString(*name) {
		return errors.New("machine name must be 1..63 letters, digits, dots, underscores, or hyphens and start with a letter or digit")
	}
	if *agent != "" {
		if _, err := installation.AgentLocations(*agent, "", ""); err != nil {
			return err
		}
	}
	if err := installation.CheckServiceMode(*mode); err != nil {
		return err
	}
	host, _, listenErr := net.SplitHostPort(*listen)
	if listenErr != nil || (host != "localhost" && !net.ParseIP(host).IsLoopback()) {
		return errors.New("setup listen address must be a loopback host:port")
	}
	if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		return err
	}
	lock := flock.New(config + ".install.lock")
	held, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !held {
		return errors.New("installation already running")
	}
	defer func() { _ = lock.Close() }()
	if _, err := os.Stat(config); err == nil {
		return errors.New("node config already exists; use control service start or install-mcp/install-skill")
	} else if !os.IsNotExist(err) {
		return err
	}
	gatewayURL, err := enrollment.GatewayURL(*base)
	if err != nil {
		return err
	}
	key := env("CONTROL_USER_KEY", env("CONTROL_SUPERUSER_KEY", os.Getenv("CONTROL_TOKEN")))
	if key == "" && gatewayURL == strings.TrimRight(saved.Gateway, "/") {
		key = saved.Key
	}
	if key == "" && term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Gateway API key: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		key = strings.TrimSpace(string(b))
	}
	if key == "" {
		return errors.New("run control login or set CONTROL_USER_KEY for initial setup")
	}
	admin := client.Admin{URL: gatewayURL, Key: key}
	var auth struct {
		Role string `json:"role"`
	}
	if err = admin.JSON(ctx, "GET", "/v1/auth", nil, &auth); err != nil {
		return err
	}
	credential := key
	if auth.Role == "superuser" || auth.Role == "user" {
		var created struct {
			Token string `json:"token"`
		}
		if err = admin.JSON(ctx, "POST", "/v1/fleet/keys", map[string]string{"name": "host:" + *name}, &created); err != nil {
			return err
		}
		credential = created.Token
		if err = installation.SaveAdmin(installation.AdminProfile{Gateway: gatewayURL, Key: key}); err != nil {
			return err
		}
	}
	if err = createNodeConfig(config, *name, gatewayURL, credential, *listen, true); err != nil {
		return err
	}
	binary, err := installation.InstallSelf(ctx)
	if err != nil {
		return err
	}
	if err = installation.Service(ctx, "start", binary, config, *mode); err != nil {
		return err
	}
	if *agent != "" {
		if err = installAgent(ctx, *agent, "", binary, config, true, true); err != nil {
			return err
		}
	}
	fmt.Printf("%s is registered and online.\n", *name)
	return nil
}

var machineName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

func createNodeConfig(path, name, gateway, token, listen string, host bool) error {
	if !machineName.MatchString(name) {
		return errors.New("invalid machine name")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	if listen == "" {
		listen = "127.0.0.1:7331"
	}
	cfg := node.Config{Name: name, Gateway: gateway, Token: token, DataDir: filepath.Join(base, "state"), WorkDir: filepath.Join(base, "work"), Listen: listen}
	// Keep the orchestrator's administrative account inaccessible to remote
	// execution, filesystem reads, and loopback proxying from common workers.
	if host {
		cfg.Allow = map[string][]string{"*": {"node.describe", "capabilities.list", "activities.list", "system.info", "artifacts.pull"}}
	}
	if err = os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return err
	}
	if err = os.MkdirAll(cfg.WorkDir, 0700); err != nil {
		return err
	}
	if _, err = identity.Load(cfg.DataDir); err != nil {
		return err
	}
	return store.Write(path, cfg)
}

type pendingEnrollment struct {
	URL                string                `json:"url"`
	Invitation         enrollment.Invitation `json:"invitation"`
	Credential         string                `json:"credential"`
	Config             json.RawMessage       `json:"config,omitempty"`
	IdentityID         string                `json:"identityId,omitempty"`
	PreviousIdentityID string                `json:"previousIdentityId,omitempty"`
	DataDir            string                `json:"dataDir,omitempty"`
}

func enrollCLI(ctx context.Context, args []string, config string) error {
	f := flag.NewFlagSet("enroll", flag.ContinueOnError)
	link := f.String("url", "", "one-time installation URL")
	mode := f.String("service", env("CONTROL_SERVICE_MODE", "auto"), "auto (Windows system service), user, or process")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *link == "" {
		return errors.New("installation URL is required")
	}
	if err := installation.CheckServiceMode(*mode); err != nil {
		return err
	}
	if _, err := enrollment.GatewayURL(*link); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		return err
	}
	lock := flock.New(config + ".install.lock")
	held, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !held {
		return errors.New("installation already running")
	}
	defer func() { _ = lock.Close() }()
	pendingPath := filepath.Join(filepath.Dir(config), "pending-enrollment.json")
	var pending pendingEnrollment
	readErr := store.Read(pendingPath, &pending)
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	if readErr == nil && pending.URL != *link {
		return errors.New("another enrollment is pending; resume it before using a new invitation")
	}
	c := client.Admin{URL: strings.TrimRight(*link, "/")}
	if os.IsNotExist(readErr) {
		if err = c.JSON(ctx, "GET", "/info?arch="+runtime.GOARCH, nil, &pending.Invitation); err != nil {
			return err
		}
		if !strings.HasPrefix(*link, pending.Invitation.Gateway+"/install/") {
			return errors.New("invitation gateway mismatch")
		}
		a := pending.Invitation.Asset
		if a.OS != runtime.GOOS || a.Arch != runtime.GOARCH {
			return errors.New("invitation platform does not match this machine")
		}
		if !update.Matches(buildinfo.Current(), a) {
			return errors.New("enrollment must use the executable pinned by the invitation")
		}
		credential, err := randomCredential()
		if err != nil {
			return err
		}
		pending.URL, pending.Credential = *link, credential
		if err = prepareReenrollment(config, &pending); err != nil {
			return err
		}
	}
	if err = replaceRetiredIdentity(ctx, config, &pending); err != nil {
		return err
	}
	if err = store.Write(pendingPath, pending); err != nil {
		return err
	}
	if _, err := os.Stat(config); os.IsNotExist(err) {
		if err = createNodeConfig(config, pending.Invitation.Name, pending.Invitation.Gateway, pending.Credential, "", false); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	cfg, err := node.LoadConfig(config)
	if err != nil {
		return err
	}
	if len(pending.Config) == 0 && (cfg.Name != pending.Invitation.Name || cfg.Gateway != pending.Invitation.Gateway || cfg.Token != pending.Credential) {
		return errors.New("pending enrollment does not match local configuration")
	}
	id, err := identity.Load(cfg.DataDir)
	if err != nil {
		return err
	}
	expectedID := pending.IdentityID
	if pending.PreviousIdentityID != "" && cfg.Token != pending.Credential {
		expectedID = pending.PreviousIdentityID
	}
	if expectedID != "" && id.ID != expectedID {
		return errors.New("pending enrollment does not match local identity")
	}
	if pending.DataDir != "" {
		id, err = identity.Load(pending.DataDir)
		if err != nil {
			return err
		}
		if id.ID != pending.IdentityID {
			return errors.New("pending replacement identity has changed")
		}
	}
	ticket := (*link)[strings.LastIndex(*link, "/")+1:]
	q := enrollment.Redemption{PublicKey: id.Public, CredentialHash: enrollment.Hash(pending.Credential)}
	if len(pending.Invitation.Assets) > 0 {
		q.Arch = pending.Invitation.Asset.Arch
	}
	q.Signature = ed25519.Sign(id.Private, enrollment.Message(ticket, q))
	if len(pending.Config) > 0 && cfg.Token != pending.Credential {
		// Stop the existing supervisor before replacing its executable or profile.
		// Uninstall also handles an already-stopped installation.
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		if err = installation.Service(ctx, "uninstall", binary, config, *mode); err != nil {
			return err
		}
	}
	// Keep a durable executable for recovery before consuming the ticket. The
	// shell installer deletes its temporary download even when this request fails.
	binary, err := installation.InstallSelf(ctx)
	if err != nil {
		return err
	}
	if err = c.JSON(ctx, "POST", "/redeem", q, nil); err != nil {
		return fmt.Errorf("enrollment pending; resume with control enroll --url using the same link: %w", err)
	}
	if len(pending.Config) > 0 {
		if err = store.Bytes(config, pending.Config, 0600); err != nil {
			return err
		}
		cfg, err = node.LoadConfig(config)
		if err != nil {
			return err
		}
	}
	if err = installation.Service(ctx, "start", binary, config, *mode); err != nil {
		return err
	}
	if err = os.Remove(pendingPath); err != nil {
		return err
	}
	fmt.Printf("%s is registered and online. Installed %s\n", cfg.Name, binary)
	return nil
}

// Preserve all profile fields, including unknown provider settings and an
// explicitly empty allow map. Only enrollment-owned fields change.
func prepareReenrollment(config string, pending *pendingEnrollment) error {
	data, err := os.ReadFile(config)
	if os.IsNotExist(err) {
		if pending.Invitation.ReplaceID != "" {
			return errors.New("this invitation replaces an existing machine; run it using that machine's profile")
		}
		return nil
	}
	if err != nil {
		return err
	}
	cfg, err := node.LoadConfig(config)
	if err != nil {
		return err
	}
	if strings.TrimRight(cfg.Gateway, "/") != pending.Invitation.Gateway {
		return errors.New("existing node belongs to another gateway; use a separate profile")
	}
	id, err := identity.Load(cfg.DataDir)
	if err != nil {
		return err
	}
	if pending.Invitation.ReplaceID != "" && pending.Invitation.ReplaceID != id.ID {
		return errors.New("invitation name belongs to another machine")
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key, value := range map[string]string{"name": pending.Invitation.Name, "gateway": pending.Invitation.Gateway, "token": pending.Credential} {
		fields[key], _ = json.Marshal(value)
	}
	pending.Config, err = json.MarshalIndent(fields, "", "  ")
	pending.IdentityID = id.ID
	return err
}

// A removed identity stays retired. Keep its state intact and use a separate
// state directory for the new installation, retaining the existing workspace.
func replaceRetiredIdentity(ctx context.Context, config string, pending *pendingEnrollment) error {
	if len(pending.Config) == 0 || pending.DataDir != "" {
		return nil
	}
	cfg, err := node.LoadConfig(config)
	if err != nil {
		return err
	}
	id, err := identity.Load(cfg.DataDir)
	if err != nil {
		return err
	}
	if id.ID != pending.IdentityID {
		return errors.New("pending enrollment does not match local identity")
	}
	state, _, err := (client.Admin{URL: pending.Invitation.Gateway}).InspectMachineState(ctx, id)
	if err != nil {
		return err
	}
	if !state.Unregistered {
		return nil
	}
	if pending.Invitation.ReplaceID != "" {
		return errors.New("invitation reserves a retired identity; create a new invitation")
	}
	dir := filepath.Join(filepath.Dir(cfg.DataDir), "state-"+identity.NewID())
	fresh, err := identity.Load(dir)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(pending.Config, &fields); err != nil {
		return err
	}
	fields["dataDir"], _ = json.Marshal(dir)
	pending.Config, err = json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	pending.PreviousIdentityID, pending.IdentityID, pending.DataDir = id.ID, fresh.ID, dir
	return nil
}

func serviceCLI(ctx context.Context, args []string, config string) error {
	if len(args) == 0 {
		return errors.New("usage: control service start|stop|status|uninstall [--mode auto|user|process]")
	}
	f := flag.NewFlagSet("service", flag.ContinueOnError)
	mode := f.String("mode", env("CONTROL_SERVICE_MODE", "auto"), "startup mode")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	if err = installation.Service(ctx, args[0], binary, config, *mode); err != nil {
		return err
	}
	fmt.Println("Service", args[0], "completed")
	return nil
}
