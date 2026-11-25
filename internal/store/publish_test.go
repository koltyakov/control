package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishNewNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	tmp, path := filepath.Join(dir, ".tmp"), filepath.Join(dir, "file")
	if err := os.WriteFile(tmp, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := PublishNew(tmp, path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("publish over an existing file: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "old" {
		t.Fatal("existing file replaced")
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatal("refused publication removed the temporary file:", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := PublishNew(tmp, path); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "new" {
		t.Fatal("published content", string(data))
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary name remains after publication:", err)
	}
}
