package model

import (
	"slices"
	"strings"
	"testing"
)

func TestNormalizeTaskTags(t *testing.T) {
	if tags, err := NormalizeTaskTags(nil); err != nil || tags != nil {
		t.Fatal("no tags must normalize to nil", tags, err)
	}
	if tags, err := NormalizeTaskTags([]string{}); err != nil || tags != nil {
		t.Fatal("an empty list must normalize to nil so the digest is unchanged", tags, err)
	}
	tags, err := NormalizeTaskTags([]string{"b", "A.1", "b", "a_2", "z-9"})
	if err != nil || !slices.Equal(tags, []string{"A.1", "a_2", "b", "z-9"}) {
		t.Fatal(tags, err)
	}
	full := []string{"1", "2", "3", "4", "5", "6", "7", strings.Repeat("x", MaxTaskTagLength)}
	if _, err := NormalizeTaskTags(full); err != nil {
		t.Fatal("bounds rejected", err)
	}
	for _, bad := range [][]string{append(full, "9"), {""}, {"-lead"}, {"_lead"}, {"a b"}, {"a/b"}, {"a:b"}, {strings.Repeat("x", MaxTaskTagLength+1)}, {"é"}} {
		if _, err := NormalizeTaskTags(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}

func TestTaskTagDestinations(t *testing.T) {
	tagged := TaskSpec{ID: "a", Capability: "exec.run", Tags: []string{"batch"}}
	plain := TaskSpec{ID: "b", Capability: "exec.run"}
	plan := WorkflowPlan{Steps: []WorkflowStep{{Name: "one", Target: "w1", Task: tagged}, {Name: "two", Target: "w2", Task: plain}, {Name: "three", Target: "w1", Task: tagged}, {Name: "four", Target: "w3", Task: tagged}}}
	for _, tc := range []struct {
		name, target, method string
		params               any
		want                 []string
	}{
		{"untagged task", "node", "tasks.start", plain, nil},
		{"tagged task", "node", "tasks.start", tagged, []string{"node"}},
		{"unfiltered page", "node", "tasks.page", map[string]any{"limit": 5}, nil},
		{"filtered page", "node", "tasks.page", map[string]any{"tag": "batch"}, []string{"node"}},
		{"filtered list", "", "tasks.list", map[string]any{"tag": "batch"}, []string{""}},
		{"cancel by tag fails explicitly on older nodes", "node", CancelTaggedTasks, map[string]any{"tag": "batch"}, nil},
		{"forwarded task", "coordinator", "peers.call", map[string]any{"target": "worker", "method": "tasks.start", "params": tagged}, []string{"worker"}},
		{"forwarded through a task", "coordinator", "tasks.start", TaskSpec{ID: "p", Capability: "peers.call", Tags: []string{"outer"}, Args: JSON(map[string]any{"target": "worker", "method": "tasks.start", "params": tagged})}, []string{"coordinator", "worker"}},
		{"workflow steps", "coordinator", "workflow.run", plan, []string{"w1", "w3"}},
		{"workflow task", "coordinator", "tasks.start", TaskSpec{ID: "p", Capability: "workflow.run", Args: JSON(plan)}, []string{"w1", "w3"}},
	} {
		if got := TaskTagDestinations(tc.target, tc.method, JSON(tc.params)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: destinations %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestWorkflowValidatesStepTags(t *testing.T) {
	plan := WorkflowPlan{Steps: []WorkflowStep{{Name: "one", Target: "w", Task: TaskSpec{Capability: "exec.run", Tags: []string{"not valid"}}}}}
	if _, err := plan.Ordered(); err == nil || !strings.Contains(err.Error(), "workflow step one") {
		t.Fatal("invalid step tags accepted", err)
	}
}
