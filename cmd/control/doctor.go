package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/installation"
)

const doctorUsageText = `usage: control doctor [NODE] [--json] [--timeout DURATION] [--check-timeout DURATION]

Read-only checks of the saved login, gateway, versions, clock, local node and
proxy environment. Without NODE it adds one line per fleet machine from gateway
status and opens no peer session. With NODE it also opens this client's peer
session and reads node.describe and diagnostics.get when permitted.

Doctor starts no task, requests no grant or lease, writes no worker file, sends
no wake packet, runs no connection test and retries nothing. Instruction-only
destinations are not diagnosed beyond discovery. Exit status is 1 when any
check failed; warnings and skipped checks do not fail.

`

func doctorCLI(ctx context.Context, c client.Client, global *flag.FlagSet, profile string, args []string) error {
	return runDoctorCLI(ctx, os.Stdout, args, func(opts client.DoctorOptions) (client.DoctorReport, error) {
		var override *string
		global.Visit(func(f *flag.Flag) {
			if f.Name == "token" {
				value := f.Value.String()
				override = &value
			}
		})
		gateway, err := gatewayClient(profile, override)
		if err != nil {
			return client.DoctorReport{}, err
		}
		cfg, err := installation.ReadConfig(profile)
		if err != nil {
			return client.DoctorReport{}, err
		}
		opts.Gateway, opts.LoginSource = gateway, doctorLoginSource(profile, override, gateway.Key)
		opts.LocalAPI, opts.LocalNode, opts.RelayOnly = c.URL, cfg.Name, cfg.RelayOnly
		// Probing never sends a credential to a non-loopback plain-HTTP API;
		// an unauthenticated refusal still shows that a node answers.
		if client.SafeCredentialURL(c.URL) {
			opts.LocalToken = c.Token
		}
		return c.Doctor(ctx, opts), nil
	})
}

// runDoctorCLI parses arguments, runs diagnose and prints the report. It
// returns an error, and so a nonzero exit, only when a check failed.
func runDoctorCLI(_ context.Context, out io.Writer, args []string, diagnose func(client.DoctorOptions) (client.DoctorReport, error)) error {
	f := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := f.Bool("json", false, "print the stable JSON report: checks and summary")
	timeout := f.Duration("timeout", client.DefaultDoctorTimeout, "overall deadline, 1s..10m")
	checkTimeout := f.Duration("check-timeout", client.DefaultDoctorCheckTimeout, "deadline for each check, 1s..timeout")
	f.SetOutput(out)
	f.Usage = func() {
		_, _ = io.WriteString(out, doctorUsageText)
		f.PrintDefaults()
	}
	node := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		node, args = args[0], args[1:]
	}
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if node == "" && f.NArg() == 1 {
		node = f.Arg(0)
	} else if f.NArg() != 0 {
		return errors.New("usage: control doctor [NODE] [--json] [--timeout DURATION] [--check-timeout DURATION]")
	}
	if *timeout < time.Second || *timeout > 10*time.Minute {
		return errors.New("doctor --timeout must be between 1s and 10m")
	}
	if *checkTimeout < time.Second || *checkTimeout > *timeout {
		return errors.New("doctor --check-timeout must be between 1s and --timeout")
	}
	report, err := diagnose(client.DoctorOptions{Node: node, Timeout: *timeout, CheckTimeout: *checkTimeout})
	if err != nil {
		return err
	}
	if *asJSON {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintln(out, string(b)); err != nil {
			return err
		}
	} else if err = report.WriteText(out); err != nil {
		return err
	}
	if report.Failed() {
		return fmt.Errorf("doctor: %d of %d checks failed", report.Summary.Failed, len(report.Checks))
	}
	return nil
}

// doctorLoginSource names where the gateway key came from, never its value.
func doctorLoginSource(profile string, override *string, key string) string {
	if key == "" {
		return ""
	}
	if override != nil {
		return "--token"
	}
	for _, name := range []string{"CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY", "CONTROL_TOKEN"} {
		if os.Getenv(name) == key {
			return "environment variable " + name
		}
	}
	if saved, err := installation.ReadAdmin(); err == nil && saved.Key == key {
		return "saved login"
	}
	if cfg, err := installation.ReadConfig(profile); err == nil && cfg.Token == key {
		return "this host's node profile"
	}
	return ""
}
