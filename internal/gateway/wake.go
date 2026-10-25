package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
	"github.com/koltyakov/control/internal/wol"
)

// Console wake relays one request to one sender node. The gateway selects the
// sender and never sends packets itself; the sender resolves the target in its
// own directory, rechecks its live interfaces, and replies once.

const (
	wakeTimeout    = 10 * time.Second
	maxPendingWake = 64
)

type wakeRequest struct {
	ID     string `json:"id"`
	Target string `json:"target"`
}

type wakeReply struct {
	ID      string `json:"id"`
	Packets int    `json:"packets"`
	Error   string `json:"error,omitempty"`
}

type pendingWake struct {
	sender, target string
	reply          chan wakeReply
}

// wakeSender returns the first online, enabled sender by name that accepts
// gateway-relayed wake and has a verified LAN relationship with the target.
// It is the same selection hint the CLI uses; the sender revalidates.
func wakeSender(nodes []model.Node, target model.Node) (model.Node, bool) {
	if target.Online || len(target.WakeInterfaces) == 0 {
		return model.Node{}, false
	}
	candidates := slices.Clone(nodes)
	slices.SortFunc(candidates, func(a, b model.Node) int { return strings.Compare(a.Name, b.Name) })
	for _, s := range candidates {
		if s.ID != target.ID && s.UserID == target.UserID && s.Online && !s.Disabled && !s.ControlPending && s.GatewayWake && wol.CanSend(s, target) {
			return s, true
		}
	}
	return model.Node{}, false
}

func (g *Gateway) wakeMachine(w http.ResponseWriter, r *http.Request) {
	p := g.requestPrincipal(r)
	g.mu.Lock()
	target, ok := g.nodes[r.PathValue("id")]
	if !ok || target.UserID != p.UserID {
		g.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if target.Online {
		g.mu.Unlock()
		http.Error(w, "machine is already online", http.StatusConflict)
		return
	}
	fleet := []model.Node{}
	for _, n := range g.nodes {
		if n.UserID == p.UserID {
			fleet = append(fleet, n)
		}
	}
	sender, ok := wakeSender(fleet, target)
	peer := g.peers[sender.ID]
	if !ok || peer == nil {
		g.mu.Unlock()
		http.Error(w, "no online machine on this LAN can send a wake packet", http.StatusConflict)
		return
	}
	for _, pending := range g.wakes {
		if pending.target == target.ID {
			g.mu.Unlock()
			http.Error(w, "a wake request for this machine is already in progress", http.StatusConflict)
			return
		}
	}
	if len(g.wakes) >= maxPendingWake {
		g.mu.Unlock()
		http.Error(w, "too many wake requests in progress", http.StatusTooManyRequests)
		return
	}
	id := identity.NewID()
	reply := make(chan wakeReply, 1)
	g.wakes[id] = pendingWake{sender: sender.ID, target: target.ID, reply: reply}
	sent := peer.enqueue(&protocol.Packet{Kind: "wake.request", From: update.GatewaySender, Data: model.JSON(wakeRequest{ID: id, Target: target.ID})})
	g.mu.Unlock()

	var result wakeReply
	status, outcome := http.StatusOK, ""
	if !sent {
		status, outcome = http.StatusServiceUnavailable, "the sender's connection is busy; nothing was sent"
	} else {
		timer := time.NewTimer(wakeTimeout)
		select {
		case result = <-reply:
			if result.Error != "" {
				status, outcome = http.StatusBadGateway, result.Error
			}
		case <-timer.C:
			// The sender may still have sent the packet; never retry automatically.
			status, outcome = http.StatusGatewayTimeout, "the sender did not confirm; it may have sent the packet, so check the machine before trying again"
		case <-r.Context().Done():
			status, outcome = http.StatusGatewayTimeout, "request canceled before the sender confirmed"
		}
		timer.Stop()
	}
	detail := fmt.Sprintf("from %s: %s", sender.Name, outcome)
	if status == http.StatusOK {
		detail = fmt.Sprintf("from %s: sent %d packet(s)", sender.Name, result.Packets)
	}
	g.mu.Lock()
	delete(g.wakes, id)
	// The wake already happened or failed; a failed record must not hide that.
	_ = g.commit(change{audit: []AuditEvent{g.auditEventLocked(r, "", "machine.wake", target.Name, target.ID, detail)}})
	g.mu.Unlock()
	if status != http.StatusOK {
		http.Error(w, outcome, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"target": target.Name, "source": sender.Name, "packets": result.Packets})
}

// receiveWake accepts a sender's single reply. A reply for another sender's
// request is a protocol violation; a late reply for a finished request is not.
func (g *Gateway) receiveWake(n model.Node, data []byte) bool {
	if len(data) > 4096 {
		return false
	}
	var reply wakeReply
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&reply) != nil || decoder.Decode(new(any)) != io.EOF || reply.Packets < 0 || len(reply.Error) > 512 {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	pending, ok := g.wakes[reply.ID]
	if !ok {
		return true
	}
	if pending.sender != n.ID {
		return false
	}
	select {
	case pending.reply <- reply:
	default:
	}
	return true
}
