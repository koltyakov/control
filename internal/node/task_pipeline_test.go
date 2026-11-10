package node

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport/transporttest"
)

type pipelineArgs struct {
	Output       string `json:"output,omitempty"`
	LargeResult  bool   `json:"largeResult,omitempty"`
	InlineResult string `json:"inlineResult,omitempty"`
	Fail         bool   `json:"fail,omitempty"`
}

func pipelineSpec(id string, args pipelineArgs) model.TaskSpec {
	spec := model.TaskSpec{ID: id, Capability: "test.pipeline", Args: model.JSON(args)}
	if args.Output != "" {
		spec.Outputs = []string{"output.txt"}
	}
	return spec
}

func pipelineNode(t *testing.T) (*Node, *atomic.Int32) {
	t.Helper()
	n := localNode(t, func(cfg *Config) {
		cfg.MaxTasks, cfg.MaxTaskStaging, cfg.MaxTaskPublications = 1, 1, 1
	})
	calls := &atomic.Int32{}
	err := n.Register(provider{cap: model.Capability{Name: "test.pipeline", InputSchema: schema(nil)}, run: func(ctx context.Context, raw json.RawMessage, e Execution) (any, error) {
		calls.Add(1)
		var args pipelineArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if args.Output != "" {
			if err := os.WriteFile(filepath.Join(e.Dir, "output.txt"), []byte(args.Output), 0600); err != nil {
				return nil, err
			}
		}
		var result any
		if args.InlineResult != "" {
			result = args.InlineResult
		}
		if args.LargeResult {
			result = strings.Repeat("z", 2<<20)
		}
		if args.Fail {
			return result, errors.New("provider failed with partial output")
		}
		return result, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return n, calls
}

func holdPipelineTransfer(t *testing.T, n *Node, id string) func() {
	t.Helper()
	unlock, err := n.lockTransfer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(unlock) }
	t.Cleanup(release)
	return release
}

func pipelineInput(t *testing.T, n *Node) model.Input {
	t.Helper()
	a, err := n.importArtifact(t.Context(), strings.NewReader("input payload"), "input.txt")
	if err != nil {
		t.Fatal(err)
	}
	return model.Input{Artifact: a, Path: "input.txt"}
}

func pipelineFinished(t *testing.T, n *Node, ctx context.Context, owner, id, state string) model.Task {
	t.Helper()
	got, err := n.waitTask(ctx, owner, id, 5*time.Second)
	if err != nil || got.State != state {
		t.Fatalf("task %s: state=%s error=%q, want %s: %v", id, got.State, got.Error, state, err)
	}
	return got
}

func TestTaskInputStagingDoesNotHoldExecution(t *testing.T) {
	n, calls := pipelineNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	input := pipelineInput(t, n)
	holdPipelineTransfer(t, n, input.Artifact.ID)
	owner := n.Identity.ID
	for _, id := range []string{"staging-active", "staging-queued"} {
		spec := pipelineSpec(id, pipelineArgs{})
		spec.Inputs = []model.Input{input}
		if _, err := n.startTaskContext(ctx, owner, spec); err != nil {
			t.Fatal(err)
		}
		if id == "staging-active" {
			eventually(t, ctx, func() bool { return n.inputStagingPool.snapshot().InUse == 1 })
		}
	}
	eventually(t, ctx, func() bool { return *n.inputStagingPool.snapshot().Queued == 1 })
	assertPool(t, n.executionPool.snapshot(), 1, 0, 0)
	assertPool(t, n.inputStagingPool.snapshot(), 1, 1, 1)
	fast := pipelineSpec("no-inputs", pipelineArgs{})
	if _, err := n.startTaskContext(ctx, owner, fast); err != nil {
		t.Fatal(err)
	}
	pipelineFinished(t, n, ctx, owner, fast.ID, "succeeded")
	if calls.Load() != 1 {
		t.Fatal("staging task executed before its inputs arrived", calls.Load())
	}
	var activity model.NodeActivitySnapshot
	if err := n.Call(ctx, "", "activities.list", model.ActivityQuery{}, &activity); err != nil || activity.Concurrency == nil || activity.Concurrency.InputStaging == nil {
		t.Fatal("missing stage visibility", err)
	}
	assertPool(t, *activity.Concurrency.InputStaging, 1, 1, 1)
	for _, id := range []string{"staging-queued", "staging-active"} {
		if _, err := n.taskMethod(ctx, owner, "tasks.cancel", model.JSON(map[string]string{"id": id})); err != nil {
			t.Fatal(err)
		}
		pipelineFinished(t, n, ctx, owner, id, "cancelled")
	}
	eventually(t, ctx, func() bool { return n.inputStagingPool.snapshot().InUse == 0 })
	assertPool(t, n.inputStagingPool.snapshot(), 1, 0, 0)
	if calls.Load() != 1 {
		t.Fatal("cancelled input staging ran a provider")
	}
}

func TestTaskOutputPublicationDoesNotHoldExecution(t *testing.T) {
	n, calls := pipelineNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	const body = "held publication"
	release := holdPipelineTransfer(t, n, fmt.Sprintf("%x", sha256.Sum256([]byte(body))))
	owner := "original-owner"
	first := pipelineSpec("publication-active", pipelineArgs{Output: body, LargeResult: true})
	if _, err := n.startTaskContext(ctx, owner, first); err != nil {
		t.Fatal(err)
	}
	eventually(t, ctx, func() bool { return n.publicationPool.snapshot().InUse == 1 })
	assertPool(t, n.executionPool.snapshot(), 1, 0, 0)
	second := pipelineSpec("publication-queued", pipelineArgs{Output: "second output"})
	if _, err := n.startTaskContext(ctx, owner, second); err != nil {
		t.Fatal(err)
	}
	eventually(t, ctx, func() bool { return *n.publicationPool.snapshot().Queued == 1 })
	assertPool(t, n.executionPool.snapshot(), 1, 0, 0)
	fast := pipelineSpec("no-publication", pipelineArgs{})
	if _, err := n.startTaskContext(ctx, owner, fast); err != nil {
		t.Fatal(err)
	}
	pipelineFinished(t, n, ctx, owner, fast.ID, "succeeded")
	if calls.Load() != 3 {
		t.Fatal("publication blocked independent provider work", calls.Load())
	}
	if _, err := n.taskMethod(ctx, "other-owner", "tasks.get", model.JSON(map[string]string{"id": first.ID})); err == nil {
		t.Fatal("publication wait widened task ownership")
	}
	var description struct {
		Concurrency model.NodeConcurrency `json:"concurrency"`
	}
	if err := n.Call(ctx, "", "node.describe", map[string]any{}, &description); err != nil || description.Concurrency.OutputPublication == nil {
		t.Fatal("publication visibility missing", err)
	}
	assertPool(t, *description.Concurrency.OutputPublication, 1, 1, 1)
	release()
	finished := pipelineFinished(t, n, ctx, owner, first.ID, "succeeded")
	pipelineFinished(t, n, ctx, owner, second.ID, "succeeded")
	if len(finished.Artifacts) != 1 || finished.ResultArtifact == nil || len(finished.Result) != 0 {
		t.Fatal("publication lost declared output or large result", finished)
	}
	for _, a := range []model.Artifact{finished.Artifacts[0], *finished.ResultArtifact} {
		stored, err := n.storedArtifact(a.ID)
		if err != nil || !slices.Contains(stored.Owners, owner) {
			t.Fatal("publication lost original artifact owner", stored, err)
		}
	}
	data, err := os.ReadFile(n.artifactPath(finished.ResultArtifact.ID))
	if err != nil || string(data) != string(model.JSON(strings.Repeat("z", 2<<20))) {
		t.Fatal("large result content changed", err)
	}
	if _, err := n.startTaskContext(ctx, owner, first); err != nil || calls.Load() != 3 {
		t.Fatal("reconciliation replayed an executed provider", calls.Load(), err)
	}
	eventually(t, ctx, func() bool { return n.publicationPool.snapshot().InUse == 0 })
	assertPool(t, n.publicationPool.snapshot(), 1, 0, 0)
}

func TestTaskPublicationCancellationDoesNotReplay(t *testing.T) {
	n, calls := pipelineNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	release, err := acquireProviderSlot(ctx, n.publicationPool)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	spec := pipelineSpec("cancel-publication", pipelineArgs{Output: "not published", LargeResult: true})
	if _, err := n.startTaskContext(ctx, n.Identity.ID, spec); err != nil {
		t.Fatal(err)
	}
	eventually(t, ctx, func() bool { return *n.publicationPool.snapshot().Queued == 1 })
	if _, err := n.taskMethod(ctx, n.Identity.ID, "tasks.cancel", model.JSON(map[string]string{"id": spec.ID})); err != nil {
		t.Fatal(err)
	}
	finished := pipelineFinished(t, n, ctx, n.Identity.ID, spec.ID, "cancelled")
	if len(finished.Artifacts) != 0 || finished.ResultArtifact != nil {
		t.Fatal("cancelled publication created artifacts")
	}
	assertPool(t, n.publicationPool.snapshot(), 1, 1, 0)
	if _, err := n.startTaskContext(ctx, n.Identity.ID, spec); err != nil || calls.Load() != 1 {
		t.Fatal("cancelled publication replayed provider", err)
	}
}

func TestTaskPublicationFailureDoesNotReplay(t *testing.T) {
	n, calls := pipelineNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	spec := pipelineSpec("failed-publication", pipelineArgs{LargeResult: true})
	spec.Outputs = []string{"missing.txt"}
	if _, err := n.startTaskContext(ctx, n.Identity.ID, spec); err != nil {
		t.Fatal(err)
	}
	finished := pipelineFinished(t, n, ctx, n.Identity.ID, spec.ID, "failed")
	if !strings.Contains(finished.Error, "missing.txt") || finished.ResultArtifact == nil {
		t.Fatal("publication failure lost diagnostic or provider result", finished)
	}
	if _, err := n.startTaskContext(ctx, n.Identity.ID, spec); err != nil || calls.Load() != 1 {
		t.Fatal("publication failure replayed provider", err)
	}
	eventually(t, ctx, func() bool { return n.publicationPool.snapshot().InUse == 0 })
	assertPool(t, n.publicationPool.snapshot(), 1, 0, 0)
}

func TestTaskInlineResultsBypassPublication(t *testing.T) {
	n, _ := pipelineNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	release, err := acquireProviderSlot(ctx, n.publicationPool)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, fail := range []bool{false, true} {
		spec := pipelineSpec(fmt.Sprintf("inline-%v", fail), pipelineArgs{InlineResult: "retained diagnostics", Fail: fail})
		if _, err := n.startTaskContext(ctx, n.Identity.ID, spec); err != nil {
			t.Fatal(err)
		}
		state := "succeeded"
		if fail {
			state = "failed"
		}
		got := pipelineFinished(t, n, ctx, n.Identity.ID, spec.ID, state)
		if string(got.Result) != `"retained diagnostics"` || got.ResultArtifact != nil {
			t.Fatal("inline result lost", got)
		}
		assertPool(t, n.publicationPool.snapshot(), 1, 1, 0)
	}
}

func TestTaskCancelledPublicationRetainsInlineResult(t *testing.T) {
	n, _ := pipelineNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	release, err := acquireProviderSlot(ctx, n.publicationPool)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	spec := pipelineSpec("cancel-inline-publication", pipelineArgs{Output: "not published", InlineResult: "retained diagnostics"})
	if _, err := n.startTaskContext(ctx, n.Identity.ID, spec); err != nil {
		t.Fatal(err)
	}
	eventually(t, ctx, func() bool { return *n.publicationPool.snapshot().Queued == 1 })
	if _, err := n.taskMethod(ctx, n.Identity.ID, "tasks.cancel", model.JSON(map[string]string{"id": spec.ID})); err != nil {
		t.Fatal(err)
	}
	got := pipelineFinished(t, n, ctx, n.Identity.ID, spec.ID, "cancelled")
	if string(got.Result) != `"retained diagnostics"` || len(got.Artifacts) != 0 || got.ResultArtifact != nil {
		t.Fatal("cancelled publication lost inline diagnostics or created artifacts", got)
	}
	assertPool(t, n.publicationPool.snapshot(), 1, 1, 0)
}

func TestTaskPublicationRetainsIndependentProviderResults(t *testing.T) {
	n, _ := pipelineNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	shared := map[string]string{}
	if err := n.Register(provider{cap: model.Capability{Name: "test.shared-result", InputSchema: schema(nil)}, run: func(ctx context.Context, raw json.RawMessage, e Execution) (any, error) {
		shared["id"] = filepath.Base(e.Dir)
		return shared, os.WriteFile(filepath.Join(e.Dir, "output.txt"), []byte("output"), 0600)
	}}); err != nil {
		t.Fatal(err)
	}
	release, err := acquireProviderSlot(ctx, n.publicationPool)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unblock := func() { once.Do(release) }
	defer unblock()
	for i, id := range []string{"result-one", "result-two"} {
		if _, err := n.startTaskContext(ctx, n.Identity.ID, model.TaskSpec{ID: id, Capability: "test.shared-result", Args: model.JSON(map[string]any{}), Outputs: []string{"output.txt"}}); err != nil {
			t.Fatal(err)
		}
		eventually(t, ctx, func() bool { return *n.publicationPool.snapshot().Queued == i+1 })
	}
	assertPool(t, n.executionPool.snapshot(), 1, 0, 0)
	unblock()
	for _, id := range []string{"result-one", "result-two"} {
		got := pipelineFinished(t, n, ctx, n.Identity.ID, id, "succeeded")
		var result map[string]string
		if err := json.Unmarshal(got.Result, &result); err != nil || result["id"] != id {
			t.Fatal("provider reused another task's unpublished result", id, result, err)
		}
	}
}

func TestTaskStageLifecycleProtection(t *testing.T) {
	for _, stage := range []string{"input", "input-queue", "output", "output-queue"} {
		for _, revoke := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/revoke=%v", stage, revoke), func(t *testing.T) {
				n, calls := pipelineNode(t)
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				const owner = "lease-and-task-owner"
				value, err := n.leaseMethod(owner, "leases.acquire", model.JSON(map[string]any{}))
				if err != nil {
					t.Fatal(err)
				}
				lease := value.(model.Lease)
				spec := pipelineSpec("protected", pipelineArgs{})
				spec.LeaseID = lease.ID
				var unblock func()
				var pool *providerPool
				if strings.HasPrefix(stage, "input") {
					input := pipelineInput(t, n)
					spec.Inputs = []model.Input{input}
					pool = n.inputStagingPool
					unblock = holdPipelineTransfer(t, n, input.Artifact.ID)
				} else {
					args := pipelineArgs{Output: "protected output"}
					spec.Args, spec.Outputs = model.JSON(args), []string{"output.txt"}
					pool = n.publicationPool
					unblock = holdPipelineTransfer(t, n, fmt.Sprintf("%x", sha256.Sum256([]byte(args.Output))))
				}
				if strings.HasSuffix(stage, "queue") {
					unblock()
					release, acquireErr := acquireProviderSlot(ctx, pool)
					err = acquireErr
					if err != nil {
						t.Fatal(err)
					}
					var once sync.Once
					unblock = func() { once.Do(release) }
					defer unblock()
				}
				grantCtx, revokeGrant := context.WithCancel(n.ctx)
				defer revokeGrant()
				grant := &delegationState{grant: model.Delegation{ID: "pipeline-grant", Owner: owner, Method: "tasks.start", Params: model.JSON(spec)}, ctx: grantCtx, cancel: revokeGrant, lastActivity: time.Now()}
				n.delegations[grant.grant.ID] = grant
				parent, stopParent := context.WithCancel(context.WithValue(ctx, authorityContextKey{}, authority{owner: owner, grant: grant}))
				defer stopParent()
				if _, err := n.startTaskContext(parent, owner, spec); err != nil {
					t.Fatal(err)
				}
				eventually(t, ctx, func() bool {
					s := pool.snapshot()
					return strings.HasSuffix(stage, "queue") && *s.Queued == 1 || !strings.HasSuffix(stage, "queue") && s.InUse == 1
				})
				stopParent() // Accepted work must not inherit the caller's lifetime.
				n.expireDelegations(time.Now().Add(2 * time.Hour))
				if grantCtx.Err() != nil {
					t.Fatal("accepted stage lost active-task authority")
				}
				n.mu.Lock()
				n.lease.Expires = time.Now().Add(-time.Minute)
				n.mu.Unlock()
				if _, err := n.leaseMethod(owner, "leases.release", model.JSON(map[string]string{"id": lease.ID})); err == nil {
					t.Fatal("released a lease during task transfer")
				}
				if _, err := n.leaseMethod("another-owner", "leases.acquire", model.JSON(map[string]any{})); err == nil {
					t.Fatal("expired lease permitted overlap during transfer")
				}
				if n.pauseForUpdate() {
					t.Fatal("maintenance ignored accepted stage work")
				}
				n.work.SetDisabled(true)
				if _, err := n.startTaskContext(ctx, owner, pipelineSpec("new-work", pipelineArgs{})); err == nil {
					t.Fatal("disabled node admitted new work")
				}
				want := "succeeded"
				if revoke {
					if strings.HasSuffix(stage, "queue") {
						n.delegationMu.Lock()
						grant.expiresAt = time.Now().Add(-time.Second)
						n.delegationMu.Unlock()
						n.expireDelegations(time.Now())
					} else {
						revokeGrant()
					}
					want = "cancelled"
				} else {
					unblock()
				}
				pipelineFinished(t, n, ctx, owner, spec.ID, want)
				if strings.HasPrefix(stage, "input") && revoke && calls.Load() != 0 {
					t.Fatal("revoked staging executed provider")
				}
				eventually(t, ctx, func() bool { n.delegationMu.Lock(); defer n.delegationMu.Unlock(); return grant.activeTasks == 0 })
				if _, err := n.leaseMethod(owner, "leases.release", model.JSON(map[string]string{"id": lease.ID})); err != nil {
					t.Fatal("terminal task retained lease", err)
				}
				n.work.SetDisabled(false)
				eventually(t, ctx, n.pauseForUpdate)
				n.work.Resume()
			})
		}
	}
}

