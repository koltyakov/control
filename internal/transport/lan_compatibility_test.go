package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// Nodes restart before the gateway in a managed rollout. The original WOL
// gateway understands signed bytes but rejects unlabeled interface metadata.
func TestLANMetadataNegotiatesBeforeAutomaticReporting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                     string
		modern, nodeBytes, label, wantInterfaces bool
	}{
		{"automatic discovery gateway", true, true, false, true},
		{"older gateway without label", false, true, false, false},
		{"older gateway explicit label", false, true, true, true},
		{"legacy registration encoding", false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var mu sync.Mutex
			var registered model.Node
			observed := make(chan model.Node, 1)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/auth":
					_ = json.NewEncoder(w).Encode(map[string]any{"userId": "fleet", "nodeBytes": tc.nodeBytes, "lanDiscovery": tc.modern})
				case "/v1/nodes":
					mu.Lock()
					node := registered
					mu.Unlock()
					_ = json.NewEncoder(w).Encode([]model.Node{node})
				case "/v1/connect":
					ws, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer func() { _ = ws.CloseNow() }()
					challenge := make([]byte, 32)
					if _, err := rand.Read(challenge); err != nil {
						t.Error(err)
						return
					}
					if ws.Write(ctx, websocket.MessageBinary, challenge) != nil {
						return
					}
					_, raw, err := ws.Read(ctx)
					if err != nil {
						return
					}
					var hello gateway.Hello
					if json.Unmarshal(raw, &hello) != nil {
						return
					}
					node, err := hello.RegisteredNode()
					if err != nil || !ed25519.Verify(node.PublicKey, hello.Message(challenge), hello.Signature) {
						return
					}
					observed <- node
					if !tc.modern && (node.LANDiscovery || (len(node.WakeInterfaces) > 0 && node.Labels["lan"] == "")) {
						_ = ws.Close(websocket.StatusPolicyViolation, "legacy gateway rejects unlabeled wake metadata")
						return
					}
					mu.Lock()
					registered = node
					mu.Unlock()
					if ws.Write(ctx, websocket.MessageBinary, []byte("ready")) != nil {
						return
					}
					for {
						if _, _, err := ws.Read(ctx); err != nil {
							return
						}
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer s.Close()
			id, err := identity.Generate()
			if err != nil {
				t.Fatal(err)
			}
			labels := map[string]string{}
			if tc.label {
				labels["lan"] = "home"
			}
			p := New(Config{Gateway: s.URL, Token: "fixture", Identity: id, Node: model.Node{ID: id.ID, PublicKey: id.Public, Name: "worker", Labels: labels}}, nil)
			defer func() { _ = p.Close() }()
			var samples atomic.Int32
			p.wakeSnapshot = func() ([]model.WakeInterface, error) {
				samples.Add(1)
				return []model.WakeInterface{{Address: "192.168.7.2/24", MAC: "02:00:00:00:00:02"}}, nil
			}
			p.networkSnapshot = func() (string, error) { return "unchanged", nil }
			if err := p.Start(ctx); err != nil {
				t.Fatal("rolling-upgrade registration failed", err)
			}
			node := <-observed
			if (len(node.WakeInterfaces) > 0) != tc.wantInterfaces || (samples.Load() > 0) != tc.wantInterfaces {
				t.Fatal("incorrect metadata negotiation", node, samples.Load())
			}
			if node.LANDiscovery != (tc.modern && tc.nodeBytes) {
				t.Fatal("incorrect discovery feature negotiation", node)
			}
		})
	}
}
