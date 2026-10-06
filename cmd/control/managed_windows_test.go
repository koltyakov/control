package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/processutil"
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
