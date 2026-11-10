//go:build !windows

package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"golang.org/x/sys/unix"
)

func TestFileReadersRejectFIFOWithoutWaitingForWriter(t *testing.T) {
	n := localNode(t, nil)
	for _, method := range []string{"files.read", "artifacts.export", "task output"} {
		t.Run(method, func(t *testing.T) {
			relative := "pipe"
			if method == "task output" {
				relative = filepath.Join("tasks", "fifo-output", "pipe")
			}
			name := filepath.Join(n.Config.WorkDir, relative)
			if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(name, 0600); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Remove(name) }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				switch method {
				case "files.read":
					_, err = n.filesRead(ctx, model.JSON(map[string]string{"path": relative}), Execution{})
				case "artifacts.export":
					_, err = n.artifactMethod(ctx, n.Identity.ID, method, model.JSON(map[string]string{"path": relative}))
				case "task output":
					_, err = n.publishTaskOutputs(ctx, "fifo-output", []string{"pipe"})
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("accepted FIFO as a regular file")
				}
			case <-time.After(time.Second):
				// Unblock a regressed blocking open so test/node cleanup can join.
				cancel()
				fd, err := unix.Open(name, unix.O_RDWR|unix.O_NONBLOCK, 0)
				if err == nil {
					_ = unix.Close(fd)
				}
				<-done
				t.Fatal("FIFO read waited for a writer instead of rejecting it")
			}
		})
	}
}
