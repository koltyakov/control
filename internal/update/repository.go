package update

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/store"
)

type Repository struct {
	dir     string
	mu      sync.Mutex
	current *Deployment
	pins    map[string]int
}

func OpenRepository(dir string) (*Repository, error) {
	r := &Repository{dir: dir, pins: map[string]int{}}
	if err := store.Read(filepath.Join(dir, "deployment.json"), &r.current); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return r, nil
}

func (r *Repository) Blob(hash string) string { return filepath.Join(r.dir, "blobs", hash) }

func (r *Repository) Upload(reader io.Reader, asset Asset) error {
	if !ValidDigest(asset.SHA256) || asset.Size <= 0 || asset.Size > MaxBinarySize {
		return errors.New("invalid binary metadata")
	}
	release := r.Pin([]Asset{asset})
	defer release()
	return SaveBinary(reader, r.Blob(asset.SHA256), asset)
}

// Pin keeps assets available while a release is downloaded and published.
func (r *Repository) Pin(assets []Asset) func() {
	// Retain our own list: callers may reuse their manifest after Pin returns.
	assets = append([]Asset(nil), assets...)
	r.mu.Lock()
	for _, asset := range assets {
		r.pins[asset.SHA256]++
	}
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			for _, asset := range assets {
				r.pins[asset.SHA256]--
				if r.pins[asset.SHA256] == 0 {
					delete(r.pins, asset.SHA256)
				}
			}
		})
	}
}

// VerifyPinned hashes binaries without holding the repository mutex. The
// returned release function keeps cleanup from deleting them until their
// durable reference is published. Pins do not authorize unverified bytes.
func (r *Repository) VerifyPinned(assets []Asset) (func(), error) {
	for _, asset := range assets {
		if !ValidDigest(asset.SHA256) || asset.Size <= 0 || asset.Size > MaxBinarySize {
			return nil, errors.New("invalid binary metadata")
		}
	}
	release := r.Pin(assets)
	verified := map[Asset]bool{}
	for _, asset := range assets {
		if verified[asset] {
			continue
		}
		if err := Verify(r.Blob(asset.SHA256), asset); err != nil {
			release()
			return nil, err
		}
		verified[asset] = true
	}
	return release, nil
}

func (r *Repository) Current() *Deployment {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		return nil
	}
	copy := *r.current
	copy.Manifest.Assets = append([]Asset(nil), copy.Manifest.Assets...)
	copy.Participants = append([]string(nil), copy.Participants...)
	return &copy
}

func (r *Repository) Publish(manifest Manifest, source string) (*Deployment, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	release, err := r.VerifyPinned(manifest.Assets)
	if err != nil {
		return nil, err
	}
	defer release()
	r.mu.Lock()
	defer r.mu.Unlock()
	id := manifest.ID()
	if r.current != nil && (r.current.ID == id || r.current.Manifest.Version == manifest.Version) {
		copy := *r.current
		return &copy, nil
	}
	if r.current != nil && r.current.Phase == "installing" {
		return nil, errors.New("another update is installing; wait for it to complete")
	}
	d := &Deployment{ID: id, Manifest: manifest, Source: source, Phase: "staging", UpdatedAt: time.Now().UTC()}
	if err := store.Write(filepath.Join(r.dir, "deployment.json"), d); err != nil {
		return nil, err
	}
	r.current = d
	copy := *d
	return &copy, nil
}

func (r *Repository) SetPhase(id, phase string, cause error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil || r.current.ID != id {
		return errors.New("update superseded")
	}
	copy := *r.current
	copy.Phase, copy.Error, copy.UpdatedAt = phase, ShortError(cause), time.Now().UTC()
	if err := store.Write(filepath.Join(r.dir, "deployment.json"), &copy); err != nil {
		return err
	}
	r.current = &copy
	return nil
}

func (r *Repository) BeginInstall(id string, participants []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil || r.current.ID != id {
		return errors.New("update superseded")
	}
	copy := *r.current
	copy.Phase, copy.Error, copy.UpdatedAt = "installing", "", time.Now().UTC()
	copy.Participants = append([]string(nil), participants...)
	if err := store.Write(filepath.Join(r.dir, "deployment.json"), &copy); err != nil {
		return err
	}
	r.current = &copy
	return nil
}
