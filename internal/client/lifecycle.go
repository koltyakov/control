package client

import (
	"context"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

// InspectMachineState reads identity policy without acknowledging it or
// advertising lifecycle support on behalf of a running node.
func (c Admin) InspectMachineState(ctx context.Context, id *identity.Identity) (model.MachineState, bool, error) {
	return transport.ReadMachineState(ctx, c.URL, id, 0, true)
}
