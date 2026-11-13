package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/processutil"
	"github.com/koltyakov/control/internal/transport"
	"github.com/koltyakov/control/internal/transport/transporttest"
	"github.com/koltyakov/control/internal/wire"
)

type lifetimeFixture struct {
	ctx     context.Context
	client  client.Client
	config  client.StandaloneConfig
	first   *Node
	second  *Node
	calls   atomic.Int32
	entered chan struct{}
	release func()
	mode    string
}

// Reply faults affect only the test stream after real destination admission.
// There is no production injection hook or mocked task/grant implementation.
type submissionReplyConn struct {
	net.Conn
	filter  func(*model.Response) bool
	mu      sync.Mutex
	replied bool
}

func (c *submissionReplyConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.replied {
		return c.Conn.Write(b)
	}
	c.replied = true
	// Only the first write is the JSON acknowledgement. Artifact payloads and
	// other streaming bytes after it must pass through without reinterpretation.
	var response model.Response
	if err := wire.ReadFrame(bytes.NewReader(b), &response); err != nil {
		return 0, err
	}
	if !c.filter(&response) {
		_ = c.Close()
		return 0, io.ErrClosedPipe
	}
	if err := wire.WriteFrame(c.Conn, response); err != nil {
		return 0, err
	}
	return len(b), nil
}

