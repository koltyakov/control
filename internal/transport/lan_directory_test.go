package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
			Capabilities: []model.Capability{{Name: "test", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			System:       &model.SystemInfo{OS: "linux"},
		}})
	}))
	defer server.Close()
	p := &Peer{cfg: Config{Gateway: server.URL}, userID: "fleet"}
	d := lanDirectory{}
	for i := range 2 {
		nodes, err := d.poll(context.Background(), p)
		if err != nil || len(nodes) != 1 {
			t.Fatal(nodes, err)
		}
		if got := <-requests; (i == 0 && got != "") || (i == 1 && got != `"one"`) {
			t.Fatal("conditional header", got)
		}
		if nodes[0].Labels != nil || nodes[0].Capabilities != nil || nodes[0].System != nil {
			t.Fatal("LAN directory retained unrelated metadata")
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
	if d.nodes[0].PublicKey[0] != 1 {
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
			p := &Peer{userID: "fleet"}
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
	p := &Peer{cfg: Config{Gateway: server.URL}, userID: "fleet"}
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
	nodes := []model.Node{
		{ID: "self", LANPeers: []model.LANPeer{{ID: "other", Local: a, Remote: b, VerifiedAt: now}}},
		{ID: "other", Online: true, LANDiscovery: true, WakeInterfaces: []model.WakeInterface{b}},
	}
	if len(lanCandidates("self", []model.WakeInterface{a}, nodes, now)) != 0 ||
		len(lanCandidates("self", []model.WakeInterface{a}, nodes, now.Add(15*time.Minute))) != 1 {
		t.Fatal("unchanged directory prevented proof freshness from aging")
	}
}
