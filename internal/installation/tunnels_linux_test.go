package installation

import (
	"strings"
	"testing"
)

func TestTunnelSystemdUnit(t *testing.T) {
	text := tunnelSystemdUnit("/home/test/a b/control", "/home/test/a%t/tunnels")
	for _, want := range []string{`"/home/test/a b/control" __tunnels "/home/test/a%%t/tunnels"`, "Restart=always", "UMask=0077", "WantedBy=default.target"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing", want, text)
		}
	}
	if strings.Contains(text, " node ") {
		t.Fatal("tunnel service enrolls a node")
	}
}
