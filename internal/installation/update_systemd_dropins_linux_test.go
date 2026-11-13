package installation

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacySystemdAcceptsOnlyInertVendorDropInContent(t *testing.T) {
	for _, content := range []string{
		"# Vendor default\n[Service]\nTimeoutStopFailureMode=abort\n",
		"; comment\r\n [Service] \r\n TimeoutStopFailureMode = abort \r\n",
		"# no configuration\n",
	} {
		if !safeLegacySystemdDropIn([]byte(content)) {
			t.Fatalf("inert vendor override rejected: %q", content)
		}
	}
	for _, directive := range []string{
		"ExecStart=/foreign/program", "ExecStart=", "ExecStartPre=/foreign/program", "ExecStop=/foreign/program",
		"User=other", "Group=other", "Environment=EXTRA=value", "Restart=always", "WorkingDirectory=/other",
		"TimeoutStopFailureMode=kill", "TimeoutStopFailureMode=abort\\\nExecStart=/foreign/program",
		"[Unit]\nOnFailure=other.service", ".include /other.conf",
	} {
		if safeLegacySystemdDropIn([]byte("[Service]\n" + directive + "\n")) {
			t.Fatalf("unsafe vendor override accepted: %q", directive)
		}
	}
	if safeLegacySystemdDropIn([]byte("TimeoutStopFailureMode=abort\n")) {
		t.Fatal("directive outside Service section accepted")
	}
}

func TestLegacySystemdVendorPathAndOwnership(t *testing.T) {
	for _, scope := range []string{"system", "user"} {
		path := "/usr/lib/systemd/" + scope + "/service.d/10-timeout-abort.conf"
		if !legacySystemdVendorPath(path, scope) {
			t.Fatal("standard vendor path rejected")
		}
	}
	for _, path := range []string{
		"/etc/systemd/system/service.d/override.conf", "/usr/lib/systemd/system/control.service.d/override.conf",
		"/usr/lib/systemd/system/service.d/../service.d/override.conf", "/home/user/override.conf",
		"/usr/lib/systemd/user/service.d/override.conf", "/usr/lib/systemd/system/service.d/override.txt",
	} {
		if legacySystemdVendorPath(path, "system") {
			t.Fatalf("foreign/custom path accepted: %s", path)
		}
	}
	if !protectedSystemdDropIn(0644, 0) || protectedSystemdDropIn(0644, 1000) ||
		protectedSystemdDropIn(0664, 0) || protectedSystemdDropIn(0666, 0) ||
		protectedSystemdDropIn(os.ModeDir|0755, 0) || protectedSystemdDropIn(os.ModeSymlink|0644, 0) {
		t.Fatal("root-owned regular-file permission boundary changed")
	}
}

func TestLegacySystemdMatchingRequiresEveryDropInValidated(t *testing.T) {
	path := "/usr/lib/systemd/system/service.d/10-timeout-abort.conf"
	unit := "/etc/systemd/system/control.service"
	properties := map[string]string{"MainPID": "123", "ActiveState": "active", "FragmentPath": unit,
		"DropInPaths": path, "NeedDaemonReload": "no", "Restart": "on-failure", "ControlGroup": "/system.slice/control.service"}
	group := "0::/system.slice/control.service\n"
	validated := map[string][32]byte{path: sha256.Sum256([]byte("[Service]\nTimeoutStopFailureMode=abort\n"))}
	if !matchesLegacySystemd(properties, unit, 123, group, validated) {
		t.Fatal("validated vendor timeout drop-in rejected")
	}
	if matchesLegacySystemd(properties, unit, 123, group, nil) {
		t.Fatal("unvalidated drop-in accepted")
	}
	properties["DropInPaths"] += " /etc/systemd/system/control.service.d/override.conf"
	if matchesLegacySystemd(properties, unit, 123, group, validated) {
		t.Fatal("additional unvalidated drop-in accepted")
	}
	properties["DropInPaths"] = ""
	if matchesLegacySystemd(properties, unit, 123, group, validated) {
		t.Fatal("loaded drop-in change ignored")
	}
}

func TestLegacySystemdDropInReadsAreBoundedAndNoFollow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "override.conf")
	content := []byte("[Service]\nTimeoutStopFailureMode=abort\n")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	data, err := readLegacySystemdDropIn(path)
	if os.Getuid() == 0 {
		if err != nil || !bytes.Equal(data, content) {
			t.Fatalf("root-owned file rejected: %v", err)
		}
	} else if err == nil {
		t.Fatal("non-root-owned file accepted")
	}
	link := filepath.Join(dir, "link.conf")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readLegacySystemdDropIn(link); err == nil {
		t.Fatal("symlink vendor file followed")
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{'#'}, (16<<10)+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLegacySystemdDropIn(path); err == nil {
		t.Fatal("oversized drop-in accepted")
	}
	if _, err := readLegacySystemdDropIn(dir); err == nil {
		t.Fatal("directory accepted as a vendor file")
	}
}

// Opt-in native verification for the distribution files that exposed this bug.
// Ordinary core tests remain self-contained and need no installed systemd files.
func TestLegacySystemdVendorDropInsNative(t *testing.T) {
	if os.Getenv("CONTROL_SYSTEMD_VENDOR_SMOKE") != "1" {
		t.Skip("set CONTROL_SYSTEMD_VENDOR_SMOKE=1 on a host with the vendor timeout override")
	}
	for _, scope := range []string{"user", "system"} {
		path := "/usr/lib/systemd/" + scope + "/service.d/10-timeout-abort.conf"
		validated, err := legacySystemdDropIns(path, scope)
		if err != nil || len(validated) != 1 {
			t.Fatalf("native %s vendor override rejected: %v", scope, err)
		}
		data, err := os.ReadFile(path)
		if err != nil || validated[path] != sha256.Sum256(data) {
			t.Fatal("native vendor override identity was not retained", err)
		}
	}
}
