package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
)

func TestHelpAndCommandsRequireSuperuser(t *testing.T) {
	const common = "common-key-for-help-tests"
	const super = "superuser-key-for-help-tests-123456789"
	g, err := gateway.New(t.TempDir(), common, gateway.Options{SuperuserKey: super})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	ctx := context.Background()
	for _, key := range []string{"", common, "wrong"} {
		c := client.Admin{URL: server.URL, Key: key}
		if text := commandHelp(ctx, c); strings.Contains(text, "update push") || strings.Contains(text, "keys create") {
			t.Fatal("common help disclosed admin commands")
		}
		if err := adminCLI(ctx, c, []string{"update", "--help"}); err == nil || strings.Contains(err.Error(), "push") {
			t.Fatal("common user accessed update help")
		}
	}
	if !strings.Contains(commandHelp(ctx, client.Admin{URL: server.URL, Key: super}), "update push") {
		t.Fatal("superuser help missing update commands")
	}
	var account struct{ Token string }
	if err = (client.Admin{URL: server.URL, Key: super}).JSON(ctx, "POST", "/v1/admin/users", map[string]string{"name": "alice"}, &account); err != nil {
		t.Fatal(err)
	}
	user := client.Admin{URL: server.URL, Key: account.Token}
	text := commandHelp(ctx, user)
	if !strings.Contains(text, "machines add") || !strings.Contains(text, "keys create") || strings.Contains(text, "users create") || strings.Contains(text, "update push") {
		t.Fatal("user help has wrong scope", text)
	}
	if err = adminCLI(ctx, user, []string{"users", "list"}); err == nil {
		t.Fatal("user accessed gateway account management")
	}
}

func TestMachineCommandReportsAuthenticationFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer common-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"role":"common"}`))
	}))
	defer server.Close()
	args := []string{"add", "render-01", "--platform", "windows/amd64", "--ttl", "15m"}
	for _, tc := range []struct{ key, want string }{
		{"", "gateway credential is not configured"},
		{"wrong-key", "401 Unauthorized"},
		{"common-key", "requires a user account key"},
	} {
		err := machineCLI(context.Background(), client.Admin{URL: server.URL, Key: tc.key}, args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("key %q: got %v, want %q", tc.key, err, tc.want)
		}
	}
	server.Close()
	err := machineCLI(context.Background(), client.Admin{URL: server.URL, Key: "account-key"}, args)
	if err == nil || !strings.Contains(err.Error(), "authenticate with gateway "+server.URL) || strings.Contains(err.Error(), "unknown machines command") {
		t.Fatalf("lost gateway connection error: %v", err)
	}
}
