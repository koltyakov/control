package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/koltyakov/control/internal/secrets"

	"github.com/shirou/gopsutil/v4/disk"
)

const (
	// defaultDownloadMaxBytes applies when http.download omits maxBytes.
	defaultDownloadMaxBytes int64 = 1 << 30
	// maxDownloadBytes bounds an explicit maxBytes; maxArtifactBytes and the
	// free-space reserve still apply beneath it.
	maxDownloadBytes int64 = 1 << 40
	// Stalls, not total duration, bound a download: no response headers or
	// no received body bytes for this long ends the attempt (see D173).
	defaultDownloadStall = 30 * time.Second
	maxDownloadStall     = time.Hour
	maxDownloadRedirects = 10
)

var errDownloadStalled = errors.New("HTTP download stalled")

// downloadHeaders are the response headers returned by http.download. The body
// is never returned inline.
var downloadHeaders = []string{"Content-Type", "Content-Length", "Content-Disposition", "ETag", "Last-Modified", "Location"}

// httpDownload streams one GET response body into a content-addressed
// artifact held by the invocation's owner. Bytes pass through the artifact
// import staging file, budget and free-space checks; any failure, including
// a size, stall or checksum error, removes the staging file and publishes
// nothing. There is no retry or Range resume.
func (n *Node) httpDownload(ctx context.Context, args json.RawMessage, e Execution) (_ any, runErr error) {
	var input struct {
		URL           string            `json:"url"`
		Method        string            `json:"method"`
		Headers       map[string]string `json:"headers"`
		HeaderSecrets map[string]struct {
			Secret string `json:"secret"`
			Prefix string `json:"prefix"`
		} `json:"headerSecrets"`
		SHA256       string `json:"sha256"`
		MaxBytes     int64  `json:"maxBytes"`
		Name         string `json:"name"`
		StallSeconds int    `json:"stallSeconds"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}
	if len(input.HeaderSecrets) > 32 {
		return nil, errors.New("at most 32 secret headers are allowed")
	}
	if input.Method != "" && !strings.EqualFold(input.Method, http.MethodGet) {
		return nil, errors.New("http.download supports only GET; use http.request for other methods")
	}
	expected := strings.ToLower(input.SHA256)
	if expected != "" && !digestID.MatchString(expected) {
		return nil, errors.New("sha256 must be 64 hexadecimal characters")
	}
	if input.MaxBytes == 0 {
		input.MaxBytes = defaultDownloadMaxBytes
	}
	if input.MaxBytes < 1 || input.MaxBytes > maxDownloadBytes {
		return nil, fmt.Errorf("maxBytes must be 1..%d", maxDownloadBytes)
	}
	stall := defaultDownloadStall
	if input.StallSeconds != 0 {
		stall = time.Duration(input.StallSeconds) * time.Second
		if stall < time.Second || stall > maxDownloadStall {
			return nil, errors.New("stallSeconds must be 1..3600")
		}
	}
	if input.Name != "" && (len(input.Name) > 255 || !utf8.ValidString(input.Name) || strings.ContainsAny(input.Name, "/\\\x00") || input.Name == "." || input.Name == "..") {
		return nil, errors.New("name must be a file name of at most 255 bytes")
	}
	values, err := n.secretValues()
	if err != nil {
		return nil, err
	}
	redactor := secrets.NewRedactor(values)
	defer func() {
		if runErr != nil && redactor.Text(runErr.Error()) != runErr.Error() {
			runErr = errors.New(redactor.Text(runErr.Error()))
		}
	}()
	// The stall timer covers response headers, then renews with each body
	// read. The invocation or task deadline still bounds the whole download.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(stall, func() { cancel(errDownloadStalled) })
	defer timer.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, input.URL, nil)
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, errors.New("URL must use http or https")
	}
	for k, v := range input.Headers {
		req.Header.Set(k, v)
	}
	for k, ref := range input.HeaderSecrets {
		if req.Header.Values(k) != nil {
			return nil, errors.New("header cannot have both a literal value and a secret reference")
		}
		value, err := resolveSecret(values, ref.Secret)
		if err != nil {
			return nil, err
		}
		req.Header.Set(k, ref.Prefix+value)
	}
	name := input.Name
	if name == "" {
		name = downloadName(req.URL.Path)
	}
	// Store the entity exactly as sent: transparent gzip decoding would
	// change the bytes a caller's sha256 describes.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport}
	if len(input.HeaderSecrets) != 0 {
		httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	} else {
		httpClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
			if len(via) >= maxDownloadRedirects {
				return fmt.Errorf("stopped after %d redirects", maxDownloadRedirects)
			}
			if next.URL.Scheme != "http" && next.URL.Scheme != "https" {
				return errors.New("redirect URL must use http or https")
			}
			return nil
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, stallCause(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	result := map[string]any{"status": resp.StatusCode, "headers": selectedHeaders(resp.Header, redactor)}
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("HTTP download requires 200 OK, got %s; nothing was published", resp.Status)
	}
	if resp.ContentLength > input.MaxBytes {
		return result, fmt.Errorf("HTTP response Content-Length %d exceeds maxBytes %d", resp.ContentLength, input.MaxBytes)
	}
	if err := n.admitDownload(resp.ContentLength); err != nil {
		return result, err
	}
	timer.Reset(stall)
	body := &downloadReader{r: resp.Body, timer: timer, stall: stall, limit: input.MaxBytes, expected: expected}
	if expected != "" {
		body.hash = sha256.New()
	}
	artifact, err := n.importArtifact(ctx, body, name)
	if err != nil {
		return result, stallCause(ctx, err)
	}
	result["artifact"] = artifact
	return result, nil
}

// admitDownload refuses a response whose declared length cannot fit the
// artifact budget or the free-space reserve. Streaming writes repeat both
// checks, so an unknown or false length is still bounded.
func (n *Node) admitDownload(length int64) error {
	if length < 0 {
		return nil
	}
	n.artifactBudgetMu.Lock()
	available := n.Config.MaxArtifactBytes - n.artifactUsed
	n.artifactBudgetMu.Unlock()
	if n.Config.MaxArtifactBytes > 0 && length > available {
		return errors.New("HTTP response exceeds the artifact storage budget; release unused artifacts or increase maxArtifactBytes")
	}
	usage, err := disk.Usage(n.Config.DataDir)
	if err != nil {
		return err
	}
	if usage.Free < n.Config.ArtifactReserveBytes+uint64(length) {
		return errors.New("HTTP response would consume the artifact disk reserve")
	}
	return nil
}

// downloadReader renews the stall timer on progress, enforces maxBytes and,
// when a digest is expected, fails at EOF on a mismatch so the import never
// publishes unverified content.
type downloadReader struct {
	r        io.Reader
	timer    *time.Timer
	stall    time.Duration
	limit    int64
	read     int64
	hash     hash.Hash
	expected string
}

func (r *downloadReader) Read(p []byte) (int, error) {
	if remaining := r.limit + 1 - r.read; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.r.Read(p)
	if err != nil {
		// Publication after EOF (sync, rename, metadata) must not be
		// mistaken for a stalled server.
		r.timer.Stop()
	}
	if n > 0 {
		if err == nil {
			r.timer.Reset(r.stall)
		}
		r.read += int64(n)
		if r.read > r.limit {
			return 0, fmt.Errorf("HTTP response exceeds maxBytes %d", r.limit)
		}
		if r.hash != nil {
			_, _ = r.hash.Write(p[:n])
		}
	}
	if errors.Is(err, io.EOF) && r.hash != nil && hex.EncodeToString(r.hash.Sum(nil)) != r.expected {
		return n, errors.New("HTTP download sha256 mismatch; nothing was published")
	}
	return n, err
}

func stallCause(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errDownloadStalled) {
		return fmt.Errorf("%w: %w", errDownloadStalled, err)
	}
	return err
}

func selectedHeaders(header http.Header, redactor secrets.Redactor) map[string]string {
	selected := map[string]string{}
	for _, name := range downloadHeaders {
		if value := header.Get(name); value != "" {
			selected[name] = redactor.Text(value)
		}
	}
	return selected
}

// downloadName derives artifact metadata from the final URL path segment.
func downloadName(urlPath string) string {
	name := path.Base(urlPath)
	if name == "." || name == "/" || name == "" || len(name) > 255 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") {
		return "download"
	}
	return name
}
