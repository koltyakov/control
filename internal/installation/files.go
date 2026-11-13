package installation

import (
	"bytes"
	"context"
	gobuildinfo "debug/buildinfo"
	"errors"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

func writeManaged(path string, content []byte) error {
	if err := checkManaged(path); err != nil {
		return err
	}
	return store.Bytes(path, content, 0600)
}

func checkManaged(path string) error {
	if old, err := os.ReadFile(path); err == nil && !bytes.Contains(old, []byte("Managed by control")) {
		return errors.New("existing service file is not managed by control: " + path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
func InstallSelf(ctx context.Context) (string, error) {
	return installSelf(ctx, true)
}

// InstallSystemSelf installs a private service executable without adding its
// administrator-only directory to the interactive user's PATH.
func InstallSystemSelf(ctx context.Context) (string, error) {
	return installSelf(ctx, false)
}

func installSelf(ctx context.Context, userPath bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dest, err := BinPath()
	if err != nil {
		return "", err
	}
	finish := func() (string, error) {
		if userPath {
			return dest, addUserPath(filepath.Dir(dest))
		}
		return dest, nil
	}
	source, err := os.Executable()
	if err != nil {
		return "", err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return "", err
	}
	if source == dest {
		return finish()
	}
	sha, err := buildinfo.ExecutableSHA256()
	if err != nil {
		return "", err
	}
	if old, err := os.Stat(dest); err == nil {
		if !old.Mode().IsRegular() {
			return "", errors.New("installation destination is not a regular file")
		}
		// Never execute an existing installation to identify it. In particular,
		// an elevated installer must treat a per-user executable as untrusted.
		if update.Verify(dest, update.Asset{Size: old.Size(), SHA256: sha}) == nil {
			return finish()
		}
		prior, err := gobuildinfo.ReadFile(dest)
		if err != nil || prior.Path != "github.com/koltyakov/control/cmd/control" {
			return "", errors.New("existing executable is not a Control installation: " + dest)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	f, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	a := update.Asset{Size: stat.Size(), SHA256: sha}
	if err = update.SaveBinary(f, dest, a); err != nil {
		return "", err
	}
	return finish()
}
