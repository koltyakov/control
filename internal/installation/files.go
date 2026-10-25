package installation

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

func writeManaged(path string, content []byte) error {
	if old, err := os.ReadFile(path); err == nil && !bytes.Contains(old, []byte("Managed by control")) {
		return errors.New("existing service file is not managed by control: " + path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return store.Bytes(path, content, 0600)
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
	info := buildinfo.Current()
	if old, err := os.Stat(dest); err == nil {
		if !old.Mode().IsRegular() {
			return "", errors.New("installation destination is not a regular file")
		}
		prior, err := update.InspectExecutable(ctx, dest)
		if err != nil {
			return "", errors.New("existing executable is not a Control installation: " + dest)
		}
		if prior.SHA256 == info.SHA256 {
			return finish()
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
	a := update.Asset{Size: stat.Size(), SHA256: info.SHA256}
	if err = update.SaveBinary(f, dest, a); err != nil {
		return "", err
	}
	return finish()
}
