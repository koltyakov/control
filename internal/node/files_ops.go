package node

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/upload"
)

const (
	// fileDigestLimit bounds files.stat hashing and fileCopyLimit bounds
	// files.copy. Both run inside one operation slot without streaming bytes
	// to the caller; larger files belong in tasks, artifacts or exec.run.
	fileDigestLimit = 1 << 30
	fileCopyLimit   = 1 << 30
	// fileMoveScanLimit bounds the reserved-path scan before a directory move,
	// matching the files.list entry bound.
	fileMoveScanLimit = 10000
	fileChunkSize     = 1 << 20
)

// fileTarget validates a mutation or inspection path. Reserved upload paths
// stay private, and only files.stat may name the root itself.
func (n *Node) fileTarget(name string, allowRoot bool) error {
	if name == "" {
		return errors.New("path is required")
	}
	if !allowRoot && filepath.Clean(name) == "." {
		return errors.New("path must name an entry below the filesystem root")
	}
	return n.uploads.CheckPath(name)
}

// fileKind names the only entry types these methods accept. Symbolic links
// (from Lstat), devices, FIFOs, sockets and Windows reparse points are refused.
func fileKind(info os.FileInfo) string {
	switch {
	case info.Mode().IsRegular():
		return "file"
	case info.IsDir():
		return "directory"
	default:
		return ""
	}
}

func destinationExists(name string) error {
	return &model.RPCError{Code: "destination_exists", Outcome: "unknown", Message: fmt.Sprintf("destination %s exists; set overwrite to replace it", name)}
}

func partialFileEffect(message string, err error) error {
	return &model.RPCError{Code: "partial_effect", Outcome: "unknown", Message: message + "; inspect the paths before any further change, do not retry: " + err.Error(), Cause: err}
}

// copyBounded copies at most limit bytes and checks cancellation between
// chunks. It returns limit+1 copied bytes when the source is larger.
func copyBounded(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	buf := make([]byte, fileChunkSize)
	var copied int64
	src = io.LimitReader(src, limit+1)
	for {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		size, readErr := src.Read(buf)
		if size > 0 {
			written, err := dst.Write(buf[:size])
			copied += int64(written)
			if err != nil {
				return copied, err
			}
		}
		if readErr == io.EOF {
			return copied, nil
		}
		if readErr != nil {
			return copied, readErr
		}
	}
}

// unchangedHandle detects a concurrent change to the opened file during a
// bounded read, so a digest or copy never reports a mixed snapshot as exact.
func unchangedHandle(f *os.File, before os.FileInfo, copied int64) error {
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if copied != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return errors.New("source changed while it was being read")
	}
	return nil
}

