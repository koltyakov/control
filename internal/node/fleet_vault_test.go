package node

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
	"github.com/koltyakov/control/internal/transport/transporttest"
)

// fakeFleetVault stands in for the gateway decision route. One allowance
// covers a principal and a machine set; a request is allowed only when a
// single allowance covers its entire set.
type fakeFleetVault struct {
	mu       sync.Mutex
	enforced bool
	fail     error
	requests []fleetVaultRequest
	// issuer is the account client's transport identity; key and owner are
	// the stable principal returned for it.
	issuer, key, owner string
	allowances         [][]string
	workerDiscovery    bool
	// denyStable models the issuer losing access after a grant was issued.
	denyStable bool
}

type fleetVaultRequest struct {
	receiver string
	model.FleetVaultCheck
}

func (v *fakeFleetVault) install(n *Node) {
	n.fleetVault.enforced = func() bool { v.mu.Lock(); defer v.mu.Unlock(); return v.enforced }
	n.fleetVault.check = func(_ context.Context, q model.FleetVaultCheck) (model.FleetVaultDecision, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.requests = append(v.requests, fleetVaultRequest{receiver: n.Identity.ID, FleetVaultCheck: q})
		if v.fail != nil {
			return model.FleetVaultDecision{}, v.fail
		}
		switch {
		case q.Issuer == "" && q.KeyID == "":
			return model.FleetVaultDecision{Allowed: v.workerDiscovery}, nil
		case q.Issuer != "" && q.Issuer != v.issuer, q.Issuer == "" && (v.denyStable || q.KeyID != v.key || q.OwnerID != v.owner):
			return model.FleetVaultDecision{}, nil
		}
		decision := model.FleetVaultDecision{KeyID: v.key, OwnerID: v.owner}
		for _, allowed := range v.allowances {
			covered := true
			for _, id := range q.MachineIDs {
				covered = covered && slices.Contains(allowed, id)
			}
			decision.Allowed = decision.Allowed || covered
		}
		return decision, nil
	}
}

func (v *fakeFleetVault) set(apply func(*fakeFleetVault)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	apply(v)
}

func (v *fakeFleetVault) take() []fleetVaultRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	requests := v.requests
	v.requests = nil
	return requests
}

type fleetVaultFixture struct {
	ctx                  context.Context
	client               client.Client
	first, second, third *Node
	vault                *fakeFleetVault
	calls                atomic.Int32
	wakes                atomic.Int32
	release              chan struct{}
}

