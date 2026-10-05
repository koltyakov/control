package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/update"
)

func TestCheckBundleRejectsCorruption(t *testing.T) {
	for _, damage := range []string{"none", "binary", "checksums", "manifest"} {
		t.Run(damage, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name string, data []byte) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			binary := []byte("bundle integrity fixture")
			hash := sha256.Sum256(binary)
			asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: int64(len(binary)), SHA256: hex.EncodeToString(hash[:])}
			manifest := update.Manifest{Version: "v0.2.0", CreatedAt: time.Now().UTC(), Assets: []update.Asset{asset}}
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			write("control-manifest.json", data)
			write(asset.File, binary)
			write("checksums.txt", []byte(fmt.Sprintf("%s  %s\n", asset.SHA256, asset.File)))
			switch damage {
			case "binary":
				binary[0] ^= 1
				write(asset.File, binary)
			case "checksums":
				write("checksums.txt", []byte("mismatched checksums\n"))
			case "manifest":
				write("control-manifest.json", []byte(`{"version":"v0.2.0","assets":[]}`))
			}
			err = checkBundle(dir)
			if (err != nil) != (damage != "none") {
				t.Fatalf("checkBundle with %s: %v", damage, err)
			}
		})
	}
}

func TestSourceVersion(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Chdir(t.TempDir())
	git := func(args ...string) string {
		t.Helper()
		output, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	check := func(want string) {
		t.Helper()
		if got := sourceVersion(); got != want {
			t.Fatalf("version = %q, want %q", got, want)
		}
	}
	check("dev") // Source archive without Git metadata.
	git("init", "-q")
	git("config", "user.name", "Control test")
	git("config", "user.email", "control@example.com")
	git("config", "commit.gpgsign", "false")
	git("config", "tag.gpgsign", "false")
	check("dev") // Repository without commits.
	if err := os.WriteFile("source.txt", []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-qm", "initial")
	check(git("rev-parse", "--short", "HEAD"))
	git("tag", "v0.1.0")
	check("v0.1.0") // Lightweight tags count too.
	git("commit", "--allow-empty", "-qm", "after tag")
	want := "v0.1.0-1-g" + git("rev-parse", "--short", "HEAD")
	check(want)
	if err := os.WriteFile("source.txt", []byte("modified\n"), 0600); err != nil {
		t.Fatal(err)
	}
	check(want + "-dirty")
	git("add", "source.txt")
	check(want + "-dirty")
	git("commit", "-qm", "changed source")
	git("tag", "-a", "v0.2.0", "-m", "release")
	check("v0.2.0")
	t.Run("without git", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if got := sourceVersion(); got != "dev" {
			t.Fatalf("version = %q, want dev", got)
		}
	})
}

func TestGitHubRepositoryRemotes(t *testing.T) {
	for _, test := range []struct{ remote, want string }{
		{"git@github.com:koltyakov/control.git", "koltyakov/control"},
		{"https://github.com/owner/fork.git", "owner/fork"},
		{"ssh://git@github.com/owner/fork.git", "owner/fork"},
		{"https://github.com/owner/fork/", "owner/fork"},
		{"https://other.example/owner/repo.git", ""},
		{"https://github.com/owner/repo/tree/main", ""},
		{"/local/repository", ""},
	} {
		t.Run(test.remote, func(t *testing.T) {
			if got := githubRepository(test.remote); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
