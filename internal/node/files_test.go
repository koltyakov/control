package node

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRegularFile(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("contents"), 0600); err != nil {
		t.Fatal(err)
	}
	f, info, err := openRegularFile(root, "file")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "contents" || info.Size() != int64(len(data)) {
		t.Fatal("regular file changed", info, string(data), err)
	}
	for _, name := range []string{".", "missing", "../outside"} {
		f, info, err := openRegularFile(root, name)
		if f != nil || info != nil || err == nil {
			t.Fatal("accepted a non-file or escaped root", name, f, info, err)
		}
	}
}
