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
		f, err := openDirectory(n.root, dir)
		if err != nil {
			return nil, err
		}
		list, err := f.ReadDir(dirArchiveMaxEntries - len(entries) + 1)
		_ = f.Close()
		if err != nil && err != io.EOF {
			return nil, err
		}
		for _, entry := range list {
			if strings.EqualFold(entry.Name(), upload.StagingDir) {
				return nil, upload.Error("upload_private_path", "directory contains private upload staging; finish or abort uploads first")
			}
			full := filepath.Join(dir, entry.Name())
			rel, err := filepath.Rel(root, full)
			if err != nil {
				return nil, err
			}
			rel = filepath.ToSlash(rel)
			if !utf8.ValidString(rel) {
				return nil, fmt.Errorf("%q is not a UTF-8 name; directory archives require UTF-8 names", rel)
			}
			info, err := n.root.Lstat(full)
			if err != nil {
				return nil, err
			}
			switch info.Mode().Type() {
			case os.ModeDir:
				entries = append(entries, archiveEntry{name: rel, dir: true})
				pending = append(pending, full)
			case 0:
				if info.Size() > dirArchiveMaxBytes-total {
					return nil, errors.New("directory export exceeds 1 TiB of file data")
				}
				total += info.Size()
				entries = append(entries, archiveEntry{name: rel, size: info.Size()})
			default:
				return nil, fmt.Errorf("%s is a symbolic link or special file; directory export accepts only regular files and directories", rel)
			}
			if len(entries) > dirArchiveMaxEntries {
				return nil, fmt.Errorf("directory export exceeds %d entries; export smaller subtrees", dirArchiveMaxEntries)
			}
		}
	}
	// Byte order of the full path puts every directory before its contents.
	slices.SortFunc(entries, func(a, b archiveEntry) int { return strings.Compare(a.name, b.name) })
	return entries, nil
}

// writeArchive writes normalized PAX-format tar headers: no ownership, fixed
// modes and the Unix epoch as modification time. A file that changes size or
// modification time while it is archived fails the export.
func (n *Node) writeArchive(ctx context.Context, root string, entries []archiveEntry, w io.Writer) error {
	tw := tar.NewWriter(w)
	epoch := time.Unix(0, 0).UTC()
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
		if err := archiveFile(ctx, n.root, filepath.Join(root, filepath.FromSlash(e.name)), e.size, tw); err != nil {
			return fmt.Errorf("%s: %w", e.name, err)
		}
	}
	return tw.Close()
}

func archiveFile(ctx context.Context, root *os.Root, name string, size int64, w io.Writer) error {
	f, info, err := openRegularFile(root, name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if info.Size() != size {
		return errors.New("file changed size while the directory was archived")
	}
	copied, err := copyBounded(ctx, w, f, size)
	if err != nil {
		return err
	}
	return unchangedHandle(f, info, copied)
}
