package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/update"
)

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}
type release struct {
	Tag        string         `json:"tag_name"`
	Published  time.Time      `json:"published_at"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

func (g *Gateway) releaseRequest(ctx context.Context, target, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if g.options.ReleaseToken != "" {
		req.Header.Set("Authorization", "Bearer "+g.options.ReleaseToken)
	}
	return (&http.Client{Timeout: 2 * time.Minute}).Do(req)
}

func (g *Gateway) fetchRelease(ctx context.Context) (*update.Deployment, error) {
	if g.options.ReleaseRepo == "" {
		return nil, errors.New("release repository is not configured")
	}
	parts := strings.Split(g.options.ReleaseRepo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, errors.New("release repository must be owner/repository")
	}
	base := strings.TrimRight(g.options.ReleaseAPI, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	target := base + "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/releases/latest"
	resp, err := g.releaseRequest(ctx, target, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub release lookup: %s", resp.Status)
	}
	var latest release
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&latest); err != nil {
		return nil, err
	}
	if latest.Draft || latest.Prerelease || latest.Tag == "" {
		return nil, errors.New("latest release is not a stable published release")
	}
	current := g.updates.Current()
	if current != nil && (current.Manifest.Version == latest.Tag || !latest.Published.After(current.Manifest.CreatedAt)) {
		return current, nil
	}
	assets := map[string]string{}
	for _, asset := range latest.Assets {
		assets[asset.Name] = asset.URL
	}
	manifestURL := assets["control-manifest.json"]
	if manifestURL == "" {
		return nil, errors.New("release lacks control-manifest.json")
	}
	if err = validateReleaseURL(base, manifestURL); err != nil {
		return nil, err
	}
	response, err := g.releaseRequest(ctx, manifestURL, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release manifest: %s", response.Status)
	}
	var manifest update.Manifest
	if err = json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&manifest); err != nil {
		return nil, err
	}
	if manifest.Version != latest.Tag {
		return nil, errors.New("release tag and manifest version differ")
	}
	manifest.CreatedAt = latest.Published
	if err = g.validatePlatforms(manifest); err != nil {
		return nil, err
	}
	for _, asset := range manifest.Assets {
		assetURL := assets[asset.File]
		if assetURL == "" {
			return nil, fmt.Errorf("release lacks %s", asset.File)
		}
		if err = validateReleaseURL(base, assetURL); err != nil {
			return nil, err
		}
		if err = update.Download(ctx, &http.Client{Timeout: 2 * time.Minute}, assetURL, g.options.ReleaseToken, g.updates.Blob(asset.SHA256), asset); err != nil {
			return nil, err
		}
	}
	g.rolloutMu.Lock()
	defer g.rolloutMu.Unlock()
	// A development upload may have arrived while release assets downloaded.
	if current := g.updates.Current(); current != nil && !latest.Published.After(current.Manifest.CreatedAt) {
		return current, nil
	}
	return g.updates.Publish(manifest, "github:"+g.options.ReleaseRepo)
}

func validateReleaseURL(base, target string) error {
	b, err := url.Parse(base)
	if err != nil {
		return err
	}
	u, err := url.Parse(target)
	if err != nil {
		return err
	}
	if u.Scheme != b.Scheme || u.Host != b.Host || u.User != nil {
		return errors.New("release asset URL must belong to the configured API origin")
	}
	return nil
}
