package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/koltyakov/control/internal/model"
)

func (n *Node) wake(ctx context.Context, args json.RawMessage, _ Execution) (any, error) {
	var q struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if q.Target == "" {
		return nil, errors.New("wake requires a registered target")
	}
	// Read the fleet directory rather than accepting caller-supplied MACs or addresses.
	nodes, err := n.Peer.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, target := range nodes {
		if target.ID != q.Target && target.Name != q.Target {
			continue
		}
		if target.ID == n.Identity.ID || target.Online {
			return nil, errors.New("wake target must be another offline machine")
		}
		packets, err := n.sendWake(ctx, n.Config.Labels["lan"], target)
		return model.WakeResult{Target: target.ID, Source: n.Identity.ID, Packets: packets}, err
	}
	return nil, fmt.Errorf("unknown wake target %q", q.Target)
}
