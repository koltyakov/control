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

// Metadata describes a secret without its value. Zero timestamps mean unknown.
type Metadata struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
}

type record struct {
	Value     string    `json:"value"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
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
	records, err := s.read()
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for name, entry := range records {
		values[name] = entry.Value
	}
	return values, nil
}

func (s Store) read() (map[string]record, error) {
	records := map[string]record{}
	f, err := os.Open(filepath.Join(s.Dir, "values.json"))
	if errors.Is(err, os.ErrNotExist) {
		return records, nil
	}
	if err != nil {
		return nil, errors.New("cannot open secret storage")
	}
	defer func() { _ = f.Close() }()
	if err := s.check(); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	var entries map[string]json.RawMessage
	if err != nil || len(b) > 1<<20 || json.Unmarshal(b, &entries) != nil || entries == nil || len(entries) > 128 {
		return nil, errors.New("invalid secret storage")
	}
	for name, raw := range entries {
		var entry record
		// Legacy entries contain only a string. Never infer their dates from
		// file metadata or another entry's update.
		if len(raw) > 0 && raw[0] == '"' {
			err = json.Unmarshal(raw, &entry.Value)
		} else {
			err = json.Unmarshal(raw, &entry)
		}
		if err != nil || !ValidName(name) || !validValue(entry.Value) {
			return nil, errors.New("invalid secret storage")
		}
		records[name] = entry
	}
	return records, nil
}

func validValue(value string) bool {
	return value != "" && len(value) <= MaxValue && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func (s Store) Set(ctx context.Context, name, value string) error {
	if !ValidName(name) || !validValue(value) {
		return errors.New("secret requires a valid name and 1..4096 UTF-8 bytes without NUL")
	}
	return s.change(ctx, func(records map[string]record) error {
		entry, exists := records[name]
		if !exists && len(records) >= 128 {
			return errors.New("at most 128 secrets may be stored")
		}
		now := time.Now().UTC()
		if !exists {
			entry.CreatedAt = now
		}
		entry.Value = value
		entry.UpdatedAt = now
		records[name] = entry
		return nil
	})
}

func (s Store) Delete(ctx context.Context, name string) error {
	if !ValidName(name) {
		return errors.New("invalid secret name")
	}
	return s.change(ctx, func(records map[string]record) error {
		delete(records, name)
		return nil
	})
}

func (s Store) Names() ([]string, error) {
	entries, err := s.List()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names, nil
}

// List returns sorted metadata only, never secret values.
func (s Store) List() ([]Metadata, error) {
	records, err := s.read()
	if err != nil {
		return nil, err
	}
	entries := make([]Metadata, 0, len(records))
	for name, entry := range records {
		entries = append(entries, Metadata{Name: name, CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func (s Store) change(ctx context.Context, update func(map[string]record) error) error {
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
	records, err := s.read()
	if err != nil {
		return err
	}
	if err := update(records); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(records)
	if err != nil || len(b) > 1<<20 {
		return errors.New("secret storage exceeds 1 MiB")
	}
	return store.Bytes(filepath.Join(s.Dir, "values.json"), b, 0600)
}
