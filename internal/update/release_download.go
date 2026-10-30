package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var repositoryPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)

// DownloadPreferred uses GitHub only for the exact authenticated gateway asset.
// Gateway storage remains a complete fallback, including for development builds.
func DownloadPreferred(ctx context.Context, repository, version, gateway, token, path string, asset Asset) error {
	releases := ""
	parts := strings.Split(repository, "/")
	if repositoryPattern.MatchString(repository) && parts[0] != "." && parts[0] != ".." && parts[1] != "." && parts[1] != ".." {
		releases = "https://github.com/" + repository + "/releases/download/" + url.PathEscape(version)
	}
	return downloadPreferred(ctx, &http.Client{Timeout: 2 * time.Minute}, repository, version, releases, gateway, token, path, asset, VerifyReleaseManifest)
}

func downloadPreferred(ctx context.Context, client *http.Client, repository, version, releases, gateway, token, path string, asset Asset, verify func(context.Context, string, string, []byte, []byte) error) error {
	if Verify(path, asset) == nil {
		return nil
	}
	if releases != "" && versionPattern.MatchString(version) {
		if err := downloadMatchingRelease(ctx, client, repository, version, releases, path, asset, verify); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		} else {
			slog.Debug("GitHub update unavailable or different; using gateway", "version", version, "error", err)
		}
	}
	// Never forward a fleet credential through a redirected binary request.
	fallback := *client
	fallback.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return Download(ctx, &fallback, gateway, token, path, asset)
}

func downloadMatchingRelease(ctx context.Context, client *http.Client, repository, version, releases, path string, asset Asset, verify func(context.Context, string, string, []byte, []byte) error) error {
	metadataCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := releaseMetadata(metadataCtx, client, releases+"/control-manifest.json", 64<<10)
	if err != nil {
		return err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	selected, ok := manifest.Asset(asset.OS, asset.Arch)
	if manifest.Version != version || !ok || selected != asset {
		return errors.New("GitHub release differs from the gateway selection")
	}
	proof, err := releaseMetadata(metadataCtx, client, releases+"/control-manifest.json.sigstore.json", 1<<20)
	if err != nil {
		return err
	}
	if err := verify(metadataCtx, repository, version, data, proof); err != nil {
		return err
	}
	// A stalled public asset must not prevent reaching the gateway fallback.
	downloadCtx, stop := context.WithTimeout(ctx, time.Minute)
	defer stop()
	return Download(downloadCtx, client, releases+"/"+asset.File, "", path, asset)
}

func releaseMetadata(ctx context.Context, client *http.Client, target string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub release metadata: %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("GitHub release metadata exceeds limit")
	}
	return data, err
}
