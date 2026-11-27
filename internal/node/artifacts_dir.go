package node

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/upload"
)

// Directory export (D187) archives a tree into one immutable artifact. The
// archive is deterministic, so identical trees are identical artifacts.
// Nothing here extracts archives.
const (
	dirArchiveFormat           = "tar-v1"
	dirArchiveMaxEntries       = 10000
	dirArchiveMaxBytes   int64 = 1 << 40
	// dirArchiveReadBatch bounds each directory read; scans continue to EOF.
	dirArchiveReadBatch = 256
)

func artifactExportDescription() map[string]any {
	return map[string]any{"directories": dirArchiveFormat, "maxEntries": dirArchiveMaxEntries, "maxBytes": dirArchiveMaxBytes}
}

type archiveEntry struct {
	name string // slash-separated, relative to the exported directory
	dir  bool
	size int64
}

// exportDirectory returns ok=false when name is not a real directory, so the
// caller keeps its regular-file behavior and errors.
func (n *Node) exportDirectory(ctx context.Context, name string) (model.Artifact, bool, error) {
	info, err := n.root.Lstat(name)
	if err != nil || info.Mode().Type() != os.ModeDir {
		return model.Artifact{}, false, nil
	}
	if filepath.Clean(name) == "." {
		return model.Artifact{}, true, errors.New("export a subdirectory, not the filesystem root")
	}
	entries, err := n.scanArchiveTree(ctx, name)
	if err != nil {
		return model.Artifact{}, true, err
	}
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = writer.CloseWithError(n.writeArchive(ctx, name, entries, writer))
	}()
	a, err := n.importArtifact(ctx, reader, filepath.Base(filepath.Clean(name))+".tar")
	// Unblock the writer if the import stopped first; staging is discarded.
	_ = reader.CloseWithError(errors.New("directory archive import ended"))
	<-done
	return a, true, err
}

// scanArchiveTree lists the tree in archive order without following links.
// Symbolic links, special files and reserved upload staging are refused.
func (n *Node) scanArchiveTree(ctx context.Context, root string) ([]archiveEntry, error) {
	var entries []archiveEntry
	var total int64
	pending := []string{root}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if err := n.uploads.CheckPath(dir); err != nil {
			return nil, err
		}
		children, err := n.scanArchiveDir(ctx, root, dir, &entries, &total)
		if err != nil {
			return nil, err
		}
		pending = append(pending, children...)
	}
	// Byte order of the full path puts every directory before its contents.
	slices.SortFunc(entries, func(a, b archiveEntry) int { return strings.Compare(a.name, b.name) })
	return entries, nil
}

// scanArchiveDir appends the entries of one directory and returns its
// subdirectories. It reads in bounded batches through EOF, so a skipped empty
// staging directory never hides entries beyond the limit; the scan stops only
// at EOF, an error, or an entry that verifiably exceeds the limit.
func (n *Node) scanArchiveDir(ctx context.Context, root, dir string, entries *[]archiveEntry, total *int64) ([]string, error) {
	f, err := openDirectory(n.root, dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var children []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		list, readErr := f.ReadDir(dirArchiveReadBatch)
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		for _, entry := range list {
			child, err := n.scanArchiveEntry(root, dir, entry, entries, total)
			if err != nil {
				return nil, err
			}
			if child != "" {
				children = append(children, child)
			}
		}
		if readErr == io.EOF {
			return children, nil
		}
	}
}

// scanArchiveEntry records one directory entry. It returns the entry's path
// when it is a subdirectory to scan next.
func (n *Node) scanArchiveEntry(root, dir string, entry os.DirEntry, entries *[]archiveEntry, total *int64) (string, error) {
	full := filepath.Join(dir, entry.Name())
	if strings.EqualFold(entry.Name(), upload.StagingDir) {
		// An empty staging directory holds no bytes and is never part of the
		// archive. One holding a partial means an unfinished upload into this
		// tree. Nothing inside staging is ever read, so a partial created
		// after this check is not exported.
		if entry.Name() == upload.StagingDir && entry.Type() == os.ModeDir && emptyDirectory(n.root, full) {
			return "", nil
		}
		return "", upload.Error("upload_private_path", "directory contains private upload staging; finish or abort uploads first")
	}
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if !utf8.ValidString(rel) {
		return "", fmt.Errorf("%q is not a UTF-8 name; directory archives require UTF-8 names", rel)
	}
	info, err := n.root.Lstat(full)
	if err != nil {
		return "", err
	}
	child := ""
	switch info.Mode().Type() {
	case os.ModeDir:
		*entries = append(*entries, archiveEntry{name: rel, dir: true})
		child = full
	case 0:
		if info.Size() > dirArchiveMaxBytes-*total {
			return "", errors.New("directory export exceeds 1 TiB of file data")
		}
		*total += info.Size()
		*entries = append(*entries, archiveEntry{name: rel, size: info.Size()})
	default:
		return "", fmt.Errorf("%s is a symbolic link or special file; directory export accepts only regular files and directories", rel)
	}
	if len(*entries) > dirArchiveMaxEntries {
		return "", fmt.Errorf("directory export exceeds %d entries; export smaller subtrees", dirArchiveMaxEntries)
	}
	return child, nil
}

// emptyDirectory reports whether name is a directory with no entries. Any
// error, including a link or special file, reports false.
func emptyDirectory(root *os.Root, name string) bool {
	f, err := openDirectory(root, name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	_, err = f.ReadDir(1)
	return err == io.EOF
}

// writeArchive writes normalized PAX-format tar headers: no ownership, fixed
// modes and the Unix epoch as modification time. A file that changes size or
// modification time while it is archived fails the export.
func (n *Node) writeArchive(ctx context.Context, root string, entries []archiveEntry, w io.Writer) error {
	tw := tar.NewWriter(w)
	epoch := time.Unix(0, 0).UTC()
	// One copy buffer serves every file in the archive.
	buf := make([]byte, fileChunkSize)
	for _, e := range entries {
		header := &tar.Header{Typeflag: tar.TypeReg, Name: e.name, Mode: 0644, Size: e.size, ModTime: epoch, Format: tar.FormatPAX}
		if e.dir {
			header.Typeflag, header.Name, header.Mode, header.Size = tar.TypeDir, e.name+"/", 0755, 0
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if e.dir {
			continue
		}
		if err := archiveFile(ctx, n.root, filepath.Join(root, filepath.FromSlash(e.name)), e.size, tw, buf); err != nil {
			return fmt.Errorf("%s: %w", e.name, err)
		}
	}
	return tw.Close()
}

func archiveFile(ctx context.Context, root *os.Root, name string, size int64, w io.Writer, buf []byte) error {
	f, info, err := openRegularFile(root, name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if info.Size() != size {
		return errors.New("file changed size while the directory was archived")
	}
	copied, err := copyBoundedBuffer(ctx, w, f, size, buf)
	if err != nil {
		return err
	}
	return unchangedHandle(f, info, copied)
}
