package node

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func fileOp(t *testing.T, n *Node, method string, params map[string]any) (map[string]any, error) {
	t.Helper()
	var result map[string]any
	err := n.Call(t.Context(), "", method, params, &result)
	return result, err
}

func mustFileOp(t *testing.T, n *Node, method string, params map[string]any) map[string]any {
	t.Helper()
	result, err := fileOp(t, n, method, params)
	if err != nil {
		t.Fatal(method, params, err)
	}
	return result
}

func writeTestFile(t *testing.T, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, name, want string) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil || string(got) != want {
		t.Fatalf("%s = %q, %v; want %q", name, got, err, want)
	}
}

func assertMissing(t *testing.T, name string) {
	t.Helper()
	if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("entry should not exist", name, err)
	}
}

func assertNoCopyTemporaries(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".control-copy-") {
			t.Fatal("copy temporary left behind", entry.Name())
		}
	}
}

func symlinkOrSkip(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symbolic links need Developer Mode or elevation on Windows:", err)
		}
		t.Fatal(err)
	}
}

func TestFileWriteTextEncodingIsOptIn(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	text := "café\n日本 & <tag>\n"
	mustFileOp(t, n, "files.write", map[string]any{"path": "notes/text.txt", "data": text, "encoding": "text"})
	assertFileContent(t, filepath.Join(n.Config.WorkDir, "notes", "text.txt"), text)
	mustFileOp(t, n, "files.write", map[string]any{"path": "notes/text.txt", "data": base64.StdEncoding.EncodeToString([]byte("raw")), "truncate": true})
	assertFileContent(t, filepath.Join(n.Config.WorkDir, "notes", "text.txt"), "raw")
	mustFileOp(t, n, "files.write", map[string]any{"path": "notes/text.txt", "data": base64.StdEncoding.EncodeToString([]byte("B")), "encoding": "base64"})
	assertFileContent(t, filepath.Join(n.Config.WorkDir, "notes", "text.txt"), "Baw")
	// Text that is not base64 is still decoded as base64 by default.
	if _, err := fileOp(t, n, "files.write", map[string]any{"path": "notes/default.txt", "data": "plain text!"}); err == nil {
		t.Fatal("default encoding accepted non-base64 data")
	}
	if _, err := fileOp(t, n, "files.write", map[string]any{"path": "notes/hex.txt", "data": "00", "encoding": "hex"}); err == nil {
		t.Fatal("unknown write encoding accepted")
	}
	if _, err := fileOp(t, n, "files.write", map[string]any{"path": "notes/large.txt", "data": strings.Repeat("x", 1<<20+1), "encoding": "text"}); err == nil {
		t.Fatal("text write bypassed the 1 MiB bound")
	}
	for _, name := range []string{"default.txt", "hex.txt", "large.txt"} {
		assertMissing(t, filepath.Join(n.Config.WorkDir, "notes", name))
	}
	read := mustFileOp(t, n, "files.read", map[string]any{"path": "notes/text.txt", "encoding": "text"})
	if read["data"] != "Baw" || read["encoding"] != "text" {
		t.Fatal("text round trip", read)
	}
}

