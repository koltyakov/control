//go:build !windows

package gateway

import (
	"os"
	"syscall"
)

// checkDatabaseOwner refuses another account, such as root, whose SQLite
// connection could leave -wal/-shm files the gateway account cannot open.
func checkDatabaseOwner(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return databaseOwnerMatches(uint64(st.Uid), uint64(os.Geteuid()))
}
