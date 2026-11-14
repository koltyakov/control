package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

func directoryTestGateway() *Gateway {
	return &Gateway{
		token: commonKey, nodes: map[string]model.Node{}, owners: map[string]string{},
		observed: map[string]*observedFleet{}, peers: map[string]*connection{},
		updateStatus: map[string]update.Status{},
	}
}

func directoryRequest(g *Gateway, token, etag string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("If-None-Match", etag)
	w := httptest.NewRecorder()
	g.auth(g.list)(w, r)
	return w
}

func TestDirectoryValidatorsTrackOnlyPublishedNodes(t *testing.T) {
	g := directoryTestGateway()
	n := model.Node{ID: "worker", Name: "worker", UserID: legacyUser, Labels: map[string]string{"host": "one"}}
	g.nodes[n.ID], g.owners[n.ID] = n, legacyUser
	g.publishObservations(n.ID)
	first := directoryRequest(g, commonKey, "")
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || etag == "" || first.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("initial directory: %d %v", first.Code, first.Header())
	}
	g.peers[n.ID] = &connection{health: &model.NodeHealth{}, healthAt: time.Now()}
	g.updateStatus[n.ID] = update.Status{State: "staged", SeenAt: time.Now(), Busy: true}
	g.publishObservations(n.ID)
	for _, condition := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		w := directoryRequest(g, commonKey, condition)
		if w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("ETag") != etag {
			t.Fatalf("unchanged response: %d %s", w.Code, w.Body.String())
		}
	}
	// Even nested mutable model fields must invalidate after publication.
	n.Labels["host"] = "two"
	g.nodes[n.ID] = n
	g.publishObservations(n.ID)
	changed := directoryRequest(g, commonKey, etag)
	if changed.Code != http.StatusOK || changed.Header().Get("ETag") == etag {
		t.Fatal("nested directory change did not invalidate")
	}
	etag = changed.Header().Get("ETag")
	n.Online = true
	g.nodes[n.ID] = n
	// Pending mutations must not change the committed validator.
	if w := directoryRequest(g, commonKey, etag); w.Code != http.StatusNotModified {
		t.Fatal("unpublished mutation invalidated directory")
	}
	g.publishObservations(n.ID)
	if w := directoryRequest(g, commonKey, etag); w.Code != http.StatusOK {
		t.Fatal("presence change did not invalidate directory")
	}
	etag = directoryRequest(g, commonKey, "").Header().Get("ETag")
	delete(g.nodes, n.ID)
	g.publishObservations(n.ID)
	w := directoryRequest(g, commonKey, etag)
	if w.Code != http.StatusOK || w.Body.String() != "[]\n" || w.Header().Get("ETag") == etag {
		t.Fatal("removal did not publish empty directory", w)
	}
}

func TestDirectoryValidatorsAreFleetScopedAndReauthenticate(t *testing.T) {
	g := directoryTestGateway()
	index := map[string]principal{
		tokenHash("alice"): {Role: "common", UserID: "alice"},
		tokenHash("bob"):   {Role: "common", UserID: "bob"},
	}
	g.credentials.Store(&index)
	for _, user := range []string{"alice", "bob"} {
		g.nodes[user] = model.Node{ID: user, Name: user, UserID: user}
		g.owners[user] = user
		g.publishObservations(user)
	}
	etag := directoryRequest(g, "alice", "").Header().Get("ETag")
	bob := directoryRequest(g, "bob", etag)
	var nodes []model.Node
	if err := json.Unmarshal(bob.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	if bob.Code != http.StatusOK || len(nodes) != 1 || nodes[0].UserID != "bob" {
		t.Fatal("foreign validator reused", bob)
	}
	g.nodes["bob"] = model.Node{ID: "bob", Name: "renamed", UserID: "bob"}
	g.publishObservations("bob")
	if w := directoryRequest(g, "alice", etag); w.Code != http.StatusNotModified {
		t.Fatal("foreign mutation invalidated directory")
	}
	revoked := map[string]principal{}
	g.credentials.Store(&revoked)
	if w := directoryRequest(g, "alice", etag); w.Code != http.StatusUnauthorized {
		t.Fatal("validator bypassed credential revocation", w.Code)
	}
}

func TestDirectoryTelemetryPublishesWithoutChangingCommittedNodes(t *testing.T) {
	g := directoryTestGateway()
	repository, err := update.OpenRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g.updates = repository
	n := model.Node{ID: "worker", Name: "committed", UserID: legacyUser}
	c := &connection{}
	g.nodes[n.ID], g.owners[n.ID], g.peers[n.ID] = n, legacyUser, c
	g.publishObservations(n.ID)
	etag := directoryRequest(g, commonKey, "").Header().Get("ETag")
	// Telemetry must preserve the committed model even if mutation state differs.
	pending := n
	pending.Name = "unpublished"
	g.nodes[n.ID] = pending
	health := model.NodeHealth{ActiveCount: 3}
	if !g.receiveHealth(n, c, model.JSON(health)) {
		t.Fatal("health rejected")
	}
	if !g.receiveUpdateStatus(n, c, model.JSON(update.Status{State: "staged", Busy: true})) {
		t.Fatal("status rejected")
	}
	entry := g.observations(legacyUser)[0]
	if entry.health == nil || entry.health.ActiveCount != 3 || entry.healthAt.IsZero() ||
		entry.status.State != "staged" || !entry.status.Busy || entry.status.SeenAt.IsZero() {
		t.Fatal("telemetry did not publish", entry)
	}
	if entry.node.Name != "committed" {
		t.Fatal("telemetry published an uncommitted node")
	}
	if w := directoryRequest(g, commonKey, etag); w.Code != http.StatusNotModified {
		t.Fatal("telemetry changed the directory validator")
	}
	// The ordinary node publication still carries policy acknowledgement state.
	c.lifecycleRevision = 1
	g.publishObservations(n.ID)
	entry = g.observations(legacyUser)[0]
	if entry.node.Name != "unpublished" || !entry.node.ControlPending {
		t.Fatal("full publication lost node changes")
	}
	if w := directoryRequest(g, commonKey, etag); w.Code != http.StatusOK {
		t.Fatal("full publication failed to invalidate")
	}
}

func TestDirectoryWildcardMatchesNeverPublishedEmptyFleet(t *testing.T) {
	g := directoryTestGateway()
	w := directoryRequest(g, commonKey, "*")
	if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatal("empty directory exists for wildcard matching", w)
	}
	if w := directoryRequest(g, commonKey, `"unknown"`); w.Code != http.StatusOK || w.Body.String() != "[]\n" {
		t.Fatal("unknown validator matched empty directory", w)
	}
}

