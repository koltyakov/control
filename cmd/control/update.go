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
	"github.com/koltyakov/control/internal/installation"
	"golang.org/x/term"
)

var errUpdatePermission = errors.New("managed updates require the gateway superuser key; the selected login lacks update permission")

func authorizeUpdateCLI(ctx context.Context, c client.Admin, args []string, allowPrompt bool) error {
	f := flag.NewFlagSet("update authorize", flag.ContinueOnError)
	f.Usage = func() {}
	stdin := f.Bool("key-stdin", false, "read the gateway superuser key from standard input")
	check := f.Bool("check", false, "verify saved update authorization without prompting or changing credentials")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return adminCLI(ctx, c, []string{"update", "--help"})
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: control update authorize [--key-stdin | --check]")
	}
	if *check {
		if *stdin {
			return errors.New("use --check without --key-stdin")
		}
		return authorizeUpdates(ctx, c, nil)
	}
	var prompt func() (string, error)
	if *stdin {
		if !allowPrompt {
			return errors.New("use --key-stdin without --token or CONTROL_SUPERUSER_KEY")
		}
		c.Key = ""
		prompt = func() (string, error) {
			data, err := io.ReadAll(io.LimitReader(os.Stdin, 4097))
			return string(data), err
		}
	} else if allowPrompt && term.IsTerminal(int(os.Stdin.Fd())) {
		prompt = func() (string, error) {
			fmt.Fprintf(os.Stderr, "Gateway-wide updates need one-time operator authorization for %s. Your dashboard login will not change.\nGateway superuser key: ", c.URL)
			data, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			return string(data), err
		}
	}
	return authorizeUpdates(ctx, c, prompt)
}

func authorizeUpdates(ctx context.Context, c client.Admin, prompt func() (string, error)) error {
	if c.Key != "" {
		role, err := c.AuthRole(ctx)
		if err != nil {
			return err
		}
		if role == "superuser" {
			return nil
		}
	}
	if prompt == nil {
		return errUpdatePermission
	}
	key, err := prompt()
	if err != nil {
		return err
	}
	if len(key) > 4096 {
		return errors.New("API key exceeds 4096 bytes")
	}
	c.Key = strings.TrimSpace(key)
	if c.Key == "" {
		return errors.New("gateway superuser key is required")
	}
	role, err := c.AuthRole(ctx)
	if err != nil {
		return err
	}
	if role != "superuser" {
		return errors.New("the supplied key is not a gateway superuser key; update authorization was not saved")
	}
	if err := installation.SaveUpdateAdmin(installation.AdminProfile{Gateway: strings.TrimRight(c.URL, "/"), Key: c.Key}); err != nil {
		return fmt.Errorf("save update authorization: %w", err)
	}
	fmt.Fprintln(os.Stderr, "Saved gateway update authorization. Your dashboard login is unchanged.")
	return nil
}

func selfUpdateCLI(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("update", flag.ContinueOnError)
	version := f.String("version", os.Getenv("CONTROL_VERSION"), "release tag; default: latest stable release")
	f.Usage = func() {
		_, _ = fmt.Fprintln(f.Output(), "Usage: control update [--version TAG]\nUpdate this CLI from GitHub Releases. Alias: control upgrade.\nEnvironment: CONTROL_RELEASE_REPO, CONTROL_VERSION")
		f.PrintDefaults()
	}
	if len(args) == 1 && args[0] == "help" {
		f.Usage()
		return nil
	}
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: control update [--version TAG]")
	}
	fmt.Fprintln(os.Stderr, "Checking Control releases...")
	result, err := installation.UpdateSelf(ctx, releaseRepository(), *version)
	if err != nil {
		return err
	}
	if result.Changed {
		fmt.Printf("Updated Control to %s at %s\n", result.Version, result.Path)
	} else {
		fmt.Printf("Control %s is already installed at %s\n", result.Version, result.Path)
	}
	return nil
}
