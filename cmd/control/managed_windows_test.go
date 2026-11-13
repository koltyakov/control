package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/processutil"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

func TestWindowsManagedExecutableStablePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile with spaces")
	for i, data := range []string{"original launcher", "verified update"} {
		source := filepath.Join(t.TempDir(), "control.exe")
		if err := os.WriteFile(source, []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(data))
		current := runtimeBinary{Path: source, Version: "test", Asset: update.Asset{Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}}
		path, err := managedExecutable(context.Background(), dir, current)
		if err != nil || path != installation.WindowsRuntimePath(dir) {
			t.Fatalf("runtime %d: %q, %v", i, path, err)
		}
		if err := update.Verify(path, current.Asset); err != nil {
			t.Fatal(err)
		}
		if original, err := os.ReadFile(source); err != nil || string(original) != data {
			t.Fatalf("versioned source changed: %q, %v", original, err)
		}
		if again, err := managedExecutable(context.Background(), dir, current); err != nil || again != path {
			t.Fatalf("repeated preparation changed runtime: %q, %v", again, err)
		}
	}
	// A corrupt next source must leave the previous verified launch copy intact.
	path := installation.WindowsRuntimePath(dir)
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "bad.exe")
	if err := os.WriteFile(source, []byte("bad"), 0700); err != nil {
		t.Fatal(err)
	}
	current := runtimeBinary{Path: source, Version: "bad", Asset: update.Asset{Size: 3, SHA256: hex.EncodeToString(make([]byte, 32))}}
	if _, err := managedExecutable(context.Background(), dir, current); err == nil {
		t.Fatal("corrupt executable was published")
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(prior) {
		t.Fatalf("failed publication changed prior runtime: %q, %v", after, err)
	}
}

func TestWindowsGUIRuntimeReportsOnlyVerifiedSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.exe")
	if err = os.WriteFile(source, image, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(image)
	selected := runtimeBinary{Path: source, Version: buildinfo.Version, Asset: update.Asset{Size: int64(len(image)), SHA256: hex.EncodeToString(sum[:])}}
	path, err := update.PrepareWindowsRuntime(dir, selected, true)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, path, "__test-gui-source", source)
	cmd.Env = append(os.Environ(), "CONTROL_TEST_CLI_MAIN=1")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("GUI source validation: %v: %s", err, output)
	}
	var info struct {
		buildinfo.Info
		ActualSHA256 string
	}
	if err = json.Unmarshal(output, &info); err != nil || info.SHA256 != selected.Asset.SHA256 {
		t.Fatalf("GUI runtime did not retain source identity: %s, %v", output, err)
	}
	_, derived, err := update.WindowsRuntimeImage(selected, true)
	if err != nil || info.ActualSHA256 != derived.SHA256 || info.ActualSHA256 == info.SHA256 {
		t.Fatalf("actual GUI bytes cannot be identified for helper copies: %+v, %v", info, err)
	}
	if err = os.WriteFile(source, append(image, byte(1)), 0700); err != nil {
		t.Fatal(err)
	}
	cmd = exec.CommandContext(ctx, path, "__test-gui-source", source)
	cmd.Env = append(os.Environ(), "CONTROL_TEST_CLI_MAIN=1")
	if output, err = cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "does not match") {
		t.Fatalf("GUI runtime accepted a different source: %s, %v", output, err)
	}
}

