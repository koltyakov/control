package gateway

import (
	"errors"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

func TestGatewaySwitchesBeforeCommittingNodeUpdates(t *testing.T) {
	g, d, info := deferralFixture(t)
	delete(g.peers, "offline")
	d.Deferred = []string{"offline"}
	status := g.updateStatus["online"]
	status.Software.Version = "older"
	status.Software.SHA256 = "older"
	status.Paused, status.State, status.LeaseUntil = true, "ready", time.Now().Add(time.Minute)
	g.updateStatus["online"] = status
	peers := g.rolloutPeers(d)
	var switches, commits int
	g.options.Apply = func(path string) error {
		if path != "verified-gateway" || commits != 0 {
			t.Fatal("nodes switched before gateway", path, commits)
		}
		switches++
		return nil
	}
	send := func(id, kind string, command update.Command) {
		if kind != "update.commit" || id != "online" || command.ID != d.ID {
			t.Fatal("unexpected commit", id, kind, command)
		}
		commits++
	}
	requested := g.installReady(d, peers, "verified-gateway", false, send)
	if !requested || switches != 1 || commits != 0 {
		t.Fatal("gateway did not switch first", requested, switches, commits)
	}
	if !g.installReady(d, peers, "verified-gateway", requested, send) || switches != 1 || commits != 0 {
		t.Fatal("gateway restart request was repeated or nodes started too early")
	}
	// The restarted gateway sees its binary already selected. Node reports may
	// still show their previous software, but idle reservations remain required.
	g.options.Software = info
	g.restarting = false
	if g.installReady(d, peers, "", false, send) || commits != 1 || switches != 1 {
		t.Fatal("selected gateway did not proceed with node updates")
	}
	// A newly connected node not in the reserved snapshot prevents the switch.
	g.peers["newcomer"] = &connection{out: make(chan *protocol.Packet, 1)}
	if g.installReady(d, peers, "verified-gateway", false, send) || switches != 1 {
		t.Fatal("gateway ignored changed live membership")
	}
}

func TestFailedGatewaySwitchNeverCommitsNodes(t *testing.T) {
	g, d, _ := deferralFixture(t)
	d.Deferred = []string{"offline"}
	status := g.updateStatus["online"]
	status.Paused, status.LeaseUntil = true, time.Now().Add(time.Minute)
	g.updateStatus["online"] = status
	g.options.Apply = func(string) error { return errors.New("fixture switch failed") }
	if g.installReady(d, g.rolloutPeers(d), "verified-gateway", false, func(string, string, update.Command) {
		t.Fatal("node commit escaped failed gateway switch")
	}) || g.restarting || g.updates.Current().Phase != "error" {
		t.Fatal("failed gateway switch was acknowledged")
	}
}
