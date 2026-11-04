package gateway

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func hybridInvite(t *testing.T, g *Gateway, server *httptest.Server) [2]enrollment.Link {
	t.Helper()
	var user enrollment.Link
	q := enrollment.Request{Name: "hybrid", OS: "linux", Arch: "amd64", Gateway: server.URL, ServiceMode: "hybrid"}
	if err := (testAdmin{URL: server.URL, Key: superKey}).JSON(context.Background(), "POST", "/v1/fleet/installations", q, &user); err != nil {
		t.Fatal(err)
	}
	ticket := enrollment.SystemTicket(user.URL[strings.LastIndex(user.URL, "/")+1:])
	g.mu.Lock()
	system := g.installations[enrollment.Hash(ticket)]
	g.mu.Unlock()
	return [2]enrollment.Link{user, {Invitation: system.Invitation, URL: server.URL + "/install/" + ticket}}
}

func hybridRedeem(t *testing.T, link enrollment.Link) (*identity.Identity, string, enrollment.Redemption) {
	t.Helper()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credential := "credential-" + identity.NewID()
	proof := enrollment.Redemption{PublicKey: id.Public, CredentialHash: enrollment.Hash(credential)}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	proof.Signature = ed25519.Sign(id.Private, enrollment.Message(ticket, proof))
	if err = (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
		t.Fatal(err)
	}
	return id, credential, proof
}

func hybridConnect(t *testing.T, server *httptest.Server, link enrollment.Link, id *identity.Identity, credential string, accepted bool) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, server.URL+"/v1/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + credential}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	_, challenge, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := model.Node{ID: id.ID, PublicKey: id.Public, Name: link.Name, OS: link.Asset.OS, RunAs: link.HybridRole, InstructionOnly: link.HybridRole == "system"}
	n.Software.Arch = link.Asset.Arch
	hello := Hello{Version: model.Version, Node: n}
	hello.Signature = ed25519.Sign(id.Private, hello.Message(challenge))
	if err = ws.Write(ctx, websocket.MessageBinary, model.JSON(hello)); err != nil {
		t.Fatal(err)
	}
	_, ready, err := ws.Read(ctx)
	if accepted != (err == nil && string(ready) == "ready") {
		t.Fatalf("registration accepted=%t: %s %v", accepted, ready, err)
	}
	return ws
}

func TestHybridIncompleteTimeout(t *testing.T) {
	for _, scenario := range []string{"neither", "system redeemed", "system online", "both redeemed", "user online", "both online"} {
		t.Run(scenario, func(t *testing.T) {
			g, server := installationFixture(t)
			links := hybridInvite(t, g, server)
			ids := []*identity.Identity{}
			credentials := []string{}
			peers := []*websocket.Conn{}
			for _, index := range []int{1, 0} {
				if scenario == "neither" || (index == 0 && strings.HasPrefix(scenario, "system")) {
					continue
				}
				id, credential, _ := hybridRedeem(t, links[index])
				ids, credentials = append(ids, id), append(credentials, credential)
				if scenario == "both online" || (index == 1 && scenario == "system online") || (index == 0 && scenario == "user online") {
					peers = append(peers, hybridConnect(t, server, links[index], id, credential, true))
				}
			}
			if err := g.cleanupHybridInstallations(links[0].ExpiresAt.Add(-time.Nanosecond)); err != nil {
				t.Fatal(err)
			}
			g.mu.Lock()
			if len(g.installations) != 2 {
				t.Error("cleaned up before deadline")
			}
			g.mu.Unlock()
			if err := g.cleanupHybridInstallations(links[0].ExpiresAt); err != nil {
				t.Fatal(err)
			}
			complete := scenario == "both online"
			g.mu.Lock()
			if complete {
				for _, i := range g.installations {
					if !i.HybridComplete {
						t.Error("pair not durably completed")
					}
				}
				if len(g.installations) != 2 || len(g.nodes) != 2 {
					t.Error("expired a completed pair")
				}
			} else if len(g.installations) != 0 || len(g.nodes) != 0 {
				t.Error("partial pair remains", g.installations, g.nodes)
			}
			for _, id := range ids {
				if g.machineStates[id.ID].Unregistered == complete || g.owners[id.ID] != legacyUser {
					t.Error("incorrect retirement or lost identity ownership")
				}
			}
			persisted := map[string]installation{}
			if err := readRecords(g.db, "invitations", "hash", persisted); err != nil || len(persisted) != len(g.installations) {
				t.Error("cleanup not persisted", err, persisted)
			}
			g.mu.Unlock()
			for _, credential := range credentials {
				if (g.authenticate(credential).Role != "") != complete {
					t.Error("incorrect credential retention")
				}
			}
			if !complete {
				for _, ws := range peers {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					_, _, err := ws.Read(ctx)
					cancel()
					if err == nil {
						t.Error("expired companion remained connected")
					}
				}
				// Both names are reusable, without force or manual invitation clearing.
				hybridInvite(t, g, server)
			}
		})
	}
}

