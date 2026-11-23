package transport

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/wol"
)

type lanCandidate struct {
	peer          model.LANNode
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
	directory := lanDirectory{}
	attempted := map[string]time.Time{}
	// changed holds an interface sample that differs from the registration.
	// It is republished only once a later sample confirms it and no relay link
	// shares the gateway socket, so transient adapters such as a container
	// bridge cannot cut this node's relay streams.
	var changed []model.WakeInterface
	interval := time.Second
	for {
		p.mu.Lock()
		enabled := p.lanDiscovery && p.ws != nil
		scope := p.lanDirectoryScopeLocked()
		local := slices.Clone(p.cfg.Node.WakeInterfaces)
		relaying := p.relayLinksLocked()
		p.mu.Unlock()
		if !enabled || directory.scope != scope {
			candidates = nil
		}
		if enabled && time.Since(refreshed) >= 10*time.Second {
			refreshed = time.Now()
			// Gateway re-registration republishes changed MAC/address metadata.
			// Wake adapters, such as container or VM bridges, are not gateway
			// paths, so healthy direct sessions stay up. A failed interface
			// sample must never erase the last successful registration.
			republish := false
			if current, err := p.localWakeInterfaces(); err == nil {
				changed, republish = wakeRepublish(current, local, changed, relaying)
			}
			if republish {
				p.republishRegistration()
				candidates = nil
			} else {
				ctx, cancel := context.WithTimeout(p.ctx, 3*time.Second)
				view, err := directory.poll(ctx, p)
				cancel()
				candidates = nil
				if err == nil {
					candidates = lanCandidates(view, local, time.Now())
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
		// Without LAN discovery there is no probing to schedule; only notice
		// when it is enabled.
		if next := time.Second; enabled != (interval == next) {
			if !enabled {
				next = 10 * time.Second
			}
			interval = next
			ticker.Reset(interval)
		}
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// wakeRepublish debounces registration changes for wake interfaces. A sample
// that differs from the registration becomes pending; a later identical sample
// confirms it, and republishing waits while relay links share the socket.
func wakeRepublish(current, registered, pending []model.WakeInterface, relaying bool) ([]model.WakeInterface, bool) {
	switch {
	case slices.Equal(current, registered):
		return nil, false
	case !slices.Equal(current, pending):
		return current, false
	case relaying:
		return pending, false
	}
	return nil, true
}

// relayLinksLocked reports whether a relay link shares the gateway socket.
// Called with p.mu held.
func (p *Peer) relayLinksLocked() bool {
	for _, l := range p.links {
		if l.mode == "relay" {
			return true
		}
	}
	return false
}

// lanDirectory belongs to one watchLAN goroutine. Its decoded records are never
// shared with Nodes, Lookup, or membership checks. Even a 304 must recompute
// candidates so the fifteen-minute proof freshness window continues to age.
type lanDirectory struct {
	view  model.LANView
	etag  string
	scope lanDirectoryScope
}

type lanDirectoryScope struct {
	userID     string
	session    string
	generation uint64
}

func (p *Peer) lanDirectoryScope() lanDirectoryScope {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lanDirectoryScopeLocked()
}

func (p *Peer) lanDirectoryScopeLocked() lanDirectoryScope {
	if p.lanViewSession != "" {
		// Unrelated directory notifications do not invalidate this view.
		return lanDirectoryScope{userID: p.userID, session: p.lanViewSession}
	}
	return lanDirectoryScope{userID: p.userID, generation: p.directoryGeneration}
}

func (d *lanDirectory) poll(ctx context.Context, p *Peer) (model.LANView, error) {
	scope := p.lanDirectoryScope()
	if d.scope != scope {
		*d = lanDirectory{scope: scope}
	}
	id := p.IdentityID()
	legacy := scope.session == ""
	path := "/v1/nodes"
	if !legacy {
		path = "/v1/lan/" + id
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL(p.cfg.Gateway, path), nil)
	if err != nil {
		return model.LANView{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	if !legacy {
		req.Header.Set(gateway.LANViewSessionHeader, scope.session)
	}
	if d.etag != "" {
		req.Header.Set("If-None-Match", d.etag)
	}
	// Never forward the live session credential on a redirect.
	resp, err := p.cfg.Route.Client(15*time.Second, legacy).Do(req)
	if err != nil {
		return model.LANView{}, gatewayNetworkError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var view model.LANView
	switch resp.StatusCode {
	case http.StatusNotModified:
		if d.etag == "" || resp.Header.Get("ETag") != d.etag {
			return model.LANView{}, errors.New("LAN directory returned an unexpected validator")
		}
		view = d.view
	case http.StatusOK:
		body := io.LimitReader(resp.Body, 8<<20)
		if legacy {
			view, err = legacyLANView(body, scope.userID, id)
		} else {
			err = json.NewDecoder(body).Decode(&view)
			if err == nil && (scope.userID == "" || view.UserID != scope.userID || view.Receiver != id) {
				err = errors.New("LAN view returned a mismatched fleet or receiver")
			}
		}
		if err != nil {
			return model.LANView{}, err
		}
	default:
		return model.LANView{}, directoryStatusError("LAN directory", resp)
	}
	if scope != p.lanDirectoryScope() {
		*d = lanDirectory{}
		return model.LANView{}, errors.New("directory changed during LAN refresh")
	}
	d.view, d.etag = view, resp.Header.Get("ETag")
	return view, nil
}

func legacyLANView(body io.Reader, user, id string) (model.LANView, error) {
	var nodes []model.Node
	if err := json.NewDecoder(body).Decode(&nodes); err != nil {
		return model.LANView{}, err
	}
	view := model.LANView{UserID: user, Receiver: id}
	for _, node := range nodes {
		if user == "" || node.UserID != user {
			return model.LANView{}, errors.New("directory returned a node outside the authenticated fleet")
		}
		if node.ID == id {
			view.LANPeers = node.LANPeers
		}
		if probe, ok := node.LANProbeNode(); ok {
			view.Nodes = append(view.Nodes, probe)
		}
	}
	return view, nil
}

func lanCandidates(view model.LANView, local []model.WakeInterface, now time.Time) []lanCandidate {
	var candidates []lanCandidate
	for _, node := range view.Nodes {
		if node.ID == view.Receiver {
			continue
		}
		for _, a := range local {
			for _, b := range node.WakeInterfaces {
				if !wol.SameSubnet(a, b) {
					continue
				}
				fresh := slices.ContainsFunc(view.LANPeers, func(peer model.LANPeer) bool {
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