func TestDirectoryPreservesConsoleNoStore(t *testing.T) {
	g := directoryTestGateway()
	n := model.Node{ID: "worker", Name: "worker", UserID: legacyUser}
	g.nodes[n.ID], g.owners[n.ID] = n, legacyUser
	g.publishObservations(n.ID)
	etag := directoryRequest(g, commonKey, "").Header().Get("ETag")
	handler := g.consoleHeaders(g.auth(g.list))
	for _, condition := range []string{"", etag} {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/console/api/nodes", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Authorization", "Bearer "+commonKey)
		r.Header.Set("If-None-Match", condition)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		want := http.StatusOK
		if condition != "" {
			want = http.StatusNotModified
		}
		if w.Code != want || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("console directory response: %d %v", w.Code, w.Header())
		}
	}
}

func BenchmarkDirectoryTelemetryPublication(b *testing.B) {
	g := directoryTestGateway()
	n := model.Node{ID: "worker", Name: "worker", UserID: legacyUser}
	for i := range 24 {
		n.Capabilities = append(n.Capabilities, model.Capability{
			Name: fmt.Sprintf("capability.%d", i), Description: "Execute an authorized worker operation",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}}}`),
		})
	}
	g.nodes[n.ID], g.owners[n.ID] = n, legacyUser
	g.peers[n.ID] = &connection{health: &model.NodeHealth{ActiveCount: 3}, healthAt: time.Now()}
	g.publishObservations(n.ID)
	for _, telemetryOnly := range []bool{false, true} {
		b.Run(fmt.Sprintf("telemetryOnly=%t", telemetryOnly), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				g.mu.Lock()
				if telemetryOnly {
					g.publishTelemetry(n.ID)
				} else {
					g.publishObservations(n.ID)
				}
				g.mu.Unlock()
			}
		})
	}
}

func BenchmarkDirectoryPoll(b *testing.B) {
	for _, count := range []int{32, 256} {
		g := directoryTestGateway()
		for i := range count {
			id := fmt.Sprintf("worker-%04d", i)
			n := model.Node{ID: id, Name: id, UserID: legacyUser, Online: true,
				PublicKey: make([]byte, 32), OS: "linux", LANDiscovery: true,
				Labels:         map[string]string{"zone": "office", "role": "worker"},
				WakeInterfaces: []model.WakeInterface{{Address: "192.168.1.2/24", MAC: "02:00:00:00:00:02"}}}
			for j := range 24 {
				n.Capabilities = append(n.Capabilities, model.Capability{
					Name: fmt.Sprintf("capability.%d", j), Description: "Execute an authorized worker operation",
					InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}}}`),
				})
			}
			g.nodes[id], g.owners[id] = n, legacyUser
			g.publishObservations(id)
		}
		etag := directoryRequest(g, commonKey, "").Header().Get("ETag")
		for _, conditional := range []bool{false, true} {
			b.Run(fmt.Sprintf("nodes=%d/conditional=%t", count, conditional), func(b *testing.B) {
				r := httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
				r.Header.Set("Authorization", "Bearer "+commonKey)
				if conditional {
					r.Header.Set("If-None-Match", etag)
				}
				b.ReportAllocs()
				for b.Loop() {
					w := httptest.NewRecorder()
					g.auth(g.list)(w, r)
					if conditional && w.Code != http.StatusNotModified || !conditional && w.Code != http.StatusOK {
						b.Fatal(w.Code)
					}
				}
			})
		}
	}
}
