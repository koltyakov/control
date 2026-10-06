package clipboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const Protocol = "clipboard-v1"
const MaxFiles = 128
const MaxBytes int64 = 1 << 40

type File struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Content contains transfer metadata only, never source paths or file bytes.
type Content struct {
	Kind  string `json:"kind"`
	Text  string `json:"text,omitempty"`
	Files []File `json:"files,omitempty"`
}

func (c Content) Validate() error {
	if c.Kind == "text" {
		if len(c.Text) > MaxText || !utf8.ValidString(c.Text) || len(c.Files) != 0 {
			return errors.New("invalid clipboard text; maximum 1 MiB UTF-8")
		}
		return nil
	}
	if c.Kind != "files" || c.Text != "" || len(c.Files) == 0 || len(c.Files) > MaxFiles {
		return errors.New("clipboard must contain text or 1..128 regular files")
	}
	seen := map[string]bool{}
	var total int64
	for _, f := range c.Files {
		if !validName(f.Name) || f.Size < 0 || f.Size > MaxBytes-total || seen[strings.ToLower(f.Name)] {
			return errors.New("invalid, duplicate, or oversized clipboard file")
		}
		seen[strings.ToLower(f.Name)] = true
		total += f.Size
	}
	return nil
}

func validName(name string) bool {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || strings.ContainsAny(name, `/\:<>"|?*`) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return false
		}
	}
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CONIN$" || base == "CONOUT$" {
		return false
	}
	chars := []rune(base)
	if len(chars) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return !strings.ContainsRune("123456789¹²³", chars[3])
	}
	return true
}

type Batch struct {
	Content Content
	files   []*os.File
	info    []os.FileInfo
}

func Snapshot(value Value) (_ *Batch, err error) {
	b := &Batch{Content: Content{Kind: "text", Text: value.Text}}
	defer func() {
		if err != nil {
			b.Close()
		}
	}()
	if len(value.Paths) > MaxFiles {
		return nil, errors.New("clipboard exceeds 128 files")
	}
	if len(value.Paths) != 0 {
		b.Content = Content{Kind: "files"}
		for _, path := range value.Paths {
			if !filepath.IsAbs(path) {
				return nil, errors.New("clipboard file paths must be absolute")
			}
			info, err := os.Lstat(path)
			if err != nil {
				return nil, fmt.Errorf("cannot inspect selected clipboard file %q", filepath.Base(path))
			}
			if !info.Mode().IsRegular() {
				return nil, errors.New("clipboard supports regular files only, not directories or symlinks")
			}
			f, err := os.Open(path)
			if err != nil {
				return nil, fmt.Errorf("cannot open selected clipboard file %q", filepath.Base(path))
			}
			b.files = append(b.files, f)
			opened, err := f.Stat()
			if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
				return nil, errors.New("clipboard file changed while opening")
			}
			b.Content.Files = append(b.Content.Files, File{Name: filepath.Base(path), Size: opened.Size()})
			b.info = append(b.info, opened)
		}
	}
	if err := b.Content.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Batch) Close() {
	for _, f := range b.files {
		_ = f.Close()
	}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Send reads file bytes only after the destination has accepted the paste.
func (b *Batch) Send(ctx context.Context, w io.Writer) error {
	for i, f := range b.files {
		info, err := f.Stat()
		if err != nil || info.Size() != b.Content.Files[i].Size || !info.ModTime().Equal(b.info[i].ModTime()) {
			return errors.New("clipboard file changed before transfer")
		}
		h := sha256.New()
		if _, err := io.CopyN(io.MultiWriter(w, h), contextReader{ctx, f}, b.Content.Files[i].Size); err != nil {
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				err = pathErr.Err
			}
			return err
		}
		info, err = f.Stat()
		if err != nil || info.Size() != b.Content.Files[i].Size || !info.ModTime().Equal(b.info[i].ModTime()) {
			return errors.New("clipboard file changed during transfer")
		}
		if _, err := w.Write(h.Sum(nil)); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Receive publishes each verified file without overwriting existing entries.
// Previously completed files remain if a later file fails.
func Receive(ctx context.Context, c Content, root *os.Root, r io.Reader) ([]string, error) {
	if err := CheckDestination(c, root); err != nil {
		return nil, err
	}
	var names []string
	for _, file := range c.Files {
		if err := receiveFile(ctx, root, file, r); err != nil {
			return names, fmt.Errorf("paste %s: %w", file.Name, err)
		}
		names = append(names, file.Name)
	}
	return names, nil
}

// CheckDestination rejects collisions before the sender starts reading file bytes.
func CheckDestination(c Content, root *os.Root) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Kind != "files" {
		return errors.New("file clipboard required")
	}
	for _, f := range c.Files {
		if _, err := root.Lstat(f.Name); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("destination already exists or cannot be inspected: %s", f.Name)
		}
	}
	return nil
}

func receiveFile(ctx context.Context, root *os.Root, file File, r io.Reader) error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temp := ".control-paste-" + hex.EncodeToString(random[:])
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = root.Remove(temp) }()
	h := sha256.New()
	if _, err = io.CopyN(io.MultiWriter(f, h), contextReader{ctx, r}, file.Size); err != nil {
		return err
	}
	var sum [sha256.Size]byte
	if _, err = io.ReadFull(contextReader{ctx, r}, sum[:]); err != nil {
		return err
	}
	if !bytes.Equal(sum[:], h.Sum(nil)) {
		return errors.New("clipboard file checksum mismatch")
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Hard-link publication is atomic and fails if the name already exists.
	return root.Link(temp, file.Name)
}
