package model

import "encoding/json"

type APICall struct {
	Target string          `json:"target"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type WorkflowStep struct {
	Name   string   `json:"name"`
	Target string   `json:"target"`
	Needs  []string `json:"needs,omitempty"`
	Task   TaskSpec `json:"task"`
	Inputs []struct {
		Step     string `json:"step"`
		Artifact int    `json:"artifact"`
		Path     string `json:"path"`
	} `json:"inputs,omitempty"`
}
