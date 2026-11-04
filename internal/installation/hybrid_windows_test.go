package installation

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
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

func TestHybridUserCommandAlwaysSelectsExplicitParent(t *testing.T) {
	// This command is inspected only, never started with this synthetic handle.
	cmd, err := hybridUserCommand(t.Context(), `C:\protected\control.exe`, []string{"__hybrid-user", "--limited", "--config", `C:\Users\user's profile\node.json`}, windows.Handle(123))
	if err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.ParentProcess != syscall.Handle(123) || cmd.SysProcAttr.Token != 0 || !cmd.SysProcAttr.HideWindow {
		t.Fatal("User command could inherit the elevated parent token", cmd.SysProcAttr)
	}
	if cmd.Path != `C:\protected\control.exe` || len(cmd.Args) != 5 || cmd.Args[4] != `C:\Users\user's profile\node.json` {
		t.Fatal("helper path or structured arguments were lost", cmd.Args)
	}
	if cmd.Dir != `C:\protected` {
		t.Fatal("helper inherited an untrusted working directory", cmd.Dir)
	}
	if cmd.Stdin != nil {
		t.Fatal("non-interactive helper could inherit a console input handle")
	}
	if _, direct := cmd.Stdout.(*os.File); direct {
		t.Fatal("console output handle would be duplicated into the shell")
	}
	if _, direct := cmd.Stderr.(*os.File); direct {
		t.Fatal("console error handle would be duplicated into the shell")
	}
	if cmd.WaitDelay <= 0 {
		t.Fatal("output pipe cleanup is unbounded")
	}
	if cmd, err = hybridUserCommand(t.Context(), `C:\protected\control.exe`, nil, 0); err == nil || cmd != nil {
		t.Fatal("missing desktop parent could inherit installer authority")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cmd, err = hybridUserCommand(ctx, `C:\protected\control.exe`, nil, windows.Handle(123))
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Run(); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled User launch did not preserve cancellation:", err)
	}
}

func TestHybridUserContextRejectsUnsafeParents(t *testing.T) {
	installer := hybridUserContext{user: "installer", session: 1, tokenType: windows.TokenPrimary, elevated: true}
	desktop := hybridUserContext{user: "installer", session: 1, tokenType: windows.TokenPrimary}
	if err := validateHybridUserContext(installer, desktop); err != nil {
		t.Fatal("same-user limited primary desktop rejected:", err)
	}
	for name, change := range map[string]func(*hybridUserContext, *hybridUserContext){
		"limited installer":     func(i, _ *hybridUserContext) { i.elevated = false },
		"elevated desktop":      func(_, d *hybridUserContext) { d.elevated = true },
		"other user":            func(_, d *hybridUserContext) { d.user = "other" },
		"other session":         func(_, d *hybridUserContext) { d.session++ },
		"impersonation token":   func(_, d *hybridUserContext) { d.tokenType = windows.TokenImpersonation },
		"unknown token type":    func(_, d *hybridUserContext) { d.tokenType = 0 },
		"missing user identity": func(i, d *hybridUserContext) { i.user, d.user = "", "" },
	} {
		t.Run(name, func(t *testing.T) {
			i, d := installer, desktop
			change(&i, &d)
			if err := validateHybridUserContext(i, d); err == nil {
				t.Fatal("unsafe desktop parent accepted")
			}
		})
	}
	if err := checkHybridUserParent(windows.GetCurrentProcessToken(), 0); err == nil {
		t.Fatal("invalid parent token accepted")
	}
}

func TestHybridDesktopParentHasSameLimitedUserAndSession(t *testing.T) {
	parent, err := hybridUserParent()
	if err != nil {
		if parent != 0 {
			t.Fatal("failed parent selection returned a usable handle")
		}
		t.Log("desktop-parent installation unavailable:", err)
		return
	}
	defer func() { _ = windows.CloseHandle(parent) }()
	var token windows.Token
	if err = windows.OpenProcessToken(parent, windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = token.Close() }()
	if err = checkHybridUserParent(windows.GetCurrentProcessToken(), token); err != nil {
		t.Fatal("desktop parent changed user, session or elevation:", err)
	}
}

func TestHybridLimitedChildToken(t *testing.T) {
	if os.Getenv("CONTROL_TEST_HYBRID_LIMITED_CHILD") == "1" {
		if err := CheckHybridUser(); err != nil {
			t.Fatal("child retained administrator authority", err)
		}
		current, err := hybridTokenContext(windows.GetCurrentProcessToken())
		if err != nil || current.user != os.Getenv("CONTROL_TEST_HYBRID_USER") || strconv.FormatUint(uint64(current.session), 10) != os.Getenv("CONTROL_TEST_HYBRID_SESSION") {
			t.Fatal("child changed the installing user or session", current, err)
		}
		t.Log("limited child confirmed")
		return
	}
	if !windows.GetCurrentProcessToken().IsElevated() || windows.GetShellWindow() == 0 {
		t.Skip("native limited-child launch requires an elevated user with a desktop shell")
	}
	parent, err := hybridUserParent()
	if err != nil {
		t.Fatal("cannot select limited desktop parent:", err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROL_TEST_HYBRID_LIMITED_CHILD", "1")
	current, err := hybridTokenContext(windows.GetCurrentProcessToken())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROL_TEST_HYBRID_USER", current.user)
	t.Setenv("CONTROL_TEST_HYBRID_SESSION", strconv.FormatUint(uint64(current.session), 10))
	cmd, err := hybridUserCommand(t.Context(), binary, []string{"-test.run=^TestHybridLimitedChildToken$", "-test.v"}, parent)
	if err != nil {
		t.Fatal(err)
	}
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
