//go:build darwin || linux

package vaultstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/koltyakov/control/internal/identity"
	"golang.org/x/sys/unix"
)

// hooks exist only so tests can stand in for a second OS account. Production
// code never changes them.
var hooks = struct {
	// uid is the principal identity compared with marker and profile
	// principals. File ownership checks always use kernel-reported owners.
	uid             func() int
	lookup          func(name string) (Principal, error)
	lookupID        func(uid int) (Principal, error)
	checkProcess    func() error
	checkExecutable func(owner int) error
	defaultLayout   func() Layout
}{
	uid:             unix.Geteuid,
	lookup:          lookupPrincipal,
	lookupID:        principalFor,
	checkProcess:    checkBrokerProcess,
	checkExecutable: checkExecutable,
	defaultLayout:   func() Layout { return Layout{Root: defaultRoot} },
}

func lookupPrincipal(name string) (Principal, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return Principal{}, fmt.Errorf("look up account %q: %w", name, err)
	}
	return Principal{UID: u.Uid, Name: u.Username}, nil
}

func principalFor(uid int) (Principal, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return Principal{}, fmt.Errorf("look up uid %d: %w", uid, err)
	}
	return Principal{UID: u.Uid, Name: u.Username}, nil
}

// dir is an open directory reached without following symbolic links. Every
// name inside it is opened relative to the descriptor, never by path.
type dir struct {
	fd   int
	path string
	// owner is the non-root account trusted for this chain, -1 if none.
	owner int
}

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

func (d *dir) Close() error {
	if d == nil || d.fd < 0 {
		return nil
	}
	err := unix.Close(d.fd)
	d.fd = -1
	return err
}

func cleanAbs(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("vault path %q must be absolute and clean", path)
	}
	return nil
}

// openTrusted walks path from "/" one component at a time without following
// symbolic links. Each component must be owned by root or owner, must not be
// writable by group or other users (root-owned sticky ancestors such as /tmp
// excepted), and must carry no ACL entry granting access to anyone else. A
// different account therefore cannot replace any ancestor.
//
// A negative owner adopts the first non-root owner found on the walk; every
// later component must then have that owner. The caller must check the
// adopted owner (dir.owner) against the accounts it distrusts.
func openTrusted(path string, owner int) (*dir, error) {
	if err := cleanAbs(path); err != nil {
		return nil, err
	}
	if owner == 0 {
		return nil, errors.New("vault account must not be root")
	}
	if owner < 0 {
		owner = -1
	}
	fd, err := unix.Open("/", dirFlags, 0)
	if err != nil {
		return nil, err
	}
	d := &dir{fd: fd, path: "/", owner: owner}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if path == "/" {
		parts = nil
	}
	if err = checkComponent(d.fd, "/", &d.owner, len(parts) > 0); err != nil {
		_ = d.Close()
		return nil, err
	}
	for i, name := range parts {
		next, err := unix.Openat(d.fd, name, dirFlags, 0)
		current := filepath.Join(d.path, name)
		owner := d.owner
		_ = d.Close()
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				return nil, fmt.Errorf("%s is a symbolic link or not a directory; use the canonical path (for example /private/var instead of /var on macOS): %w", current, err)
			}
			return nil, fmt.Errorf("open %s without following links: %w", current, err)
		}
		d = &dir{fd: next, path: current, owner: owner}
		if err = checkComponent(next, current, &d.owner, i < len(parts)-1); err != nil {
			_ = d.Close()
			return nil, err
		}
	}
	return d, nil
}

// checkComponent verifies one component. A negative *owner adopts the
// component's non-root owner.
func checkComponent(fd int, path string, owner *int, ancestor bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	uid := int(st.Uid)
	if uid != 0 && *owner < 0 {
		*owner = uid
	}
	if uid != 0 && uid != *owner {
		return fmt.Errorf("%s is owned by uid %d, not root or the vault account", path, uid)
	}
	mode := uint32(st.Mode)
	if mode&0o022 != 0 && (!ancestor || uid != 0 || mode&unix.S_ISVTX == 0) {
		return fmt.Errorf("%s is writable by other accounts", path)
	}
	return checkACL(fd, path)
}

// sub opens or creates a private child directory owned by owner with no
// group or other permissions.
func (d *dir) sub(name string, owner int, create bool) (*dir, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid vault entry %q", name)
	}
	if create {
		if err := unix.Mkdirat(d.fd, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, err
		}
	}
	fd, err := unix.Openat(d.fd, name, dirFlags, 0)
	path := filepath.Join(d.path, name)
	if err != nil {
		return nil, fmt.Errorf("open %s without following links: %w", path, err)
	}
	child := &dir{fd: fd, path: path, owner: owner}
	if err = checkPrivate(fd, path, owner, true); err != nil {
		_ = child.Close()
		return nil, err
	}
	if create {
		err = unix.Fsync(d.fd)
	}
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	return child, nil
}

