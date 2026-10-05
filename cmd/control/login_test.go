package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/installation"
)

func TestLoginPersistsValidatedAccountForSubsequentCommands(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	for _, name := range []string{"CONTROL_GATEWAY", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY", "CONTROL_TOKEN", "CONTROL_CONFIG"} {
		t.Setenv(name, "")
	}
	const super = "login-test-superuser-key-1234567890"
	g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: super, Software: buildinfo.Current()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	s := httptest.NewServer(g.Handler())
	t.Cleanup(s.Close)
	var account struct{ Token string }
	ctx := context.Background()
	if err := (client.Admin{URL: s.URL, Key: super}).JSON(ctx, "POST", "/v1/admin/users", map[string]string{"name": "login-user"}, &account); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"login", "--gateway", s.URL + "/", "--api-key", account.Token}); err != nil {
		t.Fatal(err)
	}
	saved, err := installation.ReadAdmin()
	if err != nil || saved.Gateway != s.URL || saved.Key != account.Token {
		t.Fatal("login was not saved correctly", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(installation.Home(), "admin.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("credential file is not private", err)
		}
	}
	for _, args := range [][]string{{"machines"}, {"machines", "invites"}, {"machines", "add", "worker", "--platform", runtime.GOOS + "/" + runtime.GOARCH}, {"dashboard", "--json"}} {
		if err := run(ctx, args); err != nil {
			t.Fatalf("saved login failed for %v: %v", args, err)
		}
	}
	if err := run(ctx, []string{"login", "--gateway", s.URL, "--api-key", "wrong-key"}); err == nil {
		t.Fatal("invalid login accepted")
	}
	after, err := installation.ReadAdmin()
	if err != nil || after != saved {
		t.Fatal("failed login replaced working credentials", err)
	}
	t.Setenv("CONTROL_GATEWAY", "https://different.example")
	if c := adminClient(installation.ConfigPath()); c.Key != "" {
		t.Fatal("saved credential sent to another gateway")
	}
}
