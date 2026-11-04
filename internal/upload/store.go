package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/store"
	"github.com/shirou/gopsutil/v4/disk"
)

type Limits struct {
	StagingBytes int64
	OwnerBytes   int64
	ReserveBytes uint64
	Retention    time.Duration
}

func (l Limits) Defaults() (Limits, error) {
	if l.StagingBytes == 0 {
		l.StagingBytes = 16 << 30
	}
	if l.OwnerBytes == 0 {
		l.OwnerBytes = 4 << 30
	}
	if l.ReserveBytes == 0 {
		l.ReserveBytes = 256 << 20
	}
	if l.Retention == 0 {
		l.Retention = 7 * 24 * time.Hour
	}
	if l.StagingBytes < 0 || l.OwnerBytes < 0 || l.Retention < time.Hour || l.Retention > 87600*time.Hour {
		return l, errors.New("upload byte limits must be positive and retention must be 1..87600 hours")
	}
	return l, nil
}

type record struct {
	Result
	Owner    string `json:"owner"`
	Temp     string `json:"temp"`
	Cleaned  bool   `json:"cleaned"`
	Checksum string `json:"checksum"`
}

func (r record) checksum() string {
	r.Checksum = ""
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type entry struct {
	record  record
	busy    bool
	durable bool // A metadata write was attempted; uncertainty pins the ID.
}

// Manager owns bounded metadata and reservations; individual sessions own
// files and locks. Files are staged beside their destination, not in dataDir.
type Manager struct {
	mu         sync.Mutex
	root       *os.Root
	dir        string
	privateDir string
	workDir    string
	nodeID     string
	limits     Limits
	entries    map[string]*entry
	active     int
	owners     map[string]int
}

func key(owner, id string) string {
	h := sha256.Sum256([]byte(owner + "\x00" + id))
	return hex.EncodeToString(h[:])
}

func New(root *os.Root, workDir, dataDir, nodeID string, limits Limits) (*Manager, error) {
	l, err := limits.Defaults()
	if err != nil {
		return nil, err
	}
	m := &Manager{root: root, workDir: workDir, dir: filepath.Join(dataDir, "uploads"), nodeID: nodeID, limits: l, entries: map[string]*entry{}, owners: map[string]int{}}
	if err = os.MkdirAll(m.dir, 0700); err != nil {
		return nil, err
	}
	m.privateDir = resolveExisting(m.dir)
	dir, err := os.Open(m.dir)
	if err != nil {
		return nil, err
	}
	entries, err := dir.ReadDir(4097)
	closeErr := dir.Close()
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > 4096 {
		return nil, errors.New("upload metadata directory exceeds 4096 entries; preserve it for recovery")
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".write-") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if !digest.MatchString(name) || !strings.HasSuffix(e.Name(), ".json") || !e.Type().IsRegular() {
			return nil, errors.New("invalid upload metadata entry")
		}
		f, err := os.Open(filepath.Join(m.dir, e.Name()))
		if err != nil {
			return nil, err
		}
		b, readErr := io.ReadAll(io.LimitReader(f, MetadataLimit+1))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}
		var r record
		if len(b) > MetadataLimit || json.Unmarshal(b, &r) != nil || r.Validate() != nil || r.Owner == "" || key(r.Owner, r.ID) != name || r.NodeID != nodeID || r.Temp != path.Join(StagingDir, name+".partial") || r.Checksum != r.checksum() || r.Offset < 0 || r.Offset > r.Size || !digest.MatchString(r.PrefixSHA256) || r.Updated.IsZero() || !validState(r.State) {
			return nil, fmt.Errorf("corrupt upload metadata %s; preserve it for recovery", e.Name())
		}
		m.entries[name] = &entry{record: r, durable: true}
		if len(m.entries) > 4096 {
			return nil, errors.New("upload record limit exceeded")
		}
	}
	return m, nil
}

func validState(s string) bool {
	return s == "staging" || s == "publishing" || s == "published" || s == "aborted" || s == "expired"
}

// save publishes a metadata snapshot only after the durable write succeeds.
// The upload must be claimed, but the manager mutex is never held across IO.
func (m *Manager) save(r record) error {
	r.Active = false
	r.Checksum = r.checksum()
	m.mu.Lock()
	m.entries[key(r.Owner, r.ID)].durable = true
	m.mu.Unlock()
	if err := store.Write(filepath.Join(m.dir, key(r.Owner, r.ID)+".json"), r); err != nil {
		return err
	}
	m.mu.Lock()
	m.entries[key(r.Owner, r.ID)].record = r
	m.mu.Unlock()
	return nil
}

