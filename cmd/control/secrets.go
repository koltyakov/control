package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/secrets"
	"golang.org/x/term"
)

// Credentials are entered on the worker, never as CLI arguments or MCP inputs.
func secretsCLI(ctx context.Context, args []string, profile string) error {
	if len(args) == 0 {
		return errors.New("usage: control secrets set NAME [--stdin] | list | delete NAME")
	}
	cfg, err := node.LoadConfig(profile)
	if err != nil {
		return err
	}
	s := secrets.Store{Dir: filepath.Join(cfg.DataDir, "secrets"), WorkDir: cfg.WorkDir}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: control secrets list")
		}
		names, err := s.Names()
		if err != nil {
			return err
		}
		printJSON(names)
		return nil
	case "delete":
		if len(args) != 2 {
			return errors.New("usage: control secrets delete NAME")
		}
		return s.Delete(ctx, args[1])
	case "set":
		if len(args) < 2 || !secrets.ValidName(args[1]) {
			return errors.New("usage: control secrets set NAME [--stdin]")
		}
		f := flag.NewFlagSet("secrets set", flag.ContinueOnError)
		stdin := f.Bool("stdin", false, "read the value from stdin instead of a hidden prompt")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("secret values cannot be supplied as arguments")
		}
		var b []byte
		if *stdin {
			b, err = io.ReadAll(io.LimitReader(os.Stdin, secrets.MaxValue+3))
			// Strip one transport newline, not spaces belonging to the credential.
			b = []byte(strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"))
		} else {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return errors.New("enter secrets in a terminal or use --stdin outside an AI conversation")
			}
			fmt.Fprint(os.Stderr, "Secret value: ")
			b, err = term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
		}
		if err != nil {
			return errors.New("cannot read secret value")
		}
		if err := s.Set(ctx, args[1], string(b)); err != nil {
			return err
		}
		fmt.Println("Secret saved. Its value cannot be retrieved through Control tools.")
		return nil
	default:
		return errors.New("unknown secrets command; use set, list, or delete")
	}
}
