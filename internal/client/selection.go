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
