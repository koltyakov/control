package installation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/processutil"
	"golang.org/x/sys/windows"
)

// CheckHybridUser prevents direct or older installers from running the User
// phase with the administrator token used to install its System companion.
func CheckHybridUser() error {
	elevated, err := hybridTokenElevated(windows.GetCurrentProcessToken())
	if err != nil {
		return err
	}
	if elevated {
		return errors.New("hybrid User enrollment requires the installing user's limited token; use a current generated Hybrid installer")
	}
	return CheckServiceMode("user")
}

func hybridUserParent() (windows.Handle, error) {
	current := windows.GetCurrentProcessToken()
	elevated, err := hybridTokenElevated(current)
	if err != nil {
		return 0, err
	}
	if !elevated {
		return 0, errors.New("the Hybrid installer requires Administrator PowerShell under the intended user")
	}
	shell := windows.GetShellWindow()
	if shell == 0 {
		return 0, errors.New("hybrid User startup requires the installing user's non-elevated desktop shell; run Administrator PowerShell from that user's desktop")
	}
	var pid uint32
	if _, err = windows.GetWindowThreadProcessId(shell, &pid); err != nil || pid == 0 {
		return 0, errors.New("cannot identify the Hybrid installer's desktop shell")
	}
	// Go duplicates only the command's standard handles into this parent and
	// supplies an explicit inherited-handle list. The process handle stays open
	// through launch, so a shell exit/PID reuse cannot select another parent.
	parent, err := windows.OpenProcess(windows.PROCESS_CREATE_PROCESS|windows.PROCESS_DUP_HANDLE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, fmt.Errorf("open Hybrid User desktop parent: %w", err)
	}
	var token windows.Token
	if err = windows.OpenProcessToken(parent, windows.TOKEN_QUERY, &token); err == nil {
		err = checkHybridUserParent(current, token)
		_ = token.Close()
	}
	if err != nil {
		_ = windows.CloseHandle(parent)
		return 0, fmt.Errorf("validate Hybrid User desktop parent: %w", err)
	}
	return parent, nil
}

type hybridUserContext struct {
	user      string
	session   uint32
	tokenType uint32
	elevated  bool
}

func hybridTokenContext(token windows.Token) (hybridUserContext, error) {
	user, err := token.GetTokenUser()
	if err != nil {
		return hybridUserContext{}, err
	}
	elevated, err := hybridTokenElevated(token)
	if err != nil {
		return hybridUserContext{}, err
	}
	session, err := hybridTokenValue(token, windows.TokenSessionId)
	if err != nil {
		return hybridUserContext{}, err
	}
	tokenType, err := hybridTokenValue(token, windows.TokenType)
	return hybridUserContext{user: user.User.Sid.String(), session: session, tokenType: tokenType, elevated: elevated}, err
}

func checkHybridUserParent(current, parent windows.Token) error {
	installer, err := hybridTokenContext(current)
	if err != nil {
		return err
	}
	desktop, err := hybridTokenContext(parent)
	if err != nil {
		return err
	}
	return validateHybridUserContext(installer, desktop)
}

func validateHybridUserContext(installer, desktop hybridUserContext) error {
	if !installer.elevated || desktop.elevated || desktop.tokenType != windows.TokenPrimary ||
		installer.user == "" || installer.user != desktop.user || installer.session != desktop.session {
		return errors.New("hybrid User startup requires a non-elevated primary desktop token for the installing user and session; no elevated or other-user fallback is permitted")
	}
	return nil
}

// IsElevated treats a failed query as false. This boundary must fail closed.
func hybridTokenElevated(token windows.Token) (bool, error) {
	value, err := hybridTokenValue(token, windows.TokenElevation)
	return value != 0, err
}

func hybridTokenValue(token windows.Token, class uint32) (uint32, error) {
	var value, size uint32
	err := windows.GetTokenInformation(token, class, (*byte)(unsafe.Pointer(&value)), uint32(unsafe.Sizeof(value)), &size)
	if err != nil {
		return 0, fmt.Errorf("read Hybrid installer token information %d: %w", class, err)
	}
	if size != uint32(unsafe.Sizeof(value)) {
		return 0, errors.New("invalid Hybrid installer token information response")
	}
	return value, nil
}

