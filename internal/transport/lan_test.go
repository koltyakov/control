package transport

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func TestLANProofRequiresSignedBroadcast(t *testing.T) {
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := model.LANProbe{Receiver: "receiver", Sender: id.ID, Challenge: identity.NewID(), Port: 40000,
		ReceiverNIC: model.WakeInterface{Address: "192.168.1.2/24", MAC: "02:00:00:00:00:02"},
		SenderNIC:   model.WakeInterface{Address: "192.168.1.3/24", MAC: "02:00:00:00:00:03"}}
	proof := model.LANProof{LANProbe: request, Nonce: identity.NewID(), IssuedAt: now}
	proof.Signature = ed25519.Sign(id.Private, proof.Message())
	if !validLANProof(proof, request, id.Public, "192.168.1.3", now) {
		t.Fatal("valid broadcast proof rejected")
	}
	for _, mutate := range []func(*model.LANProof){
		func(p *model.LANProof) { p.Challenge = identity.NewID() },
		func(p *model.LANProof) { p.Receiver = "other" },
		func(p *model.LANProof) { p.Port++ },
		func(p *model.LANProof) { p.Nonce = "bad" },
		func(p *model.LANProof) { p.Signature = nil },
		func(p *model.LANProof) {
			p.IssuedAt = now.Add(-2 * time.Minute)
			p.Signature = ed25519.Sign(id.Private, p.Message())
		},
		func(p *model.LANProof) {
			p.IssuedAt = now.Add(time.Minute)
			p.Signature = ed25519.Sign(id.Private, p.Message())
		},
	} {
		changed := proof
		mutate(&changed)
		if validLANProof(changed, request, id.Public, "192.168.1.3", now) {
			t.Fatal("forged or stale proof accepted", changed)
		}
	}
	if validLANProof(proof, request, id.Public, "192.168.1.4", now) || validLANProof(proof, request, nil, "192.168.1.3", now) {
		t.Fatal("wrong source or key accepted")
	}
}

func TestLANCandidatesDoNotAssumeSubnetIsEvidence(t *testing.T) {
	now := time.Now()
	a := model.WakeInterface{Address: "192.168.1.2/24", MAC: "02:00:00:00:00:02"}
	b := model.WakeInterface{Address: "192.168.1.3/24", MAC: "02:00:00:00:00:03"}
	self := model.Node{ID: "self", WakeInterfaces: []model.WakeInterface{a}}
	other := model.Node{ID: "other", Online: true, LANDiscovery: true, WakeInterfaces: []model.WakeInterface{b}}
	nodes := []model.Node{self, other}
	if got := lanCandidates(self.ID, self.WakeInterfaces, nodes, now); len(got) != 1 {
		t.Fatal("automatic candidate needs no labels", got)
	}
	self.LANPeers = []model.LANPeer{{ID: other.ID, Local: a, Remote: b, VerifiedAt: now}}
	nodes[0] = self
	if got := lanCandidates(self.ID, self.WakeInterfaces, nodes, now); len(got) != 0 {
		t.Fatal("fresh evidence reprobed", got)
	}
	nodes[0].LANPeers = nil
	for _, change := range []func(*model.Node){
		func(n *model.Node) { n.Online = false },
		func(n *model.Node) { n.LANDiscovery = false },
		func(n *model.Node) { n.WakeInterfaces = []model.WakeInterface{a} },
		func(n *model.Node) { n.WakeInterfaces = []model.WakeInterface{{Address: "192.168.2.3/24", MAC: b.MAC}} },
	} {
		n := other
		change(&n)
		nodes[1] = n
		if got := lanCandidates(self.ID, self.WakeInterfaces, nodes, now); len(got) != 0 {
			t.Fatal("incompatible candidate", got)
		}
	}
}
