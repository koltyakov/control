package main

import (
	"bytes"
	"crypto/sha256"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// Go 1.27.4, src/crypto/internal/fips140/drbg/entropy_fips140.go.
	stockEntropySHA256 = "bd3c834a29e31c93e56d81da54d4a874561814a86550de1456c8d6d40a9c5fda"
	maxWindowsZeroData = 16 << 20
)

// Work around https://go.dev/issue/81956 until the fix ships in Go. In a
// standard Windows bundle, allocate the same entropy buffer once on first use
// instead of committing it for every process. Leave opted-in toolchains alone.
func windowsBuildCompiler(console string) (bundleCompiler, error) {
	data, err := exec.Command("go", "env", "-json", "GOROOT", "GOMODCACHE", "GOFIPS140").Output()
	if err != nil {
		return bundleCompiler{}, fmt.Errorf("resolve Windows build toolchain: %w", err)
	}
	var env struct{ GOROOT, GOMODCACHE, GOFIPS140 string }
	if err := json.Unmarshal(data, &env); err != nil {
		return bundleCompiler{}, err
	}
	if env.GOROOT == "" || env.GOFIPS140 == "" {
		return bundleCompiler{}, errors.New("build toolchain did not report GOROOT and GOFIPS140 for Windows build")
	}
	if env.GOFIPS140 != "off" {
		return bundleCompiler{overlay: console}, nil
	}
	return windowsEntropyCompiler(console, env.GOROOT, env.GOMODCACHE)
}

const entropySource = "src/crypto/internal/fips140/drbg/entropy_fips140.go"

func windowsEntropyCompiler(console, root, cache string) (bundleCompiler, error) {
	compiler := bundleCompiler{overlay: console, checkWindowsMemory: true}
	data, err := os.ReadFile(filepath.Join(root, entropySource))
	if errors.Is(err, os.ErrNotExist) {
		return compiler, nil // A newer toolchain may not have this file.
	}
	if err != nil {
		return bundleCompiler{}, err
	}
	data = deferEntropyBuffer(data)
	if data == nil {
		return compiler, nil // Never rewrite a different toolchain's crypto source.
	}
	dir, err := filepath.Abs(filepath.Dir(console))
	if err != nil {
		return bundleCompiler{}, err
	}
	if withinDirectory(root, cache) {
		// Go rejects overlay targets under GOMODCACHE, including auto-selected
		// toolchains. Copy the selected distribution once into the build's
		// private workspace; leave the original and module cache untouched.
		if withinDirectory(dir, cache) {
			return bundleCompiler{}, errors.New("temporary build directory is inside GOMODCACHE; set TMPDIR (TEMP on Windows) outside the module cache")
		}
		compiler.privateGOROOT = filepath.Join(dir, "go")
		if err := os.CopyFS(compiler.privateGOROOT, os.DirFS(root)); err != nil {
			return bundleCompiler{}, fmt.Errorf("copy cached Go toolchain: %w", err)
		}
		root = compiler.privateGOROOT
	}
	compiler.overlay, err = entropyOverlay(console, dir, filepath.Join(root, entropySource), data)
	return compiler, err
}

func withinDirectory(path, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func entropyOverlay(console, dir, source string, data []byte) (string, error) {
	patched := filepath.Join(dir, "entropy_fips140.go")
	if err := os.WriteFile(patched, data, 0600); err != nil {
		return "", err
	}
	configData, err := os.ReadFile(console)
	if err != nil {
		return "", err
	}
	var config struct{ Replace map[string]string }
	if err := json.Unmarshal(configData, &config); err != nil {
		return "", err
	}
	if config.Replace == nil {
		return "", errors.New("console overlay has no replacement map")
	}
	config.Replace[source] = patched
	configData, err = json.Marshal(config)
	if err != nil {
		return "", err
	}
	overlay := filepath.Join(dir, "windows-overlay.json")
	return overlay, os.WriteFile(overlay, configData, 0600)
}

func deferEntropyBuffer(data []byte) []byte {
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != stockEntropySHA256 {
		return nil
	}
	data = bytes.ReplaceAll(data, []byte(`import entropy "crypto/internal/entropy/v1.0.0"`), []byte(`import (
	entropy "crypto/internal/entropy/v1.0.0"
	"sync"
)`))
	data = bytes.ReplaceAll(data, []byte("var memory entropy.ScratchBuffer"), []byte(`var memory = sync.OnceValue(func() *entropy.ScratchBuffer {
	return new(entropy.ScratchBuffer)
})`))
	return bytes.ReplaceAll(data, []byte("entropy.Seed(&memory)"), []byte("entropy.Seed(memory())"))
}

func checkWindowsImageMemory(path string) error {
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("inspect Windows build: %w", err)
	}
	defer func() { _ = f.Close() }()
	if f.Section(".data") == nil {
		return errors.New("missing .data section in Windows executable")
	}
	var zeroData uint64
	for _, section := range f.Sections {
		if section.Characteristics&pe.IMAGE_SCN_MEM_WRITE != 0 && section.VirtualSize > section.Size {
			zeroData += uint64(section.VirtualSize - section.Size)
		}
	}
	if zeroData > maxWindowsZeroData {
		return fmt.Errorf("zero-filled writable image data in Windows executable is %d MiB, limit %d MiB; check Go issue #81956 and the Windows build overlay",
			zeroData>>20, maxWindowsZeroData>>20)
	}
	return nil
}
