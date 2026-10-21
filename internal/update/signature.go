package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

// VerifyReleaseManifest authenticates the exact downloaded bytes against the
// release workflow at the exact tag. TUF supplies Sigstore's rotating trust roots.
func VerifyReleaseManifest(ctx context.Context, repository, tag string, manifest, proof []byte) error {
	if len(manifest) > 64<<10 || len(proof) > 1<<20 {
		return errors.New("release signature input exceeds limit")
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(proof); err != nil {
		return fmt.Errorf("release signature bundle: %w", err)
	}
	opts := tuf.DefaultOptions().WithContext(ctx).WithDisableLocalCache().WithFetcher(trustFetcher{ctx})
	trusted, err := root.FetchTrustedRootWithOptions(opts)
	if err != nil {
		return fmt.Errorf("sigstore trust roots: %w", err)
	}
	return verifyReleaseBundle(ctx, trusted, repository, tag, manifest, &b)
}

func verifyReleaseBundle(ctx context.Context, trusted root.TrustedMaterial, repository, tag string, manifest []byte, b verify.SignedEntity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	verifier, err := verify.NewVerifier(trusted, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return err
	}
	identity, err := verify.NewShortCertificateIdentity("https://token.actions.githubusercontent.com", "", "https://github.com/"+repository+"/.github/workflows/release.yml@refs/tags/"+tag, "")
	if err != nil {
		return err
	}
	_, err = verifier.Verify(b, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(manifest)), verify.WithCertificateIdentity(identity)))
	if err != nil {
		return fmt.Errorf("release signature verification: %w", err)
	}
	return ctx.Err()
}

type trustFetcher struct{ ctx context.Context }

func (f trustFetcher) DownloadFile(target string, limit int64, _ time.Duration) ([]byte, error) {
	req, err := http.NewRequestWithContext(f.ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &metadata.ErrDownloadHTTP{StatusCode: resp.StatusCode, URL: target}
	}
	limit = min(limit, 16<<20)
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("trust metadata exceeds limit")
	}
	return data, err
}