func TestFileStatMkdirAndRemove(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	work := n.Config.WorkDir
	writeTestFile(t, filepath.Join(work, "dir", "file"), "contents")
	stat := mustFileOp(t, n, "files.stat", map[string]any{"path": "dir/file"})
	if stat["type"] != "file" || stat["size"] != float64(len("contents")) || stat["modified"] == nil || stat["sha256"] != nil {
		t.Fatal("file stat", stat)
	}
	digest := sha256.Sum256([]byte("contents"))
	if stat = mustFileOp(t, n, "files.stat", map[string]any{"path": "dir/file", "sha256": true}); stat["sha256"] != hex.EncodeToString(digest[:]) {
		t.Fatal("file digest", stat)
	}
	for _, path := range []string{"dir", ".", ""} {
		if stat = mustFileOp(t, n, "files.stat", map[string]any{"path": path}); stat["type"] != "directory" || stat["size"] != nil {
			t.Fatal("directory stat", path, stat)
		}
	}
	for _, params := range []map[string]any{{"path": "dir", "sha256": true}, {"path": "missing"}, {"path": "../outside"}} {
		if _, err := fileOp(t, n, "files.stat", params); err == nil {
			t.Fatal("stat accepted", params)
		}
	}

	mustFileOp(t, n, "files.mkdir", map[string]any{"path": "made"})
	if info, err := os.Stat(filepath.Join(work, "made")); err != nil || !info.IsDir() {
		t.Fatal("mkdir did not create a directory", err)
	}
	for _, params := range []map[string]any{{"path": "made"}, {"path": "deep/er"}, {"path": "dir/file"}, {"path": "dir/file/child", "parents": true}, {"path": "."}, {"path": ""}, {"path": "../outside"}} {
		if _, err := fileOp(t, n, "files.mkdir", params); err == nil {
			t.Fatal("mkdir accepted", params)
		}
	}
	mustFileOp(t, n, "files.mkdir", map[string]any{"path": "deep/er", "parents": true})
	mustFileOp(t, n, "files.mkdir", map[string]any{"path": "deep/er", "parents": true})
	if info, err := os.Stat(filepath.Join(work, "deep", "er")); err != nil || !info.IsDir() {
		t.Fatal("mkdir parents did not create the tree", err)
	}

	for _, params := range []map[string]any{{"path": "dir"}, {"path": "deep", "recursive": true}, {"path": "."}, {"path": "deep/.."}, {"path": ""}, {"path": "missing"}, {"path": "../outside"}} {
		if _, err := fileOp(t, n, "files.remove", params); err == nil {
			t.Fatal("remove accepted", params)
		}
	}
	assertFileContent(t, filepath.Join(work, "dir", "file"), "contents")
	if result := mustFileOp(t, n, "files.remove", map[string]any{"path": "dir/file"}); result["type"] != "file" {
		t.Fatal("remove result", result)
	}
	assertMissing(t, filepath.Join(work, "dir", "file"))
	if result := mustFileOp(t, n, "files.remove", map[string]any{"path": "deep/er"}); result["type"] != "directory" {
		t.Fatal("remove result", result)
	}
	assertMissing(t, filepath.Join(work, "deep", "er"))
	if _, err := os.Stat(filepath.Join(work, "deep")); err != nil {
		t.Fatal("remove deleted the parent", err)
	}
}

