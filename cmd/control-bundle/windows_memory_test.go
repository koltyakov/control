package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsEntropyOverlayRejectsUnreviewedSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, entropySource)
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	// A similarly named but unknown toolchain file must not be patched.
	if err := os.WriteFile(source, []byte("package drbg\nvar memory entropy.ScratchBuffer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	console := filepath.Join(t.TempDir(), "overlay.json")
	if got, err := windowsEntropyCompiler(console, root, root); err != nil || got.overlay != console || got.privateGOROOT != "" {
		t.Fatalf("unknown toolchain: compiler = %+v, error = %v", got, err)
	}
}

func TestWindowsEntropyOverlayPreservesExplicitFIPSBuild(t *testing.T) {
	t.Setenv("GOFIPS140", "latest")
	console := filepath.Join(t.TempDir(), "overlay.json")
	if got, err := windowsBuildCompiler(console); err != nil || got != (bundleCompiler{overlay: console}) {
		t.Fatalf("explicit FIPS build: compiler = %+v, error = %v", got, err)
	}
}

func TestWindowsEntropyOverlayBuild(t *testing.T) {
	stock := bundleCompiler{}
	root, err := stock.command("env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	rootDir := strings.TrimSpace(string(root))
	stock.privateGOROOT = rootDir
	dir := t.TempDir()
	input := filepath.Join(dir, "main.go")
	if err := os.WriteFile(input, []byte(`package main
import "crypto/rand"
func main() {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil { panic(err) }
	println(b[0])
}
`), 0600); err != nil {
		t.Fatal(err)
	}
	console := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(console, []byte(`{"Replace":{"console-fixture":"replacement-fixture"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Treat the selected root as the module cache. This exercises the same
	// overlay restriction as auto-downloaded Go without downloading a toolchain.
	t.Setenv("GOMODCACHE", rootDir)
	compiler, err := windowsEntropyCompiler(console, rootDir, rootDir)
	if err != nil {
		t.Fatal(err)
	}
	if compiler.overlay != console {
		if compiler.privateGOROOT == "" || withinDirectory(compiler.privateGOROOT, rootDir) {
			t.Fatal("compiler must use a private copy outside the module cache")
		}
		source := filepath.Join(compiler.privateGOROOT, entropySource)
		data, err := os.ReadFile(compiler.overlay)
		if err != nil {
			t.Fatal(err)
		}
		var config struct{ Replace map[string]string }
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		patched, err := os.ReadFile(config.Replace[source])
		if err != nil {
			t.Fatal(err)
		}
		if config.Replace["console-fixture"] != "replacement-fixture" ||
			bytes.Contains(patched, []byte("entropy.Seed(&memory)")) ||
			bytes.Count(patched, []byte("entropy.Seed(memory())")) != 2 ||
			!bytes.Contains(patched, []byte("return new(entropy.ScratchBuffer)")) {
			t.Fatal("Windows overlay did not preserve the console mappings and both entropy calls")
		}
	}
	build := func(compiler bundleCompiler, name string) string {
		t.Helper()
		output := filepath.Join(dir, name+".exe")
		// Only the static library and the fixture are built. No test binary
		// runs or opens a socket on the build host.
		args := []string{"build", "-trimpath", "-ldflags=-s -w", "-o", output}
		if compiler.overlay != "" {
			args = append(args, "-overlay", compiler.overlay)
		}
		cmd := compiler.command(append(args, input)...)
		cmd.Env = append(cmd.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0", "GOFIPS140=off", "GOFLAGS=", "GOPROXY=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build fixture: %v\n%s", err, output)
		}
		return output
	}
	if compiler.overlay != console {
		err := checkWindowsImageMemory(build(stock, "stock"))
		if err == nil || !strings.Contains(err.Error(), "zero-filled writable image data") {
			t.Fatalf("stock Go fixture should exceed the writable image budget: %v", err)
		}
	}
	if err := checkWindowsImageMemory(build(compiler, "bundle")); err != nil {
		t.Fatal(err)
	}
}