func TestHybridCleanupRollbackAndRetry(t *testing.T) {
	g, server := installationFixture(t)
	links := hybridInvite(t, g, server)
	id, credential, _ := hybridRedeem(t, links[1])
	hybridConnect(t, server, links[1], id, credential, true)
	if _, err := g.db.Exec(`CREATE TRIGGER fail_cleanup BEFORE DELETE ON invitations BEGIN SELECT RAISE(ABORT, 'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := g.cleanupHybridInstallations(links[0].ExpiresAt); err == nil {
		t.Fatal("cleanup acknowledged failed persistence")
	}
	g.mu.Lock()
	if len(g.installations) != 2 || len(g.nodes) != 1 || g.machineStates[id.ID].Unregistered {
		t.Error("failed cleanup changed live state")
	}
	g.mu.Unlock()
	if g.authenticate(credential).Role == "" || len(g.directory(legacyUser)) != 1 {
		t.Fatal("failed cleanup published revocation or removal")
	}
	if _, err := g.db.Exec(`DROP TRIGGER fail_cleanup`); err != nil {
		t.Fatal(err)
	}
	if err := g.cleanupHybridInstallations(links[0].ExpiresAt); err != nil {
		t.Fatal(err)
	}
}

func TestHybridCompletionRollback(t *testing.T) {
	g, server := installationFixture(t)
	links := hybridInvite(t, g, server)
	system, systemKey, _ := hybridRedeem(t, links[1])
	hybridConnect(t, server, links[1], system, systemKey, true)
	user, userKey, _ := hybridRedeem(t, links[0])
	if _, err := g.db.Exec(`CREATE TRIGGER fail_completion BEFORE UPDATE ON invitations BEGIN SELECT RAISE(ABORT, 'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	hybridConnect(t, server, links[0], user, userKey, false)
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, i := range g.installations {
		if i.HybridComplete {
			t.Error("failed second registration completed pair")
		}
	}
	if _, exists := g.nodes[user.ID]; exists {
		t.Error("failed registration left a node")
	}
}

func TestHybridTimeoutAcrossRestart(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial", true: "complete"}[complete], func(t *testing.T) {
			g, server := installationFixture(t)
			links := hybridInvite(t, g, server)
			for _, index := range []int{1, 0} {
				if index == 0 && !complete {
					continue
				}
				id, credential, _ := hybridRedeem(t, links[index])
				hybridConnect(t, server, links[index], id, credential, true)
			}
			g.mu.Lock()
			c := change{}
			for hash, i := range g.installations {
				i.ExpiresAt = time.Now().Add(-time.Second)
				g.installations[hash] = i
				c.invitations = append(c.invitations, hash)
			}
			if err := g.commit(c); err != nil {
				t.Fatal(err)
			}
			g.mu.Unlock()
			server.Close()
			if err := g.Close(); err != nil {
				t.Fatal(err)
			}
			restored, err := New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = restored.Close() }()
			want := 0
			if complete {
				want = 2
			}
			if len(restored.nodes) != want || len(restored.installations) != want {
				t.Fatal("wrong state after restart", restored.nodes, restored.installations)
			}
		})
	}
}

