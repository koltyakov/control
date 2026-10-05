package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/koltyakov/control/internal/installation"
)

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