func newLifetimeFixture(t *testing.T, relay, system bool, filter func(*model.Response) bool, configure ...func(*Config)) *lifetimeFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	f := &lifetimeFixture{ctx: ctx, entered: make(chan struct{}, 16), mode: "webrtc"}
	if relay {
		f.mode = "relay"
	}
	release := make(chan struct{})
	var once sync.Once
	f.release = func() { once.Do(func() { close(release) }) }
	t.Cleanup(f.release)
	const common = "lifetime-fixture-worker-key-1234567890"
	g, err := gateway.New(t.TempDir(), common, gateway.Options{SuperuserKey: testAccount})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	options := transporttest.Loopback()
	for i, name := range []string{"first", "second"} {
		cfg := Config{Name: name, Gateway: server.URL, Token: common, DataDir: t.TempDir(), WorkDir: t.TempDir(),
			RelayOnly: relay, MaxTasks: 1, InstructionOnly: system && i == 1}
		for _, apply := range configure {
			apply(&cfg)
		}
		n, err := New(cfg, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = n.Close() })
		if err := n.Register(provider{
			cap: model.Capability{Name: "test.lifetime", InputSchema: model.JSON(map[string]any{"type": "object"})},
			run: func(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
				f.calls.Add(1)
				var q struct{ Block, Fail, Output bool }
				if err := json.Unmarshal(args, &q); err != nil {
					return nil, err
				}
				if q.Block {
					select {
					case f.entered <- struct{}{}:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				if q.Fail {
					return nil, errors.New("lifetime fixture failure")
				}
				if q.Output {
					if err := os.WriteFile(filepath.Join(e.Dir, "output"), []byte("workflow output"), 0600); err != nil {
						return nil, err
					}
				}
				return "done", nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		if filter != nil {
			_ = n.Peer.Close()
			n.Peer = transport.New(transport.Config{Gateway: cfg.Gateway, Token: cfg.Token, Identity: n.Identity,
				Node:      model.Node{ID: n.Identity.ID, Name: name, PublicKey: n.Identity.Public, OS: runtime.GOOS, RunAs: processutil.RunAs(), InstructionOnly: cfg.InstructionOnly, GatewayWake: !cfg.InstructionOnly && accountMayCall(cfg.Allow, "network.wake")},
				RelayOnly: relay}, func(caller string, conn net.Conn) {
				n.handle(caller, &submissionReplyConn{Conn: conn, filter: filter})
			}, options)
		}
		if err := n.Start(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			f.first = n
		} else {
			f.second = n
		}
	}
	f.config = client.StandaloneConfig{Gateway: client.Admin{URL: server.URL, Key: testAccount},
		StateDir: t.TempDir(), RelayOnly: relay, AccountRouting: true, PeerOptions: []transport.Options{options}}
	f.client = (client.Client{}).WithStandalone(ctx, f.config)
	t.Cleanup(func() { _ = f.client.Close() })
	return f
}

func (f *lifetimeFixture) grants(t *testing.T, target string) []model.Delegation {
	t.Helper()
	var grants []model.Delegation
	if err := f.client.Call(f.ctx, target, "access.list", nil, &grants); err != nil {
		t.Fatal(err)
	}
	return grants
}

func (f *lifetimeFixture) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-f.entered:
	case <-f.ctx.Done():
		t.Fatal("provider did not start", f.ctx.Err())
	}
}

func (f *lifetimeFixture) assertTransport(t *testing.T) {
	t.Helper()
	info, err := f.client.Session(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Connections[f.first.Identity.ID] != f.mode {
		t.Fatalf("client transport = %q, want %s", info.Connections[f.first.Identity.ID], f.mode)
	}
	if mode := f.first.Peer.Connections()[f.second.Identity.ID]; mode != f.mode {
		t.Fatalf("delegated transport = %q, want %s", mode, f.mode)
	}
}

func lifetimeTask(id string, block bool) model.TaskSpec {
	return model.TaskSpec{ID: id, Capability: "test.lifetime", Args: model.JSON(map[string]bool{"block": block})}
}

func lifetimeWorkflow() model.TaskSpec {
	return model.TaskSpec{ID: "lifetime-workflow", Capability: "workflow.run", Args: model.JSON(map[string]any{
		"steps": []model.WorkflowStep{
			{Name: "a", Target: "second", Task: lifetimeTask("child-a", false)},
			{Name: "b", Target: "second", Task: lifetimeTask("child-b", false)},
		},
	})}
}

func TestDelegationSynchronousCleanup(t *testing.T) {
	for _, relay := range []bool{false, true} {
		for _, outcome := range []string{"success", "error", "cancel"} {
			t.Run(fmt.Sprintf("relay=%v/%s", relay, outcome), func(t *testing.T) {
				t.Parallel()
				f := newLifetimeFixture(t, relay, false, nil)
				ctx, cancel := context.WithCancel(f.ctx)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					done <- f.client.Call(ctx, "first", "peers.call", map[string]any{
						"target": "second", "method": "test.lifetime",
						"params": map[string]bool{"block": outcome == "cancel", "fail": outcome == "error"},
					}, nil)
				}()
				if outcome == "cancel" {
					f.waitEntered(t)
					cancel()
				}
				var err error
				select {
				case err = <-done:
				case <-f.ctx.Done():
					t.Fatal("synchronous call did not return")
				}
				if (err == nil) != (outcome == "success") {
					t.Fatalf("unexpected %s result: %v", outcome, err)
				}
				if grants := f.grants(t, "second"); len(grants) != 0 {
					t.Fatalf("synchronous %s retained %d grants", outcome, len(grants))
				}
				if f.calls.Load() != 1 {
					t.Fatal("provider replayed", f.calls.Load())
				}
				f.assertTransport(t)
			})
		}
	}
}

func TestDelegationDuplicateWorkflowCleanup(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			f := newLifetimeFixture(t, relay, false, nil)
			spec := lifetimeWorkflow()
			var task model.Task
			if err := f.client.Call(f.ctx, "first", "tasks.start", spec, &task); err != nil {
				t.Fatal(err)
			}
			finished, err := f.client.Wait(f.ctx, "first", task.ID)
			if err != nil || finished.State != "succeeded" {
				t.Fatal("workflow failed", finished, err)
			}
			original := f.grants(t, "second")
			if len(original) != 2 {
				t.Fatal("missing original task grants", original)
			}
			for range 3 {
				if err := f.client.Call(f.ctx, "first", "tasks.start", spec, &task); err != nil || !model.SameJSON(model.JSON(task), model.JSON(finished)) {
					t.Fatal("duplicate changed the accepted task", err)
				}
				current := f.grants(t, "second")
				if len(current) != len(original) {
					t.Fatalf("duplicate grant growth: %d -> %d", len(original), len(current))
				}
				for i := range original {
					if current[i].ID != original[i].ID {
						t.Fatal("duplicate replaced original authority")
					}
				}
			}
			if f.calls.Load() != 2 {
				t.Fatal("duplicate executed workflow children", f.calls.Load())
			}
			record, err := os.ReadFile(f.first.taskPath(spec.ID))
			if err != nil || bytes.Contains(record, []byte(`"submission"`)) || bytes.Contains(record, []byte(`"submissionId"`)) {
				t.Fatal("response receipt entered durable task record", err)
			}
			f.assertTransport(t)
		})
	}
}

