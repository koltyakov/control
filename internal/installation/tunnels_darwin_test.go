package installation

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestTunnelLaunchPlist(t *testing.T) {
	text := tunnelLaunchPlist("/Users/test/a & b/control", "/Users/test/a & b/tunnels")
	var value any
	if err := xml.Unmarshal([]byte(text), &value); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"com.koltyakov.control.tunnels.", "__tunnels", "a &amp; b", "<key>KeepAlive</key><true/>", "<key>RunAtLoad</key><true/>"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing", want, text)
		}
	}
	if strings.Contains(text, "<string>node</string>") {
		t.Fatal("tunnel service enrolls a node")
	}
}
