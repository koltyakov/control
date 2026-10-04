package main

import (
	"testing"

	"github.com/koltyakov/control/internal/installation"
)

func TestSavedAdminRespectsCredentialAndGatewayOverrides(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	t.Setenv("CONTROL_GATEWAY", "")
	t.Setenv("CONTROL_TOKEN", "")
	t.Setenv("CONTROL_SUPERUSER_KEY", "")
	t.Setenv("CONTROL_USER_KEY", "")
	if err := installation.SaveAdmin(installation.AdminProfile{Gateway: "https://pool.example", Key: "saved-admin"}); err != nil {
		t.Fatal(err)
	}
	if c := adminClient(); c.Key != "saved-admin" || c.URL != "https://pool.example" {
		t.Fatalf("saved credentials not used: %+v", c)
	}
	t.Setenv("CONTROL_TOKEN", "common-key")
	if c := adminClient(); c.Key != "common-key" {
		t.Fatal("common credential silently elevated by saved administrator key")
	}
	t.Setenv("CONTROL_TOKEN", "")
	t.Setenv("CONTROL_GATEWAY", "https://other.example")
	if c := adminClient(); c.Key != "" {
		t.Fatal("saved administrator credential sent to another gateway")
	}
}
