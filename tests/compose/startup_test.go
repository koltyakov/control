//go:build compose

package compose_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/update"
)

func TestInstallerPassesExplicitStartupContext(t *testing.T) {
	for _, mode := range []string{"user", "system"} {
		t.Run(mode, func(t *testing.T) {
			ctx, _ := environment(t)
			dir := t.TempDir()
			binary := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" \"${CONTROL_HOME:-}\" \"${CONTROL_INSTALL_DIR:-}\" > \"$RESULT\"\n"
			var downloads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downloads.Add(1)
				_, _ = w.Write([]byte(binary))
			}))
			defer server.Close()
			for name, body := range map[string]string{
				"uname": "case \"$1\" in -s) echo Linux;; -m) echo x86_64;; esac\n",
				"id":    "echo 0\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			i := enrollment.Invitation{ServiceMode: mode, Asset: update.Asset{OS: "linux", Arch: "amd64", SHA256: enrollment.Hash(binary)}}
			script, err := enrollment.Script(i, server.URL)
			if err != nil {
				t.Fatal(err)
			}
			run := func() error {
				cmd := exec.CommandContext(ctx, "bash", "-c", script)
				cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "RESULT="+filepath.Join(dir, "result"), "CONTROL_HOME=", "CONTROL_INSTALL_DIR=", "CONTROL_SERVICE_MODE=process")
				return cmd.Run()
			}
			if err := run(); err != nil {
				t.Fatal(err)
			}
			result, err := os.ReadFile(filepath.Join(dir, "result"))
			if err != nil || !strings.Contains(string(result), "--service\n"+mode+"\n") {
				t.Fatalf("wrong enrollment context: %s %v", result, err)
			}
			if mode == "system" {
				if !strings.Contains(string(result), "/var/lib/control\n/usr/local/bin\n") {
					t.Fatalf("wrong system installation paths: %s", result)
				}
				if err := os.WriteFile(filepath.Join(dir, "id"), []byte("#!/usr/bin/env bash\necho 1000\n"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := run(); err == nil || downloads.Load() != 1 {
					t.Fatal("non-root system installer did not fail before downloading")
				}
			}
		})
	}
}
