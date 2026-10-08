//go:build compose

package compose_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

func TestManagedUpdateDefersWorkAndRestartsWholePool(t *testing.T) {
	ctx, c := environment(t)
	before := nodes(t, ctx, c)
	dir := t.TempDir()
	version := "compose-update-" + time.Now().UTC().Format("150405")
	cmd := exec.CommandContext(ctx, "go", "run", "./cmd/control-bundle", "--out", dir, "--version", version, "--repository", "compose/fixture", "--platforms", runtime.GOOS+"/"+runtime.GOARCH)
	cmd.Dir = "/workspace"
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build update: %v\n%s", err, output)
	}
	admin := client.Admin{URL: os.Getenv("CONTROL_GATEWAY"), Key: os.Getenv("CONTROL_SUPERUSER_KEY")}
	var keys struct {
		Token string `json:"token"`
	}
	if err := admin.JSON(ctx, "POST", "/v1/admin/keys", map[string]string{"name": "compose-common"}, &keys); err != nil {
		t.Fatal(err)
	}
	common := client.Admin{URL: admin.URL, Key: keys.Token}
	if _, err := common.Push(ctx, dir); err == nil {
		t.Fatal("common key published executable update")
	}
	cmd = exec.CommandContext(ctx, "control", "--token", keys.Token, "help")
	if output, err := cmd.CombinedOutput(); err != nil || strings.Contains(string(output), "update push") {
		t.Fatalf("common help exposed administration: %v %s", err, output)
	}
	// Use an explicit lease to make the no-work reservation deterministic, then
	// leave a real accepted task running while the rollout stages its binaries.
	var lease model.Lease
	call(t, ctx, c, "worker", "leases.acquire", map[string]any{"ttlSeconds": 60}, &lease)
	var task model.Task
	call(t, ctx, c, "worker", "tasks.start", model.TaskSpec{ID: "update-held-task", Capability: "exec.run", LeaseID: lease.ID, Args: model.JSON(map[string]any{"command": "sleep", "args": []string{"60"}})}, &task)
	defer func() {
		_ = c.Call(context.Background(), "worker", "tasks.cancel", map[string]string{"id": task.ID}, nil)
	}()
	deployment, err := admin.Push(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	type rollout struct {
		Deployment *update.Deployment       `json:"deployment"`
		Gateway    buildinfo.Info           `json:"gateway"`
		Nodes      map[string]update.Status `json:"nodes"`
	}
	var status rollout
	waitUpdate(t, ctx, func() bool {
		if admin.JSON(ctx, "GET", "/v1/admin/updates", nil, &status) != nil || status.Deployment == nil {
			return false
		}
		return status.Deployment.ID == deployment.ID && status.Deployment.Phase == "waiting-idle"
	})
	if status.Gateway.Version == version {
		t.Fatal("gateway updated while work was active")
	}
	for _, s := range status.Nodes {
		if s.Software.Version == version {
			t.Fatal("node updated while work was active")
		}
	}
	call(t, ctx, c, "worker", "tasks.cancel", map[string]string{"id": task.ID}, nil)
	finished, err := c.Wait(ctx, "worker", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != "cancelled" {
		t.Fatalf("task interrupted by update: %s", finished.State)
	}
	call(t, ctx, c, "worker", "leases.release", map[string]string{"id": lease.ID}, nil)
	waitUpdate(t, ctx, func() bool {
		if admin.JSON(ctx, "GET", "/v1/admin/updates", nil, &status) != nil || status.Deployment == nil {
			return false
		}
		if status.Deployment.Phase != "complete" || status.Gateway.Version != version {
			return false
		}
		for _, n := range before {
			s, ok := status.Nodes[n.ID]
			if !ok || s.Software.Version != version || s.Paused {
				return false
			}
		}
		return true
	})
	for name, n := range nodes(t, ctx, c) {
		if n.ID != before[name].ID || n.Software.Version != version {
			t.Fatalf("identity/version after rollout: %+v", n)
		}
	}
	// Rollout completion describes the fleet, not this client's old TLS/yamux
	// sessions or gateway re-registration. Resume as a new CLI invocation using
	// the same saved owner instead of sending work over a possibly stale stream.
	previousSession, err := c.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	resumed := newTestClient(ctx)
	t.Cleanup(func() { _ = resumed.Close() })
	resumedSession, err := resumed.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resumedSession.OwnerID != previousSession.OwnerID || resumedSession.TransportID == previousSession.TransportID {
		t.Fatal("post-rollout client did not retain its owner with a fresh transport", previousSession, resumedSession)
	}
	// Send execution only once. An uncertain side effect must never be retried.
	call(t, ctx, resumed, "worker", "exec.run", map[string]any{"command": "true"}, nil)
	// Keys and task records must survive the gateway/node process replacements.
	if err = common.JSON(ctx, "GET", "/v1/nodes", nil, nil); err != nil {
		t.Fatal("issued key lost across update", err)
	}
	call(t, ctx, resumed, "worker", "tasks.get", map[string]string{"id": task.ID}, &finished)
	if finished.State != "cancelled" {
		t.Fatal("task record changed across update")
	}
	b, err := os.ReadFile(filepath.Join(dir, "control-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest update.Manifest
	if err = json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	if status.Gateway.SHA256 != manifest.Assets[0].SHA256 {
		t.Fatal("gateway binary hash mismatch")
	}
	if built, err := time.Parse(time.RFC3339Nano, status.Gateway.BuildTime); err != nil || !built.Equal(manifest.CreatedAt) {
		t.Fatalf("gateway build timestamp does not match bundle: %q, %v", status.Gateway.BuildTime, err)
	}
	if status.Gateway.ReleaseRepo != "compose/fixture" {
		t.Fatalf("embedded repository missing: %q", status.Gateway.ReleaseRepo)
	}
	t.Log("common key denied; active task deferred rollout; gateway and three nodes restarted; a fresh client retained its owner and recovered task state")
}

func waitUpdate(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("update condition timed out", ctx.Err())
		case <-ticker.C:
		}
	}
}
