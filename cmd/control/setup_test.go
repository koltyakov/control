package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
)

func TestSavedAdminRespectsCredentialAndGatewayOverrides(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	t.Setenv("CONTROL_GATEWAY", "")
	t.Setenv("CONTROL_TOKEN", "")
	t.Setenv("CONTROL_SUPERUSER_KEY", "")
	t.Setenv("CONTROL_USER_KEY", "")
	if err := installation.SaveAdmin(installation.AdminProfile{Gateway: "https://pool.example", Key: "saved-admin"}); err != nil {
		t.Fatal(err)
	}
	if c := adminClient(installation.ConfigPath()); c.Key != "saved-admin" || c.URL != "https://pool.example" {
		t.Fatalf("saved credentials not used: %+v", c)
	}
	t.Setenv("CONTROL_TOKEN", "common-key")
	if c := adminClient(installation.ConfigPath()); c.Key != "common-key" {
		t.Fatal("common credential silently elevated by saved administrator key")
	}
	t.Setenv("CONTROL_TOKEN", "")
	t.Setenv("CONTROL_GATEWAY", "https://other.example")
	if c := adminClient(installation.ConfigPath()); c.Key != "" {
		t.Fatal("saved administrator credential sent to another gateway")
	}
}

func TestReenrollmentAfterUnregisterUsesFreshState(t *testing.T) {
	config := filepath.Join(t.TempDir(), "node.json")
	var retired atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var proof model.MachineStateProof
		if r.URL.Path != "/v1/node/state" || json.NewDecoder(r.Body).Decode(&proof) != nil || !proof.InspectOnly || !ed25519.Verify(proof.PublicKey, proof.Message(), proof.Signature) {
			http.Error(w, "bad proof", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(model.MachineState{Unregistered: retired.Load()})
	}))
	defer server.Close()
	if err := createNodeConfig(config, "old", server.URL, "old-token", "127.0.0.1:7339", true); err != nil {
		t.Fatal(err)
	}
	cfg, err := node.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	old, err := identity.Load(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingEnrollment{Invitation: enrollment.Invitation{Name: "replacement", Gateway: server.URL}, Credential: "new-token"}
	if err = prepareReenrollment(config, &pending); err != nil {
		t.Fatal(err)
	}
	if err = replaceRetiredIdentity(context.Background(), config, &pending); err != nil || pending.IdentityID != old.ID || pending.DataDir != "" {
		t.Fatal("registered identity was replaced", err)
	}
	retired.Store(true)
	if err = replaceRetiredIdentity(context.Background(), config, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.IdentityID == old.ID || pending.PreviousIdentityID != old.ID || pending.DataDir == cfg.DataDir {
		t.Fatal("retired identity was reused", pending)
	}
	fresh, err := identity.Load(pending.DataDir)
	if err != nil || fresh.ID != pending.IdentityID {
		t.Fatal("fresh identity not saved", err)
	}
	// Pending recovery must keep the chosen identity even if the gateway is gone.
	path := filepath.Join(filepath.Dir(config), "pending-enrollment.json")
	if err = store.Write(path, pending); err != nil {
		t.Fatal(err)
	}
	var restored pendingEnrollment
	if err = store.Read(path, &restored); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if err = replaceRetiredIdentity(context.Background(), config, &restored); err != nil || restored.IdentityID != fresh.ID {
		t.Fatal("pending recovery changed identity", err)
	}
	active, err := node.LoadConfig(config)
	if err != nil || active.DataDir != cfg.DataDir || active.Token != cfg.Token {
		t.Fatal("preparation modified active profile", err)
	}
	preserved, err := identity.Load(cfg.DataDir)
	if err != nil || preserved.ID != old.ID {
		t.Fatal("retired identity files changed", err)
	}
	var replacement node.Config
	if err = json.Unmarshal(restored.Config, &replacement); err != nil || replacement.WorkDir != cfg.WorkDir || replacement.Listen != cfg.Listen || replacement.DataDir != pending.DataDir || len(replacement.Allow) != len(cfg.Allow) {
		t.Fatal("replacement lost profile settings", err)
	}
}

func TestPrepareReenrollmentPreservesIdentityAndConfiguration(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "node.json")
	id, err := identity.Load(filepath.Join(home, "state"))
	if err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"name":"old","gateway":"https://pool.example","token":"old-token","dataDir":"state","workDir":"custom-work","listen":"127.0.0.1:7339","allow":{},"providers":{"custom":{"command":"tool"}},"futureSetting":true}`)
	if err = os.WriteFile(config, original, 0600); err != nil {
		t.Fatal(err)
	}
	pending := pendingEnrollment{Invitation: enrollment.Invitation{Name: "new", Gateway: "https://pool.example"}, Credential: "new-token"}
	if err = prepareReenrollment(config, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.IdentityID != id.ID {
		t.Fatal("replacement changed identity")
	}
	var before, after map[string]json.RawMessage
	if err = json.Unmarshal(original, &before); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(pending.Config, &after); err != nil {
		t.Fatal(err)
	}
	for key, value := range before {
		if key == "name" || key == "token" {
			continue
		}
		var want, got any
		_ = json.Unmarshal(value, &want)
		_ = json.Unmarshal(after[key], &got)
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if string(wantJSON) != string(gotJSON) {
			t.Errorf("changed setting %s: %s", key, after[key])
		}
	}
	if string(after["name"]) != `"new"` || string(after["token"]) != `"new-token"` {
		t.Fatal("replacement did not apply invitation")
	}
	saved, err := os.ReadFile(config)
	if err != nil || string(saved) != string(original) {
		t.Fatal("preparation modified the active profile", err)
	}
	pending.Invitation.ReplaceID = "another-machine"
	if err = prepareReenrollment(config, &pending); err == nil {
		t.Fatal("replacement accepted another machine's reserved name")
	}
	pending.Invitation.ReplaceID = id.ID
	if err = prepareReenrollment(config, &pending); err != nil {
		t.Fatal("same-name replacement rejected", err)
	}
	pending.Invitation.Gateway = "https://other.example"
	if err = prepareReenrollment(config, &pending); err == nil {
		t.Fatal("replacement accepted a different gateway")
	}
}
