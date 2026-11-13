package installation

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestPowerShellEnvironmentUsesBuiltInModules(t *testing.T) {
	inherited := []string{
		`PATH=C:\Windows\System32`,
		`PSModulePath=C:\Program Files\PowerShell\7\Modules`,
		`psmodulepath=C:\custom\Modules`,
		`PSExecutionPolicyPreference=AllSigned`,
		`CONTROL_HOME=C:\Users\test\Control`,
	}
	original := slices.Clone(inherited)
	modules := `C:\Windows\System32\WindowsPowerShell\v1.0\Modules`
	got := powerShellEnvironment(inherited, modules)
	want := []string{inherited[0], inherited[3], inherited[4], "PSModulePath=" + modules}
	if !slices.Equal(got, want) {
		t.Fatalf("helper environment: got %q, want %q", got, want)
	}
	if !slices.Equal(inherited, original) {
		t.Fatal("changed the caller's environment")
	}
}

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
	config := `C:\Users\O'Brien\control profile\node.json`
	want := `__user-direct "C:\Users\O'Brien\control profile\node.json"`
	if got := userTaskArguments(config); got != want {
		t.Fatalf("startup arguments changed paths: %q", got)
	}
}

func TestHybridUserPhasePreparation(t *testing.T) {
	failure := errors.New("access denied")
	for _, tc := range []struct {
		name                  string
		preflight             bool
		prepareErr, enrollErr error
		want                  []string
		failureContext        string
	}{
		{"preflight", true, failure, nil, []string{"limited"}, ""},
		{"enrollment", false, nil, nil, []string{"prepare", "limited"}, ""},
		{"preparation failed", false, failure, nil, []string{"prepare"}, "prepare Hybrid User login startup"},
		{"enrollment failed", false, nil, failure, []string{"prepare", "limited"}, "limited-token Hybrid User enrollment"},
		{"preflight failed", true, nil, failure, []string{"limited"}, "limited-token Hybrid User enrollment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			err := runHybridUserPhase(t.Context(), tc.preflight, func() error {
				calls = append(calls, "prepare")
				return tc.prepareErr
			}, func() error {
				calls = append(calls, "limited")
				return tc.enrollErr
			})
			if !slices.Equal(calls, tc.want) {
				t.Fatalf("phases = %v, want %v", calls, tc.want)
			}
			if tc.failureContext == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, failure) || !strings.Contains(err.Error(), tc.failureContext) {
				t.Fatalf("phase failure lost context or cause: %v", err)
			}
		})
	}
	for _, beforePrepare := range []bool{true, false} {
		ctx, cancel := context.WithCancel(t.Context())
		if beforePrepare {
			cancel()
		}
		err := runHybridUserPhase(ctx, false, func() error {
			if beforePrepare {
				t.Fatal("cancelled installation removed a task")
			}
			cancel()
			return nil
		}, func() error {
			t.Fatal("cancelled installation started enrollment")
			return nil
		})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation was lost:", err)
		}
	}
}

func TestUserTaskRemovalRemainsOwnershipScoped(t *testing.T) {
	script := userTaskRemovalScript(`C:\Users\user\control\node.json`)
	validation := strings.Index(script, "throw 'Control login task belongs to another profile or user'")
	removal := strings.Index(script, "if ($task) { Unregister-ScheduledTask -InputObject $task -Confirm:$false }")
	if validation < 0 || removal <= validation {
		t.Fatal("task removal bypassed profile/user ownership validation")
	}
	for _, forbidden := range []string{"Start-ScheduledTask", "Register-ScheduledTask", "Set-ScheduledTask", "SetSecurityDescriptor", "Start-Process"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("task removal must not execute an action or rewrite permissions: %s", forbidden)
		}
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
	for _, want := range []string{"Get-ScheduledTask -TaskPath '\\' -ErrorAction Stop | Where-Object", "$_.TaskPath -eq '\\' -and $_.TaskName -eq $name", "Control user node:", "$taskSid -ne $sid", "$task.Description -ne $description", "throw 'Control login task belongs to another profile or user'", "Write-Output 'owned'"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q: %s", want, script)
		}
	}
	if strings.Contains(script, "-ErrorAction SilentlyContinue") {
		t.Fatal("missing tasks must not leave a suppressed CIM error")
	}
}

func TestUserTaskRunsAtLoginWithoutElevationOrPassword(t *testing.T) {
	script := userTaskRegistration(`C:\control\runtime\control.exe`, `C:\control\node.json`)
	for _, want := range []string{"-AtLogOn -User $sid", "-LogonType Interactive -RunLevel Limited", "-MultipleInstances IgnoreNew", "-ExecutionTimeLimit ([TimeSpan]::Zero)", "-RestartCount 3", `-Execute 'C:\control\runtime\control.exe'`, `-Argument '__user-direct "C:\control\node.json"'`} {
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

func TestUserTaskRegistrationPreservesACLAndGrantsLimitedMaintenance(t *testing.T) {
	script := userTaskRegistration(`C:\control\runtime\control.exe`, `C:\control\node.json`)
	for _, want := range []string{
		"$maintenance = 0x1201bf", "$registered.GetSecurityDescriptor(7)",
		"$ace.SecurityIdentifier.Value -eq $sid", "$granted -band $maintenance",
		"$security.DiscretionaryAcl.InsertAce(0, $grant)",
		"[Security.Principal.SecurityIdentifier]::new($sid)",
		"$registered.SetSecurityDescriptor(", "-LogonType Interactive -RunLevel Limited",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("missing scoped task maintenance permission: %s", want)
		}
	}
	for _, forbidden := range []string{"SetOwner", "WriteOwner", "WriteDac", "Clear()", "LocalSystem", "Highest", ";;;WD", ";;;AU"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("task maintenance widened unrelated authority: %s", forbidden)
		}
	}
	if strings.Index(script, "Register-ScheduledTask") > strings.Index(script, "$registered.GetSecurityDescriptor") {
		t.Fatal("permissions were changed before registering the owned task")
	}
}
