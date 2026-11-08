package node

import (
	"context"
	"sync"

	"github.com/koltyakov/control/internal/model"
)

// providerPool shares admission and queue accounting between synchronous calls
// and tasks. The mutex protects only counters, never provider work or a wait.
type providerPool struct {
	slots  chan struct{}
	mu     sync.Mutex
	inUse  int
	queued int
}

func newProviderPool(limit int) *providerPool {
	return &providerPool{slots: make(chan struct{}, limit)}
}

// providerSlots is shared by durable tasks and synchronous calls. Only known
// built-ins use the operations pool; arbitrary commands and MCP tools retain
// the execution bound because their resource use cannot be inferred safely.
func (n *Node) providerSlots(capability string) (*providerPool, string) {
	switch capability {
	case "workflow.run", "peers.call":
		// Coordination must not hold a slot needed by its local child work.
		return nil, ""
	case "files.list", "files.read", "files.write", "http.request", "network.wake":
		return n.operationsPool, "waiting for operation slot"
	case "rpa.run":
		if n.Config.RPA != nil {
			return n.desktopPool, "waiting for desktop slot"
		}
	case "desktop.run":
		if n.Config.Desktop != nil {
			return n.desktopPool, "waiting for desktop slot"
		}
	}
	return n.executionPool, "waiting for worker slot"
}

func acquireProviderSlot(ctx context.Context, pool *providerPool) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pool == nil {
		return func() {}, nil
	}
	pool.mu.Lock()
	select {
	case pool.slots <- struct{}{}:
		pool.inUse++
		pool.mu.Unlock()
	default:
		pool.queued++
		pool.mu.Unlock()
		select {
		case pool.slots <- struct{}{}:
			pool.mu.Lock()
			pool.queued--
			pool.inUse++
			pool.mu.Unlock()
		case <-ctx.Done():
			pool.mu.Lock()
			pool.queued--
			pool.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	// Cancellation can race with a newly available slot. Do not start work
	// that was cancelled while it was waiting for capacity.
	if err := ctx.Err(); err != nil {
		pool.release()
		return nil, err
	}
	return pool.release, nil
}

func (p *providerPool) release() {
	p.mu.Lock()
	p.inUse--
	<-p.slots
	p.mu.Unlock()
}

func (p *providerPool) snapshot() model.ConcurrencyPool {
	p.mu.Lock()
	defer p.mu.Unlock()
	queued := p.queued
	return model.ConcurrencyPool{Limit: cap(p.slots), InUse: p.inUse, Queued: &queued}
}

func (n *Node) concurrencySnapshot() model.NodeConcurrency {
	desktop := n.desktopPool.snapshot()
	staging := n.inputStagingPool.snapshot()
	publication := n.publicationPool.snapshot()
	return model.NodeConcurrency{
		Execution:         n.executionPool.snapshot(),
		Operations:        n.operationsPool.snapshot(),
		Desktop:           &desktop,
		InputStaging:      &staging,
		OutputPublication: &publication,
	}
}
