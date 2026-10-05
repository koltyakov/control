package installation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/update"
)

type UpdateResult struct {
	Path    string
	Version string
	Changed bool
}

var releaseRepoPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)
var releaseTagPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,127}$`)

// UpdateSelf replaces the invoked CLI, independently of gateway-managed services.
func UpdateSelf(ctx context.Context, repository, version string) (UpdateResult, error) {
	if !releaseRepoPattern.MatchString(repository) {
		return UpdateResult{}, errors.New("release repository must be owner/repository; set CONTROL_RELEASE_REPO")
	}
	for _, part := range strings.Split(repository, "/") {
		if part == "." || part == ".." {
			return UpdateResult{}, errors.New("invalid release repository")
		}
	}
	path, err := os.Executable()
	if err != nil {
		return UpdateResult{}, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return UpdateResult{}, err
	}
	return updateSelf(ctx, path, "https://github.com/"+repository+"/releases", version, &http.Client{Timeout: 2 * time.Minute})
}

func updateSelf(ctx context.Context, path, releases, version string, client *http.Client) (UpdateResult, error) {
	result := UpdateResult{Path: path}
	if version != "" && !releaseTagPattern.MatchString(version) {
		return result, errors.New("invalid release tag")
	}
	lock := flock.New(path + ".update.lock")
	locked, err := lock.TryLock()
	if err != nil {
		return result, fmt.Errorf("lock CLI installation: %w", err)
	}
	if !locked {
		return result, errors.New("another CLI update is in progress")
	}
	defer func() { _ = lock.Close() }()
	base := releases + "/latest/download"
	if version != "" {
		base = releases + "/download/" + version
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/control-manifest.json", nil)
	if err != nil {
		return result, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return result, fmt.Errorf("fetch release manifest: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("fetch release manifest: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil {
		return result, err
	}
	if len(data) > 64<<10 {
		return result, errors.New("release manifest exceeds 64 KiB")
	}
	var manifest update.Manifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		return result, err
	}
	if err = manifest.Validate(); err != nil {
		return result, err
	}
	if version != "" && manifest.Version != version {
		return result, errors.New("release tag and manifest version differ")
	}
	asset, ok := manifest.Asset(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return result, fmt.Errorf("release has no binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	result.Version = manifest.Version
	if update.Verify(path, asset) == nil {
		return result, nil
	}
	// Stage beside the destination so replacement stays on the same filesystem.
	dir, err := os.MkdirTemp(filepath.Dir(path), ".control-update-*")
	if err != nil {
		return result, err
	}
	staged := filepath.Join(dir, asset.File)
	defer func() {
		_ = os.Remove(staged)
		_ = os.Remove(dir) // Retain a Windows backup if it is still running or restoration failed.
	}()
	// Pin the binary to the manifest's release even if "latest" moves meanwhile.
	url := releases + "/download/" + manifest.Version + "/" + asset.File
	if err = update.Download(ctx, client, url, "", staged, asset); err != nil {
		return result, err
	}
	if err = update.ValidateExecutable(ctx, staged, manifest.Version, asset); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = replaceCLI(staged, path); err != nil {
		return result, fmt.Errorf("replace CLI: %w", err)
	}
	result.Changed = true
	return result, nil
}
