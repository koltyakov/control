package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wol"
)

type WakeSpec struct {
	Node string `json:"node"`
	From string `json:"from,omitempty"`
}

func (c Client) Wake(ctx context.Context, spec WakeSpec) (model.WakeResult, error) {
	var result model.WakeResult
	if spec.Node == "" {
		return result, errors.New("wake requires a target machine")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	peer, err := c.backend(ctx)
	if err != nil {
		return result, err
	}
	if peer == nil || c.routing == nil || !c.routing.cfg.AccountRouting {
		return result, errors.New("wake requires account-client routing; omit --api and CONTROL_API")
	}
	// Local sends need execution authority too, not just a common discovery key.
	var auth struct {
		Role            string `json:"role"`
		FleetVaultCheck bool   `json:"fleetVaultCheck"`
	}
	if err := c.routing.cfg.Gateway.JSON(ctx, "GET", "/v1/auth", nil, &auth); err != nil {
		return result, err
	}
	if !ExecutionRole(auth.Role) {
		return result, errors.New("wake requires an account or orchestrator key")
	}
	nodes, err := peer.Nodes(ctx)
	if err != nil {
		return result, err
	}
	var target model.Node
	for _, n := range nodes {
		if n.ID == spec.Node || n.Name == spec.Node {
			target = n
			break
		}
	}
	if target.ID == "" {
		return result, fmt.Errorf("unknown wake target %q", spec.Node)
	}
	if target.Online {
		return result, errors.New("wake target is already online")
	}
	result.Target = target.ID
	if spec.From == "local" || spec.From == "" {
		local, localErr := wol.Interfaces()
		if localErr == nil {
			for _, source := range nodes {
				// Match a registered sender to a NIC on this host, then use the
				// same verified relationship as a worker. No LAN assertion needed.
				candidate := source
				candidate.WakeInterfaces = local
				if wol.CanSend(source, target) && wol.CanSend(candidate, target) {
					result.Source = "local"
					if err := localWakeAllowed(ctx, peer, auth.FleetVaultCheck, target.ID, source.ID); err != nil {
						return result, err
					}
					result.Packets, err = wol.Send(ctx, source.ID, target)
					return result, err
				}
			}
		}
		if derived, err := wol.LocalTarget(ctx, target, nodes); err == nil {
			result.Source = "local"
			if err := localWakeAllowed(ctx, peer, auth.FleetVaultCheck, target.ID); err != nil {
				return result, err
			}
			result.Packets, err = wol.Send(ctx, "local", derived)
			return result, err
		}
		if spec.From == "local" {
			return result, errors.New("this host has no verified LAN relationship or matching local IP/MAC neighbor for the target")
		}
	}
	source, err := wakeSender(nodes, target, spec.From)
	if err != nil {
		return result, err
	}
	// Select once. Never switch sender after an uncertain RPC result.
	err = c.Call(ctx, source.ID, "network.wake", map[string]string{"target": target.ID}, &result)
	return result, err
}

// fleetVaultChecker is the transport's gateway decision call.
type fleetVaultChecker interface {
	IdentityID() string
	CheckFleetVault(context.Context, model.FleetVaultCheck) (model.FleetVaultDecision, error)
}

// localWakeAllowed asks a gateway that advertises fleet vault checks whether
// this client's authenticated transport identity may reach the target, and
// the registered sender whose LAN relationship it reuses, before this host
// sends packets itself. No worker receives the request to check it. One
// allowance must cover every machine. It is asked once, with no reuse or
// retry; an unavailable decision sends nothing. Older gateways are unchanged.
func localWakeAllowed(ctx context.Context, peer fleetVaultChecker, supported bool, machines ...string) error {
	if !supported {
		return nil
	}
	decision, err := peer.CheckFleetVault(ctx, model.FleetVaultCheck{Issuer: peer.IdentityID(), MachineIDs: machines}.Normalize())
	if err != nil {
		return &model.RPCError{Code: model.FleetVaultUnavailable, Outcome: model.OutcomeNotAccepted, Message: "fleet vault decision unavailable; no wake packet was sent: " + err.Error(), Cause: err}
	}
	if !decision.Allowed {
		return &model.RPCError{Code: model.FleetVaultDenied, Outcome: model.OutcomeNotAccepted, Message: "fleet vault policy does not allow this client to wake the target from this host; no wake packet was sent"}
	}
	return nil
}

func wakeSender(nodes []model.Node, target model.Node, from string) (model.Node, error) {
	for _, source := range nodes {
		if from != "" && source.Name != from && source.ID != from {
			continue
		}
		if !source.Online || source.Disabled || source.ControlPending || !wol.CanSend(source, target) {
			continue
		}
		for _, capability := range source.Capabilities {
			if capability.Name == "network.wake" {
				return source, nil
			}
		}
	}
	return model.Node{}, errors.New("no online wake sender with a verified LAN relationship; both machines must be online together once for automatic discovery")
}