func TestFileMoveAndCopyRefuseImplicitOverwrite(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	work := n.Config.WorkDir
	path := func(parts ...string) string { return filepath.Join(append([]string{work}, parts...)...) }
	writeTestFile(t, path("a"), "alpha")
	writeTestFile(t, path("b"), "bravo")
	writeTestFile(t, path("tree", "inner", "leaf"), "leaf")
	if err := os.MkdirAll(path("other", "existing"), 0700); err != nil {
		t.Fatal(err)
	}

	// No-overwrite refusals leave both sides unchanged.
	for _, method := range []string{"files.move", "files.copy"} {
		_, err := fileOp(t, n, method, map[string]any{"from": "a", "to": "b"})
		var rpc *model.RPCError
		if !errors.As(err, &rpc) || rpc.Code != "destination_exists" {
			t.Fatal(method, "replaced an existing destination", err)
		}
		assertFileContent(t, path("a"), "alpha")
		assertFileContent(t, path("b"), "bravo")
	}
	for _, params := range []map[string]any{
		{"from": "a", "to": "missing-parent/a"},
		{"from": "a", "to": "tree", "overwrite": true},
		{"from": "tree", "to": "a", "overwrite": true},
		{"from": "tree", "to": "other"},
		{"from": "missing", "to": "c"},
		{"from": "a", "to": "."},
		{"from": ".", "to": "c"},
		{"from": "", "to": "c"},
		{"from": "a", "to": ""},
	} {
		if _, err := fileOp(t, n, "files.move", params); err == nil {
			t.Fatal("move accepted", params)
		}
	}
	for _, params := range []map[string]any{
		{"from": "a", "to": "missing-parent/a"},
		{"from": "a", "to": "tree", "overwrite": true},
		{"from": "tree", "to": "c"},
		{"from": "missing", "to": "c"},
		{"from": "a", "to": "."},
	} {
		if _, err := fileOp(t, n, "files.copy", params); err == nil {
			t.Fatal("copy accepted", params)
		}
	}
	assertFileContent(t, path("a"), "alpha")
	assertMissing(t, path("missing-parent"))
	assertMissing(t, path("c"))
	assertNoCopyTemporaries(t, work)

	if result := mustFileOp(t, n, "files.copy", map[string]any{"from": "a", "to": "tree/a-copy"}); result["size"] != float64(len("alpha")) {
		t.Fatal("copy result", result)
	}
	assertFileContent(t, path("tree", "a-copy"), "alpha")
	assertFileContent(t, path("a"), "alpha")
	mustFileOp(t, n, "files.copy", map[string]any{"from": "b", "to": "tree/a-copy", "overwrite": true})
	assertFileContent(t, path("tree", "a-copy"), "bravo")
	assertNoCopyTemporaries(t, path("tree"))

	mustFileOp(t, n, "files.move", map[string]any{"from": "a", "to": "tree/inner/a"})
	assertMissing(t, path("a"))
	assertFileContent(t, path("tree", "inner", "a"), "alpha")
	mustFileOp(t, n, "files.move", map[string]any{"from": "b", "to": "tree/inner/a", "overwrite": true})
	assertMissing(t, path("b"))
	assertFileContent(t, path("tree", "inner", "a"), "bravo")

	if result := mustFileOp(t, n, "files.move", map[string]any{"from": "tree", "to": "other/moved"}); result["type"] != "directory" {
		t.Fatal("directory move result", result)
	}
	assertMissing(t, path("tree"))
	assertFileContent(t, path("other", "moved", "inner", "leaf"), "leaf")
	// Directories are never replaced, even with overwrite.
	if _, err := fileOp(t, n, "files.move", map[string]any{"from": "other/moved", "to": "other/existing", "overwrite": true}); err == nil {
		t.Fatal("directory move replaced an existing directory")
	}
	assertFileContent(t, path("other", "moved", "inner", "leaf"), "leaf")

	// The task workspace root cannot be moved, even when it exists as a plain directory.
	if err := os.MkdirAll(path("tasks", "example"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := fileOp(t, n, "files.move", map[string]any{"from": "tasks", "to": "tasks-moved"}); err == nil {
		t.Fatal("moved the task workspace root")
	}
}

func TestFileMutationsStayInsideRoot(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	work := n.Config.WorkDir
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "protected"), "unchanged")
	writeTestFile(t, filepath.Join(work, "inside"), "inside")
	escape, err := filepath.Rel(work, outside)
	if err != nil {
		t.Fatal(err)
	}
	escape = filepath.ToSlash(escape)
	cases := []struct {
		method string
		params map[string]any
	}{
		{"files.stat", map[string]any{"path": escape + "/protected"}},
		{"files.mkdir", map[string]any{"path": escape + "/created", "parents": true}},
		{"files.remove", map[string]any{"path": escape + "/protected"}},
		{"files.move", map[string]any{"from": escape + "/protected", "to": "stolen"}},
		{"files.move", map[string]any{"from": "inside", "to": escape + "/planted"}},
		{"files.copy", map[string]any{"from": escape + "/protected", "to": "stolen"}},
		{"files.copy", map[string]any{"from": "inside", "to": escape + "/planted"}},
		{"files.copy", map[string]any{"from": "inside", "to": escape + "/protected", "overwrite": true}},
	}
	check := func() {
		t.Helper()
		assertFileContent(t, filepath.Join(outside, "protected"), "unchanged")
		assertFileContent(t, filepath.Join(work, "inside"), "inside")
		for _, name := range []string{"created", "planted"} {
			assertMissing(t, filepath.Join(outside, name))
		}
		assertMissing(t, filepath.Join(work, "stolen"))
		assertNoCopyTemporaries(t, outside)
	}
	for _, c := range cases {
		if _, err := fileOp(t, n, c.method, c.params); err == nil {
			t.Fatal("escaped root with ..", c.method, c.params)
		}
		check()
	}

	symlinkOrSkip(t, outside, filepath.Join(work, "out-dir"))
	symlinkOrSkip(t, filepath.Join(outside, "protected"), filepath.Join(work, "out-file"))
	symlinkOrSkip(t, "inside", filepath.Join(work, "alias"))
	cases = []struct {
		method string
		params map[string]any
	}{
		{"files.stat", map[string]any{"path": "out-file", "sha256": true}},
		{"files.stat", map[string]any{"path": "out-dir/protected"}},
		{"files.mkdir", map[string]any{"path": "out-dir/created"}},
		{"files.remove", map[string]any{"path": "out-dir/protected"}},
		{"files.move", map[string]any{"from": "out-dir/protected", "to": "stolen"}},
		{"files.move", map[string]any{"from": "inside", "to": "out-dir/planted"}},
		{"files.copy", map[string]any{"from": "out-file", "to": "stolen"}},
		{"files.copy", map[string]any{"from": "inside", "to": "out-dir/planted"}},
		// Links themselves are neither removed, moved nor replaced.
		{"files.remove", map[string]any{"path": "out-file"}},
		{"files.remove", map[string]any{"path": "out-dir"}},
		{"files.remove", map[string]any{"path": "alias"}},
		{"files.move", map[string]any{"from": "out-dir", "to": "moved-link"}},
		{"files.move", map[string]any{"from": "alias", "to": "moved-link"}},
		{"files.move", map[string]any{"from": "inside", "to": "out-file", "overwrite": true}},
		{"files.copy", map[string]any{"from": "inside", "to": "out-file", "overwrite": true}},
	}
	for _, c := range cases {
		if _, err := fileOp(t, n, c.method, c.params); err == nil {
			t.Fatal("followed or changed a symbolic link", c.method, c.params)
		}
		check()
		for _, link := range []string{"out-dir", "out-file", "alias"} {
			if info, err := os.Lstat(filepath.Join(work, link)); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatal("symbolic link was changed", link, err)
			}
		}
		assertMissing(t, filepath.Join(work, "moved-link"))
	}
	// Root-confined links resolve for reads, as files.read does.
	if stat := mustFileOp(t, n, "files.stat", map[string]any{"path": "alias"}); stat["type"] != "file" {
		t.Fatal("root-confined alias", stat)
	}
	mustFileOp(t, n, "files.copy", map[string]any{"from": "alias", "to": "alias-copy"})
	assertFileContent(t, filepath.Join(work, "alias-copy"), "inside")
	// A directory move scans entries without following a link out of the root.
	if err := os.MkdirAll(filepath.Join(work, "holder"), 0700); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(work, "holder", "link"))
	mustFileOp(t, n, "files.move", map[string]any{"from": "holder", "to": "held"})
	check()
}

