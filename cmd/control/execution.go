package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func taskStartCLI(ctx context.Context, c client.Client, args []string) error {
	const usage = "usage: control task start NODE JSON|@file [--wait] [--spec] [--check]"
	if len(args) < 2 {
		return errors.New(usage)
	}
	f := flag.NewFlagSet("task start", flag.ContinueOnError)
	wait := f.Bool("wait", false, "wait for the task result after durable acceptance")
	includeSpec := f.Bool("spec", false, "include the submitted specification in the printed task")
	check := f.Bool("check", false, "report whether the node would accept the specification, without submitting it")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() == 0 {
		return errors.New(usage)
	}
	value := f.Arg(0)
	// Accept flags before or after the specification without interpreting JSON.
	if err := f.Parse(f.Args()[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New(usage)
	}
	b, err := readJSON(value)
	if err != nil {
		return err
	}
	var spec model.TaskSpec
	if err = json.Unmarshal(b, &spec); err != nil {
		return err
	}
	if *check {
		if *wait || *includeSpec {
			return errors.New("--check submits nothing and cannot be combined with --wait or --spec")
		}
		return checkTaskCLI(ctx, c, args[0], spec)
	}
	return submit(ctx, c, args[0], spec, *wait, *includeSpec)
}

// checkTaskCLI prints the advisory preflight report. A failing check exits
// nonzero; nothing is submitted either way.
func checkTaskCLI(ctx context.Context, c client.Client, target string, spec model.TaskSpec) error {
	report, err := c.CheckTask(ctx, target, spec)
	if err != nil {
		return err
	}
	printJSON(report)
	if !report.OK {
		return errors.New("task check failed; nothing was submitted, and the report is advisory")
	}
	return nil
}

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
	return submit(ctx, c, args[0], spec, !*detach, false)
}

func logsCLI(ctx context.Context, c client.Client, target string, args []string) error {
	f := flag.NewFlagSet("task logs", flag.ContinueOnError)
	follow := f.Bool("follow", false, "stream logs until completion or cancellation")
	offset := f.Int64("offset", 0, "starting byte offset")
	tail := f.Bool("tail", false, "read the retained end of a log that exceeded the head limit, by absolute offset")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *offset < 0 || (*tail && *follow) {
		return errors.New("usage: control task logs NODE ID [--follow | --tail] [--offset BYTES]")
	}
	if *follow {
		return c.FollowLogs(ctx, target, args[0], *offset, os.Stdout)
	}
	params := map[string]any{"id": args[0], "offset": *offset}
	if *tail {
		// Older workers ignore this field and return an ordinary head read.
		params["tail"] = true
	}
	return callPrint(ctx, c, target, "tasks.logs", params)
}

func tunnelCLI(ctx context.Context, c client.Client, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "start", "list", "dispose", "stop":
			return persistentTunnelCLI(ctx, c, args)
		}
	}
	const usage = "usage: control tunnel NODE HOST:PORT [--reverse] [--listen 127.0.0.1:PORT], or control tunnel NODE --socks [--listen 127.0.0.1:PORT] [--allow-non-loopback]"
	if len(args) < 2 {
		return errors.New(usage)
	}
	// SOCKS mode has no destination argument; flags follow NODE directly.
	address, flags := args[1], args[2:]
	if strings.HasPrefix(address, "-") {
		address, flags = "", args[1:]
	}
	f := flag.NewFlagSet("tunnel", flag.ContinueOnError)
	listen := f.String("listen", "127.0.0.1:0", "listen address, on the remote machine with --reverse")
	reverse := f.Bool("reverse", false, "forward a remote listener to HOST:PORT on this machine")
	socks := f.Bool("socks", false, "run a local SOCKS5 proxy; NODE resolves and dials each requested host")
	allowNonLoopback := f.Bool("allow-non-loopback", false, "permit a non-loopback --socks listener, an unauthenticated open proxy into the fleet")
	if err := f.Parse(flags); err != nil {
		return err
	}
	if f.NArg() != 0 || (address == "") != *socks {
		return errors.New(usage)
	}
	forward, err := c.StartForward(ctx, client.ForwardSpec{Node: args[0], Address: address, Listen: *listen, Reverse: *reverse, SOCKS: *socks, AllowNonLoopback: *allowNonLoopback})
	if err != nil {
		return err
	}
	defer forward.Close()
	if *socks {
		fmt.Fprintln(os.Stderr, "SOCKS5 proxy listening on", forward.Info().Listen, "through", args[0])
	} else if *reverse {
		fmt.Fprintln(os.Stderr, "listening on", args[0], forward.Info().Listen, "forwarding to local", args[1])
	} else {
		fmt.Fprintln(os.Stderr, "listening on", forward.Info().Listen)
	}
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

func persistentTunnelCLI(ctx context.Context, c client.Client, args []string) error {
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: control tunnel list")
		}
		infos, err := c.PersistentTunnels(ctx)
		if err != nil {
			return err
		}
		printJSON(infos)
		return nil
	case "dispose", "stop":
		if len(args) != 2 {
			return errors.New("usage: control tunnel dispose ID")
		}
		if err := c.DisposePersistentTunnel(ctx, args[1]); err != nil {
			return err
		}
		printJSON(map[string]any{"id": args[1], "disposed": true})
		return nil
	case "start":
		if len(args) < 3 {
			return errors.New("usage: control tunnel start NODE HOST:PORT --listen HOST:PORT [--reverse] [--id ID] [--ttl DURATION]")
		}
		f := flag.NewFlagSet("tunnel start", flag.ContinueOnError)
		listen := f.String("listen", "", "fixed listener address, on the remote machine with --reverse")
		reverse := f.Bool("reverse", false, "forward a remote listener to HOST:PORT on this machine")
		id := f.String("id", "", "stable tunnel ID for reconciliation")
		ttl := f.Duration("ttl", 0, "expire after this duration; zero keeps the tunnel until disposed")
		if err := f.Parse(args[3:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *ttl < 0 || (*ttl != 0 && (*ttl < time.Second || *ttl%time.Second != 0)) {
			return errors.New("unexpected arguments or TTL; use whole seconds, or zero for no expiry")
		}
		if *id == "" {
			*id = identity.NewID()
		}
		fmt.Fprintln(os.Stderr, "tunnel", *id)
		info, err := c.StartPersistentTunnel(ctx, client.PersistentTunnelSpec{ID: *id, TTLSeconds: int64(*ttl / time.Second), ForwardSpec: client.ForwardSpec{Node: args[1], Address: args[2], Listen: *listen, Reverse: *reverse}})
		if err != nil {
			return fmt.Errorf("start tunnel %s; list tunnels to reconcile before retrying: %w", *id, err)
		}
		printJSON(info)
		return nil
	default:
		return errors.New("unknown persistent tunnel command")
	}
}
