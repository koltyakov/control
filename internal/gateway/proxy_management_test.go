package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"google.golang.org/protobuf/proto"
)

func TestOwnerProxyManagementIsolationAppliedReplyAndUncertainty(t *testing.T) {
	g, server := installationFixture(t)
	id, _ := identity.Load(t.TempDir())
	ws := rawPeer(t, server.URL, commonKey, "proxy-worker", legacyUser, id)
	if ws == nil {
		t.Fatal("registration failed")
	}
	admin := testAdmin{URL: server.URL, Key: superKey}
	provisionBrowser(t, admin, "")
	browser := browserLogin(t, server.URL, "@operator", browserPassword)
	route := "/console/api/fleet/nodes/" + id.ID + "/proxy"
	data, _ := browser.request(t, "GET", route, nil, 200)
	var state model.ProxyConfiguration
	if json.Unmarshal(data, &state) != nil || state.Supported {
		t.Fatal("old node reported supported")
	}
	browser.request(t, "PUT", route, model.ProxyConfigure{}, 409)
	g.mu.Lock()
	n := g.nodes[id.ID]
	n.GatewayProxyManagement = true
	g.nodes[id.ID] = n
	g.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var drop atomic.Bool
	var mutations atomic.Int32
	applied := make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		config := model.ProxyConfiguration{Supported: true, Interfaces: []model.ProxyInterface{{Name: "ethernet", Address: "10.0.0.5", Prefix: "10.0.0.0/24"}}}
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var packet protocol.Packet
			if proto.Unmarshal(data, &packet) != nil || packet.Kind != "proxy.request" {
				continue
			}
			var q model.ProxyManagementRequest
			if json.Unmarshal(packet.Data, &q) != nil {
				return
			}
			if q.Configure != nil {
				mutations.Add(1)
				config.Enabled = q.Configure.Enabled
				if config.Enabled {
					s := q.Configure
					config.Settings = &model.ProxySettings{Interface: s.Interface, Address: s.Address, Port: s.Port, Sources: s.Sources}
				}
				applied <- struct{}{}
			}
			reply := model.ProxyManagementReply{ID: q.ID, Configuration: config}
			if config.Enabled {
				reply.Endpoints = []model.ProxyEndpoint{{URL: "http://10.0.0.5:8080", Interface: "ethernet", Managed: true}}
			}
			if q.Configure != nil && drop.Load() {
				continue
			}
			data, _ = proto.Marshal(&protocol.Packet{Kind: "proxy.result", Data: model.JSON(reply)})
			if ws.Write(ctx, websocket.MessageBinary, data) != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); _ = ws.CloseNow(); <-done })
	q := model.ProxyConfigure{Enabled: true, Interface: "ethernet", Address: "10.0.0.5", Port: 8080, Sources: []string{"10.0.0.0/24"}}
	bad := browser
	bad.CSRF = "invalid"
	bad.request(t, "PUT", route, q, 403)
	_, foreignAdmin := createTestUser(t, server, "foreign-proxy-owner")
	provisionBrowser(t, foreignAdmin, "")
	foreign := browserLogin(t, server.URL, "foreign-proxy-owner", browserPassword)
	foreign.request(t, "GET", route, nil, 404)
	foreign.request(t, "PUT", route, q, 404)
	common := testAdmin{URL: server.URL, Key: commonKey}
	if err := common.JSON(t.Context(), "GET", "/v1/fleet/nodes/"+id.ID+"/proxy", nil, nil); err == nil {
		t.Fatal("common key gained owner management")
	}
	browser.request(t, "PUT", route, map[string]any{"enabled": true, "command": "anything"}, 400)
	data, _ = browser.request(t, "PUT", route, q, 200)
	<-applied
	if json.Unmarshal(data, &state) != nil || !state.Enabled || state.Settings.Interface != "ethernet" {
		t.Fatal("PUT did not return worker-applied state")
	}
	g.mu.Lock()
	advertised := g.nodes[id.ID].ProxyEndpoints
	g.proxyDependencies["retired-child"] = proxyDependency{UserID: legacyUser, Worker: id.ID, URL: advertised[0].URL}
	g.machineStates["retired-child"] = model.MachineState{Unregistered: true}
	g.mu.Unlock()
	browser.request(t, "PUT", route, model.ProxyConfigure{}, 409)
	data, _ = browser.request(t, "GET", route, nil, 200)
	if json.Unmarshal(data, &state) != nil || !state.Enabled || state.Dependents.Retiring != 1 {
		t.Fatal("retiring dependency not preserved")
	}
	g.mu.Lock()
	delete(g.proxyDependencies, "retired-child")
	g.mu.Unlock()
	// The worker applies this change but loses its reply. HTTP cancellation
	// must not clear the durable fence or resubmit the mutation.
	drop.Store(true)
	c := testAdmin{URL: server.URL, Key: superKey}
	short, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err := c.JSON(short, "PUT", "/v1/fleet/nodes/"+id.ID+"/proxy", model.ProxyConfigure{}, nil)
	stop()
	if err == nil {
		t.Fatal("packet acceptance reported as completion")
	}
	<-applied
	waitForCleanup := func() {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for {
			g.mu.Lock()
			count := len(g.proxyRequests)
			g.mu.Unlock()
			if count == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("abandoned HTTP request retained a reply channel")
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitForCleanup()
	g.mu.Lock()
	fenced := g.nodes[id.ID].ProxyManagementPending
	_, _, code := g.proxySelectionLocked(legacyUser, id.ID, advertised[0].URL, time.Now())
	g.mu.Unlock()
	if !fenced || code != http.StatusConflict || mutations.Load() != 2 {
		t.Fatal("uncertain mutation lost fence or replayed")
	}
	// Uncertainty is in the committed node row, not only the in-memory
	// request channel, so restarting cannot discard it.
	var saved []byte
	if err := g.db.QueryRow("SELECT data FROM nodes WHERE id = ?", id.ID).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	var persisted model.Node
	if json.Unmarshal(saved, &persisted) != nil || !persisted.ProxyManagementPending {
		t.Fatal("mutation fence was not committed durably")
	}
	browser.request(t, "PUT", route, q, 409)
	data, _ = browser.request(t, "GET", route, nil, 200)
	if json.Unmarshal(data, &state) != nil || state.Enabled || mutations.Load() != 2 {
		t.Fatal("inspection failed to reconcile without replay")
	}
	g.mu.Lock()
	fenced = g.nodes[id.ID].ProxyManagementPending
	advertised = g.nodes[id.ID].ProxyEndpoints
	g.mu.Unlock()
	if fenced || len(advertised) != 0 {
		t.Fatal("confirmed inspection did not update advertisement and fence")
	}
	// More than the global map capacity can be abandoned over time without
	// retaining request channels. Each explicit read reconciles the durable
	// fence before the next owner mutation; no mutation is replayed.
	for range 65 {
		callCtx, cancelCall := context.WithCancel(t.Context())
		finished := make(chan error, 1)
		go func() {
			finished <- c.JSON(callCtx, "PUT", "/v1/fleet/nodes/"+id.ID+"/proxy", model.ProxyConfigure{}, nil)
		}()
		select {
		case <-applied:
		case err := <-finished:
			cancelCall()
			t.Fatalf("abandoned requests exhausted admission: %v", err)
		case <-time.After(5 * time.Second):
			cancelCall()
			t.Fatal("worker did not receive request")
		}
		cancelCall()
		if err := <-finished; err == nil {
			t.Fatal("abandoned mutation reported success")
		}
		waitForCleanup()
		g.mu.Lock()
		fenced = g.nodes[id.ID].ProxyManagementPending
		g.mu.Unlock()
		if !fenced {
			t.Fatal("request cleanup cleared durable uncertainty")
		}
		browser.request(t, "GET", route, nil, 200)
	}
	if mutations.Load() != 67 {
		t.Fatal("request cleanup replayed a mutation")
	}
}

func TestProxyPendingProjectionExcludesOnlyManagedEndpoints(t *testing.T) {
	now := time.Now()
	managed, external := "http://10.0.0.5:8080", "http://external.example:8080"
	entry := observation{
		node:     model.Node{Online: true, ProxyManagementPending: true, ProxyEndpoints: []model.ProxyEndpoint{{URL: managed, Managed: true}, {URL: external}}},
		health:   &model.NodeHealth{Proxies: []model.ProxyReadiness{{URL: managed, State: model.ProxyReady, CheckedAt: now}, {URL: external, State: model.ProxyReady, CheckedAt: now}}},
		healthAt: now,
	}
	status := proxyStatuses(entry, now, nil)
	if status[0].Selectable() || status[0].State != model.ProxyUnavailable || status[0].Error == "" {
		t.Fatal("uncertain managed listener was projected selectable")
	}
	if !status[1].Selectable() {
		t.Fatal("uncertain managed settings affected external proxy")
	}
}

func TestProxyManagementFailedIntentCommitSendsNothing(t *testing.T) {
	g, server := installationFixture(t)
	id, _ := identity.Load(t.TempDir())
	ws := rawPeer(t, server.URL, commonKey, "proxy-worker", legacyUser, id)
	if ws == nil {
		t.Fatal("registration failed")
	}
	g.mu.Lock()
	n := g.nodes[id.ID]
	n.GatewayProxyManagement = true
	g.nodes[id.ID] = n
	if err := g.db.Close(); err != nil {
		g.mu.Unlock()
		t.Fatal(err)
	}
	g.mu.Unlock()
	admin := testAdmin{URL: server.URL, Key: superKey}
	if err := admin.JSON(t.Context(), "PUT", "/v1/fleet/nodes/"+id.ID+"/proxy", model.ProxyConfigure{}, nil); err == nil {
		t.Fatal("intent commit failure reported success")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.proxyRequests) != 0 || g.nodes[id.ID].ProxyManagementPending {
		t.Fatal("failed intent was dispatched or left uncommitted state")
	}
}
