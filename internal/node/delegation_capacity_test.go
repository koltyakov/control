package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

func capacityGrant(ctx context.Context, id, owner, method string) *delegationState {
	ctx, cancel := context.WithCancel(ctx)
	return &delegationState{grant: model.Delegation{ID: id, Owner: owner, Method: method},
		ctx: ctx, cancel: cancel, lastActivity: time.Now()}
}

func TestDelegationCapacityOwnerIsolationAndExpiry(t *testing.T) {
	n := &Node{Config: Config{MaxDelegationsPerOwner: 2}, delegations: map[string]*delegationState{}}
	for i, method := range []string{"tasks.start", "artifacts.open"} {
		if err := n.addDelegation(capacityGrant(t.Context(), fmt.Sprint(i), "a", method)); err != nil {
			t.Fatal(err)
		}
	}
	rejected := capacityGrant(t.Context(), "over", "a", "exec.run")
	if err := n.addDelegation(rejected); err == nil || rejected.ctx.Err() == nil {
		t.Fatal("owner cap not enforced or rejected context leaked", err)
	}
	if err := n.addDelegation(capacityGrant(t.Context(), "other", "b", "artifacts.open")); err != nil {
		t.Fatal("one owner starved another", err)
	}
	n.delegationMu.Lock()
	n.delegations["0"].lastActivity = time.Now().Add(-2 * time.Hour)
	n.delegations["1"].lastActivity = time.Now().Add(-2 * time.Hour)
	n.delegations["1"].activeTasks = 1
	n.delegationMu.Unlock()
	if err := n.addDelegation(capacityGrant(t.Context(), "replacement", "a", "exec.run")); err != nil {
		t.Fatal("expired entry not reclaimed at admission", err)
	}
	if n.delegations["0"] != nil || n.delegations["1"] == nil || n.delegations["1"].ctx.Err() != nil {
		t.Fatal("admission evicted active authority or retained expired entry")
	}
	n.delegationMu.Lock()
	n.removeDelegationLocked("replacement")
	n.removeDelegationLocked("replacement")
	n.delegationMu.Unlock()
	if err := n.addDelegation(capacityGrant(t.Context(), "after-revoke", "a", "tasks.start")); err != nil {
		t.Fatal("revocation did not release capacity exactly once", err)
	}
}

func TestDelegationCapacityConcurrentAdmissionAndGlobalLimit(t *testing.T) {
	n := &Node{Config: Config{MaxDelegationsPerOwner: 8}, delegations: map[string]*delegationState{}}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() { _ = n.addDelegation(capacityGrant(t.Context(), fmt.Sprint(i), "a", "tasks.start")) })
	}
	wg.Wait()
	if len(n.delegations) != 8 {
		t.Fatal("concurrent owner admission exceeded limit", len(n.delegations))
	}
	n.Config.MaxDelegationsPerOwner = maxDelegations
	for i := 8; i < maxDelegations; i++ {
		if err := n.addDelegation(capacityGrant(t.Context(), fmt.Sprintf("b-%d", i), "b", "artifacts.open")); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.addDelegation(capacityGrant(t.Context(), "global-overflow", "c", "tasks.start")); err == nil {
		t.Fatal("global admission exceeded limit")
	}
}

func TestDelegationCapacityConfiguration(t *testing.T) {
	for _, limit := range []int{-1, maxDelegations + 1} {
		if _, err := New(Config{Name: "invalid", Gateway: "http://127.0.0.1", Token: "fixture", MaxDelegationsPerOwner: limit}); err == nil {
			t.Fatal("invalid owner limit accepted", limit)
		}
	}
}

func TestDelegationCapacitySharedByPublicIssuancePaths(t *testing.T) {
	f := newLifetimeFixture(t, true, false, nil, func(cfg *Config) { cfg.MaxDelegationsPerOwner = 1 })
	var grant model.Delegation
	if err := f.client.Call(f.ctx, "second", "access.grant", model.Delegation{
		Subject: "first", Method: "test.lifetime", Params: model.JSON(map[string]any{}),
	}, &grant); err != nil {
		t.Fatal(err)
	}
	sameOwner := (client.Client{}).WithStandalone(f.ctx, f.config)
	t.Cleanup(func() { _ = sameOwner.Close() })
	if err := sameOwner.Call(f.ctx, "second", "access.grant", model.Delegation{
		Subject: "first", Method: "test.lifetime", Params: model.JSON(map[string]any{}),
	}, nil); err == nil || !strings.Contains(err.Error(), "delegation limit") {
		t.Fatal("new transport bypassed stable-owner capacity", err)
	}
	if err := os.WriteFile(filepath.Join(f.second.Config.WorkDir, "input"), []byte("capacity fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	var artifact model.Artifact
	if err := f.client.Call(f.ctx, "second", "artifacts.export", map[string]string{"path": "input"}, &artifact); err != nil {
		t.Fatal(err)
	}
	request := map[string]string{"id": artifact.ID, "target": "first"}
	// A source artifact grant uses the same owner's remaining capacity.
	if err := f.client.Call(f.ctx, "second", "artifacts.grant", request, nil); err == nil || !strings.Contains(err.Error(), "delegation limit") {
		t.Fatal("artifact access bypassed shared owner capacity", err)
	}
	other := (client.Client{}).WithStandalone(f.ctx, client.StandaloneConfig{Gateway: f.config.Gateway,
		StateDir: t.TempDir(), RelayOnly: true, AccountRouting: true, PeerOptions: f.config.PeerOptions})
	t.Cleanup(func() { _ = other.Close() })
	if err := other.Call(f.ctx, "second", "artifacts.grant", request, nil); err != nil {
		t.Fatal("saturated owner blocked another owner's artifact grant", err)
	}
	// Management must remain available even when the owner is at capacity.
	if grants := f.grants(t, "second"); len(grants) != 1 {
		t.Fatal("saturated owner cannot inspect grants", grants)
	}
	if err := f.client.Call(f.ctx, "second", "access.revoke", map[string]string{"id": grant.ID}, nil); err != nil {
		t.Fatal("saturated owner cannot revoke", err)
	}
	if err := f.client.Call(f.ctx, "second", "artifacts.grant", request, nil); err != nil {
		t.Fatal("revocation did not restore artifact capacity", err)
	}
}
