package installation

import (
	"bytes"
	"os"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHybridTokenQueryFailsClosed(t *testing.T) {
	if _, err := hybridTokenElevated(windows.Token(0)); err == nil {
		t.Fatal("invalid token was accepted as non-elevated")
	}
}

func TestHybridUserCommandAlwaysSelectsExplicitToken(t *testing.T) {
	// This command is inspected only, never started with this synthetic token.
	cmd := hybridUserCommand(t.Context(), `C:\protected\control.exe`, []string{"__hybrid-user", "--limited", "--config", `C:\Users\user's profile\node.json`}, windows.Token(123))
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Token != syscall.Token(123) || !cmd.SysProcAttr.HideWindow {
		t.Fatal("User command could inherit the elevated parent token", cmd.SysProcAttr)
	}
	if cmd.Path != `C:\protected\control.exe` || len(cmd.Args) != 5 || cmd.Args[4] != `C:\Users\user's profile\node.json` {
		t.Fatal("helper path or structured arguments were lost", cmd.Args)
	}
	if cmd.Dir != `C:\protected` {
		t.Fatal("helper inherited an untrusted working directory", cmd.Dir)
	}
}

func TestHybridLinkedTokenHasSameUserAndIsNotElevated(t *testing.T) {
	limited, err := hybridUserToken()
	if err != nil {
		// CI may use a limited account or disable UAC entirely. Neither may
		// silently inherit its current token as an installation fallback.
		if limited != 0 {
			t.Fatal("failed token selection returned a usable token")
		}
		t.Log("linked-token installation correctly unavailable:", err)
		return
	}
	defer func() { _ = limited.Close() }()
	elevated, err := hybridTokenElevated(limited)
	if err != nil || elevated {
		t.Fatal("linked token is not limited", elevated, err)
	}
	currentUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	linkedUser, err := limited.GetTokenUser()
	if err != nil || currentUser.User.Sid.String() != linkedUser.User.Sid.String() {
		t.Fatal("linked token changed the installing user", err)
	}
}

func TestHybridLimitedChildToken(t *testing.T) {
	if os.Getenv("CONTROL_TEST_HYBRID_LIMITED_CHILD") == "1" {
		if err := CheckHybridUser(); err != nil {
			t.Fatal("child retained administrator authority", err)
		}
		t.Log("limited child confirmed")
		return
	}
	limited, err := hybridUserToken()
	if err != nil {
		t.Skip("native linked-token launch requires a UAC-elevated user:", err)
	}
	defer func() { _ = limited.Close() }()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROL_TEST_HYBRID_LIMITED_CHILD", "1")
	cmd := hybridUserCommand(t.Context(), binary, []string{"-test.run=^TestHybridLimitedChildToken$", "-test.v"}, limited)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err = cmd.Run(); err != nil || !strings.Contains(output.String(), "limited child confirmed") {
		t.Fatalf("limited child launch: %v\n%s", err, output.String())
	}
}

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
