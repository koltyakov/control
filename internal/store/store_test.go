package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteRoundTripsCompactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	value := map[string]string{"name": "worker", "state": "running"}
	if err := Write(path, value); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("\n")) {
		t.Fatalf("stored record is not compact: %q", raw)
	}
	var got map[string]string
	if err := Read(path, &got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "worker" || got["state"] != "running" {
		t.Fatalf("round trip = %v", got)
	}
}

func TestRemoveStaleTemps(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, TempPrefix+"old")
	fresh := filepath.Join(dir, TempPrefix+"fresh")
	record := filepath.Join(dir, "record.json")
	for _, path := range []string{old, fresh, record} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{old, record} {
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveStaleTemps(dir, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale temporary file kept", err)
	}
	for _, path := range []string{fresh, record} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("removed a file it must keep", path, err)
		}
	}
	if err := RemoveStaleTemps(filepath.Join(dir, "missing"), time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestWriteReplacesExistingRecordsAndLongPaths(t *testing.T) {
	dir := t.TempDir()
	// Exceed the legacy 260-character Windows path limit.
	deep := dir
	for len(deep) < 300 {
		deep = filepath.Join(deep, "segment-of-a-long-metadata-path")
	}
	for _, path := range []string{filepath.Join(dir, "record.json"), filepath.Join(deep, "record.json")} {
		for _, value := range []string{"first", "second"} {
			if err := Write(path, value); err != nil {
				t.Fatal(len(path), err)
			}
		}
		var got string
		if err := Read(path, &got); err != nil || got != "second" {
			t.Fatalf("replaced record = %q, %v", got, err)
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), TempPrefix) {
				t.Fatal("temporary file left behind", e.Name())
			}
		}
	}
}
