package model

import (
	"errors"
	"fmt"
)

const MaxWorkflowParallel = 16

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
