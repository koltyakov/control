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

func loginCLI(ctx context.Context, args []string) error {
	saved, err := installation.ReadAdmin()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("login", flag.ContinueOnError)
	base := f.String("gateway", env("CONTROL_GATEWAY", saved.Gateway), "gateway URL")
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
	role, err := (client.Admin{URL: gatewayURL, Key: *key}).AuthRole(ctx)
	if err != nil {
		return err
	}
	if role != "user" && role != "superuser" && role != "common" {
		return errors.New("gateway returned an unsupported credential role")
	}
	if err := installation.SaveAdmin(installation.AdminProfile{Gateway: gatewayURL, Key: *key}); err != nil {
		return err
	}
	fmt.Printf("Logged in to %s as %s. API key saved for future commands.\n", gatewayURL, role)
	return nil
}
