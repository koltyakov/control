// Package secrets stores worker-local credentials. Values have no network read API.
package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/store"
)

const MaxValue = 4096

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func ValidName(name string) bool { return namePattern.MatchString(name) }

// Store uses private, atomic files, not encryption or an execution sandbox.
// Keep its directory outside the filesystem API's workspace.
type Store struct {
	Dir     string
	WorkDir string
}

func (s Store) check() error {
	dir, err := filepath.EvalSymlinks(s.Dir)
	if err != nil {
		return err
	}
	work, err := filepath.EvalSymlinks(s.WorkDir)
	if err != nil {
		return err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	work, err = filepath.Abs(work)
	if err != nil {
		return err
	}
	workInfo, err := os.Stat(work)
	if err != nil {
		return err
	}
	// Compare directory identities, not spellings. macOS and Windows can have
	// case-insensitive filesystems with differently cased profile paths.
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil {
			return err
		}
		if os.SameFile(info, workInfo) {
			return errors.New("secret storage must be outside workDir")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

// Snapshot is for destination-side injection and redaction only, never RPC results.
func (s Store) Snapshot() (map[string]string, error) {
	values := map[string]string{}
	f, err := os.Open(filepath.Join(s.Dir, "values.json"))
	if errors.Is(err, os.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, errors.New("cannot open secret storage")
	}
	defer func() { _ = f.Close() }()
	if err := s.check(); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) > 1<<20 || json.Unmarshal(b, &values) != nil || values == nil || len(values) > 128 {
		return nil, errors.New("invalid secret storage")
	}
	for name, value := range values {
		if !ValidName(name) || !validValue(value) {
			return nil, errors.New("invalid secret storage")
		}
	}
	return values, nil
}

func validValue(value string) bool {
	return value != "" && len(value) <= MaxValue && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func (s Store) Set(ctx context.Context, name, value string) error {
	if !ValidName(name) || !validValue(value) {
		return errors.New("secret requires a valid name and 1..4096 UTF-8 bytes without NUL")
	}
	return s.change(ctx, func(values map[string]string) error {
		if _, exists := values[name]; !exists && len(values) >= 128 {
			return errors.New("at most 128 secrets may be stored")
		}
		values[name] = value
		return nil
	})
}

func (s Store) Delete(ctx context.Context, name string) error {
	if !ValidName(name) {
		return errors.New("invalid secret name")
	}
	return s.change(ctx, func(values map[string]string) error {
		delete(values, name)
		return nil
	})
}

func (s Store) Names() ([]string, error) {
	values, err := s.Snapshot()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (s Store) change(ctx context.Context, update func(map[string]string) error) error {
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	if err := s.check(); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(s.Dir, "secrets.lock"))
	defer func() { _ = lock.Close() }()
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("secret storage is busy")
	}
	values, err := s.Snapshot()
	if err != nil {
		return err
	}
	if err := update(values); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(values)
	if err != nil || len(b) > 1<<20 {
		return errors.New("secret storage exceeds 1 MiB")
	}
	return store.Bytes(filepath.Join(s.Dir, "values.json"), b, 0600)
}
