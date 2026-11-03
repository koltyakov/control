package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/store"
	"golang.org/x/sys/windows"
)

func TestHybridUserHelperRejectsElevatedChildBeforeProfileAccess(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires an elevated test process; does not change services or privileges")
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "node.json")
	if err := os.WriteFile(config, []byte("invalid User profile must not be read"), 0600); err != nil {
		t.Fatal(err)
	}
	handled, err := runPlatformService(t.Context(), []string{"__hybrid-user", "--limited", "--config", config, "--url", "https://example.test/install/ticket"})
	if !handled || err == nil || !strings.Contains(err.Error(), "limited token") {
		t.Fatal("elevated child reached User enrollment", handled, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "node.log")); !os.IsNotExist(err) {
		t.Fatal("elevated helper accessed the User log", err)
	}
}

func TestHybridPendingEnrollmentRejectsElevationBeforeMutation(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires an elevated test process; does not enroll or install services")
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "node.json")
	path := filepath.Join(dir, "pending-enrollment.json")
	link := "https://example.test/install/ticket"
	pending := pendingEnrollment{URL: link, Invitation: enrollment.Invitation{HybridRole: "user"}}
	if err := store.Write(path, pending); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepareEnrollment(t.Context(), config, link, false); err == nil || !strings.Contains(err.Error(), "limited token") {
		t.Fatal("elevated pending enrollment accepted", err)
	}
	if err = preflightEnrollment(t.Context(), config, link, "user", false); err == nil || !strings.Contains(err.Error(), "limited token") {
		t.Fatal("elevated pending preflight accepted", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("rejected enrollment changed recovery state", err)
	}
}

func TestHybridUserHelperRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"--url", "https://example.test/install/ticket", "unexpected"}, {"--firewall"}} {
		if err := hybridUserCLI(t.Context(), args); err == nil {
			t.Fatal("invalid helper invocation accepted", args)
		}
	}
}

func TestHybridUserInvitationPrivilegeCheck(t *testing.T) {
	err := checkHybridUserInvitation(enrollment.Invitation{HybridRole: "user"})
	if (err != nil) != (installation.CheckHybridUser() != nil) {
		t.Fatal("invitation ignored the Hybrid User privilege boundary", err)
	}
	if err = checkHybridUserInvitation(enrollment.Invitation{HybridRole: "system"}); err != nil {
		t.Fatal("System invitation was incorrectly forced into User context", err)
	}
}
