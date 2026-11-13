package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/peimage"
	"github.com/koltyakov/control/internal/store"
)

// RuntimeBinary is the durable selection, always referring to original verified bytes.
type RuntimeBinary struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Asset   Asset  `json:"asset"`
}

// InspectRuntime validates the platform and complete executable bytes.
func InspectRuntime(ctx context.Context, path string) (RuntimeBinary, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return RuntimeBinary{}, err
	}
	info, err := InspectExecutable(ctx, path)
	if err != nil {
		return RuntimeBinary{}, err
	}
	if info.OS != buildinfo.Current().OS || info.Arch != buildinfo.Current().Arch {
		return RuntimeBinary{}, errors.New("runtime platform mismatch")
	}
	stat, err := os.Stat(path)
	if err != nil {
		return RuntimeBinary{}, err
	}
	asset := Asset{OS: info.OS, Arch: info.Arch, File: AssetName(info.OS, info.Arch), Size: stat.Size(), SHA256: info.SHA256}
	if err = Verify(path, asset); err != nil {
		return RuntimeBinary{}, err
	}
	return RuntimeBinary{Path: path, Version: info.Version, Asset: asset}, nil
}

// WindowsRuntimePath is stable across releases and covered by firewall rules.
func WindowsRuntimePath(dir string) string { return filepath.Join(dir, "runtime", "control.exe") }

// WindowsRuntimeImage verifies the source before deriving a console-free image.
// The source checksum remains the software identity; the derived checksum is
// used only to publish and verify the local launch copy.
func WindowsRuntimeImage(current RuntimeBinary, gui bool) ([]byte, Asset, error) {
	if current.Asset.Size <= 0 || current.Asset.Size > MaxBinarySize {
		return nil, Asset{}, errors.New("runtime executable exceeds the size limit")
	}
	if err := Verify(current.Path, current.Asset); err != nil {
		return nil, Asset{}, err
	}
	f, err := os.Open(current.Path)
	if err != nil {
		return nil, Asset{}, err
	}
	defer func() { _ = f.Close() }()
	image, err := io.ReadAll(io.LimitReader(f, MaxBinarySize+1))
	if err != nil {
		return nil, Asset{}, err
	}
	if int64(len(image)) > MaxBinarySize {
		return nil, Asset{}, errors.New("runtime executable exceeds the size limit")
	}
	// Verify the bytes we actually copied, not just the earlier open of the source.
	sum := sha256.Sum256(image)
	if int64(len(image)) != current.Asset.Size || hex.EncodeToString(sum[:]) != current.Asset.SHA256 {
		return nil, Asset{}, errors.New("runtime source changed during preparation")
	}
	if gui {
		if err = peimage.SetGUI(image); err != nil {
			return nil, Asset{}, err
		}
	}
	sum = sha256.Sum256(image)
	asset := current.Asset
	asset.SHA256 = hex.EncodeToString(sum[:])
	return image, asset, nil
}

// PrepareWindowsRuntime must be called under runtime.lock, after the old
// process exits. It never overwrites a running executable.
func PrepareWindowsRuntime(dir string, current RuntimeBinary, gui bool) (string, error) {
	image, asset, err := WindowsRuntimeImage(current, gui)
	if err != nil {
		return "", err
	}
	path := WindowsRuntimePath(dir)
	if Verify(path, asset) == nil {
		return path, nil
	}
	if err = SaveBinary(bytes.NewReader(image), path, asset); err != nil {
		return "", err
	}
	return path, nil
}

// SaveRuntimeSwitch retains recovery state before changing the selected binary.
func SaveRuntimeSwitch(dir string, current, next RuntimeBinary) error {
	if err := store.Write(filepath.Join(dir, "runtime-previous.json"), current); err != nil {
		return err
	}
	return store.Write(filepath.Join(dir, "runtime.json"), next)
}
