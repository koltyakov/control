package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/installation"
	"golang.org/x/term"
)

func loginCLI(ctx context.Context, args []string, config string) error {
	saved, err := installation.ReadAdmin()
	if err != nil {
		return err
	}
	cfg, err := installation.ReadConfig(config)
	if err != nil {
		return err
	}
	defaultGateway := saved.Gateway
	if defaultGateway == "" {
		defaultGateway = cfg.Gateway
	}
	f := flag.NewFlagSet("login", flag.ContinueOnError)
	base := f.String("gateway", env("CONTROL_GATEWAY", defaultGateway), "gateway URL")
	key := f.String("api-key", env("CONTROL_USER_KEY", env("CONTROL_SUPERUSER_KEY", os.Getenv("CONTROL_TOKEN"))), "gateway API key; omit to prompt securely")
	stdin := f.Bool("key-stdin", false, "read the API key from standard input")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || strings.TrimSpace(*base) == "" {
		return errors.New("usage: control login --gateway URL [--api-key KEY | --key-stdin]")
	}
	gatewayURL, err := enrollment.GatewayURL(strings.TrimSpace(*base))
	if err != nil {
		return err
	}
	remote := client.Admin{URL: gatewayURL}
	// Reuse the saved route only for its canonical gateway, never for an
	// explicit login to a different server.
	if cfg.GatewayProxy != nil && gatewayURL == strings.TrimRight(cfg.Gateway, "/") {
		if remote.Route, err = cfg.Route(); err != nil {
			return err
		}
	}
	explicitKey := false
	f.Visit(func(f *flag.Flag) {
		if f.Name == "api-key" {
			explicitKey = true
		}
	})
	if !*stdin && !explicitKey && *key == "" && gatewayURL == strings.TrimRight(saved.Gateway, "/") {
		*key = saved.Key
	}
	if *stdin {
		if *key != "" {
			return errors.New("use --key-stdin without --api-key or a credential environment variable")
		}
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 4097))
		if err != nil {
			return err
		}
		if len(data) > 4096 {
			return errors.New("API key exceeds 4096 bytes")
		}
		*key = string(data)
	} else if *key == "" && term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Gateway API key: ")
		data, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		*key = string(data)
	}
	*key = strings.TrimSpace(*key)
	if *key == "" {
		return errors.New("API key is required; use --key-stdin, --api-key, or CONTROL_USER_KEY")
	}
	remote.Key = *key
	role, err := remote.AuthRole(ctx)
	if err != nil {
		return err
	}
	if !client.ExecutionRole(role) && role != "common" && role != "discovery" {
		return errors.New("gateway returned an unsupported credential role")
	}
	profile := installation.AdminProfile{Gateway: gatewayURL, Key: *key}
	// Preserve an operator login written by an older CLI before replacing it
	// with a fleet-account login. Only a verified superuser key is retained.
	if role != "superuser" && saved.Key != "" && saved.Key != profile.Key && gatewayURL == strings.TrimRight(saved.Gateway, "/") {
		previous := remote
		previous.Key = saved.Key
		if previous.IsSuperuser(ctx) {
			if err := installation.SaveUpdateAdmin(installation.AdminProfile{Gateway: gatewayURL, Key: saved.Key}); err != nil {
				return fmt.Errorf("preserve previous update authorization: %w", err)
			}
		}
	}
	if err := installation.SaveAdmin(profile); err != nil {
		return err
	}
	if role == "superuser" {
		if err := installation.SaveUpdateAdmin(profile); err != nil {
			return fmt.Errorf("login saved, but could not save update authorization: %w", err)
		}
	}
	fmt.Printf("Logged in to %s as %s. API key saved for future commands.\n", gatewayURL, role)
	return nil
}
