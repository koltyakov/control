package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type symlinkReport struct {
	skipped string
	failed  string
}

func (*symlinkReport) Helper() {}
func (r *symlinkReport) Skipf(format string, args ...any) {
	r.skipped = fmt.Sprintf(format, args...)
}
func (r *symlinkReport) Fatalf(format string, args ...any) {
	r.failed = fmt.Sprintf(format, args...)
}

func TestSymlinkPolicy(t *testing.T) {
	unavailable := &os.LinkError{Op: "symlink", Old: "target", New: "link", Err: missingSymlinkCapability()}
	for _, tc := range []struct {
		name, required string
		err            error
		wantSkip       bool
		wantFail       bool
	}{
		{name: "success"},
		{name: "required success", required: "1"},
		{name: "missing capability", err: unavailable, wantSkip: true},
		{name: "explicit optional", required: "false", err: unavailable, wantSkip: true},
		{name: "required missing capability", required: "1", err: unavailable, wantFail: true},
		{name: "true requires capability", required: "true", err: unavailable, wantFail: true},
		{name: "unexpected collision", err: os.ErrExist, wantFail: true},
		{name: "required unexpected collision", required: "1", err: os.ErrExist, wantFail: true},
		{name: "unexpected permission", err: os.ErrPermission, wantFail: true},
		{name: "missing parent", err: os.ErrNotExist, wantFail: true},
		{name: "invalid declaration", required: "provisioned", wantFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(RequireSymlinksEnv, tc.required)
			report, calls := &symlinkReport{}, 0
			symlink(report, "target", "link", func(target, link string) error {
				calls++
				if target != "target" || link != "link" {
					t.Fatal("fixture arguments changed")
				}
				return tc.err
			})
			if (report.skipped != "") != tc.wantSkip || (report.failed != "") != tc.wantFail {
				t.Fatalf("skip=%q fail=%q", report.skipped, report.failed)
			}
			wantCalls := 1
			if tc.name == "invalid declaration" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("creation calls=%d, want %d", calls, wantCalls)
			}
			if tc.wantSkip && (!strings.Contains(report.skipped, unavailable.Error()) || !strings.Contains(report.skipped, RequireSymlinksEnv)) {
				t.Fatal("skip lost its cause or opt-in guidance", report.skipped)
			}
			if tc.wantFail && tc.err != nil && !strings.Contains(report.failed, tc.err.Error()) {
				t.Fatal("failure lost its cause", report.failed)
			}
		})
	}
}

// Exercise real testing.T skip/failure reporting without needing symlink rights.
func TestSymlinkReportingFixture(t *testing.T) {
	mode := os.Getenv("CONTROL_TEST_SYMLINK_REPORTING")
	if mode == "" {
		return
	}
	err := missingSymlinkCapability()
	if mode == "unexpected" {
		err = os.ErrExist
	}
	symlink(t, "target", "link", func(string, string) error { return err })
	t.Fatal("a failed fixture creation returned normally")
}

func TestSymlinkReporting(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, required, mode, marker, diagnostic string
		fail                                     bool
	}{
		{"unprovisioned", "", "unavailable", "--- SKIP:", "symlink creation unavailable", false},
		{"provisioned", "1", "unavailable", "--- FAIL:", "symlink creation required by " + RequireSymlinksEnv, true},
		{"unexpected", "", "unexpected", "--- FAIL:", "unexpected symlink creation failure", true},
		{"invalid declaration", "provisioned", "unavailable", "--- FAIL:", "invalid " + RequireSymlinksEnv, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(RequireSymlinksEnv, tc.required)
			t.Setenv("CONTROL_TEST_SYMLINK_REPORTING", tc.mode)
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestSymlinkReportingFixture$", "-test.v")
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail || !strings.Contains(string(out), tc.marker) || !strings.Contains(string(out), tc.diagnostic) {
				t.Fatalf("fixture reporting: err=%v output=%s", err, out)
			}
		})
	}
}

func TestSymlinkCreatesDirectoryAndFileLinks(t *testing.T) {
	for _, directory := range []bool{false, true} {
		name := "file"
		if directory {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			target, link := filepath.Join(base, "target"), filepath.Join(base, "link")
			if directory {
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(target, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			Symlink(t, target, link)
			info, err := os.Lstat(link)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatal("fixture is not a symbolic link", info, err)
			}
			original, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := os.Stat(link)
			if err != nil || !os.SameFile(original, resolved) {
				t.Fatal("fixture points at another target", err)
			}
		})
	}
}