func TestFileMutationsKeepUploadStagingPrivate(t *testing.T) {
	t.Parallel()
	n := localNode(t, func(cfg *Config) { cfg.DataDir = filepath.Join(cfg.WorkDir, "state") })
	work := n.Config.WorkDir
	writeTestFile(t, filepath.Join(work, ".control-uploads", "partial"), "staged")
	writeTestFile(t, filepath.Join(work, "outer", "dest", ".control-uploads", "partial"), "staged")
	writeTestFile(t, filepath.Join(work, "file"), "file")
	if _, err := os.Stat(filepath.Join(work, "state", "uploads")); err != nil {
		t.Fatal("upload metadata directory missing", err)
	}
	cases := []struct {
		method string
		params map[string]any
	}{
		{"files.stat", map[string]any{"path": ".control-uploads/partial"}},
		{"files.stat", map[string]any{"path": "state/uploads"}},
		{"files.mkdir", map[string]any{"path": ".CONTROL-UPLOADS/x", "parents": true}},
		{"files.mkdir", map[string]any{"path": "fresh/.control-uploads"}},
		{"files.remove", map[string]any{"path": ".control-uploads/partial"}},
		{"files.remove", map[string]any{"path": "state/uploads"}},
		{"files.move", map[string]any{"from": ".control-uploads/partial", "to": "taken"}},
		{"files.move", map[string]any{"from": "file", "to": ".control-uploads/file"}},
		{"files.move", map[string]any{"from": "outer", "to": "moved"}},
		{"files.move", map[string]any{"from": "state", "to": "moved"}},
		{"files.copy", map[string]any{"from": ".control-uploads/partial", "to": "taken"}},
		{"files.copy", map[string]any{"from": "file", "to": "outer/dest/.control-uploads/file"}},
		{"files.write", map[string]any{"path": ".control-uploads/new", "data": "x", "encoding": "text"}},
	}
	for _, c := range cases {
		if _, err := fileOp(t, n, c.method, c.params); err == nil {
			t.Fatal("reached private upload state", c.method, c.params)
		}
	}
	assertFileContent(t, filepath.Join(work, ".control-uploads", "partial"), "staged")
	assertFileContent(t, filepath.Join(work, "outer", "dest", ".control-uploads", "partial"), "staged")
	assertFileContent(t, filepath.Join(work, "file"), "file")
	for _, name := range []string{"taken", "moved", "fresh", ".CONTROL-UPLOADS/x", ".control-uploads/x", ".control-uploads/file", "outer/dest/.control-uploads/file"} {
		assertMissing(t, filepath.Join(work, filepath.FromSlash(name)))
	}
	// The upload-protected listing stays hidden from ordinary listings.
	if list, err := n.filesList(t.Context(), model.JSON(map[string]string{"path": "."}), Execution{}); err != nil || strings.Contains(string(model.JSON(list)), ".control-uploads") {
		t.Fatal("listing exposed upload staging", list, err)
	}
}