func (n *Node) filesStat(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var q struct {
		Path   string `json:"path"`
		SHA256 bool   `json:"sha256"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if q.Path == "" {
		q.Path = "."
	}
	if err := n.fileTarget(q.Path, true); err != nil {
		return nil, err
	}
	// Root-confined symlinks resolve like files.read; special targets are
	// refused from metadata without opening them.
	info, err := n.root.Stat(q.Path)
	if err != nil {
		return nil, err
	}
	kind := fileKind(info)
	if kind == "" {
		return nil, errors.New("path must be a regular file or directory")
	}
	if !q.SHA256 {
		result := map[string]any{"type": kind, "modified": info.ModTime()}
		if kind == "file" {
			result["size"] = info.Size()
		}
		return result, nil
	}
	if kind != "file" {
		return nil, errors.New("sha256 requires a regular file")
	}
	f, info, err := openRegularFile(n.root, q.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if info.Size() > fileDigestLimit {
		return nil, errors.New("sha256 is limited to files of at most 1 GiB; use artifacts.export, whose ID is the SHA-256 digest, or a task")
	}
	h := sha256.New()
	copied, err := copyBounded(ctx, h, f, fileDigestLimit)
	if err == nil {
		err = unchangedHandle(f, info, copied)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": kind, "size": info.Size(), "modified": info.ModTime(), "sha256": hex.EncodeToString(h.Sum(nil))}, nil
}

func (n *Node) filesMkdir(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var q struct {
		Path    string `json:"path"`
		Parents bool   `json:"parents"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if err := n.fileTarget(q.Path, false); err != nil {
		return nil, err
	}
	var err error
	if q.Parents {
		err = n.root.MkdirAll(q.Path, 0700)
	} else {
		err = n.root.Mkdir(q.Path, 0700)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": q.Path, "type": "directory"}, nil
}

// filesRemove deletes one regular file or empty directory. Removal unlinks
// the final name without following it, so a raced replacement cannot reach
// outside the root. Recursive removal is deliberately unsupported.
func (n *Node) filesRemove(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var q struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if q.Recursive {
		return nil, errors.New("recursive removal is not supported; remove entries individually")
	}
	if err := n.fileTarget(q.Path, false); err != nil {
		return nil, err
	}
	info, err := n.root.Lstat(q.Path)
	if err != nil {
		return nil, err
	}
	kind := fileKind(info)
	if kind == "" {
		return nil, errors.New("path must be a regular file or empty directory, not a symbolic link or special file")
	}
	if kind == "directory" {
		// An empty staging directory left by an earlier version or interrupted
		// cleanup is not content, and files.list never shows it.
		err = n.uploads.RemoveDirectory(q.Path)
	} else {
		err = n.root.Remove(q.Path)
	}
	if err != nil {
		var refused *model.RPCError
		if kind == "directory" && !errors.As(err, &refused) {
			return nil, fmt.Errorf("remove directory (only empty directories can be removed): %w", err)
		}
		return nil, err
	}
	return map[string]any{"removed": q.Path, "type": kind}, nil
}

// checkMovableTree refuses directory moves that would carry reserved upload
// staging or metadata, or the task workspace root, to another location. The
// scan never follows symbolic links and is bounded like files.list.
func (n *Node) checkMovableTree(ctx context.Context, name string) error {
	if strings.EqualFold(filepath.Clean(name), "tasks") {
		return errors.New("the task workspace root cannot be moved")
	}
	pending, seen := []string{name}, 0
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if err := n.uploads.CheckPath(dir); err != nil {
			return err
		}
		f, err := openDirectory(n.root, dir)
		if err != nil {
			return err
		}
		entries, err := f.ReadDir(fileMoveScanLimit - seen + 1)
		_ = f.Close()
		if err != nil && err != io.EOF {
			return err
		}
		seen += len(entries)
		if seen > fileMoveScanLimit {
			return errors.New("directory tree exceeds 10000 entries; move smaller subtrees")
		}
		for _, entry := range entries {
			if strings.EqualFold(entry.Name(), upload.StagingDir) {
				return upload.Error("upload_private_path", "directory contains private upload staging; finish or abort uploads first")
			}
			// Directory entry types come from the listing and do not follow links.
			if entry.IsDir() {
				pending = append(pending, filepath.Join(dir, entry.Name()))
			}
		}
	}
	return nil
}

// linkNoOverwrite publishes newname atomically without replacing an existing
// entry, including one created concurrently after an earlier check.
func (n *Node) linkNoOverwrite(oldname, newname string) error {
	err := n.root.Link(oldname, newname)
	if errors.Is(err, os.ErrExist) {
		return destinationExists(newname)
	}
	if err != nil {
		return fmt.Errorf("create %s without overwrite (requires hard-link support): %w", newname, err)
	}
	return nil
}

// checkDestination enforces no-overwrite before a mutation. Overwrite
// replaces only a regular file with a regular file; directories, links and
// special files are never replaced, matching os.Root.Rename, which refuses
// to replace a directory.
func (n *Node) checkDestination(name, kind string, overwrite bool) error {
	info, err := n.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !overwrite {
		return destinationExists(name)
	}
	if kind != "file" || fileKind(info) != "file" {
		return fmt.Errorf("destination %s exists; overwrite replaces only a regular file with a regular file", name)
	}
	return nil
}

func (n *Node) filesMove(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var q struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if err := n.fileTarget(q.From, false); err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	if err := n.fileTarget(q.To, false); err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	info, err := n.root.Lstat(q.From)
	if err != nil {
		return nil, err
	}
	kind := fileKind(info)
	if kind == "" {
		return nil, errors.New("source must be a regular file or directory, not a symbolic link or special file")
	}
	if err = n.checkDestination(q.To, kind, q.Overwrite); err != nil {
		return nil, err
	}
	if kind == "directory" {
		if err = n.checkMovableTree(ctx, q.From); err != nil {
			return nil, err
		}
	}
	result := map[string]any{"from": q.From, "to": q.To, "type": kind}
	if q.Overwrite || kind == "directory" {
		// Rename never follows the final names. A directory relies on the
		// absence check above, and os.Root.Rename also refuses to replace an
		// existing directory.
		if err = n.root.Rename(q.From, q.To); err != nil {
			return nil, err
		}
		return result, nil
	}
	if err = n.linkNoOverwrite(q.From, q.To); err != nil {
		return nil, err
	}
	if err = n.root.Remove(q.From); err != nil {
		return nil, partialFileEffect(fmt.Sprintf("%s was created but %s could not be removed; both names refer to the same file", q.To, q.From), err)
	}
	return result, nil
}

// filesCopy writes a private temporary file beside the destination and
// publishes it only after a complete, synced copy, so the destination never
// holds partial bytes. Without overwrite, publication is a hard link that
// cannot replace an existing entry.
func (n *Node) filesCopy(ctx context.Context, args json.RawMessage, e Execution) (_ any, runErr error) {
	var q struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if err := n.fileTarget(q.From, false); err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	if err := n.fileTarget(q.To, false); err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	src, info, err := openRegularFile(n.root, q.From)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()
	if info.Size() > fileCopyLimit {
		return nil, errors.New("files.copy is limited to files of at most 1 GiB; use a task or artifacts for larger files")
	}
	if err = n.checkDestination(q.To, "file", q.Overwrite); err != nil {
		return nil, err
	}
	temp := filepath.Join(filepath.Dir(q.To), ".control-copy-"+rand.Text()+".tmp")
	out, _, err := openRegular(n.root, temp, regularWriteFlags|os.O_EXCL, info.Mode().Perm()|0600)
	if err != nil {
		return nil, err
	}
	closed, published := false, false
	defer func() {
		if !closed {
			_ = out.Close()
		}
		if !published {
			if err := n.root.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
				runErr = errors.Join(runErr, fmt.Errorf("temporary file %s remains: %w", temp, err))
			}
		}
	}()
	copied, err := copyBounded(ctx, out, src, fileCopyLimit)
	if err == nil {
		err = unchangedHandle(src, info, copied)
	}
	if err == nil {
		err = out.Sync()
	}
	closed = true
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if q.Overwrite {
		if err = n.root.Rename(temp, q.To); err != nil {
			return nil, err
		}
		published = true
	} else {
		if err = n.linkNoOverwrite(temp, q.To); err != nil {
			return nil, err
		}
		published = true
		if err = n.root.Remove(temp); err != nil {
			return nil, partialFileEffect(fmt.Sprintf("%s was copied completely but temporary name %s remains", q.To, temp), err)
		}
	}
	return map[string]any{"from": q.From, "to": q.To, "size": copied}, nil
}
