package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

type recordingHints struct {
	mu         sync.Mutex
	hints      map[string]string
	remembered []model.Node
}

func (h *recordingHints) Hint(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hints[name]
}

func (h *recordingHints) Remember(nodes []model.Node) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.remembered = nodes
}

func TestNameHintsAvoidDirectoryOnlyWhenGatewayRecordStillMatches(t *testing.T) {
	const token = "name-hint-lookup-test-token-123456"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	var directoryReads, peerReads atomic.Int32
	handler := g.Handler()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/nodes":
			directoryReads.Add(1)
		case strings.HasPrefix(r.URL.Path, "/v1/peers/"):
			peerReads.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	ctx := t.Context()
	ids := map[string]string{}
	for _, name := range []string{"worker", "other"} {
		id, err := identity.Load(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		p := New(Config{Gateway: s.URL, Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}}, nil)
		if err := p.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = p.Close() }()
		ids[name] = id.ID
	}
	// Each client process starts without in-memory lookup state.
	lookup := func(hints *recordingHints) model.Node {
		t.Helper()
		id, err := identity.Load(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		p := New(Config{Client: true, Gateway: s.URL, Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: "cli-" + id.ID[:16], PublicKey: id.Public}, NameHints: hints}, nil)
		defer func() { _ = p.Close() }()
		if err := p.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Machines read the directory for their own startup work.
		directoryReads.Store(0)
		peerReads.Store(0)
		node, err := p.Lookup(ctx, "worker")
		if err != nil {
			t.Fatal(err)
		}
		return node
	}
	cold := &recordingHints{}
	if node := lookup(cold); node.ID != ids["worker"] || directoryReads.Load() != 1 {
		t.Fatalf("cold lookup: %s, %d directory reads", node.ID, directoryReads.Load())
	}
	found := map[string]string{}
	for _, node := range cold.remembered {
		found[node.Name] = node.ID
	}
	if found["worker"] != ids["worker"] || found["other"] != ids["other"] {
		t.Fatal("directory read was not remembered", found)
	}

	if node := lookup(&recordingHints{hints: map[string]string{"worker": ids["worker"]}}); node.ID != ids["worker"] || directoryReads.Load() != 0 || peerReads.Load() != 1 {
		t.Fatalf("hinted lookup: %s, %d directory and %d peer reads", node.ID, directoryReads.Load(), peerReads.Load())
	}

	// A stale hint, for example after a rename, names another identity. The
	// gateway record no longer carries the name, so the directory decides.
	stale := &recordingHints{hints: map[string]string{"worker": ids["other"]}}
	if node := lookup(stale); node.ID != ids["worker"] || directoryReads.Load() != 1 {
		t.Fatalf("stale hint selected %s with %d directory reads", node.ID, directoryReads.Load())
	}
	if len(stale.remembered) == 0 {
		t.Fatal("fallback directory read did not refresh hints")
	}
}
