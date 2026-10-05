package main

import (
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
)

func TestDashboardGatewayCredentialsFollowSelectedProfile(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	for _, name := range []string{"CONTROL_GATEWAY", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY", "CONTROL_TOKEN"} {
		t.Setenv(name, "")
	}
	if err := installation.SaveAdmin(installation.AdminProfile{Gateway: "https://first.example", Key: "first-key"}); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(t.TempDir(), "node.json")
	if err := store.Write(profile, node.Config{Name: "host", Gateway: "https://second.example", Token: "second-key"}); err != nil {
		t.Fatal(err)
	}
	check := func(want client.Admin, override *string) {
		t.Helper()
		got, err := gatewayClient(profile, override)
		if err != nil || got != want {
			t.Fatalf("got %+v %v, want %+v", got, err, want)
		}
	}
	check(client.Admin{URL: "https://second.example", Key: "second-key"}, nil)
	t.Setenv("CONTROL_GATEWAY", "https://third.example")
	check(client.Admin{URL: "https://third.example"}, nil)
	t.Setenv("CONTROL_GATEWAY", "https://first.example/")
	check(client.Admin{URL: "https://first.example", Key: "first-key"}, nil)
	t.Setenv("CONTROL_USER_KEY", "env-key")
	check(client.Admin{URL: "https://first.example", Key: "env-key"}, nil)
	explicit := "explicit-key"
	check(client.Admin{URL: "https://first.example", Key: explicit}, &explicit)
}
