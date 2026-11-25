//go:build windows

package vaultstore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"github.com/koltyakov/control/internal/identity"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The Windows boundary mirrors the Unix one with Windows primitives:
//
//   - Identities are SIDs reported by the process token and by file security
//     descriptors, never names, environment variables or caller arguments.
//   - Every protected path is opened one component at a time with NtCreateFile
//     relative to the already verified parent handle, starting at the volume
//     reached through the global (administrator-only) DOS device directory,
//     with FILE_OPEN_REPARSE_POINT. Junctions, symbolic links and other
//     reparse points are refused, so no ancestor can be redirected, and a
//     per-user drive letter mapping cannot substitute the volume.
//   - The vault root is the fixed ProgramData location. Any account may create
//     directories in ProgramData, so the root is trusted only if an
//     administrator principal owns it, its DACL is protected from inheritance,
//     and exactly one dedicated user account (the broker) may modify it.
//   - Objects the broker creates carry an explicit protected DACL. Every
//     private object is checked for owner and DACL before use, because Windows
//     grants "bypass traverse checking" to everyone and a parent directory's
//     DACL alone does not protect its children.

// hooks exist only so tests can stand in for a second OS account. Production
// code never changes them.
var hooks = struct {
	// sid is the principal identity compared with marker and profile
	// principals. File ownership checks always use kernel-reported owners.
	sid             func() (string, error)
	lookup          func(name string) (Principal, error)
	lookupSID       func(sid string) (Principal, error)
	checkProcess    func() error
	checkExecutable func(account string) error
	defaultLayout   func() (Layout, error)
}{
	sid:             processSID,
	lookup:          lookupPrincipal,
	lookupSID:       principalFor,
	checkProcess:    checkBrokerProcess,
	checkExecutable: checkExecutable,
	defaultLayout:   canonicalLayout,
}

// Well-known SIDs. Administrator principals are equivalent to root: they can
// take ownership of any object, so trusting them costs nothing.
const (
	sidSystem           = "S-1-5-18"
	sidAdministrators   = "S-1-5-32-544"
	sidTrustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	// OWNER RIGHTS applies to the object's owner, which is verified first.
	sidOwnerRights = "S-1-3-4"
)

func adminSID(sid string) bool {
	return sid == sidSystem || sid == sidAdministrators || sid == sidTrustedInstaller
}

// brokerSID accepts a local or domain user account: S-1-5-21-a-b-c-RID with
// RID >= 1000. Built-in accounts, service and virtual accounts, and cloud
// identities are refused for the broker.
func brokerSID(sid string) bool {
	return userSID(sid) && strings.HasPrefix(sid, "S-1-5-21-")
}

// Access rights, spelled out so the policy does not depend on composite
// constants.
const (
	accReadData        = 0x00000001 // FILE_READ_DATA, FILE_LIST_DIRECTORY
	accWriteData       = 0x00000002 // FILE_WRITE_DATA, FILE_ADD_FILE
	accAppendData      = 0x00000004 // FILE_APPEND_DATA, FILE_ADD_SUBDIRECTORY
	accReadEA          = 0x00000008
	accWriteEA         = 0x00000010
	accExecute         = 0x00000020 // FILE_EXECUTE, FILE_TRAVERSE
	accReadAttributes  = 0x00000080
	accWriteAttributes = 0x00000100
	accDelete          = 0x00010000
	accReadControl     = 0x00020000
	accSynchronize     = 0x00100000
	accGenericExecute  = 0x20000000
	accGenericRead     = 0x80000000

	// readRights may be granted to anyone on any checked object.
	readRights = accReadData | accReadEA | accExecute | accReadAttributes | accReadControl | accSynchronize | accGenericRead | accGenericExecute
	// stickyRights may additionally be granted to anyone on an
	// administrator-owned ancestor such as C:\ or C:\ProgramData. They let an
	// account add its own entries but not delete, rename, relabel or
	// re-permission existing ones (the Windows analogue of a sticky /tmp).
	stickyRights = accWriteData | accAppendData | accWriteEA | accWriteAttributes

	dirRead   = accReadData | accExecute | accReadAttributes | accReadControl | accSynchronize
	dirWrite  = dirRead | accWriteData | accAppendData
	fileRead  = accReadData | accReadAttributes | accReadControl | accSynchronize
	fileWrite = accWriteData | accReadAttributes | accReadControl | accDelete | accSynchronize
	shareAll  = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

	inheritOnlyACE = 0x08
	integrityHigh  = 0x3000
)

var processToken = sync.OnceValues(func() (string, error) {
	// The pseudo-handle needs no close and always names this process.
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
})

// processSID is the token user of this process.
func processSID() (string, error) { return processToken() }

func lookupPrincipal(name string) (Principal, error) {
	sid, _, kind, err := windows.LookupSID("", name)
	if err != nil {
		return Principal{}, fmt.Errorf("look up account %q: %w", name, err)
	}
	if kind != windows.SidTypeUser {
		return Principal{}, fmt.Errorf("account %q is not a user account", name)
	}
	return principalFor(sid.String())
}

