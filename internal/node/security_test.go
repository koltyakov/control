package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

func TestCoordinationRequiresLocalDelegation(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, _ := cluster(t, relay, func(i int, cfg *Config) {
				if i == 1 {
					cfg.Allow = map[string][]string{"source": {"peers.call", "workflow.run", "tasks.start", "tasks.get"}}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := executable(t, "agent")
			if conn, _, err := testOpen(t, source, ctx, "worker", "exec.run", command); err == nil {
				_ = conn.Close()
				t.Fatal("restricted orchestrator executed directly")
			}
			for _, target := range []string{"worker", worker.Identity.ID} {
				params := map[string]any{"target": target, "method": "exec.run", "params": command}
				if conn, _, err := testOpen(t, source, ctx, "worker", "peers.call", params); err == nil {
					_ = conn.Close()
					t.Fatal("self-routing bypassed execution permission")
				}
			}
			workflow := map[string]any{"steps": []model.WorkflowStep{{Name: "local", Target: "worker", Task: model.TaskSpec{ID: "unauthorized-child", Capability: "exec.run", Args: model.JSON(command)}}}}
			if conn, _, err := testOpen(t, source, ctx, "worker", "workflow.run", workflow); err == nil {
				_ = conn.Close()
				t.Fatal("local workflow bypassed execution permission")
			}
			// Accepted coordination tasks retain the same boundary after admission.
			for _, capability := range []string{"workflow.run", "peers.call"} {
				args := workflow
				if capability == "peers.call" {
					args = map[string]any{"target": "worker", "method": "exec.run", "params": command}
				}
				conn, data, err := testOpen(t, source, ctx, "worker", "tasks.start", model.TaskSpec{ID: "parent-" + strings.ReplaceAll(capability, ".", "-"), Capability: capability, Args: model.JSON(args)})
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.Close()
				var parent model.Task
				if err = json.Unmarshal(data, &parent); err != nil {
					t.Fatal(err)
				}
				finished, err := testWaitTask(t, source, ctx, "worker", parent.ID)
				if err != nil || finished.State != "failed" {
					t.Fatalf("unauthorized coordination task: %+v, %v", finished, err)
				}
			}
			worker.mu.Lock()
			child := worker.tasks["unauthorized-child"]
			worker.mu.Unlock()
			if child != nil {
				t.Fatal("unauthorized child was accepted")
			}
			// The shared client prepares a destination grant for legitimate local work.
			if err := testCall(t, worker, ctx, "worker", "peers.call", map[string]any{"target": "worker", "method": "exec.run", "params": command}, nil); err != nil {
				t.Fatal("authorized local delegation:", err)
			}
		})
	}
}

func TestArtifactAccessCannotOutliveSignedGrant(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, consumer := cluster(t, relay, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := os.WriteFile(filepath.Join(worker.Config.WorkDir, "private.txt"), []byte("private artifact"), 0600); err != nil {
				t.Fatal(err)
			}
			var artifact model.Artifact
			call(t, source, "worker", "artifacts.export", map[string]any{"path": "private.txt"}, &artifact)
			// Establish the download lane before starting the short grant lifetime.
			// A cold WebRTC attempt can take five seconds before relay fallback.
			warm, err := consumer.Peer.OpenLane(ctx, "worker", transport.BulkLane)
			if err != nil {
				t.Fatal(err)
			}
			_ = warm.Close()
			call(t, source, "worker", "artifacts.grant", map[string]any{"id": artifact.ID, "target": "consumer", "ttlSeconds": 3}, &artifact)
			var proof artifactGrant
			if !worker.decodeGrant(artifact.Grant, &proof) {
				t.Fatal("invalid signed grant")
			}
			grant := model.Delegation{ID: proof.Access, Target: worker.Identity.ID, Method: "artifacts.open", Params: model.JSON(map[string]any{"id": artifact.ID})}
			delegated := model.WithDelegations(ctx, []model.Delegation{grant})
			if err := consumer.Download(ctx, artifact, 0, io.Discard); err != nil {
				t.Fatal("live token rejected:", err)
			}
			// Direct access IDs support resumed reads, but cannot shed the token TTL.
			conn, _, err := consumer.Peer.OpenRPC(delegated, "worker", "artifacts.open", map[string]any{"id": artifact.ID, "offset": 1})
			if err != nil {
				t.Fatal("live access ID rejected:", err)
			}
			if _, err = io.Copy(io.Discard, conn); err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			var listed []model.Delegation
			call(t, source, "worker", "access.list", map[string]any{}, &listed)
			if len(listed) != 1 || listed[0].ID != proof.Access || listed[0].Expires.Unix() != proof.Expires {
				t.Fatalf("artifact lifetime was extended: %+v", listed)
			}
			worker.delegationMu.Lock()
			accessCtx := worker.delegations[proof.Access].ctx
			worker.delegationMu.Unlock()
			select {
			case <-accessCtx.Done():
			case <-ctx.Done():
				t.Fatal("backing access did not cancel at the signed deadline")
			}
			if err := consumer.Download(ctx, artifact, 0, io.Discard); err == nil {
				t.Fatal("expired signed grant accepted")
			}
			for _, token := range []string{"", artifact.Grant} {
				if conn, _, err = consumer.Peer.OpenRPC(delegated, "worker", "artifacts.open", map[string]any{"id": artifact.ID, "grant": token}); err == nil {
					_ = conn.Close()
					t.Fatal("explicit access ID bypassed signed expiry")
				}
			}
		})
	}
}

func TestMCPDiscoveryRequiresExecutionAuthority(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, _ := cluster(t, relay, func(i int, cfg *Config) {
				if i == 1 {
					cfg.MCP = map[string]Command{"side-effect": executable(t, "encode")}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := os.WriteFile(filepath.Join(worker.Config.WorkDir, "input.bin"), []byte("marker"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, params := range []map[string]any{{}, {"server": "side-effect"}} {
				if err := source.Call(ctx, "worker", "mcp.discover", params, nil); err == nil {
					t.Fatal("worker invoked MCP discovery without execution authority")
				}
			}
			if _, err := os.Stat(filepath.Join(worker.Config.WorkDir, "output.bin")); !os.IsNotExist(err) {
				t.Fatal("unauthorized discovery started configured subprocess:", err)
			}
			var servers map[string]string
			if err := testCall(t, source, ctx, "worker", "mcp.discover", map[string]any{}, &servers); err != nil || len(servers) != 1 {
				t.Fatal("account-authorized discovery:", servers, err)
			}
		})
	}
}
