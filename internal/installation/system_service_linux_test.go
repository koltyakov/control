package installation

import (
	"strings"
	"testing"
)

func TestSystemdSystemStartup(t *testing.T) {
	unit := systemdSystemUnit("/usr/local/bin/control", "/var/lib/control profile/node.json")
	for _, want := range []string{`ExecStart="/usr/local/bin/control" node --config "/var/lib/control profile/node.json"`, "WantedBy=multi-user.target", "Restart=on-failure", "UMask=0077", "Environment=CONTROL_TOKEN="} {
		if !strings.Contains(unit, want) {
			t.Fatalf("system unit missing %q: %s", want, unit)
		}
	}
	if strings.Contains(unit, "default.target") || strings.Contains(unit, "--user") {
		t.Fatal("system unit depends on a login session")
	}
}
