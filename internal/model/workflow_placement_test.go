package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestWorkflowPlacementEncoding(t *testing.T) {
	const plan = `{"steps":[
		{"name":"fixed","target":"worker","task":{"capability":"exec.run"}},
		{"name":"one","target":{"select":{"labels":{"role":"builder"},"gpuKind":"discrete"}},"task":{"capability":"exec.run"}},
		{"name":"all","target":{"each":{"capability":"exec.run"}},"needs":["one"],"when":"always","task":{"capability":"exec.run"}},
		{"name":"local","task":{"capability":"exec.run"}}]}`
	var decoded WorkflowPlan
	if err := json.Unmarshal([]byte(plan), &decoded); err != nil {
		t.Fatal(err)
	}
	steps := decoded.Steps
	if steps[0].Target != "worker" || steps[0].Placement != nil || steps[3].Target != "" || steps[3].Placement != nil {
		t.Fatal("concrete targets changed", steps)
	}
	if p := steps[1].Placement; p == nil || p.Select == nil || p.Each != nil || p.Select.Labels["role"] != "builder" || p.Select.GPUKind != GPUDiscrete || steps[1].Target != "" {
		t.Fatal("select placement", steps[1])
	}
	if p := steps[2].Placement; p == nil || p.Each == nil || p.Each.Capability != "exec.run" || steps[2].When != WorkflowWhenAlways || steps[2].Needs[0] != "one" {
		t.Fatal("each placement", steps[2])
	}
	if !decoded.HasPlacement() {
		t.Fatal("plan placement not reported")
	}
	// Round trip keeps the selector in place of the string target.
	var again WorkflowPlan
	if err := json.Unmarshal(JSON(decoded), &again); err != nil || again.Steps[2].Placement == nil || again.Steps[2].Placement.Each.Capability != "exec.run" || again.Steps[0].Target != "worker" {
		t.Fatal("round trip", string(JSON(decoded)), err)
	}
	if !strings.Contains(string(JSON(decoded.Steps[0])), `"target":"worker"`) {
		t.Fatal("concrete target encoding changed", string(JSON(decoded.Steps[0])))
	}
	for name, target := range map[string]string{
		"misspelled constraint": `{"each":{"lables":{"role":"x"}}}`,
		"both selectors":        `{"select":{},"each":{}}`,
		"unknown mode":          `{"any":{}}`,
		"empty object":          `{}`,
		"null selector":         `{"each":null}`,
		"invalid selector":      `{"select":{"gpuKind":"tpu"}}`,
		"negative memory":       `{"each":{"minMemoryBytes":-1}}`,
		"number":                `7`,
		"array":                 `["a"]`,
	} {
		var step WorkflowStep
		if err := json.Unmarshal([]byte(`{"name":"s","target":`+target+`,"task":{}}`), &step); err == nil {
			t.Errorf("%s: accepted %s as %+v", name, target, step.Placement)
		}
	}
}

// Placement belongs to the orchestrator: a coordinator must never resolve a
// selector itself, so validation rejects a plan that still carries one.
func TestWorkflowOrderedRejectsUnresolvedPlacement(t *testing.T) {
	var plan WorkflowPlan
	if err := json.Unmarshal([]byte(`{"steps":[{"name":"a","target":{"each":{}},"task":{"capability":"exec.run"}}]}`), &plan); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Ordered(); !errors.Is(err, ErrUnresolvedPlacement) || !strings.Contains(err.Error(), "workflow step a") {
		t.Fatal("unresolved placement accepted", err)
	}
	plan.Steps[0].Placement, plan.Steps[0].Target = nil, "worker"
	if _, err := plan.Ordered(); err != nil {
		t.Fatal(err)
	}
}

// An older coordinator or client decodes target as a string. A selector must
// make that decode fail rather than leave an empty target, which a local API
// coordinator would run on itself.
func TestWorkflowPlacementFailsClosedForOlderPeers(t *testing.T) {
	type legacyStep struct {
		Name   string   `json:"name"`
		Target string   `json:"target"`
		Needs  []string `json:"needs,omitempty"`
		Task   TaskSpec `json:"task"`
	}
	type legacyPlan struct {
		Steps []legacyStep `json:"steps"`
	}
	for _, mode := range []string{"select", "each"} {
		plan := WorkflowPlan{Steps: []WorkflowStep{{Name: "s", Placement: &WorkflowPlacement{}, Task: TaskSpec{Capability: "exec.run"}}}}
		if mode == "select" {
			plan.Steps[0].Placement.Select = &NodeSelector{Capability: "exec.run"}
		} else {
			plan.Steps[0].Placement.Each = &NodeSelector{Capability: "exec.run"}
		}
		var legacy legacyPlan
		if err := json.Unmarshal(JSON(plan), &legacy); err == nil {
			t.Fatalf("%s: older peer decoded an unresolved plan with target %q", mode, legacy.Steps[0].Target)
		}
	}
}
