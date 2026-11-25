package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/transport"
	"github.com/koltyakov/control/internal/transport/transporttest"
)

// An owner-issued orchestrator key executes through the account client under
// the host's stable client owner. A common key in the same fleet stays
// discovery-only, and the orchestrator key cannot administer the fleet.
func TestOrchestratorKeyExecutesThroughAccountClient(t *testing.T) {
	t.Parallel()
	peerOptions := transporttest.Loopback()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	const super = "orchestrator-test-superuser-1234567890"
	g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: super})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	s := httptest.NewServer(g.Handler())
	t.Cleanup(s.Close)
	var user struct{ Token string }
	if err = (Admin{URL: s.URL, Key: super}).JSON(ctx, http.MethodPost, "/v1/admin/users", map[string]string{"name": "owner"}, &user); err != nil {
		t.Fatal(err)
	}
	account := Admin{URL: s.URL, Key: user.Token}
	issue := func(name, role string) Admin {
		t.Helper()
		request := map[string]string{"name": name}
		if role != "" {
			request["role"] = role
		}
		var issued struct {
			Key   struct{ ID, Role string }
			Token string
		}
		if err := account.JSON(ctx, http.MethodPost, "/v1/fleet/keys", request, &issued); err != nil {
			t.Fatal(err)
		}
		want := role
		if want == "" {
			want = "common"
		}
		if issued.Key.Role != want {
			t.Fatal("issued role", issued.Key, want)
		}
		return Admin{URL: s.URL, Key: issued.Token}
	}
	orchestrator, common := issue("ci", "orchestrator"), issue("discovery", "")

	worker, err := node.New(node.Config{Name: "worker", Gateway: s.URL, Token: user.Token, DataDir: t.TempDir(), WorkDir: t.TempDir()}, peerOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Close() })
	if err = worker.Register(ownerProvider{}); err != nil {
		t.Fatal(err)
	}
	if err = worker.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Account and orchestrator logins share one state directory on this host.
	state := t.TempDir()
	connect := func(key Admin) Client {
		t.Helper()
		c := (Client{}).WithStandalone(ctx, StandaloneConfig{Gateway: key, StateDir: state, AccountRouting: true, PeerOptions: []transport.Options{peerOptions}})
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	orchestrated, owned, discovery := connect(orchestrator), connect(account), connect(common)

	if role, err := orchestrator.AuthRole(ctx); err != nil || role != "orchestrator" || !ExecutionRole(role) || OwnerRole(role) {
		t.Fatal("orchestrator role", role, err)
	}
	var owner string
	if err := orchestrated.Call(ctx, "worker", "test.owner", map[string]any{}, &owner); err != nil || owner == "" {
		t.Fatal("orchestrator key could not execute", owner, err)
	}
	spec := model.TaskSpec{ID: "orchestrator-owned", Capability: "test.owner", Args: model.JSON(map[string]any{})}
	var task model.Task
	if err := orchestrated.Call(ctx, "worker", "tasks.start", spec, &task); err != nil {
		t.Fatal(err)
	}
	if task, err = orchestrated.Wait(ctx, "worker", spec.ID); err != nil || task.State != "succeeded" || task.Owner != owner {
		t.Fatal("orchestrator task", task, err)
	}
	// The host's client owner is stable across execution keys: it is scoped
	// by gateway and fleet, so task ownership survives key rotation and a
	// Vault migration keeps the same owner under either role. Keys are
	// distinguished by the gateway-attested credential of each session.
	orchestratorInfo, err := orchestrated.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	info, err := owned.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if orchestratorInfo.OwnerID != owner || info.OwnerID != owner || info.TransportID == orchestratorInfo.TransportID {
		t.Fatal("client owner depends on the execution key", orchestratorInfo, info, owner)
	}
	if err := owned.Call(ctx, "worker", "tasks.get", map[string]any{"id": spec.ID}, &task); err != nil || task.Owner != owner {
		t.Fatal("same host owner could not recover its task", task, err)
	}
	var attested model.Node
	if err := account.JSON(ctx, http.MethodGet, "/v1/peers/"+orchestratorInfo.TransportID, nil, &attested); err != nil || attested.ClientOwner != owner || !attested.ExecutionAuthority {
		t.Fatal("orchestrator session not attested", attested, err)
	}
	// A rotated replacement key resumes the same owner.
	rotated := connect(issue("ci-rotated", "orchestrator"))
	if err := rotated.Call(ctx, "worker", "tasks.get", map[string]any{"id": spec.ID}, &task); err != nil || task.Owner != owner {
		t.Fatal("rotated orchestrator key lost the host owner", task, err)
	}
	// A different host's state never inherits that owner.
	elsewhere := (Client{}).WithStandalone(ctx, StandaloneConfig{Gateway: orchestrator, StateDir: t.TempDir(), AccountRouting: true, PeerOptions: []transport.Options{peerOptions}})
	t.Cleanup(func() { _ = elsewhere.Close() })
	if err := elsewhere.Call(ctx, "worker", "tasks.get", map[string]any{"id": spec.ID}, nil); err == nil {
		t.Fatal("orchestrator key impersonated another host's task owner")
	}

	// Common keys keep discovery but not execution.
	if err := discovery.Call(ctx, "worker", "node.describe", map[string]any{}, nil); err != nil {
		t.Fatal("common key discovery", err)
	}
	if err := discovery.Call(ctx, "worker", "test.owner", map[string]any{}, nil); err == nil {
		t.Fatal("common key executed work")
	}

	// Execution does not include fleet administration.
	if err := orchestrator.ManageMachine(ctx, worker.Identity.ID, "disable"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatal("orchestrator key managed a machine", err)
	}
	if _, err := orchestrator.Invite(ctx, enrollment.Request{Name: "new-machine", OS: "linux"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatal("orchestrator key created an invitation", err)
	}
	if err := orchestrated.Call(ctx, "worker", "test.owner", map[string]any{}, nil); err != nil {
		t.Fatal("worker stopped accepting orchestrator work", err)
	}
}
