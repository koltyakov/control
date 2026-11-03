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
	"github.com/koltyakov/control/internal/secrets"
	"github.com/koltyakov/control/internal/update"
)

type pendingSecrets struct {
	node, operation string
	peer            *connection
	reply           chan secrets.ManagementReply
}

func (g *Gateway) secretRoutes(mux *http.ServeMux) {
	for _, route := range []string{
		"GET /v1/fleet/nodes/{id}/secrets",
		"PUT /v1/fleet/nodes/{id}/secrets/{name}",
		"DELETE /v1/fleet/nodes/{id}/secrets/{name}",
	} {
		mux.Handle(route, g.consoleHeaders(http.HandlerFunc(g.fleetAdmin(g.manageSecrets))))
	}
}

func (g *Gateway) manageSecrets(w http.ResponseWriter, r *http.Request) {
	q := secrets.ManagementRequest{ID: identity.NewID(), ExpiresAt: time.Now().Add(30 * time.Second).Unix(), Name: r.PathValue("name")}
	switch r.Method {
	case http.MethodGet:
		q.Operation = "list"
	case http.MethodDelete:
		q.Operation = "delete"
	case http.MethodPut:
		q.Operation = "set"
		var envelope secrets.Envelope
		r.Body = http.MaxBytesReader(w, r.Body, secrets.MaxRemoteMessage)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&envelope) != nil || decoder.Decode(new(any)) != io.EOF {
			http.Error(w, "expected an encrypted secret envelope; plaintext values are not accepted", http.StatusBadRequest)
			return
		}
		q.ID, q.ExpiresAt, q.Envelope = envelope.ID, envelope.ExpiresAt, &envelope
	}
	if !q.Valid(time.Now()) {
		http.Error(w, "invalid or expired secret management request", http.StatusBadRequest)
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
	peer := g.peers[n.ID]
	if !n.Online || peer == nil || n.Disabled || g.machinePending(n.ID) || !secrets.ValidManagementKey(n.SecretManagementKey) {
		g.mu.Unlock()
		http.Error(w, "machine must be online, enabled, and support secrets.manage; update its node and check allow rules", http.StatusConflict)
		return
	}
	for _, pending := range g.secretRequests {
		if pending.node == n.ID {
			g.mu.Unlock()
			http.Error(w, "secret management is already in progress for this machine; do not retry an uncertain write", http.StatusConflict)
			return
		}
	}
	if len(g.secretRequests) >= 64 {
		g.mu.Unlock()
		http.Error(w, "too many secret management requests", http.StatusTooManyRequests)
		return
	}
	if _, exists := g.secretRequests[q.ID]; exists {
		g.mu.Unlock()
		http.Error(w, "secret management request ID already in use", http.StatusConflict)
		return
	}
	reply := make(chan secrets.ManagementReply, 1)
	g.secretRequests[q.ID] = pendingSecrets{node: n.ID, operation: q.Operation, peer: peer, reply: reply}
	if q.Operation != "list" {
		// Neither names nor ciphertext enter audit records, including when a
		// caller accidentally supplies a credential as its secret name.
		err := g.commit(change{audit: []AuditEvent{g.auditEventLocked(r, "", "machine.secrets."+q.Operation, n.Name, n.ID, "requested")}})
		if err != nil {
			delete(g.secretRequests, q.ID)
			g.mu.Unlock()
			http.Error(w, "could not record secret management intent; nothing was sent", http.StatusInternalServerError)
			return
		}
	}
	sent := peer.enqueue(&protocol.Packet{Kind: "secrets.request", From: update.GatewaySender, Data: model.JSON(q)})
	g.mu.Unlock()
	defer func() { g.mu.Lock(); delete(g.secretRequests, q.ID); g.mu.Unlock() }()
	if !sent {
		http.Error(w, "machine connection is busy; nothing was sent", http.StatusServiceUnavailable)
		return
	}
	var result secrets.ManagementReply
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case result = <-reply:
	case <-timer.C:
		http.Error(w, "machine did not confirm; the operation may have completed. Inspect metadata before trying again", http.StatusGatewayTimeout)
		return
	case <-r.Context().Done():
		return // Delivery is uncertain. Never resubmit automatically.
	}
	if q.Operation != "list" {
		outcome := "completed"
		if result.Error != "" {
			outcome = "failed"
		}
		g.mu.Lock()
		_ = g.commit(change{audit: []AuditEvent{g.auditEventLocked(r, "", "machine.secrets."+q.Operation, n.Name, n.ID, outcome)}})
		g.mu.Unlock()
	}
	if result.Error != "" {
		// Only server-owned diagnostics are returned, never node-supplied text.
		http.Error(w, "machine refused secret management or could not save its store; check permissions, availability, and metadata before retrying", http.StatusBadGateway)
		return
	}
	if q.Operation == "list" {
		entries := result.Secrets
		if entries == nil {
			entries = []secrets.Metadata{}
		}
		consoleJSON(w, secrets.RemoteList{Key: n.SecretManagementKey, Secrets: entries})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) receiveSecrets(n model.Node, peer *connection, data []byte) bool {
	if len(data) > 64<<10 {
		return false
	}
	var reply secrets.ManagementReply
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&reply) != nil || decoder.Decode(new(any)) != io.EOF || !secrets.ValidRequestID(reply.ID) || len(reply.Secrets) > 128 {
		return false
	}
	switch reply.Error {
	case "", "denied", "busy", "invalid", "storage", "replayed":
	default:
		return false
	}
	seen := map[string]bool{}
	for _, entry := range reply.Secrets {
		if !secrets.ValidName(entry.Name) || seen[entry.Name] {
			return false
		}
		seen[entry.Name] = true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	pending, ok := g.secretRequests[reply.ID]
	if !ok {
		return true
	}
	if pending.node != n.ID || pending.peer != peer || g.peers[n.ID] != peer || (pending.operation != "list" && len(reply.Secrets) != 0) {
		return false
	}
	select {
	case pending.reply <- reply:
	default:
	}
	return true
}
