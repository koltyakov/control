package installation

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

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
	if len(image) < 64 || string(image[:2]) != "MZ" {
		return errors.New("user startup requires a Windows PE executable")
	}
	// Read just the fixed headers. Parsing unrelated symbol/string tables could
	// allocate from untrusted size fields in a damaged installed executable.
	peOffset := uint64(binary.LittleEndian.Uint32(image[60:64]))
	optional := peOffset + 4 + 20
	if optional+70 > uint64(len(image)) || string(image[peOffset:peOffset+4]) != "PE\x00\x00" {
		return errors.New("user startup image has a truncated or invalid PE header")
	}
	optionalSize := uint64(binary.LittleEndian.Uint16(image[peOffset+20 : peOffset+22]))
	minimum := uint64(0)
	switch binary.LittleEndian.Uint16(image[optional : optional+2]) {
	case 0x10b: // PE32
		minimum = 96
	case 0x20b: // PE32+
		minimum = 112
	default:
		return errors.New("user startup image has no executable optional header")
	}
	if optionalSize < minimum || optional+optionalSize > uint64(len(image)) {
		return errors.New("user startup image has a truncated optional header")
	}
	characteristics := binary.LittleEndian.Uint16(image[peOffset+22 : peOffset+24])
	if characteristics&0x0002 == 0 || characteristics&0x2000 != 0 {
		return errors.New("user startup requires an executable, not a DLL")
	}
	offset := optional + 68
	subsystem := binary.LittleEndian.Uint16(image[offset : offset+2])
	const windowsGUI, windowsConsole = 2, 3
	if subsystem != windowsConsole && subsystem != windowsGUI {
		return errors.New("user startup requires a console or GUI executable")
	}
	// Both PE32 and PE32+ place Subsystem 68 bytes into the optional header,
	// after the PE signature and 20-byte COFF header. Change only those bytes.
	binary.LittleEndian.PutUint16(image[offset:offset+2], windowsGUI)
	return nil
}