func TestDelegationConcurrentDuplicateCleanupPreservesWinner(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			f := newLifetimeFixture(t, relay, false, nil)
			spec := model.TaskSpec{ID: "concurrent-parent", Capability: "workflow.run", Args: model.JSON(map[string]any{
				"steps": []model.WorkflowStep{{Name: "blocked", Target: "second", Task: lifetimeTask("blocked-child", true)}},
			})}
			done := make(chan error, 8)
			for range cap(done) {
				go func() { done <- f.client.Call(f.ctx, "first", "tasks.start", spec, nil) }()
			}
			for range cap(done) {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-f.ctx.Done():
					t.Fatal("duplicate callers did not finish")
				}
			}
			f.waitEntered(t)
			if grants := f.grants(t, "second"); len(grants) != 1 {
				t.Fatalf("concurrent submissions retained %d grants", len(grants))
			}
			f.release()
			task, err := f.client.Wait(f.ctx, "first", spec.ID)
			if err != nil || task.State != "succeeded" || f.calls.Load() != 1 {
				t.Fatal("loser cleanup affected winner", task, err, f.calls.Load())
			}
			f.assertTransport(t)
		})
	}
}

func TestDelegationDurableParentPreservesSynchronousGrant(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			f := newLifetimeFixture(t, relay, false, nil)
			// Occupy the destination's only provider slot before accepting the
			// parent. Its synchronous child cannot execute until explicitly freed.
			if err := f.client.Call(f.ctx, "second", "tasks.start", lifetimeTask("occupy", true), nil); err != nil {
				t.Fatal(err)
			}
			f.waitEntered(t)
			parent := model.TaskSpec{ID: "durable-peer", Capability: "peers.call", Args: model.JSON(map[string]any{
				"target": "second", "method": "test.lifetime", "params": map[string]any{},
			})}
			if err := f.client.Call(f.ctx, "first", "tasks.start", parent, nil); err != nil {
				t.Fatal(err)
			}
			if grants := f.grants(t, "second"); len(grants) != 1 || grants[0].Method != "test.lifetime" {
				t.Fatal("durable parent lost its single-use grant", grants)
			}
			_ = f.client.Close()
			f.client = (client.Client{}).WithStandalone(f.ctx, f.config)
			t.Cleanup(func() { _ = f.client.Close() })
			f.release()
			task, err := f.client.Wait(f.ctx, "first", parent.ID)
			if err != nil || task.State != "succeeded" || f.calls.Load() != 2 {
				t.Fatal("client disconnect withdrew durable authority", task, err, f.calls.Load())
			}
		})
	}
}

func TestDelegationSubmissionReceiptFallback(t *testing.T) {
	for _, relay := range []bool{false, true} {
		for _, fault := range []string{"lost-acceptance", "lost-duplicate", "legacy", "wrong-receipt", "wrong-task", "unknown-status"} {
			t.Run(fmt.Sprintf("relay=%v/%s", relay, fault), func(t *testing.T) {
				t.Parallel()
				var faultCount atomic.Int32
				filter := func(r *model.Response) bool {
					if r.Submission == nil || r.Submission.TaskID != "lifetime-workflow" {
						return true
					}
					if fault == "lost-acceptance" && r.Submission.Status == "accepted" {
						faultCount.Add(1)
						return false
					}
					if r.Submission.Status == "existing" {
						faultCount.Add(1)
						switch fault {
						case "lost-duplicate":
							return false
						case "legacy":
							r.Submission = nil
						case "wrong-receipt":
							r.Submission.ID = "not-the-current-submission"
						case "wrong-task":
							r.Submission.TaskID = "another-task"
						case "unknown-status":
							r.Submission.Status = "unknown"
						}
					}
					return true
				}
				f := newLifetimeFixture(t, relay, false, filter)
				spec := lifetimeWorkflow()
				err := f.client.Call(f.ctx, "first", "tasks.start", spec, nil)
				if fault == "lost-acceptance" {
					var rpc *model.RPCError
					if !errors.As(err, &rpc) || rpc.Outcome != "unknown" {
						t.Fatal("lost reply was not uncertain", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				task, err := f.client.Wait(f.ctx, "first", spec.ID)
				if err != nil || task.State != "succeeded" {
					t.Fatal("uncertain accepted work lost authority", task, err)
				}
				if fault != "lost-acceptance" {
					err = f.client.Call(f.ctx, "first", "tasks.start", spec, nil)
					if (err != nil) != (fault == "lost-duplicate") {
						t.Fatal("unexpected duplicate outcome", err)
					}
				}
				want := 4
				if fault == "lost-acceptance" {
					want = 2
				}
				if grants := f.grants(t, "second"); len(grants) != want {
					t.Fatalf("unsafe fallback cleanup: got %d grants, want %d", len(grants), want)
				}
				if faultCount.Load() != 1 || f.calls.Load() != 2 {
					t.Fatal("fault not exercised or work replayed", faultCount.Load(), f.calls.Load())
				}
				f.assertTransport(t)
			})
		}
	}
}

func TestDelegationHybridDuplicateCleanup(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			f := newLifetimeFixture(t, relay, true, nil)
			spec := lifetimeTask("system-task", true)
			for range 3 {
				if err := f.client.Call(f.ctx, "second", "tasks.start", spec, nil); err != nil {
					t.Fatal(err)
				}
			}
			f.waitEntered(t)
			if grants := f.grants(t, "second"); len(grants) != 1 {
				t.Fatal("System duplicate grant growth", grants)
			}
			f.release()
			task, err := f.client.Wait(f.ctx, "second", spec.ID)
			if err != nil || task.State != "succeeded" || f.calls.Load() != 1 {
				t.Fatal("System duplicate withdrew original grant", task, err)
			}
		})
	}
}

