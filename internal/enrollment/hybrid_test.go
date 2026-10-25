package enrollment

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/update"
)

func TestHybridInstallers(t *testing.T) {
	ticket := strings.Repeat("a", 43)
	link := "https://gateway.example/install/" + ticket
	if SystemTicket(ticket) == ticket || len(SystemTicket(ticket)) != 43 || SystemTicket(ticket) == SystemTicket(strings.Repeat("b", 43)) {
		t.Fatal("companion tickets are not independently derived")
	}
	for _, platform := range []string{"linux", "darwin", "windows"} {
		for _, role := range []string{"user", "system"} {
			i := Invitation{ID: "primary", HybridID: "primary", HybridRole: role, ServiceMode: "hybrid", Asset: update.Asset{OS: platform, Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
			if role == "system" {
				i.ServiceMode = "system"
			}
			script, err := Script(i, link)
			if err != nil {
				t.Fatal(err)
			}
			if role == "user" && (!strings.Contains(script, SystemTicket(ticket)) || !strings.Contains(script, "pending-enrollment.json")) {
				t.Fatal("Hybrid installer lost companion or recovery flow", script)
			}
			if role == "system" && !strings.Contains(script, "--hybrid-system") {
				t.Fatal("old installer could silently accept privileged enrollment")
			}
			if platform != "windows" && runtime.GOOS != "windows" {
				cmd := exec.Command("bash", "-n")
				cmd.Stdin = strings.NewReader(script)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("invalid %s/%s Bash: %s %v", platform, role, output, err)
				}
			}
		}
	}
}

func TestHybridAutomaticNames(t *testing.T) {
	for _, role := range []string{"user", "system"} {
		i := Invitation{AutoName: true, HybridRole: role}
		got, err := i.ResolveName("worker")
		want := "worker"
		if role == "system" {
			want += "-system"
		}
		if err != nil || got != want {
			t.Fatal(got, err)
		}
		if _, err = i.ResolveName(strings.Repeat("a", 57)); err == nil {
			t.Fatal("Hybrid hostname left no space for System suffix")
		}
	}
}
