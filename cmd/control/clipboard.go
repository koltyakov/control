package main

import (
	"context"
	"errors"
	"flag"
	"time"

	"github.com/koltyakov/control/internal/client"
)

func clipboardCLI(ctx context.Context, c client.Client, args []string) error {
	if len(args) < 2 || args[0] != "paste" || args[1] == "" {
		return errors.New("usage: control clipboard paste NODE [--reverse] [--dir PATH] [--timeout DURATION]")
	}
	f := flag.NewFlagSet("clipboard paste", flag.ContinueOnError)
	reverse := f.Bool("reverse", false, "paste the remote clipboard onto this machine")
	dir := f.String("dir", ".", "existing destination directory for files; remote paths are relative to workDir")
	timeout := f.Duration("timeout", client.DefaultClipboardTimeout, "paste deadline, including file transfer")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected clipboard arguments")
	}
	if *timeout < time.Second || *timeout > client.MaxClipboardTimeout {
		return errors.New("clipboard timeout must be between 1s and 24h")
	}
	result, err := c.PasteClipboard(ctx, client.ClipboardSpec{Node: args[1], Reverse: *reverse, Dir: *dir, TimeoutSeconds: int(timeout.Seconds())})
	if err != nil {
		return err
	}
	printJSON(result)
	return nil
}
