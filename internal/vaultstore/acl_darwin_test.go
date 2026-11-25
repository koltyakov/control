package vaultstore

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDarwinExtendedACLs(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.base, "acl")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("/bin/chmod", "-N", dir).Run() })
	// Deny-only entries, as on macOS home directories, grant nothing.
	if out, err := exec.Command("/bin/chmod", "+a", "group:everyone deny writeattr", dir).CombinedOutput(); err != nil {
		t.Skipf("chmod +a unavailable: %v %s", err, out)
	}
	if d, err := openTrusted(dir, f.broker); err != nil {
		t.Fatalf("deny-only ACL: %v", err)
	} else {
		_ = d.Close()
	}
	if out, err := exec.Command("/bin/chmod", "+a", "group:everyone allow add_file", dir).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := openTrusted(dir, f.broker); err == nil {
		t.Fatal("allow ACL accepted")
	}
}
