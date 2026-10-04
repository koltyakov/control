// Package workgate provides atomic admission and idle maintenance reservations.
package workgate

import (
	"context"
	"sync"
)

type Gate struct {
	mu      sync.Mutex
	active  int
	paused  bool
	changed chan struct{}
}

func New() *Gate { return &Gate{changed: make(chan struct{})} }

// Enter waits while reserved for maintenance. An operation is not accepted until
// it owns a ticket. Existing tickets always run to completion.
func (g *Gate) Enter(ctx context.Context) (func(), error) {
	for {
		g.mu.Lock()
		if err := ctx.Err(); err != nil {
			g.mu.Unlock()
			return nil, err
		}
		if !g.paused {
			g.active++
			g.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { g.mu.Lock(); g.active--; g.mu.Unlock() }) }, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (g *Gate) PauseIfIdle() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active != 0 {
		return false
	}
	g.paused = true
	return true
}

func (g *Gate) Resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		g.paused = false
		close(g.changed)
		g.changed = make(chan struct{})
	}
}

func (g *Gate) State() (active int, paused bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active, g.paused
}
