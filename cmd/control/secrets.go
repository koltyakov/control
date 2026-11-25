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

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/secrets"
	"golang.org/x/term"
)

// Credentials use hidden input or explicit stdin, never CLI arguments or MCP.
func secretsCLI(ctx context.Context, args []string, profile string, tokenOverride ...*string) error {
	if len(args) == 0 {
		return errors.New("usage: control secrets set NAME [--stdin] [--node WORKER] [--use USES --host PATTERN --plain-http | --clear-policy] | list [--metadata] [--node WORKER] | delete NAME [--node WORKER] | policy NAME (--use USES [--host PATTERN] [--plain-http] | --clear)")
	}
	operation, name, rest := args[0], "", args[1:]
	if operation == "set" || operation == "delete" || operation == "policy" {
		if len(rest) == 0 || !secrets.ValidName(rest[0]) {
			return errors.New("secret set, delete and policy require a valid name")
		}
		name, rest = rest[0], rest[1:]
	} else if operation != "list" {
		return errors.New("unknown secrets command; use set, list, delete, or policy")
	}
	f := flag.NewFlagSet("secrets "+operation, flag.ContinueOnError)
	target := f.String("node", "", "manage secrets on this explicit worker using fleet account authorization")
	var stdin, metadata, plainHTTP, clearPolicy bool
	var uses string
	var hosts []string
	if operation == "set" {
		f.BoolVar(&stdin, "stdin", false, "read the value from stdin instead of a hidden prompt")
		f.BoolVar(&clearPolicy, "clear-policy", false, "remove the use policy, making the secret unrestricted")
	}
	if operation == "policy" {
		f.BoolVar(&clearPolicy, "clear", false, "remove the use policy, making the secret unrestricted")
	}
	if operation == "set" || operation == "policy" {
		f.StringVar(&uses, "use", "", "replace the use policy: comma-separated http, exec, desktop")
		f.Func("host", "permitted HTTP destination pattern; repeatable", func(value string) error {
			hosts = append(hosts, value)
			return nil
		})
		f.BoolVar(&plainHTTP, "plain-http", false, "also permit unencrypted http:// destinations")
	}
	if operation == "list" {
		f.BoolVar(&metadata, "metadata", false, "include timestamps and use policies, never values")
	}
	if err := f.Parse(rest); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected secret arguments; values cannot be supplied as arguments")
	}
	changePolicy := uses != "" || len(hosts) != 0 || plainHTTP || clearPolicy
	var policy *secrets.Policy
	switch {
	case clearPolicy && (uses != "" || len(hosts) != 0 || plainHTTP):
		return errors.New("clearing a use policy cannot be combined with --use, --host or --plain-http")
	case changePolicy && !clearPolicy && uses == "":
		return errors.New("a use policy requires --use with http, exec and/or desktop")
	case changePolicy && !clearPolicy:
		var err error
		if policy, err = secrets.NewPolicy(strings.Split(uses, ","), hosts, plainHTTP); err != nil {
			return err
		}
	case operation == "policy" && !changePolicy:
		return errors.New("secrets policy requires --use with optional --host and --plain-http, or --clear")
	}
	if operation == "policy" && *target != "" {
		// Remote envelopes are encrypted but not sender-authenticated, so a
		// value-free remote policy change would let a compromised gateway
		// weaken the restriction of a credential it cannot read.
		return errors.New("remote policy changes re-enter the value: use control secrets set NAME --node WORKER with --use/--host or --clear-policy")
	}
	emptyTarget := false
	f.Visit(func(option *flag.Flag) {
		if option.Name == "node" && *target == "" {
			emptyTarget = true
		}
	})
	if emptyTarget {
		return errors.New("--node requires an explicit worker name or ID; no local fallback")
	}
	var s secrets.Store
	var remote client.Admin
	var id string
	var listing secrets.RemoteList
	var err error
	if *target != "" {
		var override *string
		if len(tokenOverride) != 0 {
			override = tokenOverride[0]
		}
		remote, err = gatewayClient(profile, override)
		if err != nil {
			return err
		}
		id, listing, err = remote.SecretTarget(ctx, *target)
	} else {
		var cfg node.Config
		cfg, err = node.LoadConfig(profile)
		if err == nil {
			s = secrets.Store{Dir: filepath.Join(cfg.DataDir, "secrets"), WorkDir: cfg.WorkDir}
		}
	}
	if err != nil {
		return err
	}
	switch operation {
	case "list":
		entries := listing.Secrets
		if *target == "" {
			entries, err = s.List()
		}
		if err != nil {
			return err
		}
		if metadata {
			printJSON(entries)
		} else {
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name)
			}
			printJSON(names)
		}
		return nil
	case "delete":
		if *target != "" {
			return remote.DeleteSecret(ctx, id, name)
		}
		return s.Delete(ctx, name)
	case "policy":
		if err := s.SetPolicy(ctx, name, policy); err != nil {
			return err
		}
		if policy == nil {
			fmt.Println("Secret use policy cleared; the secret is unrestricted.")
		} else {
			fmt.Println("Secret use policy saved. The value was not changed.")
		}
		return nil
	case "set":
		if changePolicy && *target != "" && !listing.Policies {
			// Refuse before reading the value: an older worker would store
			// it unrestricted.
			return client.ErrSecretPolicyUnsupported
		}
		var b []byte
		if stdin {
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
		defer clear(b)
		switch {
		case *target == "" && changePolicy:
			err = s.SetWithPolicy(ctx, name, string(b), policy)
		case *target == "":
			err = s.Set(ctx, name, string(b))
		case changePolicy:
			err = remote.SetSecretWithPolicy(ctx, id, name, string(b), policy, listing)
		default:
			// A value-only write keeps the worker's stored policy.
			err = remote.SetSecret(ctx, id, name, string(b), listing.Key)
		}
		if err != nil {
			return err
		}
		fmt.Println("Secret saved. Its value cannot be retrieved through Control tools.")
		return nil
	}
	return nil
}
