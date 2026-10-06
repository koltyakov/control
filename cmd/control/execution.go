package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

func execCLI(ctx context.Context, c client.Client, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: control exec NODE [--detach] [--id ID] [--timeout DURATION] [--] COMMAND [ARG...]")
	}
	f := flag.NewFlagSet("exec", flag.ContinueOnError)
	detach := f.Bool("detach", false, "return after durable acceptance instead of waiting")
	id := f.String("id", "", "stable task ID for submission recovery")
	timeout := f.Duration("timeout", time.Hour, "task timeout including queue/input time, up to 24h")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() == 0 || *timeout < time.Second || *timeout > 24*time.Hour {
		return errors.New("provide a command and a timeout between 1s and 24h")
	}
	command := f.Args()
	spec := model.TaskSpec{ID: *id, Capability: "exec.run", TimeoutSeconds: int(*timeout / time.Second), Args: model.JSON(map[string]any{"command": command[0], "args": command[1:]})}
	return submit(ctx, c, args[0], spec, !*detach)
}

func logsCLI(ctx context.Context, c client.Client, target string, args []string) error {
	f := flag.NewFlagSet("task logs", flag.ContinueOnError)
	follow := f.Bool("follow", false, "stream logs until completion or cancellation")
	offset := f.Int64("offset", 0, "starting byte offset")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *offset < 0 {
		return errors.New("usage: control task logs NODE ID [--follow] [--offset BYTES]")
	}
	if *follow {
		return c.FollowLogs(ctx, target, args[0], *offset, os.Stdout)
	}
	return callPrint(ctx, c, target, "tasks.logs", map[string]any{"id": args[0], "offset": *offset})
}

func tunnelCLI(ctx context.Context, c client.Client, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: control tunnel NODE HOST:PORT [--listen 127.0.0.1:PORT]")
	}
	f := flag.NewFlagSet("tunnel", flag.ContinueOnError)
	listen := f.String("listen", "127.0.0.1:0", "local listen address")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected tunnel arguments")
	}
	forward, err := c.StartForward(ctx, client.ForwardSpec{Node: args[0], Address: args[1], Listen: *listen})
	if err != nil {
		return err
	}
	defer forward.Close()
	fmt.Fprintln(os.Stderr, "listening on", forward.Info().Listen)
	select {
	case <-ctx.Done():
		return nil
	case <-forward.Done():
		if err := forward.Info().LastError; err != "" {
			return errors.New(err)
		}
		return nil
	}
}
