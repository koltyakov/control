package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/store"
)

func TestWindowsRuntimePublicationAndGUISource(t *testing.T) {
	// Minimal fixed PE headers; no code is executed by these byte-copy fixtures.
	image := make([]byte, 64+24+112)
	copy(image, "MZ")
	binary.LittleEndian.PutUint32(image[60:], 64)
	copy(image[64:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(image[64+20:], 112)
	binary.LittleEndian.PutUint16(image[64+22:], 2)
	binary.LittleEndian.PutUint16(image[64+24:], 0x20b)
	binary.LittleEndian.PutUint16(image[64+24+68:], 3)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.exe")
	if err := os.WriteFile(source, image, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(image)
	current := RuntimeBinary{Path: source, Version: "fixture", Asset: Asset{Size: int64(len(image)), SHA256: hex.EncodeToString(sum[:])}}
	for _, gui := range []bool{false, true} {
		path, err := PrepareWindowsRuntime(dir, current, gui)
		if err != nil || path != WindowsRuntimePath(dir) {
			t.Fatalf("runtime publication: %q, %v", path, err)
		}
		copied, derived, err := WindowsRuntimeImage(current, gui)
		if err != nil {
			t.Fatal(err)
		}
		if err = Verify(path, derived); err != nil {
			t.Fatal(err)
		}
		if gui {
			if derived.SHA256 == current.Asset.SHA256 {
				t.Fatal("derived image kept the console checksum")
			}
			expected := bytes.Clone(image)
			binary.LittleEndian.PutUint16(expected[64+24+68:], 2)
			if !bytes.Equal(expected, copied) {
				t.Fatal("GUI derivation changed bytes outside Subsystem")
			}
		}
		if original, err := os.ReadFile(source); err != nil || !bytes.Equal(original, image) {
			t.Fatal("runtime preparation changed original source", err)
		}
		if repeated, err := PrepareWindowsRuntime(dir, current, gui); err != nil || repeated != path {
			t.Fatal("unchanged runtime not reused", err)
		}
	}
	before, err := os.ReadFile(WindowsRuntimePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	image[len(image)-1] ^= 1
	if err = os.WriteFile(source, image, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = PrepareWindowsRuntime(dir, current, true); err == nil {
		t.Fatal("corrupt source replaced runtime")
	}
	if after, err := os.ReadFile(WindowsRuntimePath(dir)); err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed preparation changed runtime", err)
	}
	current.Asset.Size = MaxBinarySize + 1
	if _, _, err = WindowsRuntimeImage(current, true); err == nil {
		t.Fatal("oversized runtime accepted")
	}
}

func TestSaveRuntimeSwitchRetainsPreviousSelection(t *testing.T) {
	dir := t.TempDir()
	current := RuntimeBinary{Path: "original", Version: "old"}
	next := RuntimeBinary{Path: "verified", Version: "new"}
	if err := SaveRuntimeSwitch(dir, current, next); err != nil {
		t.Fatal(err)
	}
	var saved, previous RuntimeBinary
	if err := store.Read(filepath.Join(dir, "runtime.json"), &saved); err != nil {
		t.Fatal(err)
	}
	if err := store.Read(filepath.Join(dir, "runtime-previous.json"), &previous); err != nil {
		t.Fatal(err)
	}
	if saved != next || previous != current {
		t.Fatalf("invalid runtime switch: %+v, %+v", saved, previous)
	}
}
