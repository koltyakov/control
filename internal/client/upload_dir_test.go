package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/transport/transporttest"
	"github.com/koltyakov/control/internal/upload"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func writeTree(t *testing.T, root string, files map[string]string, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func isDirectoryUpload(t *testing.T, c Client, id string) bool {
	t.Helper()
	directory, err := c.IsDirectoryUpload(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

// unroutedClient shares c's configuration and local recovery state but has
// not started routing, as each CLI invocation and new MCP process begins.
func unroutedClient(t *testing.T, ctx context.Context, c Client) Client {
	t.Helper()
	fresh := (Client{URL: c.URL, Token: c.Token}).WithStandalone(ctx, c.routing.cfg)
	t.Cleanup(func() { _ = fresh.Close() })
	return fresh
}

// notStarted checks a new transfer refused before its remote root existed:
// it says nothing was created, without the lost-response warning.
func notStarted(t *testing.T, result DirUploadResult, err error) {
	t.Helper()
	if result.State != "not_started" || result.FilesAttempted != 0 || result.BytesSent != 0 {
		t.Fatal("refused transfer state", result)
	}
	if message := err.Error(); strings.Contains(message, "response was lost") || strings.Contains(message, "may now exist") {
		t.Fatal("definitive refusal reported as uncertain", message)
	}
}

func TestDirFileIDIsDeterministicAndValid(t *testing.T) {
	a := dirFileID("tree", "a/b.txt")
	if a != dirFileID("tree", "a/b.txt") || !upload.ValidID(a) {
		t.Fatal("unstable or invalid file upload ID", a)
	}
	for _, other := range []string{dirFileID("tree", "a/B.txt"), dirFileID("tree2", "a/b.txt"), dirFileID("tree", "a/b.txt\x00")} {
		if other == a {
			t.Fatal("distinct files share an upload ID")
		}
	}
	c := (Client{}).WithStandalone(t.Context(), StandaloneConfig{StateDir: t.TempDir(), Gateway: Admin{URL: "https://gateway.example"}, AccountRouting: true})
	defer func() { _ = c.Close() }()
	if c.dirUploadPath("CON") == c.uploadRecordPath("CON") || filepath.Base(filepath.Dir(c.dirUploadPath("CON"))) != "upload-dirs" {
		t.Fatal("directory manifests share single-file recovery records")
	}
}

// Only a lost or unreadable root mkdir response leaves the root's existence
// unknown; the worker's own refusal or an unsent request created nothing.
func TestRootNotCreatedClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"worker refusal", model.Response{Error: "mkdirat dest: file exists", Code: "operation_failed", Outcome: "unknown"}.Err(), true},
		{"wrapped worker refusal", fmt.Errorf("call: %w", model.Response{Error: "mkdirat dest: no such file or directory"}.Err()), true},
		{"not sent", model.OperationError(errors.New("open lane"), "not_sent"), true},
		{"refused before acceptance", &model.RPCError{Code: "node_leased", Outcome: model.OutcomeNotAccepted, Message: "leased"}, true},
		{"lost reply", model.OperationError(io.EOF, "unknown"), false},
		{"deadline", model.OperationError(context.DeadlineExceeded, "unknown"), false},
		{"plain error", errors.New("something failed"), false},
	} {
		if got := rootNotCreated(tc.err); got != tc.want {
			t.Errorf("%s: rootNotCreated = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestScanLocalTree(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a/b/c.txt": "c", "top.txt": "top", "z.bin": ""}, "a/empty", "x/y")
	tree, err := scanLocalTree(ctx, root, false, dirLimits{files: 10, dirs: 10})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tree.Dirs, ",") != "a,a/b,a/empty,x,x/y" || len(tree.Files) != 3 || tree.Files[0] != (dirFile{Path: "a/b/c.txt", Size: 1}) || tree.Bytes != 4 {
		t.Fatal("unexpected tree", tree)
	}
	if leaves := leafDirs(tree.Dirs); strings.Join(leaves, ",") != "a/b,a/empty,x/y" {
		t.Fatal("leaf directories", leaves)
	}
	if _, err = scanLocalTree(ctx, root, false, dirLimits{files: 2, dirs: 10}); err == nil || !strings.Contains(err.Error(), "exceeds 2 files") {
		t.Fatal("file limit", err)
	}
	if _, err = scanLocalTree(ctx, root, false, dirLimits{files: 10, dirs: 4}); err == nil || !strings.Contains(err.Error(), "exceeds 4 directories") {
		t.Fatal("directory limit", err)
	}
	if _, err = scanLocalTree(ctx, filepath.Join(root, "top.txt"), false, dirLimits{files: 10, dirs: 10}); err == nil {
		t.Fatal("regular file accepted as a tree root")
	}
	// Symbolic links are refused, or skipped and listed, never followed.
	if err = os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")); err != nil {
		t.Logf("symbolic links unavailable: %v", err)
	} else {
		if _, err = scanLocalTree(ctx, root, false, dirLimits{files: 10, dirs: 10}); err == nil || !strings.Contains(err.Error(), "link (symbolic link)") || !strings.Contains(err.Error(), "--skip-unsupported") {
			t.Fatal("symlink not refused", err)
		}
		tree, err = scanLocalTree(ctx, root, true, dirLimits{files: 10, dirs: 10})
		if err != nil || strings.Join(tree.Skipped, ",") != "link" || len(tree.Files) != 3 {
			t.Fatal("symlink not skipped", tree, err)
		}
		if _, err = scanLocalTree(ctx, filepath.Join(root, "link"), true, dirLimits{files: 10, dirs: 10}); err == nil {
			t.Fatal("symbolic link accepted as a tree root")
		}
	}
	// Windows cannot create these names, which is why they are refused.
	if runtime.GOOS != "windows" {
		bad := t.TempDir()
		writeTree(t, bad, map[string]string{"ok.txt": "ok", "aux.txt": "", "dir./f": "", "sub/trailing ": "", "sub/a:b": "", ".control-uploads/x": ""})
		_, err = scanLocalTree(ctx, bad, false, dirLimits{files: 10, dirs: 10})
		for _, want := range []string{"5 unsupported", "aux.txt", "dir.", "sub/trailing ", "sub/a:b", ".control-uploads"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("unportable name %q not refused: %v", want, err)
			}
		}
		tree, err = scanLocalTree(ctx, bad, true, dirLimits{files: 10, dirs: 10})
		if err != nil || len(tree.Files) != 1 || len(tree.Skipped) != 5 || strings.Join(tree.Dirs, ",") != "sub" {
			t.Fatal("unportable names not skipped", tree, err)
		}
	}
}