func checkPrivate(fd int, path string, owner int, directory bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	kind := uint32(st.Mode) & unix.S_IFMT
	if directory && kind != unix.S_IFDIR || !directory && (kind != unix.S_IFREG || st.Nlink != 1) {
		return fmt.Errorf("%s is not a private %s", path, map[bool]string{true: "directory", false: "single-link regular file"}[directory])
	}
	if int(st.Uid) != owner {
		return fmt.Errorf("%s is owned by uid %d, not uid %d", path, st.Uid, owner)
	}
	if uint32(st.Mode)&0o077 != 0 {
		return fmt.Errorf("%s is accessible to other accounts", path)
	}
	return checkACL(fd, path)
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

// readFile reads a regular single-link file. private requires owner and mode
// 0600 or stricter; otherwise owner must match and others must not write.
func (d *dir) readFile(name string, owner int, private bool) ([]byte, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid vault entry %q", name)
	}
	// O_NONBLOCK keeps a planted FIFO from blocking; regular files ignore it.
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	path := filepath.Join(d.path, name)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("%s: %w", path, os.ErrNotExist)
		}
		return nil, fmt.Errorf("open %s without following links: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	if private {
		err = checkPrivate(fd, path, owner, false)
	} else {
		err = checkPublic(fd, path, owner)
	}
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFileSize {
		return nil, fmt.Errorf("%s is too large", path)
	}
	return b, nil
}

func checkPublic(fd int, path string, owner int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return fmt.Errorf("%s is not a single-link regular file", path)
	}
	if int(st.Uid) != owner {
		return fmt.Errorf("%s is owned by uid %d, not uid %d", path, st.Uid, owner)
	}
	if uint32(st.Mode)&0o022 != 0 {
		return fmt.Errorf("%s is writable by other accounts", path)
	}
	return checkACL(fd, path)
}

// writeFile atomically replaces name, or with exclusive publishes it only if
// it does not exist. The data is synced before publication and the directory
// is synced after it.
func (d *dir) writeFile(name string, data []byte, perm uint32, exclusive bool) error {
	if !validName(name) {
		return fmt.Errorf("invalid vault entry %q", name)
	}
	tmp := ".write-" + identity.NewID()
	fd, err := unix.Openat(d.fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(d.path, tmp))
	published := false
	defer func() {
		if !published {
			_ = unix.Unlinkat(d.fd, tmp, 0)
		}
	}()
	err = f.Chmod(os.FileMode(perm))
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if exclusive {
		if err = unix.Linkat(d.fd, tmp, d.fd, name, 0); err != nil {
			if errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("%s: %w", filepath.Join(d.path, name), os.ErrExist)
			}
			return err
		}
		if err = unix.Unlinkat(d.fd, tmp, 0); err != nil {
			return err
		}
	} else if err = unix.Renameat(d.fd, tmp, d.fd, name); err != nil {
		return err
	}
	published = true
	return unix.Fsync(d.fd)
}

func (d *dir) rename(from, to string) error {
	if !validName(from) || !validName(to) {
		return errors.New("invalid vault entry")
	}
	if err := unix.Renameat(d.fd, from, d.fd, to); err != nil {
		return err
	}
	return unix.Fsync(d.fd)
}

func (d *dir) remove(name string) error {
	if !validName(name) {
		return fmt.Errorf("invalid vault entry %q", name)
	}
	if err := unix.Unlinkat(d.fd, name, 0); err != nil {
		return err
	}
	return unix.Fsync(d.fd)
}

// removeAll deletes a private tree below d without following links.
func (d *dir) removeAll(name string, owner int) error {
	child, err := d.sub(name, owner, false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := child.list()
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				err = child.removeAll(e.Name(), owner)
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
	if err = unix.Unlinkat(d.fd, name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return unix.Fsync(d.fd)
}

func (d *dir) list() ([]os.DirEntry, error) {
	fd, err := unix.Dup(d.fd)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), d.path)
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}

// lock takes an exclusive flock(2) on a private lock file. It interoperates
// with gofrs/flock, which the tunnel service uses.
func (d *dir) lock(name string, owner int) (io.Closer, error) {
	fd, err := unix.Openat(d.fd, name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(d.path, name))
	if err = checkPrivate(fd, f.Name(), owner, false); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s is held by a running process", ErrBusy, f.Name())
		}
		return nil, err
	}
	return f, nil
}

// probeLock reports ErrBusy when another process holds an existing lock file.
// A missing lock file is not busy. The probe never creates the file.
func (d *dir) probeLock(name string) error {
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("%w: %s is held by a running process; stop it first", ErrBusy, filepath.Join(d.path, name))
		}
		return err
	}
	return unix.Flock(fd, unix.LOCK_UN)
}

