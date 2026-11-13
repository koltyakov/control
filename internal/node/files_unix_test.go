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

func TestFileListAndWriteRejectFIFOWithoutRetainingWork(t *testing.T) {
	for _, method := range []string{"files.list", "files.write"} {
		t.Run(method, func(t *testing.T) {
			n := localNode(t, func(cfg *Config) { cfg.MaxOperations = 1 })
			name := filepath.Join(n.Config.WorkDir, "pipe")
			if err := unix.Mkfifo(name, 0600); err != nil {
				t.Fatal(err)
			}
			// Keep the recovery endpoint open until blocked operations have
			// finished, so even a regressed FIFO open cannot strand test cleanup.
			recoverFIFO := func() {
				fd, err := unix.Open(name, unix.O_RDWR|unix.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = unix.Close(fd) })
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- n.Call(ctx, "", method, map[string]any{"path": "pipe", "data": "eA==", "truncate": true}, nil)
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("accepted FIFO")
				}
			case <-time.After(time.Second):
				cancel()
				recoverFIFO()
				<-done
				t.Fatal("FIFO open waited for another endpoint")
			}
			assertPool(t, n.operationsPool.snapshot(), 1, 0, 0)
			if !n.pauseForUpdate() {
				t.Fatal("rejected FIFO retained work admission")
			}
			n.work.Resume()
			task, err := n.startTaskContext(t.Context(), n.Identity.ID, model.TaskSpec{ID: "fifo", Capability: method, Args: model.JSON(map[string]any{"path": "pipe", "data": "eA==", "truncate": true})})
			if err != nil {
				t.Fatal(err)
			}
			doneTask := make(chan model.Task, 1)
			go func() {
				result, _ := n.waitTask(t.Context(), task.Owner, task.ID, 5*time.Second)
				doneTask <- result
			}()
			select {
			case result := <-doneTask:
				if result.State != "failed" {
					recoverFIFO()
					t.Fatal("tracked special-file operation did not fail", result)
				}
			case <-time.After(time.Second):
				recoverFIFO()
				<-doneTask
				t.Fatal("tracked FIFO operation retained work")
			}
			assertPool(t, n.operationsPool.snapshot(), 1, 0, 0)
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFileWriteRejectsOpenFIFOAndEscapedSymlink(t *testing.T) {
	n := localNode(t, nil)
	name := filepath.Join(n.Config.WorkDir, "pipe")
	if err := unix.Mkfifo(name, 0600); err != nil {
		t.Fatal(err)
	}
	reader, err := unix.Open(name, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(reader) }()
	// A reader lets a nonblocking write-open succeed. The opened handle must
	// still be rejected before either truncation or writing can occur.
	if f, err := openRegularFileForWrite(n.root, "pipe", true); err == nil || f != nil {
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("accepted an open FIFO as a regular file", err)
	}
	info, err := os.Stat(name)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("rejected FIFO was modified", info, err)
	}
	data := make([]byte, 1)
	if size, err := unix.Read(reader, data); size > 0 || (err != nil && err != unix.EAGAIN) {
		t.Fatal("rejected FIFO received bytes", size, err)
	}
	outside := filepath.Join(t.TempDir(), "protected")
	if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(n.Config.WorkDir, "escaped")); err != nil {
		t.Fatal(err)
	}
	if f, err := openRegularFileForWrite(n.root, "escaped", true); err == nil || f != nil {
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("opened an escaped symlink for truncation", err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "unchanged" {
		t.Fatal("escaped file was modified", string(got), err)
	}
	if err := os.WriteFile(filepath.Join(n.Config.WorkDir, "regular"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("regular", filepath.Join(n.Config.WorkDir, "alias")); err != nil {
		t.Fatal(err)
	}
	// Root-confined symlinks to regular files keep their existing semantics.
	f, err := openRegularFileForWrite(n.root, "alias", true)
	if err != nil {
		t.Fatal("root-confined regular-file alias was rejected", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(n.Config.WorkDir, "regular")); err != nil || len(got) != 0 {
		t.Fatal("root-confined alias did not truncate its regular target", string(got), err)
	}
}
