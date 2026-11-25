package node

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func exportTree(t *testing.T, n *Node, path string) (model.Artifact, error) {
	t.Helper()
	var a model.Artifact
	err := n.Call(t.Context(), "", "artifacts.export", map[string]any{"path": path}, &a)
	return a, err
}

func TestDirectoryExportIsDeterministic(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	work := n.Config.WorkDir
	long := strings.Repeat("long-name-", 12) + "é.txt"
	for i, tree := range []string{"one", "two"} {
		for name, data := range map[string]string{"b.txt": "bee", "a/nested.txt": "nested", "a-b.txt": "dash", "a/" + long: "long"} {
			writeTestFile(t, filepath.Join(work, tree, filepath.FromSlash(name)), data)
		}
		if err := os.MkdirAll(filepath.Join(work, tree, "empty"), 0700); err != nil {
			t.Fatal(err)
		}
		// Modification times and permissions differ between the trees.
		stamp := time.Now().Add(-time.Duration(i+1) * time.Hour)
		if err := os.Chtimes(filepath.Join(work, tree, "b.txt"), stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if err := os.Chmod(filepath.Join(work, tree, "b.txt"), 0755); err != nil {
				t.Fatal(err)
			}
		}
	}
	first, err := exportTree(t, n, "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := exportTree(t, n, "two/")
	// Content addressing returns the stored record, named after the first export.
	if err != nil || second.ID != first.ID || first.Name != "one.tar" {
		t.Fatal("identical trees produced different artifacts", first, second, err)
	}
	f, err := os.Open(n.artifactPath(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	r := tar.NewReader(f)
	var names []string
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || !h.ModTime.Equal(time.Unix(0, 0)) || strings.HasPrefix(h.Name, "/") || strings.Contains(h.Name, "..") {
			t.Fatal("header not normalized", h)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if h.Mode != 0755 || !strings.HasSuffix(h.Name, "/") {
				t.Fatal("directory header", h)
			}
		case tar.TypeReg:
			b, err := io.ReadAll(r)
			if err != nil || h.Mode != 0644 || int64(len(b)) != h.Size {
				t.Fatal("file header", h, err)
			}
			if h.Name == "a/nested.txt" && string(b) != "nested" {
				t.Fatal("content", string(b))
			}
		default:
			t.Fatal("unexpected entry type", h)
		}
	}
	// Byte order of the path without the directory slash.
	want := []string{"a/", "a-b.txt", "a/" + long, "a/nested.txt", "b.txt", "empty/"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("archive entries %q, want %q", names, want)
	}
	writeTestFile(t, filepath.Join(work, "two", "b.txt"), "BEE")
	if changed, err := exportTree(t, n, "two"); err != nil || changed.ID == first.ID {
		t.Fatal("changed content kept the same artifact", changed, err)
	}
	var description map[string]any
	if err = n.Call(t.Context(), "", "node.describe", map[string]any{}, &description); err != nil {
		t.Fatal(err)
	}
	if export, _ := description["artifactExport"].(map[string]any); export["directories"] != dirArchiveFormat {
		t.Fatal("directory export not advertised", description["artifactExport"])
	}
}

// An empty staging directory, such as one left by an earlier version, holds
// no bytes: export skips it, so the tree archives exactly as without it. Only
// the name the node creates qualifies; other spellings are still refused.
func TestDirectoryExportSkipsEmptyStaging(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	work := n.Config.WorkDir
	for _, tree := range []string{"clean", "leftover", "cased"} {
		writeTestFile(t, filepath.Join(work, tree, "sub", "file.txt"), "content")
	}
	for _, dir := range []string{"leftover/.control-uploads", "leftover/sub/.control-uploads", "cased/.CONTROL-UPLOADS"} {
		if err := os.Mkdir(filepath.Join(work, filepath.FromSlash(dir)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	clean, err := exportTree(t, n, "clean")
	if err != nil {
		t.Fatal(err)
	}
	if leftover, err := exportTree(t, n, "leftover"); err != nil || leftover.ID != clean.ID {
		t.Fatal("empty staging changed the archive", clean, leftover, err)
	}
	if _, err = exportTree(t, n, "cased"); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatal("differently cased staging name skipped", err)
	}
}

func TestDirectoryExportRefusals(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	work := n.Config.WorkDir
	writeTestFile(t, filepath.Join(work, "ok", "file.txt"), "ok")
	if _, err := exportTree(t, n, "."); err == nil {
		t.Fatal("filesystem root exported")
	}
	writeTestFile(t, filepath.Join(work, "staged", "sub", ".control-uploads", "x.partial"), "private")
	if _, err := exportTree(t, n, "staged"); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatal("upload staging exported", err)
	}
	writeTestFile(t, filepath.Join(work, "linked", "file.txt"), "ok")
	if err := os.Symlink(filepath.Join(work, "ok"), filepath.Join(work, "linked", "link")); err != nil {
		t.Logf("symbolic links unavailable: %v", err)
	} else if _, err = exportTree(t, n, "linked"); err == nil || !strings.Contains(err.Error(), "symbolic link or special file") {
		t.Fatal("symbolic link archived", err)
	}
	many := filepath.Join(work, "many")
	if err := os.MkdirAll(many, 0700); err != nil {
		t.Fatal(err)
	}
	for i := range dirArchiveMaxEntries + 1 {
		if err := os.WriteFile(filepath.Join(many, fmt.Sprintf("f%05d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := exportTree(t, n, "many"); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatal("entry limit", err)
	}
	if a, err := exportTree(t, n, "ok/file.txt"); err != nil || a.Name != "file.txt" {
		t.Fatal("regular file export changed", a, err)
	}
	entries, err := os.ReadDir(filepath.Join(n.Config.DataDir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".import-") {
			t.Fatal("refused export left staging", e.Name())
		}
	}
}
