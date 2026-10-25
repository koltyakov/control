package gateway

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wol"
)

func retainedLANPeers(peers []model.LANPeer, interfaces []model.WakeInterface) []model.LANPeer {
	var result []model.LANPeer
	now := time.Now()
	for _, peer := range peers {
		if slices.Contains(interfaces, peer.Local) && !peer.VerifiedAt.After(now) && now.Sub(peer.VerifiedAt) < model.LANPeerTTL && len(result) < model.MaxLANPeers {
			result = append(result, peer)
		}
	}
	return result
}

func lanNonce(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16
}

func (g *Gateway) receiveLANReport(receiver model.Node, c *connection, data []byte) bool {
	if len(data) > 2048 {
		return false
	}
	var proof model.LANProof
	if json.Unmarshal(data, &proof) != nil || proof.Receiver != receiver.ID || !lanNonce(proof.Challenge) || !lanNonce(proof.Nonce) || proof.Port < 1024 || proof.Port > 65535 {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n, present := g.nodes[receiver.ID]
	sender, exists := g.nodes[proof.Sender]
	// Network changes, disconnects and retirement can race an in-flight probe.
	// Drop stale evidence without disconnecting an otherwise healthy node.
	now := time.Now().UTC()
	if !present || !exists || g.peers[receiver.ID] != c || !sender.Online || sender.UserID != n.UserID || !sender.LANDiscovery || proof.Sender == receiver.ID || proof.IssuedAt.After(now.Add(5*time.Second)) || now.Sub(proof.IssuedAt) > time.Minute || !slices.Contains(n.WakeInterfaces, proof.ReceiverNIC) || !slices.Contains(sender.WakeInterfaces, proof.SenderNIC) || !wol.SameSubnet(proof.ReceiverNIC, proof.SenderNIC) {
		return true
	}
	if !ed25519.Verify(sender.PublicKey, proof.Message(), proof.Signature) {
		return false
	}
	previous := n
	peers := retainedLANPeers(n.LANPeers, n.WakeInterfaces)
	next := model.LANPeer{ID: sender.ID, Local: proof.ReceiverNIC, Remote: proof.SenderNIC, VerifiedAt: proof.IssuedAt}
	index := slices.IndexFunc(peers, func(peer model.LANPeer) bool {
		return peer.ID == next.ID && peer.Local == next.Local && peer.Remote == next.Remote
	})
	if index >= 0 {
		if !next.VerifiedAt.After(peers[index].VerifiedAt.Add(10 * time.Minute)) {
			return true // Replays cannot extend evidence or force repeated fsyncs.
		}
		peers[index] = next
	} else {
		if len(peers) >= model.MaxLANPeers {
			slices.SortFunc(peers, func(a, b model.LANPeer) int { return b.VerifiedAt.Compare(a.VerifiedAt) })
			peers = peers[:model.MaxLANPeers-1]
		}
		peers = append(peers, next)
	}
	n.LANPeers = peers
	g.nodes[n.ID] = n
	if err := g.commit(change{nodes: []string{n.ID}}); err != nil {
		g.nodes[n.ID] = previous
		return true // A later discovery cycle can report fresh evidence.
	}
	return true
}