func newFleetVaultFixture(t *testing.T, relay bool) *fleetVaultFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	f := &fleetVaultFixture{ctx: ctx, vault: &fakeFleetVault{key: "key-1", owner: "owner-1"}, release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-f.release:
		default:
			close(f.release)
		}
	})
	const common = "fleet-vault-fixture-worker-key-1234567890"
	g, err := gateway.New(t.TempDir(), common, gateway.Options{SuperuserKey: testAccount})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	options := transporttest.Loopback()
	nodes := []**Node{&f.first, &f.second, &f.third}
	for i, name := range []string{"first", "second", "third"} {
		n, err := New(Config{Name: name, Gateway: server.URL, Token: common, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: relay}, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = n.Close() })
		if err := n.Register(provider{
			cap: model.Capability{Name: "test.vault", InputSchema: model.JSON(map[string]any{"type": "object"})},
			run: func(ctx context.Context, args json.RawMessage, _ Execution) (any, error) {
				f.calls.Add(1)
				var q struct{ Block bool }
				_ = json.Unmarshal(args, &q)
				if q.Block {
					select {
					case <-f.release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return "done", nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		// Decisions and packet sends are injected before the node admits any
		// peer request.
		f.vault.install(n)
		n.sendWake = func(context.Context, string, model.Node) (int, error) { f.wakes.Add(1); return 1, nil }
		if err := n.Start(ctx); err != nil {
			t.Fatal(err)
		}
		*nodes[i] = n
	}
	f.client = (client.Client{}).WithStandalone(ctx, client.StandaloneConfig{Gateway: client.Admin{URL: server.URL, Key: testAccount},
		StateDir: t.TempDir(), RelayOnly: relay, AccountRouting: true, PeerOptions: []transport.Options{options}})
	t.Cleanup(func() { _ = f.client.Close() })
	info, err := f.client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.vault.set(func(v *fakeFleetVault) { v.issuer = info.TransportID })
	return f
}

func (f *fleetVaultFixture) allow(sets ...[]*Node) {
	f.vault.set(func(v *fakeFleetVault) {
		v.enforced, v.allowances = true, nil
		for _, set := range sets {
			ids := []string{}
			for _, n := range set {
				ids = append(ids, n.Identity.ID)
			}
			v.allowances = append(v.allowances, ids)
		}
	})
}

func sortedIDs(nodes ...*Node) []string {
	ids := []string{}
	for _, n := range nodes {
		ids = append(ids, n.Identity.ID)
	}
	slices.Sort(ids)
	return ids
}

func rpcCode(err error) string {
	var rpc *model.RPCError
	if errors.As(err, &rpc) {
		return rpc.Code
	}
	return ""
}

func TestFleetVaultDirectAdmission(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(map[bool]string{false: "webrtc", true: "relay"}[relay], func(t *testing.T) {
			t.Parallel()
			f := newFleetVaultFixture(t, relay)
			// An old gateway never receives a check and the call proceeds.
			if err := f.client.Call(f.ctx, "first", "test.vault", map[string]any{}, nil); err != nil {
				t.Fatal(err)
			}
			if requests := f.vault.take(); len(requests) != 0 {
				t.Fatal("checked without gateway support", requests)
			}
			f.allow([]*Node{f.first})
			for range 2 {
				if err := f.client.Call(f.ctx, "first", "test.vault", map[string]any{}, nil); err != nil {
					t.Fatal(err)
				}
				// Each new request asks the gateway; nothing is reused.
				requests := f.vault.take()
				if len(requests) == 0 {
					t.Fatal("request admitted without a live check")
				}
				for _, q := range requests {
					if q.Issuer != f.vault.issuer || q.KeyID != "" || !slices.Equal(q.MachineIDs, sortedIDs(f.first)) {
						t.Fatal("direct admission did not name the authenticated issuer and destination", q)
					}
				}
			}
			calls := f.calls.Load()
			// The first request after a lock change is refused on a warm session.
			f.allow()
			if err := f.client.Call(f.ctx, "first", "test.vault", map[string]any{}, nil); rpcCode(err) != model.FleetVaultDenied {
				t.Fatal("denied issuer admitted", err)
			}
			f.vault.set(func(v *fakeFleetVault) {
				v.fail, v.allowances = errors.New("gateway unreachable"), [][]string{{f.first.Identity.ID}}
			})
			err := f.client.Call(f.ctx, "first", "test.vault", map[string]any{}, nil)
			var rpc *model.RPCError
			if !errors.As(err, &rpc) || rpc.Code != model.FleetVaultUnavailable || rpc.Outcome != model.OutcomeNotAccepted {
				t.Fatal("request admitted without a decision", err)
			}
			if f.calls.Load() != calls {
				t.Fatal("refused requests reached the provider")
			}
			// A recovered gateway admits the very next request.
			f.vault.set(func(v *fakeFleetVault) { v.fail = nil })
			if err := f.client.Call(f.ctx, "first", "test.vault", map[string]any{}, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Status reads, log reads, cancellation, lease release and grant revocation
// are new requests and need a live allowance. Admitted work continues.
func TestFleetVaultRevocationGatesReadsAndCancels(t *testing.T) {
	f := newFleetVaultFixture(t, true)
	f.allow([]*Node{f.first, f.second})
	var lease model.Lease
	if err := f.client.Call(f.ctx, "first", "leases.acquire", map[string]int{"ttlSeconds": 60}, &lease); err != nil {
		t.Fatal(err)
	}
	var task model.Task
	if err := f.client.Call(f.ctx, "first", "tasks.start", model.TaskSpec{ID: "vault-running", Capability: "test.vault", Args: model.JSON(map[string]bool{"block": true}), LeaseID: lease.ID}, &task); err != nil {
		t.Fatal(err)
	}
	spec := model.TaskSpec{ID: "vault-delegated", Capability: "test.vault", Args: model.JSON(map[string]any{})}
	var grant model.Delegation
	if err := f.client.Call(f.ctx, "second", "access.grant", model.Delegation{Subject: "first", Method: "tasks.start", Params: model.JSON(spec)}, &grant); err != nil {
		t.Fatal(err)
	}
	delegated := model.WithDelegations(f.ctx, []model.Delegation{grant})
	if err := f.first.Call(delegated, "second", "tasks.start", spec, nil); err != nil {
		t.Fatal(err)
	}
	f.allow()
	for _, request := range []struct {
		method string
		params any
	}{
		{"tasks.get", map[string]string{"id": task.ID}},
		{"tasks.logs", map[string]string{"id": task.ID}},
		{"tasks.list", map[string]any{}},
		{"tasks.cancel", map[string]string{"id": task.ID}},
		{model.CancelTaggedTasks, map[string]any{"tags": []string{"x"}}},
		{"leases.release", map[string]string{"id": lease.ID}},
		{"access.list", nil},
		{"access.revoke", map[string]string{"id": "absent"}},
	} {
		if err := f.client.Call(f.ctx, "first", request.method, request.params, nil); rpcCode(err) != model.FleetVaultDenied {
			t.Fatalf("%s admitted after revocation: %v", request.method, err)
		}
	}
	// The delegated worker's reads and cancellation of its task are refused
	// too, using the issuer's recorded principal.
	for _, method := range []string{"tasks.get", "tasks.logs", "tasks.cancel"} {
		if err := f.first.Call(delegated, "second", method, map[string]string{"id": spec.ID}, nil); rpcCode(err) != model.FleetVaultDenied {
			t.Fatalf("delegated %s admitted after revocation: %v", method, err)
		}
	}
	// The admitted task was neither cancelled nor replayed.
	close(f.release)
	f.allow([]*Node{f.first})
	var done model.Task
	if err := f.client.Call(f.ctx, "first", "tasks.get", map[string]any{"id": task.ID, "waitSeconds": 10}, &done); err != nil || done.State != "succeeded" {
		t.Fatal("admitted task did not continue", done.State, err)
	}
}

func TestFleetVaultDelegationCoversEveryWorker(t *testing.T) {
	f := newFleetVaultFixture(t, true)
	call := func() error {
		return f.client.Call(f.ctx, "first", "peers.call", map[string]any{"target": "second", "method": "test.vault", "params": map[string]any{}}, nil)
	}
	// Separate allowances for each worker are not combined into one.
	f.allow([]*Node{f.first}, []*Node{f.second})
	if err := call(); rpcCode(err) != model.FleetVaultDenied {
		t.Fatal("blended allowances admitted a delegation", err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("denied delegation executed")
	}
	f.vault.take()
	f.allow([]*Node{f.first, f.second})
	if err := call(); err != nil {
		t.Fatal(err)
	}
	pair := sortedIDs(f.first, f.second)
	var grant, coordinator, recheck bool
	for _, q := range f.vault.take() {
		switch {
		case q.Issuer == "" && q.KeyID == "":
			t.Fatal("check used no principal", q)
		case q.Issuer != "" && q.Issuer != f.vault.issuer:
			t.Fatal("check used a worker principal", q)
		case q.receiver == f.second.Identity.ID && q.Issuer != "" && slices.Equal(q.MachineIDs, pair):
			grant = true
		case q.receiver == f.first.Identity.ID && q.Issuer != "" && slices.Equal(q.MachineIDs, pair):
			coordinator = true
		case q.receiver == f.second.Identity.ID && q.KeyID == f.vault.key && q.OwnerID == f.vault.owner:
			if !slices.Equal(q.MachineIDs, pair) {
				t.Fatal("delegated recheck omitted a worker", q)
			}
			recheck = true
		}
	}
	if !grant || !coordinator || !recheck {
		t.Fatal("grant, coordinator or delegated acceptance was not checked", grant, coordinator, recheck)
	}
	// A policy change between grant and use refuses the delegated instruction
	// at acceptance, using the issuer's recorded principal.
	calls := f.calls.Load()
	f.vault.set(func(v *fakeFleetVault) { v.denyStable = true })
	if err := call(); rpcCode(err) != model.FleetVaultDenied {
		t.Fatal("delegated instruction admitted after the issuer lost access", err)
	}
	if f.calls.Load() != calls {
		t.Fatal("refused delegated instruction executed")
	}
}

// A workflow on first targeting second and third needs one allowance for all
// three before the coordinator admits it.
func TestFleetVaultWorkflowScopeIsAllOrNothing(t *testing.T) {
	f := newFleetVaultFixture(t, true)
	workflow := model.TaskSpec{ID: "vault-workflow", Capability: "workflow.run", Args: model.JSON(map[string]any{
		"steps": []model.WorkflowStep{
			{Name: "b", Target: "second", Task: model.TaskSpec{ID: "vault-b", Capability: "test.vault", Args: model.JSON(map[string]any{})}},
			{Name: "c", Target: "third", Task: model.TaskSpec{ID: "vault-c", Capability: "test.vault", Args: model.JSON(map[string]any{})}},
		},
	})}
	f.allow([]*Node{f.first, f.second}, []*Node{f.first, f.third})
	for _, method := range []string{model.TaskCheckMethod, "tasks.start"} {
		var result json.RawMessage
		if err := f.client.Call(f.ctx, "first", method, workflow, &result); rpcCode(err) != model.FleetVaultDenied {
			t.Fatalf("%s admitted with separate pairwise allowances: %v %s", method, err, result)
		}
	}
	all := sortedIDs(f.first, f.second, f.third)
	found := false
	for _, q := range f.vault.take() {
		found = found || q.receiver == f.first.Identity.ID && q.Issuer != "" && slices.Equal(q.MachineIDs, all)
	}
	if !found {
		t.Fatal("coordinator did not check every workflow participant together")
	}
	if f.calls.Load() != 0 {
		t.Fatal("denied workflow started a step")
	}
	var missing model.Task
	f.allow([]*Node{f.first})
	if err := f.client.Call(f.ctx, "first", "tasks.get", map[string]string{"id": workflow.ID}, &missing); err == nil {
		t.Fatal("denied workflow was accepted", missing)
	}
	f.allow([]*Node{f.first, f.second, f.third})
	var task model.Task
	if err := f.client.Call(f.ctx, "first", "tasks.start", workflow, &task); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Call(f.ctx, "first", "tasks.get", map[string]any{"id": workflow.ID, "waitSeconds": 20}, &task); err != nil || task.State != "succeeded" {
		t.Fatal("covered workflow did not run", task.State, task.Error, err)
	}
	if f.calls.Load() != 2 {
		t.Fatal("workflow steps", f.calls.Load())
	}
}

func TestFleetVaultConnectionTestNamesTarget(t *testing.T) {
	f := newFleetVaultFixture(t, true)
	f.allow([]*Node{f.first, f.second})
	if err := f.client.Call(f.ctx, "first", model.ConnectionTestMethod, map[string]any{"target": "second", "bytes": 1024, "samples": 1}, nil); err != nil {
		t.Fatal(err)
	}
	pair := sortedIDs(f.first, f.second)
	found := false
	for _, q := range f.vault.take() {
		found = found || q.receiver == f.first.Identity.ID && slices.Equal(q.MachineIDs, pair)
	}
	if !found {
		t.Fatal("connection test target was not checked with its source")
	}
}

func TestFleetVaultArtifactDeliveryChecksBothWorkers(t *testing.T) {
	f := newFleetVaultFixture(t, false)
	pair := sortedIDs(f.first, f.second)
	f.allow([]*Node{f.first, f.second})
	if err := os.WriteFile(filepath.Join(f.first.Config.WorkDir, "input"), []byte("vault delivery"), 0600); err != nil {
		t.Fatal(err)
	}
	var a model.Artifact
	if err := f.client.Call(f.ctx, "first", "artifacts.export", map[string]string{"path": "input"}, &a); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Call(f.ctx, "first", "artifacts.deliver", map[string]string{"id": a.ID, "target": "second"}, nil); err != nil {
		t.Fatal(err)
	}
	stable := map[string]bool{}
	for _, q := range f.vault.take() {
		if q.Issuer == "" && q.KeyID == "" {
			t.Fatal("transfer checked without the issuer's principal", q)
		}
		if q.KeyID != "" {
			stable[q.receiver] = true
			if !slices.Equal(q.MachineIDs, pair) {
				t.Fatal("transfer recheck omitted a worker", q)
			}
		}
	}
	// second rechecks the pull instruction; first rechecks the download.
	if !stable[f.first.Identity.ID] || !stable[f.second.Identity.ID] {
		t.Fatal("worker-to-worker transfer was not rechecked at both ends", stable)
	}
	if _, err := os.Stat(f.second.artifactPath(a.ID)); err != nil {
		t.Fatal("artifact was not delivered", err)
	}
	// A handoff to an uncovered recipient is refused before any grant.
	f.allow([]*Node{f.first})
	if err := f.client.Call(f.ctx, "first", "artifacts.deliver", map[string]string{"id": a.ID, "target": "second"}, nil); rpcCode(err) != model.FleetVaultDenied {
		t.Fatal("handoff admitted for an uncovered recipient", err)
	}
}

func TestFleetVaultWorkerDiscoveryHasNoIssuer(t *testing.T) {
	f := newFleetVaultFixture(t, true)
	f.allow()
	var described map[string]any
	err := f.first.Call(f.ctx, "second", "node.describe", nil, &described)
	if rpcCode(err) != model.FleetVaultDenied {
		t.Fatal("worker discovery admitted under lock", err)
	}
	requests := f.vault.take()
	if len(requests) != 1 || requests[0].Issuer != "" || requests[0].KeyID != "" || !slices.Equal(requests[0].MachineIDs, sortedIDs(f.first, f.second)) {
		t.Fatal("worker discovery check", requests)
	}
	f.vault.set(func(v *fakeFleetVault) { v.workerDiscovery = true })
	if err := f.first.Call(f.ctx, "second", "node.describe", nil, &described); err != nil {
		t.Fatal(err)
	}
}

func TestFleetVaultGrantBeforeEnforcementIsRefused(t *testing.T) {
	f := newFleetVaultFixture(t, true)
	spec := model.TaskSpec{ID: "vault-task", Capability: "test.vault", Args: model.JSON(map[string]any{})}
	var grant model.Delegation
	if err := f.client.Call(f.ctx, "second", "access.grant", model.Delegation{Subject: "first", Method: "tasks.start", Params: model.JSON(spec)}, &grant); err != nil {
		t.Fatal(err)
	}
	f.allow([]*Node{f.first, f.second})
	ctx := model.WithDelegations(f.ctx, []model.Delegation{grant})
	err := f.first.Call(ctx, "second", "tasks.start", spec, nil)
	if rpcCode(err) != model.FleetVaultUnavailable {
		t.Fatal("grant recorded before enforcement was admitted", err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("unchecked grant executed")
	}
}

func TestFleetVaultDecisionsAreNotReusedAcrossRequests(t *testing.T) {
	n := &Node{}
	var checks atomic.Int32
	n.fleetVault.enforced = func() bool { return true }
	n.fleetVault.check = func(context.Context, model.FleetVaultCheck) (model.FleetVaultDecision, error) {
		checks.Add(1)
		return model.FleetVaultDecision{Allowed: true, KeyID: "key", OwnerID: "owner"}, nil
	}
	q := model.FleetVaultCheck{Issuer: "client", MachineIDs: []string{"b", "a", "a"}}
	for range 3 {
		if _, err := n.decideFleetVault(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	if checks.Load() != 3 {
		t.Fatal("decision reused across requests", checks.Load())
	}
	// Within one admission, nested checks of the same proven principal and
	// set share the answer, including the stable form the issuer proved.
	admission := withFleetVaultMemo(t.Context())
	for _, nested := range []model.FleetVaultCheck{q, {Issuer: "client", MachineIDs: []string{"a", "b"}}, {KeyID: "key", OwnerID: "owner", MachineIDs: []string{"b", "a"}}} {
		if _, err := n.decideFleetVault(admission, nested); err != nil {
			t.Fatal(err)
		}
	}
	if checks.Load() != 4 {
		t.Fatal("nested checks of one admission", checks.Load())
	}
	// A different set, or a fresh admission, asks again.
	if _, err := n.decideFleetVault(admission, model.FleetVaultCheck{Issuer: "client", MachineIDs: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.decideFleetVault(withFleetVaultMemo(t.Context()), q); err != nil {
		t.Fatal(err)
	}
	if checks.Load() != 6 {
		t.Fatal("memo crossed a set or admission", checks.Load())
	}
	// A stable recheck cannot be rebound to another principal.
	n.fleetVault.check = func(context.Context, model.FleetVaultCheck) (model.FleetVaultDecision, error) {
		return model.FleetVaultDecision{Allowed: true, KeyID: "other", OwnerID: "owner"}, nil
	}
	if _, err := n.decideFleetVault(t.Context(), model.FleetVaultCheck{KeyID: "key", OwnerID: "owner", MachineIDs: []string{"a"}}); rpcCode(err) != model.FleetVaultUnavailable {
		t.Fatal("rebound stable decision accepted", err)
	}
	// Unavailability fails closed.
	n.fleetVault.check = func(context.Context, model.FleetVaultCheck) (model.FleetVaultDecision, error) {
		return model.FleetVaultDecision{}, errors.New("offline")
	}
	if _, err := n.decideFleetVault(t.Context(), q); rpcCode(err) != model.FleetVaultUnavailable {
		t.Fatal("unavailable decision", err)
	}
}

// stopThird takes the third machine offline so it is a valid wake target.
func (f *fleetVaultFixture) stopThird(t *testing.T) {
	t.Helper()
	if err := f.third.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, f.ctx, func() bool {
		nodes, err := f.first.Peer.Nodes(f.ctx)
		if err != nil {
			return false
		}
		for _, n := range nodes {
			if n.ID == f.third.Identity.ID {
				return !n.Online
			}
		}
		return false
	})
}

// The named wake target is a participant: an allowance for the sender alone
// sends no packet, directly or through a task or workflow.
func TestFleetVaultWakeTargetIsImplicated(t *testing.T) {
	f := newFleetVaultFixture(t, true)
	f.stopThird(t)
	wake := map[string]string{"target": "third"}
	task := model.TaskSpec{ID: "vault-wake", Capability: "network.wake", Args: model.JSON(wake)}
	workflow := model.TaskSpec{ID: "vault-wake-workflow", Capability: "workflow.run", Args: model.JSON(map[string]any{
		"steps": []model.WorkflowStep{{Name: "wake", Target: "second", Task: model.TaskSpec{ID: "vault-wake-step", Capability: "network.wake", Args: model.JSON(wake)}}},
	})}
	f.allow([]*Node{f.first, f.second})
	for _, request := range []struct {
		method string
		params any
	}{{"network.wake", wake}, {"tasks.start", task}, {"tasks.start", workflow}} {
		if err := f.client.Call(f.ctx, "first", request.method, request.params, nil); rpcCode(err) != model.FleetVaultDenied {
			t.Fatalf("%s admitted a wake of an excluded target: %v", request.method, err)
		}
	}
	if f.wakes.Load() != 0 {
		t.Fatal("excluded target received wake packets", f.wakes.Load())
	}
	found := false
	for _, q := range f.vault.take() {
		found = found || q.receiver == f.first.Identity.ID && slices.Equal(q.MachineIDs, sortedIDs(f.first, f.third))
	}
	if !found {
		t.Fatal("wake target was not checked with its sender")
	}
	f.allow([]*Node{f.first, f.third})
	var result model.WakeResult
	if err := f.client.Call(f.ctx, "first", "network.wake", wake, &result); err != nil || result.Target != f.third.Identity.ID || f.wakes.Load() != 1 {
		t.Fatal("covered wake", result, err, f.wakes.Load())
	}
}

// A target ID resolved at send time outside the admitted set is checked live
// with the same principal; local API wakes keep their boundary.
func TestFleetVaultWakeBindsResolvedTarget(t *testing.T) {
	f := newFleetVaultFixture(t, true)
	f.stopThird(t)
	f.allow([]*Node{f.first, f.second})
	// Admitted for first and second, as if "third" had named second then.
	admitted := &fleetVaultBinding{issuer: f.vault.issuer, keyID: f.vault.key, ownerID: f.vault.owner, machines: sortedIDs(f.first, f.second)}
	ctx := context.WithValue(f.ctx, authorityContextKey{}, authority{owner: "owner", vault: admitted})
	if _, err := f.first.wakeTarget(ctx, "third"); rpcCode(err) != model.FleetVaultDenied {
		t.Fatal("re-resolved target sent without a check", err)
	}
	if f.wakes.Load() != 0 {
		t.Fatal("packets sent to an uncovered target")
	}
	requests := f.vault.take()
	if len(requests) != 1 || requests[0].KeyID != f.vault.key || !slices.Equal(requests[0].MachineIDs, sortedIDs(f.first, f.third)) {
		t.Fatal("send-time check", requests)
	}
	// A target already in the admitted set needs no second decision.
	admitted.machines = sortedIDs(f.first, f.third)
	if _, err := f.first.wakeTarget(ctx, "third"); err != nil || len(f.vault.take()) != 0 {
		t.Fatal("admitted target", err)
	}
	// No peer authority: the local API boundary is unchanged.
	if _, err := f.first.wakeTarget(f.ctx, "third"); err != nil || len(f.vault.take()) != 0 || f.wakes.Load() != 2 {
		t.Fatal("local wake", err, f.wakes.Load())
	}
}
