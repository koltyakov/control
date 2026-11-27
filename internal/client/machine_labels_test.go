package client

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// An older gateway answers a labels-only PATCH with 400, but a gateway that
// ignored the field and acknowledged would silently drop the change. The client
// therefore refuses before sending unless the feature is advertised.
func TestLabelMachineRequiresGatewayAdvertisement(t *testing.T) {
	var advertised atomic.Bool
	var patches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/auth":
			auth := map[string]any{"role": "user", "userId": "u", "gatewayProxy": true}
			if advertised.Load() {
				auth["machineLabels"] = true
			}
			_ = json.NewEncoder(w).Encode(auth)
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/fleet/nodes/node-id":
			patches.Add(1)
			var body struct {
				Labels model.LabelPatch `json:"labels"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Labels.Set["pool"] != "render" {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(model.MachineLabels{Labels: map[string]string{"pool": "render"}, OwnerLabels: map[string]string{"pool": "render"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := Admin{URL: server.URL, Key: "account"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	patch := model.LabelPatch{Set: map[string]string{"pool": "render"}}
	if _, err := c.LabelMachine(ctx, "node-id", patch); !errors.Is(err, ErrMachineLabelsUnsupported) || patches.Load() != 0 {
		t.Fatal("label change sent to a gateway without the feature", err, patches.Load())
	}
	if _, err := c.LabelMachine(ctx, "node-id", model.LabelPatch{Set: map[string]string{"bad key": "x"}}); err == nil || patches.Load() != 0 {
		t.Fatal("invalid change sent", err)
	}
	advertised.Store(true)
	view, err := c.LabelMachine(ctx, "node-id", patch)
	if err != nil || patches.Load() != 1 || view.Labels["pool"] != "render" {
		t.Fatal("advertised label change", view, err)
	}
}

func TestParseLabelChanges(t *testing.T) {
	patch, err := ParseLabelChanges([]string{"pool=render", "zone-", "url=https://x:1/a@b+c"}, []string{"gpu"})
	if err != nil || !maps.Equal(patch.Set, map[string]string{"pool": "render", "url": "https://x:1/a@b+c"}) || !slices.Equal(patch.Remove, []string{"zone"}) || !slices.Equal(patch.Reset, []string{"gpu"}) {
		t.Fatal(patch, err)
	}
	for _, bad := range [][]string{{"pool"}, {"pool=a", "pool=b"}, {"pool=a", "pool-"}, {"=x"}, {"pool="}, {"-"}, {"a b=c"}} {
		if _, err := ParseLabelChanges(bad, nil); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	if _, err := ParseLabelChanges(nil, []string{"pool", "pool"}); err == nil {
		t.Fatal("accepted duplicate reset")
	}
	if _, err := ParseLabelChanges(nil, nil); err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatal("accepted an empty change", err)
	}
}

// Owner labels decide where fan-out runs: account routing resolves workflow
// targets from the effective directory labels, and a local node's nodes.select
// sees the same overlay. Removing a node-reported label stops it matching.
func TestOwnerLabelsDriveSelectionAndFanOut(t *testing.T) {
	t.Parallel()
	ctx, c, workers, admin := delegationClusterWithTimeout(t, true, 60*time.Second)
	if _, err := admin.LabelMachine(ctx, workers[2].Identity.ID, model.LabelPatch{Set: map[string]string{"pool": "render"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.LabelMachine(ctx, workers[1].Identity.ID, model.LabelPatch{Set: map[string]string{"pool": "render"}}); err != nil {
		t.Fatal(err)
	}
	const plan = `{"steps":[{"name":"probe","target":{"each":{"labels":{"pool":"render"},"capability":"test.owner"}},"task":{"capability":"test.owner","args":{}}}]}`
	r, err := c.ResolveTask(ctx, "first", workflowSpec("label-fanout", plan))
	if err != nil {
		t.Fatal(err)
	}
	steps := r.Steps
	if len(steps) != 2 || steps[0].Name != "probe@second" || steps[1].Name != "probe@third" || steps[1].Target != workers[2].Identity.ID {
		t.Fatal("fan-out by owner label", steps)
	}
	// Removing the label from one machine narrows the next resolution.
	if _, err := admin.LabelMachine(ctx, workers[1].Identity.ID, model.LabelPatch{Remove: []string{"pool"}}); err != nil {
		t.Fatal(err)
	}
	if r, err = c.ResolveTask(ctx, "first", workflowSpec("label-fanout-2", plan)); err != nil || len(r.Steps) != 1 || r.Steps[0].Target != workers[2].Identity.ID {
		t.Fatal("fan-out after removal", r.Steps, err)
	}
	// The local-node route reads the same gateway directory.
	api := httptest.NewServer(workers[0].Handler())
	defer api.Close()
	local := Client{URL: api.URL, Token: "delegation-worker-key-1234567890"}
	var selected model.NodeSelection
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = local.Call(ctx, "", "nodes.select", map[string]any{"labels": map[string]string{"pool": "render"}}, &selected)
		if err == nil && selected.ID == workers[2].Identity.ID && selected.OwnerLabels["pool"] == "render" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("local nodes.select ignored owner labels", selected.ID, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
