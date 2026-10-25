package transport

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/wol"
)

type lanCandidate struct {
	peer          model.Node
	local, remote model.WakeInterface
}

func (p *Peer) localWakeInterfaces() ([]model.WakeInterface, error) {
	if p.wakeSnapshot != nil {
		return p.wakeSnapshot()
	}
	return wol.Interfaces()
}

func (p *Peer) watchLAN() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var candidates []lanCandidate
	var refreshed time.Time
	attempted := map[string]time.Time{}
	for {
		p.mu.Lock()
		enabled := p.lanDiscovery && p.ws != nil
		local := slices.Clone(p.cfg.Node.WakeInterfaces)
		p.mu.Unlock()
		if enabled && time.Since(refreshed) >= 10*time.Second {
			refreshed = time.Now()
			// Reconnection republishes changed MAC/address metadata. A failed
			// interface sample must never erase the last successful registration.
			if current, err := p.localWakeInterfaces(); err == nil && !slices.Equal(current, local) {
				p.resetNetwork()
				candidates = nil
			} else {
				ctx, cancel := context.WithTimeout(p.ctx, 3*time.Second)
				nodes, err := p.Nodes(ctx)
				cancel()
				if err == nil {
					candidates = lanCandidates(p.IdentityID(), local, nodes, time.Now())
				}
			}
		}
		if enabled {
			for _, candidate := range candidates {
				key := candidate.peer.ID + candidate.local.Address + candidate.remote.Address
				if time.Since(attempted[key]) < 2*time.Minute {
					continue
				}
				if len(attempted) >= 256 {
					for k, at := range attempted {
						if time.Since(at) >= 2*time.Minute {
							delete(attempted, k)
						}
					}
					if len(attempted) >= 256 {
						break
					}
				}
				attempted[key] = time.Now()
				p.probeLAN(candidate)
				break // One bounded probe at a time, independent of execution.
			}
		}
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func lanCandidates(id string, local []model.WakeInterface, nodes []model.Node, now time.Time) []lanCandidate {
	var self model.Node
	for _, node := range nodes {
		if node.ID == id {
			self = node
			break
		}
	}
	var candidates []lanCandidate
	for _, node := range nodes {
		if node.ID == id || !node.Online || !node.LANDiscovery {
			continue
		}
		for _, a := range local {
			for _, b := range node.WakeInterfaces {
				if !wol.SameSubnet(a, b) {
					continue
				}
				fresh := slices.ContainsFunc(self.LANPeers, func(peer model.LANPeer) bool {
					return peer.ID == node.ID && peer.Local == a && peer.Remote == b && !peer.VerifiedAt.After(now) && now.Sub(peer.VerifiedAt) < 15*time.Minute
				})
				if !fresh && len(candidates) < 128 {
					candidates = append(candidates, lanCandidate{peer: node, local: a, remote: b})
				}
			}
		}
	}
	return candidates
}

func (p *Peer) probeLAN(candidate lanCandidate) {
	ctx, cancel := context.WithTimeout(p.ctx, 3*time.Second)
	defer cancel()
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return
	}
	defer func() { _ = socket.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = socket.SetReadDeadline(deadline)
	request := model.LANProbe{Receiver: p.IdentityID(), Sender: candidate.peer.ID, ReceiverNIC: candidate.local, SenderNIC: candidate.remote, Challenge: identity.NewID(), Port: socket.LocalAddr().(*net.UDPAddr).Port}
	if p.send(ctx, &protocol.Packet{Kind: "lan.probe", To: request.Sender, Session: request.Challenge, Data: model.JSON(request)}) != nil {
		return
	}
	var buffer [2049]byte
	for range 8 {
		n, from, err := socket.ReadFromUDP(buffer[:])
		if err != nil {
			return
		}
		var proof model.LANProof
		if json.Unmarshal(buffer[:n], &proof) != nil || !validLANProof(proof, request, candidate.peer.PublicKey, from.IP.String(), time.Now()) {
			continue
		}
		_ = p.SendControl(ctx, "lan.report", model.JSON(proof))
		return
	}
}

func validLANProof(proof model.LANProof, request model.LANProbe, key []byte, from string, now time.Time) bool {
	prefix, err := netip.ParsePrefix(request.SenderNIC.Address)
	return err == nil && len(key) == ed25519.PublicKeySize && proof.LANProbe == request && validChannelID(proof.Nonce) && prefix.Addr().String() == from && !proof.IssuedAt.After(now.Add(5*time.Second)) && now.Sub(proof.IssuedAt) <= time.Minute && ed25519.Verify(key, proof.Message(), proof.Signature)
}

func (p *Peer) receiveLANProbe(msg *protocol.Packet) {
	p.mu.Lock()
	enabled := p.lanDiscovery && !p.cfg.Client
	now := time.Now()
	if !enabled || now.Sub(p.lanInbound[msg.From]) < 5*time.Second {
		p.mu.Unlock()
		return
	}
	for id, at := range p.lanInbound {
		if now.Sub(at) > time.Minute {
			delete(p.lanInbound, id)
		}
	}
	if len(p.lanInbound) >= 256 {
		p.mu.Unlock()
		return
	}
	p.lanInbound[msg.From] = now
	p.mu.Unlock()
	if len(msg.Data) > 1024 {
		return
	}
	var request model.LANProbe
	if json.Unmarshal(msg.Data, &request) != nil || request.Receiver != msg.From || request.Sender != p.IdentityID() || request.Challenge != msg.Session || !validChannelID(request.Challenge) || request.Port < 1024 || request.Port > 65535 || !wol.SameSubnet(request.ReceiverNIC, request.SenderNIC) {
		return
	}
	select {
	case p.lanSlots <- struct{}{}:
		if !p.worker(func() {
			defer func() { <-p.lanSlots }()
			ctx, cancel := context.WithTimeout(p.ctx, 2*time.Second)
			defer cancel()
			receiver, err := p.Lookup(ctx, msg.From)
			if err != nil || !receiver.Online || !receiver.LANDiscovery || !slices.Contains(receiver.WakeInterfaces, request.ReceiverNIC) {
				return
			}
			p.mu.Lock()
			local := slices.Contains(p.cfg.Node.WakeInterfaces, request.SenderNIC)
			p.mu.Unlock()
			if !local {
				return
			}
			proof := model.LANProof{LANProbe: request, Nonce: identity.NewID(), IssuedAt: time.Now().UTC()}
			proof.Signature = ed25519.Sign(p.cfg.Identity.Private, proof.Message())
			_ = wol.Broadcast(ctx, request.SenderNIC, request.Port, model.JSON(proof))
		}) {
			<-p.lanSlots
		}
	default:
	}
}
