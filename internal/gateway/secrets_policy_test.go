package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/secrets"
	"google.golang.org/protobuf/proto"
)

// The gateway forwards policy-bearing envelopes only to workers that signed a
// policy advertisement, asks for policy metadata only from them, and refuses
// policy fields it did not ask for.
func TestSecretPolicyNegotiation(t *testing.T) {
	g, server := installationFixture(t)
	id, _ := identity.Load(t.TempDir())
	ws := rawPeer(t, server.URL, commonKey, "policy-worker", legacyUser, id)
	if ws == nil {
		t.Fatal("worker registration failed")
	}
	key, _ := secrets.NewManagementKey()
	admin := testAdmin{URL: server.URL, Key: superKey}
	provisionBrowser(t, admin, "")
	browser := browserLogin(t, server.URL, "@operator", browserPassword)
	route := "/console/api/fleet/nodes/" + id.ID + "/secrets"
	setNode := func(policies bool) model.Node {
		g.mu.Lock()
		defer g.mu.Unlock()
		n := g.nodes[id.ID]
		n.SecretManagementKey, n.SecretPolicy = key.PublicKey().Bytes(), policies
		g.nodes[id.ID] = n
		return n
	}
	setNode(false)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	store := secrets.Store{Dir: filepath.Join(t.TempDir(), "secrets"), WorkDir: t.TempDir()}
	var sets, policyLists atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var packet protocol.Packet
			if proto.Unmarshal(data, &packet) != nil || packet.Kind != "secrets.request" {
				continue
			}
			var q secrets.ManagementRequest
			if json.Unmarshal(packet.Data, &q) != nil {
				return
			}
			reply := secrets.ManagementReply{ID: q.ID}
			switch q.Operation {
			case "list":
				reply.Secrets, err = store.List()
				if q.Policies {
					policyLists.Add(1)
				} else {
					for i := range reply.Secrets {
						reply.Secrets[i].Policy = nil
					}
				}
			case "set":
				sets.Add(1)
				var w secrets.Write
				if w, err = secrets.OpenWrite(key, id.ID, q.Name, *q.Envelope); err == nil {
					err = store.SetWithPolicy(ctx, q.Name, w.Value, w.Policy)
				}
			}
			if err != nil {
				reply.Error = "storage"
			}
			data, _ = proto.Marshal(&protocol.Packet{Kind: "secrets.result", Data: model.JSON(reply)})
			if ws.Write(ctx, websocket.MessageBinary, data) != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })

	policy, _ := secrets.NewPolicy([]string{"http"}, []string{"policy-host.example.com"}, false)
	data, _ := browser.request(t, "GET", route, nil, 200)
	var listing secrets.RemoteList
	if json.Unmarshal(data, &listing) != nil || listing.Policies || policyLists.Load() != 0 {
		t.Fatal("gateway claimed or requested policy support for an older worker")
	}
	envelope, _ := secrets.SealPolicy(id.ID, "api.token", "private-policy-value", policy, listing.Key)
	data, _ = browser.request(t, "PUT", route+"/api.token", envelope, 409)
	if !strings.Contains(string(data), "does not support secret use policies") || sets.Load() != 0 {
		t.Fatalf("policy envelope sent to a worker without support: %s", data)
	}

	setNode(true)
	data, _ = browser.request(t, "GET", route, nil, 200)
	if json.Unmarshal(data, &listing) != nil || !listing.Policies || policyLists.Load() != 1 {
		t.Fatal("policy support not negotiated")
	}
	envelope, _ = secrets.SealPolicy(id.ID, "api.token", "private-policy-value", policy, listing.Key)
	browser.request(t, "PUT", route+"/api.token", envelope, 204)
	data, _ = browser.request(t, "GET", route, nil, 200)
	if json.Unmarshal(data, &listing) != nil || len(listing.Secrets) != 1 || listing.Secrets[0].Policy == nil || listing.Secrets[0].Policy.Hosts[0] != "policy-host.example.com" || bytes.Contains(data, []byte("private-")) {
		t.Fatalf("policy metadata not listed: %s", data)
	}
	if audit := model.JSON(readAudit(t, admin, "category=machine")); bytes.Contains(audit, []byte("policy-host")) || bytes.Contains(audit, []byte("api.token")) {
		t.Fatal("policy contents or names entered audit records")
	}

	// Replies must not carry policy fields that were not requested, nor
	// non-canonical policies, nor exceed the original bound without them.
	g.mu.Lock()
	peer := g.peers[id.ID]
	n := g.nodes[id.ID]
	g.mu.Unlock()
	pending := func(policies bool) string {
		g.mu.Lock()
		defer g.mu.Unlock()
		requestID := identity.NewID()
		g.secretRequests[requestID] = pendingSecrets{node: id.ID, operation: "list", policies: policies, peer: peer, reply: make(chan secrets.ManagementReply, 1)}
		return requestID
	}
	withPolicy := []secrets.Metadata{{Name: "api", Policy: policy}}
	if g.receiveSecrets(n, peer, model.JSON(secrets.ManagementReply{ID: pending(false), Secrets: withPolicy})) ||
		g.receiveSecrets(n, peer, model.JSON(secrets.ManagementReply{ID: pending(false), Secrets: []secrets.Metadata{{Name: "api", PolicyInvalid: true}}})) ||
		g.receiveSecrets(n, peer, model.JSON(secrets.ManagementReply{ID: pending(true), Secrets: []secrets.Metadata{{Name: "api", Policy: &secrets.Policy{Uses: []secrets.Use{"http"}}}}})) ||
		g.receiveSecrets(n, peer, model.JSON(secrets.ManagementReply{ID: pending(true), Secrets: []secrets.Metadata{{Name: "api", Policy: policy, PolicyInvalid: true}}})) {
		t.Fatal("accepted unrequested or invalid policy metadata")
	}
	padded := func(id string) []byte {
		return append(model.JSON(secrets.ManagementReply{ID: id}), bytes.Repeat([]byte(" "), 70<<10)...)
	}
	if g.receiveSecrets(n, peer, padded(pending(false))) {
		t.Fatal("accepted an oversized reply that did not request policies")
	}
	if !g.receiveSecrets(n, peer, padded(pending(true))) || g.receiveSecrets(n, peer, append(model.JSON(secrets.ManagementReply{ID: pending(true)}), bytes.Repeat([]byte(" "), maxPolicySecretsReply)...)) {
		t.Fatal("policy listing bound not applied")
	}
	if !g.receiveSecrets(n, peer, model.JSON(secrets.ManagementReply{ID: pending(true), Secrets: withPolicy})) {
		t.Fatal("refused requested policy metadata")
	}
}
