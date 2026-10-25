package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/koltyakov/control/internal/client"
)

func wakeCLI(ctx context.Context, c client.Client, args []string) error {
	if len(args) == 0 || args[0] == "" {
		return errors.New("usage: control wake NODE [--from WORKER|local] [--lan LAN] [--json]")
	}
	f := flag.NewFlagSet("wake", flag.ContinueOnError)
	from := f.String("from", "", "sender worker; omitted selects an online same-LAN node; local sends from this machine")
	lan := f.String("lan", "", "known LAN identifier, required with --from local")
	jsonOutput := f.Bool("json", false, "print packet-send metadata")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected wake arguments")
	}
	result, err := c.Wake(ctx, client.WakeSpec{Node: args[0], From: *from, LAN: *lan})
	if err != nil {
		return err
	}
	if *jsonOutput {
		printJSON(result)
	} else {
		fmt.Printf("Sent %d Wake-on-LAN packet(s) via %s. Wake-up is not confirmed.\n", result.Packets, result.Source)
	}
	return nil
}
