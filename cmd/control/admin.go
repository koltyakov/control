package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/installation"
)

func adminClient(profile string) client.Admin {
	c, _ := gatewayClient(profile, nil)
	if c.URL == "" {
		c.URL = "http://127.0.0.1:7330"
	}
	return c
}

func gatewayClient(profile string, tokenOverride *string) (client.Admin, error) {
	cfg, err := installation.ReadConfig(profile)
	if err != nil {
		return client.Admin{}, err
	}
	saved, err := installation.ReadAdmin()
	if err != nil {
		return client.Admin{}, err
	}
	base := env("CONTROL_GATEWAY", cfg.Gateway)
	if base == "" {
		base = saved.Gateway
	}
	base = strings.TrimRight(base, "/")
	key := env("CONTROL_USER_KEY", env("CONTROL_SUPERUSER_KEY", os.Getenv("CONTROL_TOKEN")))
	if tokenOverride != nil {
		key = *tokenOverride
	} else if key == "" {
		if base == strings.TrimRight(saved.Gateway, "/") {
			key = saved.Key
		}
		if key == "" && base == strings.TrimRight(cfg.Gateway, "/") {
			key = cfg.Token
		}
	}
	return client.Admin{URL: base, Key: key}, nil
}

func updateAdminClient(profile string, tokenOverride *string) (client.Admin, error) {
	if key := os.Getenv("CONTROL_SUPERUSER_KEY"); tokenOverride == nil && key != "" {
		tokenOverride = &key
	}
	c, err := gatewayClient(profile, tokenOverride)
	if err != nil {
		return client.Admin{}, err
	}
	saved, err := installation.ReadUpdateAdmin()
	if err != nil {
		return client.Admin{}, fmt.Errorf("read saved update authorization: %w", err)
	}
	if c.URL == "" {
		c.URL = strings.TrimRight(saved.Gateway, "/")
	}
	if c.URL == "" {
		return client.Admin{}, errors.New("gateway URL is not configured; set CONTROL_GATEWAY or run control login --gateway URL")
	}
	if tokenOverride == nil && c.URL == strings.TrimRight(saved.Gateway, "/") && saved.Key != "" {
		c.Key = saved.Key
	}
	return c, nil
}

func commandHelp(ctx context.Context, c client.Admin) string {
	switch c.Role(ctx) {
	case "superuser":
		return usage + fleetUsage + adminUsage
	case "user":
		return usage + fleetUsage
	}
	return usage
}

const adminUsage = `
Superuser gateway commands:
  users create NAME                    Register an isolated user; show its account key once
  users list                           List registered users
  users revoke ID                      Disable a user and its credentials
  update authorize [--key-stdin|--check] Save gateway update authorization or verify it without prompting
  update push DIR [--json]              Upload a bundle; print a summary or full deployment JSON
  update status                        Show rollout and node versions
  update check                         Fetch the latest configured GitHub release
Environment: CONTROL_GATEWAY, CONTROL_SUPERUSER_KEY
`

const fleetUsage = `
Your fleet:
  machines add [NAME|auto] --platform macos|windows|linux  Create install command; default uses target hostname
    --service user|system                Choose login-session or boot-time startup
  machines invites                     List installation status
  machines revoke ID                   Revoke an invitation and its machine key
  machines enable|disable NAME          Enable or disable new work on a machine
  machines rename NAME NEW_NAME         Change a machine's routing alias
  machines unregister NAME              Unregister a machine and request node uninstall
  keys create NAME                     Issue a common key, shown once
  keys list                            List common keys
  keys revoke ID                       Revoke a common key
Environment: CONTROL_GATEWAY, CONTROL_USER_KEY
`