func (m *Manager) claim(owner, id string) (record, func(), error) {
	if !ValidID(id) {
		return record{}, nil, Error("invalid_upload", "invalid upload ID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[key(owner, id)]
	if e == nil {
		return record{}, nil, Error("upload_not_found", "upload is unknown or its history expired; do not infer that publication did not occur")
	}
	if e.busy {
		return record{}, nil, Error("upload_busy", "upload is active; inspect status before resuming or aborting")
	}
	if m.active >= 8 || m.owners[owner] >= 2 {
		return record{}, nil, Error("upload_limit", "active upload limit reached")
	}
	e.busy = true
	m.active++
	m.owners[owner]++
	return e.record, func() {
		m.mu.Lock()
		e.busy = false
		m.active--
		m.owners[owner]--
		if m.owners[owner] == 0 {
			delete(m.owners, owner)
		}
		m.mu.Unlock()
	}, nil
}

func (m *Manager) Status(ctx context.Context, owner, id string) (Result, error) {
	if !ValidID(id) {
		return Result{}, Error("invalid_upload", "invalid upload ID")
	}
	m.mu.Lock()
	e := m.entries[key(owner, id)]
	if e == nil {
		m.mu.Unlock()
		return Result{}, Error("upload_not_found", "upload is unknown or its history expired")
	}
	r := e.record.Result
	r.Active = e.busy
	m.mu.Unlock()
	if r.Active || r.State != "publishing" {
		return r, nil
	}
	rec, release, err := m.claim(owner, id)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err = m.reconcile(ctx, &rec); err != nil {
		return rec.Result, err
	}
	return rec.Result, nil
}

func (m *Manager) parent(r record) (*os.Root, error) {
	return m.root.OpenRoot(filepath.FromSlash(path.Dir(r.Path)))
}

func checkStage(parent *os.Root, r record) (os.FileInfo, error) {
	info, err := parent.Lstat(filepath.FromSlash(r.Temp))
	if err == nil && !info.Mode().IsRegular() {
		return nil, Error("partial_corrupt", "staging entry is not a regular file")
	}
	return info, err
}

// reconcile never creates a destination. A retained staging hard link proves
// which inode was published, rather than guessing from matching contents.
func (m *Manager) reconcile(ctx context.Context, r *record) error {
	if r.State != "publishing" {
		return nil
	}
	parent, err := m.parent(*r)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	stage, err := checkStage(parent, *r)
	if err != nil {
		return Error("publication_unresolved", "publication staging identity is unavailable")
	}
	final, err := parent.Lstat(path.Base(r.Path))
	if errors.Is(err, os.ErrNotExist) {
		r.State = "staging"
		return m.save(*r)
	}
	if err != nil || !final.Mode().IsRegular() || !os.SameFile(stage, final) {
		return Error("publication_unresolved", "destination does not match the publication intent; no publication was replayed")
	}
	f, err := parent.Open(filepath.FromSlash(r.Temp))
	if err != nil {
		return err
	}
	sum, err := Hash(ctx, f, r.Size)
	_ = f.Close()
	if err != nil || stage.Size() != r.Size || sum != r.SHA256 {
		return Error("publication_unresolved", "published content no longer matches the verified upload")
	}
	if err = syncParent(parent); err != nil {
		return err
	}
	r.State = "published"
	r.Updated = time.Now().UTC()
	if err = m.save(*r); err != nil {
		return err
	}
	_ = m.cleanup(r)
	return nil
}

func (m *Manager) Abort(ctx context.Context, owner, id string) (Result, error) {
	r, release, err := m.claim(owner, id)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err = m.reconcile(ctx, &r); err != nil {
		return r.Result, err
	}
	if r.State == "staging" {
		r.State = "aborted"
		r.Updated = time.Now().UTC()
		if err = m.save(r); err != nil {
			return r.Result, err
		}
	}
	err = m.cleanup(&r)
	return r.Result, err
}

func (m *Manager) cleanup(r *record) error {
	if r.Cleaned {
		return nil
	}
	parent, err := m.parent(*r)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	_, err = checkStage(parent, *r)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err = parent.Remove(filepath.FromSlash(r.Temp)); err != nil {
			return err
		}
	}
	if err = syncStage(parent); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	r.Cleaned = true
	return m.save(*r)
}

// Prune is called at startup and periodically. Busy records and unresolved
// publication intents remain pinned. Failed deletions remain charged.
func (m *Manager) Prune(ctx context.Context, now time.Time) error {
	m.mu.Lock()
	var ids [][2]string
	for _, e := range m.entries {
		if !e.busy {
			ids = append(ids, [2]string{e.record.Owner, e.record.ID})
		}
	}
	m.mu.Unlock()
	var failures []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		r, release, err := m.claim(id[0], id[1])
		if err != nil {
			continue
		}
		err = m.pruneOne(ctx, &r, now)
		release()
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) pruneOne(ctx context.Context, r *record, now time.Time) error {
	if err := m.reconcile(ctx, r); err != nil {
		return err
	}
	if r.State == "staging" && now.Sub(r.Updated) > m.limits.Retention {
		r.State = "expired"
		r.Updated = now.UTC()
		if err := m.save(*r); err != nil {
			return err
		}
	}
	if r.State == "staging" {
		return nil
	}
	if err := m.cleanup(r); err != nil {
		return err
	}
	if now.Sub(r.Updated) <= m.limits.Retention {
		return nil
	}
	if err := os.Remove(filepath.Join(m.dir, key(r.Owner, r.ID)+".json")); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.entries, key(r.Owner, r.ID))
	m.mu.Unlock()
	return nil
}

