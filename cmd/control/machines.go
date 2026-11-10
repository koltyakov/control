package main

import (
	"context"
	"errors"
	"flag"
	"strings"

	"github.com/koltyakov/control/internal/client"
)

func machinesCLI(ctx context.Context, c client.Admin, args []string) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return machineCLI(ctx, c, args)
	}
	f := flag.NewFlagSet("machines", flag.ContinueOnError)
	q := client.MachineQuery{}
	f.BoolVar(&q.Compact, "compact", false, "print concise JSON metadata without capability schemas or resource details")
	f.StringVar(&q.Node, "node", "", "filter by exact case-sensitive machine name or stable ID")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: control machines [--compact] [--node NAME_OR_ID]")
	}
	emptyNode := false
	f.Visit(func(option *flag.Flag) {
		if option.Name == "node" && q.Node == "" {
			emptyNode = true
		}
	})
	if emptyNode {
		return errors.New("--node requires a nonempty machine name or stable ID")
	}
	data, err := c.Machines(ctx, q)
	if err != nil {
		return err
	}
	printJSON(data)
	return nil
}
