package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/transport"
	"github.com/koltyakov/control/internal/upload"
)

// Directory uploads (D187) orchestrate the existing single-file upload
// protocol: one files.mkdir per leaf directory, then one create-only,
// verified, resumable upload per file. No new peer stream type exists.
const (
	// DirUploadMaxFiles keeps one tree to half of a worker's 1,024 retained
	// upload records per owner; published receipts stay for the retention
	// period and count toward that budget.
	DirUploadMaxFiles = 512
	// DirUploadMaxDirs bounds files.mkdir calls and the local scan.
	DirUploadMaxDirs = 512
	// dirUploadMaxProblems bounds the unsupported entries a scan tolerates
	// when skipping, and those it collects before refusing.
	dirUploadMaxProblems = 1000
	// dirUploadWorkers matches the worker's two active uploads per owner.
	dirUploadWorkers = 2
	// dirStatusWorkers bounds concurrent status or abort requests.
	dirStatusWorkers = 4
	// dirReportLimit bounds paths listed in results and errors.
	dirReportLimit    = 10
	dirManifestFormat = 1
)

// DirUploadSpec describes one directory transfer. LocalPath is a directory
// on the machine running the client; Path is the remote root, which must not
// exist when the transfer starts.
type DirUploadSpec struct {
	Node            string
	ID              string
	LocalPath       string
	Path            string
	TimeoutSeconds  int
	SkipUnsupported bool
	Resume          bool
	// FileDone, when set, is called once per attempted file, serially.
	FileDone func(DirUploadFile)
}

