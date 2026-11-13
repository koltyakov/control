package installation

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestRefreshLegacyUnixBinaryPreservesDarwinACL(t *testing.T) {
	binary, selected := unixRefreshFiles(t)
	if output, err := exec.Command("/bin/chmod", "+a", "everyone allow read", binary).CombinedOutput(); err != nil {
		t.Fatalf("prepare native ACL: %v: %s", err, output)
	}
	acl := func() string {
		output, err := exec.Command("/bin/ls", "-le", binary).Output()
		if err != nil {
			t.Fatal(err)
		}
		_, entries, _ := strings.Cut(string(output), "\n")
		return entries
	}
	before := acl()
	if strings.TrimSpace(before) == "" {
		t.Fatal("native ACL fixture has no entries")
	}
	if err := refreshLegacyUnixBinary(context.Background(), binary, selected, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if after := acl(); after != before {
		t.Fatalf("startup ACL changed: %q != %q", after, before)
	}
}

func TestLegacyLaunchdBindingRequiresExactLoadedJob(t *testing.T) {
	path, binary, config := "/private/owned.plist", "/Applications/Control tools/control", "/profiles/my node.json"
	output := []byte("job = {\n\tpath = " + path + "\n\tprogram = " + binary + "\n\targuments = {\n\t\t" + binary + "\n\t\tnode\n\t\t--config\n\t\t" + config + "\n\t}\n\tpid = 123\n}\n")
	if !matchesLegacyLaunchd(output, path, binary, config, 123) {
		t.Fatal("matching loaded launchd job rejected")
	}
	for _, changed := range []string{
		strings.Replace(string(output), "pid = 123", "pid = 1234", 1),
		strings.Replace(string(output), "\tprogram = "+binary, "\tprogram = /other/control", 1),
		strings.Replace(string(output), "\tpath = "+path, "\tpath = /other.plist", 1),
		strings.Replace(string(output), "\t\tnode\n", "\t\tnode\n\t\t--extra\n", 1),
		strings.Replace(string(output), "\t\t"+config+"\n", "\t\t/foreign/profile.json\n", 1),
	} {
		if matchesLegacyLaunchd([]byte(changed), path, binary, config, 123) {
			t.Fatal("foreign loaded job accepted")
		}
	}
}
