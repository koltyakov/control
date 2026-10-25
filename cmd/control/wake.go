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
		return errors.New("usage: control wake NODE [--from WORKER|local] [--json]")
	}
	f := flag.NewFlagSet("wake", flag.ContinueOnError)
	from := f.String("from", "", "sender worker or local; omitted automatically selects a verified same-LAN sender")
	jsonOutput := f.Bool("json", false, "print packet-send metadata")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected wake arguments")
	}
	result, err := c.Wake(ctx, client.WakeSpec{Node: args[0], From: *from})
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
