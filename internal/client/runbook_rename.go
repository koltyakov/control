package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gofrs/flock"
)

var runbookAlias = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

type runbookEdit struct {
	path          string
	before, after []byte
	mode          fs.FileMode
}

// renameMachine migrates only this working directory's runbooks. The gateway
// and local filesystem cannot commit together: never replay or undo a remote
// rename when its response or a subsequent local write fails.
func (c Admin) renameMachine(ctx context.Context, id, name, directory string) error {
	return c.renameMachineWithForce(ctx, id, name, directory, false)
}

func (c Admin) renameMachineWithForce(ctx context.Context, id, name, directory string, force bool) error {
	rename := func() error {
		q := struct {
			Name  string `json:"name"`
			Force bool   `json:"force,omitempty"`
		}{name, force}
		return c.JSON(ctx, http.MethodPatch, "/v1/fleet/nodes/"+url.PathEscape(id), q, nil)
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return rename()
	}
	if err != nil {
		return fmt.Errorf("inspect runbook directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("runbook directory must be a directory, not a symlink")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	lockPath := filepath.Join(directory, ".rename.lock")
	if info, err := os.Lstat(lockPath); err == nil && !info.Mode().IsRegular() {
		return errors.New("runbook lock must be a regular file")
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	lock := flock.New(lockPath)
	defer func() { _ = lock.Close() }()
	held, err := lock.TryLock()
	if err != nil {
		return fmt.Errorf("lock runbooks: %w", err)
	}
	if !held {
		return errors.New("another runbook migration is running")
	}
	n, err := c.ResolveMachine(ctx, id)
	if err != nil {
		return err
	}
	if !runbookAlias.MatchString(n.Name) || !runbookAlias.MatchString(name) {
		return errors.New("runbook aliases must follow the machine-name syntax")
	}
	if n.Name == name {
		return rename()
	}
	source, err := root.Lstat(n.Name)
	if errors.Is(err, fs.ErrNotExist) {
		return rename()
	}
	if err != nil {
		return err
	}
	if !source.IsDir() {
		return errors.New("machine runbook path must be a directory, not a symlink")
	}
	if err := runbookDestination(root, source, name); err != nil {
		return err
	}
	edits, err := prepareRunbookEdits(ctx, root, n.Name, name)
	if err != nil {
		return fmt.Errorf("prepare runbook migration: %w", err)
	}
	if err := rename(); err != nil {
		return fmt.Errorf("machine rename not confirmed; runbooks remain under %q; resolve the machine ID before retrying: %w", n.Name, err)
	}
	// Recheck after the request. The lock excludes Control migrations, not editors.
	if err := commitRunbookEdits(root, source, n.Name, name, edits); err != nil {
		return fmt.Errorf("machine %q was renamed to %q, but runbook migration is incomplete; do not repeat the gateway rename: %w", id, name, err)
	}
	return nil
}

func runbookDestination(root *os.Root, source fs.FileInfo, name string) error {
	destination, err := root.Lstat(name)
	if err == nil && !os.SameFile(source, destination) {
		return fmt.Errorf("runbook destination %q already exists; nothing will be overwritten", name)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func prepareRunbookEdits(ctx context.Context, root *os.Root, old, name string) ([]runbookEdit, error) {
	var edits []runbookEdit
	entries, total := 0, 0
	err := fs.WalkDir(root.FS(), old, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > 1024 {
			return errors.New("runbook tree exceeds 1024 entries")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("runbook entry %q is not a regular file", p)
		}
		ext := strings.ToLower(path.Ext(p))
		if ext != ".md" && ext != ".json" {
			return nil
		}
		b, err := readRunbook(root, p)
		if err != nil {
			return err
		}
		total += len(b)
		if total > 16<<20 {
			return errors.New("runbook text exceeds 16 MiB")
		}
		after, err := updateRunbookReferences(b, ext, old, name)
		if err != nil {
			return fmt.Errorf("runbook %q: %w", p, err)
		}
		if !bytes.Equal(b, after) {
			edits = append(edits, runbookEdit{path: strings.TrimPrefix(p, old+"/"), before: b, after: after, mode: info.Mode().Perm()})
		}
		return nil
	})
	return edits, err
}

func readRunbook(root *os.Root, p string) ([]byte, error) {
	f, err := root.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if len(b) > 1<<20 {
		return nil, fmt.Errorf("runbook text file %q exceeds 1 MiB", p)
	}
	return b, err
}

func updateRunbookReferences(b []byte, ext, old, name string) ([]byte, error) {
	if ext == ".json" {
		var value any
		if json.Unmarshal(b, &value) != nil {
			return nil, errors.New("invalid JSON")
		}
	}
	// Replace only routing fields, never arbitrary strings such as secret names.
	fields := regexp.MustCompile(`("(?:node|target|machine)"\s*:\s*)("(?:[^"\\]|\\.)*")`)
	s := fields.ReplaceAllStringFunc(string(b), func(match string) string {
		parts := fields.FindStringSubmatch(match)
		var value string
		if json.Unmarshal([]byte(parts[2]), &value) == nil && value == old {
			encoded, _ := json.Marshal(name)
			return parts[1] + string(encoded)
		}
		return match
	})
	if ext == ".md" {
		alias := regexp.QuoteMeta(old)
		header := regexp.MustCompile("(?m)(^Machine:[ \\t]*`?)" + alias + "(`?[ \\t]*\\r?$)")
		s = header.ReplaceAllString(s, "${1}"+name+"${2}")
		commands := regexp.MustCompile("(\\bcontrol(?:\\.exe)?[ \\t]+(?:call|exec|system|speedtest|tunnel(?:[ \\t]+start)?|clipboard[ \\t]+paste|task[ \\t]+(?:start|wait|get|logs|cancel|list)|artifact[ \\t]+(?:get|export|deliver)|machines[ \\t]+(?:rename|enable|disable|unregister|forget))[ \\t]+)" + alias + "([[:space:]`\"']|$)")
		s = commands.ReplaceAllString(s, "${1}"+name+"${2}")
	}
	return []byte(s), nil
}

func commitRunbookEdits(root *os.Root, source fs.FileInfo, old, name string, edits []runbookEdit) error {
	current, err := root.Lstat(old)
	if err != nil {
		return err
	}
	if !current.IsDir() || !os.SameFile(source, current) {
		return errors.New("source runbook directory changed during rename")
	}
	if err := runbookDestination(root, source, name); err != nil {
		return err
	}
	if err := root.Rename(old, name); err != nil {
		return err
	}
	for _, edit := range edits {
		p := path.Join(name, edit.path)
		if err := checkRunbookPath(root, p, edit.mode); err != nil {
			return err
		}
		current, err := readRunbook(root, p)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, edit.before) {
			return fmt.Errorf("runbook %q changed during rename; its contents were preserved", p)
		}
		if err := writeRunbook(root, p, edit.after, edit.mode); err != nil {
			return fmt.Errorf("update runbook %q: %w", p, err)
		}
	}
	return nil
}

func checkRunbookPath(root *os.Root, p string, mode fs.FileMode) error {
	parts := strings.Split(p, "/")
	for i := range parts {
		entry := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(entry)
		if err != nil {
			return err
		}
		if i < len(parts)-1 && !info.IsDir() || i == len(parts)-1 && (!info.Mode().IsRegular() || info.Mode().Perm() != mode) {
			return fmt.Errorf("runbook path %q changed during rename; its contents were preserved", entry)
		}
	}
	return nil
}

func writeRunbook(root *os.Root, p string, b []byte, mode fs.FileMode) error {
	tmp := path.Join(path.Dir(p), ".rename-"+rand.Text())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmp) }()
	err = f.Chmod(mode)
	if err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return root.Rename(tmp, p)
}
