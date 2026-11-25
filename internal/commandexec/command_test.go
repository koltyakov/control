package commandexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("CONTROL_COMMAND_EMPTY_ARGS") == "1" {
		_, _ = fmt.Fprint(os.Stdout, "empty-args")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Every process in these fixtures is an isolated copy of the Go test binary.
// No shell, desktop, installed applications or fleet credentials are used.
func TestCommandFixture(t *testing.T) {
	if len(os.Args) < 6 || os.Args[2] != "--" || os.Args[3] != "--command-fixture" {
		return
	}
	mode, path := os.Args[4], os.Args[5]
	switch mode {
	case "echo":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(91)
		}
		dir, _ := os.Getwd()
		_, _ = fmt.Fprintf(os.Stdout, "%s|%s|%s", input, os.Getenv("COMMAND_TEST_VALUE"), dir)
		_, _ = fmt.Fprint(os.Stderr, "diagnostic")
		os.Exit(0)
	case "exit":
		os.Exit(23)
	case "burn":
		burnFixture()
	case "burn-parent":
		// Waits for its burning child, so the child's usage is reaped into it.
		if fixtureCommand(context.Background(), "burn", path).Run() != nil {
			os.Exit(94)
		}
		os.Exit(0)
	case "argv":
		_, _ = fmt.Fprint(os.Stdout, os.Args[0])
		os.Exit(0)
	case "marker":
		if os.WriteFile(path, []byte("executed"), 0600) != nil {
			os.Exit(90)
		}
		os.Exit(0)
	case "verbose":
		for range 64 {
			if _, err := os.Stdout.Write(bytes.Repeat([]byte("x"), 64<<10)); err != nil {
				os.Exit(24)
			}
		}
		os.Exit(0)
	case "both":
		done := make(chan struct{})
		go func() {
			for range 64 {
				_, _ = os.Stderr.Write(bytes.Repeat([]byte("e"), 1024))
			}
			close(done)
		}()
		for range 64 {
			_, _ = os.Stdout.Write(bytes.Repeat([]byte("o"), 1024))
		}
		<-done
		os.Exit(0)
	case "runner", "gated-runner":
		if mode == "gated-runner" {
			var ready [1]byte
			if _, err := io.ReadFull(os.Stdin, ready[:]); err != nil {
				os.Exit(98)
			}
		}
		childMode := "cancel"
		if mode == "gated-runner" {
			childMode = "nested"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		child := fixtureCommand(ctx, childMode, path)
		if _, err := Run(ctx, child); err != nil {
			os.Exit(99)
		}
		os.Exit(0)
	case "leaf":
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(92)
		}
		if os.WriteFile(path, []byte(listener.Addr().String()), 0600) != nil {
			os.Exit(93)
		}
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				message, _ := io.ReadAll(io.LimitReader(conn, 32))
				_ = conn.Close()
				if string(message) == "stop-test-fixture" {
					os.Exit(0)
				}
			}
		}()
		// Self-expire even if a broken implementation leaks this test process.
		time.Sleep(10 * time.Second)
		_ = listener.Close()
		os.Exit(0)
	case "branch":
		child := fixtureCommand(context.Background(), "leaf", path)
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
		if child.Run() != nil {
			os.Exit(94)
		}
		os.Exit(0)
	case "inherited", "redirected", "nested", "cancel", "escaped":
		childMode := "leaf"
		if mode == "nested" {
			childMode = "branch"
		}
		child := fixtureCommand(context.Background(), childMode, path)
		if mode == "escaped" {
			detachFixture(child)
		}
		if mode != "redirected" {
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
		}
		if child.Start() != nil {
			os.Exit(95)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(path); err == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(96)
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, _ = fmt.Fprint(os.Stdout, "started")
		if mode == "cancel" {
			time.Sleep(10 * time.Second)
		}
		os.Exit(0)
	default:
		os.Exit(97)
	}
}