func TestHybridPendingClearAndExpiredRecovery(t *testing.T) {
	g, server := installationFixture(t)
	links := hybridInvite(t, g, server)
	id, credential, proof := hybridRedeem(t, links[1])
	hybridConnect(t, server, links[1], id, credential, true)
	admin := testAdmin{URL: server.URL, Key: superKey}
	if err := admin.JSON(context.Background(), "DELETE", "/v1/fleet/installations", nil, nil); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	if len(g.installations) != 2 {
		t.Error("clear discarded incomplete pair state")
	}
	for hash, i := range g.installations {
		i.ExpiresAt = time.Now().Add(-time.Second)
		g.installations[hash] = i
	}
	g.mu.Unlock()
	if err := (testAdmin{URL: links[1].URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err == nil {
		t.Fatal("expired partial installation recovered before sweep")
	}
	if err := admin.JSON(context.Background(), "DELETE", "/v1/fleet/installations", nil, nil); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.installations) != 0 || len(g.nodes) != 0 || !g.machineStates[id.ID].Unregistered {
		t.Fatal("clearing expired pair bypassed retirement")
	}
}

func TestHybridCompletedLifecycleIsIndependent(t *testing.T) {
	g, server := installationFixture(t)
	links := hybridInvite(t, g, server)
	var system *identity.Identity
	var systemKey string
	for _, index := range []int{1, 0} {
		id, credential, _ := hybridRedeem(t, links[index])
		hybridConnect(t, server, links[index], id, credential, true)
		if index == 1 {
			system, systemKey = id, credential
		}
	}
	if err := (testAdmin{URL: server.URL, Key: superKey}).JSON(context.Background(), "DELETE", "/v1/fleet/nodes/"+system.ID+"?stop=true", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.cleanupHybridInstallations(links[0].ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if len(g.directory(legacyUser)) != 1 || g.authenticate(systemKey).Role != "" {
		t.Fatal("independent retirement removed User or retained System credential")
	}
}

func TestHybridRevokedHistoryClear(t *testing.T) {
	for _, scenario := range []string{"unredeemed", "redeemed", "registered"} {
		for _, role := range []int{0, 1} {
			t.Run(scenario+"/"+[]string{"pair", "system"}[role], func(t *testing.T) {
				g, server := installationFixture(t)
				links := hybridInvite(t, g, server)
				var id *identity.Identity
				if scenario != "unredeemed" {
					var credential string
					id, credential, _ = hybridRedeem(t, links[1])
					if scenario == "registered" {
						hybridConnect(t, server, links[1], id, credential, true)
					}
				}
				admin := testAdmin{URL: server.URL, Key: superKey}
				ctx := context.Background()
				if err := admin.JSON(ctx, "DELETE", "/v1/fleet/installations/"+links[role].ID, nil, nil); err != nil {
					t.Fatal(err)
				}
				wantHidden := 2 - role
				var result struct{ Removed, Hidden int }
				if err := admin.JSON(ctx, "DELETE", "/v1/fleet/installations", nil, &result); err != nil || result.Removed != 0 || result.Hidden != wantHidden {
					t.Fatalf("clear = %+v, want %d hidden: %v", result, wantHidden, err)
				}
				var listed []enrollment.Invitation
				if err := admin.JSON(ctx, "GET", "/v1/fleet/installations", nil, &listed); err != nil || len(listed) != role {
					t.Fatalf("listed %d invitations, want %d: %v", len(listed), role, err)
				}
				if role == 1 && listed[0].ID != links[0].ID {
					t.Fatal("clear hid the usable User invitation")
				}
				if err := admin.JSON(ctx, "DELETE", "/v1/fleet/installations", nil, &result); err != nil || result.Removed != 0 || result.Hidden != 0 {
					t.Fatalf("repeated clear = %+v: %v", result, err)
				}
				stored := map[string]installation{}
				if err := readRecords(g.db, "invitations", "hash", stored); err != nil || len(stored) != 2 {
					t.Fatal("clear discarded durable retirement records", err)
				}
				for _, i := range stored {
					if i.Cleared != i.Revoked || !i.pendingHybrid() {
						t.Fatal("incorrectly retained invitation", i.ID)
					}
				}
				// Hidden records must survive restart and still retire installed halves.
				server.Close()
				if err := g.Close(); err != nil {
					t.Fatal(err)
				}
				restored, err := New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = restored.Close() }()
				if err := restored.cleanupHybridInstallations(links[0].ExpiresAt); err != nil {
					t.Fatal(err)
				}
				restored.mu.Lock()
				defer restored.mu.Unlock()
				if len(restored.installations) != 0 || len(restored.nodes) != 0 || (id != nil && !restored.machineStates[id.ID].Unregistered) {
					t.Fatal("cleared history lost timeout retirement")
				}
			})
		}
	}
}

func TestHybridRevokedHistoryClearRollback(t *testing.T) {
	g, server := installationFixture(t)
	links := hybridInvite(t, g, server)
	admin := testAdmin{URL: server.URL, Key: superKey}
	ctx := context.Background()
	if err := admin.JSON(ctx, "DELETE", "/v1/fleet/installations/"+links[0].ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`CREATE TRIGGER fail_clear BEFORE UPDATE ON invitations BEGIN SELECT RAISE(ABORT, 'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := admin.JSON(ctx, "DELETE", "/v1/fleet/installations", nil, nil); err == nil {
		t.Fatal("clear acknowledged failed persistence")
	}
	var listed []enrollment.Invitation
	if err := admin.JSON(ctx, "GET", "/v1/fleet/installations", nil, &listed); err != nil || len(listed) != 2 {
		t.Fatal("failed clear changed live history", err)
	}
	if _, err := g.db.Exec(`DROP TRIGGER fail_clear`); err != nil {
		t.Fatal(err)
	}
	var result struct{ Removed, Hidden int }
	if err := admin.JSON(ctx, "DELETE", "/v1/fleet/installations", nil, &result); err != nil || result.Hidden != 2 {
		t.Fatalf("clear retry = %+v: %v", result, err)
	}
}

func TestHybridCleanupPreservesSupersedingEnrollment(t *testing.T) {
	g, server := installationFixture(t)
	links := hybridInvite(t, g, server)
	id, credential, _ := hybridRedeem(t, links[1])
	hybridConnect(t, server, links[1], id, credential, true)
	// A subsequent valid invitation takes ownership of this same identity.
	link := invite(t, server, "replacement")
	key := "replacement-credential"
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	proof := enrollment.Redemption{PublicKey: id.Public, CredentialHash: enrollment.Hash(key)}
	proof.Signature = ed25519.Sign(id.Private, enrollment.Message(ticket, proof))
	if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.cleanupHybridInstallations(links[0].ExpiresAt); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.nodes) != 1 || len(g.installations) != 1 || g.machineStates[id.ID].Unregistered || g.authenticate(key).Role == "" {
		t.Fatal("old attempt retired superseding enrollment")
	}
}

func TestHybridTimeoutWithExistingUser(t *testing.T) {
	for _, redeemed := range []bool{false, true} {
		t.Run(map[bool]string{false: "untouched", true: "replacement redeemed"}[redeemed], func(t *testing.T) {
			g, server := installationFixture(t)
			user, err := identity.Load(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if rawPeer(t, server.URL, commonKey, "hybrid", legacyUser, user) == nil {
				t.Fatal("existing User registration failed")
			}
			links := hybridInvite(t, g, server)
			if redeemed {
				ticket := links[0].URL[strings.LastIndex(links[0].URL, "/")+1:]
				proof := enrollment.Redemption{PublicKey: user.Public, CredentialHash: enrollment.Hash("new-user-credential")}
				proof.Signature = ed25519.Sign(user.Private, enrollment.Message(ticket, proof))
				if err := (testAdmin{URL: links[0].URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := g.cleanupHybridInstallations(links[0].ExpiresAt); err != nil {
				t.Fatal(err)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			_, exists := g.nodes[user.ID]
			if exists == redeemed || g.machineStates[user.ID].Unregistered != redeemed || len(g.installations) != 0 {
				t.Fatal("incorrect cleanup of existing User profile", exists, g.machineStates[user.ID])
			}
		})
	}
}

func TestHybridCleanupTimer(t *testing.T) {
	g, server := installationFixture(t)
	hybridInvite(t, g, server)
	synctest.Test(t, func(t *testing.T) {
		g.mu.Lock()
		for hash, i := range g.installations {
			i.ExpiresAt = time.Now().Add(time.Second)
			g.installations[hash] = i
		}
		g.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go g.runHybridCleanup(ctx)
		time.Sleep(hybridCleanupInterval)
		synctest.Wait()
		g.mu.Lock()
		defer g.mu.Unlock()
		if len(g.installations) != 0 {
			t.Fatal("timer did not clean up expired pair")
		}
	})
}

func TestHybridCleanupRepeatedIdentityRollback(t *testing.T) {
	g, server := installationFixture(t)
	links := hybridInvite(t, g, server)
	id, credential, _ := hybridRedeem(t, links[1])
	hybridConnect(t, server, links[1], id, credential, true)
	// Reusing a profile for the other half revokes its first credential, but
	// must not overwrite the original rollback snapshot during pair cleanup.
	ticket := links[0].URL[strings.LastIndex(links[0].URL, "/")+1:]
	proof := enrollment.Redemption{PublicKey: id.Public, CredentialHash: enrollment.Hash("same-profile-credential")}
	proof.Signature = ed25519.Sign(id.Private, enrollment.Message(ticket, proof))
	if err := (testAdmin{URL: links[0].URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`CREATE TRIGGER fail_cleanup BEFORE DELETE ON invitations BEGIN SELECT RAISE(ABORT, 'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := g.cleanupHybridInstallations(links[0].ExpiresAt); err == nil {
		t.Fatal("acknowledged failed cleanup")
	}
	g.mu.Lock()
	if g.machineStates[id.ID] != (model.MachineState{}) || len(g.nodes) != 1 || len(g.installations) != 2 {
		t.Error("rollback lost original state for repeated identity")
	}
	g.mu.Unlock()
	if _, err := g.db.Exec(`DROP TRIGGER fail_cleanup`); err != nil {
		t.Fatal(err)
	}
	if err := g.cleanupHybridInstallations(links[0].ExpiresAt); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.machineStates[id.ID].Unregistered || g.machineStates[id.ID].Revision != 1 {
		t.Fatal("identity was not retired exactly once")
	}
}
