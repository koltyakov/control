package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

// checkNode is a local node with a schema-bearing provider that can block.
func checkNode(t *testing.T, configure func(*Config)) (*Node, chan struct{}, func()) {
	t.Helper()
	n := localNode(t, configure)
	entered, release := make(chan struct{}, 8), make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(release) }) }
	t.Cleanup(stop)
	err := n.Register(provider{cap: model.Capability{Name: "test.check", InputSchema: model.JSON(map[string]any{
		"type": "object", "required": []string{"n"},
		"properties": map[string]any{"n": map[string]any{"type": "integer"}, "block": map[string]any{"type": "boolean"}},
	})}, run: func(ctx context.Context, args json.RawMessage, _ Execution) (any, error) {
		var q struct{ Block bool }
		if err := json.Unmarshal(args, &q); err != nil {
			return nil, err
		}
		if q.Block {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return "done", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return n, entered, stop
}

func checkSpec(id string, args string) model.TaskSpec {
	return model.TaskSpec{ID: id, Capability: "test.check", Args: json.RawMessage(args)}
}

// checkLocal calls tasks.check through dispatch, as the local API does.
func checkLocal(t *testing.T, n *Node, spec any) model.TaskCheck {
	t.Helper()
	result, err := n.dispatch(context.Background(), n.Identity.ID, model.TaskCheckMethod, model.JSON(spec))
	if err != nil {
		t.Fatal(err)
	}
	return result.(model.TaskCheck)
}

func checkItem(t *testing.T, report model.TaskCheck, name string) model.TaskCheckItem {
	t.Helper()
	for _, item := range report.Checks {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("check %s missing from %+v", name, report)
	return model.TaskCheckItem{}
}

func wantCheck(t *testing.T, report model.TaskCheck, name, status, contains string) {
	t.Helper()
	item := checkItem(t, report, name)
	if item.Status != status || !strings.Contains(item.Message, contains) {
		t.Fatalf("%s = %+v, want %s containing %q; report %+v", name, item, status, contains, report)
	}
	if status == model.TaskCheckFail && report.OK {
		t.Fatalf("ok with failing %s", name)
	}
	if !report.Advisory || report.Note != model.TaskCheckAdvisory {
		t.Fatalf("answer not marked advisory: %+v", report)
	}
}

func TestTaskCheckReportsEachCheck(t *testing.T) {
	t.Parallel()
	n, _, _ := checkNode(t, nil)
	digest := strings.Repeat("a", 64)
	foreign := model.Artifact{Node: "elsewhere", ID: digest, SHA256: digest, Size: 3}
	valid := checkLocal(t, n, checkSpec("fresh", `{"n":1}`))
	if !valid.OK || valid.TaskID != "fresh" {
		t.Fatalf("valid specification failed: %+v", valid)
	}
	for _, name := range []string{"spec", "authorization", "id", "capability", "args", "inputs", "outputs", "lease", "admission"} {
		wantCheck(t, valid, name, model.TaskCheckPass, "")
	}
	if len(valid.Pools) != 1 || valid.Pools[0].Name != "execution" || valid.Pools[0].Limit != n.Config.MaxTasks || valid.Pools[0].Queued == nil {
		t.Fatalf("pool snapshot: %+v", valid.Pools)
	}
	files := checkLocal(t, n, model.TaskSpec{Capability: "files.list", Args: json.RawMessage(`{}`), Inputs: []model.Input{{Artifact: foreign, Path: "in"}}, Outputs: []string{"out"}})
	wantCheck(t, files, "id", model.TaskCheckPass, "no ID")
	wantCheck(t, files, "inputs", model.TaskCheckWarn, "no source grant")
	if !files.OK || len(files.Pools) != 3 || files.Pools[0].Name != "inputStaging" || files.Pools[1].Name != "operations" || files.Pools[2].Name != "outputPublication" {
		t.Fatalf("stage pools: %+v", files)
	}
	granted := foreign
	granted.Grant = "opaque"
	wantCheck(t, checkLocal(t, n, model.TaskSpec{Capability: "files.list", Inputs: []model.Input{{Artifact: granted, Path: "in"}}}), "inputs", model.TaskCheckPass, "not verified")

	for _, tc := range []struct {
		name, check, status, contains string
		spec                          model.TaskSpec
	}{
		{"unknown capability", "capability", model.TaskCheckFail, "unknown capability", model.TaskSpec{ID: "x", Capability: "missing.capability"}},
		{"invalid ID", "id", model.TaskCheckFail, "invalid task ID", checkSpec("bad id", `{"n":1}`)},
		{"wrong argument type", "args", model.TaskCheckFail, "advertised input schema", checkSpec("a", `{"n":"one"}`)},
		{"missing argument", "args", model.TaskCheckFail, "advertised input schema", checkSpec("a", `{}`)},
		{"timeout bound", "spec", model.TaskCheckFail, "1..86400", model.TaskSpec{Capability: "test.check", Args: json.RawMessage(`{"n":1}`), TimeoutSeconds: 86401}},
		{"argument bound", "spec", model.TaskCheckFail, "exceeds limits", checkSpec("a", `{"n":1,"pad":"`+strings.Repeat("x", 1<<20)+`"}`)},
		{"input path", "inputs", model.TaskCheckFail, "relative to the task workspace", model.TaskSpec{Capability: "files.list", Inputs: []model.Input{{Artifact: granted, Path: "../in"}}}},
		{"input reference", "inputs", model.TaskCheckFail, "invalid artifact reference", model.TaskSpec{Capability: "files.list", Inputs: []model.Input{{Artifact: model.Artifact{ID: "short"}, Path: "in"}}}},
		{"output path", "outputs", model.TaskCheckFail, "relative to the task workspace", model.TaskSpec{Capability: "files.list", Outputs: []string{"../out"}}},
		{"workflow cycle", "coordination", model.TaskCheckFail, "", model.TaskSpec{Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": []map[string]any{{"name": "a", "needs": []string{"b"}}, {"name": "b", "needs": []string{"a"}}}})}},
		{"workflow child IDs", "coordination", model.TaskCheckFail, "same task ID", model.TaskSpec{Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": []map[string]any{{"name": "a", "target": "w", "task": map[string]any{"id": "Child"}}, {"name": "b", "target": "w", "task": map[string]any{"id": "child"}}}})}},
		{"workflow plan", "coordination", model.TaskCheckPass, "children were not contacted", model.TaskSpec{Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": []map[string]any{{"name": "a", "target": "w", "task": map[string]any{"capability": "exec.run"}}, {"name": "b", "target": "v", "needs": []string{"a"}}}})}},
		{"peer call", "coordination", model.TaskCheckFail, "target and method", model.TaskSpec{Capability: "peers.call", Args: json.RawMessage(`{"method":"exec.run","params":{}}`)}},
		{"peer stream", "coordination", model.TaskCheckFail, "streaming", model.TaskSpec{Capability: "peers.call", Args: json.RawMessage(`{"target":"w","method":"tcp.open","params":{}}`)}},
		{"missing lease", "lease", model.TaskCheckFail, "lease expired or does not exist", model.TaskSpec{Capability: "test.check", Args: json.RawMessage(`{"n":1}`), LeaseID: "gone"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := checkLocal(t, n, tc.spec)
			wantCheck(t, report, tc.check, tc.status, tc.contains)
			if tc.status == model.TaskCheckFail && report.OK {
				t.Fatalf("failing check reported ok: %+v", report)
			}
		})
	}
	// Coordination does not use a provider pool.
	if report := checkLocal(t, n, model.TaskSpec{Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": []map[string]any{{"name": "a", "target": "w"}}})}); len(report.Pools) != 0 {
		t.Fatalf("coordination reported a pool: %+v", report.Pools)
	}
	// An undecodable specification is an error, like tasks.start.
	if _, err := n.dispatch(context.Background(), n.Identity.ID, model.TaskCheckMethod, json.RawMessage(`[]`)); err == nil {
		t.Fatal("non-object specification was checked")
	}
}

func TestTaskCheckLeaseAndAdmission(t *testing.T) {
	t.Parallel()
	n, _, _ := checkNode(t, nil)
	self := n.Identity.ID
	acquired, err := n.leaseMethod(self, "leases.acquire", model.JSON(map[string]any{"ttlSeconds": 60}))
	if err != nil {
		t.Fatal(err)
	}
	lease := acquired.(model.Lease)
	spec := checkSpec("leased", `{"n":1}`)
	wantCheck(t, checkLocal(t, n, spec), "lease", model.TaskCheckFail, "exclusive lease")
	spec.LeaseID = lease.ID
	wantCheck(t, checkLocal(t, n, spec), "lease", model.TaskCheckPass, "holds the current lease")
	report, err := n.checkTask(context.Background(), self, "another-owner", model.JSON(spec))
	if err != nil {
		t.Fatal(err)
	}
	wantCheck(t, report, "lease", model.TaskCheckFail, "exclusive lease")
	if _, err = n.leaseMethod(self, "leases.release", model.JSON(map[string]any{"id": lease.ID})); err != nil {
		t.Fatal(err)
	}
	wantCheck(t, checkLocal(t, n, spec), "lease", model.TaskCheckFail, "lease expired or does not exist")

	spec.LeaseID = ""
	if !n.work.ReserveIfIdle() {
		t.Fatal("maintenance reservation refused")
	}
	// Dispatch must not wait for, or be refused by, maintenance admission.
	wantCheck(t, checkLocal(t, n, spec), "admission", model.TaskCheckWarn, "maintenance reservation")
	n.work.Resume()
	n.work.SetDisabled(true)
	wantCheck(t, checkLocal(t, n, spec), "admission", model.TaskCheckFail, "disabled")
	if _, err := n.startTaskContext(context.Background(), self, spec); err == nil {
		t.Fatal("disabled node accepted the checked task")
	}
	n.work.SetDisabled(false)
	n.mu.Lock()
	for i := range 256 {
		n.cancels[fmt.Sprint("queued-", i)] = func() {}
	}
	n.mu.Unlock()
	wantCheck(t, checkLocal(t, n, spec), "admission", model.TaskCheckFail, "queue is full")
	n.mu.Lock()
	delete(n.cancels, "queued-0")
	n.mu.Unlock()
	wantCheck(t, checkLocal(t, n, spec), "admission", model.TaskCheckPass, "")
	n.mu.Lock()
	for i := range 256 {
		delete(n.cancels, fmt.Sprint("queued-", i))
	}
	n.mu.Unlock()
}

func TestTaskCheckDuplicateIDs(t *testing.T) {
	t.Parallel()
	n, _, _ := checkNode(t, nil)
	ctx := context.Background()
	self := n.Identity.ID
	// Formatting and the default timeout are normalized before the digest.
	if _, err := n.startTaskContext(ctx, self, checkSpec("dup", `{ "n" : 1 }`)); err != nil {
		t.Fatal(err)
	}
	if _, err := n.waitTask(ctx, self, "dup", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	same := checkSpec("dup", `{"n":1}`)
	same.TimeoutSeconds = 3600
	report := checkLocal(t, n, same)
	wantCheck(t, report, "id", model.TaskCheckPass, "already exists")
	for _, name := range []string{"capability", "args", "inputs", "outputs", "lease", "admission"} {
		wantCheck(t, report, name, model.TaskCheckSkip, "existing task")
	}
	if !report.OK {
		t.Fatal("matching duplicate reported failure", report)
	}
	again, err := n.startTaskContext(ctx, self, same)
	if err != nil || again.State != "succeeded" {
		t.Fatal("matching duplicate was not returned", again, err)
	}
	wantCheck(t, checkLocal(t, n, checkSpec("dup", `{"n":2}`)), "id", model.TaskCheckFail, "different owner or specification")
	other, err := n.checkTask(ctx, self, "another-owner", model.JSON(same))
	if err != nil {
		t.Fatal(err)
	}
	wantCheck(t, other, "id", model.TaskCheckFail, "different owner or specification")
	if _, err = n.startTaskContext(ctx, "another-owner", same); err == nil {
		t.Fatal("another owner reused the ID")
	}
	wantCheck(t, checkLocal(t, n, checkSpec("DUP", `{"n":1}`)), "id", model.TaskCheckFail, "case-fold conflict")
	if _, err = n.startTaskContext(ctx, self, checkSpec("DUP", `{"n":1}`)); err == nil {
		t.Fatal("case-fold conflict accepted")
	}
	// An invalid specification never matches an accepted one.
	invalid := checkSpec("dup", `{"n":1}`)
	invalid.TimeoutSeconds = -1
	wantCheck(t, checkLocal(t, n, invalid), "id", model.TaskCheckFail, "different owner or specification")

	n.mu.Lock()
	n.accepting["pending"] = &acceptance{done: make(chan struct{})}
	n.taskIDs["pending"] = "pending"
	n.mu.Unlock()
	wantCheck(t, checkLocal(t, n, checkSpec("pending", `{"n":1}`)), "id", model.TaskCheckWarn, "being accepted")
	wantCheck(t, checkLocal(t, n, checkSpec("Pending", `{"n":1}`)), "id", model.TaskCheckFail, "case-fold conflict")
	n.mu.Lock()
	delete(n.accepting, "pending")
	delete(n.taskIDs, "pending")
	n.mu.Unlock()
}

func TestTaskCheckDuplicatePrunedID(t *testing.T) {
	t.Parallel()
	cfg := Config{Name: "local", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), TaskRetentionHours: 1, MaxRetainedTasks: 1}
	n := openLocalNode(t, cfg)
	spec := model.TaskSpec{ID: "pruned", Capability: "exec.run", Args: model.JSON(map[string]any{"command": "true"}), TimeoutSeconds: 3600}
	old := time.Now().UTC().Add(-2 * time.Hour)
	task := &model.Task{ID: spec.ID, Owner: "owner", Spec: spec, State: "succeeded", Created: old, Updated: old}
	if err := store.Write(n.taskPath(spec.ID), task); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	n.tasks[spec.ID] = task
	n.taskIDs[spec.ID] = spec.ID
	n.mu.Unlock()
	if err := n.pruneTasks(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	_, retained := n.tasks[spec.ID]
	_, tombstone := n.tombstones[spec.ID]
	n.mu.Unlock()
	if retained || !tombstone {
		t.Fatal("task was not pruned to a tombstone")
	}
	report, err := n.checkTask(context.Background(), n.Identity.ID, "owner", model.JSON(spec))
	if err != nil {
		t.Fatal(err)
	}
	wantCheck(t, report, "id", model.TaskCheckPass, "pruned")
	changed := spec
	changed.Args = model.JSON(map[string]any{"command": "false"})
	if report, err = n.checkTask(context.Background(), n.Identity.ID, "owner", model.JSON(changed)); err != nil {
		t.Fatal(err)
	}
	wantCheck(t, report, "id", model.TaskCheckFail, "different owner or specification")
	changed.ID = "PRUNED"
	if report, err = n.checkTask(context.Background(), n.Identity.ID, "owner", model.JSON(changed)); err != nil {
		t.Fatal(err)
	}
	wantCheck(t, report, "id", model.TaskCheckFail, "case-fold conflict")
}

// TestTaskCheckDigestMatchesAcceptance keeps checkedTaskSpec in step with the
// normalization in startTaskContext.
func TestTaskCheckDigestMatchesAcceptance(t *testing.T) {
	t.Parallel()
	n, _, _ := checkNode(t, nil)
	for i, args := range []string{`{"n":1}`, ` { "n" : 1.50e1 , "z":[1, 2.0] } `, `null`, ``, `{"n":12345678901234567890}`} {
		spec := checkSpec(fmt.Sprint("digest-", i), args)
		spec.Outputs = []string{"out"}
		accepted, err := n.startTaskContext(context.Background(), n.Identity.ID, spec)
		if err != nil {
			t.Fatal(err)
		}
		_, digest, err := checkedTaskSpec(spec)
		if err != nil || digest != specDigest(accepted.Spec) {
			t.Fatalf("%q: check digest %s differs from acceptance %s: %v", args, digest, specDigest(accepted.Spec), err)
		}
	}
}

type taskCheckState struct {
	Files                                                      map[string]string
	Tasks, Cold, Cancels, Notifiers, Accepting, Tombstones     int
	TaskIDs                                                    map[string]string
	Lease                                                      string
	LeasePending                                               bool
	Delegations                                                map[string]string
	Pools                                                      string
	WorkActive                                                 int
	WorkPaused                                                 bool
	Activities, RecentActivities, Untracked, RecentTasks       int
	MCPSessions                                                int
	MCPPending                                                 bool
	ActiveInvocations, Transfers, ArtifactStaging, ArtifactsIn int
	ArtifactUsed                                               int64
}

func snapshotTaskCheckState(t *testing.T, n *Node) taskCheckState {
	t.Helper()
	s := taskCheckState{Files: map[string]string{}, TaskIDs: map[string]string{}, Delegations: map[string]string{}}
	for _, root := range []string{n.Config.DataDir, n.Config.WorkDir} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			entry := fmt.Sprint(info.Mode(), info.ModTime().UnixNano())
			if info.Mode().IsRegular() {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				sum := sha256.Sum256(data)
				entry += hex.EncodeToString(sum[:])
			}
			s.Files[path] = entry
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	n.mu.Lock()
	s.Tasks, s.Cold, s.Cancels, s.Notifiers, s.Accepting, s.Tombstones = len(n.tasks), len(n.coldTasks), len(n.cancels), len(n.notifiers), len(n.accepting), len(n.tombstones)
	for k, v := range n.taskIDs {
		s.TaskIDs[k] = v
	}
	s.Lease, s.LeasePending = string(model.JSON(n.lease)), n.leasePending
	s.ActiveInvocations, s.RecentTasks = n.activeInvocations, len(n.recentTasks)
	n.mu.Unlock()
	n.delegationMu.Lock()
	for id, d := range n.delegations {
		s.Delegations[id] = fmt.Sprint(d.used, d.activeTasks, d.lastActivity.UnixNano(), d.ctx.Err())
	}
	n.delegationMu.Unlock()
	s.Pools = string(model.JSON(n.concurrencySnapshot()))
	s.WorkActive, s.WorkPaused = n.work.State()
	n.activityMu.Lock()
	s.Activities, s.RecentActivities, s.Untracked = len(n.activities), len(n.recentActivities), n.untrackedActivities
	n.activityMu.Unlock()
	n.mcpMu.Lock()
	s.MCPSessions, s.MCPPending = len(n.mcpSessions), n.mcpPending != nil
	n.mcpMu.Unlock()
	n.artifactMu.Lock()
	s.Transfers = len(n.transfers)
	n.artifactMu.Unlock()
	n.artifactBudgetMu.Lock()
	s.ArtifactUsed, s.ArtifactStaging, s.ArtifactsIn = n.artifactUsed, len(n.artifactStaging), len(n.artifactReaders)
	n.artifactBudgetMu.Unlock()
	return s
}

// TestTaskCheckChangesNothing runs every kind of check, passing and failing,
// concurrently against a node with a running leased task, a grant and a
// configured MCP server, and compares on-disk and in-memory state.
func TestTaskCheckChangesNothing(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "mcp-server-must-not-start")
	n, entered, release := checkNode(t, func(cfg *Config) {
		cfg.MCP = map[string]Command{"probe": {Command: missing}}
	})
	ctx := context.Background()
	self := n.Identity.ID
	acquired, err := n.leaseMethod(self, "leases.acquire", model.JSON(map[string]any{"ttlSeconds": 600}))
	if err != nil {
		t.Fatal(err)
	}
	lease := acquired.(model.Lease)
	running := checkSpec("running", `{"n":1,"block":true}`)
	running.LeaseID = lease.ID
	if _, err = n.startTaskContext(ctx, self, running); err != nil {
		t.Fatal(err)
	}
	<-entered
	finished := checkSpec("finished", `{"n":2}`)
	finished.LeaseID = lease.ID
	if _, err = n.startTaskContext(ctx, self, finished); err != nil {
		t.Fatal(err)
	}
	if _, err = n.waitTask(ctx, self, "finished", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	grantCtx, cancelGrant := context.WithCancel(ctx)
	defer cancelGrant()
	if err = n.addDelegation(&delegationState{grant: model.Delegation{ID: "grant", Owner: self, Subject: "worker", Method: "tasks.start", Params: model.JSON(checkSpec("granted", `{"n":3}`))}, ctx: grantCtx, cancel: cancelGrant, lastActivity: time.Now()}); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("b", 64)
	artifact := model.Artifact{Node: "elsewhere", ID: digest, SHA256: digest, Size: 1}
	specs := []model.TaskSpec{
		checkSpec("new-task", `{"n":4}`),
		{ID: "leased-new", Capability: "test.check", Args: json.RawMessage(`{"n":4}`), LeaseID: lease.ID, Inputs: []model.Input{{Artifact: artifact, Path: "in"}}, Outputs: []string{"out"}},
		running, finished, checkSpec("RUNNING", `{"n":1}`), checkSpec("running", `{"n":9}`),
		checkSpec("bad id!", `{}`), {Capability: "unknown.capability"},
		{ID: "mcp", Capability: "mcp.call", Args: model.JSON(map[string]any{"server": "probe", "tool": "t"}), LeaseID: lease.ID},
		{ID: "mcp-request", Capability: "mcp.request", Args: model.JSON(map[string]any{"server": "probe", "method": "resources/list"})},
		{ID: "flow", Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": []map[string]any{{"name": "a", "target": "elsewhere", "task": map[string]any{"capability": "exec.run"}}}})},
		{ID: "call", Capability: "peers.call", Args: model.JSON(map[string]any{"target": "elsewhere", "method": "exec.run", "params": map[string]any{}})},
		{ID: "granted", Capability: "test.check", Args: json.RawMessage(`{"n":3}`), LeaseID: "stale"},
		{ID: "pull", Capability: "files.list", Inputs: []model.Input{{Artifact: model.Artifact{Node: "elsewhere", ID: digest, SHA256: digest, Size: 1, Grant: "token"}, Path: "in"}}},
	}
	before := snapshotTaskCheckState(t, n)
	var wg sync.WaitGroup
	for range 4 {
		for _, spec := range specs {
			wg.Go(func() {
				if _, err := n.dispatch(ctx, self, model.TaskCheckMethod, model.JSON(spec)); err != nil {
					t.Error(err)
				}
			})
		}
	}
	wg.Wait()
	after := snapshotTaskCheckState(t, n)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("tasks.check changed node state:\nbefore %+v\nafter  %+v", before, after)
	}
	for _, id := range []string{"new-task", "leased-new", "mcp", "flow", "call", "pull"} {
		if _, err := os.Stat(filepath.Join(n.Config.WorkDir, "tasks", id)); !os.IsNotExist(err) {
			t.Fatal("check created a workspace", id, err)
		}
	}
	// The checked IDs remain free for their real submissions.
	release()
	if _, err := n.waitTask(ctx, self, "running", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	accepted, err := n.startTaskContext(ctx, self, specs[1])
	if err != nil || accepted.ID != "leased-new" {
		t.Fatal("checked task was not accepted afterwards", accepted, err)
	}
}

// A tasks.start grant cannot authorize a check, and a check grant is
// single-use, cannot start the task and leaves submission grants untouched.
func TestTaskCheckGrantsAreSeparate(t *testing.T) {
	t.Parallel()
	n, _, _ := checkNode(t, nil)
	ctx := context.Background()
	spec := model.JSON(checkSpec("granted-task", `{"n":1}`))
	add := func(id, method string) *delegationState {
		grantCtx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		s := &delegationState{grant: model.Delegation{ID: id, Owner: "owner", Subject: n.Identity.ID, Method: method, Params: spec}, ctx: grantCtx, cancel: cancel, lastActivity: time.Now().Add(-time.Minute)}
		if err := n.addDelegation(s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	start := add("start-grant", "tasks.start")
	check := add("check-grant", model.TaskCheckMethod)
	startActivity := start.lastActivity
	if _, _, err := n.acceptAuthority(ctx, n.Identity.ID, model.Request{Method: model.TaskCheckMethod, Params: spec, Delegation: start.grant.ID}); err == nil {
		t.Fatal("a submission grant authorized a check")
	}
	if !start.lastActivity.Equal(startActivity) || start.used || start.ctx.Err() != nil {
		t.Fatal("a refused check used the submission grant")
	}
	if _, _, err := n.acceptAuthority(ctx, n.Identity.ID, model.Request{Method: "tasks.start", Params: spec, Delegation: check.grant.ID}); err == nil {
		t.Fatal("a check grant authorized submission")
	}
	authorized, release, err := n.acceptAuthority(ctx, n.Identity.ID, model.Request{Method: model.TaskCheckMethod, Params: spec, Delegation: check.grant.ID})
	if err != nil {
		t.Fatal(err)
	}
	result, err := n.dispatch(authorized, n.Identity.ID, model.TaskCheckMethod, spec)
	release()
	if err != nil || !result.(model.TaskCheck).OK {
		t.Fatal("check under its exact grant", result, err)
	}
	if _, _, err := n.acceptAuthority(ctx, n.Identity.ID, model.Request{Method: model.TaskCheckMethod, Params: spec, Delegation: check.grant.ID}); err == nil || !strings.Contains(err.Error(), "already been used") {
		t.Fatal("check grant was reusable", err)
	}
	other := model.JSON(checkSpec("granted-task", `{"n":2}`))
	if _, _, err := n.acceptAuthority(ctx, n.Identity.ID, model.Request{Method: model.TaskCheckMethod, Params: other, Delegation: add("other", model.TaskCheckMethod).grant.ID}); err == nil {
		t.Fatal("a check grant authorized different parameters")
	}
	if !start.lastActivity.Equal(startActivity) || start.used || start.ctx.Err() != nil {
		t.Fatal("checks changed the submission grant")
	}
}
