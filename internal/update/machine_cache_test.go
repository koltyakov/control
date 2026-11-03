package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
)

func cacheFixture(data []byte) Asset {
	sum := sha256.Sum256(data)
	return Asset{OS: "linux", Arch: "amd64", File: AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

func TestUpdaterSharesMachineDownloadAcrossProfiles(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	info := buildinfo.Current()
	asset := Asset{OS: info.OS, Arch: info.Arch, File: AssetName(info.OS, info.Arch), Size: stat.Size(), SHA256: info.SHA256}
	manifest := Manifest{Version: info.Version, Assets: []Asset{asset}}
	selection := Selection{ID: manifest.ID(), Manifest: manifest}
	old := info
	old.Version, old.SHA256 = "older-version", strings.Repeat("0", 64)
	for _, startup := range []bool{false, true} {
		for _, concurrent := range []bool{false, true} {
			t.Run(fmt.Sprintf("startup=%t/concurrent=%t", startup, concurrent), func(t *testing.T) {
				var downloads atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer node-key" {
						t.Error("gateway authentication missing")
					}
					switch r.URL.Path {
					case "/v1/updates":
						_ = json.NewEncoder(w).Encode(selection)
					case "/v1/updates/blobs/" + asset.SHA256:
						downloads.Add(1)
						http.ServeFile(w, r, executable)
					default:
						t.Error("unexpected request", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				machine := t.TempDir()
				stage := func(role string) {
					a, err := NewUpdater(UpdaterOptions{Gateway: server.URL, Token: "node-key", Software: old,
						Dir: filepath.Join(machine, role), CacheDir: filepath.Join(machine, "cache"),
						Busy: func() bool { return false }, Pause: func() bool { return true }, Resume: func() {},
						Report: func(context.Context, Status) error { return nil },
						Apply:  func(string) error { t.Error("staging restarted a profile"); return nil },
					})
					if err != nil {
						t.Error(err)
						return
					}
					if startup {
						if err := a.checkSelection(t.Context()); err != nil {
							t.Error(err)
						}
					} else {
						a.process(t.Context(), queuedCommand{"update.offer", Command{ID: selection.ID, Version: manifest.Version, Asset: asset}})
					}
					if s := a.Snapshot(); s.State != "staged" || s.Paused || a.staged != a.stagePath(asset) {
						t.Errorf("profile did not stage independently: %+v", s)
					}
				}
				var wg sync.WaitGroup
				for _, role := range []string{"user", "system"} {
					if concurrent {
						wg.Go(func() { stage(role) })
					} else {
						stage(role)
					}
				}
				wg.Wait()
				// New updater objects also reuse each profile's private staged file.
				stage("user")
				stage("system")
				if n := downloads.Load(); n != 1 {
					t.Fatalf("User and System downloaded %d times, want 1", n)
				}
				user := filepath.Join(machine, "user", "updates", asset.SHA256, asset.File)
				system := filepath.Join(machine, "system", "updates", asset.SHA256, asset.File)
				first, err := os.Stat(user)
				if err != nil {
					t.Fatal(err)
				}
				second, err := os.Stat(system)
				if err != nil || os.SameFile(first, second) {
					t.Fatal("profiles share a writable runtime inode", err)
				}
				shared, err := os.Stat(filepath.Join(machine, "cache", asset.SHA256+".blob"))
				if err != nil || os.SameFile(first, shared) || os.SameFile(second, shared) {
					t.Fatal("private runtime is linked to the writable cache", err)
				}
			})
		}
	}
}

func TestMachineCachePreservesReleasePreference(t *testing.T) {
	data := []byte("trusted bytes")
	asset := cacheFixture(data)
	manifest := Manifest{Version: "v1", Assets: []Asset{asset}}
	for _, source := range []string{"github", "development"} {
		t.Run(source, func(t *testing.T) {
			var public, gateway atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/release/control-manifest.json":
					if r.Header.Get("Authorization") != "" {
						t.Error("credential leaked to GitHub")
					}
					if source == "development" {
						http.NotFound(w, r)
						return
					}
					_ = json.NewEncoder(w).Encode(manifest)
				case "/release/control-manifest.json.sigstore.json":
					_, _ = w.Write([]byte("fixture proof"))
				case "/release/" + asset.File:
					public.Add(1)
					_, _ = w.Write(data)
				case "/gateway":
					gateway.Add(1)
					if r.Header.Get("Authorization") != "Bearer node-key" {
						t.Error("gateway credential missing")
					}
					_, _ = w.Write(data)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cache := filepath.Join(t.TempDir(), "cache")
			for range 2 {
				path := filepath.Join(t.TempDir(), "binary")
				err := downloadCached(t.Context(), cache, path, asset, func() error {
					return downloadPreferred(t.Context(), server.Client(), "owner/repo", "v1", server.URL+"/release", server.URL+"/gateway", "node-key", path, asset,
						func(context.Context, string, string, []byte, []byte) error { return nil })
				})
				if err != nil || Verify(path, asset) != nil {
					t.Fatal("could not stage verified bytes", err)
				}
			}
			if public.Load()+gateway.Load() != 1 || (source == "github" && public.Load() != 1) || (source == "development" && gateway.Load() != 1) {
				t.Fatal("cache changed delivery preference or downloaded twice", public.Load(), gateway.Load())
			}
		})
	}
}

func TestMachineCacheRejectsCorruptAndLinkedBytes(t *testing.T) {
	data := []byte("trusted bytes")
	asset := cacheFixture(data)
	for _, scenario := range []string{"corrupt", "truncated", "symlink", "relative-symlink", "directory"} {
		t.Run(scenario, func(t *testing.T) {
			cache := filepath.Join(t.TempDir(), "cache")
			root, err := openMachineCache(cache)
			if err != nil {
				t.Fatal(err)
			}
			_ = root.Close()
			name := filepath.Join(cache, asset.SHA256+".blob")
			outside := filepath.Join(t.TempDir(), "untouched")
			if err := os.WriteFile(outside, data, 0600); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "corrupt":
				err = os.WriteFile(name, bytes.Repeat([]byte("x"), len(data)), 0644)
			case "truncated":
				err = os.WriteFile(name, data[:2], 0644)
			case "symlink":
				err = os.Symlink(outside, name)
				if err != nil {
					t.Skip("symlink creation unavailable", err)
				}
			case "relative-symlink":
				if err := os.WriteFile(filepath.Join(cache, "unrelated"), data, 0644); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink("unrelated", name)
				if err != nil {
					t.Skip("symlink creation unavailable", err)
				}
			case "directory":
				err = os.Mkdir(name, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			var downloads int
			path := filepath.Join(t.TempDir(), "private", "binary")
			err = downloadCached(t.Context(), cache, path, asset, func() error {
				downloads++
				return SaveBinary(bytes.NewReader(data), path, asset)
			})
			if err != nil || downloads != 1 || Verify(path, asset) != nil {
				t.Fatal("invalid cache entry was reused or prevented download", err, downloads)
			}
			// Mutating the cache after the copy cannot change private executable bytes.
			if scenario != "directory" {
				if err := os.WriteFile(name, bytes.Repeat([]byte("x"), len(data)), 0644); err != nil {
					t.Fatal(err)
				}
				if err := Verify(path, asset); err != nil {
					t.Fatal("shared cache mutation changed private binary", err)
				}
			}
			got, err := os.ReadFile(outside)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("cache touched an outside symlink target", err)
			}
		})
	}
}

func TestMachineCacheCancellationAndFailedDownloads(t *testing.T) {
	data := []byte("trusted bytes")
	asset := cacheFixture(data)
	cache := filepath.Join(t.TempDir(), "cache")
	root, err := openMachineCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lock, err := lockMachineCache(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err = downloadCached(ctx, cache, filepath.Join(t.TempDir(), "binary"), asset, func() error {
		t.Error("cancelled lock waiter started a network download")
		return nil
	})
	_ = lock.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("lock wait ignored cancellation", err)
	}
	failed := errors.New("download interrupted")
	err = downloadCached(t.Context(), cache, filepath.Join(t.TempDir(), "binary"), asset, func() error { return failed })
	if !errors.Is(err, failed) {
		t.Fatal("download error was lost", err)
	}
	if _, err := os.Stat(filepath.Join(cache, asset.SHA256+".blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed download published shared bytes", err)
	}
	path := filepath.Join(t.TempDir(), "binary")
	if err := downloadCached(t.Context(), cache, path, asset, func() error { return SaveBinary(bytes.NewReader(data), path, asset) }); err != nil {
		t.Fatal("failed download retained lock or poisoned cache", err)
	}
}

func TestMachineCacheUnavailableFallsBack(t *testing.T) {
	data := []byte("trusted bytes")
	asset := cacheFixture(data)
	for _, scenario := range []string{"missing-parent", "file", "directory-symlink", "lock-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			cache := filepath.Join(base, "cache")
			outside := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch scenario {
			case "missing-parent":
				cache = filepath.Join(base, "missing", "cache")
			case "file":
				err = os.WriteFile(cache, nil, 0600)
			case "directory-symlink":
				err = os.Symlink(filepath.Dir(outside), cache)
			case "lock-symlink":
				var root *os.Root
				root, err = openMachineCache(cache)
				if err == nil {
					_ = root.Close()
					err = os.Symlink(outside, filepath.Join(cache, "download.lock"))
				}
			}
			if err != nil {
				t.Skip("cache fixture unavailable", err)
			}
			path := filepath.Join(t.TempDir(), "binary")
			calls := 0
			err = downloadCached(t.Context(), cache, path, asset, func() error {
				calls++
				return SaveBinary(bytes.NewReader(data), path, asset)
			})
			if err != nil || calls != 1 || Verify(path, asset) != nil {
				t.Fatal("unavailable cache prevented safe private download", err)
			}
			got, err := os.ReadFile(outside)
			if err != nil || string(got) != "untouched" {
				t.Fatal("modified outside target", err)
			}
		})
	}
}

func TestMachineCacheRetentionAndDigestIsolation(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	root, err := openMachineCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(".download-abandoned", []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	for i := range 7 {
		data := []byte(fmt.Sprintf("binary %d", i))
		asset := cacheFixture(data)
		path := filepath.Join(t.TempDir(), "binary")
		if err := downloadCached(t.Context(), cache, path, asset, func() error { return SaveBinary(bytes.NewReader(data), path, asset) }); err != nil {
			t.Fatal(err)
		}
		if err := Verify(path, asset); err != nil {
			t.Fatal("reused wrong checksum", err)
		}
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != machineCacheEntries+1 {
		t.Fatalf("expected %d blobs and persistent lock, got %v", machineCacheEntries, entries)
	}
}

func TestMachineCachePrunePreservesUnrelatedFilesAndLinks(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	root, err := openMachineCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile("notes", []byte("untouched"), 0644); err != nil {
		t.Fatal(err)
	}
	directory := strings.Repeat("a", 64) + ".blob"
	if err := root.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	link := strings.Repeat("b", 64) + ".blob"
	if err := os.Symlink("notes", filepath.Join(cache, link)); err != nil {
		t.Skip("symlink creation unavailable", err)
	}
	pruneMachineCache(t.Context(), root, "")
	for _, name := range []string{"notes", directory, link} {
		if _, err := root.Lstat(name); err != nil {
			t.Fatal("pruned unrelated file, directory or symlink", name, err)
		}
	}
}

type cacheProcessInput struct {
	Cache, Path, URL string
	Asset            Asset
}

func TestMachineCacheProcessHelper(t *testing.T) {
	input := os.Getenv("CONTROL_TEST_MACHINE_CACHE")
	if input == "" {
		t.Skip("subprocess helper")
	}
	var p cacheProcessInput
	if err := json.Unmarshal([]byte(input), &p); err != nil {
		t.Fatal(err)
	}
	fmt.Println("machine-cache-ready")
	if err := downloadCached(t.Context(), p.Cache, p.Path, p.Asset, func() error {
		return DownloadPreferred(t.Context(), "", "v1", p.URL, "", p.Path, p.Asset)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMachineCacheAcrossProcesses(t *testing.T) {
	data := []byte("trusted cross-process binary")
	asset := cacheFixture(data)
	var downloads atomic.Int32
	requests := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		select {
		case requests <- struct{}{}:
		default:
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "cache")
	root, err := openMachineCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lock, err := lockMachineCache(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	var wg sync.WaitGroup
	for range 2 {
		p := cacheProcessInput{Cache: cache, Path: filepath.Join(t.TempDir(), "binary"), URL: server.URL, Asset: asset}
		input, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestMachineCacheProcessHelper$")
		cmd.Env = append(os.Environ(), "CONTROL_TEST_MACHINE_CACHE="+string(input))
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "machine-cache-ready\n" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("subprocess not ready: %q, %v", line, err)
		}
		wg.Go(func() {
			if err := cmd.Wait(); err != nil {
				t.Errorf("cache subprocess: %v\n%s", err, stderr.String())
			}
		})
	}
	// Both children are ready. Neither may bypass the parent's filesystem lock,
	// which also proves this is not just in-process goroutine coalescing.
	select {
	case <-requests:
		t.Error("another process downloaded while the cache lock was held")
	case <-time.After(100 * time.Millisecond):
	}
	_ = lock.Close()
	wg.Wait()
	if n := downloads.Load(); n != 1 {
		t.Fatalf("separate processes downloaded %d times, want 1", n)
	}
}
