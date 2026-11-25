//go:build windows

package gateway

import "os"

// checkDatabaseOwner is a no-op on Windows: new files in the state directory
// inherit its ACL, so an administrator's SQLite sidecar files stay usable by
// the gateway account.
func checkDatabaseOwner(os.FileInfo) error { return nil }
