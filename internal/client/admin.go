package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/update"
)

type Admin struct{ URL, Key string }

func (c Admin) request(ctx context.Context, method, path string, body io.Reader, size int64, result any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, body)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("gateway %s: %s", resp.Status, strings.TrimSpace(string(text)))
	}
	if result != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(result)
	}
	return nil
}

func (c Admin) IsSuperuser(ctx context.Context) bool {
	return c.Role(ctx) == "superuser"
}

func (c Admin) Role(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var info struct {
		Role string `json:"role"`
	}
	if c.Key != "" && c.request(ctx, http.MethodGet, "/v1/auth", nil, 0, &info) == nil {
		return info.Role
	}
	return ""
}

func (c Admin) JSON(ctx context.Context, method, path string, params, result any) error {
	var b []byte
	if params != nil {
		var err error
		b, err = json.Marshal(params)
		if err != nil {
			return err
		}
	}
	return c.request(ctx, method, path, bytes.NewReader(b), int64(len(b)), result)
}

func (c Admin) Push(ctx context.Context, dir string) (update.Deployment, error) {
	var manifest update.Manifest
	b, err := os.ReadFile(filepath.Join(dir, "control-manifest.json"))
	if err != nil {
		return update.Deployment{}, err
	}
	if err = json.Unmarshal(b, &manifest); err != nil {
		return update.Deployment{}, err
	}
	if err = manifest.Validate(); err != nil {
		return update.Deployment{}, err
	}
	for _, asset := range manifest.Assets {
		path := filepath.Join(dir, asset.File)
		if err = update.Verify(path, asset); err != nil {
			return update.Deployment{}, err
		}
		f, err := os.Open(path)
		if err != nil {
			return update.Deployment{}, err
		}
		err = c.request(ctx, http.MethodPut, "/v1/admin/updates/blobs/"+asset.SHA256, f, asset.Size, nil)
		_ = f.Close()
		if err != nil {
			return update.Deployment{}, err
		}
	}
	var deployment update.Deployment
	err = c.JSON(ctx, http.MethodPost, "/v1/admin/updates", manifest, &deployment)
	return deployment, err
}
