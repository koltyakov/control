package runbooks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeBook(t *testing.T, root, file, text string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRunbookLocations(t *testing.T) {
	home := t.TempDir()
	for _, platform := range []string{"darwin", "windows", "linux"} {
		empty := func(string) string { return "" }
		want := map[string]string{
			"darwin":  filepath.Join(home, "Library", "Application Support", "control", "runbooks"),
			"windows": filepath.Join(home, "AppData", "Local", "control", "runbooks"),
			"linux":   filepath.Join(home, ".local", "state", "control", "runbooks"),
		}[platform]
		if got := userDirectory(platform, home, empty); got != want {
			t.Fatalf("%s: %s, want %s", platform, got, want)
		}
		if got := userDirectory(platform, home, func(string) string { return "relative" }); got != want {
			t.Fatal("relative environment override accepted", got)
		}
	}
	override := t.TempDir()
	for _, platform := range []string{"linux", "windows"} {
		if got := userDirectory(platform, home, func(string) string { return override }); got != filepath.Join(override, "control", "runbooks") {
			t.Fatal("absolute environment override ignored", got)
		}
	}
}

func TestRunbooksSharedMachineAndConflictPreference(t *testing.T) {
	project, global := t.TempDir(), t.TempDir()
	roots := []Root{{Source: "project", Path: project}, {Source: "global", Path: global}}
	writeBook(t, project, "shared/vpn/RUNBOOK.md", "# VPN\nConnect a compatible client.\n")
	writeBook(t, global, "shared/vpn.md", "# Generic VPN\nGlobal shared procedure.\n")
	writeBook(t, global, "machines/trader/vpn.md", "# Trader VPN\nUse vpn.password.\n")
	writeBook(t, project, "machines/stable-id/vpn.md", "# Stable trader VPN\nUse vpn.portal.\n")
	writeBook(t, project, "machines/other/vpn.md", "other machine")
	selector := Selector{Node: "trader", ID: "stable-id"}
	catalog, err := Discover(context.Background(), roots, selector, Query{})
	if err != nil || len(catalog.Runbooks) != 4 || !catalog.Runbooks[0].Preferred || catalog.Runbooks[0].Machine != "stable-id" {
		t.Fatalf("matching/preference: %+v, %v", catalog, err)
	}
	for _, book := range catalog.Runbooks {
		if book.ID != "vpn" || book.ConflictCount != 3 || len(book.Conflicts) != 3 || book.Machine == "other" {
			t.Fatalf("incorrect conflict or binding: %+v", book)
		}
	}
	catalog, err = Discover(context.Background(), roots, Selector{}, Query{})
	if err != nil || len(catalog.Runbooks) != 5 {
		t.Fatalf("node-free discovery: %+v, %v", catalog, err)
	}
	if _, err := Discover(context.Background(), roots, selector, Query{Source: "project", File: "machines/other/vpn.md"}); err == nil {
		t.Fatal("read another machine's instructions")
	}
	read, err := Discover(context.Background(), roots, selector, Query{Source: "global", File: "machines/trader/vpn.md"})
	if err != nil || len(read.Runbooks) != 1 || !strings.Contains(read.Runbooks[0].Content, "vpn.password") {
		t.Fatalf("exact origin read: %+v, %v", read, err)
	}
}

func TestRunbookBudgetsAndSourceErrors(t *testing.T) {
	root, healthy := t.TempDir(), t.TempDir()
	roots := []Root{{Source: "project", Path: root}, {Source: "global", Path: healthy}}
	text := strings.Repeat("x", 32<<10)
	for i := range 5 {
		writeBook(t, root, fmt.Sprintf("shared/%d.md", i), text)
	}
	writeBook(t, root, "shared/large.md", text+"x")
	q := Query{}
	catalog, err := Discover(context.Background(), roots, Selector{}, q)
	if err != nil || len(catalog.Runbooks) != 6 {
		t.Fatalf("catalog: %+v, %v", catalog, err)
	}
	for i, book := range catalog.Runbooks {
		if i < 4 && book.Omitted || i >= 4 && (!book.Omitted || book.Content != "") {
			t.Fatal("incorrect inline budget", book.File)
		}
	}
	read, err := Discover(context.Background(), roots, Selector{}, Query{Source: "project", File: "shared/large.md"})
	if err != nil || read.Runbooks[0].Content != text+"x" || read.Runbooks[0].Omitted {
		t.Fatal("complete read failed", err)
	}
	writeBook(t, healthy, "shared/valid.md", "healthy")
	for _, invalid := range []string{"\x00", "\xff", strings.Repeat("x", (1<<20)+1)} {
		writeBook(t, root, "shared/bad.md", invalid)
		catalog, err = Discover(context.Background(), roots, Selector{}, q)
		if err != nil || catalog.Sources[0].Error == "" || len(catalog.Runbooks) != 1 || catalog.Runbooks[0].Source != "global" {
			t.Fatalf("source error hid healthy books: %+v, %v", catalog, err)
		}
		if _, err := Discover(context.Background(), roots, Selector{}, Query{Source: "project", File: "shared/bad.md"}); err == nil {
			t.Fatal("complete read hid validation failure")
		}
	}
	writeBook(t, root, "shared/bad.md", "valid")
	writeBook(t, root, "shared/bad.json", "invalid")
	if _, err := Discover(context.Background(), roots, Selector{}, Query{Source: "project", File: "shared/bad.json"}); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	writeBook(t, root, "shared/bad.json", "{}")
	for i := range 64 {
		writeBook(t, root, fmt.Sprintf("shared/extra-%d.md", i), "text")
	}
	catalog, _ = Discover(context.Background(), roots, Selector{}, q)
	if catalog.Sources[0].Error == "" {
		t.Fatal("document count limit ignored")
	}
}

func TestRunbookPathsCancellationAndSymlinks(t *testing.T) {
	root := t.TempDir()
	roots := []Root{{Source: "project", Path: root}}
	for _, q := range []Query{{Node: "../trader"}, {File: "shared/run.md"}, {Source: "project", File: "../run.md"}, {Source: "project", File: `shared\run.md`}, {Source: "project", File: "/run.md"}, {Source: "project", File: "shared/run.ps1"}, {Source: "unknown"}} {
		if _, err := Discover(context.Background(), roots, Selector{}, q); err == nil {
			t.Fatalf("invalid query accepted: %+v", q)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Discover(ctx, roots, Selector{}, Query{}); err == nil {
		t.Fatal("cancellation ignored")
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, target := range []string{"root", "parent", "file", "directory"} {
		t.Run(target, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "control", "runbooks")
			writeBook(t, root, "shared/run.md", "safe")
			outside := t.TempDir()
			var link string
			switch target {
			case "root":
				link = root
			case "parent":
				link = filepath.Dir(root)
			case "directory":
				link = filepath.Join(root, "shared")
			case "file":
				link = filepath.Join(root, "shared", "run.md")
				writeBook(t, outside, "private.md", "private")
				outside = filepath.Join(outside, "private.md")
			}
			// Move the fixture aside; never delete a user directory.
			if err := os.Rename(link, link+"-original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			catalog, err := Discover(context.Background(), []Root{{Source: "project", Path: root}}, Selector{}, Query{})
			if err != nil || catalog.Sources[0].Error == "" || len(catalog.Runbooks) != 0 {
				t.Fatalf("symlink followed: %+v, %v", catalog, err)
			}
		})
	}
}

func TestRunbooksUnavailableUserHomeKeepsHealthyStores(t *testing.T) {
	root := t.TempDir()
	writeBook(t, root, "shared/healthy.md", "Healthy instructions.")
	roots := []Root{{Source: "project", Path: root}, {Source: "global", Error: "user home unavailable"}}
	catalog, err := Discover(context.Background(), roots, Selector{}, Query{})
	if err != nil || len(catalog.Runbooks) != 1 || catalog.Sources[1].Error == "" {
		t.Fatalf("unavailable user home hid project runbooks: %+v, %v", catalog, err)
	}
	if _, err := Discover(context.Background(), roots, Selector{}, Query{Source: "global", File: "shared/run.md"}); err == nil {
		t.Fatal("unavailable user store allowed complete read")
	}
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("LOCALAPPDATA", "")
	user := userRoot("global")
	if user.Error == "" {
		t.Fatal("missing user home was not reported")
	}
	if roots := WorkerRoots(); len(roots) != 2 || roots[1].Source != "worker-common" || roots[1].Path == "" || roots[1].Error != "" {
		t.Fatalf("missing user home disabled common worker store: %+v", roots)
	}
}