// DirUploadFile reports one file's attempt outcome.
type DirUploadFile struct {
	Path      string `json:"path"`
	ID        string `json:"id"`
	State     string `json:"state"`
	BytesSent int64  `json:"bytesSent,omitempty"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
}

// DirUploadResult summarizes a directory transfer compactly: counts by file
// state and at most a few listed failures, never one line per file.
type DirUploadResult struct {
	Kind        string         `json:"kind"`
	ID          string         `json:"id"`
	NodeID      string         `json:"nodeId"`
	Path        string         `json:"path"`
	State       string         `json:"state"`
	Directories int            `json:"directories"`
	Files       int            `json:"files"`
	Bytes       int64          `json:"bytes"`
	States      map[string]int `json:"states"`
	// FilesAttempted counts files for which this call opened an upload;
	// files this client already saw published are not contacted again.
	FilesAttempted int   `json:"filesAttempted,omitempty"`
	BytesSent      int64 `json:"bytesSent,omitempty"`
	// AbortRequested reports a recorded abort. The transfer can no longer
	// be resumed; state becomes aborted once no file can still hold or
	// publish staged bytes.
	AbortRequested bool            `json:"abortRequested,omitempty"`
	Skipped        int             `json:"skipped,omitempty"`
	SkippedPaths   []string        `json:"skippedPaths,omitempty"`
	Failures       []DirUploadFile `json:"failures,omitempty"`
}

type dirFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// dirManifest is the private local recovery record of a directory transfer.
// It binds the remote root, the worker identity and the tree's names and
// sizes; each file's content is bound by its own upload record when that
// file starts.
type dirManifest struct {
	Format          int       `json:"format"`
	ID              string    `json:"id"`
	NodeID          string    `json:"nodeId"`
	Node            string    `json:"node"`
	LocalPath       string    `json:"localPath"`
	Path            string    `json:"path"`
	SkipUnsupported bool      `json:"skipUnsupported,omitempty"`
	Skipped         int       `json:"skipped,omitempty"`
	SkippedPaths    []string  `json:"skippedPaths,omitempty"`
	Dirs            []string  `json:"dirs"`
	Files           []dirFile `json:"files"`
	Bytes           int64     `json:"bytes"`
	Created         time.Time `json:"created"`
	// DirsCreated is when an attempt first confirmed every directory. Files
	// start only afterwards, so a published file is the same evidence in
	// records that predate this field; only an empty tree depends on it.
	DirsCreated time.Time `json:"dirsCreated,omitzero"`
	// Completed is when an attempt saw every file published.
	Completed time.Time `json:"completed,omitzero"`
	// Aborted is when an abort was recorded, before any file was aborted.
	// It refuses later resumes.
	Aborted time.Time `json:"aborted,omitzero"`
	// ChildRecords marks a transfer whose file records are kept until it
	// completes. Earlier releases pruned a published file's record after
	// the retention period even while the transfer was incomplete.
	ChildRecords bool `json:"childRecords,omitempty"`
}

// childRecordsKept reports whether a missing file record proves the file
// never started: its records are kept, or none can have expired yet.
func (m dirManifest) childRecordsKept(now time.Time) bool {
	return m.ChildRecords || now.Sub(m.Created) < uploadRecordRetention
}

// localTree is a scanned directory: slash-separated relative paths, sorted.
type localTree struct {
	Dirs    []string
	Files   []dirFile
	Bytes   int64
	Skipped []string
}

type dirLimits struct {
	files, dirs int
}

// dirFileID derives a file's upload ID from the directory transfer ID and
// the relative path, so a resumed transfer reuses each file's upload record.
func dirFileID(dirID, rel string) string {
	h := sha256.Sum256([]byte("control-directory-upload-v1\x00" + dirID + "\x00" + rel))
	return "d" + hex.EncodeToString(h[:20])
}

func (c Client) dirUploadPath(id string) string {
	return filepath.Join(c.uploadScopeDir(), "upload-dirs", identity.ID([]byte(id))+".json")
}

func readDirManifest(file, id string) (dirManifest, error) {
	var m dirManifest
	if err := store.Read(file, &m); err != nil {
		return m, err
	}
	if m.Format != dirManifestFormat || m.ID != id || m.NodeID == "" {
		return m, errors.New("invalid local directory upload recovery record")
	}
	return m, nil
}

// refuseDirectoryUploadID keeps single-file and directory IDs apart locally.
func (c Client) refuseDirectoryUploadID(id string) error {
	if _, err := os.Lstat(c.dirUploadPath(id)); err == nil {
		return errors.New("upload ID belongs to a directory upload; use control upload resume with the local directory")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// IsDirectoryUpload reports whether this client holds a directory transfer
// recovery record for id. Records are scoped by the stable task owner, which
// is known only after account routing starts, so routing starts first; a
// lookup before that would read another scope and miss the record.
func (c Client) IsDirectoryUpload(ctx context.Context, id string) (bool, error) {
	if !upload.ValidID(id) {
		return false, nil
	}
	if _, err := c.uploadPeer(ctx); err != nil {
		return false, err
	}
	_, err := os.Lstat(c.dirUploadPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

type uploadDescription struct {
	FileUpload struct {
		Protocol        string `json:"protocol"`
		MaxOwnerBytes   int64  `json:"maxOwnerBytes"`
		MaxStagingBytes int64  `json:"maxStagingBytes"`
	} `json:"fileUpload"`
	Capabilities []struct {
		Name string `json:"name"`
	} `json:"capabilities"`
}

func describeUploadTarget(ctx context.Context, peer *transport.Peer, target string, directories bool) (uploadDescription, error) {
	var d uploadDescription
	if err := peerCall(ctx, peer, target, "node.describe", map[string]any{}, &d); err != nil {
		return d, err
	}
	if d.FileUpload.Protocol != upload.Protocol {
		return d, errors.New("target does not support streaming uploads; upgrade it before sending files")
	}
	if directories && !slices.ContainsFunc(d.Capabilities, func(c struct {
		Name string `json:"name"`
	}) bool {
		return c.Name == "files.mkdir"
	}) {
		return d, errors.New("target does not support files.mkdir; upgrade it before uploading directories")
	}
	return d, nil
}

// foldKey maps a path to a case-insensitive key using Unicode simple case
// folding, the equivalence strings.EqualFold uses.
func foldKey(s string) string {
	return strings.Map(func(r rune) rune {
		return foldRune(r)
	}, s)
}

// scanLocalTree walks root without following symbolic links. Unsupported
// entries are refused, or skipped and listed when skip is set. The scan
// stops as soon as a limit is exceeded.
func scanLocalTree(ctx context.Context, root string, skip bool, limits dirLimits) (localTree, error) {
	var tree localTree
	info, err := os.Lstat(root)
	if err != nil {
		return tree, err
	}
	if info.Mode().Type() != os.ModeDir {
		return tree, errors.New("local directory must be a directory, not a symbolic link or special file")
	}
	type problem struct{ path, reason string }
	var problems []problem
	pending := []string{"."}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return tree, err
		}
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			return tree, err
		}
		for _, entry := range entries {
			rel := path.Join(dir, entry.Name())
			// Lstat, not the listing type, so Windows reparse points report
			// their irregular or link mode before any descent.
			info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				return tree, err
			}
			reason := ""
			switch kind := info.Mode().Type(); {
			case kind&os.ModeSymlink != 0:
				reason = "symbolic link"
			case kind != 0 && kind != os.ModeDir:
				reason = "special file"
			case upload.ValidatePath(rel) != nil:
				reason = "name the destination rejects"
			}
			if reason != "" {
				problems = append(problems, problem{rel, reason})
				if len(problems) > dirUploadMaxProblems {
					return tree, fmt.Errorf("directory contains more than %d unsupported entries", dirUploadMaxProblems)
				}
				continue
			}
			if info.IsDir() {
				tree.Dirs = append(tree.Dirs, rel)
				if len(tree.Dirs) > limits.dirs {
					return tree, fmt.Errorf("directory tree exceeds %d directories; upload smaller subtrees", limits.dirs)
				}
				pending = append(pending, rel)
				continue
			}
			tree.Files = append(tree.Files, dirFile{Path: rel, Size: info.Size()})
			if len(tree.Files) > limits.files {
				return tree, fmt.Errorf("directory tree exceeds %d files; upload smaller subtrees or archive it", limits.files)
			}
			if info.Size() > upload.MaxSize-tree.Bytes {
				return tree, errors.New("directory tree exceeds the 1 TiB upload size")
			}
			tree.Bytes += info.Size()
		}
	}
	slices.SortFunc(problems, func(a, b problem) int { return strings.Compare(a.path, b.path) })
	if len(problems) != 0 && !skip {
		lines := make([]string, 0, dirReportLimit)
		for _, p := range problems[:min(len(problems), dirReportLimit)] {
			lines = append(lines, p.path+" ("+p.reason+")")
		}
		return tree, fmt.Errorf("directory contains %d unsupported entries, nothing was sent; remove them or pass --skip-unsupported to skip them: %s", len(problems), strings.Join(lines, ", "))
	}
	for _, p := range problems {
		tree.Skipped = append(tree.Skipped, p.path)
	}
	slices.Sort(tree.Dirs)
	slices.SortFunc(tree.Files, func(a, b dirFile) int { return strings.Compare(a.Path, b.Path) })
	return tree, checkTreeNames(tree)
}

// checkTreeNames refuses two entries that one case-insensitive destination
// (Windows, default macOS) would store under the same name. Collisions are
// never skipped: choosing one entry would be arbitrary.
func checkTreeNames(tree localTree) error {
	seen := make(map[string]string, len(tree.Dirs)+len(tree.Files))
	var collisions []string
	check := func(rel string) {
		key := foldKey(rel)
		if other, ok := seen[key]; ok {
			collisions = append(collisions, other+" and "+rel)
			return
		}
		seen[key] = rel
	}
	for _, d := range tree.Dirs {
		check(d)
	}
	for _, f := range tree.Files {
		check(f.Path)
	}
	if len(collisions) != 0 {
		slices.Sort(collisions)
		return fmt.Errorf("directory contains %d names that differ only by case, nothing was sent; case-insensitive destinations cannot hold both: %s", len(collisions), strings.Join(collisions[:min(len(collisions), dirReportLimit)], ", "))
	}
	return nil
}

// checkRemoteTree validates every destination path as the worker will and
// checks the worker's advertised upload budgets before anything is sent.
func checkRemoteTree(tree localTree, remote string, d uploadDescription) error {
	if err := upload.ValidatePath(remote); err != nil {
		return fmt.Errorf("remote directory: %w", err)
	}
	for _, rel := range tree.Dirs {
		if err := upload.ValidatePath(path.Join(remote, rel)); err != nil {
			return fmt.Errorf("remote path for %s: %w", rel, err)
		}
	}
	for _, f := range tree.Files {
		if err := upload.ValidatePath(path.Join(remote, f.Path)); err != nil {
			return fmt.Errorf("remote path for %s: %w", f.Path, err)
		}
		if d.FileUpload.MaxOwnerBytes > 0 && f.Size > d.FileUpload.MaxOwnerBytes {
			return fmt.Errorf("%s is larger than the worker's per-owner upload staging budget of %d bytes; nothing was sent", f.Path, d.FileUpload.MaxOwnerBytes)
		}
	}
	if d.FileUpload.MaxStagingBytes > 0 && tree.Bytes > d.FileUpload.MaxStagingBytes {
		return fmt.Errorf("directory tree holds %d bytes, more than the worker's upload staging budget of %d bytes; nothing was sent", tree.Bytes, d.FileUpload.MaxStagingBytes)
	}
	return nil
}

// leafDirs returns directories with no subdirectory. Creating each with
// parents creates every directory of the tree, including empty ones.
func leafDirs(dirs []string) []string {
	parents := map[string]bool{}
	for _, d := range dirs {
		parents[path.Dir(d)] = true
	}
	var leaves []string
	for _, d := range dirs {
		if !parents[d] {
			leaves = append(leaves, d)
		}
	}
	return leaves
}

// UploadDirectory uploads a directory tree. Each call is one attempt; an
// interrupted transfer is continued only by an explicit Resume with the same
// ID, which never re-sends published files and never overwrites.
func (c Client) UploadDirectory(ctx context.Context, s DirUploadSpec) (DirUploadResult, error) {
	// A new transfer has not started until its remote root exists; a resume
	// continues one that has.
	state := "not_started"
	if s.Resume {
		state = "incomplete"
	}
	result := DirUploadResult{Kind: "directory", ID: s.ID, Path: s.Path, State: state, States: map[string]int{}}
	if s.Node == "" || !upload.ValidID(s.ID) || s.LocalPath == "" || (!s.Resume && s.Path == "") {
		return result, errors.New("directory upload requires node, valid id, local directory, and remote directory")
	}
	if s.TimeoutSeconds == 0 {
		s.TimeoutSeconds = 3600
	}
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > 86400 {
		return result, errors.New("upload timeoutSeconds must be 1..86400")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.TimeoutSeconds)*time.Second)
	defer cancel()
	ctx, closeLifetime := c.lifetimeContext(ctx)
	defer closeLifetime()
	peer, err := c.uploadPeer(ctx)
	if err != nil {
		return result, err
	}
	manifestPath := c.dirUploadPath(s.ID)
	if err = os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
		return result, err
	}
	if err = os.MkdirAll(filepath.Dir(c.uploadRecordPath(s.ID)), 0700); err != nil {
		return result, err
	}
	now := time.Now()
	scope := c.uploadScopeDir()
	pruneUploadRecords(ctx, scope, now)
	pruneDirManifests(ctx, scope, now)
	pruneDirAttempts(ctx, scope)
	// One process at a time owns a directory transfer, and abort refuses
	// while an attempt holds it. The lock also reserves the ID against a
	// single-file upload before the remote root is created.
	attempt, err := c.lockDirUpload(ctx, s.ID)
	if errors.Is(err, errDirUploadBusy) {
		return result, upload.Error("upload_busy", "directory upload is active in another process; inspect status after it ends")
	}
	if err != nil {
		return result, err
	}
	defer attempt.release(ctx)
	m, err := readDirManifest(manifestPath, s.ID)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if s.Resume && !exists {
		return result, errors.New("local directory upload recovery record is missing; inspect remote files rather than starting a replacement")
	}
	if !s.Resume && exists {
		return result, upload.Error("upload_exists", "directory upload ID already exists; inspect status and resume it explicitly")
	}
	if exists && !m.Aborted.IsZero() {
		return result, upload.Error("upload_aborted", "directory upload was aborted and cannot be resumed; start a new transfer with a new ID and a new remote directory")
	}
	var target string
	if exists {
		if target, err = c.checkDirTarget(ctx, peer, s.Node, m); err != nil {
			return result, err
		}
	} else {
		destination, err := peer.Lookup(ctx, s.Node)
		if err != nil {
			return result, err
		}
		target = destination.ID
	}
	result.NodeID = target
	description, err := describeUploadTarget(ctx, peer, target, true)
	if err != nil {
		return result, err
	}
	skip, remote := s.SkipUnsupported, s.Path
	if exists {
		skip, remote = m.SkipUnsupported, m.Path
	}
	result.Path = remote
	tree, err := scanLocalTree(ctx, s.LocalPath, skip, dirLimits{files: DirUploadMaxFiles, dirs: DirUploadMaxDirs})
	if err != nil {
		return result, err
	}
	if err = checkRemoteTree(tree, remote, description); err != nil {
		return result, err
	}
	if exists {
		if !slices.Equal(m.Dirs, tree.Dirs) || !slices.Equal(m.Files, tree.Files) || m.Skipped != len(tree.Skipped) {
			return result, upload.Error("source_changed", "local directory names or sizes changed since this directory upload started; nothing was sent")
		}
	} else {
		local, err := filepath.Abs(s.LocalPath)
		if err != nil {
			return result, err
		}
		m = dirManifest{Format: dirManifestFormat, ID: s.ID, NodeID: target, Node: s.Node, LocalPath: local, Path: remote, SkipUnsupported: skip, Skipped: len(tree.Skipped), SkippedPaths: tree.Skipped[:min(len(tree.Skipped), dirReportLimit)], Dirs: tree.Dirs, Files: tree.Files, Bytes: tree.Bytes, Created: now.UTC(), ChildRecords: true}
		if m.Dirs == nil {
			m.Dirs = []string{}
		}
		if m.Files == nil {
			m.Files = []dirFile{}
		}
	}
	result.Directories, result.Files, result.Bytes = len(m.Dirs), len(m.Files), m.Bytes
	result.Skipped, result.SkippedPaths = m.Skipped, m.SkippedPaths
	if !exists {
		// Without parents, files.mkdir creates the root only if it is absent,
		// so a fresh transfer never merges into an existing directory. No
		// recovery record exists unless the root was created.
		if err = peerCall(ctx, peer, target, "files.mkdir", map[string]any{"path": remote}, nil); err != nil {
			if rootNotCreated(err) {
				return result, fmt.Errorf("create remote directory %s, which must not exist and whose parent must; nothing was created or uploaded: %w", remote, err)
			}
			result.State = "unknown"
			return result, fmt.Errorf("create remote directory %s; nothing was uploaded, but the response was lost, so the directory may now exist: inspect it before choosing another destination: %w", remote, err)
		}
		result.State = "incomplete"
		// The attempt lock has reserved the ID against single-file uploads
		// since before the root was created, so no claim can intervene.
		if err = store.Write(manifestPath, m); err != nil {
			return result, fmt.Errorf("remote directory %s was created but the local recovery record could not be saved: %w", remote, err)
		}
	} else if err = peerCall(ctx, peer, target, "files.mkdir", map[string]any{"path": remote, "parents": true}, nil); err != nil {
		return result, err
	}
	for _, dir := range leafDirs(m.Dirs) {
		if err = peerCall(ctx, peer, target, "files.mkdir", map[string]any{"path": path.Join(remote, dir), "parents": true}, nil); err != nil {
			return result, fmt.Errorf("create remote directory %s; no file of this attempt was sent, resume explicitly: %w", path.Join(remote, dir), err)
		}
	}
	if m.DirsCreated.IsZero() {
		m.DirsCreated = time.Now().UTC()
		if err = store.Write(manifestPath, m); err != nil {
			// Not fatal: a published file is the same evidence, and the
			// completion record below or a later resume records it again.
			m.DirsCreated = time.Time{}
		}
	}
	firstErr := c.uploadDirFiles(ctx, peer, target, s, m, &result)
	if firstErr == nil && result.States["published"] == len(m.Files) {
		result.State = "published"
		if m.Completed.IsZero() {
			m.Completed = time.Now().UTC()
			if m.DirsCreated.IsZero() {
				m.DirsCreated = m.Completed
			}
			if err = store.Write(manifestPath, m); err != nil {
				return result, fmt.Errorf("directory upload %s published every file and directory, but its local completion record could not be saved, so status may report it incomplete; resume with the same ID to record completion without sending published files again: %w", s.ID, err)
			}
		}
		return result, nil
	}
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	if firstErr == nil {
		firstErr = errors.New("not every file was published")
	}
	return result, fmt.Errorf("directory upload %s incomplete: %d of %d files published; inspect status, then resume explicitly with the same ID: %w", s.ID, result.States["published"], len(m.Files), firstErr)
}

// rootNotCreated reports whether a failed root files.mkdir provably created
// nothing. Without parents it is one create-only mkdir, so the worker's own
// error reply means the directory was not made; so does a request that was
// never sent. A lost or unreadable reply leaves the outcome unknown.
func rootNotCreated(err error) bool {
	var rpc *model.RPCError
	if errors.Is(err, model.ErrReplied) {
		return true
	}
	return errors.As(err, &rpc) && (rpc.Outcome == "not_sent" || rpc.Outcome == model.OutcomeNotAccepted)
}

func (c Client) checkDirTarget(ctx context.Context, peer *transport.Peer, node string, m dirManifest) (string, error) {
	if node != m.Node && node != m.NodeID {
		destination, err := peer.Lookup(ctx, node)
		if err != nil || destination.ID != m.NodeID {
			return "", errors.New("directory upload belongs to a different worker identity")
		}
	}
	return m.NodeID, nil
}

// uploadDirFiles uploads files with a small bounded number of workers. The
// first failure stops new files from starting; files already in flight
// finish their own attempt. No goroutine outlives the call.
func (c Client) uploadDirFiles(ctx context.Context, peer *transport.Peer, target string, s DirUploadSpec, m dirManifest, result *DirUploadResult) error {
	var mu, report sync.Mutex
	var firstErr error
	next := 0
	work := func() {
		for {
			mu.Lock()
			if firstErr != nil || next >= len(m.Files) || ctx.Err() != nil {
				mu.Unlock()
				return
			}
			f := m.Files[next]
			next++
			mu.Unlock()
			outcome, attempted, err := c.uploadDirFile(ctx, peer, target, s.LocalPath, m, f)
			mu.Lock()
			result.States[outcome.State]++
			if attempted {
				result.FilesAttempted++
			}
			result.BytesSent += outcome.BytesSent
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", f.Path, err)
				}
				if len(result.Failures) < dirReportLimit {
					result.Failures = append(result.Failures, outcome)
				}
			}
			mu.Unlock()
			if s.FileDone != nil {
				report.Lock()
				s.FileDone(outcome)
				report.Unlock()
			}
		}
	}
	var wg sync.WaitGroup
	for range min(dirUploadWorkers, len(m.Files)) {
		wg.Go(work)
	}
	wg.Wait()
	if pending := len(m.Files) - next; pending > 0 {
		result.States["pending"] += pending
	}
	return firstErr
}

// uploadDirFile performs one file's attempt. An existing local record means
// the file started earlier, so it is resumed rather than started again; a
// record this client saw published is not contacted again. attempted reports
// whether an upload stream was requested.
func (c Client) uploadDirFile(ctx context.Context, peer *transport.Peer, target, localRoot string, m dirManifest, f dirFile) (outcome DirUploadFile, attempted bool, err error) {
	outcome = DirUploadFile{Path: f.Path, ID: dirFileID(m.ID, f.Path), State: "failed"}
	remote := path.Join(m.Path, f.Path)
	var prior localUpload
	err = store.Read(c.uploadRecordPath(outcome.ID), &prior)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		outcome.Error = err.Error()
		return outcome, false, err
	}
	resume := err == nil
	if resume {
		if err = childRecordConflict(prior, m, f, target); err != nil {
			outcome.State = "conflict"
			outcome.Code, outcome.Error = "upload_spec_conflict", err.Error()
			return outcome, false, err
		}
		if !prior.Published.IsZero() {
			outcome.State = "published"
			return outcome, false, nil
		}
	}
	// The manifest's size binds the file before its first attempt; its
	// content is bound by the upload record that attempt writes.
	spec := UploadSpec{Node: m.Node, ID: outcome.ID, LocalPath: filepath.Join(localRoot, filepath.FromSlash(f.Path)), Path: remote, Resume: resume, inTree: true, treeSize: f.Size, tree: m.ID}
	r, err := c.uploadFile(ctx, peer, target, spec)
	var refused *model.RPCError
	if resume && errors.As(err, &refused) && refused.Code == "upload_not_found" {
		// The worker definitively holds no record for this ID: an earlier
		// open was refused before reserving anything (for example by a
		// lease). Opening it as new is create-only and cannot replace a file.
		spec.Resume = false
		r, err = c.uploadFile(ctx, peer, target, spec)
	}
	outcome.BytesSent = r.BytesSent
	if err != nil {
		e := model.OperationError(err, "unknown")
		outcome.Code, outcome.Error = e.Code, e.Message
		// A failure after a published progress record is not a confirmed
		// publication; status reconciles it.
		if r.State == "staging" {
			outcome.State = "staging"
		}
		return outcome, true, err
	}
	outcome.State = r.State
	return outcome, true, nil
}

// childRecordConflict refuses a file record that does not bind this file of
// this transfer: its ID, worker, destination and scanned size, and its
// transfer when recorded. An earlier release could publish a file that grew
// after the scan and record the larger size; such a file is not this
// transfer's verified file, and its destination is left alone.
func childRecordConflict(prior localUpload, m dirManifest, f dirFile, target string) error {
	switch {
	case prior.ID != dirFileID(m.ID, f.Path) || (prior.Tree != "" && prior.Tree != m.ID):
		return errors.New("file upload record belongs to another upload")
	case prior.NodeID != target || prior.Path != path.Join(m.Path, f.Path):
		return errors.New("file upload record binds a different worker or destination")
	case prior.Size != f.Size:
		return fmt.Errorf("file upload record binds %d bytes but this directory upload scanned %d; its destination was left unchanged", prior.Size, f.Size)
	}
	return nil
}

// receiptConflict checks a worker's status or abort result for one file
// against its local record.
func receiptConflict(r upload.Result, prior localUpload, target string) error {
	if r.ID != prior.ID || r.NodeID != target {
		return errors.New("upload status identity mismatch")
	}
	if r.Spec != prior.Spec {
		return errors.New("worker upload record binds a different destination or content than this file's local record")
	}
	return nil
}

// foldRune maps r to the smallest rune of its simple case-folding orbit.
func foldRune(r rune) rune {
	smallest := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		smallest = min(smallest, f)
	}
	return smallest
}

// UploadDirectoryStatus reports, or with abort abandons, every unpublished
// file of a directory transfer. Status only reads. Abort refuses while an
// attempt holds the transfer, then records itself before aborting any file,
// so the transfer can no longer be resumed. It never removes published files
// or created directories. Once the transfer is identified, its summary is
// returned with any error, including an abort that could not settle every
// file.
func (c Client) UploadDirectoryStatus(ctx context.Context, node, id string, abort bool) (DirUploadResult, error) {
	result := DirUploadResult{Kind: "directory", ID: id, State: "incomplete", States: map[string]int{}}
	if node == "" || !upload.ValidID(id) {
		return result, errors.New("upload status/abort requires node and valid id")
	}
	peer, err := c.uploadPeer(ctx)
	if err != nil {
		return result, err
	}
	manifestPath := c.dirUploadPath(id)
	m, err := readDirManifest(manifestPath, id)
	if err != nil {
		return result, err
	}
	target, err := c.checkDirTarget(ctx, peer, node, m)
	if err != nil {
		return result, err
	}
	if abort {
		var attempt *dirUploadLock
		attempt, err = c.lockDirUpload(ctx, id)
		if errors.Is(err, errDirUploadBusy) {
			return result, upload.Error("upload_busy", "directory upload is active in another process; cancel it and wait for it to end before aborting; nothing was aborted")
		}
		if err != nil {
			return result, err
		}
		defer attempt.release(ctx)
		// An attempt that ended meanwhile may have changed the record.
		if m, err = readDirManifest(manifestPath, id); err != nil {
			return result, err
		}
		if m.NodeID != target {
			return result, errors.New("directory upload belongs to a different worker identity")
		}
		if m.Completed.IsZero() && m.Aborted.IsZero() {
			m.Aborted = time.Now().UTC()
			if err = store.Write(manifestPath, m); err != nil {
				return result, fmt.Errorf("record the directory upload abort; nothing was aborted: %w", err)
			}
		}
	}
	result.NodeID, result.Path = target, m.Path
	result.Directories, result.Files, result.Bytes = len(m.Dirs), len(m.Files), m.Bytes
	result.Skipped, result.SkippedPaths = m.Skipped, m.SkippedPaths
	result.AbortRequested = !m.Aborted.IsZero()
	now := time.Now()
	method, workers := upload.StatusMethod, dirStatusWorkers
	if abort {
		// Each worker abort claims one of the owner's two active uploads.
		method, workers = upload.AbortMethod, dirUploadWorkers
	}
	var mu sync.Mutex
	next, live := 0, 0
	work := func() {
		for {
			mu.Lock()
			if next >= len(m.Files) || ctx.Err() != nil {
				mu.Unlock()
				return
			}
			f := m.Files[next]
			next++
			mu.Unlock()
			outcome, settled := c.dirFileState(ctx, peer, target, method, m, f, now)
			mu.Lock()
			result.States[outcome.State]++
			if !settled {
				live++
			}
			if outcome.Error != "" && len(result.Failures) < dirReportLimit {
				result.Failures = append(result.Failures, outcome)
			}
			mu.Unlock()
		}
	}
	var wg sync.WaitGroup
	for range min(workers, len(m.Files)) {
		wg.Go(work)
	}
	wg.Wait()
	// Files left unqueried by cancellation are unknown, so the counts
	// still account for every file.
	if left := len(m.Files) - next; left > 0 {
		result.States["unknown"] += left
		live += left
	}
	result.State = dirSummaryState(m, result.States, live)
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if abort && result.State != "aborted" && result.State != "published" {
		return result, upload.Error("upload_abort_incomplete", fmt.Sprintf("directory upload %s abort incomplete: %d files may still hold staging or have an unknown outcome; published files were kept; inspect status, then abort again once they are inactive", id, live))
	}
	return result, nil
}

// dirSummaryState derives a transfer's state from its recovery record and
// file states. live counts files that may still hold or publish staged
// bytes, and every file whose state is unknown or in conflict: none of
// those lets a transfer be called aborted.
func dirSummaryState(m dirManifest, states map[string]int, live int) string {
	files, published, unknown := len(m.Files), states["published"], states["unknown"]
	// Files start only after every directory exists, so one published file
	// is evidence of the directories too; an empty tree needs the mark.
	dirs := !m.DirsCreated.IsZero() || !m.Completed.IsZero() || published > 0
	switch {
	case published == files && dirs:
		return "published"
	case !m.Aborted.IsZero() && live == 0 && unknown == 0 && states["conflict"] == 0:
		return "aborted"
	case unknown > 0 && published+unknown == files:
		// Every other file is published; these may be too.
		return "unknown"
	}
	return "incomplete"
}

// dirFileState reads or aborts one file. settled reports that the file is
// known to be published, or known to hold nothing staged on the worker and
// to be unable to publish: aborted, expired, or never started. Any doubt,
// including a worker that no longer has the record, leaves it unsettled.
func (c Client) dirFileState(ctx context.Context, peer *transport.Peer, target, method string, m dirManifest, f dirFile, now time.Time) (DirUploadFile, bool) {
	outcome := DirUploadFile{Path: f.Path, ID: dirFileID(m.ID, f.Path)}
	var prior localUpload
	err := store.Read(c.uploadRecordPath(outcome.ID), &prior)
	switch {
	case errors.Is(err, os.ErrNotExist) && !m.Completed.IsZero():
		// Records of a completed transfer may already be pruned.
		outcome.State = "published"
		return outcome, true
	case errors.Is(err, os.ErrNotExist) && m.childRecordsKept(now):
		// A file's record is written before its upload is requested and
		// kept until the transfer completes, so the worker was never
		// asked for this file.
		outcome.State = "pending"
		return outcome, true
	case errors.Is(err, os.ErrNotExist):
		outcome.State, outcome.Code = "unknown", "upload_record_missing"
		outcome.Error = "no local file record; an earlier client release may have pruned it after the file was published"
		return outcome, false
	case err != nil:
		outcome.State, outcome.Error = "unknown", err.Error()
		return outcome, false
	}
	if err = childRecordConflict(prior, m, f, target); err != nil {
		outcome.State, outcome.Code, outcome.Error = "conflict", "upload_spec_conflict", err.Error()
		return outcome, false
	}
	if !prior.Published.IsZero() {
		outcome.State = "published"
		return outcome, true
	}
	r, err := c.dirFileCall(ctx, peer, target, upload.StatusMethod, prior)
	// Abort only an upload whose worker record matches this file's record,
	// so a different upload under a matching ID is never abandoned.
	if err == nil && method == upload.AbortMethod && !r.Active && (r.State == "staging" || r.State == "publishing") {
		r, err = c.dirFileCall(ctx, peer, target, upload.AbortMethod, prior)
	}
	if err != nil {
		e := model.OperationError(err, "unknown")
		outcome.State, outcome.Code, outcome.Error = "unknown", e.Code, e.Message
		if e.Code == "upload_spec_conflict" {
			outcome.State = "conflict"
		}
		return outcome, false
	}
	outcome.State = r.State
	if r.Active {
		outcome.State = "active"
		return outcome, false
	}
	return outcome, r.State == "published" || r.State == "aborted" || r.State == "expired"
}

// dirFileCall queries or aborts one file's worker record and checks the
// reply against the file's local record.
func (c Client) dirFileCall(ctx context.Context, peer *transport.Peer, target, method string, prior localUpload) (upload.Result, error) {
	var r upload.Result
	if err := peerCall(ctx, peer, target, method, map[string]string{"id": prior.ID}, &r); err != nil {
		return r, err
	}
	if err := receiptConflict(r, prior, target); err != nil {
		return r, upload.Error("upload_spec_conflict", err.Error())
	}
	return r, nil
}

// UploadState reports or aborts a single-file or directory upload by ID.
func (c Client) UploadState(ctx context.Context, node, id string, abort bool) (any, error) {
	directory, err := c.IsDirectoryUpload(ctx, id)
	if err != nil {
		return nil, err
	}
	if directory {
		return c.UploadDirectoryStatus(ctx, node, id, abort)
	}
	return c.UploadStatus(ctx, node, id, abort)
}

// DirUploadError carries a directory summary with a failed status or abort,
// so MCP results keep the partial state the CLI prints.
type DirUploadError struct {
	Result DirUploadResult
	Err    error
}

func (e *DirUploadError) Error() string { return e.Err.Error() }
func (e *DirUploadError) Unwrap() error { return e.Err }

// pruneDirManifests removes records of directory transfers completed before
// the retention period. Incomplete and aborted transfers are kept for
// explicit resume or inspection. A record is decided and removed under its
// stripe while holding its attempt lock, so an active attempt keeps it and
// stale evidence never removes it. Lock files of older releases are never
// removed.
func pruneDirManifests(ctx context.Context, scope string, now time.Time) {
	dir := filepath.Join(scope, "upload-dirs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if hashed, ok := strings.CutSuffix(entry.Name(), ".json"); ok && entry.Type().IsRegular() {
			pruneDirManifest(scope, hashed, now)
		}
	}
}

func prunableDirManifest(file, hashed string, now time.Time) bool {
	var m dirManifest
	return store.Read(file, &m) == nil && identity.ID([]byte(m.ID)) == hashed && !m.Completed.IsZero() && now.Sub(m.Completed) >= uploadRecordRetention
}

func pruneDirManifest(scope, hashed string, now time.Time) {
	file := filepath.Join(scope, "upload-dirs", hashed+".json")
	// A cheap first read avoids locking records that are kept anyway.
	if !prunableDirManifest(file, hashed, now) {
		return
	}
	stripePath, ok := uploadStripePath(filepath.Join(scope, "upload-locks"), hashed)
	if !ok {
		return
	}
	stripe, ok := tryLockStripe(stripePath)
	if !ok {
		return
	}
	defer func() { _ = stripe.Close() }()
	attempt, err := acquireDirAttempt(stripePath, dirAttemptPath(scope, hashed), file+".lock")
	if err != nil {
		return
	}
	defer attempt.removeHeld()
	if prunableDirManifest(file, hashed, now) {
		_ = os.Remove(file)
	}
}
