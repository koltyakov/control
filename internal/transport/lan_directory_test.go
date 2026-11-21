package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func TestLANDirectoryConditionalRefreshAndOwnership(t *testing.T) {
	requests := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := r.Header.Get("If-None-Match")
		requests <- etag
		w.Header().Set("ETag", `"one"`)
		if etag == `"one"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_ = json.NewEncoder(w).Encode([]model.Node{{
			ID: "worker", UserID: "fleet", PublicKey: []byte{1, 2, 3}, Labels: map[string]string{"host": "one"},
			Online: true, LANDiscovery: true, WakeInterfaces: []model.WakeInterface{{Address: "192.168.1.3/24"}},
			Capabilities: []model.Capability{{Name: "test", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			System:       &model.SystemInfo{OS: "linux"},
			LANPeers:     []model.LANPeer{{ID: "not-self-proof"}},
		}})
	}))
	defer server.Close()
	p := &Peer{cfg: Config{Gateway: server.URL, Identity: &identity.Identity{ID: "self"}}, userID: "fleet"}
	d := lanDirectory{}
	for i := range 2 {
		view, err := d.poll(context.Background(), p)
		if err != nil || len(view.Nodes) != 1 || len(view.LANPeers) != 0 {
			t.Fatal(view, err)
		}
		if got := <-requests; (i == 0 && got != "") || (i == 1 && got != `"one"`) {
			t.Fatal("conditional header", got)
		}
	}
	// Public results remain independent of the watcher and other callers.
	nodes, err := p.Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := <-requests; got != "" {
		t.Fatal("Nodes unexpectedly sent a validator", got)
	}
	nodes[0].PublicKey[0] = 9
	if d.view.Nodes[0].PublicKey[0] != 1 {
		t.Fatal("Nodes result aliases watcher")
	}
	p.directoryGeneration++
	if _, err := d.poll(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := <-requests; got != "" {
		t.Fatal("notification/reconnect retained validator", got)
	}
}

func TestLANDirectoryLegacyAndErrors(t *testing.T) {
	for _, mode := range []string{"legacy", "foreign", "unauthorized", "unsolicited304", "changedGeneration"} {
		t.Run(mode, func(t *testing.T) {
			p := &Peer{cfg: Config{Identity: &identity.Identity{ID: "self"}}, userID: "fleet"}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("If-None-Match") != "" {
					t.Error("legacy response retained a validator")
				}
				switch mode {
				case "unauthorized":
					w.WriteHeader(http.StatusUnauthorized)
					return
				case "unsolicited304":
					w.WriteHeader(http.StatusNotModified)
					return
				case "changedGeneration":
					p.mu.Lock()
					p.directoryGeneration++
					p.mu.Unlock()
				}
				user := "fleet"
				if mode == "foreign" {
					user = "other"
				}
				_ = json.NewEncoder(w).Encode([]model.Node{{ID: "worker", UserID: user}})
			}))
			defer server.Close()
			p.cfg.Gateway = server.URL
			d := lanDirectory{}
			for range 2 {
				_, err := d.poll(context.Background(), p)
				if (err == nil) != (mode == "legacy") {
					t.Fatal("unexpected poll result", err)
				}
			}
		})
	}
}

func TestLANDirectoryCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	p := &Peer{cfg: Config{Gateway: server.URL, Identity: &identity.Identity{ID: "self"}}, userID: "fleet"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		d := lanDirectory{}
		_, err := d.poll(ctx, p)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("LAN directory refresh ignored cancellation")
	}
}

func TestLANCachedDirectoryStillAgesProofs(t *testing.T) {
	now := time.Now()
	a := model.WakeInterface{Address: "192.168.1.2/24", MAC: "02:00:00:00:00:02"}
	b := model.WakeInterface{Address: "192.168.1.3/24", MAC: "02:00:00:00:00:03"}
	view := model.LANView{
		Receiver: "self",
		LANPeers: []model.LANPeer{{ID: "other", Local: a, Remote: b, VerifiedAt: now}},
		Nodes:    []model.LANNode{{ID: "other", WakeInterfaces: []model.WakeInterface{b}}},
	}
	if len(lanCandidates(view, []model.WakeInterface{a}, now)) != 0 ||
		len(lanCandidates(view, []model.WakeInterface{a}, now.Add(15*time.Minute))) != 1 {
		t.Fatal("unchanged directory prevented proof freshness from aging")
	}
}

func TestLANViewPollReusesAndRefreshesProof(t *testing.T) {
	now := time.Now().UTC()
	a := model.WakeInterface{Address: "192.168.1.2/24", MAC: "02:00:00:00:00:02"}
	b := model.WakeInterface{Address: "192.168.1.3/24", MAC: "02:00:00:00:00:03"}
	steps := []struct {
		session, condition, tag string
		status                  int
		verified                time.Time
	}{
		{"one", "", `W/"old"`, http.StatusOK, now.Add(-15 * time.Minute)},
		{"one", `W/"old"`, `W/"old"`, http.StatusNotModified, time.Time{}},
		{"one", `W/"old"`, `W/"new"`, http.StatusOK, now},
		{"two", "", `W/"new"`, http.StatusOK, now},
	}
	requests := make(chan int, len(steps))
	for i := range steps {
		requests <- i
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := <-requests
		step := steps[i]
		if r.URL.Path != "/v1/lan/self" || r.Header.Get("Authorization") != "Bearer worker-key" ||
			r.Header.Get(gateway.LANViewSessionHeader) != step.session || r.Header.Get("If-None-Match") != step.condition {
			t.Errorf("unexpected LAN view request at step %d", i)
		}
		w.Header().Set("ETag", step.tag)
		w.WriteHeader(step.status)
		if step.status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(model.LANView{
				UserID: "fleet", Receiver: "self", Nodes: []model.LANNode{{ID: "other", WakeInterfaces: []model.WakeInterface{b}}},
				LANPeers: []model.LANPeer{{ID: "other", Local: a, Remote: b, VerifiedAt: step.verified}},
			})
		}
	}))
	defer server.Close()
	p := &Peer{cfg: Config{Gateway: server.URL, Token: "worker-key", Identity: &identity.Identity{ID: "self"}}, userID: "fleet", lanViewSession: "one"}
	d := lanDirectory{}
	for i := range steps {
		p.directoryGeneration++ // Name changes must not reset the narrow snapshot.
		if i == 3 {
			p.lanViewSession = "two"
		}
		view, err := d.poll(t.Context(), p)
		if err != nil || (len(lanCandidates(view, []model.WakeInterface{a}, now)) == 1) != (i < 2) {
			t.Fatalf("step %d: unexpected candidates or poll error: %v, %v", i, view, err)
		}
	}
}

func TestLANWatcherNegotiatesViewWithGateway(t *testing.T) {
	const token = "lan-test-common-key"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	handler := g.Handler()
	polls := make(chan int, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/lan/") || r.URL.Path == "/v1/nodes" {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			for key, values := range rec.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			polls <- rec.Code
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Gateway: server.URL, Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: "watcher", PublicKey: id.Public}}, nil)
	defer func() { _ = p.Close() }()
	p.wakeSnapshot = func() ([]model.WakeInterface, error) {
		return []model.WakeInterface{{Address: "192.168.1.2/24", MAC: "02:00:00:00:00:02"}}, nil
	}
	p.networkSnapshot = func() (string, error) { return "unchanged", nil }
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-polls:
		if code != http.StatusOK || p.lanDirectoryScope().session == "" {
			t.Fatal("watcher did not receive negotiated LAN view", code)
		}
	case <-ctx.Done():
		t.Fatal("watcher did not poll")
	}
	d := lanDirectory{}
	for _, status := range []int{http.StatusOK, http.StatusNotModified} {
		view, err := d.poll(ctx, p)
		if err != nil || view.Receiver != id.ID || len(view.Nodes) != 1 || view.Nodes[0].ID != id.ID || <-polls != status {
			t.Fatal("unexpected gateway LAN response", view, err)
		}
	}
}
