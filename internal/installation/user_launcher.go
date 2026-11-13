package installation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/peimage"
	"github.com/koltyakov/control/internal/update"
)

// prepareUserLauncher derives a GUI-subsystem bootstrap from the installed CLI.
// Task Scheduler can start it without creating a console at all. It launches
// the unchanged CLI with CREATE_NO_WINDOW, so runtime selection, release hashes,
// firewall paths, and the remembered installation launcher remain unchanged.
func prepareUserLauncher(source, dataDir string) (string, error) {
	f, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > update.MaxBinarySize {
		return "", errors.New("user startup requires a regular executable within the size limit")
	}
	image, err := io.ReadAll(io.LimitReader(f, update.MaxBinarySize+1))
	if err != nil {
		return "", err
	}
	if int64(len(image)) > update.MaxBinarySize {
		return "", errors.New("user startup image exceeds the executable size limit")
	}
	if err := setWindowsGUISubsystem(image); err != nil {
		return "", err
	}
	digest := sha256.Sum256(image)
	sha := hex.EncodeToString(digest[:])
	path := filepath.Join(dataDir, "startup", "control-user-"+sha+".exe")
	asset := update.Asset{Size: int64(len(image)), SHA256: sha}
	if update.Verify(path, asset) == nil {
		return path, nil
	}
	// Content-addressed copies avoid replacing a running startup executable.
	// SaveBinary verifies the complete derived image before atomic publication.
	if err := update.SaveBinary(bytes.NewReader(image), path, asset); err != nil {
		return "", err
	}
	return path, nil
}

func setWindowsGUISubsystem(image []byte) error {
	return peimage.SetGUI(image)
}
