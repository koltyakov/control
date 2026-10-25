package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

type pendingStartup struct {
	node  string
	reply chan model.StartupReply
}

func (g *Gateway) changeStartup(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Mode string `json:"mode"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || (q.Mode != "system" && q.Mode != "hybrid") {
		http.Error(w, "mode must be system or hybrid; no commands or paths are accepted", http.StatusBadRequest)
		return
	}
	p := g.requestPrincipal(r)
	g.mu.Lock()
	n, ok := g.nodes[r.PathValue("id")]
	if !ok || n.UserID != p.UserID {
		g.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	result := model.StartupReply{Status: "manual"}
	switch {
	case n.RunAs != "user":
		result.Reason = "the machine no longer reports User execution; inspect its current state"
	case n.Disabled || g.machinePending(n.ID):
		result.Reason = "the machine is disabled or has pending policy; inspect it before changing startup"
	case !n.Online || g.peers[n.ID] == nil:
		result.Reason = "the machine is offline; run the command locally"
	case !n.GatewayStartup:
		result.Reason = "the installed node does not support gateway startup changes; update it or run the command locally"
	}
	if result.Reason != "" {
		g.mu.Unlock()
		consoleJSON(w, result)
		return
	}
	for _, pending := range g.startupChanges {
		if pending.node == n.ID {
			g.mu.Unlock()
			http.Error(w, "a startup request is already in progress; inspect the machine before retrying", http.StatusConflict)
			return
		}
	}
	if len(g.startupChanges) >= 64 {
		g.mu.Unlock()
		http.Error(w, "too many startup changes in progress", http.StatusTooManyRequests)
		return
	}
	id := identity.NewID()
	reply := make(chan model.StartupReply, 1)
	g.startupChanges[id] = pendingStartup{node: n.ID, reply: reply}
	// Record intent before sending a management side effect. No installation
	// tickets, commands, custom paths, or credentials enter this request or audit.
	if err := g.commit(change{audit: []AuditEvent{g.auditEventLocked(r, "", "machine.startup", n.Name, n.ID, "requested "+q.Mode)}}); err != nil {
		delete(g.startupChanges, id)
		g.mu.Unlock()
		http.Error(w, "could not record startup request; nothing was sent", http.StatusInternalServerError)
		return
	}
	sent := g.peers[n.ID].enqueue(&protocol.Packet{Kind: "startup.request", From: update.GatewaySender, Data: model.JSON(model.StartupRequest{ID: id, Mode: q.Mode})})
	g.mu.Unlock()
	if !sent {
		result.Reason = "the machine's connection is busy; nothing was sent"
	} else {
		timer := time.NewTimer(10 * time.Second)
		select {
		case result = <-reply:
		case <-timer.C:
			result.Status, result.Reason = "unknown", "the machine did not confirm; the change may have started. Inspect its reported context before running a recovery command"
		case <-r.Context().Done():
			result.Status, result.Reason = "unknown", "the response was interrupted; inspect the machine before running a recovery command"
		}
		timer.Stop()
	}
	g.mu.Lock()
	delete(g.startupChanges, id)
	_ = g.commit(change{audit: []AuditEvent{g.auditEventLocked(r, "", "machine.startup", n.Name, n.ID, q.Mode+": "+result.Status)}})
	g.mu.Unlock()
	consoleJSON(w, result)
}

func (g *Gateway) receiveStartup(n model.Node, data []byte) bool {
	if len(data) > 1024 {
		return false
	}
	var reply model.StartupReply
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&reply) != nil || decoder.Decode(new(any)) != io.EOF || len(reply.Reason) > 512 || (reply.Status != "manual" && reply.Status != "accepted") {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	pending, ok := g.startupChanges[reply.ID]
	if !ok {
		return true
	}
	if pending.node != n.ID {
		return false
	}
	select {
	case pending.reply <- reply:
	default:
	}
	return true
}
