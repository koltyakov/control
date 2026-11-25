package main

import (
	"context"
	"errors"
	"flag"
	"strings"

	"github.com/koltyakov/control/internal/gateway"
)

const gatewayBackupUsage = "usage: control gateway backup create FILE [--data DIR] | control gateway backup verify FILE"

// gatewayBackupCLI runs local backup commands on the gateway host. Authority
// is filesystem access to the state directory as the gateway service account,
// not a gateway credential; nothing is sent over the network.
func gatewayBackupCLI(ctx context.Context, args []string) error {
	err := runGatewayBackup(ctx, args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func runGatewayBackup(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(gatewayBackupUsage)
	}
	switch args[0] {
	case "create":
		f := flag.NewFlagSet("gateway backup create", flag.ContinueOnError)
		data := f.String("data", ".control-gateway", "gateway state directory")
		file, err := parseBackupFile(f, args[1:])
		if err != nil {
			return err
		}
		info, err := gateway.Backup(ctx, *data, file)
		if err != nil {
			return err
		}
		printJSON(info)
		return nil
	case "verify":
		f := flag.NewFlagSet("gateway backup verify", flag.ContinueOnError)
		file, err := parseBackupFile(f, args[1:])
		if err != nil {
			return err
		}
		info, err := gateway.VerifyBackup(ctx, file)
		if err != nil {
			return err
		}
		printJSON(info)
		return nil
	}
	return errors.New(gatewayBackupUsage)
}

// parseBackupFile accepts the single FILE before or after the flags.
func parseBackupFile(f *flag.FlagSet, args []string) (string, error) {
	var file string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		file, args = args[0], args[1:]
	}
	if err := f.Parse(args); err != nil {
		return "", err
	}
	if file == "" && f.NArg() == 1 {
		return f.Arg(0), nil
	}
	if file == "" || f.NArg() != 0 {
		return "", errors.New(gatewayBackupUsage)
	}
	return file, nil
}
