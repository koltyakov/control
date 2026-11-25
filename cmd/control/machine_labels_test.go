package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

func TestMachineLabelCLI(t *testing.T) {
	var mu sync.Mutex
	var patches []model.LabelPatch
	role := "user"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/auth":
			_ = json.NewEncoder(w).Encode(map[string]any{"role": role, "machineLabels": true})
		case r.URL.Path == "/v1/nodes":
			_ = json.NewEncoder(w).Encode([]model.Node{{ID: "node-id", Name: "render", Labels: map[string]string{"zone": "lab"}}})
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/fleet/nodes/node-id":
			var body struct {
				Labels model.LabelPatch `json:"labels"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			patches = append(patches, body.Labels)
			_ = json.NewEncoder(w).Encode(model.MachineLabels{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := client.Admin{URL: server.URL, Key: "account"}
	ctx := context.Background()
	if err := machineCLI(ctx, c, []string{"label", "render", "pool=a", "--reset", "gpu", "zone-", "--reset=os"}); err != nil {
		t.Fatal(err)
	}
	if err := machineCLI(ctx, c, []string{"label", "render"}); err != nil {
		t.Fatal("show labels", err)
	}
	for _, bad := range [][]string{{"label"}, {"label", "render", "pool"}, {"label", "render", "bad key=x"}, {"label", "render", "--unknown"}} {
		if err := machineCLI(ctx, c, bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	mu.Lock()
	role = "common"
	got := slices.Clone(patches)
	mu.Unlock()
	if err := machineCLI(ctx, c, []string{"label", "render", "pool=b"}); err == nil || !strings.Contains(err.Error(), "account key") {
		t.Fatal("common key labeled a machine", err)
	}
	if len(got) != 1 || !maps.Equal(got[0].Set, map[string]string{"pool": "a"}) || !slices.Equal(got[0].Remove, []string{"zone"}) || !slices.Equal(got[0].Reset, []string{"gpu", "os"}) {
		t.Fatal("sent patches", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(patches) != 1 {
		t.Fatal("a rejected command sent a change", patches)
	}
}

// Label management is fleet-owner help, never common-key help.
func TestMachineLabelHelpIsOwnerScoped(t *testing.T) {
	if strings.Contains(usage, "machines label") || !strings.Contains(fleetUsage, "machines label") {
		t.Fatal("machines label help scope")
	}
	label := completionNodeAt(t, parseCompletionHelp(completionRoleHelp["user"]), "machines label")
	if !slices.Equal(label.flags, []string{"--reset"}) {
		t.Fatal("machines label flags", label.flags)
	}
}
