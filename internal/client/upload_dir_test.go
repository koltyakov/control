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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/identity"
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

	// Cancellation stops the transfer; abort keeps published files. Cancel
	// only once the worker holds the other in-flight file, so every started
	// file has a worker record and abort can settle it deterministically.
	attempt, cancel := context.WithCancel(ctx)
	firstDone := ""
	result, err = c.UploadDirectory(attempt, DirUploadSpec{Node: "second", ID: "cancelled", LocalPath: local, Path: "dest-cancelled", FileDone: func(f DirUploadFile) {
		defer cancel()
		if firstDone != "" {
			return
		}
		firstDone = f.Path
		if f.State != "published" {
			t.Errorf("first file %s: %s", f.State, f.Error)
		}
		// The two workers start the first two files in sorted order.
		other := "big.bin"
		if f.Path == other {
			other = "a/b/c.txt"
		}
		wait, stop := context.WithTimeout(ctx, 30*time.Second)
		defer stop()
		for wait.Err() == nil {
			if _, err := c.UploadStatus(wait, "second", dirFileID("cancelled", other), false); err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Error("in-flight file never reached the worker")
	}})
	cancel()
	if err == nil {
		t.Fatal("cancelled directory upload completed", result)
	}
	if result.States["pending"] == 0 {
		t.Fatal("cancellation did not stop new files", result)
	}
	var manifestOf dirManifest
	// The receiver may briefly hold the interrupted file active.
	for {
		state, err = c.UploadState(ctx, "second", "cancelled", false)
		if err != nil {
			t.Fatal(err)
		}
		if state.(DirUploadResult).States["active"] == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Abort refuses while an attempt holds the transfer and records nothing.
	held, err := c.lockDirUpload(ctx, "cancelled")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = unroutedClient(t, ctx, c).UploadState(ctx, "second", "cancelled", true); !errors.As(err, &refused) || refused.Code != "upload_busy" {
		t.Fatal("abort ran beside an active attempt", err)
	}
	held.release(ctx)
	if err = store.Read(c.dirUploadPath("cancelled"), &manifestOf); err != nil || !manifestOf.Aborted.IsZero() {
		t.Fatal("refused abort was recorded", err)
	}
	state, err = unroutedClient(t, ctx, c).UploadState(ctx, "second", "cancelled", true)
	if err != nil {
		t.Fatal(err)
	}
	if aborted := state.(DirUploadResult); aborted.State != "aborted" || !aborted.AbortRequested || aborted.States["published"] < 1 {
		t.Fatal("abort", aborted)
	}
	if got, err := os.ReadFile(filepath.Join(worker.Config.WorkDir, "dest-cancelled", filepath.FromSlash(firstDone))); err != nil || string(got) != files[firstDone] {
		t.Fatal("abort removed a published file", firstDone, err)
	}
	// An aborted transfer cannot be resumed, even its pending files.
	if _, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "cancelled", LocalPath: local, Resume: true}); !errors.As(err, &refused) || refused.Code != "upload_aborted" {
		t.Fatal("aborted transfer resumed", err)
	}

	// An abort that cannot settle a file reports it, keeps the summary and
	// is not called aborted.
	first = true
	result, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "abort-partial", LocalPath: local, Path: "dest-abort", FileDone: func(DirUploadFile) {
		if !first {
			return
		}
		first = false
		for ctx.Err() == nil {
			if err := c.Call(ctx, "second", "leases.acquire", map[string]any{"ttlSeconds": 300}, &lease); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}})
	if err == nil || len(result.Failures) == 0 {
		t.Fatal("lease did not stop the transfer", result, err)
	}
	if err = c.Call(ctx, "second", "leases.release", map[string]any{"id": lease.ID}, nil); err != nil {
		t.Fatal(err)
	}
	// A file record this client cannot read leaves that file's state unknown.
	unreadable := c.uploadRecordPath(result.Failures[0].ID)
	saved, err := os.ReadFile(unreadable)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(unreadable, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	state, err = c.UploadState(ctx, "second", "abort-partial", true)
	partial, _ := state.(DirUploadResult)
	if !errors.As(err, &refused) || refused.Code != "upload_abort_incomplete" || partial.State == "aborted" || !partial.AbortRequested || partial.States["unknown"] == 0 || partial.NodeID == "" {
		t.Fatal("incomplete abort reported as aborted", partial, err)
	}
	// Status stays read-only and reports the same incomplete abort.
	before, err := os.ReadFile(c.dirUploadPath("abort-partial"))
	if err != nil {
		t.Fatal(err)
	}
	if state, err = c.UploadState(ctx, "second", "abort-partial", false); err != nil || state.(DirUploadResult).State == "aborted" {
		t.Fatal("status after incomplete abort", state, err)
	}
	if after, _ := os.ReadFile(c.dirUploadPath("abort-partial")); !bytes.Equal(before, after) {
		t.Fatal("status changed the recovery record")
	}
	if _, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "abort-partial", LocalPath: local, Resume: true}); !errors.As(err, &refused) || refused.Code != "upload_aborted" {
		t.Fatal("resumed after a recorded abort", err)
	}
	// MCP keeps the summary beside the error.
	tool, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "control_upload_abort", Arguments: map[string]any{"node": "second", "id": "abort-partial"}})
	if err != nil || !tool.IsError {
		t.Fatal("MCP incomplete abort", tool, err)
	}
	var mcpAbort struct {
		Code   string          `json:"code"`
		Result DirUploadResult `json:"result"`
	}
	if err = json.Unmarshal([]byte(tool.Content[0].(*mcp.TextContent).Text), &mcpAbort); err != nil || mcpAbort.Code != "upload_abort_incomplete" || mcpAbort.Result.Kind != "directory" || mcpAbort.Result.States["unknown"] == 0 {
		t.Fatal("MCP incomplete abort summary", mcpAbort, err)
	}
	if err = os.WriteFile(unreadable, saved, 0600); err != nil {
		t.Fatal(err)
	}
	// The worker never reserved the leased files and now has no record of
	// them. That does not prove they were never published, since receipts
	// expire, so they stay unknown and the abort stays incomplete.
	state, err = c.UploadState(ctx, "second", "abort-partial", true)
	partial, _ = state.(DirUploadResult)
	notFound := slices.ContainsFunc(partial.Failures, func(f DirUploadFile) bool { return f.Code == "upload_not_found" && f.State == "unknown" })
	if !errors.As(err, &refused) || refused.Code != "upload_abort_incomplete" || partial.State == "aborted" || !notFound {
		t.Fatal("worker without a record treated as settled", partial, err)
	}

	// A file that changes size after the scan is refused before anything
	// is sent, rather than publishing more bytes than the manifest declared.
	growing := t.TempDir()
	writeTree(t, growing, map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c", "d.txt": "d", "z.txt": "12345"})
	first = true
	result, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "growing", LocalPath: growing, Path: "dest-growing", FileDone: func(DirUploadFile) {
		if first {
			first = false
			if err := os.WriteFile(filepath.Join(growing, "z.txt"), []byte("12345 and more"), 0600); err != nil {
				t.Error(err)
			}
		}
	}})
	if !errors.As(err, &refused) || refused.Code != "source_changed" || result.State == "published" || result.BytesSent != 4 || len(result.Failures) != 1 || result.Failures[0].Path != "z.txt" {
		t.Fatal("grown file accepted", result, err)
	}
	if exists(filepath.Join(worker.Config.WorkDir, "dest-growing", "z.txt")) || exists(c.uploadRecordPath(dirFileID("growing", "z.txt"))) {
		t.Fatal("grown file sent or recorded")
	}
	// Content of the scanned size is bound only when the file first starts.
	if err = os.WriteFile(filepath.Join(growing, "z.txt"), []byte("abcde"), 0600); err != nil {
		t.Fatal(err)
	}
	if result, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "growing", LocalPath: growing, Resume: true}); err != nil || result.State != "published" || result.FilesAttempted != 1 {
		t.Fatal("resume after restoring the size", result, err)
	}
	if got, _ := os.ReadFile(filepath.Join(worker.Config.WorkDir, "dest-growing", "z.txt")); string(got) != "abcde" {
		t.Fatal("restored file content", string(got))
	}
	// A child record an earlier release left for a grown file, or a worker
	// receipt that differs from the local record, is a conflict: never
	// published, settled or aborted, and the destination is kept.
	editRecord := func(name string, edit func(*localUpload)) func() {
		file := c.uploadRecordPath(dirFileID("growing", name))
		saved, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var record localUpload
		if err = json.Unmarshal(saved, &record); err != nil {
			t.Fatal(err)
		}
		edit(&record)
		if err = store.Write(file, record); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := os.WriteFile(file, saved, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	restoreZ := editRecord("z.txt", func(r *localUpload) { r.Size = 14 })
	restoreA := editRecord("a.txt", func(r *localUpload) { r.Published, r.SHA256 = time.Time{}, strings.Repeat("0", 64) })
	conflicts := func(summary DirUploadResult) bool {
		found := map[string]bool{}
		for _, f := range summary.Failures {
			found[f.Path] = f.State == "conflict" && f.Code == "upload_spec_conflict"
		}
		return summary.States["conflict"] == 2 && found["z.txt"] && found["a.txt"] && summary.State == "incomplete"
	}
	if state, err = c.UploadState(ctx, "second", "growing", false); err != nil || !conflicts(state.(DirUploadResult)) {
		t.Fatal("conflicting records reported", state, err)
	}
	state, err = c.UploadState(ctx, "second", "growing", true)
	if !errors.As(err, &refused) || refused.Code != "upload_abort_incomplete" || !conflicts(state.(DirUploadResult)) {
		t.Fatal("abort over conflicting records", state, err)
	}
	for name, content := range map[string]string{"a.txt": "a", "z.txt": "abcde"} {
		if got, _ := os.ReadFile(filepath.Join(worker.Config.WorkDir, "dest-growing", name)); string(got) != content {
			t.Fatal("conflicting destination changed", name, string(got))
		}
	}
	if r, err := c.UploadStatus(ctx, "second", dirFileID("growing", "a.txt"), false); err != nil || r.State != "published" {
		t.Fatal("worker receipt changed by abort", r, err)
	}
	restoreZ()
	restoreA()

	// An empty tree is published only once its directories are confirmed.
	empty := t.TempDir()
	writeTree(t, empty, nil, "e/x", "e/y")
	if result, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "empty", LocalPath: empty, Path: "dest-empty"}); err != nil || result.State != "published" {
		t.Fatal("empty tree", result, err)
	}
	if err = store.Read(c.dirUploadPath("empty"), &manifestOf); err != nil || manifestOf.DirsCreated.IsZero() || manifestOf.Completed.IsZero() {
		t.Fatal("empty tree evidence", manifestOf, err)
	}
	// Simulate an attempt that stopped after creating the root, before its
	// leaf directories, as a record written before this field existed.
	manifestOf.DirsCreated, manifestOf.Completed = time.Time{}, time.Time{}
	if err = store.Write(c.dirUploadPath("empty"), manifestOf); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(worker.Config.WorkDir, "dest-empty", "e", "y")); err != nil {
		t.Fatal(err)
	}
	if state, err = c.UploadState(ctx, "second", "empty", false); err != nil || state.(DirUploadResult).State != "incomplete" {
		t.Fatal("unconfirmed empty tree reported", state, err)
	}
	if result, err = c.UploadDirectory(ctx, DirUploadSpec{Node: "second", ID: "empty", LocalPath: empty, Resume: true}); err != nil || result.State != "published" {
		t.Fatal("empty tree resume", result, err)
	}
	if info, err := os.Stat(filepath.Join(worker.Config.WorkDir, "dest-empty", "e", "y")); err != nil || !info.IsDir() {
		t.Fatal("resume did not recreate the directory", err)
	}
	if state, err = c.UploadState(ctx, "second", "empty", false); err != nil || state.(DirUploadResult).State != "published" {
		t.Fatal("confirmed empty tree", state, err)
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
	scope := t.TempDir()
	dir, locks := filepath.Join(scope, "upload-dirs"), filepath.Join(scope, "upload-locks")
	now := time.Now()
	expired := now.Add(-8 * 24 * time.Hour)
	write := func(id string, completed, aborted time.Time) string {
		file := filepath.Join(dir, identity.ID([]byte(id))+".json")
		if err := store.Write(file, dirManifest{Format: dirManifestFormat, ID: id, NodeID: "n", Completed: completed, Aborted: aborted}); err != nil {
			t.Fatal(err)
		}
		return file
	}
	// An older release's attempt lock is never removed, but a free one
	// does not keep an expired record.
	old := write("old", expired, time.Time{})
	staleFile(t, old+".lock", expired)
	recent := write("recent", now.Add(-time.Hour), time.Time{})
	pending := write("pending", time.Time{}, time.Time{})
	aborted := write("aborted", time.Time{}, expired)
	// An expired transfer that an older release is resuming keeps its
	// record and lock.
	legacyActive := write("legacy-active", expired, time.Time{})
	holdLock(t, legacyActive+".lock")
	// So does one with an active attempt of this release.
	active := write("active", expired, time.Time{})
	holdLock(t, dirAttemptPath(scope, identity.ID([]byte("active"))))
	orphan := filepath.Join(dir, identity.ID([]byte("refused"))+".json.lock")
	staleFile(t, orphan, expired)
	pruneDirManifests(t.Context(), scope, now)
	for file, kept := range map[string]bool{old: false, old + ".lock": true, recent: true, pending: true, aborted: true, legacyActive: true, legacyActive + ".lock": true, active: true, orphan: true} {
		if exists(file) != kept {
			t.Fatal("unexpected manifest retention", filepath.Base(file), kept)
		}
	}
	if exists(dirAttemptPath(scope, identity.ID([]byte("old")))) {
		t.Fatal("pruning left an attempt lock file")
	}
	// A busy stripe defers pruning rather than deciding without it.
	again := write("again", expired, time.Time{})
	stripe, _ := uploadStripePath(locks, identity.ID([]byte("again")))
	held := holdLock(t, stripe)
	pruneDirManifests(t.Context(), scope, now)
	if !exists(again) {
		t.Fatal("record pruned without its stripe")
	}
	_ = held.Unlock()
	pruneDirManifests(t.Context(), scope, now)
	if exists(again) || !exists(stripe) {
		t.Fatal("stripe released: record not pruned, or stripe removed")
	}
}

