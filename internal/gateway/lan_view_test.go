package gateway

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestLANViewValidatorsSeparateTopologyAndReceiverProofs(t *testing.T) {
	g := directoryTestGateway()
	g.routes = map[string]*connection{}
	handler := g.Handler()
	add := func(id, user, address string) model.Node {
		n := model.Node{ID: id, UserID: user, Name: id, OS: "linux", PublicKey: make([]byte, 32),
			Online: true, LANDiscovery: true, WakeInterfaces: []model.WakeInterface{{Address: address, MAC: "02:00:00:00:00:02"}}}
		g.nodes[id], g.owners[id] = n, user
		g.routes[id] = &connection{userID: user, keyID: "bootstrap", lanViewSession: "fixture-" + id}
		g.publishObservations(id)
		return n
	}
	a := add("a", legacyUser, "192.168.1.2/24")
	b := add("b", legacyUser, "192.168.1.3/24")
	_ = add("foreign", "other", "192.168.1.4/24")
	old := time.Now().UTC().Add(-16 * time.Minute)
	publishProof := func(receiver, sender model.Node, at time.Time) {
		receiver.LANPeers = []model.LANPeer{{ID: sender.ID, Local: receiver.WakeInterfaces[0], Remote: sender.WakeInterfaces[0], VerifiedAt: at}}
		g.nodes[receiver.ID] = receiver
		g.publishObservations(receiver.ID)
	}
	publishProof(a, b, old)
	publishProof(b, a, old)

	request := func(id, key, session, condition string, compressed bool) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/v1/lan/"+id, nil)
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set(LANViewSessionHeader, session)
		r.Header.Set("If-None-Match", condition)
		if compressed {
			r.Header.Set("Accept-Encoding", "gzip")
		}
		handler.ServeHTTP(w, r)
		return w
	}
	poll := func(id, tag string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := request(id, commonKey, "fixture-"+id, tag, false)
		if w.Code != status || w.Header().Get("ETag") == "" || w.Header().Get("Cache-Control") != "private, no-cache" {
			t.Fatalf("poll %s: status=%d headers=%v body=%s", id, w.Code, w.Header(), w.Body)
		}
		if status == http.StatusNotModified && (w.Body.Len() != 0 || w.Header().Get("ETag") != tag) {
			t.Fatalf("poll %s: unexpected 304: %v %s", id, w.Header(), w.Body)
		}
		return w
	}
	checkProof := func(w *httptest.ResponseRecorder, at time.Time) {
		t.Helper()
		var view model.LANView
		decoder := json.NewDecoder(w.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&view); err != nil {
			t.Fatal(err)
		}
		if view.UserID != legacyUser || view.Receiver != "a" || len(view.Nodes) != 2 ||
			view.Nodes[0].ID != "a" || view.Nodes[1].ID != "b" || len(view.LANPeers) != 1 ||
			view.LANPeers[0].ID != "b" || !view.LANPeers[0].VerifiedAt.Equal(at) {
			t.Fatal("unexpected LAN view", view)
		}
	}
	first := poll("a", "", http.StatusOK)
	tagA := first.Header().Get("ETag")
	checkProof(first, old)
	tagB := poll("b", tagA, http.StatusOK).Header().Get("ETag")
	fullTag := directoryRequest(g, commonKey, "").Header().Get("ETag")

	fresh := old.Add(16 * time.Minute)
	publishProof(b, a, fresh)
	poll("a", tagA, http.StatusNotModified)
	tagB = poll("b", tagB, http.StatusOK).Header().Get("ETag")
	if w := directoryRequest(g, commonKey, fullTag); w.Code != http.StatusOK || w.Header().Get("ETag") == fullTag {
		t.Fatal("full directory stopped invalidating on LAN timestamp changes")
	}

	publishProof(a, b, fresh)
	refreshed := poll("a", tagA, http.StatusOK)
	newA := refreshed.Header().Get("ETag")
	checkProof(refreshed, fresh)
	poll("b", tagB, http.StatusNotModified)
	n := g.nodes["a"]
	n.Name = "renamed"
	g.nodes["a"] = n
	g.publishObservations("a")
	poll("a", newA, http.StatusNotModified)
	poll("b", tagB, http.StatusNotModified)

	gz := request("a", commonKey, "fixture-a", tagA, true)
	if gz.Code != http.StatusOK || gz.Header().Get("ETag") != "W/"+newA || !strings.Contains(gz.Header().Get("Vary"), LANViewSessionHeader) {
		t.Fatal("unexpected gzip response", gz.Code, gz.Header())
	}
	body, err := gzip.NewReader(gz.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	if w := request("a", commonKey, "fixture-a", gz.Header().Get("ETag"), true); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatal("gzip validator did not return an empty 304", w.Code)
	}
	n.Online = false
	g.nodes["a"] = n
	g.publishObservations("a")
	poll("b", tagB, http.StatusOK)
}

func TestLANViewAuthenticatesBeforeConditionalResponse(t *testing.T) {
	g := directoryTestGateway()
	index := map[string]principal{tokenHash("worker-key"): {Role: "common", KeyID: "worker", UserID: legacyUser}}
	g.credentials.Store(&index)
	n := model.Node{ID: "a", UserID: legacyUser, Online: true, LANDiscovery: true}
	g.nodes[n.ID], g.owners[n.ID] = n, legacyUser
	g.routes = map[string]*connection{
		"a": {userID: legacyUser, keyID: "worker", lanViewSession: "current"},
		"b": {userID: "foreign", keyID: "worker", lanViewSession: "current"},
	}
	g.publishObservations(n.ID)
	handler := g.Handler()
	for _, test := range []struct {
		name, id, key, session string
		status                 int
	}{
		{"current session", "a", "worker-key", "current", http.StatusNotModified},
		{"wrong key in same fleet", "a", commonKey, "current", http.StatusNotFound},
		{"missing session", "a", "worker-key", "", http.StatusNotFound},
		{"old session", "a", "worker-key", "old", http.StatusNotFound},
		{"foreign worker", "b", "worker-key", "current", http.StatusNotFound},
		{"invalid bearer", "a", "invalid", "current", http.StatusUnauthorized},
		{"revoked bearer", "a", "worker-key", "current", http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "revoked bearer" {
				empty := map[string]principal{}
				g.credentials.Store(&empty)
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/lan/"+test.id, nil)
			r.Header.Set("Authorization", "Bearer "+test.key)
			r.Header.Set(LANViewSessionHeader, test.session)
			r.Header.Set("If-None-Match", "*")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.status || (test.status != http.StatusNotModified && w.Header().Get("ETag") != "") {
				t.Fatalf("unexpected outcome: %d %v", w.Code, w.Header())
			}
		})
	}
}
