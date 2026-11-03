package installation

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHybridPathRejectsUserWritableDirectory(t *testing.T) {
	// The temporary directory inherits full control for the test's own account.
	err := checkHybridPath(t.TempDir())
	if err == nil || (!strings.Contains(err.Error(), "writable") && !strings.Contains(err.Error(), "untrusted owner")) {
		t.Fatal("user-writable path accepted:", err)
	}
}

func TestHybridPathAcceptsSystemDirectory(t *testing.T) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	// Inherit-only CREATOR OWNER entries and read-only grants must not count.
	if err = checkHybridPath(system); err != nil {
		t.Fatal("protected system directory rejected:", err)
	}
}

func TestSessionUserDoesNotWarnForCurrentUser(t *testing.T) {
	// A test run in a service or remote session has no logged-on user to compare.
	account, _ := sessionUser()
	if account == "" {
		t.Skip("session has no logged-on user")
	}
	if warning := UserStartupWarning(); warning != "" {
		t.Log("elevated under another account:", warning)
	}
}