func TestDelegationForwardedChildSurvivesReturn(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			f := newLifetimeFixture(t, relay, false, nil)
			spec := lifetimeTask("forwarded-child", true)
			request := map[string]any{"target": "second", "method": "tasks.start", "params": spec}
			for range 3 {
				if err := f.client.Call(f.ctx, "first", "peers.call", request, nil); err != nil {
					t.Fatal(err)
				}
			}
			f.waitEntered(t)
			if grants := f.grants(t, "second"); len(grants) != 1 {
				t.Fatal("forwarded duplicate did not preserve exactly the original grant", grants)
			}
			f.assertTransport(t)
			_ = f.client.Close()
			f.client = (client.Client{}).WithStandalone(f.ctx, f.config)
			t.Cleanup(func() { _ = f.client.Close() })
			f.release()
			task, err := f.client.Wait(f.ctx, "second", spec.ID)
			if err != nil || task.State != "succeeded" || f.calls.Load() != 1 {
				t.Fatal("synchronous parent return cancelled durable child", task, err)
			}
		})
	}
}

func TestDelegationLostForwardedAcceptancePreservesChild(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			f := newLifetimeFixture(t, relay, false, func(r *model.Response) bool {
				return r.Submission == nil || r.Submission.TaskID != "lost-child" || r.Submission.Status != "accepted"
			})
			spec := lifetimeTask("lost-child", true)
			err := f.client.Call(f.ctx, "first", "peers.call", map[string]any{
				"target": "second", "method": "tasks.start", "params": spec,
			}, nil)
			var rpc *model.RPCError
			if !errors.As(err, &rpc) || rpc.Outcome != "unknown" {
				t.Fatal("lost forwarded acceptance was not uncertain", err)
			}
			f.waitEntered(t)
			if grants := f.grants(t, "second"); len(grants) != 1 {
				t.Fatal("uncertain child authority withdrawn", grants)
			}
			f.release()
			task, err := f.client.Wait(f.ctx, "second", spec.ID)
			if err != nil || task.State != "succeeded" || f.calls.Load() != 1 {
				t.Fatal("uncertain child cancelled or replayed", task, err, f.calls.Load())
			}
			f.assertTransport(t)
		})
	}
}