// openHome opens the frontend's own Control home. The home itself may be any
// directory the account owns; entries below it are never followed as links.
func openHome(home string) (*dir, error) {
	abs, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(abs, dirFlags&^unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, fmt.Errorf("%s: %w", abs, os.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	d := &dir{fd: fd, path: abs, owner: unix.Geteuid()}
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err == nil && int(st.Uid) != unix.Geteuid() {
		err = fmt.Errorf("%s is not owned by this account", abs)
	}
	if err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}

// homeSub opens an existing directory below the home without following links.
func (d *dir) homeSub(name string) (*dir, error) {
	fd, err := unix.Openat(d.fd, name, dirFlags, 0)
	path := filepath.Join(d.path, name)
	if err != nil {
		return nil, fmt.Errorf("open %s without following links: %w", path, err)
	}
	child := &dir{fd: fd, path: path, owner: d.owner}
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err == nil && int(st.Uid) != unix.Geteuid() {
		err = fmt.Errorf("%s is not owned by this account", path)
	}
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	return child, nil
}

// homeMkdir creates or opens a 0700 directory below the home.
func (d *dir) homeMkdir(name string) (*dir, error) {
	if err := unix.Mkdirat(d.fd, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	return d.homeSub(name)
}

// readHomeFile reads a frontend-owned regular file without following links.
func (d *dir) readHomeFile(name string) ([]byte, error) {
	return d.readFile(name, unix.Geteuid(), false)
}

// privilegedGroupNames can administer the host or read other accounts' files.
var privilegedGroupNames = map[string]bool{
	"root": true, "wheel": true, "admin": true, "sudo": true, "docker": true,
	"lxd": true, "incus-admin": true, "libvirt": true, "disk": true, "shadow": true,
}

func groupName(gid int) string {
	g, err := user.LookupGroupId(strconv.Itoa(gid))
	if err != nil {
		return ""
	}
	return g.Name
}

func privilegedGroups(gids []int, name func(int) string) []string {
	var found []string
	for _, gid := range gids {
		n := name(gid)
		if gid == 0 || privilegedGroupNames[n] {
			found = append(found, fmt.Sprintf("%s(%d)", n, gid))
		}
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// checkBrokerProcess derives the broker's privileges from the kernel, not
// from configuration: the vault must not run as root, setuid, or with an
// administrative group, and must not have library injection configured.
func checkBrokerProcess() error {
	if unix.Getuid() != unix.Geteuid() || unix.Getgid() != unix.Getegid() {
		return errors.New("vault broker must not run setuid or setgid")
	}
	if unix.Geteuid() == 0 {
		return errors.New("vault broker must run as a dedicated non-root account")
	}
	groups, err := unix.Getgroups()
	if err != nil {
		return err
	}
	if found := privilegedGroups(append(groups, unix.Getgid()), groupName); len(found) > 0 {
		return fmt.Errorf("vault broker account must not belong to administrative groups: %s", strings.Join(found, ", "))
	}
	for _, name := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH", "DYLD_FRAMEWORK_PATH"} {
		if _, ok := os.LookupEnv(name); ok {
			return fmt.Errorf("vault broker refuses to run with %s set", name)
		}
	}
	return nil
}

// checkExecutable requires the running binary, and every directory above it,
// to be unmodifiable by any account other than root and the broker. Otherwise
// the frontend could replace the code that reads the protected profile.
func checkExecutable(owner int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	d, err := openTrusted(filepath.Dir(resolved), owner)
	if err != nil {
		return fmt.Errorf("vault binary location is not trusted: %w", err)
	}
	defer func() { _ = d.Close() }()
	fd, err := unix.Openat(d.fd, filepath.Base(resolved), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if err = checkComponent(fd, resolved, &owner, false); err != nil {
		return fmt.Errorf("vault binary is not trusted: %w", err)
	}
	return sameRunningImage(fd)
}

// CheckServiceFile verifies a broker service definition, such as a launchd
// plist or systemd unit, before it is trusted. The file and every directory
// above it must be owned by root, unwritable by other accounts, free of ACL
// entries granting access, and reached without symbolic links: whoever can
// edit the definition chooses the code and arguments the broker runs.
func CheckServiceFile(path string) error {
	d, err := openTrusted(filepath.Dir(path), -1)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if d.owner >= 0 {
		return fmt.Errorf("directories above %s must be owned by root", path)
	}
	fd, err := unix.Openat(d.fd, filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open %s without following links: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return err
	}
	if uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG || st.Uid != 0 || uint32(st.Mode)&0o022 != 0 {
		return fmt.Errorf("%s must be a root-owned regular file not writable by other accounts", path)
	}
	return checkACL(fd, path)
}
