//go:build !windows

package installation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/store"
)

func TestUnixSavedSystemStartup(t *testing.T) {
	config := filepath.Join(t.TempDir(), "node.json")
	if mode, err := ServiceMode(config, ""); err != nil || mode != "auto" {
		t.Fatalf("default changed: %q %v", mode, err)
	}
	if err := store.Write(config+".startup.json", unixStartupProfile{Mode: "system"}); err != nil {
		t.Fatal(err)
	}
	if mode, err := ServiceMode(config, ""); err != nil || mode != "system" {
		t.Fatalf("system mode was not retained: %q %v", mode, err)
	}
	if err := checkServiceProfile(config, "user"); err == nil {
		t.Fatal("switch to user mode could create competing startup")
	}
	if err := checkServiceProfile(config, "system"); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 && CheckServiceMode("system") == nil {
		t.Fatal("system installation allowed without root")
	}
	if err := store.Write(config+".startup.json", unixStartupProfile{Mode: "invalid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ServiceMode(config, ""); err == nil {
		t.Fatal("corrupt saved mode accepted")
	}
}

func TestUnixSystemServicesAreProfileScoped(t *testing.T) {
	if systemServiceID("/control/node.json") == systemServiceID("/other/node.json") || systemServiceID("/Control/node.json") == systemServiceID("/control/node.json") {
		t.Fatal("system startup must distinguish Unix profile paths")
	}
}
