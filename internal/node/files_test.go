package node

import (
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/model"
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

func TestFileListAndWriteValidateHandlesAndPreserveOrdinaryFiles(t *testing.T) {
	n := localNode(t, nil)
	write := func(path, data string, offset int64, truncate bool) error {
		t.Helper()
		_, err := n.filesWrite(t.Context(), model.JSON(map[string]any{"path": path, "data": base64.StdEncoding.EncodeToString([]byte(data)), "offset": offset, "truncate": truncate}), Execution{})
		return err
	}
	if err := write("nested/file", "abcdef", 0, false); err != nil {
		t.Fatal("creating a regular file failed", err)
	}
	for _, test := range []struct {
		data     string
		offset   int64
		truncate bool
		want     string
	}{
		{"XY", 2, false, "abXYef"},
		{"z", 0, true, "z"},
		{"q", 2, true, "\x00\x00q"},
	} {
		if err := write("nested/file", test.data, test.offset, test.truncate); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(n.Config.WorkDir, "nested", "file"))
		if err != nil || string(got) != test.want {
			t.Fatal("ordinary write semantics changed", string(got), test.want, err)
		}
	}
	list, err := n.filesList(t.Context(), model.JSON(map[string]string{"path": "nested"}), Execution{})
	if err != nil || len(list.([]map[string]any)) != 1 {
		t.Fatal("directory listing failed", list, err)
	}
	for _, path := range []string{"nested/file", "../outside"} {
		if _, err := n.filesList(t.Context(), model.JSON(map[string]string{"path": path}), Execution{}); err == nil {
			t.Fatal("listed a non-directory or escaped path", path)
		}
	}
	for _, path := range []string{"nested", "../outside"} {
		if err := write(path, "replacement", 0, true); err == nil {
			t.Fatal("wrote a non-file or escaped path", path)
		}
	}
	if info, err := os.Stat(filepath.Join(n.Config.WorkDir, "nested")); err != nil || !info.IsDir() {
		t.Fatal("rejected write changed the directory", err)
	}
}
