package gateway

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func TestUnregisterConnectedLegacyMachine(t *testing.T) {
	for _, online := range []bool{true, false} {
		t.Run(fmt.Sprintf("snapshot-online-%t", online), func(t *testing.T) {
			g, s := installationFixture(t)
			alice, a := createTestUser(t, s, "alice")
			_, b := createTestUser(t, s, "bob")
			id, err := identity.Load(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ws := rawPeer(t, s.URL, a.Key, "old-agent", alice.ID, id)
			if ws == nil {
				t.Fatal("registration failed")
			}
			g.mu.Lock()
			n := g.nodes[id.ID]
			n.Online = online
			g.nodes[id.ID] = n
			g.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			// Installer inspection must not claim that an old running agent can
			// enforce disable or acknowledge a policy it has not applied.
			proof := model.MachineStateProof{PublicKey: id.Public, SignedAt: time.Now().UTC(), InspectOnly: true}
			proof.Signature = ed25519.Sign(id.Private, proof.Message())
			if err := (testAdmin{URL: s.URL}).JSON(ctx, "POST", "/v1/node/state", proof, nil); err != nil {
				t.Fatal("inspect identity", err)
			}
			proof.InspectOnly = false
			if err := (testAdmin{URL: s.URL}).JSON(ctx, "POST", "/v1/node/state", proof, nil); err == nil {
				t.Fatal("inspection proof was reused as an acknowledgement")
			}
			g.mu.Lock()
			if g.nodes[id.ID].Managed || !g.peers[id.ID].lifecycleAt.IsZero() {
				t.Error("installer inspection advertised lifecycle support")
			}
			g.mu.Unlock()
			path := "/v1/fleet/nodes/" + id.ID + "?stop=true"
			if err := b.JSON(ctx, "DELETE", path, nil, nil); err == nil {
				t.Fatal("foreign owner removed old agent")
			}
			if err := (testAdmin{URL: s.URL, Key: commonKey}).JSON(ctx, "DELETE", path, nil, nil); err == nil {
				t.Fatal("common key removed old agent")
			}
			if err := a.JSON(ctx, "DELETE", path, nil, nil); err != nil {
				t.Fatal("owner could not unregister connected old agent", err)
			}
			if _, _, err := ws.Read(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("old agent was not disconnected", err)
			}
			var nodes []model.Node
			if err := a.JSON(ctx, "GET", "/v1/nodes", nil, &nodes); err != nil || len(nodes) != 0 {
				t.Fatal("removed registration remains", nodes, err)
			}
			if rawPeer(t, s.URL, a.Key, "old-agent", alice.ID, id) != nil {
				t.Fatal("retired agent reconnected")
			}
		})
	}
}

func TestUnregisterOfflineLegacyMachine(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	_, b := createTestUser(t, s, "bob")
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := rawPeer(t, s.URL, a.Key, "worker", alice.ID, id)
	if ws == nil {
		t.Fatal("register")
	}
	ctx := context.Background()
	path := "/v1/fleet/nodes/" + id.ID + "?stop=true"
	_ = ws.CloseNow()
	deadline := time.Now().Add(3 * time.Second)
	for {
		g.mu.Lock()
		offline := g.peers[id.ID] == nil
		g.mu.Unlock()
		if offline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("machine did not go offline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := b.JSON(ctx, "DELETE", path, nil, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatal("foreign owner could unregister offline machine", err)
	}
	if err := (testAdmin{URL: s.URL, Key: commonKey}).JSON(ctx, "DELETE", path, nil, nil); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatal("common key could unregister offline machine", err)
	}
	if err := a.JSON(ctx, "DELETE", path, nil, nil); err != nil {
		t.Fatal("offline legacy unregister", err)
	}
	var nodes []model.Node
	if err := a.JSON(ctx, "GET", "/v1/nodes", nil, &nodes); err != nil || len(nodes) != 0 {
		t.Fatal("removed machine remains in directory", nodes, err)
	}
	_ = g.Close()
	s.Close()
	g, err = New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s = httptest.NewServer(g.Handler())
	defer s.Close()
	defer func() { _ = g.Close() }()
	a.URL = s.URL
	if err := a.JSON(ctx, "GET", "/v1/nodes", nil, &nodes); err != nil || len(nodes) != 0 {
		t.Fatal("removed machine returned after restart", nodes, err)
	}
	proof := model.MachineStateProof{PublicKey: id.Public, SignedAt: time.Now().UTC()}
	proof.Signature = ed25519.Sign(id.Private, proof.Message())
	var state model.MachineState
	if err := (testAdmin{URL: s.URL}).JSON(ctx, "POST", "/v1/node/state", proof, &state); err != nil || !state.Unregistered || !state.Disabled || state.Revision == 0 {
		t.Fatal("stop policy did not survive restart", state, err)
	}
	if rawPeer(t, s.URL, a.Key, "worker", alice.ID, id) != nil {
		t.Fatal("retired identity rejoined with a valid account key")
	}
}

func TestMachinePolicyOwnershipAndRestart(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	_, b := createTestUser(t, s, "bob")
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := rawPeer(t, s.URL, a.Key, "worker", alice.ID, id)
	if ws == nil {
		t.Fatal("register")
	}
	ctx := context.Background()
	proof := model.MachineStateProof{PublicKey: id.Public, SignedAt: time.Now().UTC()}
	proof.Signature = ed25519.Sign(id.Private, proof.Message())
	var state model.MachineState
	if err := (testAdmin{URL: s.URL}).JSON(ctx, "POST", "/v1/node/state", proof, &state); err != nil {
		t.Fatal(err)
	}
	path := "/v1/fleet/nodes/" + id.ID
	if err := a.JSON(ctx, "DELETE", path, nil, nil); err == nil {
		t.Fatal("legacy forget stopped an online machine")
	}
	if err := b.JSON(ctx, "PATCH", path, map[string]bool{"disabled": true}, nil); err == nil {
		t.Fatal("foreign owner disabled machine")
	}
	if err := a.JSON(ctx, "PATCH", path, map[string]bool{"disabled": true}, &state); err != nil || !state.Disabled {
		t.Fatal("disable", err)
	}
	proof.Revision = state.Revision
	if err := (testAdmin{URL: s.URL}).JSON(ctx, "POST", "/v1/node/state", proof, nil); err == nil {
		t.Fatal("forged acknowledgement accepted")
	}
	proof.Signature = ed25519.Sign(id.Private, proof.Message())
	if err := (testAdmin{URL: s.URL}).JSON(ctx, "POST", "/v1/node/state", proof, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.JSON(ctx, "DELETE", path+"?stop=true", nil, nil); err == nil {
		t.Fatal("foreign owner unregistered machine")
	}
	_ = ws.CloseNow()
	_ = g.Close()
	s.Close()
	dir := filepath.Dir(g.path)
	g, err = New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s = httptest.NewServer(g.Handler())
	defer s.Close()
	defer func() { _ = g.Close() }()
	a.URL = s.URL
	if !g.nodes[id.ID].Disabled || !g.nodes[id.ID].Managed {
		t.Fatal("policy/capability lost on restart")
	}
	if err := a.JSON(ctx, "DELETE", path+"?stop=true", nil, nil); err != nil {
		t.Fatal("offline unregister", err)
	}
	_ = g.Close()
	s.Close()
	g, err = New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s = httptest.NewServer(g.Handler())
	defer s.Close()
	a.URL = s.URL
	var retired model.MachineState
	if err := (testAdmin{URL: s.URL}).JSON(ctx, "POST", "/v1/node/state", proof, &retired); err != nil || !retired.Unregistered {
		t.Fatal("identity cannot read stop after removal", err)
	}
	if rawPeer(t, s.URL, a.Key, "worker", alice.ID, id) != nil {
		t.Fatal("removed identity registered again")
	}
}

func TestMachinePolicySchemaMigrationPreservesAccounts(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	dir := filepath.Dir(g.path)
	_ = g.Close()
	s.Close()
	db, err := sql.Open("sqlite", filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`DROP TABLE machine_states; PRAGMA user_version=1;`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	g, err = New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	if got := g.authenticate(a.Key); got.UserID != alice.ID || got.Role != "user" {
		t.Fatal("migration lost account")
	}
	var version int
	if err := g.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatal("schema not migrated", err)
	}
}
