package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// CONTROL_BENCH_DIR selects the filesystem, for example a Linux tmpfs mount.
// This isolates synchronous commit contention from handshakes and network I/O.
func BenchmarkGatewayCommitWithDirectoryReads(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprintf("nodes=%d", count), func(b *testing.B) {
			dir := b.TempDir()
			if base := os.Getenv("CONTROL_BENCH_DIR"); base != "" {
				var err error
				dir, err = os.MkdirTemp(base, "control-gateway-bench-")
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { _ = os.RemoveAll(dir) })
			}
			g, err := New(dir, commonKey)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = g.Close() })
			g.metricsCancel()
			g.metricsWG.Wait()
			var initial change
			for i := range count {
				id := fmt.Sprintf("node-%06d", i)
				g.nodes[id] = model.Node{ID: id, Name: id, UserID: legacyUser}
				g.owners[id] = legacyUser
				initial.identities = append(initial.identities, id)
				initial.nodes = append(initial.nodes, id)
			}
			if err := g.commit(initial); err != nil {
				b.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
			request.Header.Set("Authorization", "Bearer "+commonKey)
			stop, ready := make(chan struct{}), make(chan struct{})
			var wg sync.WaitGroup
			var samples []time.Duration
			reads := 0
			wg.Go(func() {
				response := &discardResponse{header: make(http.Header)}
				close(ready)
				for {
					select {
					case <-stop:
						return
					default:
					}
					started := time.Now()
					g.list(response, request)
					elapsed := time.Since(started)
					// Keep a bounded ring of recent reads, including lock waiting.
					if len(samples) < 4096 {
						samples = append(samples, elapsed)
					} else {
						samples[reads%len(samples)] = elapsed
					}
					reads++
				}
			})
			<-ready
			for b.Loop() {
				g.mu.Lock()
				n := g.nodes["node-000000"]
				n.LastSeen = time.Now().UTC()
				g.nodes[n.ID] = n
				err = g.commit(change{nodes: []string{n.ID}})
				g.mu.Unlock()
				if err != nil {
					close(stop)
					wg.Wait()
					b.Fatal(err)
				}
			}
			close(stop)
			wg.Wait()
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "commits/s")
			b.ReportMetric(float64(reads)/b.Elapsed().Seconds(), "reads/s")
			if len(samples) > 0 {
				slices.Sort(samples)
				b.ReportMetric(float64(samples[(len(samples)-1)*99/100])/float64(time.Microsecond), "list-p99-us")
			}
		})
	}
}

type discardResponse struct{ header http.Header }

func (w *discardResponse) Header() http.Header         { return w.header }
func (w *discardResponse) WriteHeader(int)             {}
func (w *discardResponse) Write(b []byte) (int, error) { return len(b), nil }
