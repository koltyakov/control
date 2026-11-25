package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/vaultstore"
)

func TestVaultLockedRoutesDoNotConsultGatewayOrLocalAPI(t *testing.T) {
	var calls atomic.Int64
	decoy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer decoy.Close()
	t.Setenv("CONTROL_API", decoy.URL)
	t.Setenv("CONTROL_GATEWAY", decoy.URL)
	t.Setenv("CONTROL_USER_KEY", "test-only-account-credential")
	for _, args := range [][]string{
		{"mcp"}, {"machines"}, {"exec", "worker", "--", "hostname"},
		{"tunnel", "list"}, {"tunnel", "dispose", "known-id"}, {"keys", "list"},
	} {
		f := flag.NewFlagSet("control", flag.ContinueOnError)
		if err := vaultRoute(t.Context(), &vaultstore.Marker{}, f, args); err == nil {
			t.Fatalf("unapproved %v accepted", args)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("locked command made %d requests", n)
	}
}

func TestExplicitVaultMCPNeverUsesLegacyCredentials(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := run(ctx, []string{"mcp", "--vault-process"}); err == nil {
		t.Fatal("session-scoped MCP adapter ran without Vault activation")
	}
}
