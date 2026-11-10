package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// MachineQuery filters or projects an already fleet-scoped directory response.
// Its zero value preserves the full JSON array, including unknown fields.
type MachineQuery struct {
	Node    string `json:"node,omitempty"`
	Compact bool   `json:"compact,omitempty"`
}

type machineSummary struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	OS              string    `json:"os"`
	Arch            string    `json:"arch,omitempty"`
	Online          bool      `json:"online"`
	LastSeen        time.Time `json:"lastSeen"`
	Disabled        bool      `json:"disabled"`
	ControlPending  bool      `json:"controlPending"`
	RunAs           string    `json:"runAs,omitempty"`
	InstructionOnly bool      `json:"instructionOnly,omitempty"`
	HybridID        string    `json:"hybridId,omitempty"`
	HybridRole      string    `json:"hybridRole,omitempty"`
	Version         string    `json:"version,omitempty"`
}

// Machines lists directory metadata without connecting to execution workers.
func (c Admin) Machines(ctx context.Context, q MachineQuery) (json.RawMessage, error) {
	var data json.RawMessage
	if err := c.JSON(ctx, http.MethodGet, "/v1/nodes", nil, &data); err != nil {
		return nil, err
	}
	return q.project(data)
}

// Machines uses the selected client backend and applies the same CLI projection.
// Query options are local and are never sent as new peer or gateway parameters.
func (c Client) Machines(ctx context.Context, q MachineQuery) (json.RawMessage, error) {
	var data json.RawMessage
	if err := c.Call(ctx, "", "nodes.list", map[string]any{}, &data); err != nil {
		return nil, err
	}
	return q.project(data)
}

func (q MachineQuery) project(data json.RawMessage) (json.RawMessage, error) {
	if q.Node == "" && !q.Compact {
		return data, nil
	}
	var records []json.RawMessage
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	selected := make([]json.RawMessage, 0, len(records))
	summaries := make([]machineSummary, 0, len(records))
	for _, record := range records {
		var n model.Node
		if err := json.Unmarshal(record, &n); err != nil {
			return nil, err
		}
		if q.Node != "" && n.ID != q.Node && n.Name != q.Node {
			continue
		}
		selected = append(selected, record)
		arch := n.Software.Arch
		if n.System != nil && n.System.Arch != "" {
			arch = n.System.Arch
		}
		summaries = append(summaries, machineSummary{
			ID: n.ID, Name: n.Name, OS: n.OS, Arch: arch, Online: n.Online,
			LastSeen: n.LastSeen, Disabled: n.Disabled, ControlPending: n.ControlPending,
			RunAs: n.RunAs, InstructionOnly: n.InstructionOnly,
			HybridID: n.HybridID, HybridRole: n.HybridRole, Version: n.Software.Version,
		})
	}
	if q.Node != "" && len(selected) == 0 {
		return nil, fmt.Errorf("unknown machine %q", q.Node)
	}
	if q.Compact {
		return json.Marshal(summaries)
	}
	return json.Marshal(selected)
}
