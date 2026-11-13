//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/store"
)

func TestManagedExecPreservesPIDAndRestoresSelection(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
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
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(dir, "installed control")
	next := filepath.Join(dir, "next control")
	if err = os.WriteFile(installed, image, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(next, append(image, []byte("next fixture")...), 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state with spaces")
	args := []string{"__test-managed-exec", state, next, "argument with spaces"}
	for i := range 3 {
		// Starting the original installed CLI again must exec the retained
		// selection without overwriting runtime-previous.json.
		if i == 2 {
			// Legacy manual recovery records can name a launcher without version
			// metadata. They must still select and validate that exact image.
			if err = store.Write(filepath.Join(state, "runtime.json"), runtimeBinary{Path: next}); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.CommandContext(ctx, installed, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "CONTROL_TEST_CLI_MAIN=1", "CONTROL_TEST_EXEC_ENV=retained")
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("exec %d: %v: %s", i, err, output)
		}
		var result struct {
			PID              int
			CWD, Environment string
			Args             []string
		}
		if err = json.Unmarshal(output, &result); err != nil {
			t.Fatalf("invalid fixture output: %s: %v", output, err)
		}
		if result.PID != cmd.Process.Pid || result.CWD != dir || result.Environment != "retained" || fmt.Sprint(result.Args) != fmt.Sprint(args) {
			t.Fatalf("process handoff lost execution context: %+v, pid=%d", result, cmd.Process.Pid)
		}
		var current, previous runtimeBinary
		if err = store.Read(filepath.Join(state, "runtime.json"), &current); err != nil {
			t.Fatal(err)
		}
		if err = store.Read(filepath.Join(state, "runtime-previous.json"), &previous); err != nil {
			t.Fatal(err)
		}
		if current.Path != next || current.Version == "" || current.Asset.SHA256 == "" || previous.Path != installed {
			t.Fatalf("wrong runtime selections: %+v, %+v", current, previous)
		}
	}
}
