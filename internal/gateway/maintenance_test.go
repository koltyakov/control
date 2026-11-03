package gateway

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
)

func TestGatewayRelayLifetimeAndRestartFence(t *testing.T) {
	a, b := &connection{}, &connection{}
	g := &Gateway{}
	packet := &protocol.Packet{Kind: "open", To: "b", Session: "session"}
	if !g.admitRelay("a", a, b, packet) || g.reserveRestart(1) {
		t.Fatal("pending setup did not block restart")
	}
	if !g.admitRelay("b", b, a, &protocol.Packet{Kind: "answer", To: "a", Session: packet.Session}) {
		t.Fatal("answer rejected")
	}
	if !g.admitRelay("a", a, b, &protocol.Packet{Kind: "close", To: "b", Session: packet.Session}) || g.reserveRestart(1) {
		t.Fatal("one-sided close lost receiver lifetime")
	}
	if !g.admitRelay("b", b, a, &protocol.Packet{Kind: "close", To: "a", Session: packet.Session}) {
		t.Fatal("receiver close rejected")
	}
	if !g.reserveRestart(1) {
		t.Fatal("closed carrier blocked restart")
	}
	if g.admitRelay("a", a, b, packet) {
		t.Fatal("new relay passed final restart fence")
	}
	if len(g.maintenance.relays) != 0 || len(g.maintenance.counts) != 0 {
		t.Fatal("relay accounting leaked")
	}
}

func TestGatewayDrainTimeoutNeverDisconnectsCarriers(t *testing.T) {
	aware := &connection{relayDrain: true, out: make(chan *protocol.Packet, 8)}
	legacy := &connection{out: make(chan *protocol.Packet, 8)}
	g := &Gateway{peers: map[string]*connection{"aware": aware, "legacy": legacy}}
	packet := &protocol.Packet{Kind: "open", To: "legacy", Session: "active"}
	if !g.admitRelay("aware", aware, legacy, packet) {
		t.Fatal("open failed")
	}
	now := time.Now()
	if !g.drainGateway(now) || len(aware.out) != 1 || len(legacy.out) != 0 {
		t.Fatal("drain negotiation failed")
	}
	if !g.drainGateway(now.Add(time.Second)) || len(aware.out) != 1 {
		t.Fatal("drain hints flooded queues")
	}
	if g.admitRelay("aware", aware, legacy, &protocol.Packet{Kind: "open", To: "legacy", Session: "new"}) {
		t.Fatal("new carrier admitted during drain")
	}
	if !g.admitRelay("aware", aware, legacy, &protocol.Packet{Kind: "data", To: "legacy", Session: "active", Data: []byte("accepted work")}) {
		t.Fatal("drain interrupted existing data")
	}
	if g.drainGateway(now.Add(gatewayDrainWindow)) || g.reserveRestart(1) {
		t.Fatal("timeout forced a restart")
	}
	if len(g.maintenance.relays) != 1 || len(aware.out) != 2 {
		t.Fatal("timeout discarded carrier or omitted resume")
	}
	if !g.admitRelay("aware", aware, legacy, &protocol.Packet{Kind: "open", To: "legacy", Session: "new"}) {
		t.Fatal("timeout did not restore admission")
	}
	g.forgetRelays(aware)
	if len(g.maintenance.relays) != 0 || len(g.maintenance.counts) != 0 {
		t.Fatal("disconnect leaked carriers")
	}
}

func TestFailedSetupAndConnectionGenerationCleanup(t *testing.T) {
	a, b := &connection{}, &connection{}
	g := &Gateway{routes: map[string]*connection{"a": a, "b": b}}
	p := &protocol.Packet{Kind: "open", To: "b", Session: "failed"}
	if !g.admitRelay("a", a, b, p) {
		t.Fatal("open failed")
	}
	p.Kind = "close"
	if !g.admitRelay("a", a, b, p) || len(g.maintenance.relays) != 0 {
		t.Fatal("unanswered setup leaked")
	}
	old := b
	b = &connection{}
	g.routes["b"] = b
	p.Kind = "open"
	if g.admitRelay("a", a, old, p) {
		t.Fatal("stale route created immortal carrier")
	}
	if !g.admitRelay("a", a, b, p) {
		t.Fatal("new connection rejected")
	}
	g.forgetRelays(old)
	if len(g.maintenance.relays) != 1 {
		t.Fatal("old disconnect erased a new carrier")
	}
}

func TestGatewayRestartProtectsHTTPAndRestoresAdmission(t *testing.T) {
	for _, path := range []string{"/v1/updates/blobs/digest", "/install/ticket/binary"} {
		t.Run(path, func(t *testing.T) {
			g := &Gateway{}
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			h := g.maintenanceHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release }))
			go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil)) }()
			<-started
			blocked := !g.reserveRestart(1)
			close(release)
			<-done
			if !blocked || !g.reserveRestart(1) {
				t.Fatal("download lifetime not respected")
			}
			r := httptest.NewRecorder()
			h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
			if r.Code != http.StatusServiceUnavailable {
				t.Fatal("request passed restart fence")
			}
			g.releaseRestart()
			if g.restarting || g.maintenance.switching {
				t.Fatal("failed switch retained admission fence")
			}
		})
	}
}

func TestGatewayRestartSerializesWithRelayOpen(t *testing.T) {
	for range 100 {
		g := &Gateway{}
		a, b := &connection{}, &connection{}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var opened, reserved bool
		wg.Go(func() {
			<-start
			opened = g.admitRelay("a", a, b, &protocol.Packet{Kind: "open", To: "b", Session: "racing"})
		})
		wg.Go(func() { <-start; reserved = g.reserveRestart(1) })
		close(start)
		wg.Wait()
		if opened == reserved {
			t.Fatal("relay open and restart did not share an atomic fence", opened, reserved)
		}
	}
}

func TestGatewayRestartRejectsIncompatibleRunningNode(t *testing.T) {
	g := &Gateway{peers: map[string]*connection{"node": {}}, nodes: map[string]model.Node{"node": {Software: buildinfo.Info{NodeGatewayMax: 1}}}}
	if g.reserveRestart(2) {
		t.Fatal("gateway stranded an incompatible node")
	}
	if !g.reserveRestart(1) {
		t.Fatal("compatible busy membership blocked gateway")
	}
}

func TestGatewayRestartProtectsPacketWritesAndBoundsCarrierTracking(t *testing.T) {
	g := &Gateway{admission: newAdmission(DefaultLimits())}
	g.admission.packets.used.Store(128) // Retained until an in-progress write completes.
	if g.reserveRestart(1) {
		t.Fatal("in-progress packet write passed restart fence")
	}
	g.admission.packets.used.Store(0)
	a, b := &connection{}, &connection{}
	for i := range 256 {
		if !g.admitRelay("a", a, b, &protocol.Packet{Kind: "open", To: "b", Session: strconv.Itoa(i)}) {
			t.Fatal("legal carrier limit rejected")
		}
	}
	if g.admitRelay("a", a, b, &protocol.Packet{Kind: "open", To: "b", Session: "overflow"}) {
		t.Fatal("unbounded carrier tracking")
	}
	g.forgetRelays(a)
	if !g.reserveRestart(1) {
		t.Fatal("released packet/carrier reservations still block restart")
	}
}
