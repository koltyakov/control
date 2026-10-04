package main

import (
	"context"
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
	defer g.Close()
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
