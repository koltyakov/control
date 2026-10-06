package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMakeUpdateAuthorizesBeforeBundlingEvenInParallel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Make recipes require a POSIX shell")
	}
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is not installed")
	}
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "authorized", true: "denied"}[fail], func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "Makefile"), makefile, 0600); err != nil {
				t.Fatal(err)
			}
			builder := filepath.Join(dir, "builder.sh")
			if err := os.WriteFile(builder, []byte(`#!/bin/sh
set -eu
if [ "$1" = env ]; then echo linux; exit 0; fi
binary=
out=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --binary) shift; binary=$1 ;;
        --out) shift; out=$1 ;;
    esac
    shift
done
if [ -n "$binary" ]; then
    echo build >> "$UPDATE_TEST_LOG"
    mkdir -p "$(dirname "$binary")"
    cat > "$binary" <<'CONTROL'
#!/bin/sh
set -eu
case "$*" in
    "update authorize --check")
        echo authorize >> "$UPDATE_TEST_LOG"
        if [ "$UPDATE_TEST_FAIL_AUTH" = 1 ]; then exit 1; fi
        ;;
    "update push "*) echo push >> "$UPDATE_TEST_LOG" ;;
    *) exit 2 ;;
esac
CONTROL
    chmod +x "$binary"
elif [ -n "$out" ]; then
    echo bundle >> "$UPDATE_TEST_LOG"
else
    exit 2
fi
`), 0600); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "steps")
			failAuth := "0"
			want := "build\nauthorize\nbundle\npush\n"
			if fail {
				failAuth = "1"
				want = "build\nauthorize\n"
			}
			t.Setenv("UPDATE_TEST_LOG", log)
			t.Setenv("UPDATE_TEST_FAIL_AUTH", failAuth)
			cmd := exec.CommandContext(t.Context(), makePath, "-j4", "update", "GO=sh '"+strings.ReplaceAll(builder, "'", "'\\''")+"'", "BIN_DIR=bin with spaces", "DIST_DIR=dist with spaces", "VERSION=make-test")
			cmd.Dir = dir
			output, err := cmd.CombinedOutput()
			if (err != nil) != fail {
				t.Fatalf("make update: %v\n%s", err, output)
			}
			steps, err := os.ReadFile(log)
			if err != nil || string(steps) != want {
				t.Fatalf("steps = %q, want %q: %v\n%s", steps, want, err, output)
			}
		})
	}
}
