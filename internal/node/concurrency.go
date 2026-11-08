package node

import "context"

// providerSlots is shared by durable tasks and synchronous calls. Only known
// built-ins use the operations pool; arbitrary commands and MCP tools retain
// the execution bound because their resource use cannot be inferred safely.
func (n *Node) providerSlots(capability string) (chan struct{}, string) {
	switch capability {
	case "workflow.run", "peers.call":
		// Coordination must not hold a slot needed by its local child work.
		return nil, ""
	case "files.list", "files.read", "files.write", "http.request", "network.wake":
		return n.operationSlots, "waiting for operation slot"
	case "rpa.run":
		if n.Config.RPA != nil {
			return n.operationSlots, "waiting for operation slot"
		}
	case "desktop.run":
		if n.Config.Desktop != nil {
			return n.operationSlots, "waiting for operation slot"
		}
	}
	return n.slots, "waiting for worker slot"
}

func acquireProviderSlot(ctx context.Context, slots chan struct{}) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if slots == nil {
		return func() {}, nil
	}
	select {
	case slots <- struct{}{}:
		// Cancellation can race with a newly available slot. Do not start work
		// that was cancelled while it was waiting for capacity.
		if err := ctx.Err(); err != nil {
			<-slots
			return nil, err
		}
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type concurrencyPool struct {
	Limit int `json:"limit"`
	InUse int `json:"inUse"`
}

type nodeConcurrency struct {
	Execution  concurrencyPool `json:"execution"`
	Operations concurrencyPool `json:"operations"`
}

func (n *Node) concurrencySnapshot() nodeConcurrency {
	return nodeConcurrency{
		Execution:  concurrencyPool{Limit: cap(n.slots), InUse: len(n.slots)},
		Operations: concurrencyPool{Limit: cap(n.operationSlots), InUse: len(n.operationSlots)},
	}
}