func TestWindowsTemporaryHandoffReplacesRuntimeAndFinishes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	installed, next := filepath.Join(dir, "installed.exe"), filepath.Join(dir, "next.exe")
	if err = os.WriteFile(installed, image, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(next, append(image, []byte("next fixture")...), 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state with spaces")
	log, err := os.Create(filepath.Join(dir, "handoff.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	cmd := exec.CommandContext(ctx, installed, "__test-managed-exec", state, next, "argument with spaces")
	processutil.HideWindow(cmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CONTROL_TEST_CLI_MAIN=1", "CONTROL_TEST_EXEC_ENV=retained")
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Run(); err != nil {
		data, _ := os.ReadFile(log.Name())
		t.Fatalf("handoff parent: %v: %s", err, data)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		var result struct {
			PID         int
			Environment string
		}
		resultErr := store.Read(filepath.Join(state, "result.json"), &result)
		_, markerErr := os.Stat(filepath.Join(state, "runtime-starting.json"))
		acks, globErr := filepath.Glob(filepath.Join(state, "runtime-ready-*.json"))
		if resultErr == nil && os.IsNotExist(markerErr) && globErr == nil && len(acks) == 0 {
			if result.PID == cmd.Process.Pid || result.Environment != "retained" {
				t.Fatalf("invalid replacement context: %+v", result)
			}
			var current, previous runtimeBinary
			if err = store.Read(filepath.Join(state, "runtime.json"), &current); err != nil {
				t.Fatal(err)
			}
			if err = store.Read(filepath.Join(state, "runtime-previous.json"), &previous); err != nil {
				t.Fatal(err)
			}
			if current.Path != next || previous.Path != installed {
				t.Fatalf("wrong runtime selections: %+v, %+v", current, previous)
			}
			return
		}
		select {
		case <-ctx.Done():
			data, _ := os.ReadFile(log.Name())
			t.Fatalf("temporary updater did not finish: %s", data)
		case <-ticker.C:
		}
	}
}

func TestWindowsHandoffEOFDoesNotCommit(t *testing.T) {
	t.Run("reject-foreign-endpoints", func(t *testing.T) {
		for _, endpoint := range [][]string{
			{"192.0.2.1:1234", strings.Repeat("a", 64)},
			{"127.0.0.1:1234", "short"},
			{"127.0.0.1:1234"},
		} {
			if err := runUserManagedHandoff(t.Context(), endpoint); err == nil {
				t.Fatalf("invalid User helper endpoint accepted: %q", endpoint)
			}
		}
	})
	for _, transport := range []string{"stdio", "user-pipe"} {
		t.Run(transport, func(t *testing.T) { windowsHandoffEOFDoesNotCommit(t, transport) })
	}
}

func windowsHandoffEOFDoesNotCommit(t *testing.T, transport string) {
	t.Setenv("CONTROL_TEST_CLI_MAIN", "1")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	next, err := inspectBinary(ctx, executable)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = store.Write(filepath.Join(dir, "runtime-request.json"), next); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "__update-handoff")
	processutil.HideWindow(cmd)
	var input io.WriteCloser
	var reader *bufio.Reader
	var listener net.Listener
	token := strings.Repeat("a", 64)
	if transport == "stdio" {
		input, err = cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = output.Close() }()
		reader = bufio.NewReader(output)
	} else {
		listener, err = listenUserManagedHandoff(identity.NewID())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
		defer stop()
		cmd.Args = append(cmd.Args, listener.Addr().String(), token)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	if listener != nil {
		conn, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		deadline, _ := ctx.Deadline()
		if err = conn.SetDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		input, reader = conn, bufio.NewReader(conn)
		if line, err := reader.ReadString('\n'); err != nil || line != token+"\n" {
			t.Fatalf("User updater did not authenticate: %q, %v", line, err)
		}
	}
	defer func() { _ = input.Close() }()
	request := windowsHandoffRequest{ID: "0123456789abcdef0123456789abcdef", Parent: os.Getpid(), Dir: dir, Next: next}
	if err = json.NewEncoder(input).Encode(request); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("updater did not prepare: %q, %v", line, err)
	}
	_ = input.Close()
	if err = cmd.Wait(); err == nil {
		t.Fatal("uncommitted updater reported success")
	}
	if _, err = os.Stat(filepath.Join(dir, "runtime.json")); !os.IsNotExist(err) {
		t.Fatal("EOF changed runtime selection")
	}
	if _, err = os.Stat(installation.WindowsRuntimePath(dir)); !os.IsNotExist(err) {
		t.Fatal("EOF replaced runtime executable")
	}
}

func TestWindowsManagedExecutableLaunchesVerifiedBinary(t *testing.T) {
	if marker := os.Getenv("CONTROL_TEST_STABLE_RUNTIME_CHILD"); marker != "" {
		fmt.Println(marker)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "profile with spaces")
	for i := range 2 {
		// PE executables accept trailing data. Distinct valid fixtures exercise
		// replacement after a real child exits, without Go or an installed node.
		payload := append(append([]byte(nil), data...), byte(i))
		source := filepath.Join(t.TempDir(), "control.exe")
		if err := os.WriteFile(source, payload, 0700); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(payload)
		current := runtimeBinary{Path: source, Version: "test", Asset: update.Asset{Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}}
		path, err := managedExecutable(ctx, dir, current)
		if err != nil || path != installation.WindowsRuntimePath(dir) {
			t.Fatalf("prepare child %d: %q, %v", i, path, err)
		}
		cmd := exec.CommandContext(ctx, path, "-test.run=^TestWindowsManagedExecutableLaunchesVerifiedBinary$")
		processutil.HideWindow(cmd)
		marker := fmt.Sprintf("verified runtime child %d", i)
		cmd.Env = append(os.Environ(), "CONTROL_TEST_STABLE_RUNTIME_CHILD="+marker)
		if output, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(output), marker) {
			t.Fatalf("launch child %d: %v: %s", i, err, output)
		}
	}
}
