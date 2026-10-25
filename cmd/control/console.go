package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/koltyakov/control/internal/client"
	"golang.org/x/term"
)

func consoleCLI(ctx context.Context, c client.Admin, args []string) error {
	const usage = "usage: control console password [USER] [--password-stdin]"
	if len(args) == 0 || args[0] != "password" {
		return errors.New(usage)
	}
	username := ""
	options := args[1:]
	if len(options) > 0 && !strings.HasPrefix(options[0], "-") {
		username, options = options[0], options[1:]
	}
	f := flag.NewFlagSet("console password", flag.ContinueOnError)
	stdin := f.Bool("password-stdin", false, "read a browser password from standard input")
	if err := f.Parse(options); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New(usage)
	}
	role, err := c.AuthRole(ctx)
	if err != nil {
		return err
	}
	if role != "user" && role != "superuser" {
		return errors.New("browser password provisioning requires an account or superuser key")
	}
	var password []byte
	if *stdin {
		password, err = io.ReadAll(io.LimitReader(os.Stdin, 1027))
		password = []byte(strings.TrimSuffix(strings.TrimSuffix(string(password), "\n"), "\r"))
	} else if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "New browser password: ")
		password, err = term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err == nil {
			fmt.Fprint(os.Stderr, "Confirm password: ")
			var confirm []byte
			confirm, err = term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err == nil && string(password) != string(confirm) {
				return errors.New("passwords do not match")
			}
		}
	} else {
		return errors.New("use --password-stdin when not running in a terminal")
	}
	if err != nil {
		return err
	}
	if len(password) < 12 || len(password) > 1024 {
		return errors.New("password must contain 12..1024 bytes")
	}
	if err := c.JSON(ctx, http.MethodPut, "/v1/console/password", map[string]string{"username": username, "password": string(password)}, nil); err != nil {
		return err
	}
	if username == "" && role == "superuser" {
		username = "@operator"
	}
	if username == "" {
		username = "your fleet account name"
	}
	fmt.Printf("Browser password saved. Sign in at %s as %s. Existing browser sessions were revoked.\n", c.URL, username)
	return nil
}
