package client

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
)

func TestStableOwnerAccessRulesPreserveTransportBoundGrants(t *testing.T) {
	const token = "owner-access-rules-test-token-123456"
	const account = "owner-access-account-key-1234567890"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	g, err := gateway.New(t.TempDir(), token, gateway.Options{SuperuserKey: account})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	s := httptest.NewServer(g.Handler())
	defer s.Close()
	state := t.TempDir()
	owner, err := identity.Load(filepath.Join(state, identity.ID([]byte(s.URL+"\x00legacy"))))
	if err != nil {
		t.Fatal(err)
	}
	w, err := node.New(node.Config{Name: "worker", Gateway: s.URL, Token: token, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true,
		Allow: map[string][]string{owner.ID: {"node.describe", "tasks.*", "test.owner", "artifacts.export", "artifacts.grant"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Register(ownerProvider{}); err != nil {
		t.Fatal(err)
	}
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	local := Client{URL: refusedAPI(t)}
	cfg := StandaloneConfig{Gateway: Admin{URL: s.URL, Key: account}, StateDir: state, RelayOnly: true}
	c := local.WithStandalone(ctx, cfg)
	defer func() { _ = c.Close() }()
	other := local.WithStandalone(ctx, cfg)
	defer func() { _ = other.Close() }()
	for _, client := range []Client{c, other} {
		var result string
		if err := client.Call(ctx, "worker", "test.owner", map[string]any{}, &result); err != nil || result != owner.ID {
			t.Fatal("stable-ID access rule", result, err)
		}
	}
	foreignCfg := cfg
	foreignCfg.StateDir = t.TempDir()
	foreign := local.WithStandalone(ctx, foreignCfg)
	defer func() { _ = foreign.Close() }()
	if err := foreign.Call(ctx, "worker", "test.owner", map[string]any{}, nil); err == nil {
		t.Fatal("account credential bypassed owner allow rule")
	}
	if err := os.WriteFile(filepath.Join(w.Config.WorkDir, "result.txt"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	var artifact model.Artifact
	if err := c.Call(ctx, "worker", "artifacts.export", map[string]any{"path": "result.txt"}, &artifact); err != nil {
		t.Fatal(err)
	}
	if err := c.Download(ctx, artifact, 0, io.Discard); err == nil {
		t.Fatal("owner rule granted unlisted artifact access")
	}
	if err := c.Call(ctx, "worker", "artifacts.grant", map[string]any{"id": artifact.ID, "target": c.routing.peer.IdentityID()}, &artifact); err != nil {
		t.Fatal(err)
	}
	var result bytes.Buffer
	if err := c.Download(ctx, artifact, 0, &result); err != nil || result.String() != "content" {
		t.Fatal(result.String(), err)
	}
	if err := other.Download(ctx, artifact, 0, io.Discard); err == nil {
		t.Fatal("transport grant authorized another process sharing task ownership")
	}
}
