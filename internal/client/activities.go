package client

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

// Account clients observe peers with their own authority, not a worker's.
func peerActivities(ctx context.Context, p *transport.Peer, args json.RawMessage) (model.PoolActivitySnapshot, error) {
	var q model.PoolActivityQuery
	if err := json.Unmarshal(args, &q); err != nil {
		return model.PoolActivitySnapshot{}, err
	}
	if q.Recent < 0 || q.Recent > 64 {
		return model.PoolActivitySnapshot{}, fmt.Errorf("recent must be 0..64")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	registered, err := p.Nodes(ctx)
	if err != nil {
		return model.PoolActivitySnapshot{}, err
	}
	selected := map[string]bool{}
	for _, name := range q.Nodes {
		selected[name] = false
	}
	pool := model.PoolActivitySnapshot{Nodes: []model.NodeActivitySnapshot{}}
	for _, machine := range registered {
		_, byID := selected[machine.ID]
		_, byName := selected[machine.Name]
		if len(selected) > 0 && !byID && !byName {
			continue
		}
		if byID {
			selected[machine.ID] = true
		}
		if byName {
			selected[machine.Name] = true
		}
		pool.Nodes = append(pool.Nodes, model.NodeActivitySnapshot{ID: machine.ID, Name: machine.Name, OS: machine.OS, Labels: machine.Labels, Online: machine.Online, LastSeen: machine.LastSeen, Software: machine.Software, Disabled: machine.Disabled, ControlPending: machine.ControlPending, Status: "offline", Active: []model.Activity{}, Recent: []model.Activity{}, System: machine.System})
	}
	for name, found := range selected {
		if !found {
			return pool, fmt.Errorf("unknown node %q", name)
		}
	}
	jobs := make(chan int, len(pool.Nodes))
	for i := range pool.Nodes {
		if pool.Nodes[i].Online {
			jobs <- i
		}
	}
	close(jobs)
	var wg sync.WaitGroup
	for range min(8, len(pool.Nodes)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				s := &pool.Nodes[i]
				poll, done := context.WithTimeout(ctx, 6*time.Second)
				var remote model.NodeActivitySnapshot
				err := peerCall(poll, p, s.ID, "activities.list", model.ActivityQuery{Recent: q.Recent}, &remote)
				done()
				if err != nil {
					s.Status = "unavailable"
					s.Error = err.Error()
					if len(s.Error) > 256 {
						s.Error = s.Error[:256]
					}
					continue
				}
				s.Status, s.ObservedAt, s.Active, s.Recent = "ready", remote.ObservedAt, remote.Active, remote.Recent
				s.ActiveCount, s.Omitted = remote.ActiveCount, remote.Omitted
				s.DirectSessions, s.RelaySessions = remote.DirectSessions, remote.RelaySessions
				s.LeaseOwner, s.LeaseExpires = remote.LeaseOwner, remote.LeaseExpires
				s.System = remote.System
			}
		}()
	}
	wg.Wait()
	remaining, recent := 4096, 512
	for i := range pool.Nodes {
		s := &pool.Nodes[i]
		if len(s.Active) > remaining {
			s.Omitted += len(s.Active) - remaining
			s.Active = s.Active[:remaining]
		}
		remaining -= len(s.Active)
		if len(s.Recent) > recent {
			s.Recent = s.Recent[:recent]
		}
		recent -= len(s.Recent)
	}
	pool.ObservedAt = time.Now().UTC()
	return pool, nil
}
