package installation

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

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