func TestTaskStageShutdownAndRestartDoNotReplay(t *testing.T) {
	for _, stage := range []string{"input", "output"} {
		t.Run(stage, func(t *testing.T) {
			n, calls := pipelineNode(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			spec := pipelineSpec("interrupted-stage", pipelineArgs{})
			pool := n.inputStagingPool
			if stage == "input" {
				input := pipelineInput(t, n)
				spec.Inputs = []model.Input{input}
				holdPipelineTransfer(t, n, input.Artifact.ID)
			} else {
				spec = pipelineSpec(spec.ID, pipelineArgs{Output: "interrupted output"})
				pool = n.publicationPool
				holdPipelineTransfer(t, n, fmt.Sprintf("%x", sha256.Sum256([]byte("interrupted output"))))
			}
			if _, err := n.startTaskContext(ctx, n.Identity.ID, spec); err != nil {
				t.Fatal(err)
			}
			eventually(t, ctx, func() bool { return pool.snapshot().InUse == 1 })
			if err := n.Close(); err != nil {
				t.Fatal("shutdown did not join stage work", err)
			}
			assertPool(t, pool.snapshot(), 1, 0, 0)
			before := calls.Load()
			restarted := openLocalNode(t, n.Config)
			if err := restarted.Register(provider{cap: model.Capability{Name: spec.Capability, InputSchema: schema(nil)}, run: func(context.Context, json.RawMessage, Execution) (any, error) { calls.Add(1); return nil, nil }}); err != nil {
				t.Fatal(err)
			}
			got, err := restarted.startTaskContext(ctx, restarted.Identity.ID, spec)
			if err != nil || got.State != "interrupted" || calls.Load() != before {
				t.Fatal("restart replayed interrupted transfer stage", got.State, calls.Load(), err)
			}
		})
	}
}

func TestPeerTaskTransferStagesRetainAuthority(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, consumer := cluster(t, relay, func(i int, cfg *Config) {
				cfg.MaxTasks, cfg.MaxTaskStaging, cfg.MaxTaskPublications = 1, 1, 1
			}, transporttest.Loopback())
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			for _, stage := range []string{"input", "output"} {
				input := pipelineInput(t, worker)
				spec := execSpec(t, "delegated-"+stage, "encode")
				spec.Inputs, spec.Outputs = []model.Input{{Artifact: input.Artifact, Path: "input.bin"}}, []string{"output.bin"}
				artifactID := input.Artifact.ID
				pool := worker.inputStagingPool
				if stage == "output" {
					artifactID = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.ToUpper("input payload"))))
					pool = worker.publicationPool
				}
				unblock := holdPipelineTransfer(t, worker, artifactID)
				var grant model.Delegation
				call(t, source, "worker", "access.grant", model.Delegation{Subject: "source", Method: "tasks.start", Params: model.JSON(spec)}, &grant)
				var task model.Task
				if err := source.Call(model.WithDelegations(ctx, []model.Delegation{grant}), "worker", "tasks.start", spec, &task); err != nil {
					t.Fatal(err)
				}
				eventually(t, ctx, func() bool { return pool.snapshot().InUse == 1 })
				if task.Owner != testOrchestrator(t, source).owner.ID {
					t.Fatal("stage lost orchestrator owner", task.Owner)
				}
				assertPool(t, worker.executionPool.snapshot(), 1, 0, 0)
				if err := testCall(t, consumer, ctx, "worker", "tasks.cancel", map[string]string{"id": spec.ID}, nil); err == nil {
					t.Fatal("another owner controlled staged work")
				}
				call(t, source, "worker", "access.revoke", map[string]string{"id": grant.ID}, nil)
				finished, err := testWaitTask(t, source, ctx, "worker", spec.ID)
				if err != nil || finished.State != "cancelled" {
					t.Fatal("revocation did not cancel task transfer stage", finished.State, err)
				}
				eventually(t, ctx, func() bool { return pool.snapshot().InUse == 0 })
				unblock()
			}
			want := "webrtc"
			if relay {
				want = "relay"
			}
			if got := source.Peer.Connections()[worker.Identity.ID]; got != want {
				t.Fatalf("delegated worker transport=%q, want %q", got, want)
			}
		})
	}
}