func TestDelegationLostAcceptancePreservesWorkflowOutputs(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			f := newLifetimeFixture(t, relay, false, func(r *model.Response) bool {
				return r.Submission == nil || r.Submission.TaskID != "output-parent" || r.Submission.Status != "accepted"
			})
			source := lifetimeTask("output-source", false)
			source.Args, source.Outputs = model.JSON(map[string]bool{"output": true}), []string{"output"}
			steps := []model.WorkflowStep{
				{Name: "source", Target: "second", Task: source},
				{Name: "consumer", Target: "first", Task: lifetimeTask("output-consumer", false)},
			}
			steps[1].Inputs = append(steps[1].Inputs, struct {
				Step     string `json:"step"`
				Artifact int    `json:"artifact"`
				Path     string `json:"path"`
			}{Step: "source", Path: "input"})
			spec := model.TaskSpec{ID: "output-parent", Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": steps})}
			if err := f.client.Call(f.ctx, "first", "tasks.start", spec, nil); err == nil {
				t.Fatal("acceptance reply was not lost")
			}
			task, err := f.client.Wait(f.ctx, "first", spec.ID)
			if err != nil || task.State != "succeeded" {
				t.Fatal("uncertain workflow failed", task, err)
			}
			var children map[string]model.Task
			if err := json.Unmarshal(task.Result, &children); err != nil {
				t.Fatal(err)
			}
			consumer := children["consumer"]
			if len(consumer.Spec.Inputs) != 1 {
				t.Fatal("missing workflow consumer input", children)
			}
			artifact := consumer.Spec.Inputs[0].Artifact
			var content bytes.Buffer
			if err := f.first.Download(f.ctx, artifact, 0, &content); err != nil || content.String() != "workflow output" {
				t.Fatal("workflow output authority withdrawn after completion", content.String(), err)
			}
			var sourceGrant string
			for _, grant := range f.grants(t, "second") {
				if grant.TaskID() == source.ID {
					sourceGrant = grant.ID
				}
			}
			if sourceGrant == "" {
				t.Fatal("missing source task authority")
			}
			if err := f.client.Call(f.ctx, "second", "access.revoke", map[string]string{"id": sourceGrant}, nil); err != nil {
				t.Fatal(err)
			}
			if err := f.first.Download(f.ctx, artifact, 0, io.Discard); err == nil {
				t.Fatal("source revocation left derived output access live")
			}
			f.assertTransport(t)
		})
	}
}

func TestDelegationRejectedSubmissionCleansOnlyFreshGrants(t *testing.T) {
	f := newLifetimeFixture(t, true, false, nil)
	spec := lifetimeWorkflow()
	if err := f.client.Call(f.ctx, "first", "tasks.start", spec, nil); err != nil {
		t.Fatal(err)
	}
	if task, err := f.client.Wait(f.ctx, "first", spec.ID); err != nil || task.State != "succeeded" {
		t.Fatal(task, err)
	}
	original := f.grants(t, "second")
	var changed map[string]any
	if err := json.Unmarshal(spec.Args, &changed); err != nil {
		t.Fatal(err)
	}
	changed["maxParallel"] = 2
	spec.Args = model.JSON(changed)
	if err := f.client.Call(f.ctx, "first", "tasks.start", spec, nil); err == nil {
		t.Fatal("conflicting parent specification accepted")
	}
	current := f.grants(t, "second")
	if len(current) != len(original) || f.calls.Load() != 2 {
		t.Fatal("rejection retained fresh authority or replayed work", current)
	}
	for i := range original {
		if original[i].ID != current[i].ID {
			t.Fatal("rejection removed the original grant")
		}
	}
}

func TestDelegationCleanupFailureDoesNotChangeSuccess(t *testing.T) {
	f := newLifetimeFixture(t, true, false, nil, func(cfg *Config) {
		// Intentionally deny revocation while allowing this synthetic operation.
		cfg.Allow = map[string][]string{"@orchestrator": {"access.grant", "access.list", "peers.call", "test.lifetime", "node.describe"}}
	})
	if err := f.client.Call(f.ctx, "first", "peers.call", map[string]any{
		"target": "second", "method": "test.lifetime", "params": map[string]any{},
	}, nil); err != nil {
		t.Fatal("cleanup failure changed successful operation result", err)
	}
	if grants := f.grants(t, "second"); len(grants) != 1 || f.calls.Load() != 1 {
		t.Fatal("cleanup failure was not exercised or execution replayed", grants, f.calls.Load())
	}
}

func TestDelegationDeliveryCleanupKeepsArtifactAccessSeparate(t *testing.T) {
	f := newLifetimeFixture(t, true, false, nil)
	if err := os.WriteFile(filepath.Join(f.first.Config.WorkDir, "input"), []byte("delivery fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	var a model.Artifact
	if err := f.client.Call(f.ctx, "first", "artifacts.export", map[string]string{"path": "input"}, &a); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Call(f.ctx, "first", "artifacts.deliver", map[string]string{"id": a.ID, "target": "second"}, nil); err != nil {
		t.Fatal(err)
	}
	if grants := f.grants(t, "second"); len(grants) != 0 {
		t.Fatal("delivery pull authority retained", grants)
	}
	if grants := f.grants(t, "first"); len(grants) != 1 || grants[0].Method != "artifacts.open" {
		t.Fatal("source artifact access confused with temporary pull authority", grants)
	}
}