func TestCheckTreeNamesRefusesCaseCollisions(t *testing.T) {
	ok := localTree{Dirs: []string{"a", "b"}, Files: []dirFile{{Path: "a/x"}, {Path: "b/x"}}}
	if err := checkTreeNames(ok); err != nil {
		t.Fatal(err)
	}
	for _, tree := range []localTree{
		{Files: []dirFile{{Path: "README"}, {Path: "readme"}}},
		{Dirs: []string{"Src"}, Files: []dirFile{{Path: "src"}}},
		{Dirs: []string{"a", "a/Éte", "a/éTE"}},
		{Files: []dirFile{{Path: "k"}, {Path: "K"}}}, // Kelvin sign folds to k.
	} {
		if err := checkTreeNames(tree); err == nil || !strings.Contains(err.Error(), "differ only by case") {
			t.Fatal("case collision accepted", tree, err)
		}
	}
}

func TestCheckRemoteTreeBudgets(t *testing.T) {
	var d uploadDescription
	d.FileUpload.MaxOwnerBytes, d.FileUpload.MaxStagingBytes = 10, 15
	tree := localTree{Dirs: []string{"d"}, Files: []dirFile{{Path: "d/a", Size: 8}, {Path: "b", Size: 6}}, Bytes: 14}
	if err := checkRemoteTree(tree, "dest", d); err != nil {
		t.Fatal(err)
	}
	for name, remote := range map[string]string{"absolute": "/dest", "dot": ".", "parent": "../dest", "reserved": "nul", "staging": "x/.control-uploads"} {
		if err := checkRemoteTree(tree, remote, d); err == nil {
			t.Fatal("invalid remote root accepted", name)
		}
	}
	tree.Files[0].Size, tree.Bytes = 11, 17
	if err := checkRemoteTree(tree, "dest", d); err == nil || !strings.Contains(err.Error(), "per-owner") {
		t.Fatal("file over owner budget", err)
	}
	tree.Files[0].Size, tree.Bytes = 10, 16
	if err := checkRemoteTree(tree, "dest", d); err == nil || !strings.Contains(err.Error(), "staging budget") {
		t.Fatal("tree over staging budget", err)
	}
	long := localTree{Files: []dirFile{{Path: strings.Repeat("a/", 2100) + "file"}}}
	if err := checkRemoteTree(long, "dest", d); err == nil {
		t.Fatal("over-long destination accepted")
	}
}

