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
	const usage = "usage: control upload start NODE LOCAL_PATH REMOTE_PATH [--id ID --timeout DURATION --recursive --skip-unsupported] | resume NODE ID LOCAL_PATH [--timeout DURATION] | status|abort NODE ID"
	if len(args) < 3 {
		return errors.New(usage)
	}
	if args[0] == "status" || args[0] == "abort" {
		if len(args) != 3 {
			return errors.New(usage)
		}
		result, err := c.UploadState(ctx, args[1], args[2], args[0] == "abort")
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
	var recursive, skipUnsupported bool
	if !s.Resume {
		f.StringVar(&s.ID, "id", "", "unique upload ID for recovery")
		f.BoolVar(&recursive, "recursive", false, "upload a local directory tree into a new remote directory")
		f.BoolVar(&skipUnsupported, "skip-unsupported", false, "with --recursive, skip and list symlinks, special files and unportable names instead of refusing")
	}
	timeout := f.Duration("timeout", time.Hour, "attempt deadline, up to 24h")
	if err := f.Parse(args[4:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *timeout < time.Second || *timeout > 24*time.Hour || (skipUnsupported && !recursive) {
		return errors.New(usage)
	}
	info, statErr := os.Lstat(s.LocalPath)
	directory := statErr == nil && info.IsDir()
	if s.Resume {
		directory = directory || c.IsDirectoryUpload(s.ID)
	} else if directory != recursive {
		if directory {
			return errors.New("LOCAL_PATH is a directory; add --recursive to upload the tree")
		}
		return errors.New("--recursive requires LOCAL_PATH to be a directory")
	}
	if s.ID == "" {
		s.ID = identity.NewID()
	}
	s.TimeoutSeconds = int(*timeout / time.Second)
	fmt.Fprintln(os.Stderr, "upload", s.ID)
	if directory {
		return uploadDirectoryCLI(ctx, c, client.DirUploadSpec{Node: s.Node, ID: s.ID, LocalPath: s.LocalPath, Path: s.Path, TimeoutSeconds: s.TimeoutSeconds, SkipUnsupported: skipUnsupported, Resume: s.Resume})
	}
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

// uploadDirectoryCLI prints one line per attempted file and the compact
// summary, including after a failure, so partial state stays visible.
func uploadDirectoryCLI(ctx context.Context, c client.Client, s client.DirUploadSpec) error {
	done := 0
	s.FileDone = func(f client.DirUploadFile) {
		done++
		if f.Error != "" {
			fmt.Fprintf(os.Stderr, "upload %s: %s %s: %s\n", s.ID, f.Path, f.State, f.Error)
			return
		}
		fmt.Fprintf(os.Stderr, "upload %s: %s %s (%d this attempt)\n", s.ID, f.Path, f.State, done)
	}
	result, err := c.UploadDirectory(ctx, s)
	if result.NodeID != "" {
		printJSON(result)
	}
	return err
}
