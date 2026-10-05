package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/processutil"
)

func Verify(path string, asset Asset) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != asset.Size {
		return errors.New("update binary size mismatch")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != asset.SHA256 {
		return errors.New("update binary checksum mismatch")
	}
	return nil
}

// SaveBinary never publishes incomplete or unverified data.
func SaveBinary(reader io.Reader, path string, asset Asset) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".update-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(reader, asset.Size+1))
	if err != nil {
		return err
	}
	if size != asset.Size || hex.EncodeToString(hash.Sum(nil)) != asset.SHA256 {
		return errors.New("update binary size or checksum mismatch")
	}
	if err = f.Chmod(0755); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func Download(ctx context.Context, client *http.Client, url, token, path string, asset Asset) error {
	if Verify(path, asset) == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download update: %s", resp.Status)
	}
	return SaveBinary(resp.Body, path, asset)
}

func ValidateExecutable(ctx context.Context, path, version string, asset Asset) error {
	if err := Verify(path, asset); err != nil {
		return err
	}
	info, err := InspectExecutable(ctx, path)
	if err != nil {
		return err
	}
	if info.Version != version || !Matches(info, asset) {
		return errors.New("staged executable does not match its manifest")
	}
	return nil
}

func InspectExecutable(ctx context.Context, path string) (buildinfo.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version", "--json")
	processutil.HideWindow(cmd)
	cmd.WaitDelay = time.Second
	var output limitedOutput
	cmd.Stdout = &output
	err := cmd.Run()
	if err != nil {
		return buildinfo.Info{}, fmt.Errorf("validate staged executable: %w", err)
	}
	var info buildinfo.Info
	if json.Unmarshal(output.data, &info) != nil {
		return info, errors.New("invalid executable version response")
	}
	return info, nil
}

type limitedOutput struct{ data []byte }

func (b *limitedOutput) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 4096 {
		return 0, errors.New("version output exceeds limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
