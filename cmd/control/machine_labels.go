package main

import (
	"context"
	"errors"
	"flag"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

const machineLabelUsage = "usage: control machines label NAME [KEY=VALUE|KEY-]... [--reset KEY]"

// machineLabelCLI changes the fleet owner's label overlay for one machine, or
// shows its labels and their origin when no change is given. machineCLI has
// already required an owner credential.
func machineLabelCLI(ctx context.Context, c client.Admin, args []string) error {
	if len(args) == 0 {
		return errors.New(machineLabelUsage)
	}
	f := flag.NewFlagSet("machines label", flag.ContinueOnError)
	var reset []string
	f.Func("reset", "discard the owner's entry for KEY so the node-reported value applies again; repeatable", func(key string) error {
		reset = append(reset, key)
		return nil
	})
	// Changes and --reset may interleave; keys never start with "-".
	var changes []string
	rest := args[1:]
	for {
		if err := f.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if f.NArg() == 0 {
			break
		}
		changes = append(changes, f.Arg(0))
		rest = f.Args()[1:]
	}
	show := len(changes) == 0 && len(reset) == 0
	var patch model.LabelPatch
	if !show {
		var err error
		if patch, err = client.ParseLabelChanges(changes, reset); err != nil {
			return err
		}
	}
	n, err := c.ResolveMachine(ctx, args[0])
	if err != nil {
		return err
	}
	if show {
		printJSON(model.LabelView(n))
		return nil
	}
	view, err := c.LabelMachine(ctx, n.ID, patch)
	if err != nil {
		return err
	}
	printJSON(view)
	return nil
}
