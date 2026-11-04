package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/desktop"
)

func TestDesktopCLIHelperMode(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("CONTROL_TEST_CLI_MAIN", "1")
	t.Setenv("CONTROL_CONFIG", filepath.Join(home, "unreadable-profile"))
	t.Setenv("CONTROL_HOME", filepath.Join(home, "must-not-create-account-state"))
	text := "pipe-only-credential-canary"
	for _, args := range [][]string{{"__desktop"}, {"__desktop", "relative-private-path"}, {"__desktop", filepath.Join(home, "missing-private-library")}} {
		cmd := exec.CommandContext(t.Context(), executable, args...)
		cmd.Stdin = bytes.NewReader(desktop.Packet(desktop.Request{Command: "setValue", Ref: "@stest:e1", Text: &text}, home))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil || stderr.Len() != 0 {
			t.Fatal("helper mode entered ordinary CLI setup or emitted diagnostics")
		}
		var envelope struct {
			OK    bool
			Error struct{ Code string }
		}
		if json.Unmarshal(out, &envelope) != nil || envelope.OK || envelope.Error.Code != "NATIVE_HELPER_FAILED" || bytes.Contains(out, []byte(text)) || bytes.Contains(out, []byte(home)) || bytes.Contains(out, []byte("relative-private-path")) {
			t.Fatal("helper infrastructure failure was not private and fixed")
		}
	}
	if _, err := os.Stat(filepath.Join(home, "must-not-create-account-state")); !os.IsNotExist(err) {
		t.Fatal("helper initialized ordinary account state")
	}
}

// Optional real-library status smoke test. It makes no GUI mutations and needs
// no agent-desktop CLI, compiler, account, or enrolled node.
func TestDesktopCLIRealLibraryStatus(t *testing.T) {
	library := os.Getenv("CONTROL_TEST_DESKTOP_LIBRARY")
	if library == "" {
		t.Skip("set CONTROL_TEST_DESKTOP_LIBRARY to an ABI 6 or 7 native library")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("CONTROL_TEST_CLI_MAIN", "1")
	t.Setenv("AGENT_DESKTOP_SESSION", "must-not-inherit")
	t.Setenv("AGENT_DESKTOP_HOME", filepath.Join(home, "wrong-state"))
	cmd := exec.CommandContext(t.Context(), executable, "__desktop", library)
	cmd.Stdin = bytes.NewReader(desktop.Packet(desktop.Request{Command: "status"}, home))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || stderr.Len() != 0 {
		t.Fatal("native status failed or emitted diagnostics; output withheld")
	}
	var envelope struct {
		OK   bool
		Data struct {
			StateRoot string  `json:"state_root"`
			SessionID *string `json:"session_id"`
			Tracing   bool
			Platform  string
		}
	}
	if json.Unmarshal(out, &envelope) != nil || !envelope.OK || envelope.Data.StateRoot != home || envelope.Data.SessionID != nil || envelope.Data.Tracing || envelope.Data.Platform == "" {
		t.Fatal("real native status or private environment isolation failed; output withheld")
	}
}
