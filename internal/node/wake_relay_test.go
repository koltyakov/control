package node

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestAccountMayCallFollowsAllowRules(t *testing.T) {
	for _, c := range []struct {
		allow map[string][]string
		want  bool
	}{
		{nil, true},
		{map[string][]string{"@orchestrator": {"network.*"}}, true},
		{map[string][]string{"*": {"network.wake"}}, true},
		{map[string][]string{"some-peer": {"*"}}, false},
		{map[string][]string{}, false},
	} {
		if got := accountMayCall(c.allow, "network.wake"); got != c.want {
			t.Fatalf("accountMayCall(%v) = %v", c.allow, got)
		}
	}
}

func TestRelayedWakeAppliesNodeChecks(t *testing.T) {
	t.Parallel()
	_, worker, target := cluster(t, false, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var sends atomic.Int32
	worker.sendWake = func(ctx context.Context, sourceID string, n model.Node) (int, error) {
		sends.Add(1)
		return 1, ctx.Err()
	}
	nodes, err := worker.Peer.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == worker.Identity.ID && !n.GatewayWake {
			t.Fatal("default allow rules did not advertise gateway wake")
		}
	}
	if _, err := worker.relayedWake(ctx, target.Identity.ID); err == nil {
		t.Fatal("woke an online machine")
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
	if result, err := worker.relayedWake(ctx, target.Identity.ID); err != nil || result.Packets != 1 || result.Source != worker.Identity.ID {
		t.Fatal(result, err)
	}
	worker.work.SetDisabled(true)
	if _, err := worker.relayedWake(ctx, target.Identity.ID); err == nil {
		t.Fatal("disabled machine sent a wake packet")
	}
	worker.work.SetDisabled(false)
	worker.Config.Allow = map[string][]string{}
	if _, err := worker.relayedWake(ctx, target.Identity.ID); err == nil {
		t.Fatal("ignored allow rules")
	}
	if sends.Load() != 1 {
		t.Fatal("unexpected packet sends", sends.Load())
	}
}
