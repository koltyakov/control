package client

import (
	"context"
	"errors"

	"github.com/koltyakov/control/internal/model"
)

// ConnectionTestSpec selects either this orchestrator or a named source worker.
type ConnectionTestSpec struct {
	Node string `json:"node"`
	From string `json:"from,omitempty"`
	model.ConnectionTestOptions
}

func (c Client) TestConnection(ctx context.Context, spec ConnectionTestSpec) (model.ConnectionTestResult, error) {
	var result model.ConnectionTestResult
	if spec.Node == "" {
		return result, errors.New("connection test requires a target machine")
	}
	options, err := spec.Normalize()
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, model.ConnectionTestTimeout)
	defer cancel()
	peer, err := c.backend(ctx)
	if err != nil {
		return result, err
	}
	if peer == nil {
		return result, errors.New("connection tests require account-client routing; omit --api and CONTROL_API")
	}
	if spec.From != "" {
		err = c.Call(ctx, spec.From, model.ConnectionTestMethod, model.ConnectionTestRequest{Target: spec.Node, ConnectionTestOptions: options}, &result)
		return result, err
	}
	result, err = peer.TestConnection(ctx, spec.Node, options)
	result.Source = "orchestrator"
	return result, err
}
