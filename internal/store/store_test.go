package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
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
