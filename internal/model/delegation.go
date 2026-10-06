package model

import (
	"bytes"
	"context"
	"encoding/json"
	"time"
)

// Delegation permits one instruction, or the lifecycle of one named task.
// It is bound to its destination, requesting worker and orchestrator owner.
type Delegation struct {
	ID         string           `json:"id"`
	Target     string           `json:"target"`
	Alias      string           `json:"alias,omitempty"`
	Subject    string           `json:"subject"`
	Owner      string           `json:"owner"`
	Method     string           `json:"method"`
	Params     json.RawMessage  `json:"params"`
	InputsFrom []DelegatedInput `json:"inputsFrom,omitempty"`
	DeliverTo  []string         `json:"deliverTo,omitempty"`
	Expires    time.Time        `json:"expires"`
}

type DelegatedInput struct {
	Node     string `json:"node"`
	Path     string `json:"path"`
	TaskID   string `json:"taskId"`
	Artifact int    `json:"artifact"`
}

// SameJSON compares decoded JSON, preserving numeric precision.
func SameJSON(a, b json.RawMessage) bool {
	normalize := func(raw json.RawMessage) []byte {
		if !json.Valid(raw) {
			return nil
		}
		var v any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if d.Decode(&v) != nil {
			return nil
		}
		return JSON(v)
	}
	x, y := normalize(a), normalize(b)
	return x != nil && y != nil && bytes.Equal(x, y)
}

func (d Delegation) TaskID() string {
	if d.Method != "tasks.start" {
		return ""
	}
	var spec TaskSpec
	_ = json.Unmarshal(d.Params, &spec)
	return spec.ID
}

// Matches checks instruction arguments, not just a method or capability name.
// Artifact ownership and permitted output recipients are also checked by nodes.
func (d Delegation) Matches(method string, params json.RawMessage) bool {
	if d.Method == "artifacts.open" && method == d.Method {
		var expected, actual struct {
			ID string `json:"id"`
		}
		return json.Unmarshal(d.Params, &expected) == nil && json.Unmarshal(params, &actual) == nil && expected.ID != "" && expected.ID == actual.ID
	}
	if d.Method == "artifacts.pull" && method == d.Method {
		var expected, actual struct {
			Artifact Artifact `json:"artifact"`
		}
		if json.Unmarshal(d.Params, &expected) != nil || json.Unmarshal(params, &actual) != nil {
			return false
		}
		actual.Artifact.Grant = ""
		expected.Artifact.Grant = ""
		return SameJSON(JSON(expected), JSON(actual))
	}
	if d.Method != "tasks.start" {
		return method == d.Method && SameJSON(d.Params, params)
	}
	var spec TaskSpec
	if json.Unmarshal(d.Params, &spec) != nil || spec.ID == "" {
		return false
	}
	switch method {
	case "tasks.start":
		var actual TaskSpec
		if json.Unmarshal(params, &actual) != nil || len(actual.Inputs) != len(spec.Inputs)+len(d.InputsFrom) {
			return false
		}
		for i, input := range d.InputsFrom {
			v := actual.Inputs[len(spec.Inputs)+i]
			if v.Path != input.Path || v.Artifact.Node != input.Node || v.Artifact.Grant == "" {
				return false
			}
		}
		actual.Inputs = actual.Inputs[:len(spec.Inputs)]
		return SameJSON(JSON(spec), JSON(actual))
	case "tasks.get", "tasks.cancel", "tasks.logs":
		var q struct {
			ID string `json:"id"`
		}
		return json.Unmarshal(params, &q) == nil && q.ID == spec.ID
	case "artifacts.grant":
		var q struct {
			Target string `json:"target"`
		}
		if json.Unmarshal(params, &q) != nil {
			return false
		}
		for _, target := range d.DeliverTo {
			if target == q.Target {
				return true
			}
		}
	}
	return false
}

type delegationContextKey struct{}

func WithDelegations(ctx context.Context, grants []Delegation) context.Context {
	return context.WithValue(ctx, delegationContextKey{}, grants)
}

func Delegations(ctx context.Context) []Delegation {
	grants, _ := ctx.Value(delegationContextKey{}).([]Delegation)
	return grants
}
