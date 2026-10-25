package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func (n *Node) wake(ctx context.Context, args json.RawMessage, _ Execution) (any, error) {
	var q struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	return n.wakeTarget(ctx, q.Target)
}

func (n *Node) wakeTarget(ctx context.Context, name string) (model.WakeResult, error) {
	if name == "" {
		return model.WakeResult{}, errors.New("wake requires a registered target")
	}
	// Read the fleet directory rather than accepting caller-supplied MACs or addresses.
	nodes, err := n.Peer.Nodes(ctx)
	if err != nil {
		return model.WakeResult{}, err
	}
	for _, target := range nodes {
		if target.ID != name && target.Name != name {
			continue
		}
		if target.ID == n.Identity.ID || target.Online {
			return model.WakeResult{}, errors.New("wake target must be another offline machine")
		}
		packets, err := n.sendWake(ctx, n.Identity.ID, target)
		return model.WakeResult{Target: target.ID, Source: n.Identity.ID, Packets: packets}, err
	}
	return model.WakeResult{}, fmt.Errorf("unknown wake target %q", name)
}

// accountMayCall applies allow rules as they apply to an account-authenticated
// client. A gateway-relayed wake acts for the fleet owner, so it is permitted
// exactly where that owner's CLI could call the method.
func accountMayCall(allow map[string][]string, method string) bool {
	if allow == nil {
		return true
	}
	for _, pattern := range append(append([]string{}, allow["@orchestrator"]...), allow["*"]...) {
		if ok, _ := path.Match(pattern, method); ok {
			return true
		}
	}
	return false
}

// gatewayWake serves a wake the fleet owner requested in the gateway console.
// It applies the same allow rules, work admission, directory lookup, and live
// LAN checks as network.wake, and replies exactly once without retrying.
func (n *Node) gatewayWake(data []byte) {
	var q struct {
		ID     string `json:"id"`
		Target string `json:"target"`
	}
	if json.Unmarshal(data, &q) != nil || q.ID == "" || len(q.ID) > 64 {
		return
	}
	ctx, cancel := context.WithTimeout(n.ctx, 8*time.Second)
	defer cancel()
	reply := struct {
		ID      string `json:"id"`
		Packets int    `json:"packets"`
		Error   string `json:"error,omitempty"`
	}{ID: q.ID}
	result, err := n.relayedWake(ctx, q.Target)
	reply.Packets = result.Packets
	if err != nil {
		reply.Error = err.Error()
		if len(reply.Error) > 500 {
			reply.Error = reply.Error[:500]
		}
	}
	_ = n.Peer.SendControl(ctx, "wake.result", model.JSON(reply))
}

func (n *Node) relayedWake(ctx context.Context, target string) (model.WakeResult, error) {
	if !accountMayCall(n.Config.Allow, "network.wake") {
		return model.WakeResult{}, errors.New("this machine's allow rules do not permit account clients to send wake packets")
	}
	release, err := n.work.Enter(ctx)
	if err != nil {
		return model.WakeResult{}, err
	}
	defer release()
	return n.wakeTarget(ctx, target)
}
