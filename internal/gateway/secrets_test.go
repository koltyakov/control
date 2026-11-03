package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/secrets"
	"google.golang.org/protobuf/proto"
)

func TestConsoleEncryptedSecretManagement(t *testing.T) {
	g, server := installationFixture(t)
	id, _ := identity.Load(t.TempDir())
	ws := rawPeer(t, server.URL, commonKey, "secret-worker", legacyUser, id)
	if ws == nil {
		t.Fatal("worker registration failed")
	}
	key, _ := secrets.NewManagementKey()
	admin := testAdmin{URL: server.URL, Key: superKey}
	provisionBrowser(t, admin, "")
	browser := browserLogin(t, server.URL, "@operator", browserPassword)
	route := "/console/api/fleet/nodes/" + id.ID + "/secrets"
	browser.request(t, "GET", route, nil, 409) // Older nodes fail before entry.
	g.mu.Lock()
	n := g.nodes[id.ID]
	n.SecretManagementKey = key.PublicKey().Bytes()
	g.nodes[id.ID] = n
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	store := secrets.Store{Dir: filepath.Join(t.TempDir(), "secrets"), WorkDir: t.TempDir()}
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
			if bytes.Contains(packet.Data, []byte("private-browser-value")) {
				t.Error("gateway transmitted plaintext")
			}
			var q secrets.ManagementRequest
			if json.Unmarshal(packet.Data, &q) != nil {
				return
			}
			reply := secrets.ManagementReply{ID: q.ID}
			switch q.Operation {
			case "list":
				reply.Secrets, err = store.List()
			case "set":
				var value string
				value, err = secrets.Open(key, id.ID, q.Name, *q.Envelope)
				if err == nil {
					err = store.Set(ctx, q.Name, value)
				}
			case "delete":
				err = store.Delete(ctx, q.Name)
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
	data, headers := browser.request(t, "GET", route, nil, 200)
	var listing secrets.RemoteList
	if json.Unmarshal(data, &listing) != nil || len(listing.Secrets) != 0 || !bytes.Equal(listing.Key, key.PublicKey().Bytes()) || headers.Get("Cache-Control") != "no-store" {
		t.Fatal("invalid metadata/key discovery")
	}
	envelope, _ := secrets.Seal(id.ID, "app.password", "private-browser-value", listing.Key)
	bad := browser
	bad.CSRF = "invalid"
	bad.request(t, "PUT", route+"/app.password", envelope, 403)
	browser.request(t, "PUT", route+"/app.password", map[string]string{"value": "private-browser-value"}, 400)
	browser.request(t, "PUT", route+"/app.password", envelope, 204)
	data, _ = browser.request(t, "GET", route, nil, 200)
	if json.Unmarshal(data, &listing) != nil || len(listing.Secrets) != 1 || listing.Secrets[0].CreatedAt.IsZero() || bytes.Contains(data, []byte("private-browser-value")) || bytes.Contains(data, []byte(`"value"`)) {
		t.Fatal("invalid or value-bearing browser metadata")
	}
	_, other := createTestUser(t, server, "other-secret-owner")
	provisionBrowser(t, other, "")
	foreign := browserLogin(t, server.URL, "other-secret-owner", browserPassword)
	foreign.request(t, "GET", route, nil, 404)
	foreign.request(t, "DELETE", route+"/app.password", nil, 404)
	browser.request(t, "DELETE", route+"/app.password", nil, 204)
	data, _ = browser.request(t, "GET", route, nil, 200)
	if json.Unmarshal(data, &listing) != nil || len(listing.Secrets) != 0 {
		t.Fatal("browser delete failed")
	}
	audit := readAudit(t, admin, "category=machine")
	if bytes.Contains(model.JSON(audit), []byte("private-browser-value")) || bytes.Contains(model.JSON(audit), []byte("app.password")) {
		t.Fatal("secret values or names entered audit records")
	}
	g.mu.Lock()
	peer := g.peers[id.ID]
	requestID := identity.NewID()
	g.secretRequests[requestID] = pendingSecrets{node: id.ID, operation: "list", peer: peer, reply: make(chan secrets.ManagementReply, 1)}
	g.mu.Unlock()
	if g.receiveSecrets(model.Node{ID: "forged"}, peer, model.JSON(secrets.ManagementReply{ID: requestID})) ||
		g.receiveSecrets(n, &connection{}, model.JSON(secrets.ManagementReply{ID: requestID})) ||
		g.receiveSecrets(n, peer, model.JSON(secrets.ManagementReply{ID: requestID, Error: "private-browser-value"})) {
		t.Fatal("forged connection or arbitrary secret diagnostic accepted")
	}
}
