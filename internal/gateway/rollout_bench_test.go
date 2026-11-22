package gateway

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

// This isolates the recurring applied-rollout reconciliation without sockets,
// filesystem cleanup, telemetry collection or persisted state mutations.
func BenchmarkAppliedRolloutReconciliation(b *testing.B) {
	for _, count := range []int{20, 1000} {
		b.Run(fmt.Sprintf("nodes=%d", count), func(b *testing.B) {
			repo, err := update.OpenRepository(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			data := "rollout benchmark"
			asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(data)))}
			if err := repo.Upload(strings.NewReader(data), asset); err != nil {
				b.Fatal(err)
			}
			d, err := repo.Publish(update.Manifest{Version: "bench-v1", Assets: []update.Asset{asset}}, "benchmark")
			if err != nil {
				b.Fatal(err)
			}
			g := &Gateway{updates: repo, nodes: map[string]model.Node{}, peers: map[string]*connection{}, clientOwners: map[string]bool{}, updateStatus: map[string]update.Status{}}
			info := buildinfo.Info{Version: d.Manifest.Version, OS: asset.OS, Arch: asset.Arch, SHA256: asset.SHA256, Runtime: "single"}
			ids := make([]string, 0, count)
			refreshed := time.Now()
			for i := range count {
				id := fmt.Sprintf("node-%06d", i)
				ids = append(ids, id)
				g.nodes[id] = model.Node{ID: id, Online: true, OS: info.OS, Software: info}
				g.peers[id] = &connection{}
				g.updateStatus[id] = update.Status{ID: d.ID, State: "applied", Software: info, SeenAt: refreshed}
			}
			if err := repo.BeginInstall(d.ID, ids); err != nil {
				b.Fatal(err)
			}
			if err := repo.SetPhase(d.ID, "complete", nil); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				// Keep synthetic reports fresh even during a long benchmark.
				now := time.Now()
				if now.Sub(refreshed) > updateStatusFreshness/2 {
					for id, status := range g.updateStatus {
						status.SeenAt = now
						g.updateStatus[id] = status
					}
					refreshed = now
				}
				d, err := g.reconcileDeferred(repo.Current(), now)
				if err != nil {
					b.Fatal(err)
				}
				peers := g.rolloutPeers(d)
				if applied, err := g.advanceNodes(d, peers, true, false, func(string, string, update.Command) {
					b.Fatal("applied node received an update command")
				}); err != nil || !applied {
					b.Fatal("applied rollout changed", applied, err)
				}
				if repo.Current() == nil {
					b.Fatal("missing deployment")
				}
			}
		})
	}
}
