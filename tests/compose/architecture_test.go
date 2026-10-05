//go:build compose

package compose_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/update"
)

func TestInstallerDetectsArchitectureAndChecksSelectedBinary(t *testing.T) {
	for _, osName := range []string{"linux", "darwin"} {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(osName+"/"+arch, func(t *testing.T) {
				ctx, _ := environment(t)
				dir := t.TempDir()
				binaries := map[string]string{}
				var assets []update.Asset
				for _, target := range []string{"amd64", "arm64"} {
					binary := "#!/usr/bin/env bash\nprintf '%s' '" + target + "' > \"$RESULT\"\n"
					binaries[target] = binary
					assets = append(assets, update.Asset{OS: osName, Arch: target, File: update.AssetName(osName, target), Size: int64(len(binary)), SHA256: enrollment.Hash(binary)})
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/binary" {
						http.NotFound(w, r)
						return
					}
					_, _ = w.Write([]byte(binaries[r.URL.Query().Get("arch")]))
				}))
				defer server.Close()
				unameOS, unameArch := "Linux", "x86_64"
				if osName == "darwin" {
					unameOS = "Darwin"
				}
				if arch == "arm64" {
					unameArch = "aarch64"
				}
				uname := "#!/usr/bin/env bash\ncase \"$1\" in -s) echo " + unameOS + ";; -m) echo " + unameArch + ";; esac\n"
				if err := os.WriteFile(filepath.Join(dir, "uname"), []byte(uname), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "sysctl"), []byte("#!/usr/bin/env bash\nexit 1\n"), 0700); err != nil {
					t.Fatal(err)
				}
				script, err := enrollment.Script(enrollment.Invitation{Asset: assets[0], Assets: assets}, server.URL)
				if err != nil {
					t.Fatal(err)
				}
				run := func() error {
					cmd := exec.CommandContext(ctx, "bash", "-c", script)
					cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "RESULT="+filepath.Join(dir, "result"))
					return cmd.Run()
				}
				if err := run(); err != nil {
					t.Fatal(err)
				}
				result, err := os.ReadFile(filepath.Join(dir, "result"))
				if err != nil || string(result) != arch {
					t.Fatalf("wrong architecture executed: %s %v", result, err)
				}
				// Change only the expected digest; the script must reject the file.
				for i := range assets {
					assets[i].SHA256 = strings.Repeat("0", 64)
				}
				script, err = enrollment.Script(enrollment.Invitation{Asset: assets[0], Assets: assets}, server.URL)
				if err != nil {
					t.Fatal(err)
				}
				if err := run(); err == nil {
					t.Fatal("corrupted installer was executed")
				}
			})
		}
	}
}
