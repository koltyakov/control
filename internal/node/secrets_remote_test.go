package node

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/secrets"
)

func TestRemoteSecretNodeChecks(t *testing.T) {
	t.Parallel()
	n := rpaNode(t, "success")
	key, err := secrets.NewManagementKey()
	if err != nil {
		t.Fatal(err)
	}
	m := &secretManagement{key: key, seen: map[string]int64{}}
	s := secrets.Store{Dir: filepath.Join(n.Config.DataDir, "secrets"), WorkDir: n.Config.WorkDir}
	request := func(operation string) secrets.ManagementRequest {
		return secrets.ManagementRequest{ID: identity.NewID(), Operation: operation, ExpiresAt: time.Now().Add(30 * time.Second).Unix()}
	}
	e, err := secrets.Seal(n.Identity.ID, "api", "private-worker-credential", key.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	q := secrets.ManagementRequest{ID: e.ID, Operation: "set", Name: "api", ExpiresAt: e.ExpiresAt, Envelope: &e}
	if reply := m.run(t.Context(), n, q); reply.Error != "" || len(reply.Secrets) != 0 {
		t.Fatalf("set failed: %+v", reply)
	}
	before, _ := s.List()
	if reply := m.run(t.Context(), n, q); reply.Error != "replayed" {
		t.Fatal("replayed write accepted")
	}
	list := request("list")
	reply := m.run(t.Context(), n, list)
	data, _ := json.Marshal(reply)
	if reply.Error != "" || len(reply.Secrets) != 1 || reply.Secrets[0].Name != "api" || strings.Contains(string(data), "private-") || strings.Contains(string(data), `"value"`) {
		t.Fatal("invalid or value-bearing metadata response")
	}
	n.Config.Allow = map[string][]string{}
	if got := m.run(t.Context(), n, request("list")); got.Error != "denied" {
		t.Fatal("ignored node allow rules")
	}
	n.Config.Allow = nil
	n.work.SetDisabled(true)
	if got := m.run(t.Context(), n, request("list")); got.Error != "busy" {
		t.Fatal("disabled node managed credentials")
	}
	n.work.SetDisabled(false)
	n.work.PauseIfIdle()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := m.run(ctx, n, request("list")); got.Error != "busy" {
		t.Fatal("maintenance or cancellation ignored")
	}
	n.work.Resume()
	for _, operation := range []string{"get", "exec.run"} {
		if got := m.run(t.Context(), n, request(operation)); got.Error != "invalid" {
			t.Fatal("arbitrary operation accepted")
		}
	}
	n.Config.Gateway = "http://untrusted.example.com"
	if got := m.run(t.Context(), n, request("list")); got.Error != "denied" {
		t.Fatal("insecure gateway accepted")
	}
	n.Config.Gateway = "http://127.0.0.1:1"
	after, _ := s.List()
	if len(after) != 1 || !before[0].UpdatedAt.Equal(after[0].UpdatedAt) {
		t.Fatal("rejected requests changed metadata")
	}
	q = request("delete")
	q.Name = "api"
	if got := m.run(t.Context(), n, q); got.Error != "" {
		t.Fatal("delete failed")
	}
	values, err := s.Snapshot()
	if err != nil || len(values) != 0 {
		t.Fatal("deleted secret retained")
	}
	for range 128 {
		m.seen[identity.NewID()] = time.Now().Add(time.Minute).Unix()
	}
	if got := m.run(t.Context(), n, request("list")); got.Error != "busy" {
		t.Fatal("replay cache limit ignored")
	}
}