// Attempt locks are short-lived files reserved under the ID's stripe: a held
// one survives pruning, release removes it, a crashed attempt's file is
// pruned, and an older release's lock file is respected but never created.
func TestDirUploadLockIdentity(t *testing.T) {
	c := (Client{}).WithStandalone(t.Context(), StandaloneConfig{StateDir: t.TempDir(), Gateway: Admin{URL: "https://gateway.example"}, AccountRouting: true})
	defer func() { _ = c.Close() }()
	ctx := t.Context()
	scope := c.uploadScopeDir()
	// Pruning finds the same stripe from the hashed record name alone.
	if listed, ok := uploadStripePath(c.uploadLockDir(), filepath.Base(strings.TrimSuffix(c.dirUploadPath("tree"), ".json"))); !ok || listed != c.uploadStripePath("tree") {
		t.Fatal("stripe not derived from the hashed record name", listed)
	}
	attempt, err := c.lockDirUpload(ctx, "tree")
	if err != nil {
		t.Fatal(err)
	}
	attemptPath := c.dirAttemptPath("tree")
	before, err := os.Stat(attemptPath)
	if err != nil {
		t.Fatal(err)
	}
	if exists(c.dirUploadPath("tree") + ".lock") {
		t.Fatal("older release's lock path created")
	}
	if _, err = c.lockDirUpload(ctx, "tree"); !errors.Is(err, errDirUploadBusy) {
		t.Fatal("second attempt lock acquired", err)
	}
	// A single-file upload cannot claim the ID while the directory holds it,
	// so a directory start cannot be left with a root but no record.
	if active, err := c.dirAttemptActive("tree"); err != nil || !active {
		t.Fatal("single-file check missed the directory attempt", active, err)
	}
	pruneDirAttempts(ctx, scope)
	if after, err := os.Stat(attemptPath); err != nil || !os.SameFile(before, after) {
		t.Fatal("held attempt lock unlinked or replaced", err)
	}
	attempt.release(ctx)
	if exists(attemptPath) {
		t.Fatal("released attempt left its lock file")
	}
	if active, err := c.dirAttemptActive("tree"); err != nil || active {
		t.Fatal("released attempt still reported", active, err)
	}
	// A crashed attempt's file is pruned under the stripe.
	staleFile(t, c.dirAttemptPath("crashed"), time.Now())
	pruneDirAttempts(ctx, scope)
	if exists(c.dirAttemptPath("crashed")) {
		t.Fatal("crashed attempt's lock file kept")
	}
	// An older release's attempt lock is honored while held and kept.
	legacy := c.dirUploadPath("old-release") + ".lock"
	staleFile(t, legacy, time.Now())
	held := holdLock(t, legacy)
	if _, err = c.lockDirUpload(ctx, "old-release"); !errors.Is(err, errDirUploadBusy) {
		t.Fatal("older release's attempt ignored", err)
	}
	if active, err := c.dirAttemptActive("old-release"); err != nil || !active {
		t.Fatal("single-file check missed the older release's attempt", active, err)
	}
	_ = held.Unlock()
	if attempt, err = c.lockDirUpload(ctx, "old-release"); err != nil {
		t.Fatal(err)
	}
	attempt.release(ctx)
	if !exists(legacy) || exists(c.dirAttemptPath("old-release")) {
		t.Fatal("older release's lock removed, or attempt lock left")
	}
	// A directory start refuses an ID a single-file upload holds.
	if err = store.Write(c.uploadRecordPath("claimed"), localUpload{Spec: upload.Spec{ID: "claimed"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.lockDirUpload(ctx, "claimed"); err == nil || exists(c.dirAttemptPath("claimed")) {
		t.Fatal("directory attempt reserved a single-file upload ID", err)
	}
}

func TestDirSummaryState(t *testing.T) {
	now := time.Now()
	files := []dirFile{{Path: "a"}, {Path: "b"}}
	for _, tc := range []struct {
		name   string
		m      dirManifest
		states map[string]int
		live   int
		want   string
	}{
		// Before an attempt confirms the directories, an empty tree is not
		// published, as when the leaf mkdir failed or was never reached.
		{"empty tree before directories", dirManifest{}, map[string]int{}, 0, "incomplete"},
		{"empty tree after directories", dirManifest{DirsCreated: now}, map[string]int{}, 0, "published"},
		{"empty tree completed", dirManifest{Completed: now}, map[string]int{}, 0, "published"},
		{"empty tree aborted", dirManifest{Aborted: now}, map[string]int{}, 0, "aborted"},
		// Records that predate dirsCreated: a published file proves them.
		{"files published", dirManifest{Files: files}, map[string]int{"published": 2}, 0, "published"},
		{"partly published", dirManifest{Files: files, DirsCreated: now}, map[string]int{"published": 1, "staging": 1}, 1, "incomplete"},
		{"rest unknown", dirManifest{Files: files, DirsCreated: now}, map[string]int{"published": 1, "unknown": 1}, 1, "unknown"},
		{"aborted settled", dirManifest{Files: files, Aborted: now}, map[string]int{"aborted": 1, "pending": 1}, 0, "aborted"},
		{"abort with a live file", dirManifest{Files: files, Aborted: now}, map[string]int{"aborted": 1, "staging": 1}, 1, "incomplete"},
		{"abort with an unknown file", dirManifest{Files: files, Aborted: now}, map[string]int{"published": 1, "unknown": 1}, 1, "unknown"},
		// No unknown or conflicting file ever yields aborted, even if a
		// caller miscounted it as settled.
		{"unknown never aborted", dirManifest{Files: files, Aborted: now}, map[string]int{"aborted": 1, "unknown": 1}, 0, "incomplete"},
		{"conflict never aborted", dirManifest{Files: files, Aborted: now}, map[string]int{"aborted": 1, "conflict": 1}, 0, "incomplete"},
	} {
		if got := dirSummaryState(tc.m, tc.states, tc.live); got != tc.want {
			t.Errorf("%s: state %s, want %s", tc.name, got, tc.want)
		}
	}
}

// Without a kept record, only a transfer whose records cannot have expired
// proves a file never started.
func TestDirChildRecordsKept(t *testing.T) {
	now := time.Now()
	if !(dirManifest{ChildRecords: true, Created: now.Add(-30 * 24 * time.Hour)}).childRecordsKept(now) || !(dirManifest{Created: now.Add(-time.Hour)}).childRecordsKept(now) || (dirManifest{Created: now.Add(-8 * 24 * time.Hour)}).childRecordsKept(now) {
		t.Fatal("child record retention evidence")
	}
	m := dirManifest{ID: "tree", Path: "dest"}
	f := dirFile{Path: "a/z.txt", Size: 5}
	good := localUpload{Spec: upload.Spec{ID: dirFileID("tree", "a/z.txt"), Path: "dest/a/z.txt", Size: 5}, NodeID: "n"}
	if err := childRecordConflict(good, m, f, "n"); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]func(*localUpload){
		"grown":       func(r *localUpload) { r.Size = 14 },
		"destination": func(r *localUpload) { r.Path = "dest/other" },
		"worker":      func(r *localUpload) { r.NodeID = "other" },
		"id":          func(r *localUpload) { r.ID = "other" },
		"tree":        func(r *localUpload) { r.Tree = "other" },
	} {
		r := good
		bad(&r)
		if childRecordConflict(r, m, f, "n") == nil {
			t.Error("conflicting child record accepted:", name)
		}
	}
}
