package transport

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// SelectRequest answers nodes.select. With "all": true it returns every
// eligible machine as a JSON array instead of one machine. Older selectors
// ignore the field and answer a single object, which a caller expecting an
// array cannot decode, so a fan-out never silently shrinks to one machine.
func (p *Peer) SelectRequest(ctx context.Context, args json.RawMessage) (any, error) {
	var mode struct {
		All bool `json:"all"`
	}
	if json.Unmarshal(args, &mode) == nil && mode.All {
		return p.SelectAll(ctx, args)
	}
	return p.Select(ctx, args)
}

// SelectAll applies exactly the filters of Select (online, enabled,
// policy-acknowledged, not instruction-only, labels, capability, known leases,
// update reservations and fresh resource constraints) and returns every
// match ordered by name and ID. Like Select it is advisory and reserves
// nothing; destination admission still decides.
func (p *Peer) SelectAll(ctx context.Context, args json.RawMessage) ([]model.NodeSelection, error) {
	query, candidates, err := p.selectCandidates(ctx, args)
	if err != nil {
		return nil, err
	}
	eligible, err := eligiblePlacements(candidates, p.placementHealth(ctx), query, time.Now())
	if err != nil {
		return nil, err
	}
	selections := make([]model.NodeSelection, len(eligible))
	for i, n := range eligible {
		selections[i] = model.NodeSelection{Node: n, GPUConstraints: query.GPUConstrained()}
	}
	return selections, nil
}

func (p *Peer) selectCandidates(ctx context.Context, args json.RawMessage) (model.NodeSelector, []model.Node, error) {
	query, err := model.DecodeNodeSelector(args)
	if err != nil {
		return query, nil, err
	}
	nodes, err := p.Nodes(ctx)
	if err != nil {
		return query, nil, err
	}
	candidates := selectorCandidates(nodes, query)
	if len(candidates) == 0 {
		return query, nil, errors.New("no online enabled node matches the selector")
	}
	return query, candidates, nil
}

// selectorCandidates keeps directory entries that may receive selected work.
// Instruction-only machines, such as Hybrid System companions, need an
// explicitly named target and are never selected.
func selectorCandidates(nodes []model.Node, query model.NodeSelector) []model.Node {
	candidates := []model.Node{}
	for _, peer := range nodes {
		if !peer.Online || peer.Disabled || peer.ControlPending || peer.InstructionOnly {
			continue
		}
		matches := true
		for k, v := range query.Labels {
			if peer.Labels[k] != v {
				matches = false
				break
			}
		}
		if matches && query.Capability != "" {
			matches = slices.ContainsFunc(peer.Capabilities, func(c model.Capability) bool { return c.Name == query.Capability })
		}
		if matches {
			candidates = append(candidates, peer)
		}
	}
	return candidates
}
