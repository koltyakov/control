package installation

import (
	"context"
	"errors"
	"maps"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLegacySystemdBindingRequiresExactLoadedJob(t *testing.T) {
	properties := map[string]string{"MainPID": "123", "ActiveState": "active", "FragmentPath": "/etc/systemd/system/control.service", "DropInPaths": "", "NeedDaemonReload": "no", "Restart": "on-failure", "ControlGroup": "/system.slice/control.service"}
	group := "0::/system.slice/control.service\n"
	if !matchesLegacySystemd(properties, properties["FragmentPath"], 123, group) {
		t.Fatal("matching loaded systemd job rejected")
	}
	for key, value := range map[string]string{"MainPID": "1234", "ActiveState": "inactive", "FragmentPath": "/other/unit", "DropInPaths": "/override.conf", "NeedDaemonReload": "yes", "Restart": "no", "ControlGroup": "/other/group"} {
		changed := maps.Clone(properties)
		changed[key] = value
		if matchesLegacySystemd(changed, properties["FragmentPath"], 123, group) {
			t.Fatalf("foreign systemd %s accepted", key)
		}
	}
	if matchesLegacySystemd(properties, properties["FragmentPath"], 123, "0::/other/group\n") {
		t.Fatal("foreign process cgroup accepted")
	}
}

func TestRefreshLegacyUnixBinaryPreservesExtendedAttributes(t *testing.T) {
	binary, selected := unixRefreshFiles(t)
	value := []byte("preserved startup metadata")
	if err := unix.Setxattr(binary, "user.control-test", value, 0); errors.Is(err, unix.ENOTSUP) {
		t.Skip("filesystem does not support extended attributes")
	} else if err != nil {
		t.Fatal(err)
	}
	if err := refreshLegacyUnixBinary(context.Background(), binary, selected, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	var read [128]byte
	n, err := unix.Getxattr(binary, "user.control-test", read[:])
	if err != nil || string(read[:n]) != string(value) {
		t.Fatal("startup attribute lost", err)
	}
	info, err := os.Stat(binary)
	if err != nil || info.Mode().Perm() != 0750 {
		t.Fatal("metadata copy changed executable permissions", err)
	}
}
