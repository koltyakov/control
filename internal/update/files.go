package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/processutil"
	"github.com/koltyakov/control/internal/store"
)

// Verify checks the actual bytes on every call. File metadata is not evidence
// of integrity: contents can change without changing size or modification time.
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
	size, err := io.Copy(hash, f)
	if err != nil {
		return err
	}
	if size != asset.Size || hex.EncodeToString(hash.Sum(nil)) != asset.SHA256 {
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
	return store.RenameInDirectory(f.Name(), path)
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
		return &DownloadStatusError{Code: resp.StatusCode, Status: resp.Status}
	}
	return SaveBinary(resp.Body, path, asset)
}

// DownloadStatusError reports a non-OK update download response.
type DownloadStatusError struct {
	Code   int
	Status string
}

func (e *DownloadStatusError) Error() string { return "download update: " + e.Status }

// gatewayRetryWindow covers a gateway restarting during a rollout. The
// gateway's reverse proxy answers 502 until the new process listens again.
const gatewayRetryWindow = 30 * time.Second

// downloadRetrying repeats a gateway download after transient proxy and
// connection failures within gatewayRetryWindow. A GET of verified bytes has
// no side effects, so retrying it is safe; checksum and disk errors are not
// retried. Callers holding the machine cache lock keep it, so a companion
// context keeps waiting for this download instead of fetching its own.
func downloadRetrying(ctx context.Context, client *http.Client, url, token, path string, asset Asset) error {
	deadline := time.Now().Add(gatewayRetryWindow)
	delay := 500 * time.Millisecond
	for {
		err := Download(ctx, client, url, token, path, asset)
		if err == nil || ctx.Err() != nil || !transientDownload(err) || time.Now().Add(delay).After(deadline) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(2*delay, 4*time.Second)
	}
}

func transientDownload(err error) bool {
	var status *DownloadStatusError
	if errors.As(err, &status) {
		switch status.Code {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF)
}

func ValidateExecutable(ctx context.Context, path, version string, asset Asset) error {
	_, err := ValidatedExecutable(ctx, path, version, asset)
	return err
}

// ValidatedExecutable reads compatibility only after authenticating the bytes.
func ValidatedExecutable(ctx context.Context, path, version string, asset Asset) (buildinfo.Info, error) {
	if err := Verify(path, asset); err != nil {
		return buildinfo.Info{}, err
	}
	info, err := InspectExecutable(ctx, path)
	if err != nil {
		return buildinfo.Info{}, err
	}
	if info.Version != version || !Matches(info, asset) {
		return buildinfo.Info{}, errors.New("staged executable does not match its manifest")
	}
	return info, nil
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
