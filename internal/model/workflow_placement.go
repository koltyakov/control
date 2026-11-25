package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// ErrUnresolvedPlacement rejects a plan whose step still carries a target
// selector. Placement is the orchestrator's decision: the submitting client
// resolves selectors before preparing grants, so coordinators and workers only
// ever see concrete targets and never choose machines themselves.
var ErrUnresolvedPlacement = errors.New("target selector was not resolved; the submitting client must resolve select and each targets into concrete machines before the coordinator receives the plan")

// WorkflowPlacement is an unresolved step target with exactly one selector,
// using the nodes.select fields. Select resolves to one machine, as
// nodes.select would choose; Each expands the step into one step per
// eligible machine.
//
// It is encoded in place of the string target, as {"select": {...}} or
// {"each": {...}}, so an older coordinator or client fails to decode the plan
// instead of reading an empty target, which a local API coordinator would run
// on itself.
type WorkflowPlacement struct {
	Select *NodeSelector `json:"select,omitempty"`
	Each   *NodeSelector `json:"each,omitempty"`
}

// Selector returns the placement's selector.
func (p WorkflowPlacement) Selector() NodeSelector {
	if p.Each != nil {
		return *p.Each
	}
	if p.Select != nil {
		return *p.Select
	}
	return NodeSelector{}
}

// HasPlacement reports whether any step still carries a target selector.
func (p WorkflowPlan) HasPlacement() bool {
	return slices.ContainsFunc(p.Steps, func(s WorkflowStep) bool { return s.Placement != nil })
}

// UnmarshalJSON accepts a string target, or an object holding exactly one of
// select and each. Selectors reject unknown fields, so a misspelled
// constraint cannot widen a fan-out to every machine.
func (s *WorkflowStep) UnmarshalJSON(data []byte) error {
	type plain WorkflowStep
	var raw struct {
		plain
		Target json.RawMessage `json:"target"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	step := WorkflowStep(raw.plain)
	step.Target, step.Placement = "", nil
	target := bytes.TrimSpace(raw.Target)
	switch {
	case len(target) == 0 || bytes.Equal(target, []byte("null")):
	case target[0] == '"':
		if err := json.Unmarshal(target, &step.Target); err != nil {
			return err
		}
	case target[0] == '{':
		placement, err := decodeWorkflowPlacement(target)
		if err != nil {
			return fmt.Errorf("workflow step %s: %w", step.Name, err)
		}
		step.Placement = &placement
	default:
		return fmt.Errorf(`workflow step %s: target must be a machine name or ID, {"select": selector} or {"each": selector}`, step.Name)
	}
	*s = step
	return nil
}

// MarshalJSON writes an unresolved placement in place of the string target.
func (s WorkflowStep) MarshalJSON() ([]byte, error) {
	type plain WorkflowStep
	if s.Placement == nil {
		return json.Marshal(plain(s))
	}
	return json.Marshal(struct {
		plain
		Target *WorkflowPlacement `json:"target"`
	}{plain(s), s.Placement})
}

func decodeWorkflowPlacement(data []byte) (WorkflowPlacement, error) {
	var p WorkflowPlacement
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return p, err
	}
	if len(fields) != 1 {
		return p, errors.New(`target object must hold exactly one of "select" or "each"`)
	}
	for key, value := range fields {
		if key != "select" && key != "each" {
			return p, fmt.Errorf(`unknown target selector %q; use "select" or "each"`, key)
		}
		if value = bytes.TrimSpace(value); len(value) == 0 || value[0] != '{' {
			return p, fmt.Errorf("target %s must be a nodes.select object", key)
		}
		q, err := decodeNodeSelector(value, true)
		if err != nil {
			return p, fmt.Errorf("target %s: %w", key, err)
		}
		if key == "each" {
			p.Each = &q
		} else {
			p.Select = &q
		}
	}
	return p, nil
}
