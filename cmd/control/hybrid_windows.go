package main

import (
	"context"
	"errors"
	"flag"
	"path/filepath"

	"github.com/koltyakov/control/internal/installation"
)

// Dispatch before loading any User profile or opening its logs with elevation.
func hybridUserCLI(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("__hybrid-user", flag.ContinueOnError)
	config := f.String("config", installation.ConfigPath(), "User profile")
	link := f.String("url", "", "installation URL")
	preflight := f.Bool("preflight", false, "check User enrollment only")
	autoName := f.Bool("auto-name", false, "require automatic naming")
	limited := f.Bool("limited", false, "limited-token child")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *link == "" {
		return errors.New("hybrid User helper requires an installation URL and no positional arguments")
	}
	path, err := filepath.Abs(*config)
	if err != nil {
		return err
	}
	if !*limited {
		return installation.RunHybridUser(ctx, path, *link, *preflight, *autoName)
	}
	if err = installation.CheckHybridUser(); err != nil {
		return err
	}
	enroll := []string{"--url", *link, "--service", "user"}
	if *preflight {
		enroll = append(enroll, "--preflight")
	}
	if *autoName {
		enroll = append(enroll, "--auto-name")
	}
	// Firewall setup belongs to the elevated protected parent, never to a UAC
	// invocation of the newly installed, user-writable CLI.
	return enrollCLI(ctx, enroll, path)
}
