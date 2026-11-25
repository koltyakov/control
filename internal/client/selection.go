package client

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/koltyakov/control/internal/model"
)

// selectNode validates nodes.select before routing it. A local node API or an
// explicitly targeted selector may run older software that ignores GPU fields,
// so a GPU-constrained answer must confirm that the selector applied them.
func (c Client) selectNode(ctx context.Context, target string, params, result any) error {
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	query, err := model.DecodeNodeSelector(data)
	if err != nil {
		return err
	}
	var mode struct {
		All bool `json:"all"`
	}
	if json.Unmarshal(data, &mode) == nil && mode.All {
		matches, err := c.selectAll(ctx, target, query, params)
		if err != nil || result == nil {
			return err
		}
		return json.Unmarshal(model.JSON(matches), result)
	}
	if !query.GPUConstrained() {
		return c.call(ctx, target, "nodes.select", params, result)
	}
	var raw json.RawMessage
	if err := c.call(ctx, target, "nodes.select", params, &raw); err != nil {
		return err
	}
	var selection model.NodeSelection
	if err := json.Unmarshal(raw, &selection); err != nil {
		return err
	}
	if !selection.GPUConstraints {
		return errors.New("the selecting node does not support GPU constraints; upgrade it before using gpuKind, gpuVendor, minGpuMemoryBytes, or minGpuFreeMemoryBytes")
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(raw, result)
}

var errSelectAllUnsupported = errors.New("the selecting node does not support nodes.select with all: true; upgrade it before fanning out workflow steps")

// selectAll asks for every eligible machine. An older selector ignores all
// and answers one object, which is rejected instead of fanning out to one
// machine; GPU-constrained matches must each confirm the constraints.
func (c Client) selectAll(ctx context.Context, target string, query model.NodeSelector, params any) ([]model.NodeSelection, error) {
	var raw json.RawMessage
	if err := c.call(ctx, target, "nodes.select", params, &raw); err != nil {
		return nil, err
	}
	var matches []model.NodeSelection
	if err := json.Unmarshal(raw, &matches); err != nil {
		return nil, errSelectAllUnsupported
	}
	for _, match := range matches {
		if match.ID == "" {
			return nil, errors.New("the selecting node returned a machine without an ID")
		}
		if query.GPUConstrained() && !match.GPUConstraints {
			return nil, errors.New("the selecting node does not support GPU constraints; upgrade it before using gpuKind, gpuVendor, minGpuMemoryBytes, or minGpuFreeMemoryBytes")
		}
	}
	return matches, nil
}
