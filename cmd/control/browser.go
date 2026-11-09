package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/koltyakov/control/internal/browser"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

func browserCLI(ctx context.Context, c client.Client, args []string) error {
	const usage = "usage: control browser run NODE [--session NAME] -- COMMAND [ARG...] | control browser view NODE [--port 4848] [--listen 127.0.0.1:0]"
	if len(args) < 2 {
		return errors.New(usage)
	}
	switch args[0] {
	case "run":
		f := flag.NewFlagSet("browser run", flag.ContinueOnError)
		session := f.String("session", "", "browser session in the worker's private owner namespace")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		request := browser.Request{Session: *session, Args: f.Args()}
		if _, err := browser.Decode(model.JSON(request)); err != nil {
			return err
		}
		return callPrint(ctx, c, args[1], "browser.run", request)
	case "view":
		f := flag.NewFlagSet("browser view", flag.ContinueOnError)
		port := f.Int("port", 4848, "already running agent-browser dashboard port on the worker")
		listen := f.String("listen", "127.0.0.1:0", "local loopback listener")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New(usage)
		}
		forward, info, err := c.StartBrowserView(ctx, client.BrowserViewSpec{Node: args[1], Port: *port, Listen: *listen})
		if err != nil {
			return err
		}
		defer func() { _ = c.StopForward(info.ID) }()
		fmt.Println(info.URL)
		fmt.Println("Interactive worker browser dashboard. Ctrl+C closes this forward, not the browser or dashboard.")
		select {
		case <-ctx.Done():
			return nil
		case <-forward.Done():
			if message := forward.Info().LastError; message != "" {
				return errors.New(message)
			}
			return nil
		}
	default:
		return errors.New(usage)
	}
}
