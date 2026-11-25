package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

const MaxWorkflowParallel = 16

// Step conditions. A success step starts only while no step has failed and
// after every dependency succeeded. An always step is a pre-planned cleanup
// step: it starts once every dependency is settled, whatever the outcome,
// unless the workflow itself was cancelled, timed out or lost its authority.
const (
	WorkflowWhenSuccess = "success"
	WorkflowWhenAlways  = "always"
)

// WorkflowWhenSchema is advertised in the workflow.run step schema. Clients
// check it before sending always steps, because older coordinators ignore the
// field and would run cleanup as an ordinary step.
func WorkflowWhenSchema() map[string]any {
	return map[string]any{"type": "string", "enum": []string{WorkflowWhenSuccess, WorkflowWhenAlways}, "default": WorkflowWhenSuccess}
}

// Always reports whether the step is a pre-planned cleanup step.
func (s WorkflowStep) Always() bool { return s.When == WorkflowWhenAlways }

// Dependencies lists needs and artifact input steps; both order execution.
func (s WorkflowStep) Dependencies() []string {
	deps := slices.Clone(s.Needs)
	for _, input := range s.Inputs {
		deps = append(deps, input.Step)
	}
	return deps
}

// HasAlways reports whether any step is a cleanup step.
func (p WorkflowPlan) HasAlways() bool {
	return slices.ContainsFunc(p.Steps, WorkflowStep.Always)
}

// WorkflowAlwaysSupported reports whether advertised capabilities include a
// workflow.run coordinator that understands always steps.
func WorkflowAlwaysSupported(capabilities []Capability) bool {
	for _, capability := range capabilities {
		if capability.Name != "workflow.run" {
			continue
		}
		var schema struct {
			Properties struct {
				Steps struct {
					Items struct {
						Properties struct {
							When struct {
								Enum []string `json:"enum"`
							} `json:"when"`
						} `json:"properties"`
					} `json:"items"`
				} `json:"steps"`
			} `json:"properties"`
		}
		if json.Unmarshal(capability.InputSchema, &schema) == nil && slices.Contains(schema.Properties.Steps.Items.Properties.When.Enum, WorkflowWhenAlways) {
			return true
		}
	}
	return false
}

type WorkflowPlan struct {
	Steps       []WorkflowStep `json:"steps"`
	MaxParallel int            `json:"maxParallel,omitempty"`
}

func (s WorkflowStep) Ready(completed map[string]bool) bool {
	for _, dep := range s.Needs {
		if !completed[dep] {
			return false
		}
	}
	for _, input := range s.Inputs {
		if !completed[input.Step] {
			return false
		}
	}
	return true
}

// Ordered validates the whole graph before issuing grants or starting children.
func (p WorkflowPlan) Ordered() ([]WorkflowStep, error) {
	if len(p.Steps) == 0 || len(p.Steps) > 100 {
		return nil, errors.New("workflow requires 1..100 steps")
	}
	if p.MaxParallel < 0 || p.MaxParallel > MaxWorkflowParallel {
		return nil, fmt.Errorf("maxParallel must be 1..%d", MaxWorkflowParallel)
	}
	known := map[string]bool{}
	for _, step := range p.Steps {
		if step.Name == "" || known[step.Name] {
			return nil, errors.New("workflow step names must be nonempty and unique")
		}
		known[step.Name] = true
	}
	for _, step := range p.Steps {
		if step.Task.Capability == "workflow.run" {
			return nil, errors.New("nested workflows are not supported")
		}
		if step.Placement != nil {
			return nil, fmt.Errorf("workflow step %s: %w", step.Name, ErrUnresolvedPlacement)
		}
		if step.When != "" && step.When != WorkflowWhenSuccess && step.When != WorkflowWhenAlways {
			return nil, fmt.Errorf("workflow step %s: when must be %q or %q", step.Name, WorkflowWhenSuccess, WorkflowWhenAlways)
		}
		if _, err := NormalizeTaskTags(step.Task.Tags); err != nil {
			return nil, fmt.Errorf("workflow step %s: %w", step.Name, err)
		}
		for _, dep := range step.Needs {
			if !known[dep] {
				return nil, fmt.Errorf("unknown dependency %s", dep)
			}
		}
		for _, input := range step.Inputs {
			if !known[input.Step] || input.Artifact < 0 {
				return nil, errors.New("invalid workflow input")
			}
		}
	}
	var ordered []WorkflowStep
	sorted := map[string]bool{}
	for len(ordered) < len(p.Steps) {
		before := len(ordered)
		for _, step := range p.Steps {
			if !sorted[step.Name] && step.Ready(sorted) {
				ordered = append(ordered, step)
				sorted[step.Name] = true
			}
		}
		if len(ordered) == before {
			return nil, errors.New("workflow dependency cycle")
		}
	}
	return ordered, nil
}
