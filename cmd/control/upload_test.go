package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/node"
)

func TestUploadCLI(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	for _, key := range []string{"CONTROL_GATEWAY", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY", "CONTROL_TOKEN", "CONTROL_CONFIG", "CONTROL_API"} {
		t.Setenv(key, "")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const account = "upload-cli-account-key-1234567890"
	const common = "upload-cli-common-key-1234567890"
	g, err := gateway.New(t.TempDir(), common, gateway.Options{SuperuserKey: account})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	s := httptest.NewServer(g.Handler())
	defer s.Close()
	if err = installation.SaveAdmin(installation.AdminProfile{Gateway: s.URL, Key: account}); err != nil {
		t.Fatal(err)
	}
	w, err := node.New(node.Config{Name: "worker", Gateway: s.URL, Token: common, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err = w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	c := (client.Client{}).WithStandalone(ctx, client.StandaloneConfig{Gateway: client.Admin{URL: s.URL, Key: account}, StateDir: t.TempDir(), RelayOnly: true, AccountRouting: true})
	defer func() { _ = c.Close() }()
	file := filepath.Join(t.TempDir(), "local.bin")
	data := bytes.Repeat([]byte("cli\x00"), 100000)
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"start", "worker", file, "remote.bin", "--id", "cli"}, {"status", "worker", "cli"}, {"resume", "worker", "cli", file}, {"abort", "worker", "cli"}} {
		if err = uploadCLI(ctx, c, args); err != nil {
			t.Fatal(args, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(w.Config.WorkDir, "remote.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("CLI content", err)
	}
	for _, args := range [][]string{{}, {"start", "worker"}, {"start", "worker", file, "remote.bin", "--timeout", "25h"}, {"status", "worker", "cli", "unexpected"}} {
		if err = uploadCLI(ctx, c, args); err == nil {
			t.Fatal("bad CLI arguments accepted", args)
		}
	}
	// Go through the real entrypoint too: command routing must initialize an
	// account client from saved login without requiring a local node profile.
	for _, args := range [][]string{{"upload", "start", "worker", file, "entrypoint.bin", "--id", "entrypoint"}, {"upload", "status", "worker", "entrypoint"}, {"upload", "resume", "worker", "entrypoint", file}, {"upload", "abort", "worker", "entrypoint"}} {
		if err = run(ctx, args); err != nil {
			t.Fatal("saved-login CLI entrypoint", args, err)
		}
	}
	got, err = os.ReadFile(filepath.Join(w.Config.WorkDir, "entrypoint.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("entrypoint content", err)
	}
}