// RunHybridUser starts only the fixed enrollment helper, using the verified,
// read-only staging executable and a validated same-user desktop parent. It
// never runs a per-user installed executable with its own elevated token.
func RunHybridUser(ctx context.Context, config, link string, preflight, autoName bool) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	if err = CheckHybridSystem(ctx, binary, binary, "auto"); err != nil {
		return err
	}
	parent, err := hybridUserParent()
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(parent) }()
	args := []string{"__hybrid-user", "--limited", "--config", config, "--url", link}
	if preflight {
		args = append(args, "--preflight")
	}
	if autoName {
		args = append(args, "--auto-name")
	}
	cmd, err := hybridUserCommand(ctx, binary, args, parent)
	if err != nil {
		return err
	}
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("limited-token Hybrid User enrollment: %w", err)
	}
	if preflight {
		return nil
	}
	// The limited phase must not request UAC by executing its writable installed
	// CLI. Configure its firewall rules here, from the protected installer.
	cfg, err := node.LoadConfig(config)
	if err != nil {
		return err
	}
	installed, err := BinPath()
	if err != nil {
		return err
	}
	if err = configureElevatedFirewall(ctx, installed, config, cfg); err != nil {
		return fmt.Errorf("user enrollment finished, but its firewall rules could not be configured from the protected installer: %w", err)
	}
	return nil
}

func hybridUserCommand(ctx context.Context, binary string, args []string, parent windows.Handle) (*exec.Cmd, error) {
	if parent == 0 {
		return nil, errors.New("hybrid User startup requires an explicit validated desktop parent")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	processutil.HideWindow(cmd)
	// TokenLinkedToken may return an identification-only impersonation token,
	// which CreateProcessAsUser rejects with ERROR_BAD_IMPERSONATION_LEVEL.
	// The explicit desktop parent supplies its primary token instead. Do not
	// set Token: that would override inheritance and use CreateProcessAsUser.
	cmd.SysProcAttr.ParentProcess = syscall.Handle(parent)
	cmd.Env = cleanEnvironment()
	cmd.Dir = filepath.Dir(binary)
	// Console handles cannot be duplicated into another process. Force exec to
	// use pipes for output instead of handing console handles to the shell.
	// This fixed enrollment helper is non-interactive; nil stdin supplies NUL.
	cmd.Stdout, cmd.Stderr = struct{ io.Writer }{os.Stdout}, struct{ io.Writer }{os.Stderr}
	cmd.WaitDelay = 5 * time.Second
	return cmd, nil
}

// SYSTEM, Administrators, and TrustedInstaller may own or write these paths.
var trustedHybridSIDs = map[string]bool{
	"S-1-5-18":     true,
	"S-1-5-32-544": true,
	"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464": true,
}

// Every right that could replace or redirect a privileged executable.
const (
	fileDeleteChild   = 0x40 // Not defined by x/sys/windows.
	hybridWriteRights = windows.GENERIC_ALL | windows.GENERIC_WRITE | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | fileDeleteChild | windows.FILE_WRITE_ATTRIBUTES
)

func checkHybridSystem(_ context.Context, binary, config, mode string) error {
	if mode != "auto" || !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("hybrid system startup requires elevation and system mode")
	}
	base, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return err
	}
	root := filepath.Join(base, "Control Hybrid")
	checked := map[string]bool{}
	for _, path := range []string{binary, config} {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			return errors.New("hybrid system profiles and executables must be installed under Program Files\\Control Hybrid")
		}
		for {
			if key := strings.ToLower(path); !checked[key] {
				checked[key] = true
				if err = checkHybridPath(path); err != nil {
					return err
				}
			}
			if strings.EqualFold(path, base) {
				break
			}
			path = filepath.Dir(path)
		}
	}
	return nil
}

// Reject writable ACLs rather than repairing an arbitrary user profile.
func checkHybridPath(path string) error {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(pointer)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("hybrid system paths must not contain reparse points")
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read hybrid system path security: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if !trustedHybridSIDs[owner.String()] {
		return fmt.Errorf("hybrid system path has an untrusted owner: %s", path)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	// A NULL DACL grants everyone full access.
	if dacl == nil {
		return fmt.Errorf("hybrid system path has no access control list: %s", path)
	}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			// Conditional and object entries have other layouts; fail closed.
			return fmt.Errorf("hybrid system path has an unsupported access entry: %s", path)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || uint32(ace.Mask)&uint32(hybridWriteRights) == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !trustedHybridSIDs[sid.String()] {
			return fmt.Errorf("hybrid system path is writable by another account: %s", path)
		}
	}
	return nil
}
