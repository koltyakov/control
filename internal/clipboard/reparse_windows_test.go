package clipboard

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/testutil"
)

func TestClipboardRejectsWindowsReparsePoints(t *testing.T) {
	for _, kind := range []string{"junction", "reparse-file"} {
		t.Run(kind, func(t *testing.T) {
			dest, outside := t.TempDir(), t.TempDir()
			keep := filepath.Join(outside, "keep")
			if err := os.WriteFile(keep, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dest, "output.txt")
			if kind == "junction" {
				testutil.Junction(t, outside, link)
			} else {
				testutil.ReparseFile(t, link)
			}
			batch, err := Snapshot(Value{Paths: []string{link}})
			if batch != nil {
				batch.Close()
			}
			if err == nil {
				t.Fatal("reparse point accepted as a regular clipboard source")
			}
			root, err := os.OpenRoot(dest)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			sum := sha256.Sum256([]byte("data"))
			payload := append([]byte("data"), sum[:]...)
			reader := bytes.NewReader(payload)
			content := Content{Kind: "files", Files: []File{{Name: "output.txt", Size: 4}}}
			if names, err := Receive(t.Context(), content, root, reader); err == nil || len(names) != 0 || reader.Len() != len(payload) {
				t.Fatal("reparse destination was not rejected before reading bytes", names, err)
			}
			if _, err := os.Lstat(link); err != nil {
				t.Fatal("existing reparse entry removed", err)
			}
			if data, err := os.ReadFile(keep); err != nil || string(data) != "untouched" {
				t.Fatal("clipboard changed outside target", err)
			}
			entries, err := os.ReadDir(dest)
			if err != nil || len(entries) != 1 {
				t.Fatal("rejected paste left temporary files", entries, err)
			}
		})
	}
}
