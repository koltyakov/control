package transport

import (
	"slices"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// Fan-out uses the same exclusions as nodes.select: offline, disabled,
// policy-pending and instruction-only machines never become candidates, and
// known leases and update reservations are skipped.
func TestSelectAllExclusionsMatchSelect(t *testing.T) {
	capability := []model.Capability{{Name: "exec.run"}}
	directory := []model.Node{
		{ID: "id-c", Name: "c", Online: true, Capabilities: capability, Labels: map[string]string{"env": "prod"}},
		{ID: "id-a", Name: "a", Online: true, Capabilities: capability, Labels: map[string]string{"env": "prod"}},
		{ID: "id-offline", Name: "offline", Capabilities: capability, Labels: map[string]string{"env": "prod"}},
		{ID: "id-disabled", Name: "disabled", Online: true, Disabled: true, Capabilities: capability, Labels: map[string]string{"env": "prod"}},
		{ID: "id-pending", Name: "pending", Online: true, ControlPending: true, Capabilities: capability, Labels: map[string]string{"env": "prod"}},
		{ID: "id-system", Name: "a-system", Online: true, InstructionOnly: true, Capabilities: capability, Labels: map[string]string{"env": "prod"}},
		{ID: "id-leased", Name: "leased", Online: true, Capabilities: capability, Labels: map[string]string{"env": "prod"}},
		{ID: "id-update", Name: "update", Online: true, Capabilities: capability, Labels: map[string]string{"env": "prod"}},
		{ID: "id-b", Name: "b", Online: true, Capabilities: capability, Labels: map[string]string{"env": "prod"}},
		{ID: "id-dev", Name: "dev", Online: true, Capabilities: capability, Labels: map[string]string{"env": "dev"}},
		{ID: "id-nocap", Name: "nocap", Online: true, Labels: map[string]string{"env": "prod"}},
	}
	query := model.NodeSelector{Labels: map[string]string{"env": "prod"}, Capability: "exec.run"}
	now := time.Now()
	health := map[string]model.NodeActivitySnapshot{
		"id-leased": {ObservedAt: now, Leased: true},
		"id-update": {Status: "update"},
	}
	candidates := selectorCandidates(directory, query)
	eligible, err := eligiblePlacements(candidates, health, query, now)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, n := range eligible {
		ids = append(ids, n.ID)
	}
	if !slices.Equal(ids, []string{"id-a", "id-b", "id-c"}) {
		t.Fatal("fan-out candidates", ids)
	}
	// Every machine fan-out returns is one Select could choose, and no other.
	seen := map[string]bool{}
	for range 200 {
		chosen, err := resourcePlacement(slices.Clone(candidates), health, query, now)
		if err != nil {
			t.Fatal(err)
		}
		seen[chosen.ID] = true
	}
	if len(seen) != 3 || !seen["id-a"] || !seen["id-b"] || !seen["id-c"] {
		t.Fatal("select and fan-out disagree", seen)
	}
	// Resource constraints without fresh samples match nothing, as for Select.
	constrained := query
	constrained.MinMemoryBytes = 1
	if _, err := eligiblePlacements(candidates, health, constrained, now); err == nil {
		t.Fatal("constraint matched without fresh resources")
	}
	if got := selectorCandidates(directory, model.NodeSelector{Labels: map[string]string{"env": "none"}}); len(got) != 0 {
		t.Fatal("unmatched labels", got)
	}
}
