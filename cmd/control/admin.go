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

func adminClient() client.Admin {
	saved, _ := installation.ReadAdmin()
	if saved.Gateway == "" {
		if cfg, err := installation.ReadConfig(installation.ConfigPath()); err == nil {
			saved.Gateway = cfg.Gateway
		}
	}
	if saved.Gateway == "" {
		saved.Gateway = "http://127.0.0.1:7330"
	}
	base := env("CONTROL_GATEWAY", saved.Gateway)
	key := env("CONTROL_USER_KEY", env("CONTROL_SUPERUSER_KEY", os.Getenv("CONTROL_TOKEN")))
	if key == "" && strings.TrimRight(base, "/") == strings.TrimRight(saved.Gateway, "/") {
		key = saved.Key
	}
	return client.Admin{URL: base, Key: key}
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
  update push DIR                      Upload a development/release bundle
  update status                        Show rollout and node versions
  update check                         Fetch the latest configured GitHub release
Environment: CONTROL_GATEWAY, CONTROL_SUPERUSER_KEY
`

const fleetUsage = `
Your fleet:
  machines add NAME --platform OS/ARCH [--ttl 15m]  Create one-time install command
  machines invites                     List installation status
  machines revoke ID                   Revoke an invitation and its machine key
  machines forget NODE_ID              Remove an offline registration and its invitation key
  keys create NAME                     Issue a common key, shown once
  keys list                            List common keys
  keys revoke ID                       Revoke a common key
Environment: CONTROL_GATEWAY, CONTROL_USER_KEY
`

func adminCLI(ctx context.Context, c client.Admin, args []string) error {
	role := c.Role(ctx)
	if role != "superuser" && !(role == "user" && args[0] == "keys") {
		return fmt.Errorf("unknown command %q", args[0])
	}
	if len(args) < 2 || args[1] == "--help" || args[1] == "help" {
		if args[0] == "keys" {
			fmt.Print(fleetUsage)
		} else {
			fmt.Print(adminUsage)
		}
		return nil
	}
	if args[0] == "update" && args[1] == "push" {
		if len(args) != 3 {
			return errors.New("usage: control update push DIR")
		}
		deployment, err := c.Push(ctx, args[2])
		if err != nil {
			return err
		}
		printJSON(deployment)
		return nil
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
	role := c.Role(ctx)
	if role != "user" && role != "superuser" {
		return errors.New("unknown machines command")
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
	if args[0] == "forget" && len(args) == 2 {
		return c.JSON(ctx, "DELETE", "/v1/fleet/nodes/"+url.PathEscape(args[1]), nil, nil)
	}
	if args[0] != "add" || len(args) < 2 {
		return errors.New("usage: control machines add NAME --platform linux/amd64 [--ttl 15m]")
	}
	f := flag.NewFlagSet("machines add", flag.ContinueOnError)
	platform := f.String("platform", "", "linux, darwin, or windows with /amd64 or /arm64")
	ttl := f.Duration("ttl", 15*time.Minute, "installation link lifetime")
	asJSON := f.Bool("json", false, "print installation JSON")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	parts := strings.Split(*platform, "/")
	if len(parts) != 2 || *ttl < time.Minute || *ttl > 24*time.Hour {
		return errors.New("provide --platform OS/ARCH and a TTL between 1m and 24h")
	}
	link, err := c.Invite(ctx, enrollment.Request{Name: args[1], OS: parts[0], Arch: parts[1], TTLSeconds: int(ttl.Seconds())})
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(link)
	} else {
		fmt.Fprintf(os.Stderr, "Install %s. Link expires %s; redeemable by one machine.\n", link.Name, link.ExpiresAt.Local().Format(time.RFC3339))
		fmt.Println(link.Command)
	}
	return nil
}
