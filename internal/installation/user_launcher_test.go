package installation

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/update"
)

func testConsolePE(t *testing.T, machine uint16, pe32 bool) []byte {
	t.Helper()
	image := make([]byte, 64)
	copy(image, "MZ")
	binary.LittleEndian.PutUint32(image[60:], 64)
	buffer := bytes.NewBuffer(image)
	buffer.WriteString("PE\x00\x00")
	var header any = pe.OptionalHeader64{Magic: 0x20b, Subsystem: 3, NumberOfRvaAndSizes: 16}
	if pe32 {
		header = pe.OptionalHeader32{Magic: 0x10b, Subsystem: 3, NumberOfRvaAndSizes: 16}
	}
	coff := pe.FileHeader{Machine: machine, SizeOfOptionalHeader: uint16(binary.Size(header)), Characteristics: pe.IMAGE_FILE_EXECUTABLE_IMAGE}
	if err := binary.Write(buffer, binary.LittleEndian, coff); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(buffer, binary.LittleEndian, header); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestWindowsGUIStartupChangesOnlySubsystem(t *testing.T) {
	for _, tc := range []struct {
		name    string
		machine uint16
		pe32    bool
	}{
		{"amd64", pe.IMAGE_FILE_MACHINE_AMD64, false},
		{"arm64", pe.IMAGE_FILE_MACHINE_ARM64, false},
		{"PE32", pe.IMAGE_FILE_MACHINE_I386, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := testConsolePE(t, tc.machine, tc.pe32)
			before := bytes.Clone(image)
			if err := setWindowsGUISubsystem(image); err != nil {
				t.Fatal(err)
			}
			file, err := pe.NewFile(bytes.NewReader(image))
			if err != nil {
				t.Fatal(err)
			}
			var subsystem uint16
			switch header := file.OptionalHeader.(type) {
			case *pe.OptionalHeader32:
				subsystem = header.Subsystem
			case *pe.OptionalHeader64:
				subsystem = header.Subsystem
			}
			if subsystem != 2 {
				t.Fatal("startup image still has the console subsystem")
			}
			binary.LittleEndian.PutUint16(before[64+4+20+68:], 2)
			if !bytes.Equal(before, image) {
				t.Fatal("GUI startup changed executable bytes outside Subsystem")
			}
			if err := setWindowsGUISubsystem(image); err != nil || !bytes.Equal(before, image) {
				t.Fatal("GUI subsystem conversion is not repeatable", err)
			}
		})
	}
}

func TestWindowsGUIStartupRejectsInvalidExecutables(t *testing.T) {
	valid := testConsolePE(t, pe.IMAGE_FILE_MACHINE_AMD64, false)
	unsupported := bytes.Clone(valid)
	binary.LittleEndian.PutUint16(unsupported[64+4+20+68:], 1)
	invalidOffset := bytes.Clone(valid)
	binary.LittleEndian.PutUint32(invalidOffset[60:], ^uint32(0))
	invalidMagic := bytes.Clone(valid)
	binary.LittleEndian.PutUint16(invalidMagic[64+4+20:], 0)
	dll := bytes.Clone(valid)
	binary.LittleEndian.PutUint16(dll[64+22:], pe.IMAGE_FILE_EXECUTABLE_IMAGE|pe.IMAGE_FILE_DLL)
	for _, image := range [][]byte{nil, []byte("not PE"), valid[:65], unsupported, invalidOffset, invalidMagic, dll} {
		if err := setWindowsGUISubsystem(image); err == nil {
			t.Fatal("invalid startup executable was accepted")
		}
	}
}

func TestPrepareWindowsUserLauncherPreservesCLIAndRunningCopies(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "control.exe")
	original := testConsolePE(t, pe.IMAGE_FILE_MACHINE_AMD64, false)
	if err := os.WriteFile(source, original, 0700); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "user's profile with spaces")
	launcher, err := prepareUserLauncher(source, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(launcher) != filepath.Join(dataDir, "startup") || launcher == source {
		t.Fatal("startup copy did not remain separate from the CLI")
	}
	saved, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(saved, original) {
		t.Fatal("startup preparation modified the original CLI", err)
	}
	prepared, err := os.ReadFile(launcher)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, err := prepareUserLauncher(source, dataDir); err != nil || repeated != launcher {
		t.Fatal("unchanged startup did not reuse its verified copy", err)
	}
	if err := os.WriteFile(launcher, []byte("corrupt"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareUserLauncher(source, dataDir); err != nil {
		t.Fatal("corrupt startup copy could not be repaired", err)
	}
	if repaired, err := os.ReadFile(launcher); err != nil || !bytes.Equal(repaired, prepared) {
		t.Fatal("startup repair did not restore the verified bytes", err)
	}
	if err := os.WriteFile(source, append(original, 0), 0700); err != nil {
		t.Fatal(err)
	}
	newLauncher, err := prepareUserLauncher(source, dataDir)
	if err != nil || newLauncher == launcher {
		t.Fatal("changed CLI reused a potentially running startup image", err)
	}
	if previous, err := os.ReadFile(launcher); err != nil || !bytes.Equal(previous, prepared) {
		t.Fatal("new startup preparation replaced the previous copy", err)
	}
}

func TestPrepareWindowsUserLauncherBoundsSourceSize(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "oversized.exe")
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(update.MaxBinarySize + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := prepareUserLauncher(source, dir); err == nil {
		t.Fatal("oversized source was read into a startup copy")
	}
	if _, err := os.Stat(filepath.Join(dir, "startup")); !os.IsNotExist(err) {
		t.Fatal("invalid source modified startup state", err)
	}
}
