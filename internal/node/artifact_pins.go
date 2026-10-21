package node

import (
	"context"
	"encoding/json"
)

func (n *Node) pinArtifact(ctx context.Context, id string) (func(), error) {
	unlock, err := n.lockTransfer(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	n.artifactMu.Lock()
	if n.artifactReaders == nil {
		n.artifactReaders = make(map[string]int)
	}
	n.artifactReaders[id]++
	n.artifactMu.Unlock()
	return func() {
		n.artifactMu.Lock()
		n.artifactReaders[id]--
		if n.artifactReaders[id] == 0 {
			delete(n.artifactReaders, id)
		}
		n.artifactMu.Unlock()
	}, nil
}

func (n *Node) artifactReading(id string) bool {
	n.artifactMu.Lock()
	defer n.artifactMu.Unlock()
	return n.artifactReaders[id] > 0
}

func (n *Node) artifactGranted(id string) bool {
	n.delegationMu.Lock()
	defer n.delegationMu.Unlock()
	for _, s := range n.delegations {
		if s.grant.Method != "artifacts.open" || s.ctx.Err() != nil {
			continue
		}
		var q struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(s.grant.Params, &q) == nil && q.ID == id {
			return true
		}
	}
	return false
}
