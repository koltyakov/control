package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestRunbooksCLIWithoutConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(".control/runbooks/machines/trader", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".control/runbooks/machines/trader/connect-to-vpn.md", []byte("# Connect VPN\nApplication: GlobalProtect\nSecret: vpn.portal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("invalid.json", []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--config", "invalid.json", "runbooks", "--source", "project"},
		{"--config", "invalid.json", "runbooks", "--node", "trader", "--local", "--source", "project"},
		{"--config", "invalid.json", "runbooks", "--node", "trader", "--source", "project", "--file", "machines/trader/connect-to-vpn.md"},
	} {
		output, err := captureUpdatePushOutput(t, func() error { return run(context.Background(), args) })
		if err != nil || !strings.Contains(output, "GlobalProtect") || !strings.Contains(output, "vpn.portal") {
			t.Fatalf("runbooks command: %s, %v", output, err)
		}
	}
	for _, args := range [][]string{{"trader"}, {"--node", "../trader"}, {"--file", "shared/run.md"}, {"--source", "worker-user"}} {
		if _, err := captureUpdatePushOutput(t, func() error { return run(context.Background(), append([]string{"runbooks"}, args...)) }); err == nil {
			t.Fatalf("invalid query accepted: %q", args)
		}
	}
}
