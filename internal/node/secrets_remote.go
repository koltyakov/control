package node

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/secrets"
)

type secretManagement struct {
	key  *ecdh.PrivateKey
	mu   sync.Mutex
	seen map[string]int64
}

func (n *Node) configureSecrets() error {
	if !secrets.SecureURL(n.Config.Gateway) || !accountMayCall(n.Config.Allow, "secrets.manage") {
		return nil
	}
	key, err := secrets.NewManagementKey()
	if err != nil {
		return err
	}
	m := &secretManagement{key: key, seen: map[string]int64{}}
	n.Peer.SetSecretsHandler(key.PublicKey().Bytes(), func(data []byte) {
		if len(data) > secrets.MaxRemoteMessage {
			return
		}
		var q secrets.ManagementRequest
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || !secrets.ValidRequestID(q.ID) {
			return
		}
		ctx, cancel := context.WithTimeout(n.ctx, 8*time.Second)
		defer cancel()
		reply := m.run(ctx, n, q)
		_ = n.Peer.SendControl(ctx, "secrets.result", model.JSON(reply))
	})
	return nil
}

func (m *secretManagement) run(ctx context.Context, n *Node, q secrets.ManagementRequest) secrets.ManagementReply {
	reply := secrets.ManagementReply{ID: q.ID}
	if !q.Valid(time.Now()) {
		reply.Error = "invalid"
		return reply
	}
	if !secrets.SecureURL(n.Config.Gateway) || !accountMayCall(n.Config.Allow, "secrets.manage") {
		reply.Error = "denied"
		return reply
	}
	release, err := n.work.Enter(ctx)
	if err != nil {
		reply.Error = "busy"
		return reply
	}
	defer release()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, expires := range m.seen {
		if expires <= time.Now().Unix() {
			delete(m.seen, id)
		}
	}
	if _, exists := m.seen[q.ID]; exists {
		reply.Error = "replayed"
		return reply
	}
	if len(m.seen) >= 128 || !q.Valid(time.Now()) || ctx.Err() != nil {
		reply.Error = "busy"
		return reply
	}
	m.seen[q.ID] = q.ExpiresAt
	s := secrets.Store{Dir: filepath.Join(n.Config.DataDir, "secrets"), WorkDir: n.Config.WorkDir}
	switch q.Operation {
	case "list":
		reply.Secrets, err = s.List()
	case "delete":
		err = s.Delete(ctx, q.Name)
	case "set":
		var value string
		value, err = secrets.Open(m.key, n.Identity.ID, q.Name, *q.Envelope)
		if err != nil {
			reply.Error = "invalid"
			return reply
		}
		err = s.Set(ctx, q.Name, value)
	}
	if err != nil {
		// Filesystem, decoder and credential diagnostics never cross the gateway.
		reply.Error = "storage"
	}
	return reply
}
