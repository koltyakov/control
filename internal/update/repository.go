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
}

func OpenRepository(dir string) (*Repository, error) {
	r := &Repository{dir: dir}
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
	return SaveBinary(reader, r.Blob(asset.SHA256), asset)
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
	for _, asset := range manifest.Assets {
		if err := Verify(r.Blob(asset.SHA256), asset); err != nil {
			return nil, err
		}
	}
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