func fixtureCommand(ctx context.Context, mode, path string) *exec.Cmd {
	executable, err := os.Executable()
	if err != nil {
		panic(err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestCommandFixture$", "--", "--command-fixture", mode, path)
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	cmd.WaitDelay = 500 * time.Millisecond
	return cmd
}

func TestRunCommandIOAndStatus(t *testing.T) {
	dir := t.TempDir()
	actualDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := fixtureCommand(t.Context(), "echo", "")
	cmd.Dir = dir
	cmd.Env = append(cmd.Env, "COMMAND_TEST_VALUE=argument-value")
	cmd.Stdin = strings.NewReader("input-value")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	exit, err := Run(t.Context(), cmd)
	if err != nil || exit != 0 || stdout.String() != "input-value|argument-value|"+actualDir || stderr.String() != "diagnostic" {
		t.Fatalf("command I/O: exit=%d err=%v stdout=%q stderr=%q", exit, err, stdout.String(), stderr.String())
	}
	exit, err = Run(t.Context(), fixtureCommand(t.Context(), "exit", ""))
	if exit != 23 || err == nil {
		t.Fatalf("original exit status lost: %d, %v", exit, err)
	}
	missing := exec.CommandContext(t.Context(), filepath.Join(t.TempDir(), "absent-command"))
	if exit, err := Run(t.Context(), missing); exit != -1 || err == nil {
		t.Fatalf("missing command: %d, %v", exit, err)
	}
}

func TestRunCommandDescendants(t *testing.T) {
	for _, mode := range []string{"inherited", "redirected", "nested", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "listener")
			cmd := fixtureCommand(ctx, mode, path)
			var output, diagnostic bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &diagnostic
			type result struct {
				exit int
				err  error
			}
			done := make(chan result, 1)
			go func() {
				exit, err := Run(ctx, cmd)
				done <- result{exit, err}
			}()
			if mode == "cancel" {
				deadline := time.NewTimer(5 * time.Second)
				defer deadline.Stop()
				for {
					if _, err := os.Stat(path); err == nil {
						cancel()
						break
					}
					select {
					case result := <-done:
						t.Fatalf("command finished before cancellation: %+v", result)
					case <-deadline.C:
						cancel()
						<-done
						t.Fatal("descendant did not become ready")
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			got := <-done
			if mode == "cancel" {
				if got.err == nil {
					t.Fatal("cancelled command succeeded")
				}
			} else if got.err != nil || got.exit != 0 || output.String() != "started" {
				t.Fatalf("exit=%d err=%v output=%q stderr=%q", got.exit, got.err, output.String(), diagnostic.String())
			}
			address, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer stopFixture(address)
			// Job/group termination can briefly precede the kernel's descriptor
			// release. Wait a bounded interval, never submit another command.
			deadline := time.Now().Add(time.Second)
			for {
				conn, err := net.DialTimeout("tcp", string(address), 50*time.Millisecond)
				if err != nil {
					break
				}
				_ = conn.Close()
				if time.Now().After(deadline) {
					t.Fatal("descendant listener survived command cleanup")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestRunCommandAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	exit, err := Run(ctx, fixtureCommand(ctx, "echo", ""))
	if exit != -1 || err == nil {
		t.Fatalf("cancelled setup: %d %v", exit, err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRunCommandOutputFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	cmd := fixtureCommand(ctx, "verbose", "")
	writeErr := errors.New("synthetic log storage failure")
	cmd.Stdout = failingWriter{writeErr}
	started := time.Now()
	_, err := Run(ctx, cmd)
	if !errors.Is(err, writeErr) || errors.Is(err, exec.ErrWaitDelay) || ctx.Err() != nil {
		t.Fatalf("output failure was lost or misclassified: %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("output failure waited for the execution deadline")
	}
}

func TestRunCommandSharedWriter(t *testing.T) {
	cmd := fixtureCommand(t.Context(), "both", "")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	exit, err := Run(t.Context(), cmd)
	if err != nil || exit != 0 || output.Len() != 128<<10 {
		t.Fatalf("shared output: exit=%d err=%v bytes=%d", exit, err, output.Len())
	}
}

func TestRunCommandArguments(t *testing.T) {
	cmd := fixtureCommand(t.Context(), "argv", "")
	cmd.Args[0] = "custom-argv-zero"
	var output bytes.Buffer
	cmd.Stdout = &output
	if exit, err := Run(t.Context(), cmd); exit != 0 || err != nil || output.String() != "custom-argv-zero" {
		t.Fatalf("argv zero: exit=%d err=%v output=%q", exit, err, output.String())
	}
	output.Reset()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd = &exec.Cmd{Path: executable, Env: append(os.Environ(), "CONTROL_COMMAND_EMPTY_ARGS=1"), Stdout: &output}
	if exit, err := Run(t.Context(), cmd); exit != 0 || err != nil || output.String() != "empty-args" {
		t.Fatalf("empty args: exit=%d err=%v output=%q", exit, err, output.String())
	}
}

func TestRunCommandParentExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "listener")
	parent := fixtureCommand(ctx, "runner", path)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Process.Kill() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = parent.Process.Kill()
			_ = parent.Wait()
			t.Fatal("descendant did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := parent.Process.Kill(); err != nil {
		_ = parent.Wait()
		t.Fatal(err)
	}
	_ = parent.Wait()
	address, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer stopFixture(address)
	deadline = time.Now().Add(time.Second)
	for {
		conn, err := net.DialTimeout("tcp", string(address), 50*time.Millisecond)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("descendant survived its execution owner's exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stopFixture(address []byte) {
	conn, err := net.DialTimeout("tcp", string(address), time.Second)
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write([]byte("stop-test-fixture"))
	_ = conn.Close()
}

func TestHelperGatePreventsExecution(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "gate closed", true: "cancel before adoption"}[cancelled], func(t *testing.T) {
			status, statusWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = status.Close(); _ = statusWriter.Close() }()
			gateReader, gate, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = gateReader.Close(); _ = gate.Close() }()
			path := filepath.Join(t.TempDir(), "effect")
			requested := fixtureCommand(t.Context(), "marker", path)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			helper := exec.CommandContext(t.Context(), executable)
			helper.Env = requested.Env
			owner, handles, err := prepare(helper, statusWriter, gateReader)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.close()
			helper.Args = append([]string{executable, helperMode, handles[0], handles[1], requested.Path}, requested.Args...)
			if err := helper.Start(); err != nil {
				t.Fatal(err)
			}
			_ = statusWriter.Close()
			_ = gateReader.Close()
			if cancelled {
				_ = helper.Cancel()
			}
			_ = gate.Close() // Never authorize the requested executable.
			_ = owner.terminate(helper.Process)
			_ = helper.Wait()
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("requested command ran before gate authorization", err)
			}
		})
	}
}
