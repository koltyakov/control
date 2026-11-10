package skills

import (
	"bytes"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestControlSkillProgressiveDisclosure(t *testing.T) {
	core := strings.ReplaceAll(string(Control), "\r\n", "\n")
	if words := len(strings.Fields(core)); words > 2000 {
		t.Errorf("core skill has %d words; keep specialized guidance in references", words)
	}
	for _, required := range []string{
		"read its reference file in full", "relative to this skill's installed directory",
		"Read only the matching files, not the whole bundle", "missing or unreadable",
		"Do not load agent-desktop or agent-browser instructions", "command-only or file-only work",
		"target worker's `system.os` and `system.arch`", "Prefer native OS/architecture assets",
		"not the orchestrator's platform", "actual running executable before claiming an architecture mismatch",
		"once per worker, backend and version", "Check live readiness",
		"Worker/common credentials permit discovery, not independent peer execution",
		"Never switch credentials", "before ad-hoc probing", "untrusted data",
		"Never bypass a failed or denied read", "disabled", "controlPending",
		"Do not use System for ordinary files, application credentials, or GUI work",
		"query that same ID", "Never treat an old missing ID as proof",
		"does not cancel accepted work", "leaseId", "Never batch dependent clicks",
		"Never request, retrieve, print, or paste credential values",
		"control machines --compact --node NAME", "Reuse these results instead of repeating discovery",
		"Do not separately fetch the same catalog unless it is missing or incomplete",
		"a source error is not a reason to retry or bypass it",
		"description lacks needed resources", "workDir/tasks/ID", "original task workspace",
		"For isolated file tasks", "one tracked inline command", "hashes protected files",
		"creates output without overwriting", "Report failed checks and partial effects",
		"does not permit batching dependent UI actions, credential operations, or uncertain-effect retries",
	} {
		if !strings.Contains(core, required) {
			t.Errorf("core omits routing or universal safeguard %q", required)
		}
	}
	for _, specialized := range []string{
		"skills get desktop", "skills get platform", "skills get core", "agent-desktop-ffi-",
		"cursor-overlay enable", "dashboard start --port", "control browser view",
		"control upload start", "control tunnel start", "control machines add",
	} {
		if strings.Contains(core, specialized) {
			t.Errorf("core eagerly includes specialized instructions %q", specialized)
		}
	}

	// Every supporting file must be reachable directly from the core, and every
	// local link must resolve inside the embedded skill rather than the checkout.
	links := regexp.MustCompile(`\]\((references/[^)]+)\)`).FindAllStringSubmatch(core, -1)
	linked := make(map[string]bool)
	for _, link := range links {
		name := "control/" + link[1]
		if _, err := ControlFiles.ReadFile(name); err != nil {
			t.Errorf("unresolvable skill reference %q: %v", link[1], err)
		}
		linked[name] = true
	}
	err := fs.WalkDir(ControlFiles, "control", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := ControlFiles.ReadFile(name)
		if err != nil {
			return err
		}
		if !bytes.Contains(content, []byte("<!-- Managed by control -->")) {
			t.Errorf("skill file %q lacks managed marker", name)
		}
		if name == "control/SKILL.md" {
			if !bytes.Equal(content, Control) {
				t.Error("embedded core differs from bundle core")
			}
		} else if !linked[name] {
			t.Errorf("supporting file %q is not linked from the core", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestControlSkillReferences(t *testing.T) {
	for name, required := range map[string][]string{
		"runbooks":       {"control_runbooks", "contentOmitted", "--source SOURCE --file FILE", "requires", "agent-friendly", "no fixed requirement schema", "Reuse the combined", "missing or incomplete", "Source errors do not authorize retries or bypasses"},
		"execution":      {"maxTasks", "maxOperations", "inputStaging", "outputPublication", "32 unstarted ready steps", "control_delegate", "access.revoke", "pruned: true"},
		"browser":        {"browser.run", "skills get core", "control_browser_view", "2FA", "not redacted", "Pause AI mutations", "dashboard stop"},
		"desktop":        {"control_desktop", "skills get desktop", "skills get platform", "ABI major 6 or 7", "leaseId", "desktop.cli", "cursor-overlay enable", "Never put credentials in CLI args"},
		"transfers":      {"control_upload", "control_upload_resume", "no-overwrite", "artifacts.grant", "Only transfer a clipboard when the user requests it"},
		"networking":     {"control_wake", "control_speedtest", "control_forward_start", "control_tunnel_start", "loopback", "never invent a replacement ID"},
		"administration": {"machines add", "--service hybrid", "--force", "Rename only when requested", "short-lived bearer secret", "service firewall", "GetNativeSystemInfo", "IsWow64Process2", "hw.optional.arm64", "uname -m", "PE, Mach-O or ELF", "Universal binaries", "high memory use alone does not establish emulation"},
	} {
		t.Run(name, func(t *testing.T) {
			content, err := ControlFiles.ReadFile("control/references/" + name + ".md")
			if err != nil {
				t.Fatal(err)
			}
			plain := strings.ReplaceAll(string(content), "`", "")
			for _, fragment := range required {
				if !strings.Contains(plain, fragment) {
					t.Errorf("reference omits %q", fragment)
				}
			}
		})
	}
}