func adminCLI(ctx context.Context, c client.Admin, args []string) error {
	help := len(args) < 2 || args[1] == "--help" || args[1] == "help"
	role, authErr := c.AuthRole(ctx)
	if authErr != nil && !help {
		return authErr
	}
	if role != "superuser" && (role != "user" || args[0] != "keys") {
		if !help && args[0] == "update" {
			return errUpdatePermission
		}
		return fmt.Errorf("unknown command %q", args[0])
	}
	if help {
		if args[0] == "keys" {
			fmt.Print(fleetUsage)
		} else {
			fmt.Print(adminUsage)
		}
		return nil
	}
	if args[0] == "update" && args[1] == "push" {
		return pushUpdateCLI(ctx, c, args[2:])
	}
	var result json.RawMessage
	var err error
	switch args[0] + " " + args[1] {
	case "users create":
		if len(args) != 3 {
			return errors.New("usage: control users create NAME")
		}
		err = c.JSON(ctx, http.MethodPost, "/v1/admin/users", map[string]string{"name": args[2]}, &result)
	case "users list":
		err = c.JSON(ctx, http.MethodGet, "/v1/admin/users", nil, &result)
	case "users revoke":
		if len(args) != 3 {
			return errors.New("usage: control users revoke ID")
		}
		return c.JSON(ctx, http.MethodDelete, "/v1/admin/users/"+url.PathEscape(args[2]), nil, nil)
	case "update status":
		if len(args) != 2 {
			return errors.New("usage: control update status")
		}
		err = c.JSON(ctx, http.MethodGet, "/v1/admin/updates", nil, &result)
	case "update check":
		if len(args) != 2 {
			return errors.New("usage: control update check")
		}
		err = c.JSON(ctx, http.MethodPost, "/v1/admin/updates/check", nil, &result)
	case "keys list":
		err = c.JSON(ctx, http.MethodGet, "/v1/fleet/keys", nil, &result)
	case "keys create":
		if len(args) != 3 {
			return errors.New("usage: control keys create NAME")
		}
		err = c.JSON(ctx, http.MethodPost, "/v1/fleet/keys", map[string]string{"name": args[2]}, &result)
	case "keys revoke":
		if len(args) != 3 {
			return errors.New("usage: control keys revoke ID")
		}
		return c.JSON(ctx, http.MethodDelete, "/v1/fleet/keys/"+url.PathEscape(args[2]), nil, nil)
	default:
		return errors.New("unknown administrative command")
	}
	if err != nil {
		return err
	}
	printJSON(result)
	return nil
}

func machineCLI(ctx context.Context, c client.Admin, args []string) error {
	role, err := c.AuthRole(ctx)
	if err != nil {
		return err
	}
	if role != "user" && role != "superuser" {
		return errors.New("machine management requires a user account key; run control login with your account key")
	}
	if args[0] == "invites" {
		var list any
		if err := c.JSON(ctx, "GET", "/v1/fleet/installations", nil, &list); err != nil {
			return err
		}
		printJSON(list)
		return nil
	}
	if args[0] == "revoke" && len(args) == 2 {
		return c.JSON(ctx, "DELETE", "/v1/fleet/installations/"+url.PathEscape(args[1]), nil, nil)
	}
	if args[0] == "rename" {
		if len(args) != 3 {
			return errors.New("usage: control machines rename NAME NEW_NAME")
		}
		n, err := c.ResolveMachine(ctx, args[1])
		if err != nil {
			return err
		}
		return c.RenameMachine(ctx, n.ID, args[2])
	}
	if (args[0] == "forget" || args[0] == "unregister" || args[0] == "enable" || args[0] == "disable") && len(args) == 2 {
		n, err := c.ResolveMachine(ctx, args[1])
		if err != nil {
			return err
		}
		action := args[0]
		if action == "forget" {
			return c.JSON(ctx, "DELETE", "/v1/fleet/nodes/"+url.PathEscape(n.ID), nil, nil)
		}
		return c.ManageMachine(ctx, n.ID, action)
	}
	if args[0] != "add" {
		return errors.New("usage: control machines add [NAME|auto] --platform macos|windows|linux")
	}
	name, options := "auto", args[1:]
	if len(options) > 0 && !strings.HasPrefix(options[0], "-") {
		name, options = options[0], options[1:]
	}
	f := flag.NewFlagSet("machines add", flag.ContinueOnError)
	platform := f.String("platform", "", "macos, windows, or linux; architecture is detected by the installer")
	service := f.String("service", "", "user (login session) or system (boot-time service); omitted keeps platform defaults")
	ttl := f.Duration("ttl", 15*time.Minute, "installation link lifetime")
	asJSON := f.Bool("json", false, "print installation JSON")
	if err := f.Parse(options); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: control machines add [NAME|auto] --platform macos|windows|linux")
	}
	if *service != "" && *service != "user" && *service != "system" {
		return errors.New("service must be user or system")
	}
	parts := strings.Split(strings.ToLower(*platform), "/")
	if len(parts) > 2 || *ttl < time.Minute || *ttl > 24*time.Hour {
		return errors.New("provide --platform macos|windows|linux and a TTL between 1m and 24h")
	}
	if parts[0] == "macos" {
		parts[0] = "darwin"
	}
	if parts[0] != "darwin" && parts[0] != "windows" && parts[0] != "linux" {
		return errors.New("platform must be macos, windows, or linux")
	}
	arch := ""
	if len(parts) == 2 {
		arch = parts[1]
	}
	link, err := c.Invite(ctx, enrollment.Request{Name: name, OS: parts[0], Arch: arch, ServiceMode: *service, TTLSeconds: int(ttl.Seconds())})
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(link)
	} else {
		displayName := link.Name
		if link.AutoName {
			displayName = "using the target machine's hostname"
		}
		fmt.Fprintf(os.Stderr, "Install %s. Link expires %s; redeemable by one machine.\n", displayName, link.ExpiresAt.Local().Format(time.RFC3339))
		fmt.Println(link.Command)
	}
	return nil
}
