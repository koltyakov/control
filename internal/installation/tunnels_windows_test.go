package installation

import (
	"strings"
	"testing"
)

func TestTunnelTaskScript(t *testing.T) {
	text := tunnelTaskScript(`C:\Users\test\a b\control.exe`, `C:\Users\test\a'b\tunnels`)
	for _, want := range []string{"ControlTunnels-", "Control persistent tunnels:", "-LogonType Interactive -RunLevel Limited", "-ExecutionTimeLimit ([TimeSpan]::Zero)", "-RestartCount 999", "Start-ScheduledTask", "task belongs to another profile or user"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing", want, text)
		}
	}
	if strings.Contains(text, "-RunLevel Highest") || strings.Contains(text, "-Password") {
		t.Fatal("tunnel startup requests elevated or stored credentials")
	}
}
