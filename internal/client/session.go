package client

import (
	"context"
	"encoding/json"

	"github.com/koltyakov/control/internal/model"
)

type SessionInfo struct {
	Role        string              `json:"role"`
	OwnerID     string              `json:"ownerId"`
	TransportID string              `json:"transportId"`
	Connections map[string]string   `json:"connections"`
	Sessions    []model.PeerSession `json:"sessions"`
	Forwards    []ForwardInfo       `json:"forwards"`
}

// Session describes this requesting process, not an enrolled fleet machine.
func (c Client) Session(ctx context.Context) (SessionInfo, error) {
	peer, err := c.backend(ctx)
	if err != nil {
		return SessionInfo{}, err
	}
	info := SessionInfo{Role: "client", Connections: map[string]string{}, Forwards: c.Forwards()}
	if peer != nil {
		info.OwnerID, info.TransportID = c.routing.ownerID, peer.IdentityID()
		info.Connections = peer.Connections()
		info.Sessions = peer.SessionStats()
		return info, nil
	}
	var local struct {
		ID          string              `json:"id"`
		Connections map[string]string   `json:"connections"`
		Sessions    []model.PeerSession `json:"sessions"`
	}
	if err := c.Call(ctx, "", "node.describe", json.RawMessage(`{}`), &local); err != nil {
		return info, err
	}
	info.Role, info.OwnerID, info.TransportID, info.Connections = "node-api", local.ID, local.ID, local.Connections
	info.Sessions = local.Sessions
	return info, nil
}
