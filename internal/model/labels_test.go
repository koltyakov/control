package model

import (
	"maps"
	"slices"
	"testing"
)

func TestLabelOverlaySemantics(t *testing.T) {
	reported := map[string]string{"zone": "lab", "gpu": "none"}
	o, err := LabelOverlay{}.Patch(LabelPatch{Set: map[string]string{"gpu": "a100", "pool": "render"}, Remove: []string{"zone", "later"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Apply(reported); !maps.Equal(got, map[string]string{"gpu": "a100", "pool": "render"}) {
		t.Fatal("apply", got)
	}
	if !maps.Equal(reported, map[string]string{"zone": "lab", "gpu": "none"}) {
		t.Fatal("apply modified the node's labels", reported)
	}
	// Setting clears a removal; removing clears a value; reset clears both.
	o, err = o.Patch(LabelPatch{Set: map[string]string{"zone": "edge"}, Remove: []string{"pool"}, Reset: []string{"gpu", "later"}})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(o.Set, map[string]string{"zone": "edge"}) || !slices.Equal(o.Remove, []string{"pool"}) {
		t.Fatal("patch", o)
	}
	if got := o.Apply(reported); !maps.Equal(got, map[string]string{"zone": "edge", "gpu": "none"}) {
		t.Fatal("apply after patch", got)
	}
	if got := (LabelOverlay{Remove: []string{"zone", "gpu"}}).Apply(reported); got != nil {
		t.Fatal("removing every label", got)
	}
	empty, err := o.Patch(LabelPatch{Reset: []string{"zone", "pool"}})
	if err != nil || !empty.Empty() || empty.Set != nil || empty.Remove != nil {
		t.Fatal("reset to empty", empty, err)
	}
	view := LabelView(Node{Labels: reported})
	if !maps.Equal(view.NodeLabels, reported) || len(view.OwnerLabels) != 0 || view.RemovedLabels == nil {
		t.Fatal("view without overlay", view)
	}
}
