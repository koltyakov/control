package update

import (
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"testing"
)

func TestReleaseIdentityAndBytes(t *testing.T) {
	trusted, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"version":"v1"}`)
	signed, err := trusted.Sign("https://github.com/owner/repo/.github/workflows/release.yml@refs/tags/v1", "https://token.actions.githubusercontent.com", data)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyReleaseBundle(t.Context(), trusted, "owner/repo", "v1", data, signed); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		repo, tag string
		data      []byte
	}{
		{"other/repo", "v1", data}, {"owner/repo", "v2", data}, {"owner/repo", "v1", []byte(`{"version":"v2"}`)},
	} {
		if err = verifyReleaseBundle(t.Context(), trusted, tc.repo, tc.tag, tc.data, signed); err == nil {
			t.Fatalf("accepted mismatched signature: %+v", tc)
		}
	}
	if err = VerifyReleaseManifest(t.Context(), "owner/repo", "v1", data, []byte(`{}`)); err == nil {
		t.Fatal("accepted missing signature")
	}
}