func (m *Manager) reserve(owner string, s Spec, resume bool) (record, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active >= 8 || m.owners[owner] >= 2 {
		return record{}, nil, Error("upload_limit", "active upload limit reached")
	}
	k := key(owner, s.ID)
	e := m.entries[k]
	if e != nil {
		if e.record.Spec != s {
			return record{}, nil, Error("upload_spec_conflict", "upload ID already binds a different destination or content")
		}
		if e.busy {
			return record{}, nil, Error("upload_busy", "upload already has an active writer")
		}
		if !resume && e.record.State == "staging" {
			return record{}, nil, Error("upload_exists", "partial upload exists; resume it explicitly")
		}
	} else {
		if resume {
			return record{}, nil, Error("upload_not_found", "resume requires an existing upload")
		}
		var total, owned int64
		var ownerRecords, partials, ownerPartials int
		for _, other := range m.entries {
			r := other.record
			if r.Owner == owner {
				ownerRecords++
			}
			if !r.Cleaned {
				total += r.Size
				partials++
				if r.Owner == owner {
					owned += r.Size
					ownerPartials++
				}
			}
		}
		if len(m.entries) >= 4096 || ownerRecords >= 1024 || partials >= 256 || ownerPartials >= 64 || s.Size > m.limits.StagingBytes-total || s.Size > m.limits.OwnerBytes-owned {
			return record{}, nil, Error("upload_storage_exhausted", "upload staging byte or record budget exhausted")
		}
		empty := sha256.Sum256(nil)
		e = &entry{record: record{Result: Result{Spec: s, NodeID: m.nodeID, State: "staging", PrefixSHA256: hex.EncodeToString(empty[:]), Updated: time.Now().UTC()}, Owner: owner, Temp: path.Join(StagingDir, k+".partial")}}
		m.entries[k] = e
	}
	e.busy = true
	m.active++
	m.owners[owner]++
	return e.record, func() {
		m.mu.Lock()
		e.busy = false
		if !e.durable {
			delete(m.entries, k)
		}
		m.active--
		m.owners[owner]--
		if m.owners[owner] == 0 {
			delete(m.owners, owner)
		}
		m.mu.Unlock()
	}, nil
}

func (m *Manager) space(r record, bytes int64) error {
	for _, dir := range []string{filepath.Join(m.workDir, filepath.FromSlash(path.Dir(r.Path))), m.dir} {
		usage, err := disk.Usage(dir)
		if err != nil {
			return err
		}
		if usage.Free < m.limits.ReserveBytes || uint64(bytes) > usage.Free-m.limits.ReserveBytes {
			return Error("upload_storage_exhausted", "upload would consume the filesystem free-space reserve")
		}
	}
	return nil
}
