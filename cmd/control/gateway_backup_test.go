package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/gateway"
)

// Online backup is an operator command: only verified superuser help lists it.
func TestGatewayBackupHelpIsOperatorOnly(t *testing.T) {
	for role, help := range completionRoleHelp {
		if strings.Contains(help, "backup") != (role == "superuser") {
			t.Errorf("%s help backup listing = %v", role, strings.Contains(help, "backup"))
		}
	}
	s := parseCompletionHelp(completionRoleHelp["superuser"])
	if got := completionNodeAt(t, s, "gateway backup").childNames(); strings.Join(got, " ") != "create verify" {
		t.Fatalf("gateway backup subcommands %v", got)
	}
}

func TestGatewayBackupCLI(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	dir := t.TempDir()
	g, err := gateway.New(dir, "common-key-for-backup-cli-tests")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	out := filepath.Join(t.TempDir(), "gateway.backup")
	if err := run(t.Context(), []string{"gateway", "backup", "create", out, "--data", dir}); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"gateway", "backup", "verify", out}); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"gateway", "backup", "create", "--data", dir, out}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second backup to the same file: %v", err)
	}
	for _, args := range [][]string{{"gateway", "backup"}, {"gateway", "backup", "restore", out}, {"gateway", "backup", "create"}, {"gateway", "backup", "verify", out, "extra"}} {
		if err := run(t.Context(), args); err == nil || !strings.Contains(err.Error(), "usage: control gateway backup") {
			t.Errorf("%q: %v", args, err)
		}
	}
	// A mistyped subcommand is refused instead of starting a gateway here.
	if err := run(t.Context(), []string{"gateway", "--data", t.TempDir(), "backups"}); err == nil || !strings.Contains(err.Error(), "unexpected gateway arguments") {
		t.Fatalf("unknown gateway argument: %v", err)
	}
}
