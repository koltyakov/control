package installation

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestLaunchDaemonSystemStartup(t *testing.T) {
	plist := systemLaunchPlist("/usr/local/bin/control & tools", "/Library/Application Support/control/node.json")
	var parsed any
	if err := xml.Unmarshal([]byte(plist), &parsed); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"com.koltyakov.control.", "/usr/local/bin/control &amp; tools", "/Library/Application Support/control/node.json", "<key>RunAtLoad</key><true/>", "<key>SuccessfulExit</key><false/>"} {
		if !strings.Contains(plist, want) {
			t.Fatalf("system plist missing %q: %s", want, plist)
		}
	}
}
