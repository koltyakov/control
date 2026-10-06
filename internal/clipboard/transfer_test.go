package clipboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClipboardFileStreaming(t *testing.T) {
	source, dest := t.TempDir(), t.TempDir()
	data := bytes.Repeat([]byte{0, 255, 1, 128}, (9<<20)/4)
	path := filepath.Join(source, "file with spaces.bin")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(source, "empty.txt")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	batch, err := Snapshot(Value{Paths: []string{path, empty}, Text: "file URI text must not win"})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	if batch.Content.Kind != "files" || batch.Content.Text != "" || batch.Content.Result().Bytes != int64(len(data)) {
		t.Fatal(batch.Content)
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	r, w := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := batch.Send(context.Background(), w)
		_ = w.CloseWithError(err)
		done <- err
	}()
	names, err := Receive(context.Background(), batch.Content, root, r)
	_ = r.Close()
	if err != nil || len(names) != 2 {
		t.Fatal(names, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, batch.Content.Files[0].Name))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("file bytes differ", err)
	}
	if _, err = Receive(context.Background(), batch.Content, root, strings.NewReader("")); err == nil {
		t.Fatal("existing destination accepted")
	}
}

func TestClipboardReceiveFailureCleanup(t *testing.T) {
	for _, test := range []string{"truncated", "checksum", "cancelled", "symlink"} {
		t.Run(test, func(t *testing.T) {
			dest := t.TempDir()
			root, err := os.OpenRoot(dest)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			content := Content{Kind: "files", Files: []File{{Name: "output.txt", Size: 4}}}
			payload := append([]byte("data"), make([]byte, sha256.Size)...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch test {
			case "truncated":
				payload = payload[:2]
			case "cancelled":
				cancel()
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(dest, "output.txt")); err != nil {
					t.Skip("symlinks unavailable", err)
				}
			}
			if _, err := Receive(ctx, content, root, bytes.NewReader(payload)); err == nil {
				t.Fatal("invalid transfer succeeded")
			}
			entries, err := os.ReadDir(dest)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if test != "symlink" || entry.Name() != "output.txt" {
					t.Fatal("partial file retained", entry.Name())
				}
			}
		})
	}
}

func TestClipboardValidation(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../outside", "a/b", `a\b`, "C:escape", "name\x00", "CON", "nul.txt", "LPT1.txt", "COM¹.txt", "trailing.", "space ", "a\nfile"} {
		if err := (Content{Kind: "files", Files: []File{{Name: name}}}).Validate(); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	for _, content := range []Content{
		{Kind: "files", Files: []File{{Name: "a"}, {Name: "A"}}},
		{Kind: "files", Files: []File{{Name: "a", Size: -1}}},
		{Kind: "files", Files: []File{{Name: "a", Size: MaxBytes}, {Name: "b", Size: 1}}},
		{Kind: "files", Files: make([]File, MaxFiles+1)},
		{Kind: "text", Text: strings.Repeat("a", MaxText+1)},
		{Kind: "text", Text: string([]byte{255})},
		{Kind: "text", Files: []File{{Name: "a"}}},
		{Kind: "image"},
	} {
		if err := content.Validate(); err == nil {
			t.Errorf("accepted invalid content kind %q", content.Kind)
		}
	}
	if _, err := Snapshot(Value{Paths: []string{t.TempDir()}}); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := Snapshot(Value{Paths: []string{"relative.txt"}}); err == nil {
		t.Fatal("relative source path accepted")
	}
	missing := filepath.Join(t.TempDir(), "missing.txt")
	if _, err := Snapshot(Value{Paths: []string{missing}}); err == nil || strings.Contains(err.Error(), filepath.Dir(missing)) {
		t.Fatal("source path exposed in error", err)
	}
}

func TestClipboardSnapshotDoesNotReadBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	batch, err := Snapshot(Value{Paths: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	if offset, err := batch.files[0].Seek(0, io.SeekCurrent); err != nil || offset != 0 {
		t.Fatal("snapshot read bytes", offset, err)
	}
	if err := os.WriteFile(path, []byte("changed size"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := batch.Send(context.Background(), io.Discard); err == nil {
		t.Fatal("changed source accepted")
	}
}
