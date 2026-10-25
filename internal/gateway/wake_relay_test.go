package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"google.golang.org/protobuf/proto"
)

var (
	wakeRemote = model.WakeInterface{Address: "192.168.4.10/24", MAC: "02:ab:cd:ef:01:23"}
	wakeLocal  = model.WakeInterface{Address: "192.168.4.2/24", MAC: "02:00:00:00:00:01"}
)

func sleepingTarget(userID string, verified ...string) model.Node {
	target := model.Node{ID: "sleeper-id", Name: "sleeper", UserID: userID, WakeInterfaces: []model.WakeInterface{wakeRemote}}
	for _, id := range verified {
		target.LANPeers = append(target.LANPeers, model.LANPeer{ID: id, Local: wakeRemote, Remote: wakeLocal, VerifiedAt: time.Now().Add(-time.Second)})
	}
	return target
}

func TestWakeSenderSelection(t *testing.T) {
	sender := model.Node{ID: "s-beta", Name: "beta", UserID: "u", Online: true, GatewayWake: true, WakeInterfaces: []model.WakeInterface{wakeLocal}}
	target := sleepingTarget("u", "s-beta", "s-alpha")
	if got, ok := wakeSender([]model.Node{sender, target}, target); !ok || got.ID != sender.ID {
		t.Fatalf("selected %+v, %v", got, ok)
	}
	alpha := sender
	alpha.ID, alpha.Name = "s-alpha", "alpha"
	if got, _ := wakeSender([]model.Node{sender, alpha, target}, target); got.ID != alpha.ID {
		t.Fatal("selection is not alphabetical", got.Name)
	}
	for name, change := range map[string]func(s, t *model.Node){
		"sender offline":        func(s, _ *model.Node) { s.Online = false },
		"sender disabled":       func(s, _ *model.Node) { s.Disabled = true },
		"sender policy pending": func(s, _ *model.Node) { s.ControlPending = true },
		"sender not opted in":   func(s, _ *model.Node) { s.GatewayWake = false },
		"other fleet":           func(s, _ *model.Node) { s.UserID = "other" },
		"target online":         func(_, t *model.Node) { t.Online = true },
		"no verified LAN":       func(_, t *model.Node) { t.LANPeers = nil },
	} {
		s, tt := sender, sleepingTarget("u", "s-beta")
		change(&s, &tt)
		if got, ok := wakeSender([]model.Node{s, tt}, tt); ok {
			t.Fatalf("%s: selected %s", name, got.Name)
		}
	}
}

func TestConsoleWakeRelaysOnceThroughTheSender(t *testing.T) {
	g, s := installationFixture(t)
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := rawPeer(t, s.URL, commonKey, "sender", legacyUser, id)
	if ws == nil {
		t.Fatal("sender registration failed")
	}
	g.mu.Lock()
	sender := g.nodes[id.ID]
	sender.GatewayWake, sender.WakeInterfaces = true, []model.WakeInterface{wakeLocal}
	g.nodes[id.ID] = sender
	g.nodes["sleeper-id"] = sleepingTarget(legacyUser, id.ID)
	g.publishObservations(id.ID, "sleeper-id")
	g.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	operator := testAdmin{URL: s.URL, Key: superKey}
	var pool model.PoolActivitySnapshot
	if err := operator.JSON(ctx, "GET", "/v1/status", nil, &pool); err != nil {
		t.Fatal(err)
	}
	for _, n := range pool.Nodes {
		if n.ID == "sleeper-id" && n.WakeVia != "sender" {
			t.Fatalf("status wakeVia = %q", n.WakeVia)
		}
	}

	// The fake sender answers each request once: first success, then failure.
	answers := []wakeReply{{Packets: 1}, {Error: "no verified LAN relationship for these live interfaces"}}
	go func() {
		for _, answer := range answers {
			for {
				_, data, err := ws.Read(ctx)
				if err != nil {
					return
				}
				var p protocol.Packet
				if proto.Unmarshal(data, &p) != nil || p.Kind != "wake.request" {
					continue
				}
				var request wakeRequest
				if json.Unmarshal(p.Data, &request) != nil || request.Target != "sleeper-id" {
					return
				}
				answer.ID = request.ID
				reply, _ := proto.Marshal(&protocol.Packet{Kind: "wake.result", Data: model.JSON(answer)})
				_ = ws.Write(ctx, websocket.MessageBinary, reply)
				break
			}
		}
	}()

	var result struct {
		Source  string
		Packets int
	}
	if err := operator.JSON(ctx, "POST", "/v1/fleet/nodes/sleeper-id/wake", nil, &result); err != nil || result.Source != "sender" || result.Packets != 1 {
		t.Fatalf("wake result %+v: %v", result, err)
	}
	if err := operator.JSON(ctx, "POST", "/v1/fleet/nodes/sleeper-id/wake", nil, nil); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("sender failure not reported: %v", err)
	}
	if err := operator.JSON(ctx, "POST", "/v1/fleet/nodes/"+id.ID+"/wake", nil, nil); err == nil {
		t.Fatal("woke an online machine")
	}
	_, alice := createTestUser(t, s, "alice")
	req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/v1/fleet/nodes/sleeper-id/wake", nil)
	req.Header.Set("Authorization", "Bearer "+alice.Key)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatal("another fleet reached the machine", err)
	} else {
		_ = resp.Body.Close()
	}
	got := readAudit(t, operator, "category=machine")
	if len(got.Events) < 2 || got.Events[0].Action != "machine.wake" || !strings.Contains(got.Events[1].Detail, "sent 1 packet") || !strings.Contains(got.Events[0].Detail, "no verified LAN") {
		t.Fatalf("wake attempts not audited: %+v", got.Events)
	}
}
