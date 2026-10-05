package installation

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func replaceCLI(staged, path string) error {
	// Preserve explicit grants, including read/execute access for LocalService.
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err = windows.SetNamedSecurityInfo(staged, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return err
	}
	// Windows permits renaming a running executable but not overwriting it.
	backup := filepath.Join(filepath.Dir(staged), "previous.exe")
	if err = os.Rename(path, backup); err != nil {
		return err
	}
	if err = os.Rename(staged, path); err != nil {
		if restoreErr := os.Rename(backup, path); restoreErr != nil {
			return fmt.Errorf("install: %w; restore failed: %v; previous CLI is at %s", err, restoreErr, backup)
		}
		return err
	}
	_ = os.Remove(backup) // It can remain locked by this CLI or the service launcher.
	return nil
}
