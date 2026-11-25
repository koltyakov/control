package vaultstore

import (
	"fmt"
	"sync"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

const (
	aclTypeExtended = 0x00000100
	aclFirstEntry   = 0
	aclNextEntry    = -1
	aclExtendedDeny = 2
)

type aclFuncs struct {
	// getFD is called through purego.SyscallN, which captures errno in the
	// trampoline before any Go or race-detector code can overwrite it.
	getFD    uintptr
	getEntry func(acl uintptr, id int32, entry *uintptr) int32
	getTag   func(entry uintptr, tag *int32) int32
	free     func(obj uintptr) int32
}

// libSystem's ACL API reports extended (NFSv4-style) entries that POSIX mode
// bits do not show. purego calls it without CGO.
var darwinACL = sync.OnceValues(func() (*aclFuncs, error) {
	lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	f := &aclFuncs{}
	if f.getFD, err = purego.Dlsym(lib, "acl_get_fd_np"); err != nil {
		return nil, err
	}
	for name, fn := range map[string]any{"acl_get_entry": &f.getEntry, "acl_get_tag_type": &f.getTag, "acl_free": &f.free} {
		symbol, err := purego.Dlsym(lib, name)
		if err != nil {
			return nil, err
		}
		purego.RegisterFunc(fn, symbol)
	}
	return f, nil
})

// checkACL refuses any extended ACL entry other than a deny. macOS adds
// deny-only entries such as "everyone deny delete" to home directories.
func checkACL(fd int, path string) error {
	f, err := darwinACL()
	if err != nil {
		return fmt.Errorf("cannot inspect ACLs: %w", err)
	}
	acl, _, errno := purego.SyscallN(f.getFD, uintptr(fd), aclTypeExtended)
	if acl == 0 {
		// libSystem reports a missing extended ACL as ENOENT.
		if e := unix.Errno(uint32(errno)); e != unix.ENOENT {
			return fmt.Errorf("cannot inspect ACL of %s: acl_get_fd_np: %w", path, e)
		}
		return nil
	}
	defer f.free(acl)
	var entry uintptr
	for id := int32(aclFirstEntry); f.getEntry(acl, id, &entry) == 0; id = aclNextEntry {
		var tag int32
		if f.getTag(entry, &tag) != 0 {
			return fmt.Errorf("cannot inspect ACL of %s", path)
		}
		if tag != aclExtendedDeny {
			return fmt.Errorf("%s has an ACL entry granting access; remove it with chmod -N", path)
		}
	}
	return nil
}

// macOS has no reliable path for the running image's inode; the service
// definition must point at the verified path.
func sameRunningImage(int) error { return nil }

// defaultRoot is created by an administrator and owned by the broker.
const defaultRoot = "/Library/Application Support/Control Vault"
