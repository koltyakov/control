package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	for _, args := range [][]string{{"login"}, {"login", "--gateway", s.URL + "/"}, {"machines"}, {"machines", "invites"}, {"machines", "add", "worker", "--platform", runtime.GOOS + "/" + runtime.GOARCH}, {"dashboard", "--json"}} {
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

func TestSuperuserLoginPersistsUpdateAuthorizationAcrossFleetLogins(t *testing.T) {
	c, fleetLogin := updateAuthorizationEnvironment(t)
	if err := run(t.Context(), []string{"login", "--gateway", c.URL + "/", "--api-key", updateAuthorizationTestKey}); err != nil {
		t.Fatal(err)
	}
	operatorLogin := installation.AdminProfile{Gateway: c.URL, Key: updateAuthorizationTestKey}
	if saved, err := installation.ReadUpdateAdmin(); err != nil || saved != operatorLogin {
		t.Fatal("superuser login did not persist update authorization", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(installation.Home(), "update-admin.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("persisted update login is not private", err)
		}
	}
	if err := run(t.Context(), []string{"login"}); err != nil {
		t.Fatal("persisted operator login asked for the key again", err)
	}
	if err := run(t.Context(), []string{"login", "--api-key", c.Key}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		for _, args := range [][]string{{"login"}, {"update", "authorize", "--check"}, {"update", "status"}, {"machines"}, {"dashboard", "--json"}} {
			if err := run(t.Context(), args); err != nil {
				t.Fatalf("persisted login failed for %v: %v", args, err)
			}
		}
	}
	if saved, err := installation.ReadAdmin(); err != nil || saved != fleetLogin {
		t.Fatal("update commands changed the fleet login", err)
	}
	if saved, err := installation.ReadUpdateAdmin(); err != nil || saved != operatorLogin {
		t.Fatal("fleet login erased saved update authorization", err)
	}
	if role, err := adminClient(installation.ConfigPath()).AuthRole(t.Context()); err != nil || role != "user" {
		t.Fatal("fleet commands were elevated to superuser", err)
	}
	if err := run(t.Context(), []string{"login", "--api-key", "wrong-key"}); err == nil {
		t.Fatal("invalid login was accepted")
	}
	if saved, err := installation.ReadAdmin(); err != nil || saved != fleetLogin {
		t.Fatal("failed login changed the fleet login", err)
	}
	if saved, err := installation.ReadUpdateAdmin(); err != nil || saved != operatorLogin {
		t.Fatal("failed login changed the update login", err)
	}
}

func TestLoginDoesNotReuseSavedKeyForAnotherGateway(t *testing.T) {
	updateAuthorizationEnvironment(t)
	requests := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		http.Error(w, "unexpected authentication", http.StatusUnauthorized)
	}))
	defer server.Close()
	if err := run(t.Context(), []string{"login", "--gateway", server.URL}); err == nil || !strings.Contains(err.Error(), "API key is required") {
		t.Fatalf("login unexpectedly reused another gateway's key: %v", err)
	}
	if len(requests) != 0 {
		t.Fatal("saved login was sent to another gateway")
	}
}

func TestFleetLoginPreservesPreviouslySavedOperatorKey(t *testing.T) {
	c, fleetLogin := updateAuthorizationEnvironment(t)
	operatorLogin := installation.AdminProfile{Gateway: c.URL, Key: updateAuthorizationTestKey}
	// Older CLIs saved the operator only in the regular login profile.
	if err := installation.SaveAdmin(operatorLogin); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"login", "--api-key", c.Key}); err != nil {
		t.Fatal(err)
	}
	if saved, err := installation.ReadAdmin(); err != nil || saved != fleetLogin {
		t.Fatal("fleet login was not saved", err)
	}
	if saved, err := installation.ReadUpdateAdmin(); err != nil || saved != operatorLogin {
		t.Fatal("previous operator login was lost", err)
	}
	if err := run(t.Context(), []string{"update", "authorize", "--check"}); err != nil {
		t.Fatal("previous operator login was not reused for updates", err)
	}
}
