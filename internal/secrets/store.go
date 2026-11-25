// Package secrets stores worker-local credentials. Values have no network read API.
package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/pathutil"
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
// Policy is absent for unrestricted secrets. PolicyInvalid marks a stored
// policy that cannot be read; such a secret is refused for every use.
type Metadata struct {
	Name          string    `json:"name"`
	CreatedAt     time.Time `json:"createdAt,omitzero"`
	UpdatedAt     time.Time `json:"updatedAt,omitzero"`
	Policy        *Policy   `json:"policy,omitempty"`
	PolicyInvalid bool      `json:"policyInvalid,omitempty"`
}

type record struct {
	Value     string    `json:"value"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	// Policy is kept verbatim, so rewriting the store for another secret
	// never drops or repairs an unreadable policy.
	Policy json.RawMessage `json:"policy,omitempty"`
	policy *Policy
	// invalid means a stored policy exists but fails validation: fail closed.
	invalid bool
}

// formatKey marks a store containing use policies. It is not a valid secret
// name, so binaries that predate policies refuse the whole store instead of
// silently treating restricted secrets as unrestricted.
const formatKey = "#format"

const policyFormat = "2"

func (s Store) check() error {
	dir, err := pathutil.Resolve(s.Dir)
	if err != nil {
		return err
	}
	work, err := pathutil.Resolve(s.WorkDir)
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

// Snapshot returns every value, ignoring use policies, for destination-side
// redaction and validation only, never RPC results. Values for injection or
// sending must come from Resolver.Resolve.
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
	if err != nil || len(b) > 1<<20 || json.Unmarshal(b, &entries) != nil || entries == nil {
		return nil, errors.New("invalid secret storage")
	}
	if format, ok := entries[formatKey]; ok {
		// An unknown future format must not be read as an older one.
		if string(format) != policyFormat {
			return nil, errors.New("unsupported secret storage format; upgrade this Control binary")
		}
		delete(entries, formatKey)
	}
	if len(entries) > 128 {
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
		if len(entry.Policy) != 0 {
			// A present but unusable policy, including an explicit null,
			// refuses every use of this secret. It never means unrestricted.
			entry.policy, err = DecodePolicy(entry.Policy)
			entry.invalid = err != nil
		}
		records[name] = entry
	}
	return records, nil
}

func validValue(value string) bool {
	return value != "" && len(value) <= MaxValue && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// Set replaces a value and keeps any existing use policy unchanged.
func (s Store) Set(ctx context.Context, name, value string) error {
	return s.set(ctx, name, value, false, nil)
}

// SetWithPolicy replaces a value and its use policy in one atomic write. A nil
// policy explicitly makes the secret unrestricted.
func (s Store) SetWithPolicy(ctx context.Context, name, value string, policy *Policy) error {
	return s.set(ctx, name, value, true, policy)
}

func (s Store) set(ctx context.Context, name, value string, replacePolicy bool, policy *Policy) error {
	if !ValidName(name) || !validValue(value) {
		return errors.New("secret requires a valid name and 1..4096 UTF-8 bytes without NUL")
	}
	raw, err := encodePolicy(policy)
	if err != nil {
		return err
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
		if replacePolicy {
			entry.Policy = raw
		}
		records[name] = entry
		return nil
	})
}

// SetPolicy changes only the use policy of an existing secret, without its
// value or timestamps, which describe credential entry. Nil removes the policy.
func (s Store) SetPolicy(ctx context.Context, name string, policy *Policy) error {
	if !ValidName(name) {
		return errors.New("invalid secret name")
	}
	raw, err := encodePolicy(policy)
	if err != nil {
		return err
	}
	return s.change(ctx, func(records map[string]record) error {
		entry, exists := records[name]
		if !exists {
			return errors.New("secret is not configured; set its value first")
		}
		entry.Policy = raw
		records[name] = entry
		return nil
	})
}

func encodePolicy(policy *Policy) (json.RawMessage, error) {
	if policy == nil {
		return nil, nil
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(policy)
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
		entries = append(entries, Metadata{Name: name, CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt, Policy: entry.policy.Clone(), PolicyInvalid: entry.invalid})
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
	entries := make(map[string]any, len(records)+1)
	for name, entry := range records {
		entries[name] = entry
		if len(entry.Policy) != 0 {
			// Stores without policies keep the legacy byte format.
			entries[formatKey] = json.RawMessage(policyFormat)
		}
	}
	b, err := json.Marshal(entries)
	if err != nil || len(b) > 1<<20 {
		return errors.New("secret storage exceeds 1 MiB")
	}
	return store.Bytes(filepath.Join(s.Dir, "values.json"), b, 0600)
}

// Reference is one secret name an invocation wants to consume. HTTP uses carry
// the URL the worker will connect to.
type Reference struct {
	Name string
	Use  Use
	URL  *url.URL
}

// Resolver is one consistent read of a store. Masking returns every value for
// redaction only; Resolve is the single way to obtain a value for use.
type Resolver struct{ records map[string]record }

// Resolver reads the store once for one invocation.
func (s Store) Resolver() (*Resolver, error) {
	records, err := s.read()
	if err != nil {
		return nil, err
	}
	return &Resolver{records: records}, nil
}

// Masking returns all configured values, including refused ones, for output
// redaction. Never send or inject these values.
func (r *Resolver) Masking() map[string]string {
	values := make(map[string]string, len(r.records))
	for name, entry := range r.records {
		values[name] = entry.Value
	}
	return values
}

// Resolve checks every reference against its secret's use policy before it
// returns any value, so one refused reference fails the whole invocation with
// nothing sent. Errors name the secret, never its value, and describe a
// refusal without the policy's contents.
func (r *Resolver) Resolve(refs []Reference) (map[string]string, error) {
	ordered := slices.Clone(refs)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Name != ordered[j].Name {
			return ordered[i].Name < ordered[j].Name
		}
		return ordered[i].Use < ordered[j].Use
	})
	values := make(map[string]string, len(ordered))
	for _, ref := range ordered {
		if !ValidName(ref.Name) {
			return nil, errors.New("invalid secret reference")
		}
		if !slices.Contains(useOrder, ref.Use) {
			return nil, errors.New("invalid secret use")
		}
		entry, ok := r.records[ref.Name]
		if !ok {
			return nil, errors.New("referenced secret is not configured on this worker")
		}
		switch {
		case entry.invalid:
			return nil, fmt.Errorf("secret %q has an invalid use policy on this worker and is refused for every use until its policy is set again or cleared", ref.Name)
		case entry.policy == nil:
		case !slices.Contains(entry.policy.Uses, ref.Use):
			return nil, fmt.Errorf("secret %q is not permitted for %s use by its policy on this worker", ref.Name, ref.Use)
		case !entry.policy.permits(ref.Use, ref.URL):
			return nil, fmt.Errorf("secret %q is not permitted for this HTTP destination by its policy on this worker; nothing was sent", ref.Name)
		}
		values[ref.Name] = entry.Value
	}
	return values, nil
}
