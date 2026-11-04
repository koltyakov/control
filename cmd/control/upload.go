package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/upload"
)

func uploadCLI(ctx context.Context, c client.Client, args []string) error {
	const usage = "usage: control upload start NODE LOCAL_PATH REMOTE_PATH [--id ID --timeout DURATION] | resume NODE ID LOCAL_PATH [--timeout DURATION] | status|abort NODE ID"
	if len(args) < 3 {
		return errors.New(usage)
	}
	if args[0] == "status" || args[0] == "abort" {
		if len(args) != 3 {
			return errors.New(usage)
		}
		result, err := c.UploadStatus(ctx, args[1], args[2], args[0] == "abort")
		if err != nil {
			return err
		}
		printJSON(result)
		return nil
	}
	if len(args) < 4 || (args[0] != "start" && args[0] != "resume") {
		return errors.New(usage)
	}
	s := client.UploadSpec{Node: args[1], Resume: args[0] == "resume"}
	if s.Resume {
		s.ID = args[2]
		s.LocalPath = args[3]
	} else {
		s.LocalPath = args[2]
		s.Path = args[3]
	}
	f := flag.NewFlagSet("upload "+args[0], flag.ContinueOnError)
	if !s.Resume {
		f.StringVar(&s.ID, "id", "", "unique upload ID for recovery")
	}
	timeout := f.Duration("timeout", time.Hour, "attempt deadline, up to 24h")
	if err := f.Parse(args[4:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *timeout < time.Second || *timeout > 24*time.Hour {
		return errors.New(usage)
	}
	if s.ID == "" {
		s.ID = identity.NewID()
	}
	s.TimeoutSeconds = int(*timeout / time.Second)
	fmt.Fprintln(os.Stderr, "upload", s.ID)
	s.Progress = func(p upload.Result) {
		fmt.Fprintf(os.Stderr, "upload %s: %d/%d bytes durable (%s)\n", s.ID, p.Offset, p.Size, p.State)
	}
	result, err := c.Upload(ctx, s)
	if err != nil {
		return err
	}
	printJSON(result)
	return nil
}
