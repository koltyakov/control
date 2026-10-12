package update

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVerifiedPinsProtectAssetsUntilReferencePublication(t *testing.T) {
	r, err := OpenRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	current := retentionAsset(t, r, "selected", 2*time.Hour)
	candidate := retentionAsset(t, r, "verified installer", 2*time.Hour)
	d, err := r.Publish(Manifest{Version: "v1", Assets: []Asset{current}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetPhase(d.ID, "complete", nil); err != nil {
		t.Fatal(err)
	}
	release, err := r.VerifyPinned([]Asset{candidate, candidate})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := r.Prune(t.Context(), d.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := Verify(r.Blob(candidate.SHA256), candidate); err != nil {
		t.Fatal("verified asset was pruned before its durable reference", err)
	}
	release()
	release()
	if _, err := r.Prune(t.Context(), d.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.Blob(candidate.SHA256)); !os.IsNotExist(err) {
		t.Fatal("released asset was not pruned", err)
	}
}

func TestVerifyPinnedFailureReleasesPinsAndKeepsChecksumsStrict(t *testing.T) {
	r, err := OpenRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	asset := retentionAsset(t, r, "trusted", 2*time.Hour)
	badSize := asset
	badSize.Size++
	missing := asset
	missing.SHA256 = strings.Repeat("f", 64)
	invalid := asset
	invalid.SHA256 = "../outside"
	for _, assets := range [][]Asset{{asset, badSize}, {asset, missing}, {invalid}} {
		if release, err := r.VerifyPinned(assets); err == nil || release != nil {
			t.Fatal("invalid assets acquired verified pins", err)
		}
		if len(r.pins) != 0 {
			t.Fatal("failed verification leaked pins")
		}
	}
	if err := os.WriteFile(r.Blob(asset.SHA256), []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if release, err := r.VerifyPinned([]Asset{asset}); err == nil || release != nil {
		t.Fatal("same-size corruption bypassed verification")
	}
	if _, err := r.Publish(Manifest{Version: "v1", Assets: []Asset{asset}}, "test"); err == nil || r.Current() != nil || len(r.pins) != 0 {
		t.Fatal("corrupt publication changed deployment or leaked pins", err)
	}
}

func TestPinsOwnTheirAssetList(t *testing.T) {
	r, err := OpenRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	assets := []Asset{{SHA256: strings.Repeat("a", 64)}}
	release := r.Pin(assets)
	assets[0].SHA256 = strings.Repeat("b", 64)
	release()
	if len(r.pins) != 0 {
		t.Fatal("reusing caller metadata changed retained pins")
	}
}

func TestPublicationRacingCleanupNeverSelectsPrunedAssets(t *testing.T) {
	r, err := OpenRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		current := retentionAsset(t, r, fmt.Sprintf("current-%d", i), 2*time.Hour)
		candidate := retentionAsset(t, r, fmt.Sprintf("candidate-%d", i), 2*time.Hour)
		d, err := r.Publish(Manifest{Version: fmt.Sprintf("current-%d", i), Assets: []Asset{current}}, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.SetPhase(d.ID, "complete", nil); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var published *Deployment
		wg.Go(func() {
			<-start
			published, _ = r.Publish(Manifest{Version: fmt.Sprintf("candidate-%d", i), Assets: []Asset{candidate}}, "test")
		})
		wg.Go(func() {
			<-start
			_, _ = r.Prune(t.Context(), d.ID, nil)
		})
		close(start)
		wg.Wait()
		// Cleanup may win before the pin exists, making publication fail. A
		// successful publication must always retain its verified candidate.
		if published != nil {
			if err := Verify(r.Blob(candidate.SHA256), candidate); err != nil {
				t.Fatal("selected deployment lost its binary", err)
			}
		}
		if len(r.pins) != 0 {
			t.Fatal("publication leaked verification pins")
		}
	}
}
