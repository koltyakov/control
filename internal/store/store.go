// Package store provides crash-safe writes for small local metadata records.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func Write(path string, value any) error {
	// Metadata records are machine-read only; compact encoding writes and
	// syncs fewer bytes than indented JSON on every task/lease update.
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return Bytes(path, b, 0600)
}

func Bytes(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), TempPrefix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return RenameInDirectory(tmp, path)
}

// RenameInDirectory publishes a synced temporary file in the same directory.
// A sync failure is returned even though the rename may already be visible.
func RenameInDirectory(tmp, path string) error {
	if err := replaceFile(tmp, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func Read(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// Append durably appends records to a file, creating it if necessary. A newly
// created file's directory entry is synced before Append returns.
func Append(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && os.IsNotExist(statErr) {
		err = syncDirectory(filepath.Dir(path))
	}
	return err
}

// TempPrefix names Bytes' temporary files, which a crash can leave behind.
const TempPrefix = ".write-"

// RemoveStaleTemps deletes this package's temporary files in dir that are
// older than age. Younger files may belong to a write still in progress.
func RemoveStaleTemps(dir string, age time.Duration) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-age)
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), TempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