func TestFileMutationsUseOperationAdmission(t *testing.T) {
	t.Parallel()
	n := localNode(t, func(cfg *Config) { cfg.MaxOperations = 1 })
	writeTestFile(t, filepath.Join(n.Config.WorkDir, "keep"), "keep")
	if !n.pauseForUpdate() {
		t.Fatal("idle node did not pause")
	}
	// Each request would succeed if admitted.
	for method, params := range map[string]map[string]any{
		"files.stat":   {"path": "keep"},
		"files.mkdir":  {"path": "made"},
		"files.remove": {"path": "keep"},
		"files.move":   {"from": "keep", "to": "other"},
		"files.copy":   {"from": "keep", "to": "other"},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		err := n.Call(ctx, "", method, params, nil)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("maintenance admitted", method, err)
		}
	}
	n.work.Resume()
	assertFileContent(t, filepath.Join(n.Config.WorkDir, "keep"), "keep")
	assertMissing(t, filepath.Join(n.Config.WorkDir, "other"))
	assertMissing(t, filepath.Join(n.Config.WorkDir, "made"))
	task, err := n.startTaskContext(t.Context(), n.Identity.ID, model.TaskSpec{ID: "copy-task", Capability: "files.copy", Args: model.JSON(map[string]any{"from": "keep", "to": "other"})})
	if err != nil {
		t.Fatal(err)
	}
	result, err := n.waitTask(t.Context(), task.Owner, task.ID, 5*time.Second)
	if err != nil || result.State != "succeeded" {
		t.Fatal("tracked copy", result.State, result.Error, err)
	}
	assertFileContent(t, filepath.Join(n.Config.WorkDir, "other"), "keep")
	assertPool(t, n.operationsPool.snapshot(), 1, 0, 0)
}
