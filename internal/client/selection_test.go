package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
)

// A local node API running an older selector ignores GPU fields. Its answer
// must be rejected rather than presented as a GPU-satisfying machine.
func TestSelectRejectsSelectorsIgnoringGPUConstraints(t *testing.T) {
	var confirm atomic.Bool
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q node.APICall
		_ = json.NewDecoder(r.Body).Decode(&q)
		if q.Method != "nodes.select" {
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		result := map[string]any{"id": "worker-id", "name": "worker"}
		if confirm.Load() {
			result["gpuConstraints"] = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result})
	}))
	defer api.Close()
	c := Client{URL: api.URL}
	ctx := context.Background()
	var selected model.Node
	if err := c.Call(ctx, "", "nodes.select", map[string]any{"labels": map[string]string{}}, &selected); err != nil || selected.ID != "worker-id" {
		t.Fatal("unconstrained selection through an older selector", selected, err)
	}
	selected = model.Node{}
	err := c.Call(ctx, "", "nodes.select", map[string]any{"gpuKind": model.GPUDiscrete, "minGpuMemoryBytes": 1 << 30}, &selected)
	if err == nil || !strings.Contains(err.Error(), "does not support GPU constraints") || selected.ID != "" {
		t.Fatal("older selector's GPU answer accepted", selected, err)
	}
	confirm.Store(true)
	var raw json.RawMessage
	if err := c.Call(ctx, "", "nodes.select", json.RawMessage(`{"gpuVendor":"NVIDIA"}`), &raw); err != nil || !strings.Contains(string(raw), `"gpuConstraints":true`) {
		t.Fatal("confirmed GPU selection rejected", string(raw), err)
	}
	before := calls.Load()
	for _, params := range []any{map[string]any{"gpuKind": "tpu"}, map[string]any{"minGpuFreeMemoryBytes": -1}} {
		if err := c.Call(ctx, "", "nodes.select", params, nil); err == nil {
			t.Fatal("invalid selector accepted", params)
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid selector reached the node")
	}
}
