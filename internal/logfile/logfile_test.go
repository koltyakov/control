package logfile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestTrimKeepsOnePreviousFileAndAppendsAtNewEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.log")
	writer, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	old := bytes.Repeat([]byte("old line\n"), 16)
	if _, err = writer.Write(old); err != nil {
		t.Fatal(err)
	}
	if err = trim(path, int64(len(old))); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Size() != int64(len(old)) {
		t.Fatal("log at the limit was rotated")
	}
	if _, err = writer.Write([]byte("over\n")); err != nil {
		t.Fatal(err)
	}
	if err = trim(path, int64(len(old))); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(path + ".1")
	if err != nil || !bytes.Equal(previous, append(old, "over\n"...)) {
		t.Fatal("previous log not kept", err)
	}
	// The existing append-mode handle keeps writing at the truncated end.
	if _, err = writer.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != "new\n" {
		t.Fatalf("current log %q %v", current, err)
	}
	if err = trim(filepath.Join(t.TempDir(), "missing.log"), 1); err != nil {
		t.Fatal("missing log failed", err)
	}
}
