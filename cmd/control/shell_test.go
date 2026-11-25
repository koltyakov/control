package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestShellCLI(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	t.Setenv("CONTROL_API", "")
	t.Setenv("CONTROL_CONFIG", "")
	t.Setenv("CONTROL_TEST_CLI_MAIN", "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		args []string
		want string
		ok   bool
	}{
		{[]string{"shell", "--help"}, shellUsage, true},
		{[]string{"shell", "worker"}, shellUsage, false},
		{[]string{"shell", "", "bash"}, shellUsage, false},
		{[]string{"shell", "worker", "--", "/bin/bash"}, shellUsage, false},
		{[]string{"shell", "worker", "cmd", "/c"}, shellUsage, false},
		{[]string{"shell", "worker", "--", "pwsh", "-Command", "exit"}, shellUsage, false},
		{[]string{"--api", "http://localhost", "shell", "worker", "bash"}, "requires account-client routing", false},
		{[]string{"shell", "worker", "powershell"}, "requires a terminal", false},
		{[]string{"shell", "worker", "--", "pwsh"}, "requires a terminal", false},
	} {
		output, err := exec.CommandContext(t.Context(), binary, tt.args...).CombinedOutput()
		if (err == nil) != tt.ok || !strings.Contains(string(output), tt.want) {
			t.Errorf("control %q: %v, %s", tt.args, err, output)
		}
	}
}

type testShellStream struct {
	net.Conn
	resizes chan shellSize
	code    int
}

func (s *testShellStream) Resize(cols, rows uint16) error {
	s.resizes <- shellSize{cols, rows}
	return nil
}

func (s *testShellStream) ExitCode() (int, bool) { return s.code, s.code >= 0 }

func TestShellRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	local, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	deadline, _ := ctx.Deadline()
	if err := peer.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	stream := &testShellStream{Conn: local, code: 37, resizes: make(chan shellSize, 8)}
	newSize := shellSize{120, 40}
	var output bytes.Buffer
	done := make(chan error, 1)
	inputDone := make(chan struct{})
	go func() {
		done <- relayShell(ctx, stream, &output, func(ctx context.Context, w io.Writer) error {
			defer close(inputDone)
			if _, err := w.Write([]byte{0x03}); err != nil {
				return err
			}
			<-ctx.Done() // No more keypresses, including after remote exit.
			return ctx.Err()
		}, shellSize{80, 24}, func() (shellSize, error) { return newSize, nil })
	}()
	var key [1]byte
	if _, err := io.ReadFull(peer, key[:]); err != nil || key[0] != 0x03 {
		t.Fatalf("remote Ctrl+C = %v, %v", key, err)
	}
	select {
	case got := <-stream.resizes:
		if got != newSize {
			t.Fatalf("resize = %v", got)
		}
	case <-ctx.Done():
		t.Fatal("resize not forwarded")
	}
	data := []byte("\x1b[31mlast line\r\n")
	if _, err := peer.Write(data); err != nil {
		t.Fatal(err)
	}
	_ = peer.Close()
	if err := <-done; !errors.Is(err, shellExitStatus(37)) || !slices.Equal(output.Bytes(), data) {
		t.Fatalf("shell returned %v, output %q", err, output.Bytes())
	}
	select {
	case <-inputDone:
	default:
		t.Fatal("returned with input still running")
	}
}

func TestShellRelayCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	local, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	stream := &testShellStream{Conn: local, code: -1}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- relayShell(ctx, stream, io.Discard, func(_ context.Context, w io.Writer) error {
			close(started)
			_, err := w.Write([]byte("blocked input"))
			return err
		}, shellSize{}, func() (shellSize, error) { return shellSize{}, nil })
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not unblock the stream")
	}
}

type outputOnlyShell struct{ *testShellStream }

func (outputOnlyShell) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestShellRelayDrainsAfterRemoteInputCloses(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	local, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	deadline, _ := ctx.Deadline()
	if err := peer.SetWriteDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	stream := outputOnlyShell{&testShellStream{Conn: local, code: 37}}
	var out bytes.Buffer
	inputDone := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- relayShell(ctx, stream, &out, func(_ context.Context, w io.Writer) error {
			defer close(inputDone)
			_, err := io.WriteString(w, "input already closed")
			return err
		}, shellSize{}, func() (shellSize, error) { return shellSize{}, nil })
	}()
	<-inputDone
	if _, err := io.WriteString(peer, "last output"); err != nil {
		t.Fatal(err)
	}
	_ = peer.Close()
	if err := <-done; !errors.Is(err, shellExitStatus(37)) || out.String() != "last output" {
		t.Fatalf("shell returned %v, output %q", err, out.String())
	}
}
