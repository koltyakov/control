package installation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/update"
)

func TestInstallSelfNeverExecutesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CONTROL_INSTALL_DIR", dir)
	dest, err := BinPath()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "executed")
	// The old installer executed this binary and trusted its self-reported
	// checksum. No elevation is needed to reproduce that execution mistake.
	source := filepath.Join(dir, "untrusted.go")
	code := fmt.Sprintf(`package main
import ("fmt"; "os")
func main() {
 if err := os.WriteFile(%q, []byte("executed"), 0600); err != nil { panic(err) }
 fmt.Print(%q)
}
`, marker, `{"sha256":"`+buildinfo.Current().SHA256+`"}`)
	if err = os.WriteFile(source, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(t.Context(), "go", "build", "-o", dest, source).CombinedOutput(); err != nil {
		t.Fatalf("build untrusted executable: %v: %s", err, output)
	}
	before, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = InstallSystemSelf(t.Context()); err == nil || !strings.Contains(err.Error(), "not a Control installation") {
		t.Fatal("unrelated installation was not rejected", err)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("installer executed its untrusted destination", err)
	}
	after, err := os.ReadFile(dest)
	if err != nil || string(before) != string(after) {
		t.Fatal("rejected destination was modified", err)
	}
}

func TestInstallSelfReusesIdenticalBytesWithoutExecution(t *testing.T) {
	t.Setenv("CONTROL_INSTALL_DIR", t.TempDir())
	dest, err := InstallSystemSelf(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = InstallSystemSelf(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(dest)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("identical installation was replaced", err)
	}
}

func TestInstallSelfReplacesOldControlWithoutExecution(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CONTROL_INSTALL_DIR", filepath.Join(dir, "installed"))
	dest, err := BinPath()
	if err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(dir, "cmd", "control")
	if err = os.MkdirAll(command, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/koltyakov/control\n"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "executed")
	code := fmt.Sprintf("package main\nimport \"os\"\nfunc main() { _ = os.WriteFile(%q, []byte(\"executed\"), 0600) }\n", marker)
	if err = os.WriteFile(filepath.Join(command, "main.go"), []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", dest, "./cmd/control")
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build older Control fixture: %v: %s", err, output)
	}
	if _, err = InstallSystemSelf(t.Context()); err != nil {
		t.Fatal("read-only build metadata did not identify the old Control binary", err)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("installer executed its old Control destination", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err = update.Verify(dest, update.Asset{Size: info.Size(), SHA256: buildinfo.Current().SHA256}); err != nil {
		t.Fatal("old destination was not replaced with the running installer", err)
	}
}
