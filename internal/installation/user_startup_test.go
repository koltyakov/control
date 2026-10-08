package installation

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestEncodedPowerShell(t *testing.T) {
	for _, script := range []string{"", "Write-Output 'hello'", "Write-Output '用户 😀'"} {
		data, err := base64.StdEncoding.DecodeString(encodedPowerShell(script))
		if err != nil {
			t.Fatal(err)
		}
		units := make([]uint16, len(data)/2)
		for i := range units {
			units[i] = uint16(data[2*i]) | uint16(data[2*i+1])<<8
		}
		if got := string(utf16.Decode(units)); got != script {
			t.Fatalf("encoded command changed %q to %q", script, got)
		}
	}
}

func TestUserTaskArgumentsQuotePaths(t *testing.T) {
	binary := `C:\Users\O'Brien\程序\control.exe`
	config := `C:\Users\O'Brien\control profile\node.json`
	want := `__user-launch "C:\Users\O'Brien\control profile\node.json" "C:\Users\O'Brien\程序\control.exe"`
	if got := userTaskArguments(binary, config); got != want {
		t.Fatalf("startup arguments changed paths: %q", got)
	}
}

func TestUserTaskOwnershipAndProfileScope(t *testing.T) {
	config := `C:\Users\andrew\control\node.json`
	if userTaskName(config) != userTaskName(strings.ToUpper(config)) {
		t.Fatal("Windows profile names must be case insensitive")
	}
	if userTaskName(config) == userTaskName(config+".other") {
		t.Fatal("different profiles share a login task")
	}
	script := userTaskScript(config, "Write-Output 'owned'")
	for _, want := range []string{"Get-ScheduledTask -ErrorAction Stop | Where-Object", "$_.TaskPath -eq '\\' -and $_.TaskName -eq $name", "Control user node:", "$taskSid -ne $sid", "$task.Description -ne $description", "throw 'Control login task belongs to another profile or user'", "Write-Output 'owned'"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q: %s", want, script)
		}
	}
	if strings.Contains(script, "-ErrorAction SilentlyContinue") {
		t.Fatal("missing tasks must not leave a suppressed CIM error")
	}
}

func TestUserTaskRunsAtLoginWithoutElevationOrPassword(t *testing.T) {
	script := userTaskRegistration(`C:\control\startup\control-user.exe`, `C:\control\control.exe`, `C:\control\node.json`)
	for _, want := range []string{"-AtLogOn -User $sid", "-LogonType Interactive -RunLevel Limited", "-MultipleInstances IgnoreNew", "-ExecutionTimeLimit ([TimeSpan]::Zero)", "-RestartCount 3", `-Execute 'C:\control\startup\control-user.exe'`, `-Argument '__user-launch "C:\control\node.json" "C:\control\control.exe"'`} {
		if !strings.Contains(script, want) {
			t.Fatalf("registration missing %q: %s", want, script)
		}
	}
	if strings.Contains(script, "-Password") || strings.Contains(script, "Highest") {
		t.Fatal("user startup must not store a password or request elevation")
	}
	for _, unwanted := range []string{"powershell.exe", "Start-Process", "-WindowStyle", "-EncodedCommand"} {
		if strings.Contains(script, unwanted) {
			t.Fatalf("login task retained a console wrapper: %s", unwanted)
		}
	}
}
