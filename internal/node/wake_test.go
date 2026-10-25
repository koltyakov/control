package node

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestWakePeerAuthorityAndAdmission(t *testing.T) {
	t.Parallel()
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, target := cluster(t, relay, func(i int, cfg *Config) {
				cfg.Labels["lan"] = "home"
				if i == 1 {
					cfg.Allow = map[string][]string{"source": {"network.wake"}}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var sends atomic.Int32
			worker.sendWake = func(ctx context.Context, lan string, n model.Node) (int, error) {
				if lan != "home" || n.ID != target.Identity.ID || n.Online {
					return 0, errors.New("incorrect wake destination")
				}
				sends.Add(1)
				return 1, ctx.Err()
			}
			params := map[string]string{"target": target.Identity.ID}
			if err := testCall(t, source, ctx, "worker", "network.wake", params, nil); err == nil {
				t.Fatal("accepted online target")
			}
			if err := target.Close(); err != nil {
				t.Fatal(err)
			}
			eventually(t, ctx, func() bool {
				nodes, err := worker.Peer.Nodes(ctx)
				if err != nil {
					return false
				}
				for _, n := range nodes {
					if n.ID == target.Identity.ID {
						return !n.Online
					}
				}
				return false
			})
			var result model.WakeResult
			if err := testCall(t, source, ctx, "worker", "network.wake", params, &result); err != nil || result.Packets != 1 || result.Source != worker.Identity.ID || result.Target != target.Identity.ID {
				t.Fatal(result, err)
			}
			if stream, _, err := source.Peer.OpenRPC(ctx, "worker", "network.wake", params); err == nil {
				_ = stream.Close()
				t.Fatal("worker woke without delegation")
			}
			// Another account-client owner is still subject to the sender's allow rules.
			if err := testCall(t, target, ctx, "worker", "network.wake", params, nil); err == nil {
				t.Fatal("ignored sender permissions")
			}
			worker.work.SetDisabled(true)
			if err := testCall(t, source, ctx, "worker", "network.wake", params, nil); err == nil {
				t.Fatal("disabled sender admitted wake")
			}
			worker.work.SetDisabled(false)
			for _, name := range []string{"missing", worker.Identity.ID} {
				if err := testCall(t, source, ctx, "worker", "network.wake", map[string]string{"target": name}, nil); err == nil {
					t.Fatal("accepted unknown or self target")
				}
			}
			if sends.Load() != 1 {
				t.Fatal("unexpected packet sends", sends.Load())
			}
		})
	}
}
