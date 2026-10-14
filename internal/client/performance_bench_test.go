package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
)

// Real in-process gateway, enrolled nodes, account client, TLS, and transport.
// Setup and warm-up are outside the benchmark timer. No installed services,
// user credentials, public STUN servers, or external fleets are involved.
func benchmarkFleet(b *testing.B, count int, relay bool) (Client, Admin, []*node.Node) {
	b.Helper()
	const key = "performance-test-superuser-key-1234567890"
	g, err := gateway.New(b.TempDir(), "", gateway.Options{SuperuserKey: key})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = g.Close() })
	s := httptest.NewServer(g.Handler())
	b.Cleanup(s.Close)
	admin := Admin{URL: s.URL, Key: key}
	workers := make([]*node.Node, count)
	for i := range workers {
		w, err := node.New(node.Config{Name: fmt.Sprintf("worker-%03d", i), Gateway: s.URL, Token: key, DataDir: b.TempDir(), WorkDir: b.TempDir(), RelayOnly: relay, MetricsIntervalSeconds: -1})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = w.Close() })
		if err := w.Start(b.Context()); err != nil {
			b.Fatal(err)
		}
		workers[i] = w
	}
	c := (Client{}).WithStandalone(b.Context(), StandaloneConfig{Gateway: admin, StateDir: b.TempDir(), RelayOnly: relay, AccountRouting: true})
	b.Cleanup(func() { _ = c.Close() })
	return c, admin, workers
}

func BenchmarkFleetDashboard(b *testing.B) {
	for _, count := range []int{16, 64} {
		for _, cached := range []bool{false, true} {
			b.Run(fmt.Sprintf("nodes=%d/cached=%v", count, cached), func(b *testing.B) {
				c, admin, _ := benchmarkFleet(b, count, true)
				if !cached {
					c.pool = nil
				}
				query := model.PoolActivityQuery{Recent: 5}
				if _, err := c.Dashboard(b.Context(), admin, query); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				for b.Loop() {
					snapshot, err := c.Dashboard(b.Context(), admin, query)
					if err != nil || len(snapshot.Nodes) != count {
						b.Fatal("dashboard", err)
					}
					for _, n := range snapshot.Nodes {
						if n.Status != "ready" {
							b.Fatalf("missing peer observation for %s: %s %s", n.Name, n.Status, n.Error)
						}
					}
				}
			})
		}
	}
}

func BenchmarkArtifactDownload(b *testing.B) {
	for _, relay := range []bool{false, true} {
		b.Run(fmt.Sprintf("relay=%v/16MiB", relay), func(b *testing.B) {
			c, _, workers := benchmarkFleet(b, 1, relay)
			payload := bytes.Repeat([]byte("artifact payload"), 1<<20)
			if err := os.WriteFile(filepath.Join(workers[0].Config.WorkDir, "input.bin"), payload, 0600); err != nil {
				b.Fatal(err)
			}
			var artifact model.Artifact
			if err := c.Call(b.Context(), workers[0].Config.Name, "artifacts.export", map[string]string{"path": "input.bin"}, &artifact); err != nil {
				b.Fatal(err)
			}
			want := sha256.Sum256(payload)
			download := func(ctx context.Context) {
				hash := sha256.New()
				if err := c.Download(ctx, artifact, 0, hash); err != nil {
					b.Fatal(err)
				}
				if hex.EncodeToString(hash.Sum(nil)) != hex.EncodeToString(want[:]) {
					b.Fatal("artifact checksum mismatch")
				}
			}
			download(b.Context())
			wantMode := "webrtc"
			if relay {
				wantMode = "relay"
			}
			if got := c.routing.peer.Connections()[workers[0].Identity.ID]; got != wantMode {
				b.Fatalf("transport %s, want %s", got, wantMode)
			}
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for b.Loop() {
				download(b.Context())
			}
		})
	}
}
