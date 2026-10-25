package installation

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/koltyakov/control/internal/enrollment"
	"golang.org/x/sys/windows"
)

func checkHybridSystem(ctx context.Context, binary, config, mode string) error {
	if mode != "auto" || !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("hybrid system startup requires elevation and system mode")
	}
	base, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return err
	}
	root := filepath.Join(base, "Control Hybrid")
	for _, path := range []string{binary, config} {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			return errors.New("hybrid system profiles and executables must be installed under Program Files\\Control Hybrid")
		}
		for {
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
			// Reject writable ACLs rather than repairing an arbitrary user profile.
			script := "$acl = Get-Acl -LiteralPath " + enrollment.PowerShellQuote(path) + `
$trusted = @('S-1-5-18', 'S-1-5-32-544', 'S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464')
$owner = $acl.GetOwner([Security.Principal.SecurityIdentifier]).Value
if ($owner -notin $trusted) { throw 'Hybrid System path has an untrusted owner' }
foreach ($rule in $acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) {
  if ($rule.AccessControlType -eq 'Allow' -and ($rule.PropagationFlags -band [Security.AccessControl.PropagationFlags]::InheritOnly) -eq 0 -and ([long]$rule.FileSystemRights -band 0x500D0156) -ne 0 -and $rule.IdentityReference.Value -notin $trusted) { throw 'Hybrid System path is writable by another account' }
}`
			if err = userPowerShell(ctx, script); err != nil {
				return err
			}
			if strings.EqualFold(path, base) {
				break
			}
			path = filepath.Dir(path)
		}
	}
	return nil
}
