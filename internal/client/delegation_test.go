package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
)

type taskFileProvider struct{}

func (taskFileProvider) Capability() model.Capability {
	return model.Capability{Name: "test.file", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (taskFileProvider) Run(_ context.Context, args json.RawMessage, e node.Execution) (any, error) {
	var q struct {
		Read bool `json:"read"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if q.Read {
		b, err := os.ReadFile(filepath.Join(e.Dir, "in.bin"))
		return string(b), err
	}
	return "written", os.WriteFile(filepath.Join(e.Dir, "result.bin"), []byte("delegated-output"), 0600)
}

func delegationCluster(t *testing.T, relay bool) (context.Context, Client, []*node.Node, Admin) {
	t.Helper()
	return delegationClusterWithTimeout(t, relay, 45*time.Second)
}

func delegationClusterWithTimeout(t *testing.T, relay bool, timeout time.Duration) (context.Context, Client, []*node.Node, Admin) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	const common = "delegation-worker-key-1234567890"
	const account = "delegation-account-key-1234567890"
	g, err := gateway.New(t.TempDir(), common, gateway.Options{SuperuserKey: account})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	workers := []*node.Node{}
	for _, name := range []string{"first", "second", "third"} {
		w, err := node.New(node.Config{Name: name, Gateway: server.URL, Token: common, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: relay})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		if err = w.Register(ownerProvider{}); err != nil {
			t.Fatal(err)
		}
		if err = w.Register(longProvider{}); err != nil {
			t.Fatal(err)
		}
		if err = w.Register(taskFileProvider{}); err != nil {
			t.Fatal(err)
		}
		if err = w.Start(ctx); err != nil {
			t.Fatal(err)
		}
		workers = append(workers, w)
	}
	admin := Admin{URL: server.URL, Key: account}
	// Even with a listening worker API, account routing must not inherit it.
	api := httptest.NewServer(workers[0].Handler())
	t.Cleanup(api.Close)
	c := (Client{URL: api.URL, Token: common}).WithStandalone(ctx, StandaloneConfig{Gateway: admin, StateDir: t.TempDir(), RelayOnly: relay, AccountRouting: true})
	t.Cleanup(func() { _ = c.Close() })
	return ctx, c, workers, admin
}

func TestInstructionDelegation(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, c, workers, admin := delegationCluster(t, relay)
			for _, method := range []string{"test.owner", "tasks.list", "access.grant", "files.list", "system.info"} {
				if err := workers[0].Call(ctx, "second", method, map[string]any{}, nil); err == nil {
					t.Fatalf("worker independently invoked %s", method)
				}
			}
			if err := workers[0].Call(ctx, "second", "capabilities.list", map[string]any{}, nil); err != nil {
				t.Fatal("worker discovery", err)
			}
			var owner string
			instruction := map[string]any{"target": "second", "method": "test.owner", "params": map[string]any{}}
			if err := c.Call(ctx, "first", "peers.call", instruction, &owner); err != nil {
				t.Fatal("orchestrator through worker", err)
			}
			info, err := c.Session(ctx)
			if err != nil || owner != info.OwnerID || info.Role != "client" {
				t.Fatal("delegated owner", owner, info, err)
			}
			if err := workers[0].Call(ctx, "second", "test.owner", map[string]any{}, nil); err == nil {
				t.Fatal("delegation became ambient authority")
			}
			var grant model.Delegation
			if err := c.Call(ctx, "second", "access.grant", model.Delegation{Subject: "first", Method: "files.list", Params: model.JSON(map[string]any{"path": "."})}, &grant); err != nil {
				t.Fatal(err)
			}
			delegated := model.WithDelegations(ctx, []model.Delegation{grant})
			if err := workers[2].Call(delegated, "second", "files.list", map[string]any{"path": "."}, nil); err == nil {
				t.Fatal("wrong worker used grant")
			}
			// Force the ID onto a changed request to test destination validation.
			forged := grant
			forged.Params = model.JSON(map[string]any{"path": "other"})
			if err := workers[0].Call(model.WithDelegations(ctx, []model.Delegation{forged}), "second", "files.list", map[string]any{"path": "other"}, nil); err == nil {
				t.Fatal("grant accepted changed arguments")
			}
			if err := workers[0].Call(delegated, "second", "files.list", map[string]any{"path": "."}, nil); err != nil {
				t.Fatal(err)
			}
			if err := workers[0].Call(delegated, "second", "files.list", map[string]any{"path": "."}, nil); err == nil {
				t.Fatal("synchronous instruction replayed")
			}
			// The same fleet's common key cannot turn a CLI into an orchestrator.
			common := (Client{URL: refusedAPI(t)}).WithStandalone(ctx, StandaloneConfig{Gateway: Admin{URL: admin.URL, Key: workers[0].Config.Token}, StateDir: t.TempDir(), RelayOnly: relay, AccountRouting: true})
			defer func() { _ = common.Close() }()
			if err := common.Call(ctx, "second", "test.owner", map[string]any{}, nil); err == nil {
				t.Fatal("common-key client executed work")
			}
			if err := common.Call(ctx, "second", "node.describe", map[string]any{}, nil); err != nil {
				t.Fatal("common-key discovery", err)
			}
		})
	}
}

func TestAccountRoutingUsesCurrentNamedDestination(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, c, workers, admin := delegationCluster(t, relay)
			var description struct{ ID string }
			if err := c.Call(ctx, "second", "node.describe", map[string]any{}, &description); err != nil {
				t.Fatal(err)
			}
			old := workers[1]
			if description.ID != old.Identity.ID {
				t.Fatal("wrong original destination")
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			if err := admin.JSON(ctx, "DELETE", "/v1/fleet/nodes/"+old.Identity.ID+"?stop=true", nil, nil); err != nil {
				t.Fatal(err)
			}
			cfg := old.Config
			cfg.DataDir, cfg.WorkDir = t.TempDir(), t.TempDir()
			replacement, err := node.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = replacement.Close() })
			if err := replacement.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if err := c.Call(ctx, "second", "node.describe", map[string]any{}, &description); err != nil {
				t.Fatal(err)
			}
			if description.ID != replacement.Identity.ID {
				t.Fatal("named call reused the previous registration's session")
			}
		})
	}
}

func TestDelegatedTaskRevocation(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, c, workers, admin := delegationCluster(t, relay)
			var grant model.Delegation
			spec := model.TaskSpec{ID: "delegated-long", Capability: "test.long", Args: json.RawMessage(`{}`)}
			if err := c.Call(ctx, "second", "access.grant", model.Delegation{Subject: "first", Method: "tasks.start", Params: model.JSON(spec)}, &grant); err != nil {
				t.Fatal(err)
			}
			delegated := model.WithDelegations(ctx, []model.Delegation{grant})
			var task model.Task
			if err := workers[0].Call(delegated, "second", "tasks.start", spec, &task); err != nil {
				t.Fatal(err)
			}
			info, _ := c.Session(ctx)
			if task.Owner != info.OwnerID {
				t.Fatal("child task lost root ownership")
			}
			// Delegation management remains owner-scoped even for account clients.
			other := (Client{URL: refusedAPI(t)}).WithStandalone(ctx, StandaloneConfig{Gateway: admin, StateDir: t.TempDir(), RelayOnly: relay, AccountRouting: true})
			defer func() { _ = other.Close() }()
			if err := other.Call(ctx, "second", "access.revoke", map[string]any{"id": grant.ID}, nil); err == nil {
				t.Fatal("different orchestrator revoked grant")
			}
			if err := c.Call(ctx, "second", "access.revoke", map[string]any{"id": grant.ID}, nil); err != nil {
				t.Fatal(err)
			}
			finished, err := c.Wait(ctx, "second", spec.ID)
			if err != nil || finished.State != "cancelled" {
				t.Fatal("revocation did not cancel durable child", finished, err)
			}
			if err := workers[0].Call(delegated, "second", "tasks.get", map[string]any{"id": spec.ID}, nil); err == nil {
				t.Fatal("revoked access still works")
			}
			if err := workers[0].Call(delegated, "second", "tasks.start", spec, nil); err == nil {
				t.Fatal("revoked task was resubmitted")
			}
		})
	}
}

func TestDelegatedWorkflowAndDelivery(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, c, workers, _ := delegationCluster(t, relay)
			if err := os.WriteFile(filepath.Join(workers[1].Config.WorkDir, "input.txt"), []byte("direct artifact bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			steps := []node.WorkflowStep{
				{Name: "source", Target: "second", Task: model.TaskSpec{Capability: "test.file", Args: json.RawMessage(`{}`), Outputs: []string{"result.bin", "result.bin"}}},
				{Name: "local", Target: "first", Needs: []string{"source"}, Task: model.TaskSpec{Capability: "test.owner", Args: json.RawMessage(`{}`)}},
				{Name: "remote", Target: "third", Needs: []string{"local"}, Task: model.TaskSpec{Capability: "test.file", Args: json.RawMessage(`{"read":true}`)}},
			}
			steps[2].Inputs = append(steps[2].Inputs, struct {
				Step     string `json:"step"`
				Artifact int    `json:"artifact"`
				Path     string `json:"path"`
			}{Step: "source", Artifact: 1, Path: "in.bin"})
			workflow := map[string]any{"steps": steps}
			spec := model.TaskSpec{ID: "delegated-workflow", Capability: "workflow.run", Args: model.JSON(workflow)}
			var task model.Task
			if err := c.Call(ctx, "first", "tasks.start", spec, &task); err != nil {
				t.Fatal(err)
			}
			finished, err := c.Wait(ctx, "first", task.ID)
			if err != nil || finished.State != "succeeded" {
				t.Fatal("workflow", finished, err)
			}
			var results map[string]model.Task
			if err := json.Unmarshal(finished.Result, &results); err != nil {
				t.Fatal(err)
			}
			var originalGrants []model.Delegation
			if err := c.Call(ctx, "second", "access.list", map[string]any{}, &originalGrants); err != nil {
				t.Fatal(err)
			}
			if string(results["remote"].Result) != `"delegated-output"` {
				t.Fatal("dynamic workflow input was not delivered", results)
			}
			if _, err := os.Stat(filepath.Join(workers[0].Config.DataDir, "artifacts", results["source"].Artifacts[0].ID)); !os.IsNotExist(err) {
				t.Fatal("workflow artifact passed through coordinator storage")
			}
			// A source-signed grant for the same bytes without upstream-task
			// provenance must not satisfy a workflow input authorization.
			var unscoped model.Artifact
			if err := c.Call(ctx, "second", "artifacts.grant", map[string]any{"id": results["source"].Artifacts[0].ID, "target": "third"}, &unscoped); err != nil {
				t.Fatal(err)
			}
			child := model.TaskSpec{ID: "forged-input", Capability: "test.file", Args: json.RawMessage(`{"read":true}`)}
			var inputGrant model.Delegation
			if err := c.Call(ctx, "third", "access.grant", model.Delegation{Subject: "first", Method: "tasks.start", Params: model.JSON(child), InputsFrom: []model.DelegatedInput{{Node: workers[1].Identity.ID, Path: "in.bin", TaskID: results["source"].ID, Artifact: 1}}}, &inputGrant); err != nil {
				t.Fatal(err)
			}
			child.Inputs = []model.Input{{Artifact: unscoped, Path: "in.bin"}}
			if err := workers[0].Call(model.WithDelegations(ctx, []model.Delegation{inputGrant}), "third", "tasks.start", child, nil); err == nil {
				t.Fatal("unscoped artifact substituted for an authorized upstream output")
			}
			// Repeating the original parent specification must retain child IDs.
			if err := c.Call(ctx, "first", "tasks.start", spec, &task); err != nil || task.Updated != finished.Updated {
				t.Fatal("workflow idempotency", task, err)
			}
			var artifact, delivered model.Artifact
			if err := c.Call(ctx, "second", "artifacts.export", map[string]any{"path": "input.txt"}, &artifact); err != nil {
				t.Fatal(err)
			}
			if err := c.Call(ctx, "second", "artifacts.deliver", map[string]any{"id": artifact.ID, "target": "third"}, &delivered); err != nil {
				t.Fatal("delivery", err)
			}
			if delivered.Node != workers[2].Identity.ID {
				t.Fatal("wrong delivery target")
			}
			if _, err := os.Stat(filepath.Join(workers[0].Config.DataDir, "artifacts", artifact.ID)); !os.IsNotExist(err) {
				t.Fatal("bulk bytes went through orchestrator")
			}
			if err := workers[1].Call(ctx, "third", "files.list", map[string]any{}, nil); err == nil {
				t.Fatal("artifact delivery granted unrelated authority")
			}
			var grants []model.Delegation
			if err := c.Call(ctx, "second", "access.list", map[string]any{}, &grants); err != nil {
				t.Fatal(err)
			}
			for _, grant := range grants {
				if grant.Expires.Before(time.Now()) || grant.Expires.After(time.Now().Add(time.Hour+time.Minute)) {
					t.Fatal("wrong idle expiry", grant)
				}
			}
			if len(grants) == 0 {
				t.Fatal("missing owner-visible delegations")
			}
			if !strings.Contains(string(model.JSON(grants)), "artifacts.open") {
				t.Fatal("artifact access was not revocable")
			}
			// Revoking the upstream task's authority also revokes its signed
			// output access, even after the task and workflow have completed.
			output := results["remote"].Spec.Inputs[0].Artifact
			if err := workers[2].Download(ctx, output, 0, io.Discard); err != nil {
				t.Fatal("derived artifact access", err)
			}
			var parent string
			for _, grant := range originalGrants {
				if grant.TaskID() == results["source"].ID {
					parent = grant.ID
					break
				}
			}
			if parent == "" {
				t.Fatal("missing upstream task grant")
			}
			if err := c.Call(ctx, "second", "access.revoke", map[string]any{"id": parent}, nil); err != nil {
				t.Fatal(err)
			}
			if err := workers[2].Download(ctx, output, 0, io.Discard); err == nil {
				t.Fatal("upstream revocation left derived artifact access live")
			}
		})
	}
}
