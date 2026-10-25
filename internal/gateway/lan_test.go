package gateway

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func TestLANReportsPersistOnlyVerifiedFleetRelationships(t *testing.T) {
	dir := t.TempDir()
	g, server := gatewayAt(t, dir)
	t.Cleanup(func() { server.Close(); _ = g.Close() })
	user, admin := createTestUser(t, server, "lan-owner")
	otherUser, foreign := createTestUser(t, server, "other-owner")
	a, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	first := rawPeer(t, server.URL, admin.Key, "first", user.ID, a)
	second := rawPeer(t, server.URL, admin.Key, "second", user.ID, b)
	if first == nil || second == nil {
		t.Fatal("registration failed")
	}
	c, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	third := rawPeer(t, server.URL, foreign.Key, "foreign", otherUser.ID, c)
	if third == nil {
		t.Fatal("foreign registration failed")
	}
	x := model.WakeInterface{Address: "192.168.1.2/24", MAC: "02:00:00:00:00:02"}
	y := model.WakeInterface{Address: "192.168.1.3/24", MAC: "02:00:00:00:00:03"}
	g.mu.Lock()
	for id, nic := range map[string]model.WakeInterface{a.ID: x, b.ID: y, c.ID: y} {
		n := g.nodes[id]
		n.WakeInterfaces = []model.WakeInterface{nic}
		n.LANDiscovery = true
		g.nodes[id] = n
	}
	err = g.commit(change{nodes: []string{a.ID, b.ID, c.ID}})
	receiver, conn := g.nodes[a.ID], g.peers[a.ID]
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	proof := model.LANProof{LANProbe: model.LANProbe{Receiver: a.ID, Sender: b.ID, ReceiverNIC: x, SenderNIC: y, Port: 40000, Challenge: identity.NewID()}, Nonce: identity.NewID(), IssuedAt: time.Now().UTC()}
	proof.Signature = ed25519.Sign(b.Private, proof.Message())
	foreignProof := proof
	foreignProof.Sender = c.ID
	foreignProof.Signature = ed25519.Sign(c.Private, foreignProof.Message())
	if !g.receiveLANReport(receiver, conn, model.JSON(foreignProof)) {
		t.Fatal("foreign report should be dropped")
	}
	g.mu.Lock()
	foreignAccepted := len(g.nodes[a.ID].LANPeers) != 0
	g.mu.Unlock()
	if foreignAccepted {
		t.Fatal("signed foreign witness created a LAN relationship")
	}
	bad := proof
	bad.Signature = nil
	if g.receiveLANReport(receiver, conn, model.JSON(bad)) {
		t.Fatal("unsigned claim accepted")
	}
	if !g.receiveLANReport(receiver, conn, model.JSON(proof)) {
		t.Fatal("valid proof rejected")
	}
	g.mu.Lock()
	peers := g.nodes[a.ID].LANPeers
	g.mu.Unlock()
	if len(peers) != 1 || peers[0].ID != b.ID || peers[0].Local != x || peers[0].Remote != y {
		t.Fatal(peers)
	}
	if !g.receiveLANReport(receiver, conn, model.JSON(proof)) {
		t.Fatal("duplicate proof rejected")
	}
	var nodes []model.Node
	if err := foreign.JSON(context.Background(), "GET", "/v1/nodes", nil, &nodes); err != nil || len(nodes) != 1 || nodes[0].ID != c.ID || len(nodes[0].LANPeers) != 0 {
		t.Fatal("LAN relationship leaked", nodes, err)
	}
	// A failed transaction must leave the prior committed evidence untouched.
	g.mu.Lock()
	n := g.nodes[a.ID]
	n.LANPeers = []model.LANPeer{peers[0]}
	n.LANPeers[0].VerifiedAt = time.Now().Add(-20 * time.Minute)
	g.nodes[a.ID] = n
	err = g.commit(change{nodes: []string{a.ID}})
	before := g.nodes[a.ID].LANPeers[0].VerifiedAt
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	if !g.receiveLANReport(receiver, conn, model.JSON(proof)) {
		t.Fatal("failed commit disconnected node")
	}
	if _, err := g.db.Exec("PRAGMA query_only=OFF"); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	after := g.nodes[a.ID].LANPeers[0].VerifiedAt
	g.mu.Unlock()
	if !before.Equal(after) {
		t.Fatal("failed commit changed evidence")
	}
	_ = first.CloseNow()
	_ = second.CloseNow()
	_ = third.CloseNow()
	waitSessionClosed(t, g, a.ID)
	waitSessionClosed(t, g, b.ID)
	server.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g, server = gatewayAt(t, dir)
	admin.URL = server.URL
	if err := admin.JSON(context.Background(), "GET", "/v1/nodes", nil, &nodes); err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == a.ID && len(n.LANPeers) == 1 && n.LANPeers[0].ID == b.ID {
			return
		}
	}
	t.Fatal("offline LAN relationship lost after restart", nodes)
}

func TestLANRelationshipsExpireAndBindInterfaces(t *testing.T) {
	nic := model.WakeInterface{Address: "192.168.1.2/24", MAC: "02:00:00:00:00:02"}
	peer := model.LANPeer{ID: "peer", Local: nic, VerifiedAt: time.Now().Add(-time.Second)}
	if len(retainedLANPeers([]model.LANPeer{peer}, []model.WakeInterface{nic})) != 1 {
		t.Fatal("current relationship lost")
	}
	changed := nic
	changed.MAC = "02:00:00:00:00:03"
	if len(retainedLANPeers([]model.LANPeer{peer}, []model.WakeInterface{changed})) != 0 {
		t.Fatal("changed interface retained evidence")
	}
	peer.VerifiedAt = time.Now().Add(-model.LANPeerTTL)
	if len(retainedLANPeers([]model.LANPeer{peer}, []model.WakeInterface{nic})) != 0 {
		t.Fatal("expired evidence retained")
	}
}