func principalFor(id string) (Principal, error) {
	sid, err := windows.StringToSid(id)
	if err != nil {
		return Principal{}, fmt.Errorf("invalid SID %s: %w", id, err)
	}
	account, domain, kind, err := sid.LookupAccount("")
	if err != nil {
		return Principal{}, fmt.Errorf("look up SID %s: %w", id, err)
	}
	if kind != windows.SidTypeUser {
		return Principal{}, fmt.Errorf("SID %s is not a user account", id)
	}
	return Principal{UID: sid.String(), Name: domain + `\` + account}, nil
}

// canonicalLayout is the fixed vault root, <system drive>\ProgramData\Control
// Vault. The system drive comes from the kernel's system root, not from
// %SystemDrive% or %ProgramData%, and ProgramData must be at its default,
// administrator-configured location; a relocated ProgramData is unsupported.
func canonicalLayout() (Layout, error) {
	win, err := windows.GetSystemWindowsDirectory()
	if err != nil {
		return Layout{}, fmt.Errorf("%w: cannot locate the system drive: %v", ErrUnsupported, err)
	}
	drive := filepath.VolumeName(win)
	if len(drive) != 2 || drive[1] != ':' || !asciiLetter(drive[0]) {
		return Layout{}, fmt.Errorf("%w: system root %s is not on a drive letter", ErrUnsupported, win)
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return Layout{}, fmt.Errorf("%w: cannot read the ProgramData location: %v", ErrUnsupported, err)
	}
	defer func() { _ = k.Close() }()
	value, _, err := k.GetStringValue("ProgramData")
	if err != nil {
		return Layout{}, fmt.Errorf("%w: cannot read the ProgramData location: %v", ErrUnsupported, err)
	}
	programData := strings.ToUpper(drive) + `\ProgramData`
	if !strings.EqualFold(value, `%SystemDrive%\ProgramData`) && !strings.EqualFold(value, programData) {
		return Layout{}, fmt.Errorf("%w: ProgramData is relocated to %s", ErrUnsupported, value)
	}
	return Layout{Root: programData + `\Control Vault`}, nil
}

func asciiLetter(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }

// requireCanonical refuses any vault root other than the fixed one. A
// manually chosen root cannot prove that an administrator created it.
func requireCanonical(root string) (Layout, error) {
	canonical, err := hooks.defaultLayout()
	if err != nil {
		return Layout{}, err
	}
	if !strings.EqualFold(filepath.Clean(root), canonical.Root) {
		return Layout{}, fmt.Errorf("on Windows the vault root is fixed at %s; %s is not trusted", canonical.Root, root)
	}
	return canonical, nil
}

// dir is an open directory handle reached without following reparse points.
// Every name inside it is opened relative to the handle, never by path.
type dir struct {
	h    windows.Handle
	path string
	// account is the non-administrator account trusted for this chain, ""
	// if none (or not yet adopted).
	account string
}

func (d *dir) Close() error {
	if d == nil || d.h == 0 || d.h == windows.InvalidHandle {
		return nil
	}
	err := windows.CloseHandle(d.h)
	d.h = windows.InvalidHandle
	return err
}

// validName accepts a single literal NTFS name. Separators, stream (":") and
// wildcard characters, and names that Win32 would silently rewrite are refused.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	for _, r := range name {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return false
		}
	}
	return true
}

// splitAbs accepts only a clean absolute local drive path, optionally with
// the \\?\ prefix, and returns its drive and literal components.
func splitAbs(path string) (string, []string, error) {
	p := strings.TrimPrefix(path, `\\?\`)
	if len(p) < 3 || p[1] != ':' || p[2] != '\\' || !asciiLetter(p[0]) || filepath.Clean(p) != p {
		return "", nil, fmt.Errorf("vault path %q must be an absolute, clean local drive path", path)
	}
	var parts []string
	if rest := p[3:]; rest != "" {
		parts = strings.Split(rest, `\`)
		for _, name := range parts {
			if !validName(name) {
				return "", nil, fmt.Errorf("vault path %q has an unsupported component %q", path, name)
			}
		}
	}
	return strings.ToUpper(p[:2]), parts, nil
}

func ntError(err error) error {
	var st windows.NTStatus
	if !errors.As(err, &st) {
		return err
	}
	if st == windows.STATUS_OBJECT_NAME_COLLISION {
		return os.ErrExist
	}
	return st.Errno()
}

// ntOpen opens name relative to root (or an absolute NT path when root is 0)
// synchronously, never following a reparse point in the final component.
func ntOpen(root windows.Handle, name string, access, share, disposition, options uint32, sd *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	u, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{RootDirectory: root, ObjectName: u, Attributes: windows.OBJ_CASE_INSENSITIVE, SecurityDescriptor: sd}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	attrs := uint32(0)
	if options&windows.FILE_DIRECTORY_FILE == 0 {
		attrs = windows.FILE_ATTRIBUTE_NORMAL
	}
	var h windows.Handle
	err = windows.NtCreateFile(&h, access|accSynchronize, oa, &windows.IO_STATUS_BLOCK{}, nil, attrs, share, disposition,
		options|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	runtime.KeepAlive(sd)
	if err != nil {
		return windows.InvalidHandle, ntError(err)
	}
	return h, nil
}

// openVolume opens a drive root through \GLOBAL??, which only administrators
// can change, so a per-logon-session drive mapping cannot substitute it.
func openVolume(drive string) (windows.Handle, error) {
	h, err := ntOpen(0, `\GLOBAL??\`+drive+`\`, dirRead, shareAll, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("open volume %s: %w", drive, err)
	}
	return h, nil
}

func fileInfo(h windows.Handle) (windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	err := windows.GetFileInformationByHandle(h, &info)
	return info, err
}

// kind verifies the object type. Reparse points are never accepted.
func kind(h windows.Handle, path string, directory bool) (windows.ByHandleFileInformation, error) {
	info, err := fileInfo(h)
	if err != nil {
		return info, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return info, fmt.Errorf("%s is a reparse point (link, junction or placeholder)", path)
	}
	isDir := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if directory && !isDir {
		return info, fmt.Errorf("%s is not a directory", path)
	}
	if !directory && (isDir || info.FileAttributes&windows.FILE_ATTRIBUTE_DEVICE != 0) {
		return info, fmt.Errorf("%s is not a regular file", path)
	}
	return info, nil
}

type ace struct {
	sid         string
	mask        uint32
	inheritOnly bool
}

// security is an object's owner and the allow entries of its DACL.
type security struct {
	owner     string
	protected bool
	aces      []ace
}

func readSecurity(h windows.Handle, path string) (*security, error) {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("cannot read the security descriptor of %s: %w", path, err)
	}
	return parseSecurity(sd, path)
}

// parseSecurity refuses a missing or NULL DACL (full access for everyone) and
// any ACE type it cannot evaluate. Deny entries only remove access and are
// ignored.
func parseSecurity(sd *windows.SECURITY_DESCRIPTOR, path string) (*security, error) {
	defer runtime.KeepAlive(sd)
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.IsValid() {
		return nil, fmt.Errorf("%s has no owner", path)
	}
	control, _, err := sd.Control()
	if err != nil {
		return nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return nil, fmt.Errorf("%s has no DACL, which grants everyone full access", path)
	}
	s := &security{owner: owner.String(), protected: control&windows.SE_DACL_PROTECTED != 0}
	for i := range uint32(dacl.AceCount) {
		var a *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(dacl, i, &a); err != nil {
			return nil, fmt.Errorf("cannot read the DACL of %s: %w", path, err)
		}
		switch a.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			sid := (*windows.SID)(unsafe.Pointer(&a.SidStart))
			s.aces = append(s.aces, ace{sid: sid.String(), mask: uint32(a.Mask), inheritOnly: a.Header.AceFlags&inheritOnlyACE != 0})
		case windows.ACCESS_DENIED_ACE_TYPE, 0x06, 0x0A, 0x0C: // denied, denied object, denied callback, denied callback object
		default:
			return nil, fmt.Errorf("%s has an unsupported access control entry type 0x%x", path, a.Header.AceType)
		}
	}
	return s, nil
}

// grants reports an allow entry giving an untrusted SID more than allowed.
// Inherit-only entries do not apply to the object itself.
func (s *security) grants(path string, trusted func(string) bool, allowed uint32, inherited bool) error {
	for _, a := range s.aces {
		if a.inheritOnly && !inherited || trusted(a.sid) || a.sid == sidOwnerRights {
			continue
		}
		if extra := a.mask &^ allowed; extra != 0 {
			return fmt.Errorf("%s grants %s access 0x%x; only administrators and the vault account may modify it", path, a.sid, extra)
		}
	}
	return nil
}

func trustedFor(account string) func(string) bool {
	return func(sid string) bool { return adminSID(sid) || account != "" && sid == account }
}

// checkComponent verifies one directory on a trusted walk. It must be owned
// by an administrator principal or *account; an empty *account adopts the
// first non-administrator owner, which every later component must share.
// Other accounts may only read, except that an administrator-owned ancestor
// may also let them add entries.
func checkComponent(h windows.Handle, path string, account *string, ancestor bool) error {
	if _, err := kind(h, path, true); err != nil {
		return err
	}
	s, err := readSecurity(h, path)
	if err != nil {
		return err
	}
	if !adminSID(s.owner) {
		if *account == "" {
			*account = s.owner
		}
		if s.owner != *account {
			return fmt.Errorf("%s is owned by %s, not an administrator or the vault account", path, s.owner)
		}
	}
	allowed := uint32(readRights)
	if ancestor && adminSID(s.owner) {
		allowed |= stickyRights
	}
	return s.grants(path, trustedFor(*account), allowed, false)
}

// openTrusted walks path from its volume root without following reparse
// points, checking every component with checkComponent. strictLast refuses
// the sticky allowance for the final component. lastAccess is requested for
// the final handle.
func openTrusted(path, account string, strictLast bool, lastAccess uint32) (*dir, error) {
	drive, parts, err := splitAbs(path)
	if err != nil {
		return nil, err
	}
	h, err := openVolume(drive)
	if err != nil {
		return nil, err
	}
	d := &dir{h: h, path: drive + `\`, account: account}
	if err = checkComponent(d.h, d.path, &d.account, len(parts) > 0 || !strictLast); err != nil {
		_ = d.Close()
		return nil, err
	}
	for i, name := range parts {
		last := i == len(parts)-1
		access := uint32(dirRead)
		if last {
			access = lastAccess
		}
		next, err := ntOpen(d.h, name, access, shareAll, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
		current := filepath.Join(d.path, name)
		chain := d.account
		_ = d.Close()
		if err != nil {
			return nil, fmt.Errorf("open %s without following links: %w", current, err)
		}
		d = &dir{h: next, path: current, account: chain}
		if err = checkComponent(next, current, &d.account, !last || !strictLast); err != nil {
			_ = d.Close()
			return nil, err
		}
	}
	return d, nil
}

var (
	// errNotAdministered marks a vault root that any account could have
	// created.
	errNotAdministered = errors.New("vault root is not administrator-owned")
	// errRootUnreadable marks a vault root this account may not open. A
	// provisioned root always lets its frontend read it.
	errRootUnreadable = errors.New("vault root is not readable by this account")
)

func closeHandle(h windows.Handle) error { return windows.CloseHandle(h) }

// checkRoot verifies the fixed vault root and returns its broker: the root
// must be owned by an administrator principal, must not inherit its DACL, and
// exactly one dedicated user account may hold more than read access. When the
// walk already determined an account, the broker must be that account.
func checkRoot(d *dir) (string, error) {
	if _, err := kind(d.h, d.path, true); err != nil {
		return "", err
	}
	s, err := readSecurity(d.h, d.path)
	if err != nil {
		return "", err
	}
	return rootBroker(s, d.path, d.account)
}

// rootBroker applies the vault root policy to a parsed descriptor.
func rootBroker(s *security, path, account string) (string, error) {
	if !adminSID(s.owner) {
		return "", fmt.Errorf("%w: %s is owned by %s; any account can create directories in ProgramData, so an administrator must own it", errNotAdministered, path, s.owner)
	}
	if !s.protected {
		return "", fmt.Errorf("%s inherits permissions from its parent; disable inheritance", path)
	}
	var writer string
	for _, a := range s.aces {
		if a.inheritOnly || adminSID(a.sid) || a.sid == sidOwnerRights || a.mask&^readRights == 0 {
			continue
		}
		if writer != "" && writer != a.sid {
			return "", fmt.Errorf("%s lets both %s and %s modify it; only the vault account may", path, writer, a.sid)
		}
		writer = a.sid
	}
	if writer == "" {
		return "", fmt.Errorf("%s grants no vault account write access", path)
	}
	if !brokerSID(writer) {
		return "", fmt.Errorf("%s may be modified by %s, which is not a dedicated local or domain user account", path, writer)
	}
	if account != "" && account != writer {
		return "", fmt.Errorf("%s belongs to vault account %s, but its location is controlled by %s", path, writer, account)
	}
	return writer, nil
}

// openRootDir opens the fixed vault root below its trusted parent chain.
func openRootDir(root, account string, access uint32) (*dir, string, error) {
	parent, err := openTrusted(filepath.Dir(root), account, false, dirRead)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = parent.Close() }()
	h, err := ntOpen(parent.h, filepath.Base(root), access, shareAll, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
	if errors.Is(err, os.ErrPermission) {
		return nil, "", fmt.Errorf("%w: %s: %v", errRootUnreadable, root, err)
	}
	if err != nil {
		return nil, "", fmt.Errorf("open %s without following links: %w", root, err)
	}
	d := &dir{h: h, path: root, account: parent.account}
	broker, err := checkRoot(d)
	if err != nil {
		_ = d.Close()
		return nil, "", err
	}
	d.account = broker
	return d, broker, nil
}

// privateSD is an explicit, protected descriptor: the broker owns the object
// and is the only account granted access. Directories pass it on to entries
// that other code creates inside them.
func privateSD(account string, directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	flags := ""
	if directory {
		flags = "OICI"
	}
	return windows.SecurityDescriptorFromString("O:" + account + "D:P(A;" + flags + ";FA;;;" + account + ")")
}

// publicSD lets authenticated users read the nonsecret marker.
func publicSD(account string) (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.SecurityDescriptorFromString("O:" + account + "D:P(A;;FA;;;" + account + ")(A;;FR;;;AU)")
}

// homeSD protects a frontend file like mode 0600; the default owner applies.
func homeSD() (*windows.SECURITY_DESCRIPTOR, error) {
	self, err := processSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + self + ")")
}

// sub opens or creates a private child directory of the broker.
func (d *dir) sub(name, account string, create bool) (*dir, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid vault entry %q", name)
	}
	path := filepath.Join(d.path, name)
	disposition := uint32(windows.FILE_OPEN)
	var sd *windows.SECURITY_DESCRIPTOR
	if create {
		var err error
		if sd, err = privateSD(account, true); err != nil {
			return nil, err
		}
		disposition = windows.FILE_OPEN_IF
	}
	h, err := ntOpen(d.h, name, dirWrite, shareAll, disposition, windows.FILE_DIRECTORY_FILE, sd)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", path, os.ErrNotExist)
		}
		return nil, fmt.Errorf("open %s without following links: %w", path, err)
	}
	child := &dir{h: h, path: path, account: account}
	if err = checkPrivate(h, path, account, true); err != nil {
		_ = child.Close()
		return nil, err
	}
	return child, nil
}

// checkPrivate requires a broker- or administrator-owned object whose DACL,
// including entries inherited by future children, grants nobody else any
// access. Files must have a single link.
func checkPrivate(h windows.Handle, path, account string, directory bool) error {
	info, err := kind(h, path, directory)
	if err != nil {
		return err
	}
	if !directory && info.NumberOfLinks != 1 {
		return fmt.Errorf("%s is not a single-link regular file", path)
	}
	s, err := readSecurity(h, path)
	if err != nil {
		return err
	}
	if s.owner != account && !adminSID(s.owner) {
		return fmt.Errorf("%s is owned by %s, not the vault account %s", path, s.owner, account)
	}
	if err = s.grants(path, trustedFor(account), 0, true); err != nil {
		return fmt.Errorf("%s is accessible to other accounts: %w", path, err)
	}
	return nil
}

func requireProtected(d *dir) error {
	s, err := readSecurity(d.h, d.path)
	if err != nil {
		return err
	}
	if !s.protected {
		return fmt.Errorf("%s inherits permissions from its parent", d.path)
	}
	return nil
}

// checkPublic requires a single-link regular file owned by account that no
// other account may modify.
func checkPublic(h windows.Handle, path, account string) error {
	info, err := kind(h, path, false)
	if err != nil {
		return err
	}
	if info.NumberOfLinks != 1 {
		return fmt.Errorf("%s is not a single-link regular file", path)
	}
	s, err := readSecurity(h, path)
	if err != nil {
		return err
	}
	if s.owner != account {
		return fmt.Errorf("%s is owned by %s, not %s", path, s.owner, account)
	}
	return s.grants(path, trustedFor(account), readRights, false)
}

// readFile reads a regular single-link file. private requires a private
// object of account; otherwise account must own it and others must not write.
func (d *dir) readFile(name, account string, private bool) ([]byte, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid vault entry %q", name)
	}
	path := filepath.Join(d.path, name)
	h, err := ntOpen(d.h, name, fileRead, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", path, os.ErrNotExist)
		}
		return nil, fmt.Errorf("open %s without following links: %w", path, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if private {
		err = checkPrivate(h, path, account, false)
	} else {
		err = checkPublic(h, path, account)
	}
	if err != nil {
		return nil, err
	}
	return readHandle(h, path)
}

func readHandle(h windows.Handle, path string) ([]byte, error) {
	var out []byte
	buf := make([]byte, 32<<10)
	for {
		var n uint32
		if err := windows.ReadFile(h, buf, &n, nil); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if n == 0 {
			return out, nil
		}
		out = append(out, buf[:n]...)
		if len(out) > maxFileSize {
			return nil, fmt.Errorf("%s is too large", path)
		}
	}
}

type fileRenameInformation struct {
	Flags          uint32 // ReplaceIfExists for FileRenameInformation
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [windows.MAX_PATH]uint16
}

// renameHandle renames the open file to name in directory root. Without
// replace an existing target fails with os.ErrExist.
func renameHandle(h, root windows.Handle, name string, replace bool) error {
	const fileRenameInformationEx = 65
	u := utf16.Encode([]rune(name))
	if len(u) > windows.MAX_PATH {
		return fmt.Errorf("vault entry %q is too long", name)
	}
	info := &fileRenameInformation{RootDirectory: root, FileNameLength: uint32(2 * len(u))}
	copy(info.FileName[:], u)
	if replace {
		info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	}
	err := windows.NtSetInformationFile(h, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(info)), uint32(unsafe.Sizeof(*info)), fileRenameInformationEx)
	if errors.Is(err, windows.STATUS_INVALID_INFO_CLASS) || errors.Is(err, windows.STATUS_INVALID_PARAMETER) || errors.Is(err, windows.STATUS_NOT_SUPPORTED) {
		info.Flags = 0
		if replace {
			info.Flags = 1
		}
		err = windows.NtSetInformationFile(h, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(info)), uint32(unsafe.Sizeof(*info)), windows.FileRenameInformation)
	}
	if err != nil {
		return ntError(err)
	}
	return nil
}

// deleteHandle unlinks the open object, immediately where the file system
// supports POSIX semantics and otherwise when the last handle closes.
func deleteHandle(h windows.Handle) error {
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE)
	err := windows.NtSetInformationFile(h, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&flags)), 4, windows.FileDispositionInformationEx)
	if errors.Is(err, windows.STATUS_INVALID_INFO_CLASS) || errors.Is(err, windows.STATUS_INVALID_PARAMETER) || errors.Is(err, windows.STATUS_NOT_SUPPORTED) {
		del := uint32(1)
		err = windows.NtSetInformationFile(h, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&del)), 4, windows.FileDispositionInformation)
	}
	if err != nil {
		return ntError(err)
	}
	return nil
}

// writeFile atomically replaces name, or with exclusive publishes it only if
// it does not exist. The new file carries sd, is flushed before it is renamed
// into place, and is flushed again so the rename reaches the NTFS log.
func (d *dir) writeFile(name string, data []byte, sd *windows.SECURITY_DESCRIPTOR, exclusive bool) error {
	if !validName(name) {
		return fmt.Errorf("invalid vault entry %q", name)
	}
	tmp := ".write-" + identity.NewID()
	h, err := ntOpen(d.h, tmp, fileWrite, 0, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE, sd)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Join(d.path, tmp), err)
	}
	published := false
	defer func() {
		if !published {
			_ = deleteHandle(h)
		}
		_ = windows.CloseHandle(h)
	}()
	for rest := data; len(rest) > 0; {
		var n uint32
		if err = windows.WriteFile(h, rest, &n, nil); err != nil {
			return err
		}
		rest = rest[n:]
	}
	if err = windows.FlushFileBuffers(h); err != nil {
		return err
	}
	if err = renameHandle(h, d.h, name, !exclusive); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s: %w", filepath.Join(d.path, name), os.ErrExist)
		}
		return err
	}
	published = true
	return windows.FlushFileBuffers(h)
}

func (d *dir) openEntry(name string, access uint32) (windows.Handle, error) {
	if !validName(name) {
		return windows.InvalidHandle, fmt.Errorf("invalid vault entry %q", name)
	}
	h, err := ntOpen(d.h, name, access, shareAll, windows.FILE_OPEN, 0, nil)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("open %s: %w", filepath.Join(d.path, name), err)
	}
	return h, nil
}

func (d *dir) rename(from, to string) error {
	if !validName(to) {
		return errors.New("invalid vault entry")
	}
	h, err := d.openEntry(from, accDelete|accReadAttributes)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	// NTFS journals the rename; a directory handle cannot be flushed.
	return renameHandle(h, d.h, to, true)
}

func (d *dir) remove(name string) error {
	h, err := d.openEntry(name, accDelete|accReadAttributes)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return deleteHandle(h)
}

// removeAll deletes a private tree below d without following links.
func (d *dir) removeAll(name, account string) error {
	child, err := d.sub(name, account, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := child.list()
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				err = child.removeAll(e.Name(), account)
			} else {
				err = child.remove(e.Name())
			}
			if err != nil {
				break
			}
		}
	}
	_ = child.Close()
	if err != nil {
		return err
	}
	return d.remove(name)
}

// entry is a directory listing entry. Reparse points are neither
// directories nor regular files.
type entry struct {
	name  string
	attrs uint32
}

func (e entry) Name() string { return e.name }

func (e entry) IsDir() bool {
	return e.attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 && e.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
}

func (e entry) regular() bool {
	return e.attrs&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DEVICE) == 0
}

// list enumerates d through its handle (FILE_FULL_DIR_INFO records).
func (d *dir) list() ([]entry, error) {
	const header = 68 // offset of FileName in FILE_FULL_DIR_INFO
	buf := make([]byte, 64<<10)
	class := uint32(windows.FileFullDirectoryRestartInfo)
	var out []entry
	for {
		err := windows.GetFileInformationByHandleEx(d.h, class, &buf[0], uint32(len(buf)))
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", d.path, err)
		}
		class = windows.FileFullDirectoryInfo
		for off := 0; ; {
			if off+header > len(buf) {
				return nil, fmt.Errorf("list %s: malformed directory record", d.path)
			}
			next := int(binary.LittleEndian.Uint32(buf[off:]))
			attrs := binary.LittleEndian.Uint32(buf[off+56:])
			size := int(binary.LittleEndian.Uint32(buf[off+60:]))
			if size%2 != 0 || off+header+size > len(buf) {
				return nil, fmt.Errorf("list %s: malformed directory record", d.path)
			}
			u := make([]uint16, size/2)
			for i := range u {
				u[i] = binary.LittleEndian.Uint16(buf[off+header+2*i:])
			}
			if name := string(utf16.Decode(u)); name != "." && name != ".." {
				out = append(out, entry{name: name, attrs: attrs})
			}
			if next == 0 {
				break
			}
			off += next
		}
	}
	slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

type handleCloser windows.Handle

func (h handleCloser) Close() error { return windows.CloseHandle(windows.Handle(h)) }

// lock takes an exclusive LockFileEx lock on byte 0 of a private lock file,
// the same range gofrs/flock locks on Windows.
func (d *dir) lock(name, account string) (io.Closer, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid vault entry %q", name)
	}
	sd, err := privateSD(account, false)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(d.path, name)
	h, err := ntOpen(d.h, name, fileRead|accWriteData, shareAll, windows.FILE_OPEN_IF, windows.FILE_NON_DIRECTORY_FILE, sd)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err = checkPrivate(h, path, account, false); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	if err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		_ = windows.CloseHandle(h)
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, fmt.Errorf("%w: %s is held by a running process", ErrBusy, path)
		}
		return nil, err
	}
	return handleCloser(h), nil
}

// probeLock reports ErrBusy when another process holds an existing lock file.
// A missing lock file is not busy. The probe never creates the file.
func (d *dir) probeLock(name string) error {
	if !validName(name) {
		return fmt.Errorf("invalid vault entry %q", name)
	}
	path := filepath.Join(d.path, name)
	h, err := ntOpen(d.h, name, fileRead, shareAll, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, nil)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if _, err = kind(h, path, false); err != nil {
		return err
	}
	if err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return fmt.Errorf("%w: %s is held by a running process; stop it first", ErrBusy, path)
		}
		return err
	}
	return windows.UnlockFileEx(h, 0, 1, 0, &windows.Overlapped{})
}

// homeOwner accepts objects owned by this account or by an administrator
// principal (an elevated process of this account creates objects owned by
// Administrators). The home is not a security boundary: it only names the
// trusted marker, which is always re-verified.
func homeOwner(path, owner string) error {
	self, err := processSID()
	if err != nil {
		return err
	}
	if owner != self && !adminSID(owner) {
		return fmt.Errorf("%s is not owned by this account", path)
	}
	return nil
}

// openHome opens the frontend's own Control home. The home itself may be any
// directory this account owns; entries below it are never followed as links.
func openHome(home string) (*dir, error) {
	abs, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, dirRead, shareAll, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", abs, os.ErrNotExist)
		}
		return nil, err
	}
	self, err := processSID()
	d := &dir{h: h, path: abs, account: self}
	if err == nil {
		err = checkHomeDir(h, abs)
	}
	if err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}

func checkHomeDir(h windows.Handle, path string) error {
	info, err := fileInfo(h)
	if err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("%s is not a directory", path)
	}
	s, err := readSecurity(h, path)
	if err != nil {
		return err
	}
	return homeOwner(path, s.owner)
}

// homeSub opens an existing directory below the home without following links.
func (d *dir) homeSub(name string) (*dir, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid home entry %q", name)
	}
	path := filepath.Join(d.path, name)
	h, err := ntOpen(d.h, name, dirRead, shareAll, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", path, os.ErrNotExist)
		}
		return nil, fmt.Errorf("open %s without following links: %w", path, err)
	}
	child := &dir{h: h, path: path, account: d.account}
	if _, err = kind(h, path, true); err == nil {
		err = checkHomeDir(h, path)
	}
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	return child, nil
}

// readHomeFile reads a home file owned by this account (or an administrator)
// that no other account may modify, without following links.
func (d *dir) readHomeFile(name string) ([]byte, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid home entry %q", name)
	}
	path := filepath.Join(d.path, name)
	h, err := ntOpen(d.h, name, fileRead, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", path, os.ErrNotExist)
		}
		return nil, fmt.Errorf("open %s without following links: %w", path, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	info, err := kind(h, path, false)
	if err != nil {
		return nil, err
	}
	if info.NumberOfLinks != 1 {
		return nil, fmt.Errorf("%s is not a single-link regular file", path)
	}
	s, err := readSecurity(h, path)
	if err != nil {
		return nil, err
	}
	if err = homeOwner(path, s.owner); err != nil {
		return nil, err
	}
	if err = s.grants(path, trustedFor(d.account), readRights, false); err != nil {
		return nil, err
	}
	return readHandle(h, path)
}

// Token inspection.

const tokenIsAppContainer = 29

func tokenInfo(t windows.Token, class uint32) ([]byte, error) {
	n := uint32(256)
	for {
		buf := make([]byte, n)
		err := windows.GetTokenInformation(t, class, &buf[0], n, &n)
		if err == nil {
			return buf[:n], nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || n <= uint32(len(buf)) {
			return nil, err
		}
	}
}

func tokenFlag(t windows.Token, class uint32) (bool, error) {
	b, err := tokenInfo(t, class)
	if err != nil {
		return false, err
	}
	if len(b) < 4 {
		return false, errors.New("short token information")
	}
	return binary.LittleEndian.Uint32(b) != 0, nil
}

// privilegedGroupSIDs can administer the host, read other accounts' files, or
// control a root-equivalent service. Domain groups are matched by RID below.
var privilegedGroupSIDs = map[string]string{
	sidSystem:           "SYSTEM",
	sidAdministrators:   "Administrators",
	"S-1-5-32-548":      "Account Operators",
	"S-1-5-32-549":      "Server Operators",
	"S-1-5-32-550":      "Print Operators",
	"S-1-5-32-551":      "Backup Operators",
	"S-1-5-32-578":      "Hyper-V Administrators",
	"S-1-5-32-582":      "Storage Replica Administrators",
	"S-1-5-9":           "Enterprise Domain Controllers",
	sidTrustedInstaller: "TrustedInstaller",
}

// privilegedDomainRIDs are domain groups with administrative rights.
var privilegedDomainRIDs = map[string]string{
	"498": "Enterprise Read-only Domain Controllers", "512": "Domain Admins", "516": "Domain Controllers",
	"518": "Schema Admins", "519": "Enterprise Admins", "520": "Group Policy Creator Owners",
	"521": "Read-only Domain Controllers", "526": "Key Admins", "527": "Enterprise Key Admins",
}

// privilegedGroups names token groups that make an account administrative.
// Deny-only membership counts: a UAC-filtered administrator can elevate.
func privilegedGroups(groups []string, local map[string]bool) []string {
	var found []string
	for _, sid := range groups {
		if name, ok := privilegedGroupSIDs[sid]; ok {
			found = append(found, name)
			continue
		}
		if local[sid] {
			found = append(found, "docker-users")
			continue
		}
		if parts := strings.Split(sid, "-"); len(parts) == 8 && strings.HasPrefix(sid, "S-1-5-21-") {
			if name, ok := privilegedDomainRIDs[parts[7]]; ok {
				found = append(found, name)
			}
		}
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// localPrivilegedGroups resolves root-equivalent local groups without
// well-known SIDs, such as Docker Desktop's docker-users.
func localPrivilegedGroups() map[string]bool {
	out := map[string]bool{}
	if sid, _, _, err := windows.LookupSID("", "docker-users"); err == nil {
		out[sid.String()] = true
	}
	return out
}

// allowedPrivileges are the ordinary user privileges. Any other privilege,
// enabled or not, refuses the broker; services must restrict theirs with
// `sc.exe privs`.
var allowedPrivileges = map[string]bool{
	"SeChangeNotifyPrivilege": true, "SeIncreaseWorkingSetPrivilege": true, "SeShutdownPrivilege": true,
	"SeUndockPrivilege": true, "SeTimeZonePrivilege": true, "SeCreateGlobalPrivilege": true,
}

var procLookupPrivilegeName = windows.NewLazySystemDLL("advapi32.dll").NewProc("LookupPrivilegeNameW")

func privilegeName(luid windows.LUID) string {
	buf := make([]uint16, 128)
	n := uint32(len(buf))
	r, _, _ := procLookupPrivilegeName.Call(0, uintptr(unsafe.Pointer(&luid)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return fmt.Sprintf("privilege %d:%d", luid.HighPart, luid.LowPart)
	}
	return windows.UTF16ToString(buf[:n])
}

// tokenFacts is what the process token proves about this process.
type tokenFacts struct {
	user        string
	groups      []string
	privileges  []string
	elevated    bool
	virtualized bool
	container   bool
	integrity   uint32
}

func currentToken() (*tokenFacts, error) {
	t := windows.GetCurrentProcessToken()
	f := &tokenFacts{elevated: t.IsElevated()}
	u, err := t.GetTokenUser()
	if err != nil {
		return nil, err
	}
	f.user = u.User.Sid.String()
	groups, err := t.GetTokenGroups()
	if err != nil {
		return nil, err
	}
	for _, g := range unsafe.Slice(&groups.Groups[0], groups.GroupCount) {
		sid := g.Sid.String()
		if g.Attributes&windows.SE_GROUP_INTEGRITY != 0 {
			if parts := strings.Split(sid, "-"); len(parts) == 4 && parts[2] == "16" {
				if n, err := strconv.ParseUint(parts[3], 10, 32); err == nil {
					f.integrity = uint32(n)
				}
			}
			continue
		}
		f.groups = append(f.groups, sid)
	}
	b, err := tokenInfo(t, windows.TokenPrivileges)
	if err != nil {
		return nil, err
	}
	privs := (*windows.Tokenprivileges)(unsafe.Pointer(&b[0]))
	for _, p := range unsafe.Slice(&privs.Privileges[0], privs.PrivilegeCount) {
		f.privileges = append(f.privileges, privilegeName(p.Luid))
	}
	if f.virtualized, err = tokenFlag(t, windows.TokenVirtualizationEnabled); err != nil {
		return nil, err
	}
	if f.container, err = tokenFlag(t, tokenIsAppContainer); err != nil {
		return nil, err
	}
	return f, nil
}

// brokerProblems lists why a token cannot run the broker.
func (f *tokenFacts) brokerProblems(local map[string]bool) []string {
	var out []string
	if !brokerSID(f.user) {
		out = append(out, "it must run as a dedicated local or domain user account, not "+f.user)
	}
	if f.elevated || f.integrity >= integrityHigh {
		out = append(out, "it must not run elevated")
	}
	if found := privilegedGroups(f.groups, local); len(found) > 0 {
		out = append(out, "its account must not belong to administrative groups: "+strings.Join(found, ", "))
	}
	var extra []string
	for _, p := range f.privileges {
		if !allowedPrivileges[p] {
			extra = append(extra, p)
		}
	}
	if len(extra) > 0 {
		slices.Sort(extra)
		out = append(out, "its token holds privileges beyond an ordinary user's (restrict a service with sc.exe privs): "+strings.Join(extra, ", "))
	}
	if f.virtualized {
		out = append(out, "UAC file and registry virtualization must be off")
	}
	if f.container {
		out = append(out, "it must not run in an AppContainer")
	}
	return out
}

// checkBrokerProcess derives the broker's privileges from its token, not
// from configuration: a dedicated user account, not elevated, in no
// administrative group, with only ordinary user privileges.
func checkBrokerProcess() error {
	f, err := currentToken()
	if err != nil {
		return fmt.Errorf("cannot inspect the vault broker token: %w", err)
	}
	if problems := f.brokerProblems(localPrivilegedGroups()); len(problems) > 0 {
		return errors.New("vault broker refuses to run: " + strings.Join(problems, "; "))
	}
	return nil
}

// checkFrontendProcess refuses UAC virtualization, which could redirect
// reads of ProgramData to a per-user store.
func checkFrontendProcess() error {
	virtualized, err := tokenFlag(windows.GetCurrentProcessToken(), windows.TokenVirtualizationEnabled)
	if err != nil {
		return err
	}
	if virtualized {
		return errors.New("UAC file virtualization is enabled for this process; the vault marker cannot be verified")
	}
	return nil
}

// checkExecutable requires the running binary, its directory, and every
// ancestor to be unmodifiable by any account other than an administrator
// principal and the broker, without reparse points. Otherwise the frontend
// could replace the code that reads the protected profile, or plant a DLL
// beside it.
func checkExecutable(account string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	d, err := openTrusted(filepath.Dir(exe), account, true, dirRead)
	if err != nil {
		return fmt.Errorf("vault binary location is not trusted: %w", err)
	}
	defer func() { _ = d.Close() }()
	name := filepath.Base(exe)
	h, err := ntOpen(d.h, name, fileRead, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, nil)
	if err != nil {
		return fmt.Errorf("open vault binary %s: %w", exe, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if _, err = kind(h, exe, false); err != nil {
		return fmt.Errorf("vault binary is not trusted: %w", err)
	}
	s, err := readSecurity(h, exe)
	if err != nil {
		return err
	}
	if !adminSID(s.owner) && s.owner != d.account {
		return fmt.Errorf("vault binary %s is owned by %s, not an administrator or the vault account", exe, s.owner)
	}
	if err = s.grants(exe, trustedFor(d.account), readRights, false); err != nil {
		return fmt.Errorf("vault binary is not trusted: %w", err)
	}
	return nil
}
