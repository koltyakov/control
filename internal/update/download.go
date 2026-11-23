package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/store"
)

// Stalls, not total duration, bound a binary download: a slow link must
// still finish a large release asset. Variables only so tests can shorten them.
var (
	downloadHeaderTimeout = 30 * time.Second
	downloadIdleTimeout   = 30 * time.Second
)

var (
	errDownloadStalled = errors.New("update download stalled")
	errRangeMismatch   = errors.New("update download resume range mismatch")
	errChecksum        = errors.New("update binary size or checksum mismatch")
)

// Download fetches one binary and publishes it only after verifying it. The
// client's whole-request timeout is replaced by header and idle-read timeouts.
func Download(ctx context.Context, client *http.Client, url, token, path string, asset Asset) error {
	if Verify(path, asset) == nil {
		return nil
	}
	s, err := newStagedDownload(path)
	if err != nil {
		return err
	}
	defer s.discard()
	if _, err := s.fetch(ctx, client, url, token, asset); err != nil {
		return err
	}
	return s.publish(path, asset)
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
// connection failures. A GET of verified bytes has no side effects, so
// retrying it is safe; disk errors are not retried. Received bytes are kept and
// the next attempt resumes with a Range request, and progress renews the retry
// window. A checksum mismatch after resuming refetches once from offset zero.
// Callers holding the machine cache lock keep it, so a companion context keeps
// waiting for this download instead of fetching its own.
func downloadRetrying(ctx context.Context, client *http.Client, url, token, path string, asset Asset) error {
	if Verify(path, asset) == nil {
		return nil
	}
	s, err := newStagedDownload(path)
	if err != nil {
		return err
	}
	defer s.discard()
	deadline := time.Now().Add(gatewayRetryWindow)
	delay := 500 * time.Millisecond
	restarted := false
	for {
		progressed, err := s.fetch(ctx, client, url, token, asset)
		if err == nil {
			if err = s.publish(path, asset); err == nil {
				return nil
			}
			if !errors.Is(err, errChecksum) || !s.resumed || restarted {
				return err
			}
			// The bytes before the resume point may belong to a different
			// response. Never stage them; start over once.
			if err = s.reset(); err != nil {
				return err
			}
			restarted = true
			continue
		}
		if ctx.Err() != nil || !transientDownload(err) {
			return err
		}
		if progressed {
			deadline, delay = time.Now().Add(gatewayRetryWindow), 500*time.Millisecond
		}
		if time.Now().Add(delay).After(deadline) {
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
	if errors.Is(err, errDownloadStalled) || errors.Is(err, errRangeMismatch) {
		return true
	}
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

// stagedDownload accumulates one asset in a private temporary file beside its
// destination. It never publishes incomplete or unverified data.
type stagedDownload struct {
	f       *os.File
	hash    hash.Hash
	size    int64
	resumed bool
}

func newStagedDownload(path string) (*stagedDownload, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".update-*")
	if err != nil {
		return nil, err
	}
	return &stagedDownload{f: f, hash: sha256.New()}, nil
}

func (s *stagedDownload) discard() {
	_ = s.f.Close()
	_ = os.Remove(s.f.Name())
}

func (s *stagedDownload) reset() error {
	if err := s.f.Truncate(0); err != nil {
		return err
	}
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	s.hash.Reset()
	s.size, s.resumed = 0, false
	return nil
}

// fetch appends the remaining bytes of asset and reports whether any arrived.
func (s *stagedDownload) fetch(ctx context.Context, client *http.Client, url, token string, asset Asset) (bool, error) {
	if s.size > asset.Size {
		if err := s.reset(); err != nil {
			return false, err
		}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := time.AfterFunc(downloadHeaderTimeout, func() { cancel(errDownloadStalled) })
	defer stall.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/octet-stream")
	if s.size > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(s.size, 10)+"-")
	}
	unbounded := *client
	unbounded.Timeout = 0
	resp, err := unbounded.Do(req)
	if err != nil {
		return false, stallCause(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		if s.size > 0 {
			if err := s.reset(); err != nil {
				return false, err
			}
		}
	case http.StatusPartialContent:
		if s.size == 0 || !rangeStartsAt(resp.Header.Get("Content-Range"), s.size, asset.Size) {
			if err := s.reset(); err != nil {
				return false, err
			}
			return false, errRangeMismatch
		}
		s.resumed = true
	case http.StatusRequestedRangeNotSatisfiable:
		if err := s.reset(); err != nil {
			return false, err
		}
		return false, errRangeMismatch
	default:
		return false, &DownloadStatusError{Code: resp.StatusCode, Status: resp.Status}
	}
	stall.Reset(downloadIdleTimeout)
	body := &idleReader{r: resp.Body, stall: stall}
	n, err := io.Copy(io.MultiWriter(s.f, s.hash), io.LimitReader(body, asset.Size-s.size+1))
	s.size += n
	if err != nil {
		return n > 0, stallCause(ctx, err)
	}
	return n > 0, nil
}

func (s *stagedDownload) publish(path string, asset Asset) error {
	if s.size != asset.Size || hex.EncodeToString(s.hash.Sum(nil)) != asset.SHA256 {
		return errChecksum
	}
	if err := s.f.Chmod(0755); err != nil {
		return err
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	if err := s.f.Close(); err != nil {
		return err
	}
	return store.RenameInDirectory(s.f.Name(), path)
}

// rangeStartsAt accepts "bytes START-END/TOTAL" for the expected resume point.
func rangeStartsAt(header string, start, total int64) bool {
	spec, ok := strings.CutPrefix(header, "bytes ")
	if !ok {
		return false
	}
	span, size, ok := strings.Cut(spec, "/")
	if !ok || (size != "*" && size != strconv.FormatInt(total, 10)) {
		return false
	}
	first, _, ok := strings.Cut(span, "-")
	return ok && first == strconv.FormatInt(start, 10)
}

func stallCause(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errDownloadStalled) {
		return fmt.Errorf("%w: %w", errDownloadStalled, err)
	}
	return err
}

// idleReader renews the stall deadline whenever bytes arrive.
type idleReader struct {
	r     io.Reader
	stall *time.Timer
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.stall.Reset(downloadIdleTimeout)
	}
	return n, err
}