func TestUploadDirectoryResumeLeaseAndConflicts(t *testing.T) {
	ctx, c, workers, _ := delegationClusterWithTransport(t, false, 180*time.Second, transporttest.Loopback(), true)
	worker := workers[1]
	local := t.TempDir()
	files := map[string]string{"a/b/c.txt": "nested", "big.bin": strings.Repeat("payload\x00", 256<<10), "z/zero.bin": ""}
	for _, name := range []string{"f01", "f02", "f03", "f04", "f05", "f06", "f07", "f08"} {
		files["many/"+name+".txt"] = name
	}
	writeTree(t, local, files, "a/empty", "deep/x/y")
	remoteFile := func(name string) string {
		return filepath.Join(worker.Config.WorkDir, "dest", filepath.FromSlash(name))
	}

	// A directory needs the explicit CLI path; MCP and single-file calls refuse it.
	if _, err := c.Upload(ctx, UploadSpec{Node: "second", ID: "as-file", Path: "dest", LocalPath: local}); err == nil || !strings.Contains(err.Error(), "--recursive") {
		t.Fatal("single-file upload accepted a directory", err)
	}
	// The remote parent must exist; nothing is recorded when the root fails.
	if refused, err := c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "noparent", LocalPath: local, Path: "missing/dest"}); err == nil || isDirectoryUpload(t, c, "noparent") {
		t.Fatal("directory created under a missing parent", err)
	} else {
		notStarted(t, refused, err)
	}
	// A scan refusal also happens before anything exists remotely.
	unportable := t.TempDir()
	writeTree(t, unportable, map[string]string{"ok.txt": "ok", "aux.txt": "device"})
	if refused, err := c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "unportable", LocalPath: unportable, Path: "dest-unportable"}); err == nil || isDirectoryUpload(t, c, "unportable") {
		t.Fatal("unportable tree accepted", err)
	} else {
		notStarted(t, refused, err)
		if _, statErr := os.Lstat(filepath.Join(worker.Config.WorkDir, "dest-unportable")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("scan refusal created the remote root", statErr)
		}
	}

	// Interrupt after the first published file, then take a lease while the
	// other worker goroutine is held in its serialized report.
	var lease model.Lease
	first := true
	result, err := c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "tree", LocalPath: local, Path: "dest", FileDone: func(f DirUploadFile) {
		if !first {
			return
		}
		first = false
		if f.State != "published" {
			t.Errorf("first file %s: %s", f.State, f.Error)
		}
		for ctx.Err() == nil {
			if err := c.Call(ctx, "second", "leases.acquire", map[string]any{"ttlSeconds": 300}, &lease); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}})
	var refused *model.RPCError
	if err == nil || result.State != "incomplete" || lease.ID == "" || !errors.As(err, &refused) || refused.Code != "node_leased" {
		t.Fatal("lease did not stop the directory upload", result, err)
	}
	if result.Kind != "directory" || result.Files != 11 || result.Directories != 8 || result.States["published"] < 1 || len(result.Failures) == 0 || result.Failures[0].Code != "node_leased" {
		t.Fatal("unexpected partial result", result)
	}
	publishedBefore := result.States["published"]
	if info, err := os.Stat(remoteFile("a/empty")); err != nil || !info.IsDir() {
		t.Fatal("empty directory not created", err)
	}
	// While leased, resume fails cleanly before any file is sent.
	if result, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "tree", LocalPath: local, Resume: true}); err == nil || result.FilesAttempted != 0 {
		t.Fatal("resume ignored the lease", result, err)
	}
	if err = c.Call(ctx, "second", "leases.release", map[string]any{"id": lease.ID}, nil); err != nil {
		t.Fatal(err)
	}

	// Status is compact and accounts for every file.
	state, err := c.UploadState(ctx, "second", "tree", false)
	if err != nil {
		t.Fatal(err)
	}
	status := state.(DirUploadResult)
	total := 0
	for _, n := range status.States {
		total += n
	}
	if total != 11 || status.States["published"] != publishedBefore || status.Path != "dest" || len(status.Failures) > dirReportLimit {
		t.Fatal("status", status)
	}

	// A file that appears at a pending destination is never replaced.
	if err = os.WriteFile(remoteFile("z/zero.bin"), []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "tree", LocalPath: local, Resume: true})
	if err == nil || !strings.Contains(err.Error(), "z/zero.bin") || result.States["published"] != 10 || result.FilesAttempted != 11-publishedBefore {
		t.Fatal("conflict", result, err)
	}
	if got, _ := os.ReadFile(remoteFile("z/zero.bin")); string(got) != "occupied" {
		t.Fatal("existing destination overwritten", string(got))
	}
	if err = os.Remove(remoteFile("z/zero.bin")); err != nil {
		t.Fatal(err)
	}
	// Changing the local tree's names or sizes blocks resume.
	if err = os.WriteFile(filepath.Join(local, "extra.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "tree", LocalPath: local, Resume: true}); !errors.As(err, &refused) || refused.Code != "source_changed" {
		t.Fatal("changed tree resumed", err)
	}
	if err = os.Remove(filepath.Join(local, "extra.txt")); err != nil {
		t.Fatal(err)
	}
	// Only the remaining file is sent.
	result, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "tree", LocalPath: local, Resume: true})
	if err != nil || result.State != "published" || result.FilesAttempted != 1 || result.States["published"] != 11 {
		t.Fatal("final resume", result, err)
	}
	for name, content := range files {
		if got, err := os.ReadFile(remoteFile(name)); err != nil || !bytes.Equal(got, []byte(content)) {
			t.Fatal("uploaded content", name, err)
		}
	}
	for _, dir := range []string{"a/empty", "deep/x/y"} {
		if info, err := os.Stat(remoteFile(dir)); err != nil || !info.IsDir() {
			t.Fatal("directory", dir, err)
		}
	}
	if result, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "tree", LocalPath: local, Resume: true}); err != nil || result.State != "published" || result.FilesAttempted != 0 {
		t.Fatal("completed resume re-sent files", result, err)
	}
	// The published tree holds no private staging, including where a
	// collision earlier retained a partial, so it exports as a directory and
	// its directories can be removed through the files API.
	if err = filepath.WalkDir(filepath.Join(worker.Config.WorkDir, "dest"), func(name string, d fs.DirEntry, err error) error {
		if err == nil && strings.EqualFold(d.Name(), upload.StagingDir) {
			err = errors.New("staging left at " + name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var exported model.Artifact
	if err = c.Call(ctx, "second", "artifacts.export", map[string]any{"path": "dest"}, &exported); err != nil || exported.Name != "dest.tar" {
		t.Fatal("published tree export", exported, err)
	}
	for _, name := range []string{"a/b/c.txt", "a/b"} {
		if err = c.Call(ctx, "second", "files.remove", map[string]any{"path": "dest/" + name}, nil); err != nil {
			t.Fatal("remove published entry", name, err)
		}
	}
	// A fresh transfer never merges into an existing root, and IDs stay
	// separate between single-file and directory uploads.
	if refused, err := c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "again", LocalPath: local, Path: "dest"}); err == nil || isDirectoryUpload(t, c, "again") {
		t.Fatal("directory upload merged into an existing root", err)
	} else {
		notStarted(t, refused, err)
	}
	if _, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "tree", LocalPath: local, Path: "dest2"}); err == nil {
		t.Fatal("started a second transfer with an existing ID")
	}
	if _, err = c.Upload(ctx, UploadSpec{Node: "second", ID: "tree", Path: "single.txt", LocalPath: filepath.Join(local, "a", "b", "c.txt")}); err == nil || !strings.Contains(err.Error(), "directory upload") {
		t.Fatal("single-file upload reused a directory ID", err)
	}

	// Status finds the transfer from a client that has not connected yet;
	// the recovery record's owner scope must not be computed before routing.
	state, err = unroutedClient(t, ctx, c).UploadState(ctx, "second", "tree", false)
	if err != nil {
		t.Fatal("status from a new client", err)
	}
	if summary, ok := state.(DirUploadResult); !ok || summary.Kind != "directory" || summary.State != "published" || summary.States["published"] != 11 {
		t.Fatal("status from a new client", state)
	}

	// MCP status reports the directory summary through the unchanged tool,
	// also as the first call of a new server process.
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := unroutedClient(t, ctx, c).MCPServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "dir-upload-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	tool, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_upload_status", Arguments: map[string]any{"node": "second", "id": "tree"}})
	if err != nil || tool.IsError {
		t.Fatal(tool, err)
	}
	var summary DirUploadResult
	if err = json.Unmarshal([]byte(tool.Content[0].(*mcp.TextContent).Text), &summary); err != nil || summary.Kind != "directory" || summary.State != "published" {
		t.Fatal("MCP directory status", summary, err)
	}
	tool, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "control_upload", Arguments: map[string]any{"node": "second", "id": "mcp-dir", "localPath": local, "path": "mcp-dir"}})
	if err != nil || !tool.IsError {
		t.Fatal("MCP uploaded a directory", tool, err)
	}

	// Cancellation stops the transfer; abort keeps published files.
	attempt, cancel := context.WithCancel(ctx)
	result, err = c.UploadDirectory(attempt, DirUploadSpec{Node: "second", ID: "cancelled", LocalPath: local, Path: "dest-cancelled", FileDone: func(DirUploadFile) { cancel() }})
	cancel()
	if err == nil {
		t.Fatal("cancelled directory upload completed", result)
	}
	if result.States["pending"] == 0 {
		t.Fatal("cancellation did not stop new files", result)
	}
	state, err = unroutedClient(t, ctx, c).UploadState(ctx, "second", "cancelled", true)
	if err != nil {
		t.Fatal(err)
	}
	if aborted := state.(DirUploadResult); aborted.State != "aborted" || aborted.States["published"] < 1 {
		t.Fatal("abort", aborted)
	}
	if got, err := os.ReadFile(filepath.Join(worker.Config.WorkDir, "dest-cancelled", "a", "b", "c.txt")); err != nil || string(got) != "nested" {
		t.Fatal("abort removed a published file", err)
	}
	var grants []model.Delegation
	if err = c.Call(ctx, "second", "access.list", map[string]any{}, &grants); err != nil || len(grants) != 0 {
		t.Fatal("temporary upload grants retained", grants, err)
	}
	var manifest dirManifest
	if err = store.Read(c.dirUploadPath("tree"), &manifest); err != nil || manifest.Completed.IsZero() || manifest.LocalPath == "" {
		t.Fatal("manifest", manifest, err)
	}
}

func TestDirManifestsArePrunedOnlyAfterCompletion(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, completed time.Time) string {
		file := filepath.Join(dir, name)
		if err := store.Write(file, dirManifest{Format: dirManifestFormat, ID: name, Completed: completed}); err != nil {
			t.Fatal(err)
		}
		return file
	}
	old := write("old.json", now.Add(-8*24*time.Hour))
	recent := write("recent.json", now.Add(-time.Hour))
	pending := write("pending.json", time.Time{})
	pruneDirManifests(t.Context(), dir, now)
	for file, kept := range map[string]bool{old: false, recent: true, pending: true} {
		if _, err := os.Stat(file); (err == nil) != kept {
			t.Fatal("unexpected manifest retention", filepath.Base(file), err)
		}
	}
}
