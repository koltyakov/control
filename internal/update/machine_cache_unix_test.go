//go:build linux || darwin

package update

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMachineCacheCrossAccountPermissionsAndSpecialFiles(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	root, err := openMachineCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(".")
	if err != nil || info.Mode().Perm() != 0777 {
		t.Fatal("cache directory is not cross-account writable", info, err)
	}
	lock, err := lockMachineCache(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	info, err = root.Stat("download.lock")
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("cache lock is not cross-account readable", info, err)
	}
	data := []byte("trusted bytes")
	asset := cacheFixture(data)
	name := asset.SHA256 + ".blob"
	if err := unix.Mkfifo(filepath.Join(cache, name), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "binary")
	downloads := 0
	if err := downloadCached(t.Context(), cache, path, asset, func() error {
		downloads++
		return SaveBinary(bytes.NewReader(data), path, asset)
	}); err != nil || downloads != 1 {
		t.Fatal("special file blocked safe download", err, downloads)
	}
	info, err = root.Stat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0644 {
		t.Fatal("published cache entry is not cross-account readable", info, err)
	}
	// A FIFO in place of the lock must also fail without blocking or changing
	// arbitrary filesystem objects. Direct private downloads remain available.
	if err := root.Remove("download.lock"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(cache, "download.lock"), 0600); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "binary")
	if err := downloadCached(t.Context(), cache, path, asset, func() error {
		downloads++
		return SaveBinary(bytes.NewReader(data), path, asset)
	}); err != nil || downloads != 2 {
		t.Fatal("invalid lock blocked private download", err, downloads)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
