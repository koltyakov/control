package node

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestShellStartingDirectoryDefaults(t *testing.T) {
	homeEnv := "HOME"
	if runtime.GOOS == "windows" {
		homeEnv = "USERPROFILE"
	}
	home, work := t.TempDir(), t.TempDir()
	t.Setenv(homeEnv, home)

	for _, tc := range []struct {
		name, runAs, want string
		instructionOnly   bool
		hybridSystem      bool
	}{
		{name: "User", runAs: "user", want: home},
		{name: "instruction-only User", runAs: "user", instructionOnly: true, want: home},
		{name: "System", runAs: "system", want: work},
		{name: "Hybrid System", runAs: "system", instructionOnly: true, hybridSystem: true, want: work},
		{name: "Hybrid System with unexpected OS context", runAs: "user", instructionOnly: true, hybridSystem: true, want: work},
		{name: "unknown OS context", want: work},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{WorkDir: work, InstructionOnly: tc.instructionOnly, HybridSystem: tc.hybridSystem}
			got, err := cfg.shellStartDir(tc.runAs)
			if got != tc.want || err != nil {
				t.Fatalf("shell starting directory = %q, %v; want %q", got, err, tc.want)
			}
		})
	}

	t.Setenv(homeEnv, "")
	cfg := Config{WorkDir: work}
	if got, err := cfg.shellStartDir("user"); got != "" || err == nil {
		t.Fatalf("User shell with no home = %q, %v; want an error", got, err)
	}
	if got, err := cfg.shellStartDir("system"); got != work || err != nil {
		t.Fatalf("System shell with no home = %q, %v; want %q", got, err, work)
	}
}

func TestShellOpenWorkerPolicy(t *testing.T) {
	source, worker, consumer := cluster(t, true, func(i int, cfg *Config) {
		if i == 1 {
			cfg.ShellEnabled = true
			cfg.Allow = map[string][]string{
				"source":   {"node.describe", "exec.run"},
				"consumer": {"node.describe", model.ShellOpenMethod},
			}
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	q := model.ShellRequest{Shell: "bash", Cols: 80, Rows: 24}
	if conn, _, err := testOpen(t, source, ctx, "worker", model.ShellOpenMethod, q); err == nil {
		_ = conn.Close()
		t.Fatal("exec.run permission also allowed shell.open")
	} else if !strings.Contains(err.Error(), "not authorized") {
		t.Fatal("unexpected denial", err)
	}
	invalid := q
	invalid.Cols = 32768
	if conn, _, err := testOpen(t, consumer, ctx, "worker", model.ShellOpenMethod, invalid); err == nil {
		_ = conn.Close()
		t.Fatal("opened a terminal with invalid dimensions")
	} else if !strings.Contains(err.Error(), "terminal dimensions") {
		t.Fatal("expected request validation, got", err)
	}
	if !worker.pauseForUpdate() {
		t.Fatal("idle worker did not pause")
	}
	defer worker.resumeAfterUpdate()
	// Drop an open request during maintenance before the worker can reserve
	// a slot or start a terminal. The receiver must not wait for resumption.
	local, remote := net.Pipe()
	defer func() { _ = local.Close(); _ = remote.Close() }()
	done := make(chan error, 1)
	caller := testOrchestrator(t, consumer).peer.IdentityID()
	go func() {
		done <- worker.startShellStream(ctx, caller, remote, q)
	}()
	_ = local.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("opened a shell during maintenance")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not release pending open")
	}
}

type retainedShellOutput struct {
	*io.PipeReader
	done <-chan struct{}
}

func (r retainedShellOutput) Done() <-chan struct{} { return r.done }
func (retainedShellOutput) Wait() (int, error)      { return 0, errors.New("no shell exit") }

func TestShellCancellationClosesRetainedTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	local, remote := net.Pipe()
	defer func() { _ = local.Close(); _ = remote.Close() }()
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	terminal := retainedShellOutput{PipeReader: reader, done: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- writeShellOutput(ctx, terminal, remote) }()
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("interrupted terminal reported a clean exit")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled shell stayed attached to a terminal with an inherited output descriptor")
	}
}
